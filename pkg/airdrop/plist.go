package airdrop

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
)

type bplistNode struct {
	kind     uint8
	boolVal  bool
	intVal   uint64
	bytesVal []byte
	strVal   string
	utf16Val []uint16
	children []int
	keyRefs  []int
	valRefs  []int
}

// EncodeBinaryPlist encodes a flat or nested Go map/slice/string/bytes/bool/int structure
// into an Apple `bplist00` binary property list so iOS and macOS `sharingd` daemons that strictly
// require `bplist00` over XML plist accept our `/Discover` and `/Ask` responses natively.
func EncodeBinaryPlist(root any) []byte {
	var nodes []*bplistNode
	strDedupe := make(map[string]int)

	var build func(v any) int
	build = func(v any) int {
		if s, ok := v.(string); ok {
			if idx, exists := strDedupe[s]; exists {
				return idx
			}
			idx := len(nodes)
			strDedupe[s] = idx
			if isPureASCII(s) {
				nodes = append(nodes, &bplistNode{kind: 0x50, strVal: s})
			} else {
				u16 := utf16.Encode([]rune(s))
				nodes = append(nodes, &bplistNode{kind: 0x60, utf16Val: u16})
			}
			return idx
		}

		idx := len(nodes)
		node := &bplistNode{}
		nodes = append(nodes, node)

		switch val := v.(type) {
		case nil:
			node.kind = 0x00
		case bool:
			node.kind = 0x08
			node.boolVal = val
		case int:
			node.kind = 0x10
			node.intVal = uint64(val)
		case int64:
			node.kind = 0x10
			node.intVal = uint64(val)
		case uint64:
			node.kind = 0x10
			node.intVal = val
		case []byte:
			node.kind = 0x40
			node.bytesVal = val
		case []any:
			node.kind = 0xA0
			node.children = make([]int, len(val))
			for i, item := range val {
				node.children[i] = build(item)
			}
		case map[string]any:
			node.kind = 0xD0
			keys := make([]string, 0, len(val))
			for k := range val {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			node.keyRefs = make([]int, len(keys))
			node.valRefs = make([]int, len(keys))
			for i, k := range keys {
				node.keyRefs[i] = build(k)
			}
			for i, k := range keys {
				node.valRefs[i] = build(val[k])
			}
		default:
			s := fmt.Sprintf("%v", val)
			node.kind = 0x50
			node.strVal = s
		}
		return idx
	}

	build(root)

	refSize := uint8(1)
	if len(nodes) > 0xFF {
		refSize = 2
	}

	writeRef := func(buf *bytes.Buffer, ref int) {
		if refSize == 1 {
			buf.WriteByte(uint8(ref))
		} else {
			_ = binary.Write(buf, binary.BigEndian, uint16(ref))
		}
	}

	var buf bytes.Buffer
	buf.WriteString("bplist00")

	offsets := make([]uint64, len(nodes))
	for i, n := range nodes {
		offsets[i] = uint64(buf.Len())
		switch n.kind {
		case 0x00:
			buf.WriteByte(0x00)
		case 0x08:
			if n.boolVal {
				buf.WriteByte(0x09)
			} else {
				buf.WriteByte(0x08)
			}
		case 0x10:
			writeBplistInt(&buf, n.intVal)
		case 0x40:
			writeBplistHeader(&buf, 0x40, len(n.bytesVal))
			buf.Write(n.bytesVal)
		case 0x50:
			writeBplistHeader(&buf, 0x50, len(n.strVal))
			buf.WriteString(n.strVal)
		case 0x60:
			writeBplistHeader(&buf, 0x60, len(n.utf16Val))
			for _, cp := range n.utf16Val {
				_ = binary.Write(&buf, binary.BigEndian, cp)
			}
		case 0xA0:
			writeBplistHeader(&buf, 0xA0, len(n.children))
			for _, childRef := range n.children {
				writeRef(&buf, childRef)
			}
		case 0xD0:
			writeBplistHeader(&buf, 0xD0, len(n.keyRefs))
			for _, kr := range n.keyRefs {
				writeRef(&buf, kr)
			}
			for _, vr := range n.valRefs {
				writeRef(&buf, vr)
			}
		}
	}

	offsetTableStart := uint64(buf.Len())
	offsetIntSize := uint8(2)
	if offsetTableStart > 0xFFFF {
		offsetIntSize = 4
	}
	for _, off := range offsets {
		if offsetIntSize == 2 {
			_ = binary.Write(&buf, binary.BigEndian, uint16(off))
		} else {
			_ = binary.Write(&buf, binary.BigEndian, uint32(off))
		}
	}

	var trailer [32]byte
	trailer[6] = offsetIntSize
	trailer[7] = refSize
	binary.BigEndian.PutUint64(trailer[8:16], uint64(len(nodes)))
	binary.BigEndian.PutUint64(trailer[16:24], 0)
	binary.BigEndian.PutUint64(trailer[24:32], offsetTableStart)
	buf.Write(trailer[:])

	return buf.Bytes()
}

// decodes a bplist00 binary to list into go maps, slices, and strings
func DecodeBinaryPlist(raw []byte) (any, error) {
	data := maybeGunzip(raw)
	if len(data) < 40 || !bytes.HasPrefix(data, []byte("bplist00")) {
		return nil, fmt.Errorf("not a valid bplist00 payload (len=%d)", len(data))
	}

	trailer := data[len(data)-32:]
	offsetIntSize := int(trailer[6])
	objectRefSize := int(trailer[7])
	numObjects := int(binary.BigEndian.Uint64(trailer[8:16]))
	topObject := int(binary.BigEndian.Uint64(trailer[16:24]))
	offsetTableOffset := int(binary.BigEndian.Uint64(trailer[24:32]))

	if offsetIntSize != 1 && offsetIntSize != 2 && offsetIntSize != 4 && offsetIntSize != 8 {
		return nil, fmt.Errorf("invalid bplist00 offsetIntSize %d", offsetIntSize)
	}
	if objectRefSize != 1 && objectRefSize != 2 && objectRefSize != 4 && objectRefSize != 8 {
		return nil, fmt.Errorf("invalid bplist00 objectRefSize %d", objectRefSize)
	}
	if numObjects <= 0 || numObjects > 65536 || offsetTableOffset < 8 || offsetTableOffset+numObjects*offsetIntSize > len(data)-32 {
		return nil, fmt.Errorf("corrupt bplist00 offset table")
	}

	offsets := make([]int, numObjects)
	for i := 0; i < numObjects; i++ {
		pos := offsetTableOffset + i*offsetIntSize
		offsets[i] = int(readSizedUint(data[pos : pos+offsetIntSize]))
	}

	visited := make(map[int]bool)
	var decodeObj func(objIdx int) (any, error)
	decodeObj = func(objIdx int) (any, error) {
		if objIdx < 0 || objIdx >= numObjects {
			return nil, fmt.Errorf("bplist00 object index %d out of range", objIdx)
		}
		if visited[objIdx] {
			return nil, nil
		}
		visited[objIdx] = true
		defer func() { visited[objIdx] = false }()

		pos := offsets[objIdx]
		if pos < 8 || pos >= len(data)-32 {
			return nil, fmt.Errorf("bplist00 offset %d out of bounds", pos)
		}

		marker := data[pos]
		hi := marker & 0xF0
		lo := int(marker & 0x0F)
		pos++

		readLength := func() (int, error) {
			if lo != 0x0F {
				return lo, nil
			}
			if pos >= len(data)-32 {
				return 0, fmt.Errorf("unexpected EOF reading bplist length")
			}
			intMarker := data[pos]
			pos++
			byteCount := 1 << (intMarker & 0x0F)
			if pos+byteCount > len(data)-32 {
				return 0, fmt.Errorf("length integer out of bounds")
			}
			val := int(readSizedUint(data[pos : pos+byteCount]))
			pos += byteCount
			return val, nil
		}

		switch hi {
		case 0x00:
			switch lo {
			case 0x08:
				return false, nil
			case 0x09:
				return true, nil
			default:
				return nil, nil
			}
		case 0x10:
			byteCount := 1 << lo
			if pos+byteCount > len(data)-32 {
				return int64(0), nil
			}
			return int64(readSizedUint(data[pos : pos+byteCount])), nil
		case 0x20:
			byteCount := 1 << lo
			if byteCount == 4 && pos+4 <= len(data)-32 {
				bits := binary.BigEndian.Uint32(data[pos : pos+4])
				return float64(math.Float32frombits(bits)), nil
			}
			if byteCount == 8 && pos+8 <= len(data)-32 {
				bits := binary.BigEndian.Uint64(data[pos : pos+8])
				return math.Float64frombits(bits), nil
			}
			return 0.0, nil
		case 0x30:
			if pos+8 <= len(data)-32 {
				bits := binary.BigEndian.Uint64(data[pos : pos+8])
				return math.Float64frombits(bits), nil
			}
			return 0.0, nil
		case 0x40:
			count, err := readLength()
			if err != nil || pos+count > len(data)-32 {
				return []byte{}, err
			}
			out := make([]byte, count)
			copy(out, data[pos:pos+count])
			return out, nil
		case 0x50:
			count, err := readLength()
			if err != nil || pos+count > len(data)-32 {
				return "", err
			}
			return string(data[pos : pos+count]), nil
		case 0x60:
			count, err := readLength()
			byteLen := count * 2
			if err != nil || pos+byteLen > len(data)-32 {
				return "", err
			}
			u16 := make([]uint16, count)
			for i := 0; i < count; i++ {
				u16[i] = binary.BigEndian.Uint16(data[pos+i*2 : pos+i*2+2])
			}
			return string(utf16.Decode(u16)), nil
		case 0x80:
			byteCount := lo + 1
			if pos+byteCount > len(data)-32 {
				return uint64(0), nil
			}
			return readSizedUint(data[pos : pos+byteCount]), nil
		case 0xA0:
			count, err := readLength()
			if err != nil || pos+count*objectRefSize > len(data)-32 {
				return []any{}, err
			}
			arr := make([]any, 0, count)
			for i := 0; i < count; i++ {
				refPos := pos + i*objectRefSize
				ref := int(readSizedUint(data[refPos : refPos+objectRefSize]))
				elem, err := decodeObj(ref)
				if err == nil {
					arr = append(arr, elem)
				}
			}
			return arr, nil
		case 0xD0:
			count, err := readLength()
			if err != nil || pos+count*2*objectRefSize > len(data)-32 {
				return map[string]any{}, err
			}
			dict := make(map[string]any, count)
			keysBase := pos
			valsBase := pos + count*objectRefSize
			for i := 0; i < count; i++ {
				kRef := int(readSizedUint(data[keysBase+i*objectRefSize : keysBase+(i+1)*objectRefSize]))
				vRef := int(readSizedUint(data[valsBase+i*objectRefSize : valsBase+(i+1)*objectRefSize]))
				kObj, _ := decodeObj(kRef)
				vObj, _ := decodeObj(vRef)
				if kStr, ok := kObj.(string); ok {
					dict[kStr] = vObj
				}
			}
			return dict, nil
		default:
			return nil, nil
		}
	}

	return decodeObj(topObject)
}

func readSizedUint(b []byte) uint64 {
	var v uint64
	for _, by := range b {
		v = (v << 8) | uint64(by)
	}
	return v
}

func maybeGunzip(raw []byte) []byte {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			defer gr.Close()
			if out, err := io.ReadAll(io.LimitReader(gr, 8<<20)); err == nil && len(out) > 0 {
				return out
			}
		}
	}
	return raw
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
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
	} else if v <= 0xFFFFFFFF {
		buf.WriteByte(0x12)
		_ = binary.Write(buf, binary.BigEndian, uint32(v))
	} else {
		buf.WriteByte(0x13)
		_ = binary.Write(buf, binary.BigEndian, v)
	}
}

// buildReceiverBinaryPlist constructs the binary plist (`bplist00`) response for `POST /Discover`.
func buildReceiverBinaryPlist(computerName, modelName, deviceID string, mediaCapsJSON []byte) []byte {
	dict := map[string]any{
		"ReceiverComputerName":      computerName,
		"ReceiverModelName":         modelName,
		"ReceiverID":                deviceID,
		"ReceiverMediaCapabilities": mediaCapsJSON,
		"ReceiverRecordData":        []byte{},
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

func buildDiscoverRequestBinaryPlist(computerName, modelName, deviceID string) []byte {
	dict := map[string]any{
		"SenderComputerName": computerName,
		"SenderModelName":    modelName,
		"SenderID":           deviceID,
		"SenderRecordData":   []byte{},
	}
	return EncodeBinaryPlist(dict)
}

// reads the senders name and model from the /discover request
func parseDiscoverRequestPayload(raw []byte) (senderName, senderModel string) {
	if root, err := DecodeBinaryPlist(raw); err == nil {
		if dict, ok := root.(map[string]any); ok {
			if v, ok := dict["SenderComputerName"].(string); ok && v != "" {
				senderName = v
			}
			if v, ok := dict["SenderModelName"].(string); ok && v != "" {
				senderModel = v
			}
		}
	}
	return senderName, senderModel
}

// parseDiscoverResponsePayload extracts ReceiverComputerName and ReceiverModelName from a `/Discover` response.
func parseDiscoverResponsePayload(raw []byte, fallbackIP string) (computerName, modelName string) {
	computerName = "AirDrop Peer (" + fallbackIP + ")"
	modelName = "macOS / iOS"

	if root, err := DecodeBinaryPlist(raw); err == nil {
		if dict, ok := root.(map[string]any); ok {
			if v, ok := dict["ReceiverComputerName"].(string); ok && strings.TrimSpace(v) != "" {
				computerName = strings.TrimSpace(v)
			}
			if v, ok := dict["ReceiverModelName"].(string); ok && strings.TrimSpace(v) != "" {
				modelName = strings.TrimSpace(v)
			}
			return computerName, modelName
		}
	}

	text := string(maybeGunzip(raw))
	if v := extractXMLKeyString(text, "ReceiverComputerName"); v != "" {
		computerName = v
	}
	if v := extractXMLKeyString(text, "ReceiverModelName"); v != "" {
		modelName = v
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
		"SenderComputerName":  computerName,
		"SenderModelName":     modelName,
		"SenderID":            senderID,
		"BundleID":            "com.apple.finder",
		"ConvertMediaFormats": false,
		"Items":               []any{},
		"Files":               fileList,
	}
	return EncodeBinaryPlist(dict)
}

// parseIncomingAskPayload extracts SenderComputerName, SenderModelName, and Files from either
// a binary plist (`bplist00` sent by iOS/macOS sharingd) or an XML plist.
func parseIncomingAskPayload(raw []byte) (senderName, senderModel string, files []FileEntry) {
	senderName = "Apple Device"
	senderModel = "iPhone / Mac"

	if root, err := DecodeBinaryPlist(raw); err == nil {
		if dict, ok := root.(map[string]any); ok {
			if v, ok := dict["SenderComputerName"].(string); ok && strings.TrimSpace(v) != "" {
				senderName = strings.TrimSpace(v)
			}
			if v, ok := dict["SenderModelName"].(string); ok && strings.TrimSpace(v) != "" {
				senderModel = strings.TrimSpace(v)
			}
			if rawFiles, ok := dict["Files"].([]any); ok {
				for _, item := range rawFiles {
					if fm, ok := item.(map[string]any); ok {
						fname, _ := fm["FileName"].(string)
						ftype, _ := fm["FileType"].(string)
						fbom, _ := fm["FileBomPath"].(string)
						isDir, _ := fm["FileIsDirectory"].(bool)
						conv, _ := fm["ConvertMediaFormats"].(bool)
						if fname == "" && strings.HasPrefix(fbom, "./") {
							fname = strings.TrimPrefix(fbom, "./")
						}
						if fname != "" {
							if ftype == "" {
								ftype = detectAppleUTI(fname)
							}
							if fbom == "" {
								fbom = "./" + fname
							}
							files = append(files, FileEntry{
								FileName:            fname,
								FileType:            ftype,
								FileBomPath:         fbom,
								FileIsDirectory:     isDir,
								ConvertMediaFormats: conv,
							})
						}
					}
				}
			}
		}
	} else {
		text := string(maybeGunzip(raw))
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
