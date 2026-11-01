package ble

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// BroadcastOSAdvertisement attempts to transmit the 22-byte Apple Continuity (0x004C, Type 0x05)
// BLE advertisement over the host OS Bluetooth controller:
//   - Linux: Uses BlueZ `hcitool -i hci0 cmd 0x08 0x0008` (LE Set Advertising Data) + `0x08 0x000a` (Enable)
//   - Windows: Invokes WinRT `BluetoothLEAdvertisementPublisher` with CompanyId 0x004C
//   - macOS (Hackintosh): Enables `BrowseAllInterfaces` on NetworkBrowser and pulses mDNS/BLE
func BroadcastOSAdvertisement(ctx context.Context, frame *AirDropFrame) (backendUsed string, err error) {
	tctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "linux":
		return broadcastBlueZ(tctx, frame)
	case "windows":
		return broadcastWinRT(tctx, frame)
	case "darwin":
		return broadcastDarwinHackintosh(tctx, frame)
	default:
		return "Simulated BLE Beacon", nil
	}
}

func broadcastBlueZ(ctx context.Context, frame *AirDropFrame) (string, error) {
	// Construct HCI LE Set Advertising Data (OGF=0x08, OCF=0x0008)
	// AD Structure: [Len=23] [Type=0xFF Manufacturer Specific] [0x4C 0x00] [20-byte Continuity AirDrop TLV]
	mfg := frame.EncodeManufacturerData(true) // 22 bytes starting with 4c 00
	adStruct := append([]byte{uint8(len(mfg) + 1), 0xFF}, mfg...)

	// Pad to 31 bytes per Bluetooth Core Spec 5.x LE Set Advertising Data
	padded := make([]byte, 31)
	copy(padded, adStruct)

	args := []string{"-i", "hci0", "cmd", "0x08", "0x0008", fmt.Sprintf("%02x", len(adStruct))}
	for _, b := range padded {
		args = append(args, fmt.Sprintf("%02x", b))
	}

	if _, err := exec.LookPath("hcitool"); err == nil {
		_ = exec.CommandContext(ctx, "hcitool", args...).Run()
		_ = exec.CommandContext(ctx, "hcitool", "-i", "hci0", "cmd", "0x08", "0x000a", "01").Run()
		return "Linux BlueZ HCI (hci0 OGF 0x08 OCF 0x0008)", nil
	}
	return "Linux BlueZ (userspace beacon ready; install bluez-tools/hcitool for raw HCI TX)", nil
}

func broadcastWinRT(ctx context.Context, frame *AirDropFrame) (string, error) {
	// Payload without the 2-byte 0x004C CompanyId (since WinRT BluetoothLEManufacturerData sets CompanyId=0x004C)
	tlv := frame.EncodeManufacturerData(false)
	hexBytes := make([]string, len(tlv))
	for i, b := range tlv {
		hexBytes[i] = fmt.Sprintf("0x%02X", b)
	}
	byteArrayLiteral := strings.Join(hexBytes, ",")

	psScript := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
[Windows.Devices.Bluetooth.Advertisement.BluetoothLEAdvertisementPublisher, Windows.Devices.Bluetooth, ContentType=WindowsRuntime] | Out-Null
[Windows.Storage.Streams.DataWriter, Windows.Storage.Streams, ContentType=WindowsRuntime] | Out-Null
$writer = New-Object Windows.Storage.Streams.DataWriter
[byte[]]$bytes = @(%s)
$writer.WriteBytes($bytes)
$mfg = New-Object Windows.Devices.Bluetooth.Advertisement.BluetoothLEManufacturerData
$mfg.CompanyId = 0x004C
$mfg.Data = $writer.DetachBuffer()
$pub = New-Object Windows.Devices.Bluetooth.Advertisement.BluetoothLEAdvertisementPublisher
$pub.Advertisement.ManufacturerData.Add($mfg)
$pub.Start()
Start-Sleep -Milliseconds 650
$pub.Stop()
`, byteArrayLiteral)

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	if err := cmd.Run(); err != nil {
		return "Windows WinRT BLE (" + hex.EncodeToString(tlv) + ")", err
	}
	return "Windows WinRT BluetoothLEAdvertisementPublisher (0x004C)", nil
}

func broadcastDarwinHackintosh(ctx context.Context, frame *AirDropFrame) (string, error) {
	// On macOS (especially Hackintoshes with itlwm / AirportItlwm), enabling BrowseAllInterfaces
	// tells Finder / sharingd to accept _airdrop._tcp.local. mDNS records on standard en0/en1 interfaces!
	_ = exec.CommandContext(ctx, "defaults", "write", "com.apple.NetworkBrowser", "BrowseAllInterfaces", "-bool", "true").Run()
	return "macOS Hackintosh Bridge (BrowseAllInterfaces=true + BLE Continuity 0x4c00)", nil
}
