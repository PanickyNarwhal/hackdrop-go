package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"hackdrop-go/pkg/daemon"
)

//go:embed frontend/dist/*
var embeddedAssets embed.FS

func main() {
	headless := flag.Bool("cli", false, "Run in headless CLI mode without opening the desktop GUI window")
	uiPort := flag.Int("ui-port", 0, "Local port for the desktop UI bridge (0 = auto)")
	flag.Parse()

	coord, err := daemon.NewCoordinator()
	if err != nil {
		log.Fatalf("Failed to initialize HackDrop coordinator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := NewApp(coord)
	app.startup(ctx)
	defer app.shutdown(ctx)

	state := app.GetState()
	fmt.Printf("============================================================\n")
	fmt.Printf("  HackDrop (FOSS AWDL / NAN / IPv6 AirDrop Bridge)\n")
	fmt.Printf("  Node Name : %s (%s)\n", state.ComputerName, state.DeviceID)
	fmt.Printf("  Local IP  : %s:%d (IPv6 %s)\n", state.LocalIPv4, state.HTTPSPort, state.LocalIPv6)
	fmt.Printf("  Downloads : %s\n", state.DownloadDir)
	fmt.Printf("  BLE Frame : %s\n", state.BLEFrameHex)
	fmt.Printf("  Version   : 0.2.1-alpha\n")
	fmt.Printf("============================================================\n")

	if !*headless {
		url, err := startDesktopUIServer(ctx, app, *uiPort)
		if err != nil {
			log.Fatalf("Failed to start UI server: %v", err)
		}
		fmt.Printf("  Desktop UI: %s\n", url)
		launchNativeAppWindow(url)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\nShutting down HackDrop cleanly...")
}

func startDesktopUIServer(ctx context.Context, app *App, preferredPort int) (string, error) {
	distFS, err := fs.Sub(embeddedAssets, "frontend/dist")
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(distFS)))

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, app.GetState())
	})

	mux.HandleFunc("/api/wakeup", func(w http.ResponseWriter, r *http.Request) {
		hexStr := app.TriggerWakeup()
		writeJSON(w, map[string]string{"status": "ok", "bleFrameHex": hexStr})
	})

	mux.HandleFunc("/api/scan-subnet", func(w http.ResponseWriter, r *http.Request) {
		found := app.ScanSubnet()
		writeJSON(w, map[string]any{"status": "ok", "found": found})
	})

	mux.HandleFunc("/api/peer/add", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DisplayName string `json:"displayName"`
			ModelName   string `json:"modelName"`
			IPAddr      string `json:"ipAddr"`
			Port        int    `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		peer := app.AddPeer(req.DisplayName, req.ModelName, req.IPAddr, req.Port)
		writeJSON(w, peer)
	})

	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PeerID    string   `json:"peerId"`
			FilePaths []string `json:"filePaths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := app.SendFiles(req.PeerID, req.FilePaths); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"status": "sent"})
	})

	mux.HandleFunc("/api/upload-temp", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(512 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		peerID := r.FormValue("peerId")
		files := r.MultipartForm.File["files"]
		tmpDir := filepath.Join(os.TempDir(), "hackdrop-staging")
		_ = os.MkdirAll(tmpDir, 0755)

		var stagedPaths []string
		for _, fh := range files {
			src, err := fh.Open()
			if err != nil {
				continue
			}
			dstPath := filepath.Join(tmpDir, filepath.Base(fh.Filename))
			dst, err := os.Create(dstPath)
			if err != nil {
				_ = src.Close()
				continue
			}
			_, _ = dst.ReadFrom(src)
			_ = dst.Close()
			_ = src.Close()
			stagedPaths = append(stagedPaths, dstPath)
		}

		if err := app.SendFiles(peerID, stagedPaths); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"status": "sent", "files": stagedPaths})
	})

	mux.HandleFunc("/api/prompt", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TransferID string `json:"transferId"`
			Accept     bool   `json:"accept"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ok := app.RespondPrompt(req.TransferID, req.Accept)
		writeJSON(w, map[string]bool{"resolved": ok})
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ComputerName   string `json:"computerName"`
			VisibilityMode string `json:"visibilityMode"`
			AutoAccept     bool   `json:"autoAccept"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		app.SaveSettings(req.ComputerName, req.VisibilityMode, req.AutoAccept)
		writeJSON(w, app.GetState())
	})

	addr := fmt.Sprintf("127.0.0.1:%d", preferredPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		_ = srv.Serve(ln)
	}()

	return fmt.Sprintf("http://%s", ln.Addr().String()), nil
}

// launches the UI in an app window
func launchNativeAppWindow(url string) {
	switch runtime.GOOS {
	case "windows":
		edgePaths := []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		}
		for _, p := range edgePaths {
			if _, err := os.Stat(p); err == nil {
				cmd := exec.Command(p, "--app="+url, "--window-size=1060,720")
				_ = cmd.Start()
				return
			}
		}
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		chromeApp := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(chromeApp); err == nil {
			_ = exec.Command(chromeApp, "--app="+url, "--window-size=1060,720").Start()
			return
		}
		_ = exec.Command("open", url).Start()
	case "linux":
		_ = exec.Command("xdg-open", url).Start()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
