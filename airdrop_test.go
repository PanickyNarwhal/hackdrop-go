package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hackdrop-go/pkg/airdrop"
	"hackdrop-go/pkg/ble"
	"hackdrop-go/pkg/transport"
	"hackdrop-go/pkg/transport/awdl"
	"hackdrop-go/pkg/transport/wifiaware"
)

func TestBLEContinuityEncodeAndParse(t *testing.T) {
	frame := ble.NewAirDropFrame("user@example.com", "+15551234567", "user@example.com", true)
	raw := frame.EncodeManufacturerData(true)
	if len(raw) != 22 {
		t.Fatalf("expected 22-byte Apple Continuity BLE frame, got %d", len(raw))
	}

	peer, err := ble.ParseAppleManufacturerData("AA:BB:CC:DD:EE:FF", -48, raw)
	if err != nil {
		t.Fatalf("ParseAppleManufacturerData failed: %v", err)
	}
	if !peer.HasAirDrop || peer.AirDropFrame == nil {
		t.Fatalf("expected HasAirDrop=true")
	}
	if !peer.AirDropFrame.SupportsNAN {
		t.Fatalf("expected SupportsNAN=true")
	}
	if peer.AirDropFrame.EmailHash != frame.EmailHash {
		t.Fatalf("email hash mismatch: got %x, want %x", peer.AirDropFrame.EmailHash, frame.EmailHash)
	}
}

func TestBinaryPlistEncoding(t *testing.T) {
	bplist := airdrop.EncodeBinaryPlist(map[string]any{
		"ReceiverComputerName": "HackDrop-PC",
		"ReceiverModelName":    "MacBookPro16,1",
	})
	if !bytes.HasPrefix(bplist, []byte("bplist00")) {
		t.Fatalf("expected bplist00 magic header")
	}
}

func TestAWDLActionFrameAndEUI64(t *testing.T) {
	eng := awdl.NewEngine("awdl0")
	psf := eng.BuildActionFrame(awdl.SubtypePSF)
	srcMAC := net.HardwareAddr{0x02, 0x17, 0xf2, 0x11, 0x22, 0x33}

	info, err := eng.ParseActionFrame(srcMAC, -52, psf)
	if err != nil {
		t.Fatalf("ParseActionFrame failed: %v", err)
	}
	expectedIP := transport.DeriveEUI64LinkLocal(srcMAC)
	if !info.IPv6LinkLocal.Equal(expectedIP) {
		t.Fatalf("expected IPv6 link-local %s, got %s", expectedIP, info.IPv6LinkLocal)
	}
}

func TestWiFiAwareNANServiceDiscoveryFrame(t *testing.T) {
	eng := wifiaware.NewEngine()
	sdf := eng.BuildServiceDiscoveryFrame(true)
	if len(sdf) < 20 {
		t.Fatalf("expected valid NAN SDF frame, got %d bytes", len(sdf))
	}
}

func TestCPIORoundTripAndAirDropTransfer(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "hello-hackdrop.txt")
	payload := []byte("HackDrop CPIO 070707 POSIX archive verification payload!")
	if err := os.WriteFile(srcFile, payload, 0644); err != nil {
		t.Fatal(err)
	}

	archive, err := airdrop.CreateGzipCPIOArchive([]string{srcFile})
	if err != nil {
		t.Fatalf("CreateGzipCPIOArchive failed: %v", err)
	}

	outDir := filepath.Join(tmpDir, "extracted")
	extracted, err := airdrop.ExtractGzipCPIOArchive(bytes.NewReader(archive), outDir, nil)
	if err != nil {
		t.Fatalf("ExtractGzipCPIOArchive failed: %v", err)
	}
	if len(extracted) != 1 {
		t.Fatalf("expected 1 extracted file, got %d", len(extracted))
	}

	gotBytes, err := os.ReadFile(extracted[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("extracted file content mismatch")
	}

	// Verify end-to-end /Discover -> /Ask -> /Upload HTTPS transfer with bplist00
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recvDir := filepath.Join(tmpDir, "downloads")
	srv, err := airdrop.NewServer("test01", "Test-PC", "PC-Win11", recvDir, 18771, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAutoAccept(true)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := srv.SendFiles(ctx, "127.0.0.1", 18771, []string{srcFile}); err != nil {
		t.Fatalf("SendFiles E2E failed: %v", err)
	}

	savedPath := filepath.Join(recvDir, "hello-hackdrop.txt")
	savedBytes, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("expected received file at %s: %v", savedPath, err)
	}
	if !bytes.Equal(savedBytes, payload) {
		t.Fatalf("received AirDrop file content mismatch")
	}
}
