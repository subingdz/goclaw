package qq

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
)

// stubRT answers every request from a queue without touching the network, so the download
// path runs end to end while tools.CheckSSRF still judges the URL. 8.8.8.8 is used as the
// "public" address because the checker resolves names, and a test must not depend on DNS.
type stubRT struct {
	responses []*http.Response
	bodies    []string
	calls     []string
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls = append(s.calls, req.URL.String())
	if len(s.responses) == 0 {
		return nil, errors.New("stub: no response left")
	}
	resp := s.responses[0]
	body := s.bodies[0]
	s.responses, s.bodies = s.responses[1:], s.bodies[1:]
	resp.Body = io.NopCloser(strings.NewReader(body))
	resp.Request = req
	return resp, nil
}

// stubResponse describes one canned HTTP reply. The body is a string because the client
// closes the reader it is handed, and a shared bytes.Reader would be consumed once.
type stubResponse struct {
	status int
	header http.Header
	body   string
}

func newMediaTestChannel(t *testing.T, responses ...*stubResponse) (*Channel, *stubRT) {
	t.Helper()

	rt := &stubRT{}
	for _, r := range responses {
		rt.bodies = append(rt.bodies, r.body)
		rt.responses = append(rt.responses, &http.Response{
			StatusCode: r.status,
			Header:     r.header,
		})
	}

	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{DMPolicy: "open"}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// The production redirect guard is attached explicitly: it is part of what these tests
	// are about, and a stub client must not quietly drop it.
	c.mediaHTTP = &http.Client{Transport: rt, CheckRedirect: c.checkRedirect}
	return c, rt
}

func okResponse(body string, mime string) *stubResponse {
	h := http.Header{}
	if mime != "" {
		h.Set("Content-Type", mime)
	}
	return &stubResponse{status: http.StatusOK, header: h, body: body}
}

func TestAttachmentKind(t *testing.T) {
	tests := []struct {
		name string
		att  attachmentRef
		want string
	}{
		{"content type audio", attachmentRef{ContentType: "audio/mpeg"}, "audio"},
		{"content type image", attachmentRef{ContentType: "image/png"}, "image"},
		{"content type video", attachmentRef{ContentType: "video/mp4"}, "video"},
		{"silk extension with no content type", attachmentRef{Filename: "msg.slk"}, "audio"},
		{"silk extension, generic content type", attachmentRef{Filename: "voice.silk", ContentType: "application/octet-stream"}, "audio"},
		{"image by name", attachmentRef{Filename: "photo.jpg", ContentType: "application/octet-stream"}, "image"},
		{"extension from the url path", attachmentRef{URL: "https://example.com/a/b/record.amr?sign=1"}, "audio"},
		{"no signal at all is a file", attachmentRef{URL: "https://example.com/x"}, "file"},
		{"video extension", attachmentRef{Filename: "clip.mp4"}, "video"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attachmentKind(tt.att); got != tt.want {
				t.Fatalf("attachmentKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSafeExt(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{".SLK", ".slk"},
		{".wav", ".wav"},
		{"..slk", ""},
		{".a/b", ""},
		{".htmlxtra", ""},
		{"", ""},
		{"slk", ""},
		{".mp4", ".mp4"},
	}
	for _, tt := range tests {
		if got := safeExt(tt.in); got != tt.want {
			t.Fatalf("safeExt(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDownloadAttachmentWritesTempFile(t *testing.T) {
	body := "#!SILK_V3 fake payload"
	c, _ := newMediaTestChannel(t, okResponse(body, "audio/silk"))

	info, err := c.downloadAttachment(context.Background(), attachmentRef{
		URL: "http://8.8.8.8/voice.slk", Filename: "voice.slk", ContentType: "audio/silk",
	})
	if err != nil {
		t.Fatalf("downloadAttachment() error = %v", err)
	}
	t.Cleanup(func() { os.Remove(info.FilePath) })

	if info.Type != media.TypeVoice {
		t.Fatalf("type = %q, want voice", info.Type)
	}
	if info.ContentType != "audio/silk" {
		t.Fatalf("mime = %q, want the response content type", info.ContentType)
	}
	if info.FileSize != int64(len(body)) {
		t.Fatalf("size = %d, want %d", info.FileSize, len(body))
	}
	saved, err := os.ReadFile(info.FilePath)
	if err != nil || string(saved) != body {
		t.Fatalf("saved bytes = %q (err %v), want the payload", saved, err)
	}
	if !strings.HasSuffix(info.FilePath, ".slk") {
		t.Fatalf("temp path = %q, want the attachment extension kept", info.FilePath)
	}
	if info.SourceURL != "http://8.8.8.8/voice.slk" {
		t.Fatalf("source url = %q, want it recorded for the agent", info.SourceURL)
	}
}

func TestDownloadAttachmentMimeFromNameWhenResponseSaysNothing(t *testing.T) {
	c, _ := newMediaTestChannel(t, okResponse("RIFFxxxxWAVE", ""))

	info, err := c.downloadAttachment(context.Background(), attachmentRef{URL: "http://8.8.8.8/a.wav", Filename: "a.wav"})
	if err != nil {
		t.Fatalf("downloadAttachment() error = %v", err)
	}
	t.Cleanup(func() { os.Remove(info.FilePath) })
	if !strings.HasPrefix(info.ContentType, "audio/") {
		t.Fatalf("mime = %q, want it detected from the filename", info.ContentType)
	}
}

func TestDownloadAttachmentRefusals(t *testing.T) {
	tests := []struct {
		name     string
		att      attachmentRef
		contains string
	}{
		{"no url", attachmentRef{Filename: "a.slk"}, "no url"},
		{"declared too large", attachmentRef{URL: "http://8.8.8.8/big.bin", Size: qqMaxMediaBytes + 1}, "too large"},
		{"loopback target", attachmentRef{URL: "http://127.0.0.1:9999/x.slk"}, "refused attachment url"},
		{"metadata endpoint", attachmentRef{URL: "http://169.254.169.254/latest/user-data"}, "refused attachment url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rt := newMediaTestChannel(t)
			_, err := c.downloadAttachment(context.Background(), tt.att)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.contains)
			}
			if len(rt.calls) != 0 {
				t.Fatalf("requests made = %v, want none for a refused attachment", rt.calls)
			}
		})
	}
}

// A signed CDN URL that redirects is normal; one that redirects into a private range is the
// attack the guard exists for.
func TestDownloadAttachmentRefusesRedirectToPrivateTarget(t *testing.T) {
	redirect := &stubResponse{
		status: http.StatusFound,
		header: http.Header{"Location": []string{"http://127.0.0.1/internal"}},
	}
	c, rt := newMediaTestChannel(t, redirect)

	_, err := c.downloadAttachment(context.Background(), attachmentRef{URL: "http://8.8.8.8/voice.slk"})
	if err == nil || !strings.Contains(err.Error(), "refused redirect target") {
		t.Fatalf("error = %v, want the redirect refused", err)
	}
	if len(rt.calls) != 1 {
		t.Fatalf("requests made = %v, want the hop after the refused redirect skipped", rt.calls)
	}
}

func TestDownloadAttachmentRejectsEmptyBody(t *testing.T) {
	c, _ := newMediaTestChannel(t, okResponse("", "audio/silk"))
	if _, err := c.downloadAttachment(context.Background(), attachmentRef{URL: "http://8.8.8.8/x.bin"}); err == nil {
		t.Fatal("an empty attachment body was accepted")
	}
}

func TestDownloadAttachmentNon200(t *testing.T) {
	c, _ := newMediaTestChannel(t, &stubResponse{status: http.StatusForbidden})
	_, err := c.downloadAttachment(context.Background(), attachmentRef{URL: "http://8.8.8.8/x.bin"})
	if err == nil || !strings.Contains(err.Error(), "http 403") {
		t.Fatalf("error = %v, want the status code", err)
	}
}

func TestProcessAttachmentsKeepsGoingWhenOneFails(t *testing.T) {
	c, _ := newMediaTestChannel(t, okResponse("#!SILK_V3 payload", "audio/silk"))

	files, tags := c.processAttachments(context.Background(), []attachmentRef{
		{URL: "http://8.8.8.8/voice.slk", Filename: "voice.slk", ContentType: "audio/silk"},
		{URL: "http://127.0.0.1/evil.slk", Filename: "evil.slk"},
	})
	t.Cleanup(func() {
		for _, f := range files {
			os.Remove(f.Path)
		}
	})

	if len(files) != 1 {
		t.Fatalf("files = %d, want the one that was fetchable", len(files))
	}
	if !strings.Contains(tags, "未能获取") || !strings.Contains(tags, "evil.slk") {
		t.Fatalf("tags = %q, want the failed attachment named instead of dropped", tags)
	}
	// The fetched voice must appear as a media tag, not vanish behind the note.
	if !strings.Contains(tags, "<media:voice>") {
		t.Fatalf("tags = %q, want the downloaded file represented as a voice tag", tags)
	}
}

// End to end at the channel level: download, transcribe, and put the text where the agent
// reads it.
func TestProcessAttachmentsTranscribesVoice(t *testing.T) {
	c, _ := newMediaTestChannel(t, okResponse("#!SILK_V3 payload", "audio/silk"))
	c.WithAudio(newSTTManager(&stubSTT{name: "stub", text: "帮我订张票"}))

	files, tags := c.processAttachments(context.Background(), []attachmentRef{
		{URL: "http://8.8.8.8/voice.slk", Filename: "voice.slk", ContentType: "audio/silk"},
	})
	t.Cleanup(func() {
		for _, f := range files {
			os.Remove(f.Path)
		}
	})

	if len(files) != 1 || files[0].MimeType != "audio/silk" {
		t.Fatalf("files = %+v, want the voice file with its MIME", files)
	}
	if !strings.Contains(tags, "<transcript>帮我订张票</transcript>") {
		t.Fatalf("tags = %q, want the transcript inline", tags)
	}
	if strings.Contains(tags, "未能获取") {
		t.Fatalf("tags = %q, want no failure note when everything was fetched", tags)
	}
}

func TestProcessAttachmentsNothingToSendWhenEmpty(t *testing.T) {
	c, rt := newMediaTestChannel(t)
	files, tags := c.processAttachments(context.Background(), nil)
	if files != nil || tags != "" {
		t.Fatalf("files=%v tags=%q, want a clean no-op", files, tags)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("requests = %v, want none", rt.calls)
	}
}

type stubSTT struct {
	name  string
	text  string
	got   audio.STTInput
	calls int
}

func (s *stubSTT) Name() string { return s.name }

func (s *stubSTT) Transcribe(_ context.Context, in audio.STTInput, _ audio.STTOptions) (*audio.TranscriptResult, error) {
	s.calls++
	s.got = in
	if s.text == "" {
		return nil, errors.New("stub stt failed")
	}
	return &audio.TranscriptResult{Text: s.text}, nil
}

func newSTTManager(p audio.STTProvider) *audio.Manager {
	m := audio.NewManager(audio.ManagerConfig{})
	m.RegisterSTT(p)
	m.SetSTTChain([]string{p.Name()})
	return m
}

func voiceInfo(t *testing.T) *media.MediaInfo {
	t.Helper()
	path := t.TempDir() + "/voice.slk"
	if err := os.WriteFile(path, []byte("#!SILK_V3 payload"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return &media.MediaInfo{
		Type:        media.TypeVoice,
		FilePath:    path,
		ContentType: "audio/silk",
		FileName:    "voice.slk",
	}
}

func TestTranscribeFillsTranscript(t *testing.T) {
	stub := &stubSTT{name: "stub", text: "帮我订张票"}
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.WithAudio(newSTTManager(stub))

	info := voiceInfo(t)
	c.transcribe(context.Background(), info)

	if info.Transcript != "帮我订张票" {
		t.Fatalf("transcript = %q, want the provider text", info.Transcript)
	}
	// The channel hands over what the platform reported; SILK handling is the Manager's job.
	if stub.got.MimeType != "audio/silk" {
		t.Fatalf("mime = %q, want the platform type forwarded", stub.got.MimeType)
	}
	if stub.got.Filename != "voice.slk" {
		t.Fatalf("filename = %q, want the original name forwarded", stub.got.Filename)
	}
}

// A failed transcription must leave the file for the pipeline rather than lose the message.
func TestTranscribeFailureLeavesPlaceholder(t *testing.T) {
	stub := &stubSTT{name: "stub"}
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.WithAudio(newSTTManager(stub))

	info := voiceInfo(t)
	c.transcribe(context.Background(), info)

	if info.Transcript != "" {
		t.Fatalf("transcript = %q, want empty after a failure", info.Transcript)
	}
	if stub.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", stub.calls)
	}
}

func TestTranscribeWithoutManagerIsQuiet(t *testing.T) {
	c, err := New(Credentials{AppID: "a", AppSecret: "b"}, InstanceConfig{}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info := voiceInfo(t)
	c.transcribe(context.Background(), info) // no audio manager: must not panic
	if info.Transcript != "" {
		t.Fatalf("transcript = %q, want nothing", info.Transcript)
	}
}
