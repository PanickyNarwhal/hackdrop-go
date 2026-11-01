package main

import (
	"context"

	"hackdrop-go/pkg/daemon"
	"hackdrop-go/pkg/mdns"
)

// App wraps the HackDrop Coordinator and exposes methods to both Wails frontend bindings
// and the embedded desktop WebView RPC bridge.
type App struct {
	ctx         context.Context
	coordinator *daemon.Coordinator
}

func NewApp(coord *daemon.Coordinator) *App {
	return &App{
		coordinator: coord,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	_ = a.coordinator.Start(ctx)
}

func (a *App) shutdown(ctx context.Context) {
	a.coordinator.Stop()
}

func (a *App) GetState() daemon.SystemState {
	return a.coordinator.State()
}

func (a *App) TriggerWakeup() string {
	return a.coordinator.TriggerBLEWakeup()
}

func (a *App) ScanSubnet() int {
	return a.coordinator.ScanLocalSubnet()
}

func (a *App) AddPeer(displayName, modelName, ipAddr string, port int) mdns.PeerService {
	return a.coordinator.AddManualPeer(displayName, modelName, ipAddr, port)
}

func (a *App) SendFiles(peerID string, filePaths []string) error {
	return a.coordinator.SendFilesToPeer(peerID, filePaths)
}

func (a *App) RespondPrompt(transferID string, accept bool) bool {
	return a.coordinator.RespondToPrompt(transferID, accept)
}

func (a *App) SaveSettings(computerName, visibilityMode string, autoAccept bool) {
	a.coordinator.UpdateSettings(computerName, visibilityMode, autoAccept)
}
