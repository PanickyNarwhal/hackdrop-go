package airdrop

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// EncodeBinaryPlist encodes a flat or nested Go map/slice/string/bytes/bool/int structure
// into an Apple `bplist00` binary property list so iOS and macOS `sharingd` daemons that strictly
// require `bplist00` over XML plist accept our `/Discover` and `/Ask` responses natively.
func EncodeBinaryPlist(root any) []byte {
	var objects []any
	objIndex := make(map[string]int)

	var collect func(v any) int
	collect = func(v any) int {
		switch val := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(val))
			for k := range val {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				collect(k)
				collect(val[k])
			}
			idx := len(objects)
			objects = append(objects, val)
			return idx
		case []any:
			for _, item := range val {
				collect(item)
			}
			idx := len(objects)
			objects = append(objects, val)
			return idx
		case string:
			key := "s:" + val
			if idx, ok := objIndex[key]; ok {
				return idx
			}
			idx := len(objects)
			objects = append(objects, val)
			objIndex[key] = idx
			return idx
		default:
			idx := len(objects)
			objects = append(objects, val)
			return idx
		}
	}

	rootIdx := collect(root)

	// Re-order so root is object 0 (standard Apple CFBinaryPlist convention)
	ordered := make([]any, 0, len(objects))
	ordered = append(ordered, objects[rootIdx])
	oldToNew := make(map[int]int)
	oldToNew[rootIdx] = 0
	for i, obj := range objects {
		if i == rootIdx {
			continue
		}
		oldToNew[i] = len(ordered)
		ordered = append(ordered, obj)
	}

	findIndex := func(v any) int {
		if s, ok := v.(string); ok {
			if oldIdx, exists := objIndex["s:"+s]; exists {
				return oldToNew[oldIdx]
			}
		}
		for i, o := range ordered {
			if fmt.Sprintf("%T:%v", o, o) == fmt.Sprintf("%T:%v", v, v) {
				return i
			}
		}
		return 0
	}

	var buf bytes.Buffer
	buf.WriteString("bplist00")

	offsets := make([]uint32, len(ordered))
	for i, obj := range ordered {
		offsets[i] = uint32(buf.Len())
		switch val := obj.(type) {
		case bool:
			if val {
				buf.WriteByte(0x09)
			} else {
				buf.WriteByte(0x08)
			}
		case int:
			writeBplistInt(&buf, uint64(val))
		case string:
			writeBplistHeader(&buf, 0x50, len(val))
			buf.WriteString(val)
		case []byte:
			writeBplistHeader(&buf, 0x40, len(val))
			buf.Write(val)
		case []any:
			writeBplistHeader(&buf, 0xA0, len(val))
			for _, item := range val {
				buf.WriteByte(uint8(findIndex(item)))
			}
		case map[string]any:
			keys := make([]string, 0, len(val))
			for k := range val {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			writeBplistHeader(&buf, 0xD0, len(keys))
			for _, k := range keys {
				buf.WriteByte(uint8(findIndex(k)))
			}
			for _, k := range keys {
				buf.WriteByte(uint8(findIndex(val[k])))
			}
		}
	}

	offsetTableStart := uint64(buf.Len())
	for _, off := range offsets {
		_ = binary.Write(&buf, binary.BigEndian, uint16(off))
	}

	// 32-byte bplist00 trailer
	var trailer [32]byte
	trailer[6] = 2 // offsetIntSize = 2 bytes
	trailer[7] = 1 // objectRefSize = 1 byte
	binary.BigEndian.PutUint64(trailer[8:16], uint64(len(ordered)))
	binary.BigEndian.PutUint64(trailer[16:24], 0) // topObject = 0
	binary.BigEndian.PutUint64(trailer[24:32], offsetTableStart)
	buf.Write(trailer[:])

	return buf.Bytes()
}

func writeBplistHeader(buf *bytes.Buffer, marker uint8, length int) {
	if length < 15 {
		buf.WriteByte(marker | uint8(length))
		return
	}
	buf.WriteByte(marker | 0x0F)
	writeBplistInt(buf, uint64(length))
}

func writeBplistInt(buf *bytes.Buffer, v uint64) {
	if v <= 0xFF {
		buf.WriteByte(0x10)
		buf.WriteByte(uint8(v))
	} else if v <= 0xFFFF {
		buf.WriteByte(0x11)
		_ = binary.Write(buf, binary.BigEndian, uint16(v))
	} else {
		buf.WriteByte(0x12)
		_ = binary.Write(buf, binary.BigEndian, uint32(v))
	}
}

// buildReceiverBinaryPlist constructs the binary plist (`bplist00`) response for `POST /Discover`.
func buildReceiverBinaryPlist(computerName, modelName, deviceID string, mediaCapsJSON []byte) []byte {
	dict := map[string]any{
		"ReceiverComputerName":      computerName,
		"ReceiverModelName":         modelName,
		"ReceiverID":                deviceID,
		"ReceiverMediaCapabilities": mediaCapsJSON,
	}
	return EncodeBinaryPlist(dict)
}

func buildAskAcceptBinaryPlist(computerName, modelName string) []byte {
	dict := map[string]any{
		"ReceiverComputerName": computerName,
		"ReceiverModelName":    modelName,
	}
	return EncodeBinaryPlist(dict)
}

func buildDiscoverRequestBinaryPlist() []byte {
	dict := map[string]any{
		"SenderRecordData": []byte{},
	}
	return EncodeBinaryPlist(dict)
}

// parseDiscoverResponsePayload extracts ReceiverComputerName and ReceiverModelName from a `/Discover` response.
func parseDiscoverResponsePayload(raw []byte, fallbackIP string) (computerName, modelName string) {
	computerName = "AirDrop Peer (" + fallbackIP + ")"
	modelName = "macOS / iOS"
	if bytes.HasPrefix(raw, []byte("bplist00")) {
		tokens := extractBinaryPlistASCII(raw)
		for i, t := range tokens {
			if t == "ReceiverComputerName" {
				if v := findNextNonKeyString(tokens, i+1); v != "" {
					computerName = v
				}
			}
			if t == "ReceiverModelName" {
				if v := findNextNonKeyString(tokens, i+1); v != "" {
					modelName = v
				}
			}
		}
	} else {
		text := string(raw)
		if v := extractXMLKeyString(text, "ReceiverComputerName"); v != "" {
			computerName = v
		}
		if v := extractXMLKeyString(text, "ReceiverModelName"); v != "" {
			modelName = v
		}
	}
	return computerName, modelName
}

func buildAskRequestBinaryPlist(computerName, modelName, senderID string, files []FileEntry) []byte {
	fileList := make([]any, 0, len(files))
	for _, f := range files {
		fileList = append(fileList, map[string]any{
			"FileName":            f.FileName,
			"FileType":            f.FileType,
			"FileBomPath":         f.FileBomPath,
			"FileIsDirectory":     f.FileIsDirectory,
			"ConvertMediaFormats": false,
		})
	}
	dict := map[string]any{
		"SenderComputerName": computerName,
		"SenderModelName":    modelName,
		"SenderID":           senderID,
		"BundleID":           "com.apple.finder",
		"Items":              []any{},
		"Files":              fileList,
	}
	return EncodeBinaryPlist(dict)
}

// parseIncomingAskPayload extracts SenderComputerName, SenderModelName, and Files from either
// a binary plist (`bplist00` sent by iOS/macOS sharingd) or an XML plist.
func parseIncomingAskPayload(raw []byte) (senderName, senderModel string, files []FileEntry) {
	senderName = "Apple Device"
	senderModel = "iPhone / Mac"

	if bytes.HasPrefix(raw, []byte("bplist00")) {
		stringsFound := extractBinaryPlistASCII(raw)
		// In bplist00 dictionaries, keys are listed first or paired; also check key->value adjacency and known keys
		for i := 0; i < len(stringsFound); i++ {
			token := stringsFound[i]
			switch token {
			case "SenderComputerName":
				if val := findNextNonKeyString(stringsFound, i+1); val != "" {
					senderName = val
				}
			case "SenderModelName":
				if val := findNextNonKeyString(stringsFound, i+1); val != "" {
					senderModel = val
				}
			case "FileName":
				if val := findNextNonKeyString(stringsFound, i+1); val != "" {
					files = append(files, FileEntry{
						FileName:    val,
						FileType:    detectAppleUTI(val),
						FileBomPath: "./" + val,
					})
				}
			}
		}
		// Also check for any "./<filename>" FileBomPath entries in sorted bplist tables
		if len(files) == 0 {
			for _, s := range stringsFound {
				if strings.HasPrefix(s, "./") && len(s) > 2 {
					fname := strings.TrimPrefix(s, "./")
					files = append(files, FileEntry{
						FileName:    fname,
						FileType:    detectAppleUTI(fname),
						FileBomPath: s,
					})
				}
			}
		}
	} else {
		text := string(raw)
		if v := extractXMLKeyString(text, "SenderComputerName"); v != "" {
			senderName = v
		}
		if v := extractXMLKeyString(text, "SenderModelName"); v != "" {
			senderModel = v
		}
		if v := extractXMLKeyString(text, "FileName"); v != "" {
			files = append(files, FileEntry{
				FileName:    v,
				FileType:    detectAppleUTI(v),
				FileBomPath: "./" + v,
			})
		}
	}

	if len(files) == 0 {
		files = append(files, FileEntry{
			FileName:    "HackDrop_Payload",
			FileType:    "public.data",
			FileBomPath: "./HackDrop_Payload",
		})
	}
	return senderName, senderModel, files
}

var knownPlistKeys = map[string]bool{
	"BundleID": true, "ConvertMediaFormats": true, "FileBomPath": true,
	"FileIsDirectory": true, "FileName": true, "FileType": true, "Files": true,
	"Items": true, "SenderComputerName": true, "SenderID": true, "SenderModelName": true,
	"SenderRecordData": true, "ReceiverComputerName": true, "ReceiverModelName": true,
}

func findNextNonKeyString(tokens []string, startIdx int) string {
	for i := startIdx; i < len(tokens); i++ {
		t := tokens[i]
		if !knownPlistKeys[t] && t != "com.apple.finder" && !strings.HasPrefix(t, "public.") && !strings.HasPrefix(t, "./") {
			return t
		}
	}
	return ""
}

// extractBinaryPlistASCII scans a bplist00 object table for printable ASCII strings (object marker 0x50..0x5F).
func extractBinaryPlistASCII(data []byte) []string {
	var out []string
	i := 8 // Skip "bplist00"
	limit := len(data)
	if limit > 32 {
		limit -= 32 // Exclude trailer
	}
	for i < limit {
		b := data[i]
		if (b & 0xF0) == 0x50 {
			strLen := int(b & 0x0F)
			i++
			if strLen == 0x0F && i < limit {
				intMarker := data[i]
				intBytes := 1 << (intMarker & 0x0F)
				i++
				if intBytes == 1 && i < limit {
					strLen = int(data[i])
					i++
				} else if intBytes == 2 && i+1 < limit {
					strLen = int(data[i])<<8 | int(data[i+1])
					i += 2
				}
			}
			if strLen > 0 && strLen < 512 && i+strLen <= limit {
				candidate := string(data[i : i+strLen])
				if isPrintableASCII(candidate) {
					out = append(out, candidate)
				}
				i += strLen
				continue
			}
		}
		i++
	}
	return out
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return len(s) > 0
}

func extractXMLKeyString(xmlText, key string) string {
	needle := "<key>" + key + "</key>"
	idx := strings.Index(xmlText, needle)
	if idx == -1 {
		return ""
	}
	rest := xmlText[idx+len(needle):]
	openTag := strings.Index(rest, "<string>")
	closeTag := strings.Index(rest, "</string>")
	if openTag != -1 && closeTag != -1 && closeTag > openTag+8 {
		return rest[openTag+8 : closeTag]
	}
	return ""
}

func detectAppleUTI(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "public.jpeg"
	case ".png":
		return "public.png"
	case ".heic", ".heif":
		return "public.heic"
	case ".gif":
		return "com.compuserve.gif"
	case ".mp4", ".m4v":
		return "public.mpeg-4"
	case ".mov":
		return "com.apple.quicktime-movie"
	case ".mp3":
		return "public.mp3"
	case ".pdf":
		return "com.adobe.pdf"
	case ".zip":
		return "public.zip-archive"
	case ".txt", ".md", ".go", ".py", ".js", ".ts", ".json":
		return "public.plain-text"
	default:
		return "public.data"
	}
}
