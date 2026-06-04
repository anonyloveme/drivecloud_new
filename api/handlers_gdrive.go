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
	"strings"
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
	ID             string
	Name           string
	MimeType       string
	Size           int64
	RelativePath   string
}

type gdriveListResp struct {
	Files         []struct {
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

	files, skipped, err := listDriveFilesRecursive(ctx, apiKey, folderID, "", 0)
	if err != nil {
		tgclient.UpdateTask(taskID, "error", 0, "gdrive_list_failed: "+truncate(err.Error(), 200), owner)
		return
	}

	total := len(files)
	if total == 0 && len(skipped) == 0 {
		tgclient.UpdateTask(taskID, "done", 100, "gdrive_empty", owner)
		return
	}

	imported := 0
	var failed []string

	for i, f := range files {
		select {
		case <-ctx.Done():
			tgclient.UpdateTask(taskID, "cancelled", imported*100/total, "cancelled", owner)
			return
		default:
		}

		targetPath := dbPath
		if f.RelativePath != "" {
			targetPath = path.Clean(dbPath + "/" + f.RelativePath)
		}

		pct := (i * 100) / total
		tgclient.UpdateTask(taskID, "importing", pct, fmt.Sprintf("gdrive_file|%s|%d|%d", f.Name, i+1, total), owner)

		if err := h.importOneDriveFile(ctx, f, targetPath, apiKey, owner, taskID); err != nil {
			log.Printf("[GDrive] Failed to import %s: %v", f.Name, err)
			failed = append(failed, f.Name)
			continue
		}
		imported++
	}

	msg := fmt.Sprintf("gdrive_done|%d|%d|%d|%s", imported, len(failed), len(skipped), strings.Join(failed, ","))
	if len(failed) > 0 {
		tgclient.UpdateTask(taskID, "done", 100, msg, owner)
	} else {
		tgclient.UpdateTask(taskID, "done", 100, msg, owner)
	}
}

func (h *Handler) importOneDriveFile(ctx context.Context, f gdriveFile, targetPath, apiKey, owner, taskID string) error {
	if f.Size <= 0 {
		return fmt.Errorf("zero-size file")
	}

	downloadURL := fmt.Sprintf("%s/%s?alt=media&key=%s", driveAPIBase, f.ID, apiKey)
	if len(downloadURL) > 2000 {
		downloadURL = fmt.Sprintf("%s/%s?alt=media", driveAPIBase, f.ID)
	}

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

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		tempFile.Close()
		return fmt.Errorf("create request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		tempFile.Close()
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		time.Sleep(5 * time.Second)
		req2, _ := http.NewRequestWithContext(ctx, "GET", downloadURL+"&confirm=t", nil)
		resp2, err := client.Do(req2)
		if err != nil {
			return fmt.Errorf("retry download: %w", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			return fmt.Errorf("download failed HTTP %d after retry", resp2.StatusCode)
		}
		resp = resp2
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed HTTP %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") {
		tempFile.Close()
		return fmt.Errorf("virus scan interstitial (file too large for direct download)")
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

func listDriveFilesRecursive(ctx context.Context, apiKey, folderID, parentPath string, depth int) ([]gdriveFile, []string, error) {
	if depth > maxGDriveDepth {
		return nil, nil, fmt.Errorf("max recursion depth %d exceeded", maxGDriveDepth)
	}

	var files []gdriveFile
	var skipped []string
	pageToken := ""

	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
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
			return nil, nil, err
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			time.Sleep(3 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("drive list HTTP %d", resp.StatusCode)
		}

		var listResp gdriveListResp
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			resp.Body.Close()
			return nil, nil, err
		}
		resp.Body.Close()

		for _, item := range listResp.Files {
			if len(files)+len(skipped) >= maxGDriveFiles {
				skipped = append(skipped, "MAX_FILES_REACHED")
				return files, skipped, nil
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
				subFiles, subSkipped, err := listDriveFilesRecursive(ctx, apiKey, item.ID, subPath, depth+1)
				if err != nil {
					skipped = append(skipped, item.Name+"/ (error: "+err.Error()+")")
					continue
				}
				files = append(files, subFiles...)
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

	return files, skipped, nil
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
