package media

import "testing"

func TestAudioSTTMime(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		fallback    string
		want        string
	}{
		{"empty falls back", "", "audio/ogg", "audio/ogg"},
		{"whitespace falls back", "   ", "audio/mpeg", "audio/mpeg"},
		{"octet-stream is not a format", "application/octet-stream", "audio/ogg", "audio/ogg"},
		{"octet-stream casing", "Application/OCTET-STREAM", "audio/ogg", "audio/ogg"},
		{"real type passes through", "audio/webm; codecs=opus", "audio/ogg", "audio/webm; codecs=opus"},
		{"silk is not rewritten into ogg", "audio/silk", "audio/ogg", "audio/silk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AudioSTTMime(tt.contentType, tt.fallback); got != tt.want {
				t.Fatalf("AudioSTTMime(%q, %q) = %q, want %q", tt.contentType, tt.fallback, got, tt.want)
			}
		})
	}
}
