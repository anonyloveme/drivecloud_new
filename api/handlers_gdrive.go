package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"telecloud/database"
	"telecloud/tgclient"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	driveAPIBase     = "https://www.googleapis.com/drive/v3/files"
	maxGDriveDepth   = 10
	maxGDriveFiles   = 500
	googleNativeMime = "application/vnd.google-apps."
)

type gdriveFile struct {
	ID           string
	Name         string
	MimeType     string
	Size         int64
	RelativePath string
}

type gdriveListResp struct {
	Files []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		MimeType string `json:"mimeType"`
		Size     string `json:"size"`
	} `json:"files"`
	NextPageToken string `json:"nextPageToken"`
}

func parseGDriveFolderID(input string) string {
	input = strings.TrimSpace(input)
	re := regexp.MustCompile(`folders/([a-zA-Z0-9_-]+)`)
	if m := re.FindStringSubmatch(input); len(m) > 1 {
		return m[1]
	}
	re = regexp.MustCompile(`[?&]id=([a-zA-Z0-9_-]+)`)
	if m := re.FindStringSubmatch(input); len(m) > 1 {
		return m[1]
	}
	if regexp.MustCompile(`^[a-zA-Z0-9_-]{10,}$`).MatchString(input) {
		return input
	}
	return ""
}

func (h *Handler) handlePostGDriveImport(c *gin.Context) {
	var req struct {
		FolderURL string `json:"folder_url"`
		Path      string `json:"path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.FolderURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing_folder_url"})
		return
	}

	apiKey := database.GetSetting("gdrive_api_key")
	if apiKey == "" {
		apiKey = os.Getenv("GDRIVE_API_KEY")
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gdrive_api_key_not_set"})
		return
	}

	folderID := parseGDriveFolderID(req.FolderURL)
	if folderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_folder_url"})
		return
	}

	username := c.GetString("username")
	isAdmin := c.GetBool("is_admin")
	dbPath := mapPath(req.Path, username, isAdmin)

	taskID := uuid.New().String()
	go h.processGDriveImport(taskID, folderID, dbPath, apiKey, username)

	c.JSON(http.StatusOK, gin.H{"task_id": taskID})
}

func (h *Handler) handleGetGDriveStatus(c *gin.Context) {
	apiKey := database.GetSetting("gdrive_api_key")
	if apiKey == "" {
		apiKey = os.Getenv("GDRIVE_API_KEY")
	}
	c.JSON(http.StatusOK, gin.H{"configured": apiKey != ""})
}

func (h *Handler) handlePostGDriveAPIKey(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	apiKey := c.PostForm("api_key")
	if err := database.SetSetting("gdrive_api_key", apiKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "success", "configured": apiKey != ""})
}

func (h *Handler) processGDriveImport(taskID, folderID, dbPath, apiKey, owner string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tgclient.TaskMutex.Lock()
	tgclient.TaskCancels[taskID] = cancel
	tgclient.TaskMutex.Unlock()
	defer func() {
		tgclient.TaskMutex.Lock()
		delete(tgclient.TaskCancels, taskID)
		tgclient.TaskMutex.Unlock()
	}()

	tgclient.UpdateTask(taskID, "listing", 0, "gdrive_listing", owner)

	// BUG1 FIX: Fetch root folder name to include in path
	rootName := getDriveFileName(ctx, apiKey, folderID)
	if rootName == "" {
		rootName = "gdrive_import"
	}

	// Pass root folder name as initial parentPath so the tree mirrors Drive
	files, folders, skipped, err := listDriveFilesRecursive(ctx, apiKey, folderID, rootName, 0)
	if err != nil {
		tgclient.UpdateTask(taskID, "error", 0, "gdrive_list_failed: "+truncate(err.Error(), 200), owner)
		return
	}

	total := len(files)
	if total == 0 && len(skipped) == 0 && len(folders) == 0 {
		tgclient.UpdateTask(taskID, "done", 100, "gdrive_empty", owner)
		return
	}

	// Build the FULL folder tree from collected folder relative paths (includes empty folders).
	// Sort by depth so parents are created before children.
	sortFoldersByDepth(folders)
	for _, folderRelPath := range folders {
		folderAbsPath := path.Clean(dbPath + "/" + folderRelPath)
		log.Printf("[GDrive] Creating folder: %s (rel=%s)", folderAbsPath, folderRelPath)
		database.EnsureFoldersExist(folderAbsPath, owner)
	}

	// Also create the root folder itself (its name is rootName, already in every RelativePath)
	rootAbsPath := path.Clean(dbPath + "/" + rootName)
	database.EnsureFoldersExist(rootAbsPath, owner)

	// Debug: log target paths for first few files to verify no root duplication
	for i, f := range files {
		if i >= 3 {
			break
		}
		targetPath := dbPath
		if f.RelativePath != "" {
			targetPath = path.Clean(dbPath + "/" + f.RelativePath)
		}
		log.Printf("[GDrive] File target: %s/%s (rel=%s, dbPath=%s)", targetPath, f.Name, f.RelativePath, dbPath)
	}

	// BUG2 FIX: Worker pool with bounded concurrency
	concurrency := 3
	if h.cfg.UploadConcurrency > 0 && h.cfg.UploadConcurrency < concurrency {
		concurrency = h.cfg.UploadConcurrency
	}

	var (
		imported int64
		failedMu sync.Mutex
		failed   []string
		done     int64
	)

	tgclient.UpdateTask(taskID, "importing", 0, fmt.Sprintf("gdrive_importing|%d|%d", total, len(skipped)), owner)

	jobs := make(chan gdriveFile, len(files))
	for _, f := range files {
		jobs <- f
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				targetPath := dbPath
				if f.RelativePath != "" {
					targetPath = path.Clean(dbPath + "/" + f.RelativePath)
				}

				if err := h.importOneDriveFile(ctx, f, targetPath, apiKey, owner, taskID); err != nil {
					log.Printf("[GDrive] Failed to import %s: %v", f.Name, err)
					failedMu.Lock()
					failed = append(failed, f.Name)
					failedMu.Unlock()
				} else {
					atomic.AddInt64(&imported, 1)
				}

				atomic.AddInt64(&done, 1)
				pct := int(atomic.LoadInt64(&done)) * 100 / total
				tgclient.UpdateTask(taskID, "importing", pct,
					fmt.Sprintf("gdrive_file|%s|%d|%d", f.Name, atomic.LoadInt64(&done), total), owner)
			}
		}()
	}

	wg.Wait()

	if ctx.Err() != nil {
		tgclient.UpdateTask(taskID, "cancelled", int(atomic.LoadInt64(&done))*100/total, "cancelled", owner)
		return
	}

	failedMu.Lock()
	failedCopy := append([]string{}, failed...)
	failedMu.Unlock()

	msg := fmt.Sprintf("gdrive_done|%d|%d|%d|%s", atomic.LoadInt64(&imported), len(failedCopy), len(skipped), strings.Join(failedCopy, ","))
	tgclient.UpdateTask(taskID, "done", 100, msg, owner)
}

func (h *Handler) importOneDriveFile(ctx context.Context, f gdriveFile, targetPath, apiKey, owner, taskID string) error {
	// BUG3 FIX: Handle zero-size files gracefully (create empty DB row, no download)
	if f.Size == 0 {
		database.EnsureFoldersExist(targetPath, owner)
		unlock := database.AcquireFileInsertLock(owner, targetPath)
		uniqueName := database.GetUniqueFilename(database.RODB, targetPath, f.Name, false, 0, owner)
		mimeType := f.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		_, err := database.InsertAndGetID(database.DB,
			"INSERT INTO files (filename, path, size, mime_type, is_folder, owner) VALUES (?, ?, 0, ?, 0, ?)",
			uniqueName, targetPath, mimeType, owner)
		unlock()
		return err
	}

	downloadURL := fmt.Sprintf("%s/%s?alt=media&key=%s", driveAPIBase, f.ID, apiKey)
	if len(downloadURL) > 2000 {
		downloadURL = fmt.Sprintf("%s/%s?alt=media", driveAPIBase, f.ID)
	}

	// BUG3: Stream to temp file then upload (uploader requires seekable file path).
	// ProcessRemoteUploadSync exists but doesn't accept a filename override —
	// it extracts from URL/Content-Disposition, which fails for Drive URLs.
	// So we keep the temp-file approach with proper cleanup.
	tempDir := h.cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	tempFile, err := os.CreateTemp(tempDir, "gdrive_*_"+sanitizeFilename(f.Name))
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)

	resp, err := doDriveDownload(ctx, downloadURL, apiKey)
	if err != nil {
		tempFile.Close()
		return err
	}
	defer resp.Body.Close()

	// BUG3 FIX: Virus-scan interstitial — extract confirm token and retry
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") {
		resp.Body.Close()
		confirmURL := downloadURL + "&confirm=t"
		resp2, err := doDriveDownload(ctx, confirmURL, apiKey)
		if err != nil {
			return fmt.Errorf("virus scan retry failed: %w", err)
		}
		defer resp2.Body.Close()
		if strings.Contains(resp2.Header.Get("Content-Type"), "text/html") {
			return fmt.Errorf("virus scan interstitial — download manually")
		}
		resp = resp2
	}

	written, err := io.Copy(tempFile, resp.Body)
	tempFile.Close()
	if err != nil {
		return fmt.Errorf("download body: %w", err)
	}
	if written == 0 {
		return fmt.Errorf("downloaded 0 bytes")
	}

	mimeType := f.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	_, _, err = tgclient.ProcessCompleteUploadSync(ctx, tempPath, f.Name, targetPath, mimeType, taskID, h.cfg, false, owner)
	return err
}

func doDriveDownload(ctx context.Context, url, apiKey string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		time.Sleep(5 * time.Second)
		req2, _ := http.NewRequestWithContext(ctx, "GET", url+"&confirm=t", nil)
		resp2, err := client.Do(req2)
		if err != nil {
			return nil, fmt.Errorf("retry download: %w", err)
		}
		if resp2.StatusCode != http.StatusOK {
			resp2.Body.Close()
			return nil, fmt.Errorf("download failed HTTP %d after retry", resp2.StatusCode)
		}
		return resp2, nil
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed HTTP %d", resp.StatusCode)
	}

	return resp, nil
}

// getDriveFileName fetches the name of a Drive file/folder by ID.
func getDriveFileName(ctx context.Context, apiKey, fileID string) string {
	url := fmt.Sprintf("%s/%s?fields=name&key=%s", driveAPIBase, fileID, apiKey)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Name
}

func listDriveFilesRecursive(ctx context.Context, apiKey, folderID, parentPath string, depth int) ([]gdriveFile, []string, []string, error) {
	if depth > maxGDriveDepth {
		return nil, nil, nil, fmt.Errorf("max recursion depth %d exceeded", maxGDriveDepth)
	}

	var files []gdriveFile
	var folders []string
	var skipped []string
	pageToken := ""

	for {
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		q := fmt.Sprintf("'%s' in parents and trashed=false", folderID)
		fields := "files(id,name,mimeType,size),nextPageToken"
		listURL := fmt.Sprintf("%s?q=%s&fields=%s&key=%s&pageSize=1000", driveAPIBase, encodeQuery(q), fields, apiKey)
		if pageToken != "" {
			listURL += "&pageToken=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
		if err != nil {
			return nil, nil, nil, err
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, nil, err
		}

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, nil, nil, fmt.Errorf("drive list HTTP %d", resp.StatusCode)
		}

		var listResp gdriveListResp
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			resp.Body.Close()
			return nil, nil, nil, err
		}
		resp.Body.Close()

		for _, item := range listResp.Files {
			if len(files)+len(skipped) >= maxGDriveFiles {
				skipped = append(skipped, "MAX_FILES_REACHED")
				return files, folders, skipped, nil
			}

			if strings.HasPrefix(item.MimeType, googleNativeMime) {
				skipped = append(skipped, item.Name)
				continue
			}

			if item.MimeType == "application/vnd.google-apps.folder" {
				subPath := parentPath
				if subPath == "" {
					subPath = item.Name
				} else {
					subPath = subPath + "/" + item.Name
				}
				// FIX: Record every folder encountered (including empty ones) BEFORE recursing
				folders = append(folders, subPath)
				subFiles, subFolders, subSkipped, err := listDriveFilesRecursive(ctx, apiKey, item.ID, subPath, depth+1)
				if err != nil {
					skipped = append(skipped, item.Name+"/ (error: "+err.Error()+")")
					continue
				}
				files = append(files, subFiles...)
				folders = append(folders, subFolders...)
				skipped = append(skipped, subSkipped...)
			} else {
				size := int64(0)
				fmt.Sscanf(item.Size, "%d", &size)
				files = append(files, gdriveFile{
					ID:           item.ID,
					Name:         item.Name,
					MimeType:     item.MimeType,
					Size:         size,
					RelativePath: parentPath,
				})
			}
		}

		pageToken = listResp.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return files, folders, skipped, nil
}

// sortFoldersByDepth sorts folder relative paths by depth (fewest "/" first)
// so parent folders are created before children.
func sortFoldersByDepth(folders []string) {
	sort.Slice(folders, func(i, j int) bool {
		depthI := strings.Count(folders[i], "/")
		depthJ := strings.Count(folders[j], "/")
		if depthI != depthJ {
			return depthI < depthJ
		}
		return folders[i] < folders[j]
	})
}

func encodeQuery(q string) string {
	r := strings.NewReplacer("'", "%27", " ", "+")
	return r.Replace(q)
}

func sanitizeFilename(name string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9._-]`)
	return re.ReplaceAllString(name, "_")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
