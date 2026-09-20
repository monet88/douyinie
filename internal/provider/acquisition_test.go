package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/monet88/douyinie/internal/domain"
)

func TestClassifyAcquisitionFailure(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   domain.AcquisitionState
	}{
		{"captcha", "Please complete Safety Check captcha to continue", domain.AcquisitionCaptchaRequired},
		{"session expired", "error: cookie expired, please re-login", domain.AcquisitionSessionExpired},
		{"auth required", "aweme detail requires login cookie", domain.AcquisitionAuthRequired},
		{"anti-bot empty", "empty response from server, likely anti-bot", domain.AcquisitionAntiBotOrEmpty},
		{"anti-bot status", `status_code": -1 retrying`, domain.AcquisitionAntiBotOrEmpty},
		{"anti-bot 403 forbidden", "HTTP Error 403: Forbidden - WAF challenge", domain.AcquisitionAntiBotOrEmpty},
		{"argus deterministic gate", "Blocked by ArgusSecurityPlugin Uifid Not Found", domain.AcquisitionAntiBotOrEmpty},
		{"anti-bot 429 rate limit", "HTTP 429: Too Many Requests / rate limit reached", domain.AcquisitionAntiBotOrEmpty},
		{"removed", "aweme not found or removed", domain.AcquisitionContentUnavailable},
		{"410 gone", "HTTP status 410: resource gone", domain.AcquisitionContentUnavailable},
		{"private", "this account is private", domain.AcquisitionContentUnavailable},
		{"generic", "connection reset by peer", domain.AcquisitionDownloadFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAcquisitionFailure(tc.output); got != tc.want {
				t.Errorf("ClassifyAcquisitionFailure(%q) = %s, want %s", tc.output, got, tc.want)
			}
		})
	}
}

func TestExtractAwemeID(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://www.douyin.com/video/7659396126545619322?previous_page=web_code_link", "7659396126545619322"},
		{"https://www.douyin.com/note/1234567890123456789", "1234567890123456789"},
		{"https://www.douyin.com/discover?modal_id=9876543210987654321", "9876543210987654321"},
		{"https://www.douyin.com/user/xyz?aweme_id=5555666677778888999", "5555666677778888999"},
		{"https://example.com/nothing", ""},
	}
	for _, tc := range cases {
		if got := extractAwemeID(tc.url); got != tc.want {
			t.Errorf("extractAwemeID(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestAcquisitionErrorUnwrap(t *testing.T) {
	hard := &domain.AcquisitionError{State: domain.AcquisitionContentUnavailable, ProviderID: "x"}
	if !errors.Is(hard, domain.ErrContentUnavailable) {
		t.Error("CONTENT_UNAVAILABLE must unwrap to ErrContentUnavailable for fail-closed matching")
	}
	invalid := &domain.AcquisitionError{State: domain.AcquisitionInvalidURL, ProviderID: "x"}
	if !errors.Is(invalid, domain.ErrInvalidURL) {
		t.Error("INVALID_URL must unwrap to ErrInvalidURL for fail-closed matching")
	}
	unsupported := &domain.AcquisitionError{State: domain.AcquisitionUnsupportedMediaType, ProviderID: "x"}
	if !errors.Is(unsupported, domain.ErrUnsupportedMediaType) {
		t.Error("UNSUPPORTED_MEDIA_TYPE must unwrap to ErrUnsupportedMediaType for fail-closed matching")
	}
	soft := &domain.AcquisitionError{State: domain.AcquisitionDownloadFailed, ProviderID: "x"}
	if errors.Is(soft, domain.ErrContentUnavailable) || errors.Is(soft, domain.ErrInvalidURL) || errors.Is(soft, domain.ErrUnsupportedMediaType) {
		t.Error("DOWNLOAD_FAILED must not unwrap to a hard-block sentinel")
	}
}

// TestFakeAcquisitionMimeTypeReflectsProducedFile proves AcquiredMedia.MimeType
// follows the produced media file's container (MediaExt) instead of a
// hardcoded video/mp4.
func TestFakeAcquisitionMimeTypeReflectsProducedFile(t *testing.T) {
	for ext, want := range map[string]string{".mp4": "video/mp4", ".webm": "video/webm", ".mkv": "video/x-matroska"} {
		p := NewFakeAcquisitionProvider("fake_mime_"+ext, domain.PolicyAllowed, 0.5)
		p.MediaExt = ext
		media, err := p.Acquire(context.Background(), domain.SourceDescriptor{SourceID: "douyin:aweme:9"}, t.TempDir(), "")
		if err != nil {
			t.Fatalf("acquire %s: %v", ext, err)
		}
		if media.MimeType != want {
			t.Errorf("ext %s: expected mime %s, got %s", ext, want, media.MimeType)
		}
	}
}

// TestCLIAdapterAcquireNoNilPanic guards the constructor wiring regression:
// buildArgs must be assigned, or Acquire nil-panics when it builds argv.
// Uses the real `go` binary with a harmless arg template so the subprocess
// path executes without network; the adapter must return a structured error,
// never panic.
func TestCLIAdapterAcquireNoNilPanic(t *testing.T) {
	adapter := NewJijiAdapter("test", "go", "version", func(ctx context.Context, authRef, providerID string) (string, error) {
		return "fake-session-secret", nil
	})
	dest := t.TempDir()
	media, err := adapter.Acquire(context.Background(), domain.SourceDescriptor{SourceID: "douyin:aweme:1", CanonicalURL: "https://example.invalid/x"}, dest, "cred-ref")
	if err == nil {
		t.Fatalf("expected structured failure, got media %+v", media)
	}
	var acqErr *domain.AcquisitionError
	if !errors.As(err, &acqErr) {
		t.Fatalf("expected *domain.AcquisitionError, got %T: %v", err, err)
	}
}
