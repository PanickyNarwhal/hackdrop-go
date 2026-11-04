package airdrop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests encode/decode payloads for /discover /ask 
func TestBinaryPlistRoundTripAndHandshake(t *testing.T) {
	unicodeName := "Isaac\u2019s MacBook Pro"
	mediaCaps := []byte(`{"Version":1,"Vendor":{"com.apple":{"LivePhotoFormatVersion":"1"}}}`)

	discResp := buildReceiverBinaryPlist(unicodeName, "MacBookPro18,1", "a4e027e9680f", mediaCaps)
	gotName, gotModel := parseDiscoverResponsePayload(discResp, "10.0.0.233")
	if gotName != unicodeName {
		t.Fatalf("expected ReceiverComputerName %q, got %q", unicodeName, gotName)
	}
	if gotModel != "MacBookPro18,1" {
		t.Fatalf("expected ReceiverModelName %q, got %q", "MacBookPro18,1", gotModel)
	}

	discReq := buildDiscoverRequestBinaryPlist("Narwhal-Shdwfax", "PC", "a4e027e9680f")
	sName, sModel := parseDiscoverRequestPayload(discReq)
	if sName != "Narwhal-Shdwfax" || sModel != "PC" {
		t.Fatalf("unexpected discover request parse: name=%q model=%q", sName, sModel)
	}

	askReq := buildAskRequestBinaryPlist(unicodeName, "MacBookPro18,1", "deadbeef1234", []FileEntry{
		{
			FileName:        "screenshot.png",
			FileType:        "public.png",
			FileBomPath:     "./screenshot.png",
			FileIsDirectory: false,
		},
		{
			FileName:        "notes.pdf",
			FileType:        "com.adobe.pdf",
			FileBomPath:     "./notes.pdf",
			FileIsDirectory: false,
		},
	})
	askSender, askModel, askFiles := parseIncomingAskPayload(askReq)
	if askSender != unicodeName || askModel != "MacBookPro18,1" {
		t.Fatalf("unexpected Ask sender=%q model=%q", askSender, askModel)
	}
	if len(askFiles) != 2 || askFiles[0].FileName != "screenshot.png" || askFiles[1].FileName != "notes.pdf" {
		t.Fatalf("unexpected Ask files: %+v", askFiles)
	}
}

// tests a TLS /discover /ask /upload file transfer
func TestFullAirDropServerDiscoverAskUpload(t *testing.T) {
	tmpOut := t.TempDir()
	tmpIn := t.TempDir()

	sampleFile := filepath.Join(tmpIn, "hello_airdrop.txt")
	if err := os.WriteFile(sampleFile, []byte("AirDrop bplist00 + CPIO verification payload"), 0644); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer("112233445566", "Test-Receiver", "MacBookPro18,1", tmpOut, 18770, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAutoAccept(true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}

	cName, mName, activePort, err := srv.ProbePeer(ctx, "127.0.0.1", srv.Port())
	if err != nil {
		t.Fatalf("ProbePeer failed: %v", err)
	}
	if cName != "Test-Receiver" || mName != "MacBookPro18,1" || activePort != srv.Port() {
		t.Fatalf("unexpected ProbePeer result: name=%q model=%q port=%d", cName, mName, activePort)
	}

	if err := srv.SendFiles(ctx, "127.0.0.1", srv.Port(), []string{sampleFile}); err != nil {
		t.Fatalf("SendFiles failed: %v", err)
	}

	savedBytes, err := os.ReadFile(filepath.Join(tmpOut, "hello_airdrop.txt"))
	if err != nil {
		t.Fatalf("expected extracted file in %s: %v", tmpOut, err)
	}
	if string(savedBytes) != "AirDrop bplist00 + CPIO verification payload" {
		t.Fatalf("unexpected file content: %q", string(savedBytes))
	}
}
