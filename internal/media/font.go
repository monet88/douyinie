package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// ErrFontFamilyNotFound is returned when a font file carries no usable family name.
var ErrFontFamilyNotFound = errors.New("font file carries no family name")

// assFilterSpec builds the libass filter spec for an ASS subtitle file.
// When a font file is configured, its directory is registered with libass via fontsdir so
// custom fonts outside system search paths can be matched by family name.
func assFilterSpec(assPath, fontFile string) string {
	spec := fmt.Sprintf("ass=filename='%s'", EscapeFFmpegFilterPath(assPath))
	trimmed := strings.TrimSpace(fontFile)
	if trimmed != "" {
		dir := filepath.Dir(trimmed)
		spec += fmt.Sprintf(":fontsdir='%s'", EscapeFFmpegFilterPath(dir))
	}
	return spec
}

// ResolveASSFontFamily inspects a TTF, OTF, or TTC font file and extracts the primary
// font family name that libass will match against the ASS 'Fontname' style attribute.
// It parses the sfnt 'name' table directly without any third-party dependencies.
// An empty path returns ("", nil).
func ResolveASSFontFamily(fontFile string) (string, error) {
	p := strings.TrimSpace(fontFile)
	if p == "" {
		return "", nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("open font file: %w", err)
	}
	defer f.Close()

	return ParseFontFamily(f)
}

// ComputeFontSHA256 reads the font file and calculates its hex-encoded SHA-256 digest.
// An empty path returns ("", nil).
func ComputeFontSHA256(fontFile string) (string, error) {
	p := strings.TrimSpace(fontFile)
	if p == "" {
		return "", nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("open font file for hashing: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash font file %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ParseFontFamily extracts the font family name from an open font reader (TTF, OTF, or TTC).
func ParseFontFamily(r io.ReaderAt) (string, error) {
	var tag [4]byte
	if _, err := r.ReadAt(tag[:], 0); err != nil {
		return "", fmt.Errorf("read font tag: %w", err)
	}

	offset := int64(0)
	if string(tag[:]) == "ttcf" {
		// TrueType Collection: read first font's offset
		var numFonts uint32
		var buf [4]byte
		if _, err := r.ReadAt(buf[:], 8); err != nil {
			return "", fmt.Errorf("read TTC font count: %w", err)
		}
		numFonts = binary.BigEndian.Uint32(buf[:])
		if numFonts == 0 {
			return "", fmt.Errorf("%w: empty TTC collection", ErrFontFamilyNotFound)
		}
		if _, err := r.ReadAt(buf[:], 12); err != nil {
			return "", fmt.Errorf("read first TTC font offset: %w", err)
		}
		offset = int64(binary.BigEndian.Uint32(buf[:]))
	}

	var hdr [12]byte
	if _, err := r.ReadAt(hdr[:], offset); err != nil {
		return "", fmt.Errorf("read sfnt header: %w", err)
	}
	numTables := int(binary.BigEndian.Uint16(hdr[4:6]))
	if numTables <= 0 || numTables > 256 {
		return "", fmt.Errorf("%w: invalid table count %d", ErrFontFamilyNotFound, numTables)
	}

	dir := make([]byte, numTables*16)
	if _, err := r.ReadAt(dir, offset+12); err != nil {
		return "", fmt.Errorf("read sfnt table directory: %w", err)
	}

	var nameOff, nameLen uint32
	found := false
	for i := range numTables {
		entry := dir[i*16 : (i+1)*16]
		if string(entry[0:4]) == "name" {
			nameOff = binary.BigEndian.Uint32(entry[8:12])
			nameLen = binary.BigEndian.Uint32(entry[12:16])
			found = true
			break
		}
	}
	if !found || nameLen < 6 {
		return "", fmt.Errorf("%w: name table missing or too small", ErrFontFamilyNotFound)
	}
	if nameLen > 4*1024*1024 {
		return "", fmt.Errorf("%w: name table exceeds size limit (%d bytes)", ErrFontFamilyNotFound, nameLen)
	}

	nameTable := make([]byte, nameLen)
	if _, err := r.ReadAt(nameTable, int64(nameOff)); err != nil {
		return "", fmt.Errorf("read name table: %w", err)
	}

	count := int(binary.BigEndian.Uint16(nameTable[2:4]))
	strOffset := int(binary.BigEndian.Uint16(nameTable[4:6]))
	if strOffset > len(nameTable) {
		return "", fmt.Errorf("%w: invalid string offset in name table", ErrFontFamilyNotFound)
	}

	type candidate struct {
		nameID   uint16
		platform uint16
		text     string
	}
	var candidates []candidate

	for i := range count {
		recOffset := 6 + i*12
		if recOffset+12 > strOffset {
			break
		}
		rec := nameTable[recOffset : recOffset+12]
		platformID := binary.BigEndian.Uint16(rec[0:2])
		encodingID := binary.BigEndian.Uint16(rec[2:4])
		nameID := binary.BigEndian.Uint16(rec[6:8])
		length := int(binary.BigEndian.Uint16(rec[8:10]))
		strRelOffset := int(binary.BigEndian.Uint16(rec[10:12]))
		// Consider Typographic Family (16) and Font Family (1)
		if nameID != 16 && nameID != 1 {
			continue
		}

		fullStrOffset := strOffset + strRelOffset
		if fullStrOffset+length > len(nameTable) {
			continue
		}
		rawBytes := nameTable[fullStrOffset : fullStrOffset+length]

		var text string
		switch platformID {
		case 3: // Windows UTF-16BE
			if length%2 != 0 {
				continue
			}
			u16s := make([]uint16, length/2)
			for j := range u16s {
				u16s[j] = binary.BigEndian.Uint16(rawBytes[j*2 : j*2+2])
			}
			text = string(utf16.Decode(u16s))
		case 1: // Macintosh
			if encodingID != 0 {
				// Non-Roman Macintosh encodings are not supported without dedicated charmaps.
				continue
			}
			// Mac Roman is byte-compatible with UTF-8 only within 7-bit ASCII (0x00-0x7F).
			// Non-ASCII bytes in Mac Roman (>=0x80) map to different code points than UTF-8,
			// which corrupts the family name and causes libass font selection failure.
			isASCII := true
			for _, b := range rawBytes {
				if b > 0x7F {
					isASCII = false
					break
				}
			}
			if !isASCII {
				continue
			}
			text = string(rawBytes)
		case 0: // Unicode UTF-16BE
			if length%2 != 0 {
				continue
			}
			u16s := make([]uint16, length/2)
			for j := range u16s {
				u16s[j] = binary.BigEndian.Uint16(rawBytes[j*2 : j*2+2])
			}
			text = string(utf16.Decode(u16s))
		default:
			continue
		}

		clean := strings.TrimSpace(text)
		clean = strings.Trim(clean, "\x00")
		if clean != "" {
			candidates = append(candidates, candidate{
				nameID:   nameID,
				platform: platformID,
				text:     clean,
			})
		}
	}

	// Priority:
	// 1. nameID 16 (Typographic Family), Windows (3) or Unicode (0)
	// 2. nameID 1  (Font Family), Windows (3) or Unicode (0)
	// 3. nameID 16, Mac (1)
	// 4. nameID 1,  Mac (1)
	// 5. any candidate
	for _, c := range candidates {
		if c.nameID == 16 && (c.platform == 3 || c.platform == 0) {
			return c.text, nil
		}
	}
	for _, c := range candidates {
		if c.nameID == 1 && (c.platform == 3 || c.platform == 0) {
			return c.text, nil
		}
	}
	for _, c := range candidates {
		if c.nameID == 16 && c.platform == 1 {
			return c.text, nil
		}
	}
	for _, c := range candidates {
		if c.nameID == 1 && c.platform == 1 {
			return c.text, nil
		}
	}
	if len(candidates) > 0 {
		return candidates[0].text, nil
	}

	return "", fmt.Errorf("%w: no family name record found in font", ErrFontFamilyNotFound)
}
