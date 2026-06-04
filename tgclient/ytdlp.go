package tgclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"telecloud/config"
	"telecloud/utils"
	"time"
)

func translateYTDLPError(errMsg string) string {
	errMsg = strings.ToLower(errMsg)

	switch {
	case strings.Contains(errMsg, "sign in to confirm your age"):
		return "age_restricted"
	case strings.Contains(errMsg, "incomplete youtube id") || strings.Contains(errMsg, "not a valid url"):
		return "invalid_url"
	case strings.Contains(errMsg, "this video is unavailable") || strings.Contains(errMsg, "video unavailable"):
		return "video_unavailable"
	case strings.Contains(errMsg, "private video"):
		return "video_private"
	case strings.Contains(errMsg, "403: forbidden") || strings.Contains(errMsg, "403 forbidden"):
		return "remote_forbidden"
	case strings.Contains(errMsg, "429: too many requests") || strings.Contains(errMsg, "429 too many requests"):
		return "too_many_requests"
	case strings.Contains(errMsg, "unsupported url"):
		return "unsupported_url"
	case strings.Contains(errMsg, "geo-restricted") || strings.Contains(errMsg, "not available in your country"):
		return "geo_restricted"
	case strings.Contains(errMsg, "ffmpeg not found"):
		return "ffmpeg_missing"
	case strings.Contains(errMsg, "getaddrinfo failed") || strings.Contains(errMsg, "name or service not known"):
		return "network_error"
	}

	return "ytdlp_error"
}

func IsValidURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// YTDLPInfo represents the structure of yt-dlp -J output
type YTDLPInfo struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Thumbnail   string        `json:"thumbnail"`
	Uploader    string        `json:"uploader"`
	Duration    float64       `json:"duration"`
	UploadDate  string        `json:"upload_date"`
	Formats     []YTDLPFormat `json:"formats"`
	Ext         string        `json:"ext"`
}

type YTDLPFormat struct {
	FormatID       string `json:"format_id"`
	FormatNote     string `json:"format_note"`
	Ext            string `json:"ext"`
	Resolution     string `json:"resolution"`
	Filesize       int64  `json:"filesize"`
	FilesizeApprox int64  `json:"filesize_approx"`
	VCodec         string `json:"vcodec"`
	ACodec         string `json:"acodec"`
	Format         string `json:"format"`
	Height         int    `json:"height"`
}

func GetYTDLPFormats(url string, cfg *config.Config, owner string) (*YTDLPInfo, error) {
	ytdlpEnabled := cfg.YTDLPPath != "disabled" && cfg.YTDLPPath != "disable"
	ffmpegEnabled := cfg.FFMPEGPath != "disabled" && cfg.FFMPEGPath != "disable"
	if !ytdlpEnabled || !ffmpegEnabled {
		return nil, fmt.Errorf("ytdlp_disabled")
	}

	if !IsValidURL(url) {
		return nil, fmt.Errorf("invalid_url_format")
	}

	if utils.IsPrivateIP(url) {
		return nil, fmt.Errorf("forbidden_url")
	}

	args := []string{"-J", "--no-playlist", "--js-runtimes", "node", url}

	// Check for user cookie file
	cookieFile := filepath.Join(cfg.CookiesDir, fmt.Sprintf("user_%s.txt", owner))
	if _, err := os.Stat(cookieFile); err == nil {
		args = append([]string{"--cookies", cookieFile}, args...)
	}

	var stdout, stderr strings.Builder
	cmd := exec.Command(cfg.YTDLPPath, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()

	err := cmd.Run()
	if err != nil {
		// Clean up common yt-dlp error prefixes from stderr
		errMsg := stderr.String()
		if idx := strings.Index(errMsg, "ERROR:"); idx != -1 {
			errMsg = strings.TrimSpace(errMsg[idx+6:])
		}
		if errMsg == "" {
			errMsg = err.Error()
		}

		cleanErr := translateYTDLPError(errMsg)
		if cleanErr != "ytdlp_error" {
			return nil, fmt.Errorf("%s", cleanErr)
		}

		// Limit error message length for generic errors
		if len(errMsg) > 200 {
			errMsg = errMsg[:197] + "..."
		}
		return nil, fmt.Errorf("ytdlp_error: %s", errMsg)
	}

	var info YTDLPInfo

	// Decode the string and automatically ignore any trailing junk data
	rawStr := stdout.String()

	startIdx := strings.IndexAny(rawStr, "{")
	if startIdx == -1 {
		return nil, fmt.Errorf("json_unmarshal_error: no valid JSON start found")
	}

	cleanStr := rawStr[startIdx:]

	decoder := json.NewDecoder(strings.NewReader(cleanStr))
	if err := decoder.Decode(&info); err != nil {
		return nil, fmt.Errorf("json_unmarshal_error: %w", err)
	}

	// Filter duplicate formats (keep only one per unique height/resolution)
	if len(info.Formats) > 0 {
		filtered := make([]YTDLPFormat, 0)
		seenHeights := make(map[int]bool)

		for i := len(info.Formats) - 1; i >= 0; i-- {
			f := info.Formats[i]
			vcodec := strings.ToLower(f.VCodec)

			isAudio := vcodec == "none" || f.Resolution == "audio only"

			if isAudio {
				// For audio, use FormatID or combination of ext+filesize as key if needed,
				// but usually audio formats are distinct enough.
				// Let's just keep them all for now or filter by ext if needed.
				filtered = append(filtered, f)
			} else if f.Height > 0 {
				if !seenHeights[f.Height] {
					filtered = append(filtered, f)
					seenHeights[f.Height] = true
				}
			} else {
				// Other formats (unknown height)
				filtered = append(filtered, f)
			}
		}
		// Reverse back to maintain original order (usually best quality last or first)
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
		info.Formats = filtered
	}

	return &info, nil
}

func ProcessYTDLPUpload(ctx context.Context, url, formatID, path, taskID, downloadType string, cfg *config.Config, owner string) {
	ytdlpEnabled := cfg.YTDLPPath != "disabled" && cfg.YTDLPPath != "disable"
	ffmpegEnabled := cfg.FFMPEGPath != "disabled" && cfg.FFMPEGPath != "disable"
	if !ytdlpEnabled || !ffmpegEnabled {
		UpdateTaskWithFile(taskID, "error", 0, "ytdlp_disabled", "", owner, 0, 0)
		return
	}

	if !IsValidURL(url) {
		UpdateTaskWithFile(taskID, "error", 0, "invalid_url_format", "", owner, 0, 0)
		return
	}

	if utils.IsPrivateIP(url) {
		UpdateTaskWithFile(taskID, "error", 0, "forbidden_url", "", owner, 0, 0)
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register for cancellation
	TaskMutex.Lock()
	TaskCancels[taskID] = cancel
	TaskMutex.Unlock()
	defer func() {
		TaskMutex.Lock()
		delete(TaskCancels, taskID)
		TaskMutex.Unlock()
	}()

	UpdateTaskWithFile(taskID, "waiting_slot", 0, "waiting_slot", "", owner, 0, 0)

	// Wait for a slot in the global download queue
	select {
	case globalDownloadSemaphore <- struct{}{}:
		defer func() { <-globalDownloadSemaphore }()
	case <-ctx.Done():
		UpdateTaskWithFile(taskID, "error", 0, "cancelled", "", owner, 0, 0)
		return
	}

	UpdateTaskWithFile(taskID, "downloading", 0, "initiating_ytdlp", "", owner, 0, 0)

	// Use a unique filename for the download to avoid collisions
	tempFileName := fmt.Sprintf("ytdlp_%s_%%(title)s.%%(ext)s", taskID)
	tempPathPattern := filepath.Join(cfg.TempDir, tempFileName)

	baseArgs := []string{
		"--newline",
		"--no-playlist",
		"--js-runtimes", "node",
		"-o", tempPathPattern,
	}

	// Audio conversion logic
	if downloadType == "audio" {
		baseArgs = append(baseArgs, "--extract-audio", "--audio-format", "mp3", "--embed-thumbnail", "--add-metadata", "--convert-thumbnails", "jpg")
	}

	// Check for user cookie file
	cookieFile := filepath.Join(cfg.CookiesDir, fmt.Sprintf("user_%s.txt", owner))
	if _, err := os.Stat(cookieFile); err == nil {
		baseArgs = append(baseArgs, "--cookies", cookieFile)
	}

	// Build format args for primary attempt and fallback
	var primaryFormatArgs, fallbackFormatArgs []string
	if formatID != "" {
		switch downloadType {
		case "video":
			primaryFormatArgs = []string{"-f", formatID + "+bestaudio/best", "--merge-output-format", "mp4"}
			fallbackFormatArgs = []string{"-f", "best", "--merge-output-format", "mp4"}
		case "audio":
			primaryFormatArgs = []string{"-f", formatID}
			fallbackFormatArgs = []string{"-f", "bestaudio/best"}
		}
	} else {
		switch downloadType {
		case "audio":
			primaryFormatArgs = []string{"-f", "bestaudio/best"}
			fallbackFormatArgs = primaryFormatArgs
		default:
			primaryFormatArgs = []string{"-f", "bestvideo*+bestaudio/bestvideo+bestaudio/best", "--merge-output-format", "mp4"}
			fallbackFormatArgs = []string{"-f", "best", "--merge-output-format", "mp4"}
		}
	}

	// Try primary format first; on format failure, retry with fallback
	type runResult struct {
		stderr    string
		exitOK    bool
		timedOut  bool
	}
	attempts := [][]string{primaryFormatArgs}
	if fmt.Sprintf("%v", primaryFormatArgs) != fmt.Sprintf("%v", fallbackFormatArgs) {
		attempts = append(attempts, fallbackFormatArgs)
	}

	var lastResult runResult
	for attemptIdx, formatArgs := range attempts {
		args := append(append([]string{}, baseArgs...), formatArgs...)
		args = append(args, url) // URL must be last

		if attemptIdx > 0 {
			UpdateTask(taskID, "downloading", 0, "ytdlp_retry_best", owner)
			log.Printf("[YTDLP] Retry with fallback format for task %s (attempt %d)", taskID, attemptIdx+1)
		}

		ytdlpCtx, cancelTimeout := context.WithTimeout(ctx, 1*time.Hour)

		cmd := exec.CommandContext(ytdlpCtx, cfg.YTDLPPath, args...)
		cmd.Env = os.Environ()
		setProcessGroup(cmd)

		var stderrBuf strings.Builder
		pr, pw := io.Pipe()
		stderrWriter := io.MultiWriter(pw, &stderrBuf)
		cmd.Stdout = pw
		cmd.Stderr = stderrWriter

		if err := cmd.Start(); err != nil {
			cancelTimeout()
			pw.Close()
			UpdateTaskWithFile(taskID, "error", 0, "start_error", "", owner, 0, 0)
			return
		}

		go func() {
			_ = cmd.Wait()
			pw.Close()
		}()

		go func() {
			<-ytdlpCtx.Done()
			killProcessGroup(cmd)
		}()

		progressRegex := regexp.MustCompile(`\[download\]\s+(\d+(?:\.\d+)?)%`)
		lastPercent := -1
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Text()
			matches := progressRegex.FindStringSubmatch(line)
			if len(matches) > 1 {
				percent, _ := strconv.ParseFloat(matches[1], 64)
				p := int(percent)
				if p != lastPercent {
					UpdateTask(taskID, "downloading", p, "downloading", owner)
					lastPercent = p
				}
			} else {
				if strings.HasPrefix(line, "[Merger] Merging formats") {
					UpdateTask(taskID, "downloading", 100, "ytdlp_merging", owner)
				} else if strings.Contains(line, "Adding thumbnail to") || strings.HasPrefix(line, "[EmbedThumbnail]") {
					UpdateTask(taskID, "downloading", 100, "ytdlp_thumbnail", owner)
				}
			}
		}

		timedOut := ytdlpCtx.Err() == context.DeadlineExceeded
		exitOK := cmd.ProcessState != nil && cmd.ProcessState.Success()
		lastResult = runResult{stderr: stderrBuf.String(), exitOK: exitOK, timedOut: timedOut}
		cancelTimeout()

		if exitOK {
			break // success, no need to retry
		}

		// Check if this is a format-related error worth retrying
		errMsg := lastResult.stderr
		isFormatErr := strings.Contains(errMsg, "Requested format is not available") ||
			strings.Contains(errMsg, "no matches") ||
			strings.Contains(errMsg, "format") && strings.Contains(errMsg, "not available")

		if !isFormatErr || attemptIdx == len(attempts)-1 {
			break // not a format error, or last attempt — don't retry
		}
	}

	if ctx.Err() != nil {
		UpdateTask(taskID, "error", 0, "cancelled", owner)
		return
	}
	if lastResult.timedOut {
		UpdateTask(taskID, "error", 0, "ytdlp_timeout", owner)
		return
	}
	if !lastResult.exitOK {
		errMsg := lastResult.stderr
		if idx := strings.Index(errMsg, "ERROR:"); idx != -1 {
			errMsg = strings.TrimSpace(errMsg[idx+6:])
		}
		cleanErr := translateYTDLPError(errMsg)
		UpdateTask(taskID, "error", 0, cleanErr, owner)
		return
	}

	// Find the downloaded file
	files, err := os.ReadDir(cfg.TempDir)
	if err != nil {
		UpdateTask(taskID, "error", 0, "read_temp_dir_failed", owner)
		return
	}

	var downloadedFile string
	prefix := "ytdlp_" + taskID + "_"

	// Better selection: skip intermediate files like .part, .ytdl or .fXXX extensions
	var candidates []string
	for _, f := range files {
		if f.IsDir() || !strings.HasPrefix(f.Name(), prefix) {
			continue
		}

		name := f.Name()
		// Skip temporary/partial files
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".temp") {
			continue
		}

		// Check for intermediate format extensions like .f248.webm or .f140.m4a
		isIntermediate := false
		parts := strings.Split(name, ".")
		for _, p := range parts {
			if strings.HasPrefix(p, "f") && len(p) > 1 {
				if _, err := strconv.Atoi(p[1:]); err == nil {
					isIntermediate = true
					break
				}
			}
		}

		if !isIntermediate {
			downloadedFile = filepath.Join(cfg.TempDir, name)
			break
		}
		candidates = append(candidates, filepath.Join(cfg.TempDir, name))
	}

	// Fallback to first candidate if no "clean" file found
	if downloadedFile == "" && len(candidates) > 0 {
		downloadedFile = candidates[0]
	}

	if downloadedFile == "" {
		UpdateTask(taskID, "error", 0, "downloaded_file_not_found", owner)
		return
	}

	// Ensure cleanup
	defer os.Remove(downloadedFile)

	// Prepare for upload
	filename := filepath.Base(downloadedFile)
	// Strip the ytdlp_taskID_ prefix
	filename = strings.TrimPrefix(filename, prefix)

	// Refine MIME type based on actual extension
	mimeType := "application/octet-stream"
	ext := filepath.Ext(filename)
	if ext != "" {
		mimeType = mime.TypeByExtension(ext)
	}

	// Fallback for common types if mime package is incomplete
	if mimeType == "" || mimeType == "application/octet-stream" {
		switch strings.ToLower(ext) {
		case ".mp4", ".m4v":
			mimeType = "video/mp4"
		case ".webm":
			mimeType = "video/webm"
		case ".mkv":
			mimeType = "video/x-matroska"
		case ".mp3":
			mimeType = "audio/mpeg"
		case ".m4a":
			mimeType = "audio/mp4"
		case ".ogg", ".opus":
			mimeType = "audio/ogg"
		}
	}

	// Call existing upload logic
	ProcessCompleteUpload(ctx, downloadedFile, filename, path, mimeType, taskID, cfg, false, owner)
}
