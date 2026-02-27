package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed yt-dlp-mac yt-dlp-win.exe
var embeddedBinaries embed.FS

const (
	PORT        = 7337
	YTDLP_REPO  = "yt-dlp/yt-dlp"
	CONFIG_DIR  = "tatatext-helper"
)

var (
	ytdlpPath    string
	ytdlpVersion string
	updateMu     sync.Mutex
)

func main() {
	ytdlpPath = extractYtDlp()
	ytdlpVersion = getYtDlpVersion(ytdlpPath)
	log.Printf("yt-dlp version: %s", ytdlpVersion)

	// Auto-update yt-dlp in background
	go autoUpdateYtDlp()

	mux := http.NewServeMux()

	// Health check + version info
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://tatatext.com")
		w.Header().Set("Content-Type", "application/json")
		updateMu.Lock()
		v := ytdlpVersion
		updateMu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{
			"status":       "ok",
			"version":      "1.0.0",
			"ytdlpVersion": v,
		})
	})

	// Audio download
	mux.HandleFunc("/audio", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://tatatext.com")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		youtubeURL := r.URL.Query().Get("url")
		if youtubeURL == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"url parameter required"}`, http.StatusBadRequest)
			return
		}

		updateMu.Lock()
		bin := ytdlpPath
		updateMu.Unlock()

		// Single yt-dlp call: get title + URL together via --print
		cmd := exec.Command(bin,
			"--no-playlist",
			"-f", "bestaudio[ext=m4a]/bestaudio",
			"--print", "%(title)s\n%(url)s",
			"--",
			youtubeURL,
		)
		out, err := cmd.Output()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, fmt.Sprintf(`{"error":"yt-dlp failed: %s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
		title := "YouTube Video"
		if len(lines) >= 1 && lines[0] != "" {
			title = lines[0]
		}
		if len(lines) < 2 || lines[1] == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"no audio URL found"}`, http.StatusInternalServerError)
			return
		}
		audioURL := strings.TrimSpace(lines[1])

		// Proxy the audio stream to the browser
		req, _ := http.NewRequest("GET", audioURL, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, fmt.Sprintf(`{"error":"download failed: %s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		safeTitle := sanitizeFilename(title)
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "audio/mp4"
		}
		ext := "m4a"
		if strings.Contains(ct, "webm") || strings.Contains(ct, "ogg") {
			ext = "webm"
		}

		w.Header().Set("Content-Type", ct)
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, safeTitle, ext))
		w.Header().Set("X-Video-Title", title)
		w.Header().Set("X-Video-Extension", ext)
		io.Copy(w, resp.Body)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", PORT)
	log.Printf("tatatext helper running on http://%s", addr)
	showNotification("tatatext Helper", "Running in background — YouTube transcription is now enabled.")

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// extractYtDlp writes the embedded yt-dlp binary to a persistent config dir.
// On next run it reuses the file unless it was replaced by auto-update.
func extractYtDlp() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = os.TempDir()
	}
	dir := filepath.Join(configDir, CONFIG_DIR)
	os.MkdirAll(dir, 0755)

	outName := "yt-dlp"
	if runtime.GOOS == "windows" {
		outName = "yt-dlp.exe"
	}
	outPath := filepath.Join(dir, outName)

	// Only extract if not already there (auto-update will overwrite)
	if _, err := os.Stat(outPath); os.IsNotExist(err) {
		binName := "yt-dlp-mac"
		if runtime.GOOS == "windows" {
			binName = "yt-dlp-win.exe"
		}
		data, err := embeddedBinaries.ReadFile(binName)
		if err != nil {
			log.Fatalf("failed to read embedded %s: %v", binName, err)
		}
		if err := os.WriteFile(outPath, data, 0755); err != nil {
			log.Fatal("failed to write yt-dlp:", err)
		}
		log.Printf("extracted embedded yt-dlp to %s", outPath)
	}

	return outPath
}

func getYtDlpVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// autoUpdateYtDlp checks GitHub releases and downloads a newer yt-dlp if available.
func autoUpdateYtDlp() {
	// Check on startup, then every 6 hours
	checkAndUpdate()
	ticker := time.NewTicker(6 * time.Hour)
	for range ticker.C {
		checkAndUpdate()
	}
}

func checkAndUpdate() {
	log.Println("checking for yt-dlp updates...")
	latestVersion, downloadURL, err := getLatestYtDlpRelease()
	if err != nil {
		log.Printf("update check failed: %v", err)
		return
	}

	updateMu.Lock()
	current := ytdlpVersion
	updateMu.Unlock()

	if current == latestVersion {
		log.Printf("yt-dlp is up to date (%s)", current)
		return
	}

	log.Printf("updating yt-dlp %s → %s", current, latestVersion)
	newPath, err := downloadYtDlp(downloadURL)
	if err != nil {
		log.Printf("update download failed: %v", err)
		return
	}

	updateMu.Lock()
	ytdlpPath = newPath
	ytdlpVersion = latestVersion
	updateMu.Unlock()
	log.Printf("yt-dlp updated to %s", latestVersion)
}

func getLatestYtDlpRelease() (version, downloadURL string, err error) {
	resp, err := http.Get(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", YTDLP_REPO))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", "", err
	}

	var assetName string
	if runtime.GOOS == "windows" {
		assetName = "yt-dlp.exe"
	} else {
		assetName = "yt-dlp_macos"
	}

	for _, asset := range release.Assets {
		if asset.Name == assetName {
			return release.TagName, asset.BrowserDownloadURL, nil
		}
	}
	return "", "", fmt.Errorf("asset %s not found in release", assetName)
}

func downloadYtDlp(url string) (string, error) {
	configDir, _ := os.UserConfigDir()
	dir := filepath.Join(configDir, CONFIG_DIR)

	outName := "yt-dlp"
	if runtime.GOOS == "windows" {
		outName = "yt-dlp.exe"
	}
	outPath := filepath.Join(dir, outName)
	tmpPath := outPath + ".tmp"

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	// Atomic replace
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", err
	}
	return outPath, nil
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	result := b.String()
	if len(result) > 80 {
		result = result[:80]
	}
	return result
}

func showNotification(title, message string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, message, title)
		exec.Command("osascript", "-e", script).Run()
	case "windows":
		ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('%s','%s')`, message, title)
		exec.Command("powershell", "-Command", ps).Run()
	}
}
