package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
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
	maxGDriveFiles   = 50000
	googleNativeMime = "application/vnd.google-apps."
)

var gdriveDebug = os.Getenv("GDRIVE_DEBUG") == "1"

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

	// Try OAuth first, fall back to API key
	accessToken := getValidAccessToken()
	apiKey := database.GetSetting("gdrive_api_key")
	if apiKey == "" {
		apiKey = os.Getenv("GDRIVE_API_KEY")
	}
	if accessToken == "" && apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gdrive_auth_not_configured"})
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
	go h.processGDriveImport(taskID, folderID, dbPath, apiKey, accessToken, username)

	c.JSON(http.StatusOK, gin.H{"task_id": taskID})
}

func (h *Handler) handleGetGDriveStatus(c *gin.Context) {
	apiKey := database.GetSetting("gdrive_api_key")
	if apiKey == "" {
		apiKey = os.Getenv("GDRIVE_API_KEY")
	}
	refreshToken := database.GetSetting("gdrive_oauth_refresh_token")
	clientID := database.GetSetting("gdrive_oauth_client_id")
	c.JSON(http.StatusOK, gin.H{
		"configured":    apiKey != "" || refreshToken != "",
		"api_key_set":   apiKey != "",
		"oauth_connected": refreshToken != "",
		"oauth_client_id": clientID != "",
	})
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

func (h *Handler) handlePostGDriveOAuthConfig(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request"})
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" || req.RedirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "all_fields_required"})
		return
	}
	database.SetSetting("gdrive_oauth_client_id", req.ClientID)
	database.SetSetting("gdrive_oauth_client_secret", req.ClientSecret)
	database.SetSetting("gdrive_oauth_redirect_uri", req.RedirectURI)
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (h *Handler) processGDriveImport(taskID, folderID, dbPath, apiKey, accessToken, owner string) {
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

	// Fetch root folder name to include in path
	rootName := sanitizePathSegment(getDriveFileName(ctx, apiKey, accessToken, folderID))
	if rootName == "" {
		rootName = "gdrive_import"
	}
	log.Printf("[GDrive] Starting import: folderID=%s rootName=%q dbPath=%q auth=%s", folderID, rootName, dbPath, authMode(accessToken, apiKey))

	// Pass root folder name as initial parentPath so the tree mirrors Drive
	files, folders, skipped, err := listDriveFilesRecursive(ctx, apiKey, accessToken, folderID, rootName, 0)
	if err != nil {
		log.Printf("[GDrive] listDriveFilesRecursive failed: %v", err)
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
	// Ensure rootName itself is in the list (first, depth 0).
	hasRoot := false
	for _, fp := range folders {
		if fp == rootName {
			hasRoot = true
			break
		}
	}
	if !hasRoot {
		folders = append([]string{rootName}, folders...)
	}
	sortFoldersByDepth(folders)
	log.Printf("[GDrive] Total: %d files, %d folders, %d skipped", total, len(folders), len(skipped))
	for _, folderRelPath := range folders {
		folderAbsPath := path.Clean(dbPath + "/" + folderRelPath)
		log.Printf("[GDrive] Creating folder: %s (rel=%s)", folderAbsPath, folderRelPath)
		if err := database.EnsureFoldersExist(folderAbsPath, owner); err != nil {
			log.Printf("[GDrive] WARNING: EnsureFoldersExist failed for %s: %v", folderAbsPath, err)
		}
	}

	// Debug: log target paths for first ~10 files
	if gdriveDebug {
		for i, f := range files {
			if i >= 10 {
				break
			}
			targetPath := dbPath
			if f.RelativePath != "" {
				targetPath = path.Clean(dbPath + "/" + f.RelativePath)
			}
			log.Printf("[GDrive] File target: %s/%s (rel=%s, dbPath=%s)", targetPath, f.Name, f.RelativePath, dbPath)
		}
	}

	// Log the full sorted folder list for verification
	if gdriveDebug {
		for i, fp := range folders {
			log.Printf("[GDrive] Folder[%d]: %s", i, fp)
		}
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

	// Shuffle files so workers process files from ALL folders interleaved,
	// not sequentially by folder (which starves later folders).
	rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })

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

				// Belt-and-suspenders: ensure target folder exists right before import
				if err := database.EnsureFoldersExist(targetPath, owner); err != nil {
					log.Printf("[GDrive] WARNING: worker EnsureFoldersExist(%s) failed: %v", targetPath, err)
				}

				if err := h.importOneDriveFile(ctx, f, targetPath, apiKey, accessToken, owner, taskID); err != nil {
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

func (h *Handler) importOneDriveFile(ctx context.Context, f gdriveFile, targetPath, apiKey, accessToken, owner, taskID string) error {
	// Handle zero-size files gracefully (create empty DB row, no download)
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

	downloadURL := fmt.Sprintf("%s/%s?alt=media&supportsAllDrives=true", driveAPIBase, f.ID)
	if accessToken == "" {
		downloadURL += "&key=" + apiKey
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

	resp, err := doDriveDownload(ctx, downloadURL, apiKey, accessToken)
	if err != nil {
		tempFile.Close()
		return err
	}
	defer resp.Body.Close()

	// Virus-scan interstitial — extract confirm token and retry
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") {
		resp.Body.Close()
		confirmURL := downloadURL + "&confirm=t"
		resp2, err := doDriveDownload(ctx, confirmURL, apiKey, accessToken)
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

func doDriveDownload(ctx context.Context, url, apiKey, accessToken string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
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
		if accessToken != "" {
			req2.Header.Set("Authorization", "Bearer "+accessToken)
		}
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
func getDriveFileName(ctx context.Context, apiKey, accessToken, fileID string) string {
	url := fmt.Sprintf("%s/%s?fields=name&supportsAllDrives=true", driveAPIBase, fileID)
	if accessToken == "" {
		url += "&key=" + apiKey
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
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

func listDriveFilesRecursive(ctx context.Context, apiKey, accessToken, folderID, parentPath string, depth int) ([]gdriveFile, []string, []string, error) {
	if depth > maxGDriveDepth {
		return nil, nil, nil, fmt.Errorf("max recursion depth %d exceeded", maxGDriveDepth)
	}

	var files []gdriveFile
	var folders []string
	var skipped []string
	pageToken := ""
	retries := 0

	for {
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		q := fmt.Sprintf("'%s' in parents and trashed=false", folderID)
		fields := "files(id,name,mimeType,size),nextPageToken"
		listURL := fmt.Sprintf("%s?q=%s&fields=%s&pageSize=1000&supportsAllDrives=true&includeItemsFromAllDrives=true", driveAPIBase, encodeQuery(q), fields)
		if accessToken != "" {
			// OAuth: no key param, use Authorization header
		} else {
			listURL += "&key=" + apiKey
		}
		if pageToken != "" {
			listURL += "&pageToken=" + pageToken
		}

		req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
		if err != nil {
			log.Printf("[GDrive] list request error: %v", err)
			break
		}
		if accessToken != "" {
			req.Header.Set("Authorization", "Bearer "+accessToken)
		}

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[GDrive] list HTTP error: %v", err)
			break
		}

		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			retries++
			if retries > 5 {
				log.Printf("[GDrive] rate limit: giving up after retries")
				break
			}
			delay := time.Duration(1<<uint(retries-1)) * time.Second
			jitter := time.Duration(rand.Int63n(int64(delay) / 2))
			time.Sleep(delay + jitter)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Printf("[GDrive] list HTTP %d", resp.StatusCode)
			break
		}
		retries = 0

		var listResp gdriveListResp
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			resp.Body.Close()
			log.Printf("[GDrive] list JSON decode error: %v", err)
			break
		}
		resp.Body.Close()

		fileLimitReached := false
		for _, item := range listResp.Files {
			if gdriveDebug {
				log.Printf("[GDrive] API item: name=%q mime=%q size=%s parentPath=%q depth=%d", item.Name, item.MimeType, item.Size, parentPath, depth)
			}

			if item.MimeType == "application/vnd.google-apps.folder" {
				// FOLDERS always enumerated — never cut short by file limit
				safeName := sanitizePathSegment(item.Name)
				subPath := parentPath
				if subPath == "" {
					subPath = safeName
				} else {
					subPath = subPath + "/" + safeName
				}
				// Record every folder encountered (including empty ones) BEFORE recursing
				folders = append(folders, subPath)
				subFiles, subFolders, subSkipped, err := listDriveFilesRecursive(ctx, apiKey, accessToken, item.ID, subPath, depth+1)
				if err != nil {
					skipped = append(skipped, item.Name+"/ (error: "+err.Error()+")")
					continue
				}
				files = append(files, subFiles...)
				folders = append(folders, subFolders...)
				skipped = append(skipped, subSkipped...)
			} else if strings.HasPrefix(item.MimeType, googleNativeMime) {
				skipped = append(skipped, item.Name)
				continue
			} else {
				// FILES capped at maxGDriveFiles — skip collecting more but keep enumerating siblings
				if len(files) >= maxGDriveFiles {
					if !fileLimitReached {
						log.Printf("[GDrive] file limit reached: collected %d files, remaining files skipped", len(files))
						skipped = append(skipped, "MAX_FILES_REACHED")
						fileLimitReached = true
					}
					continue
				}
				size := int64(0)
				fmt.Sscanf(item.Size, "%d", &size)
				files = append(files, gdriveFile{
					ID:           item.ID,
					Name:         sanitizePathSegment(item.Name),
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

func sanitizePathSegment(name string) string {
	name = strings.ReplaceAll(name, "/", "／")
	name = strings.ReplaceAll(name, "\\", "＼")
	return name
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

func authMode(accessToken, apiKey string) string {
	if accessToken != "" {
		return "oauth"
	}
	if apiKey != "" {
		return "api_key"
	}
	return "none"
}

// getValidAccessToken returns a valid access token, refreshing if expired.
// Returns "" if OAuth is not configured.
func getValidAccessToken() string {
	accessToken := database.GetSetting("gdrive_oauth_access_token")
	refreshToken := database.GetSetting("gdrive_oauth_refresh_token")
	if refreshToken == "" {
		return ""
	}

	expiryStr := database.GetSetting("gdrive_oauth_token_expiry")
	if expiryStr != "" {
		expiry, err := time.Parse(time.RFC3339, expiryStr)
		if err == nil && time.Now().Add(60*time.Second).Before(expiry) {
			return accessToken
		}
	}

	// Token expired or missing — refresh
	clientID := database.GetSetting("gdrive_oauth_client_id")
	clientSecret := database.GetSetting("gdrive_oauth_client_secret")
	if clientID == "" || clientSecret == "" {
		return ""
	}

	data := fmt.Sprintf("client_id=%s&client_secret=%s&refresh_token=%s&grant_type=refresh_token",
		clientID, clientSecret, refreshToken)

	resp, err := http.Post("https://oauth2.googleapis.com/token", "application/x-www-form-urlencoded", strings.NewReader(data))
	if err != nil {
		log.Printf("[GDrive] OAuth token refresh failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[GDrive] OAuth token refresh HTTP %d: %s", resp.StatusCode, string(body))
		return ""
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		log.Printf("[GDrive] OAuth token refresh decode error: %v", err)
		return ""
	}

	if tokenResp.AccessToken == "" {
		log.Printf("[GDrive] OAuth token refresh returned empty access_token")
		return ""
	}

	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	database.SetSetting("gdrive_oauth_access_token", tokenResp.AccessToken)
	database.SetSetting("gdrive_oauth_token_expiry", expiry)

	log.Printf("[GDrive] OAuth token refreshed, expires in %ds", tokenResp.ExpiresIn)
	return tokenResp.AccessToken
}

func (h *Handler) handleGDriveOAuthStart(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	clientID := database.GetSetting("gdrive_oauth_client_id")
	clientSecret := database.GetSetting("gdrive_oauth_client_secret")
	redirectURI := database.GetSetting("gdrive_oauth_redirect_uri")
	if clientID == "" || clientSecret == "" || redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gdrive_oauth_not_configured"})
		return
	}

	state := uuid.New().String()
	database.SetSetting("gdrive_oauth_state", state)

	authURL := fmt.Sprintf(
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&access_type=offline&prompt=consent&state=%s",
		clientID, redirectURI, "https://www.googleapis.com/auth/drive.readonly", state,
	)

	c.Redirect(http.StatusTemporaryRedirect, authURL)
}

func (h *Handler) handleGDriveOAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing_code"})
		return
	}

	storedState := database.GetSetting("gdrive_oauth_state")
	if storedState == "" || storedState != state {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_state"})
		return
	}
	database.SetSetting("gdrive_oauth_state", "")

	clientID := database.GetSetting("gdrive_oauth_client_id")
	clientSecret := database.GetSetting("gdrive_oauth_client_secret")
	redirectURI := database.GetSetting("gdrive_oauth_redirect_uri")

	data := fmt.Sprintf("code=%s&client_id=%s&client_secret=%s&redirect_uri=%s&grant_type=authorization_code",
		code, clientID, clientSecret, redirectURI)

	resp, err := http.Post("https://oauth2.googleapis.com/token", "application/x-www-form-urlencoded", strings.NewReader(data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token_exchange_failed"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[GDrive] OAuth token exchange HTTP %d: %s", resp.StatusCode, string(body))
		c.JSON(http.StatusBadGateway, gin.H{"error": "token_exchange_failed"})
		return
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token_decode_failed"})
		return
	}

	database.SetSetting("gdrive_oauth_access_token", tokenResp.AccessToken)
	database.SetSetting("gdrive_oauth_token_expiry", time.Now().Add(time.Duration(tokenResp.ExpiresIn)*time.Second).Format(time.RFC3339))
	if tokenResp.RefreshToken != "" {
		database.SetSetting("gdrive_oauth_refresh_token", tokenResp.RefreshToken)
	}

	log.Printf("[GDrive] OAuth connected, expires in %ds", tokenResp.ExpiresIn)
	c.Redirect(http.StatusTemporaryRedirect, "/")
}

func (h *Handler) handleGDriveOAuthStatus(c *gin.Context) {
	refreshToken := database.GetSetting("gdrive_oauth_refresh_token")
	clientID := database.GetSetting("gdrive_oauth_client_id")
	clientSecret := database.GetSetting("gdrive_oauth_client_secret")
	redirectURI := database.GetSetting("gdrive_oauth_redirect_uri")

	c.JSON(http.StatusOK, gin.H{
		"connected":       refreshToken != "",
		"client_id":       clientID != "",
		"client_secret":   clientSecret != "",
		"redirect_uri":    redirectURI != "",
		"config_complete": clientID != "" && clientSecret != "" && redirectURI != "",
	})
}

func (h *Handler) handleGDriveOAuthDisconnect(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}

	database.SetSetting("gdrive_oauth_access_token", "")
	database.SetSetting("gdrive_oauth_refresh_token", "")
	database.SetSetting("gdrive_oauth_token_expiry", "")
	database.SetSetting("gdrive_oauth_state", "")

	c.JSON(http.StatusOK, gin.H{"status": "disconnected"})
}
