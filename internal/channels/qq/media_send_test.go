package qq

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// mediaREST is the QQ REST surface for rich media: the upload endpoint and the send endpoint
// recorded separately, because the interesting assertions are about which one was called with
// what, and in which order.
type mediaREST struct {
	t           *testing.T
	mu          sync.Mutex
	uploads     []mediaUploadRequest
	uploadPaths []string
	sends       []sendRequest
	sendPaths   []string

	uploadErrCode int
	fileInfo      string
}

func newMediaREST(t *testing.T) (*mediaREST, *api) {
	t.Helper()
	f := &mediaREST{t: t, fileInfo: "FI-1"}

	mux := http.NewServeMux()
	mux.HandleFunc(tokenPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 7200})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/files") {
			var req mediaUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				f.t.Errorf("decode upload: %v", err)
			}
			f.uploads = append(f.uploads, req)
			f.uploadPaths = append(f.uploadPaths, r.URL.Path)
			if f.uploadErrCode != 0 {
				_, _ = w.Write([]byte(`{"err_code":` + strconv.Itoa(f.uploadErrCode) + `,"message":"不支持的文件格式"}`))
				return
			}
			_, _ = w.Write([]byte(`{"err_code":0,"file_uuid":"u1","file_info":"` + f.fileInfo + `","ttl":300}`))
			return
		}

		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.t.Errorf("decode send: %v", err)
		}
		f.sends = append(f.sends, req)
		f.sendPaths = append(f.sendPaths, r.URL.Path)
		_, _ = w.Write([]byte(`{"err_code":0,"id":"SENT1"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, err := newAPI(srv.URL, "app", "secret")
	if err != nil {
		t.Fatalf("newAPI() error = %v", err)
	}
	return f, a
}

func (f *mediaREST) recorded() ([]mediaUploadRequest, []string, []sendRequest, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mediaUploadRequest(nil), f.uploads...),
		append([]string(nil), f.uploadPaths...),
		append([]sendRequest(nil), f.sends...),
		append([]string(nil), f.sendPaths...)
}

func writeTempMedia(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write media: %v", err)
	}
	return path
}

func TestSendUploadsThenSendsRichMedia(t *testing.T) {
	f, a := newMediaREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	chat := chatPrefixC2C + "U1"
	c.noteInbound(chat, "MID1")
	png := writeTempMedia(t, "chart.png", append([]byte("\x89PNG\r\n\x1a\n"), []byte("payload")...))

	if err := c.Send(context.Background(), bus.OutboundMessage{
		ChatID:  chat,
		Content: "看图",
		Media:   []bus.MediaAttachment{{URL: png, ContentType: "image/png"}},
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	uploads, uploadPaths, sends, sendPaths := f.recorded()
	if len(uploads) != 1 || len(sends) != 2 {
		t.Fatalf("uploads=%d sends=%d, want one upload then a text and a media message", len(uploads), len(sends))
	}
	if uploadPaths[0] != "/v2/users/U1/files" {
		t.Fatalf("upload path = %q, want the C2C upload endpoint", uploadPaths[0])
	}
	if uploads[0].FileType != fileTypeImage || uploads[0].SrvSendMsg {
		t.Fatalf("upload = %+v, want file_type=1 and srv_send_msg=false", uploads[0])
	}
	body, err := base64.StdEncoding.DecodeString(uploads[0].FileData)
	if err != nil {
		t.Fatalf("file_data is not base64: %v", err)
	}
	want, err := os.ReadFile(png)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("file_data carries %d bytes, want the %d-byte file", len(body), len(want))
	}
	if uploads[0].FileName != "chart.png" {
		t.Fatalf("file_name = %q", uploads[0].FileName)
	}

	// The text keeps the passive reference; the attachment follows as its own message so the
	// reply budget is spent on answering the human first.
	if sends[0].MsgType != msgTypeText || sends[0].MsgID != "MID1" {
		t.Fatalf("first send = %+v, want the passive text reply", sends[0])
	}
	if sends[1].MsgType != msgTypeMedia || sends[1].Media == nil || sends[1].Media.FileInfo != "FI-1" {
		t.Fatalf("second send = %+v, want msg_type 7 carrying the uploaded file_info", sends[1])
	}
	if sends[1].MsgSeq != 2 {
		t.Fatalf("media msg_seq = %d, want it to continue after the text chunk", sends[1].MsgSeq)
	}
	if sendPaths[1] != "/v2/users/U1/messages" {
		t.Fatalf("send path = %q", sendPaths[1])
	}
}

func TestSendGroupMediaUsesGroupUploadEndpoint(t *testing.T) {
	f, a := newMediaREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	chat := chatPrefixGroup + "G7"
	pdf := writeTempMedia(t, "报告.pdf", []byte("%PDF-1.4 ...\n"))

	if err := c.Send(context.Background(), bus.OutboundMessage{
		ChatID: chat,
		Media:  []bus.MediaAttachment{{URL: pdf}},
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	uploads, uploadPaths, sends, _ := f.recorded()
	if uploadPaths[0] != "/v2/groups/G7/files" {
		t.Fatalf("upload path = %q, want the group endpoint: uploads are not interchangeable", uploadPaths[0])
	}
	if len(uploads) != 1 || uploads[0].FileType != fileTypeFile {
		t.Fatalf("uploads = %+v, want a file-type upload for a pdf with no content type", uploads)
	}
	if len(sends) != 1 || sends[0].MsgType != msgTypeMedia {
		t.Fatalf("sends = %+v, want only the media message (no text was given)", sends)
	}
}

func TestSendAnnouncesFailedAttachment(t *testing.T) {
	f, a := newMediaREST(t)
	f.mu.Lock()
	f.uploadErrCode = 850019
	f.mu.Unlock()

	c := channelWithAPI(t, a, InstanceConfig{})
	bad := writeTempMedia(t, "clip.mov", []byte("moov..."))

	err := c.Send(context.Background(), bus.OutboundMessage{
		ChatID:  chatPrefixC2C + "U1",
		Content: "看视频",
		Media:   []bus.MediaAttachment{{URL: bad, ContentType: "video/quicktime"}},
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want the failure announced rather than returned", err)
	}

	_, _, sends, _ := f.recorded()
	if len(sends) != 2 {
		t.Fatalf("sends = %+v, want the text plus the failure note", sends)
	}
	if !strings.Contains(sends[1].Content, "附件发送失败") || !strings.Contains(sends[1].Content, "clip.mov") {
		t.Fatalf("note = %+v, want it to name the attachment that did not arrive", sends[1])
	}
}

func TestUploadMediaRefusesOversizedWithoutRequesting(t *testing.T) {
	f, a := newMediaREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	_, err := c.uploadMedia(context.Background(), "c2c", "U1", "big.bin", make([]byte, qqMaxOutboundMediaBytes+1), fileTypeFile)
	if err == nil || !strings.Contains(err.Error(), "byte send cap") {
		t.Fatalf("error = %v, want the size ceiling to refuse before any upload", err)
	}
	if uploads, _, _, _ := f.recorded(); len(uploads) != 0 {
		t.Fatal("an oversized file must not reach the network")
	}

	if _, err := c.uploadMedia(context.Background(), "c2c", "U1", "empty.bin", nil, fileTypeFile); err == nil {
		t.Fatal("an empty upload was accepted")
	}
}

func TestQQFileTypeMapping(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		fileName    string
		want        int
	}{
		{"image by mime", "image/jpeg", "photo", fileTypeImage},
		{"tts voice is a voice card", "audio/ogg", "auto-2026.ogg", fileTypeVoice},
		{"mp3 voice", "audio/mpeg", "say.mp3", fileTypeVoice},
		{"mp4 video", "video/mp4", "clip.mp4", fileTypeVideo},
		{"other container becomes a file", "video/quicktime", "clip.mov", fileTypeFile},
		{"extension fills a missing mime", "", "shot.png", fileTypeImage},
		{"unknown becomes a file", "", "mystery", fileTypeFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qqFileTypeFor(tt.contentType, tt.fileName); got != tt.want {
				t.Fatalf("qqFileTypeFor(%q,%q) = %d, want %d", tt.contentType, tt.fileName, got, tt.want)
			}
		})
	}
}

func TestLocalMediaDropsEmptyEntriesOnly(t *testing.T) {
	files := localMedia([]bus.MediaAttachment{
		{URL: "  "},
		{URL: "/tmp/a.png"},
		{URL: "https://remote.example/b.png"},
	})
	if len(files) != 2 {
		t.Fatalf("files = %+v, want a blank path dropped and everything else kept for reporting", files)
	}
}

func TestTTSVoiceForTextWithoutManager(t *testing.T) {
	_, a := newMediaREST(t)
	c := channelWithAPI(t, a, InstanceConfig{})

	content, voice := c.ttsVoiceForText(context.Background(), "[[tts]]你好[[/tts]]", "c2c:U1", false)
	if voice != nil {
		t.Fatal("a channel with no audio manager invented audio")
	}
	// audioMgr is nil, so nothing stripped: this is exactly why the marker must not be
	// delivered literally, and why the channel is wired to the manager in the factory.
	if content != "[[tts]]你好[[/tts]]" {
		t.Fatalf("content = %q", content)
	}
}
