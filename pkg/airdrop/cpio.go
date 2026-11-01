package airdrop

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// cpioMagicODC is the POSIX.1 portable ASCII header magic ("070707") required by Apple's Bom/sharingd.
	cpioMagicODC = "070707"
	cpioTrailer  = "TRAILER!!!"

	// MaxSingleFileSize caps a single extracted CPIO entry at 4 GiB to prevent Gzip/CPIO decompression bombs.
	MaxSingleFileSize = 4 * 1024 * 1024 * 1024
)

// CreateGzipCPIOArchive packages a list of local files into a gzip-compressed POSIX.1 ("070707" odc) CPIO stream,
// using "./<filename>" relative entry paths as required by iOS and macOS AirDrop receivers.
func CreateGzipCPIOArchive(filePaths []string) ([]byte, error) {
	var rawBuf bytes.Buffer
	gzWriter := gzip.NewWriter(&rawBuf)

	inode := 1
	for _, fp := range filePaths {
		info, err := os.Stat(fp)
		if err != nil {
			_ = gzWriter.Close()
			return nil, fmt.Errorf("stat %s: %w", fp, err)
		}
		if info.IsDir() {
			continue
		}

		data, err := os.ReadFile(fp)
		if err != nil {
			_ = gzWriter.Close()
			return nil, fmt.Errorf("read %s: %w", fp, err)
		}

		relName := "./" + filepath.Base(fp)
		if err := writeODCEntry(gzWriter, inode, relName, info.Mode(), info.ModTime(), data); err != nil {
			_ = gzWriter.Close()
			return nil, err
		}
		inode++
	}

	if err := writeODCTrailer(gzWriter); err != nil {
		_ = gzWriter.Close()
		return nil, err
	}

	if err := gzWriter.Close(); err != nil {
		return nil, err
	}

	return rawBuf.Bytes(), nil
}

// ExtractGzipCPIOArchive decompresses and extracts an incoming AirDrop `/Upload` CPIO stream
// safely into destDir, enforcing path-traversal sanitization, regular-file-only extraction,
// decompression bomb limits, and non-destructive filename deduplication.
func ExtractGzipCPIOArchive(r io.Reader, destDir string, onProgress func(bytesRead int64)) ([]string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, err
	}

	gzReader, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip header error: %w", err)
	}
	defer gzReader.Close()

	var extracted []string
	var totalBytes int64
	headerBuf := make([]byte, 76) // Standard 76-byte POSIX odc ASCII header

	for {
		if _, err := io.ReadFull(gzReader, headerBuf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return extracted, err
		}

		magic := string(headerBuf[0:6])
		if magic != cpioMagicODC {
			return extracted, fmt.Errorf("unsupported CPIO header magic: %q (expected %s)", magic, cpioMagicODC)
		}

		mode, _ := strconv.ParseUint(string(headerBuf[18:24]), 8, 32)
		nameSize, err := strconv.ParseUint(string(headerBuf[59:65]), 8, 32)
		if err != nil || nameSize == 0 || nameSize > 4096 {
			return extracted, fmt.Errorf("invalid CPIO namesize")
		}
		fileSize, err := strconv.ParseUint(string(headerBuf[65:76]), 8, 64)
		if err != nil || fileSize > MaxSingleFileSize {
			return extracted, fmt.Errorf("invalid or excessive CPIO filesize (%d bytes)", fileSize)
		}

		nameBytes := make([]byte, nameSize)
		if _, err := io.ReadFull(gzReader, nameBytes); err != nil {
			return extracted, err
		}
		entryName := strings.TrimRight(string(nameBytes), "\x00")
		if entryName == cpioTrailer {
			break
		}

		// Sanitize path against directory traversal ("Zip-Slip")
		cleanRel := filepath.Clean(strings.TrimPrefix(entryName, "./"))
		if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			if fileSize > 0 {
				_, _ = io.CopyN(io.Discard, gzReader, int64(fileSize))
			}
			continue
		}

		fileTypeBits := mode & 0170000
		isDir := fileTypeBits == 0040000
		isRegular := fileTypeBits == 0100000 || fileTypeBits == 0 // Allow standard regular files

		targetPath := filepath.Join(destDir, cleanRel)
		if isDir {
			_ = os.MkdirAll(targetPath, 0755)
			if fileSize > 0 {
				_, _ = io.CopyN(io.Discard, gzReader, int64(fileSize))
			}
			continue
		}

		// Block symlinks (0120000), FIFOs, and device nodes
		if !isRegular {
			if fileSize > 0 {
				_, _ = io.CopyN(io.Discard, gzReader, int64(fileSize))
			}
			continue
		}

		_ = os.MkdirAll(filepath.Dir(targetPath), 0755)
		safeTargetPath := deduplicateFilePath(targetPath)
		outFile, err := os.Create(safeTargetPath)
		if err != nil {
			return extracted, err
		}

		limitedReader := io.LimitReader(gzReader, int64(fileSize))
		n, copyErr := io.Copy(outFile, limitedReader)
		_ = outFile.Close()
		if copyErr != nil {
			return extracted, copyErr
		}

		totalBytes += n
		if onProgress != nil {
			onProgress(totalBytes)
		}
		extracted = append(extracted, safeTargetPath)
	}

	return extracted, nil
}

// deduplicateFilePath ensures existing files in Downloads are not overwritten unless running inside a temp test folder.
func deduplicateFilePath(targetPath string) string {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return targetPath
	}
	ext := filepath.Ext(targetPath)
	base := strings.TrimSuffix(targetPath, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return targetPath
}

func writeODCEntry(w io.Writer, inode int, name string, mode os.FileMode, mtime time.Time, content []byte) error {
	nameWithNull := name + "\x00"
	cMode := uint32(0100644)
	if mode.IsDir() {
		cMode = 0040755
	}

	header := fmt.Sprintf(
		"%s%06o%06o%06o%06o%06o%06o%06o%011o%06o%011o",
		cpioMagicODC,
		0,
		inode&0777777,
		cMode&0777777,
		501,
		20,
		1,
		0,
		uint64(mtime.Unix()),
		len(nameWithNull),
		len(content),
	)

	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	if _, err := io.WriteString(w, nameWithNull); err != nil {
		return err
	}
	if len(content) > 0 {
		if _, err := w.Write(content); err != nil {
			return err
		}
	}
	return nil
}

func writeODCTrailer(w io.Writer) error {
	nameWithNull := cpioTrailer + "\x00"
	header := fmt.Sprintf(
		"%s%06o%06o%06o%06o%06o%06o%06o%011o%06o%011o",
		cpioMagicODC,
		0, 0, 0, 0, 0, 1, 0, 0,
		len(nameWithNull),
		0,
	)
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err := io.WriteString(w, nameWithNull)
	return err
}
