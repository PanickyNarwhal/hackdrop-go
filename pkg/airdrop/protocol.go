package airdrop

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAirDropPort is the standard TCP port listened on by Apple's sharingd for AirDrop HTTPS.
	DefaultAirDropPort = 8770
)

// FileEntry describes a single file inside an AirDrop `/Ask` request or outbound transfer.
type FileEntry struct {
	FileName            string `json:"fileName"`
	FileType            string `json:"fileType"`
	FileBomPath         string `json:"fileBomPath"`
	FileIsDirectory     bool   `json:"fileIsDirectory"`
	ConvertMediaFormats bool   `json:"convertMediaFormats"`
	FileSize            int64  `json:"fileSize,omitempty"`
}

// TransferPrompt represents an incoming AirDrop transfer waiting for user acceptance in the GUI.
type TransferPrompt struct {
	TransferID         string      `json:"transferId"`
	SenderComputerName string      `json:"senderComputerName"`
	SenderModelName    string      `json:"senderModelName"`
	SenderIP           string      `json:"senderIp"`
	SenderID           string      `json:"senderId"`
	Files              []FileEntry `json:"files"`
	CreatedAt          time.Time   `json:"createdAt"`
	Status             string      `json:"status"`
}

// TransferEvent notifies the Wails GUI about incoming prompts, active progress, and completed transfers.
type TransferEvent struct {
	Type         string          `json:"type"`
	TransferID   string          `json:"transferId"`
	PeerName     string          `json:"peerName"`
	BytesCopied  int64           `json:"bytesCopied"`
	SavedFiles   []string        `json:"savedFiles,omitempty"`
	Prompt       *TransferPrompt `json:"prompt,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
}

// Server implements Apple's AirDrop HTTPS server (`/Discover`, `/Ask`, `/Upload`)
// and outbound client so this PC can both receive files from and send files to iPhones, iPads, and Macs.
type Server struct {
	mu              sync.Mutex
	deviceID        string
	computerName    string
	modelName       string
	port            int
	downloadDir     string
	autoAccept      bool
	tlsConfig       *tls.Config
	httpServer      *http.Server
	pendingAsks     map[string]chan bool
	approvedSenders map[string]time.Time // Security gate: tracks remote IPs approved via /Ask
	history         []TransferEvent
	onEvent         func(TransferEvent)
}

func NewServer(deviceID, computerName, modelName, downloadDir string, port int, onEvent func(TransferEvent)) (*Server, error) {
	tlsCfg, err := GenerateAirDropTLSConfig(deviceID)
	if err != nil {
		return nil, err
	}

	if downloadDir == "" {
		home, _ := os.UserHomeDir()
		downloadDir = filepath.Join(home, "Downloads", "HackDrop")
	}

	return &Server{
		deviceID:        deviceID,
		computerName:    computerName,
		modelName:       modelName,
		port:            port,
		downloadDir:     downloadDir,
		autoAccept:      false,
		tlsConfig:       tlsCfg,
		pendingAsks:     make(map[string]chan bool),
		approvedSenders: make(map[string]time.Time),
		onEvent:         onEvent,
	}, nil
}

// Start launches the TLS 1.2/1.3 HTTPS server on `[::]:8770` (or falls back to `:8771` if macOS sharingd holds 8770).
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/Discover", s.handleDiscover)
	mux.HandleFunc("/Ask", s.handleAsk)
	mux.HandleFunc("/Upload", s.handleUpload)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		// On macOS, native sharingd may already hold 8770; fall back to 8771
		fallbackPort := s.port + 1
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", fallbackPort))
		if err != nil {
			return fmt.Errorf("failed to bind AirDrop HTTPS port %d or %d: %w", s.port, fallbackPort, err)
		}
		s.port = fallbackPort
	}

	tlsListener := tls.NewListener(ln, s.tlsConfig)
	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = s.httpServer.Close()
	}()

	go func() {
		_ = s.httpServer.Serve(tlsListener)
	}()

	return nil
}

// Port returns the active TCP port bound by the AirDrop HTTPS server.
func (s *Server) Port() int {
	return s.port
}

// ProbePeer sends a quick TLS `POST /Discover` handshake to a target IP (:8770 or :8771)
// and extracts the remote peer's ReceiverComputerName and ReceiverModelName if reachable.
func (s *Server) ProbePeer(ctx context.Context, targetHost string, preferredPort int) (compName, modelName string, activePort int, err error) {
	ports := []int{8770, 8771}
	if preferredPort != 0 && preferredPort != 8770 && preferredPort != 8771 {
		ports = append([]int{preferredPort}, ports...)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: s.tlsConfig,
		},
		Timeout: 2500 * time.Millisecond,
	}

	discoverBody := buildDiscoverRequestBinaryPlist()
	for _, p := range ports {
		hostPort := fmt.Sprintf("%s:%d", targetHost, p)
		if strings.Contains(targetHost, ":") && !strings.HasPrefix(targetHost, "[") {
			hostPort = fmt.Sprintf("[%s]:%d", targetHost, p)
		}
		url := fmt.Sprintf("https://%s/Discover", hostPort)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(discoverBody))
		if reqErr != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("User-Agent", "AirDrop/1.0")

		resp, doErr := client.Do(req)
		if doErr != nil {
			continue
		}
		respBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		cName, mName := parseDiscoverResponsePayload(respBytes, targetHost)
		return cName, mName, p, nil
	}

	return "", "", preferredPort, fmt.Errorf("no AirDrop HTTPS listener responded at %s (ports 8770/8771)", targetHost)
}

func (s *Server) SetAutoAccept(auto bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoAccept = auto
}

func (s *Server) RespondToAsk(transferID string, accept bool) bool {
	s.mu.Lock()
	ch, ok := s.pendingAsks[transferID]
	if ok {
		delete(s.pendingAsks, transferID)
	}
	s.mu.Unlock()

	if ok {
		ch <- accept
		return true
	}
	return false
}

// handleDiscover responds to Apple's `POST /Discover` request with a `bplist00` binary plist
// containing ReceiverComputerName, ReceiverModelName, and ReceiverMediaCapabilities.
func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	defer r.Body.Close()

	mediaCaps := map[string]int{"Version": 1}
	mediaCapsBytes, _ := json.Marshal(mediaCaps)

	respBytes := buildReceiverBinaryPlist(s.computerName, s.modelName, s.deviceID, mediaCapsBytes)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)

	s.emit(TransferEvent{
		Type:      "discover",
		PeerName:  r.RemoteAddr,
		Timestamp: time.Now(),
	})
}

// handleAsk parses an incoming `POST /Ask` request (`bplist00` or XML plist), prompts the user
// unless AutoAccept is enabled, and records approval for `POST /Upload`.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()

	senderName, senderModel, files := parseIncomingAskPayload(body)
	transferID := fmt.Sprintf("xfer-%d", time.Now().UnixNano())

	prompt := &TransferPrompt{
		TransferID:         transferID,
		SenderComputerName: senderName,
		SenderModelName:    senderModel,
		SenderIP:           r.RemoteAddr,
		Files:              files,
		CreatedAt:          time.Now(),
		Status:             "pending",
	}

	s.mu.Lock()
	auto := s.autoAccept
	decisionCh := make(chan bool, 1)
	if !auto {
		s.pendingAsks[transferID] = decisionCh
	}
	s.mu.Unlock()

	s.emit(TransferEvent{
		Type:       "ask",
		TransferID: transferID,
		PeerName:   senderName,
		Prompt:     prompt,
		Timestamp:  time.Now(),
	})

	accepted := true
	if !auto {
		select {
		case accepted = <-decisionCh:
		case <-time.After(60 * time.Second):
			accepted = false
		case <-r.Context().Done():
			accepted = false
		}
	}

	if !accepted {
		http.Error(w, "Declined by user", http.StatusUnauthorized)
		return
	}

	// Authorize this remote IP to perform POST /Upload for the next 5 minutes
	remoteHost := extractRemoteIP(r.RemoteAddr)
	s.mu.Lock()
	s.approvedSenders[remoteHost] = time.Now().Add(5 * time.Minute)
	s.mu.Unlock()

	respBytes := buildAskAcceptBinaryPlist(s.computerName, s.modelName)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

// handleUpload verifies that the sender was approved via `/Ask` (or AutoAccept is enabled)
// before extracting the incoming `POST /Upload` gzipped CPIO stream.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	remoteHost := extractRemoteIP(r.RemoteAddr)
	s.mu.Lock()
	expiry, approved := s.approvedSenders[remoteHost]
	auto := s.autoAccept
	if approved && time.Now().Before(expiry) {
		delete(s.approvedSenders, remoteHost)
	} else {
		approved = false
	}
	s.mu.Unlock()

	if !approved && !auto {
		http.Error(w, "Unauthorized: /Upload requires prior /Ask approval", http.StatusForbidden)
		return
	}

	transferID := fmt.Sprintf("upload-%d", time.Now().UnixNano())
	extractedFiles, err := ExtractGzipCPIOArchive(r.Body, s.downloadDir, func(bytesRead int64) {
		s.emit(TransferEvent{
			Type:        "progress",
			TransferID:  transferID,
			PeerName:    r.RemoteAddr,
			BytesCopied: bytesRead,
			Timestamp:   time.Now(),
		})
	})

	if err != nil {
		s.emit(TransferEvent{
			Type:         "error",
			TransferID:   transferID,
			PeerName:     r.RemoteAddr,
			ErrorMessage: err.Error(),
			Timestamp:    time.Now(),
		})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	s.emit(TransferEvent{
		Type:       "complete",
		TransferID: transferID,
		PeerName:   r.RemoteAddr,
		SavedFiles: extractedFiles,
		Timestamp:  time.Now(),
	})
}

// SendFiles executes the full 3-step outbound AirDrop client flow (`/Discover` -> `/Ask` -> `/Upload`)
// using `bplist00` binary property lists and a POSIX `070707` Gzip CPIO archive stream.
func (s *Server) SendFiles(ctx context.Context, targetHost string, targetPort int, filePaths []string) error {
	if len(filePaths) == 0 {
		return fmt.Errorf("no files selected to send")
	}
	if targetPort == 0 {
		targetPort = DefaultAirDropPort
	}

	hostPort := fmt.Sprintf("%s:%d", targetHost, targetPort)
	if strings.Contains(targetHost, ":") && !strings.HasPrefix(targetHost, "[") {
		hostPort = fmt.Sprintf("[%s]:%d", targetHost, targetPort)
	}
	baseURL := fmt.Sprintf("https://%s", hostPort)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: s.tlsConfig,
		},
		Timeout: 90 * time.Second,
	}

	// Step 1: POST /Discover (bplist00)
	discoverBody := buildDiscoverRequestBinaryPlist()
	reqDisc, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/Discover", bytes.NewReader(discoverBody))
	if err != nil {
		return err
	}
	reqDisc.Header.Set("Content-Type", "application/octet-stream")
	reqDisc.Header.Set("User-Agent", "AirDrop/1.0")
	respDisc, err := client.Do(reqDisc)
	if err != nil {
		return fmt.Errorf("AirDrop /Discover handshake failed against %s: %w", baseURL, err)
	}
	_ = respDisc.Body.Close()

	// Step 2: POST /Ask (bplist00)
	var entries []FileEntry
	for _, fp := range filePaths {
		info, err := os.Stat(fp)
		if err != nil {
			return err
		}
		entries = append(entries, FileEntry{
			FileName:            filepath.Base(fp),
			FileType:            detectAppleUTI(fp),
			FileBomPath:         "./" + filepath.Base(fp),
			FileIsDirectory:     info.IsDir(),
			ConvertMediaFormats: false,
			FileSize:            info.Size(),
		})
	}

	askBody := buildAskRequestBinaryPlist(s.computerName, s.modelName, s.deviceID, entries)
	reqAsk, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/Ask", bytes.NewReader(askBody))
	if err != nil {
		return err
	}
	reqAsk.Header.Set("Content-Type", "application/octet-stream")
	reqAsk.Header.Set("User-Agent", "AirDrop/1.0")
	respAsk, err := client.Do(reqAsk)
	if err != nil {
		return fmt.Errorf("AirDrop /Ask failed: %w", err)
	}
	_ = respAsk.Body.Close()
	if respAsk.StatusCode != http.StatusOK {
		return fmt.Errorf("recipient declined AirDrop transfer (HTTP %d)", respAsk.StatusCode)
	}

	// Step 3: POST /Upload (Gzip POSIX 070707 CPIO archive)
	archiveBytes, err := CreateGzipCPIOArchive(filePaths)
	if err != nil {
		return fmt.Errorf("failed to pack CPIO archive: %w", err)
	}

	reqUp, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/Upload", bytes.NewReader(archiveBytes))
	if err != nil {
		return err
	}
	reqUp.Header.Set("Content-Type", "application/x-cpio")
	reqUp.Header.Set("User-Agent", "AirDrop/1.0")
	respUp, err := client.Do(reqUp)
	if err != nil {
		return fmt.Errorf("AirDrop /Upload failed: %w", err)
	}
	_ = respUp.Body.Close()
	if respUp.StatusCode != http.StatusOK {
		return fmt.Errorf("AirDrop /Upload rejected by peer (HTTP %d)", respUp.StatusCode)
	}

	s.emit(TransferEvent{
		Type:        "complete",
		TransferID:  fmt.Sprintf("sent-%d", time.Now().UnixNano()),
		PeerName:    targetHost,
		BytesCopied: int64(len(archiveBytes)),
		SavedFiles:  filePaths,
		Timestamp:   time.Now(),
	})
	return nil
}

func extractRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func (s *Server) emit(ev TransferEvent) {
	s.mu.Lock()
	s.history = append(s.history, ev)
	cb := s.onEvent
	s.mu.Unlock()
	if cb != nil {
		cb(ev)
	}
}
