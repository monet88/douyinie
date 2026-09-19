package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func TestResolveASSFontFamily_Empty(t *testing.T) {
	name, err := ResolveASSFontFamily("")
	if err != nil {
		t.Fatalf("expected nil error on empty font file, got %v", err)
	}
	if name != "" {
		t.Errorf("expected empty string for empty font file, got %q", name)
	}
}

func TestResolveASSFontFamily_MissingFile(t *testing.T) {
	_, err := ResolveASSFontFamily(filepath.Join(t.TempDir(), "nonexistent.ttf"))
	if err == nil {
		t.Fatalf("expected error for missing font file")
	}
}

func TestResolveASSFontFamily_CorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "corrupt.ttf")
	if err := os.WriteFile(p, []byte("random garbage bytes that are not a font"), 0644); err != nil {
		t.Fatalf("write corrupt font: %v", err)
	}
	_, err := ResolveASSFontFamily(p)
	if err == nil {
		t.Fatalf("expected error for corrupt font file")
	}
}

func TestResolveASSFontFamily_ExtractsFamilyName(t *testing.T) {
	family := "UnitTestCustomFont"
	fontPath := writeTestFontForMedia(t, t.TempDir(), family)

	got, err := ResolveASSFontFamily(fontPath)
	if err != nil {
		t.Fatalf("ResolveASSFontFamily failed: %v", err)
	}
	if got != family {
		t.Errorf("ResolveASSFontFamily = %q, want %q", got, family)
	}
}

func TestComputeFontSHA256_EmptyAndValid(t *testing.T) {
	// Empty path -> ("", nil)
	gotEmpty, err := ComputeFontSHA256("")
	if err != nil {
		t.Fatalf("unexpected error for empty font file: %v", err)
	}
	if gotEmpty != "" {
		t.Errorf("expected empty SHA for empty font file, got %q", gotEmpty)
	}

	// Missing file -> error
	_, err = ComputeFontSHA256(filepath.Join(t.TempDir(), "nonexistent.ttf"))
	if err == nil {
		t.Fatalf("expected error for nonexistent font file")
	}

	// Valid file -> matches sha256.Sum256
	data := []byte("minimal-test-font-content-12345")
	p := filepath.Join(t.TempDir(), "test_font.ttf")
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatalf("write test font file: %v", err)
	}
	expectedSHA := fmt.Sprintf("%x", sha256.Sum256(data))
	gotSHA, err := ComputeFontSHA256(p)
	if err != nil {
		t.Fatalf("ComputeFontSHA256 failed: %v", err)
	}
	if gotSHA != expectedSHA {
		t.Errorf("ComputeFontSHA256 = %q, want %q", gotSHA, expectedSHA)
	}
}

func TestAssFilterSpec_WithAndWithoutFont(t *testing.T) {
	// Platform-native relative paths containing spaces and apostrophes, with expected
	// ASS filter specs spelled out literally so they cannot track a shared escaping bug.
	assPath := filepath.Join("sub 'titles dir", "test 'sub.ass")
	fontFile := filepath.Join("fonts 'dir with space", "My 'Font.ttf")

	// Without font file
	specNoFont := assFilterSpec(assPath, "")
	wantNoFont := "ass=filename='sub \\'titles dir/test \\'sub.ass'"
	if specNoFont != wantNoFont {
		t.Errorf("assFilterSpec without font = %q, want %q", specNoFont, wantNoFont)
	}

	// With font file: fontsdir is the font file's directory
	specWithFont := assFilterSpec(assPath, fontFile)
	wantWithFont := "ass=filename='sub \\'titles dir/test \\'sub.ass':fontsdir='fonts \\'dir with space'"
	if specWithFont != wantWithFont {
		t.Errorf("assFilterSpec with font = %q, want %q", specWithFont, wantWithFont)
	}
}

func TestParseFontFamily_MacintoshNonASCII_FailsClosed(t *testing.T) {
	// Font carries only a Macintosh record containing non-ASCII Mac Roman bytes (e.g. 0x8E = É in Mac Roman).
	// It must fail closed with ErrFontFamilyNotFound rather than mis-decoding the raw bytes as UTF-8.
	fontPath := writeTestFontWithRecords(t, t.TempDir(), "mac_nonascii_only.ttf", []testNameRecord{
		{
			platformID: 1,                                          // Macintosh
			encodingID: 0,                                          // Roman
			languageID: 0,                                          // English
			nameID:     1,                                          // Font Family
			data:       []byte{0x8E, 'l', 'e', 'g', 'a', 'n', 't'}, // 0x8E is non-ASCII
		},
	})

	got, err := ResolveASSFontFamily(fontPath)
	if err == nil {
		t.Fatalf("expected error for non-ASCII Macintosh font, got %q", got)
	}
	if !errors.Is(err, ErrFontFamilyNotFound) {
		t.Errorf("expected ErrFontFamilyNotFound, got: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string on failure, got %q", got)
	}
}

func TestParseFontFamily_MacintoshNonASCII_FallbackToWindows(t *testing.T) {
	// Font has a non-ASCII Macintosh record AND a valid Windows UTF-16 record.
	// The non-ASCII Mac record must be skipped, deterministically falling back to Windows.
	fontPath := writeTestFontWithRecords(t, t.TempDir(), "mac_nonascii_windows_fallback.ttf", []testNameRecord{
		{
			platformID: 1,                                          // Macintosh
			encodingID: 0,                                          // Roman
			languageID: 0,                                          // English
			nameID:     16,                                         // Typographic Family
			data:       []byte{0x8E, 'l', 'e', 'g', 'a', 'n', 't'}, // non-ASCII
		},
		{
			platformID: 3,      // Windows
			encodingID: 1,      // Unicode BMP
			languageID: 0x0409, // English US
			nameID:     1,      // Font Family
			data:       encodeUTF16BE("WindowsValidFamily"),
		},
	})

	got, err := ResolveASSFontFamily(fontPath)
	if err != nil {
		t.Fatalf("expected successful resolution via Windows fallback, got: %v", err)
	}
	if got != "WindowsValidFamily" {
		t.Errorf("got %q, want %q", got, "WindowsValidFamily")
	}
}

func TestParseFontFamily_MacintoshUnsupportedEncoding_FallbackToWindows(t *testing.T) {
	// Font with an unsupported Macintosh encoding (encodingID != 0, e.g. 1 = Japanese).
	// Must be skipped and fall back to valid Windows record.
	fontPath := writeTestFontWithRecords(t, t.TempDir(), "mac_unsupported_enc.ttf", []testNameRecord{
		{
			platformID: 1,  // Macintosh
			encodingID: 1,  // Japanese (unsupported without dedicated charmap)
			languageID: 11, // Japanese
			nameID:     1,
			data:       []byte{0x93, 0xfa, 0x96, 0x7b}, // Shift-JIS bytes
		},
		{
			platformID: 3,      // Windows
			encodingID: 1,      // Unicode BMP
			languageID: 0x0409, // English US
			nameID:     1,
			data:       encodeUTF16BE("JapaneseFontWindowsName"),
		},
	})

	got, err := ResolveASSFontFamily(fontPath)
	if err != nil {
		t.Fatalf("expected resolution via Windows fallback, got: %v", err)
	}
	if got != "JapaneseFontWindowsName" {
		t.Errorf("got %q, want %q", got, "JapaneseFontWindowsName")
	}
}

func TestParseFontFamily_MacintoshASCII_Supported(t *testing.T) {
	// Font with only an ASCII Macintosh record (platform 1, encoding 0).
	// Pure ASCII is byte-compatible with UTF-8 and resolves correctly.
	family := "ValidMacASCIIFont"
	fontPath := writeTestFontWithRecords(t, t.TempDir(), "mac_ascii_only.ttf", []testNameRecord{
		{
			platformID: 1, // Macintosh
			encodingID: 0, // Roman
			languageID: 0, // English
			nameID:     1, // Font Family
			data:       []byte(family),
		},
	})

	got, err := ResolveASSFontFamily(fontPath)
	if err != nil {
		t.Fatalf("expected resolution for valid ASCII Macintosh font, got: %v", err)
	}
	if got != family {
		t.Errorf("got %q, want %q", got, family)
	}
}

type testNameRecord struct {
	platformID uint16
	encodingID uint16
	languageID uint16
	nameID     uint16
	data       []byte
}

func encodeUTF16BE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, len(units)*2)
	for i, u := range units {
		b[i*2] = byte(u >> 8)
		b[i*2+1] = byte(u & 0xFF)
	}
	return b
}

func writeTestFontWithRecords(t *testing.T, dir, filename string, records []testNameRecord) string {
	t.Helper()

	recordCount := uint16(len(records))
	stringOffset := uint16(6 + 12*int(recordCount))

	nameTable := new(bytes.Buffer)
	nameTable.Write([]byte{0x00, 0x00})                                         // format 0
	nameTable.Write([]byte{byte(recordCount >> 8), byte(recordCount & 0xFF)})   // count
	nameTable.Write([]byte{byte(stringOffset >> 8), byte(stringOffset & 0xFF)}) // string offset

	var strStorage bytes.Buffer
	for _, rec := range records {
		offset := uint16(strStorage.Len())
		length := uint16(len(rec.data))

		recBytes := make([]byte, 12)
		binary.BigEndian.PutUint16(recBytes[0:2], rec.platformID)
		binary.BigEndian.PutUint16(recBytes[2:4], rec.encodingID)
		binary.BigEndian.PutUint16(recBytes[4:6], rec.languageID)
		binary.BigEndian.PutUint16(recBytes[6:8], rec.nameID)
		binary.BigEndian.PutUint16(recBytes[8:10], length)
		binary.BigEndian.PutUint16(recBytes[10:12], offset)

		nameTable.Write(recBytes)
		strStorage.Write(rec.data)
	}
	nameTable.Write(strStorage.Bytes())
	for nameTable.Len()%4 != 0 {
		nameTable.WriteByte(0)
	}

	nameLen := uint32(nameTable.Len())
	nameOffset := uint32(28)

	fontBuf := new(bytes.Buffer)
	fontBuf.Write([]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00})
	fontBuf.WriteString("name")
	fontBuf.Write([]byte{0x00, 0x00, 0x00, 0x00})
	fontBuf.Write([]byte{byte(nameOffset >> 24), byte(nameOffset >> 16), byte(nameOffset >> 8), byte(nameOffset)})
	fontBuf.Write([]byte{byte(nameLen >> 24), byte(nameLen >> 16), byte(nameLen >> 8), byte(nameLen)})
	fontBuf.Write(nameTable.Bytes())

	fontPath := filepath.Join(dir, filename)
	if err := os.WriteFile(fontPath, fontBuf.Bytes(), 0644); err != nil {
		t.Fatalf("write test font %s: %v", filename, err)
	}
	return fontPath
}

func writeTestFontForMedia(t *testing.T, dir, family string) string {
	t.Helper()
	return writeTestFontWithRecords(t, dir, "media_test_font.ttf", []testNameRecord{
		{
			platformID: 3,      // Windows
			encodingID: 1,      // Unicode BMP
			languageID: 0x0409, // English US
			nameID:     1,      // Font Family
			data:       encodeUTF16BE(family),
		},
	})
}
