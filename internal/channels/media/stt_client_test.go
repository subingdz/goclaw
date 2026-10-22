package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The two silent cases in TranscribeAudio were a documented decision (internal/audio/proxy_stt's
// own tests pin it) that nothing in this package covered: an empty payload and an unconfigured
// proxy both answer ("", nil) without dialling, because a second provider handed the same nothing
// would fail identically and the result would only be a warning stream per voice message.
// A path that exists but points at a missing file is the opposite case — that is an error, and the
// chain does fall through.

func TestTranscribeAudioSilentCasesDialNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "should not have been called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		cfg    STTConfig
		path   string
		wantOK bool
	}{
		{"no payload", STTConfig{ProxyURL: srv.URL}, "", true},
		{"no proxy configured", STTConfig{}, "/tmp/voice.ogg", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := TranscribeAudio(ctx, tc.cfg, tc.path)
			if err != nil {
				t.Fatalf("TranscribeAudio() error = %v, want the silent no-op", err)
			}
			if text != "" {
				t.Fatalf("text = %q, want empty", text)
			}
		})
	}
	if hits != 0 {
		t.Fatalf("the proxy was dialled %d times for two no-op cases", hits)
	}
}

// The counterpart the proxy_stt tests already pin, restated here so the boundary is covered in
// the package that owns it: a path that is set but unreadable must be an error.
func TestTranscribeAudioMissingFileIsAnError(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
	}))
	t.Cleanup(srv.Close)

	text, err := TranscribeAudio(context.Background(), STTConfig{ProxyURL: srv.URL}, filepath.Join(t.TempDir(), "absent.ogg"))
	if err == nil {
		t.Fatalf("TranscribeAudio() = %q, nil — an unreadable payload must not look like success", text)
	}
	if hits != 0 {
		t.Fatalf("a missing file still reached the proxy %d times", hits)
	}
}

// And the happy path, so the silent cases above are known to be skips rather than a broken
// client: a real file is posted and the transcript is read back.
func TestTranscribeAudioPostsTheFileAndReadsTheTranscript(t *testing.T) {
	var gotField, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sttTranscribeEndpoint {
			t.Errorf("path = %q, want %s", r.URL.Path, sttTranscribeEndpoint)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			return
		}
		gotField = r.FormValue("tenant_id")
		f, hdr, err := r.FormFile("file")
		if err == nil {
			gotName = hdr.Filename
			defer f.Close()
		}
		_, _ = w.Write([]byte(`{"transcript":"xin chao"}`))
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(path, []byte("oggbytes"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	text, err := TranscribeAudio(context.Background(), STTConfig{ProxyURL: srv.URL, TenantID: "t-1"}, path)
	if err != nil {
		t.Fatalf("TranscribeAudio() error = %v", err)
	}
	if text != "xin chao" {
		t.Fatalf("text = %q, want the transcript from the proxy", text)
	}
	if gotName != "voice.ogg" {
		t.Fatalf("uploaded filename = %q, want voice.ogg", gotName)
	}
	if gotField != "t-1" {
		t.Fatalf("tenant_id field = %q, want t-1", gotField)
	}
}
