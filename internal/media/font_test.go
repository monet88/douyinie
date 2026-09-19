package media

import (
	"bytes"
	"crypto/sha256"
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
	assPath := `C:\tmp\subtitles.ass`
	escapedAss := EscapeFFmpegFilterPath(assPath)

	// Without font file
	specNoFont := assFilterSpec(assPath, "")
	expectedNoFont := "ass=filename='" + escapedAss + "'"
	if specNoFont != expectedNoFont {
		t.Errorf("assFilterSpec without font = %q, want %q", specNoFont, expectedNoFont)
	}

	// With font file
	fontFile := `C:\custom fonts\MyFont.ttf`
	specWithFont := assFilterSpec(assPath, fontFile)
	escapedDir := EscapeFFmpegFilterPath(filepath.Dir(fontFile))
	expectedWithFont := "ass=filename='" + escapedAss + "':fontsdir='" + escapedDir + "'"
	if specWithFont != expectedWithFont {
		t.Errorf("assFilterSpec with font = %q, want %q", specWithFont, expectedWithFont)
	}
}

func writeTestFontForMedia(t *testing.T, dir, family string) string {
	t.Helper()
	utf16Units := utf16.Encode([]rune(family))
	strBytes := make([]byte, len(utf16Units)*2)
	for i, u := range utf16Units {
		strBytes[i*2] = byte(u >> 8)
		strBytes[i*2+1] = byte(u & 0xFF)
	}

	nameTable := new(bytes.Buffer)
	nameTable.Write([]byte{0x00, 0x00})                                                 // format 0
	nameTable.Write([]byte{0x00, 0x01})                                                 // count = 1
	nameTable.Write([]byte{0x00, 18})                                                   // string offset = 18
	nameTable.Write([]byte{0x00, 0x03, 0x00, 0x01, 0x04, 0x09, 0x00, 0x01})             // platform 3, encoding 1, lang 0x409, nameID 1
	nameTable.Write([]byte{byte(len(strBytes) >> 8), byte(len(strBytes) & 0xFF), 0, 0}) // length, offset=0
	nameTable.Write(strBytes)
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

	fontPath := filepath.Join(dir, "media_test_font.ttf")
	if err := os.WriteFile(fontPath, fontBuf.Bytes(), 0644); err != nil {
		t.Fatalf("write test font: %v", err)
	}
	return fontPath
}
