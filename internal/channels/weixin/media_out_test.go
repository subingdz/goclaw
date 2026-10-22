package weixin

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
)

// Outbound media is the inverse of the inbound path, so the tests below are mostly about the
// two directions agreeing: the key the upload is told must be the key the message carries, the
// size that gets signed must be the size that gets POSTed, and the bytes the CDN stores must be
// exactly what decryptAESMedia can read back.

// newOutboundChannel points api_base *and* the CDN at stubs: without the base_url override the
// CGI calls leave the process and hit the real iLink endpoint, which answers -14 and makes the
// test depend on Tencent.
func newOutboundChannel(t *testing.T, fake *fakeILink, stub *cdnTransport) *Channel {
	t.Helper()
	if fake.replies == nil {
		fake.replies = []string{`{"ret":0}`}
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	c, err := New(Credentials{Token: "tok"}, InstanceConfig{
		BaseURL:    srv.URL,
		DMPolicy:   "open",
		StateDir:   t.TempDir(),
		CDNBaseURL: "https://8.8.8.8/c2c",
	}, bus.New(), nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if stub != nil {
		c.transport = stub
	}
	c.SetName("weixin")
	c.SetRunning(true)
	if err := c.st.rememberToken("user1", "CTX-1"); err != nil {
		t.Fatalf("rememberToken: %v", err)
	}
	return c
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestOutboundMediaTypeMapping(t *testing.T) {
	c := newOutboundChannel(t, &fakeILink{}, nil)
	cases := []struct {
		name          string
		contentType   string
		fileName      string
		wantMediaType int
	}{
		{"image by mime", "image/png", "x.bin", uploadMediaImage},
		{"video by mime", "video/mp4", "x.bin", uploadMediaVideo},
		{"video by extension", "", "clip.MP4", uploadMediaVideo},
		{"other video is a file", "video/x-matroska", "clip.mkv", uploadMediaFile},
		{"audio is a file", "audio/mpeg", "say.mp3", uploadMediaFile},
		{"document is a file", "application/pdf", "r.pdf", uploadMediaFile},
		{"unknown is a file", "", "notes", uploadMediaFile},
	}
	for _, tt := range cases {
		if got := c.outboundMediaType(tt.contentType, tt.fileName); got != tt.wantMediaType {
			t.Errorf("outboundMediaType(%q,%q) = %d, want %d (%s)", tt.contentType, tt.fileName, got, tt.wantMediaType, tt.name)
		}
	}
}

func TestPaddedLenMatchesWhatEncryptProduces(t *testing.T) {
	// The size signed in getuploadurl must equal the size of the bytes POSTed, or the CDN
	// rejects the object. Both are derived here, so they are asserted against each other.
	for _, n := range []int{1, 15, 16, 17, 31, 32, 4095} {
		sealed, err := encryptAESMedia(testAESKey, bytes.Repeat([]byte("x"), n))
		if err != nil {
			t.Fatalf("encryptAESMedia(%d): %v", n, err)
		}
		if len(sealed) != paddedLen(n) {
			t.Fatalf("n=%d: sealed %d bytes, paddedLen says %d", n, len(sealed), paddedLen(n))
		}
		back, err := decryptAESMedia(testAESKey, sealed)
		if err != nil {
			t.Fatalf("the inbound decryptor cannot read the outbound ciphertext (n=%d): %v", n, err)
		}
		if len(back) != n {
			t.Fatalf("round trip n=%d gave %d bytes", n, len(back))
		}
	}
}

func TestEncryptAESMediaRejectsBadKey(t *testing.T) {
	if _, err := encryptAESMedia([]byte("short"), []byte("data")); err == nil {
		t.Fatal("a 5-byte key was accepted")
	}
}

func TestCDNUploadURLStaysOnThePinnedHost(t *testing.T) {
	c := newOutboundChannel(t, &fakeILink{}, nil)

	got, err := c.cdnUploadURL(&getUploadURLResp{UploadParam: "slot 1"}, "fk")
	if err != nil {
		t.Fatalf("relative slot refused: %v", err)
	}
	if got != "https://8.8.8.8/c2c/upload?encrypted_query_param=slot+1&filekey=fk" {
		t.Fatalf("built url = %q, want the pinned base with both parameters escaped", got)
	}

	onHost := "https://8.8.8.8/c2c/upload?encrypted_query_param=abc"
	got, err = c.cdnUploadURL(&getUploadURLResp{UploadFullURL: onHost, UploadParam: "ignored"}, "fk")
	if err != nil || got != onHost {
		t.Fatalf("on-host answer = %q, %v, want it honored verbatim", got, err)
	}

	// A signed URL pointing somewhere else is a redirect to an arbitrary host, whatever the
	// service's intent was.
	if _, err := c.cdnUploadURL(&getUploadURLResp{UploadFullURL: "https://169.254.169.254/latest/meta-data"}, "fk"); err == nil {
		t.Fatal("off-host upload_full_url accepted")
	}
	if _, err := c.cdnUploadURL(&getUploadURLResp{}, "fk"); err == nil {
		t.Fatal("an answer with neither parameter nor URL accepted")
	}
}

func TestSendOneMediaUploadsThenReferencesWhatCameBack(t *testing.T) {
	plain := pngBytes()
	path := writeFile(t, "photo.png", plain)

	var posted []byte
	stub := &cdnTransport{handler: func(req *http.Request) (*http.Response, error) {
		posted, _ = io.ReadAll(req.Body)
		return cdnResponse(http.StatusOK, nil, http.Header{"X-Encrypted-Param": []string{"dl-token-1"}}), nil
	}}
	fake := &fakeILink{}
	c := newOutboundChannel(t, fake, stub)

	if err := c.sendOneMedia(context.Background(), "user1", bus.MediaAttachment{URL: path, ContentType: "image/png"}, "CTX-1"); err != nil {
		t.Fatalf("sendOneMedia() error = %v", err)
	}

	if len(fake.uploads) != 1 {
		t.Fatalf("getuploadurl calls = %d, want 1", len(fake.uploads))
	}
	up := fake.uploads[0]
	sum := md5.Sum(plain) //nolint:gosec // mirroring the implementation
	if up.MediaType != uploadMediaImage || up.ToUserID != "user1" {
		t.Fatalf("upload = %+v, want an image upload for this peer", up)
	}
	if up.RawSize != len(plain) || up.FileSize != paddedLen(len(plain)) {
		t.Fatalf("sizes raw=%d sealed=%d, want %d and %d", up.RawSize, up.FileSize, len(plain), paddedLen(len(plain)))
	}
	if up.RawFileMD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("rawfilemd5 = %q, want the plaintext digest", up.RawFileMD5)
	}
	key, err := hexAESKey(up.AesKey)
	if err != nil {
		t.Fatalf("aeskey = %q: %v", up.AesKey, err)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("CDN posts = %v, want one", stub.requests)
	}
	if !strings.HasPrefix(stub.requests[0], "https://8.8.8.8/c2c/upload?encrypted_query_param=up-slot-1&filekey=") {
		t.Fatalf("upload url = %q", stub.requests[0])
	}
	if len(posted) != up.FileSize {
		t.Fatalf("posted %d bytes but signed %d", len(posted), up.FileSize)
	}
	// The stored ciphertext must be readable by the channel's own inbound decryptor.
	back, err := decryptAESMedia(key, posted)
	if err != nil || !bytes.Equal(back, plain) {
		t.Fatalf("posted bytes are not the file sealed under the advertised key (err %v)", err)
	}

	if len(fake.sentReqs) != 1 {
		t.Fatalf("sendmessage calls = %d, want 1", len(fake.sentReqs))
	}
	items := fake.sentReqs[0].Msg.ItemList
	if len(items) != 1 || items[0].ItemType != itemTypeImage || items[0].ImageItem == nil {
		t.Fatalf("items = %+v, want one image item", items)
	}
	img := items[0].ImageItem
	if img.Media == nil || img.Media.EncryptQueryParam != "dl-token-1" || img.Media.EncryptType != 1 {
		t.Fatalf("media ref = %+v, want the download parameter from the upload response", img.Media)
	}
	if img.MidSize != int64(len(posted)) {
		t.Fatalf("mid_size = %d, want the stored object size %d", img.MidSize, len(posted))
	}
	// The key the receiver needs travels base64-wrapped around the hex, which is the shape
	// inbound media already parses — assert the two directions really do agree.
	if got, err := base64AESKey(img.Media.AesKey); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("message aes_key does not decode to the upload key (err %v)", err)
	}
	if fake.sentReqs[0].Msg.ContextToken != "CTX-1" {
		t.Fatal("media send dropped the context token; iLink would reject it")
	}
}

func TestSendOneMediaAudioTravelsAsFile(t *testing.T) {
	plain := []byte("ID3\x03\x00\x00\x00fake mp3 body")
	path := writeFile(t, "回答.mp3", plain)

	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, nil, http.Header{"X-Encrypted-Param": []string{"dl-a"}}), nil
	}}
	fake := &fakeILink{}
	c := newOutboundChannel(t, fake, stub)

	if err := c.sendOneMedia(context.Background(), "user1", bus.MediaAttachment{URL: path, ContentType: "audio/mpeg"}, "CTX"); err != nil {
		t.Fatalf("sendOneMedia() error = %v", err)
	}
	if fake.uploads[0].MediaType != uploadMediaFile {
		t.Fatalf("media_type = %d, want FILE: this protocol has no proven voice card", fake.uploads[0].MediaType)
	}
	items := fake.sentReqs[0].Msg.ItemList
	if items[0].ItemType != itemTypeFile || items[0].FileItem == nil {
		t.Fatalf("items = %+v, want a file item", items)
	}
	f := items[0].FileItem
	if f.FileName != "回答.mp3" {
		t.Fatalf("file_name = %q", f.FileName)
	}
	if f.Len != strconv.Itoa(len(plain)) {
		t.Fatalf("len = %q, want the plaintext size %d", f.Len, len(plain))
	}
	sum := md5.Sum(plain) //nolint:gosec
	if f.MD5 != hex.EncodeToString(sum[:]) {
		t.Fatalf("md5 = %q, want the plaintext digest", f.MD5)
	}
}

func TestSendOneMediaRejectsUnusableAttachments(t *testing.T) {
	big := writeFile(t, "big.bin", bytes.Repeat([]byte("x"), weixinMaxOutboundMediaBytes+1))
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"empty", "", "no path"},
		{"remote url", "https://example.com/a.png", "not supported"},
		{"missing file", filepath.Join(os.TempDir(), "goclaw-absent-file.png"), "stat attachment"},
		{"over the cap", big, "over the"},
	}
	c := newOutboundChannel(t, &fakeILink{}, nil)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := c.sendOneMedia(context.Background(), "user1", bus.MediaAttachment{URL: tt.url}, "CTX")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestPutCDNRetriesOnlyTransientFailures(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
		wantTries int
		wantError bool
	}{
		{"502 then 200", []int{http.StatusBadGateway, http.StatusOK}, 2, false},
		{"403 is not retried", []int{http.StatusForbidden}, 1, true},
		{"200 without the header is not retried", []int{http.StatusOK}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n int
			stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
				status := tt.statuses[min(n, len(tt.statuses)-1)]
				n++
				h := http.Header{}
				if status == http.StatusOK && !tt.wantError {
					h.Set("X-Encrypted-Param", "dl-1")
				}
				return cdnResponse(status, []byte("body"), h), nil
			}}
			c := newOutboundChannel(t, &fakeILink{}, stub)

			_, err := c.putCDN(context.Background(), "https://8.8.8.8/c2c/upload?encrypted_query_param=p", "fk", []byte("sealed bytes"))
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %v", err, tt.wantError)
			}
			if stub.roundTrips != tt.wantTries {
				t.Fatalf("attempts = %d, want %d", stub.roundTrips, tt.wantTries)
			}
		})
	}
}

func TestSendTextThenAttachmentsThenNothingElse(t *testing.T) {
	plain := pngBytes()
	img := writeFile(t, "a.png", plain)

	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, nil, http.Header{"X-Encrypted-Param": []string{"dl-1"}}), nil
	}}
	fake := &fakeILink{}
	c := newOutboundChannel(t, fake, stub)

	err := c.Send(context.Background(), bus.OutboundMessage{
		Channel: "weixin",
		ChatID:  "user1",
		Content: "图表如下",
		Media:   []bus.MediaAttachment{{URL: img, ContentType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if len(fake.sentReqs) != 2 {
		t.Fatalf("sends = %d, want the caption and then the image", len(fake.sentReqs))
	}
	if got := fake.sentReqs[0].Msg.ItemList[0].TextItem; got == nil || got.Text != "图表如下" {
		t.Fatalf("first item = %+v, want the text", fake.sentReqs[0].Msg.ItemList[0])
	}
	if fake.sentReqs[1].Msg.ItemList[0].ItemType != itemTypeImage {
		t.Fatalf("second item = %+v, want the image", fake.sentReqs[1].Msg.ItemList[0])
	}
	// A delivered attachment must not also be announced as failed.
	for _, r := range fake.sentReqs {
		if it := r.Msg.ItemList[0].TextItem; it != nil && strings.Contains(it.Text, "附件发送失败") {
			t.Fatalf("unexpected failure note: %q", it.Text)
		}
	}
}

func TestSendAnnouncesFailedAttachments(t *testing.T) {
	good := writeFile(t, "ok.png", pngBytes())
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, nil, http.Header{"X-Encrypted-Param": []string{"dl-1"}}), nil
	}}
	fake := &fakeILink{}
	c := newOutboundChannel(t, fake, stub)

	err := c.Send(context.Background(), bus.OutboundMessage{
		Channel: "weixin", ChatID: "user1", Content: "两份文件",
		Media: []bus.MediaAttachment{
			{URL: good, ContentType: "image/png"},
			{URL: "https://example.com/remote.png"},
		},
	})
	// Text arrived, so the send is not a failure — but the missing attachment is stated.
	if err != nil {
		t.Fatalf("Send() error = %v, want nil because the text was delivered", err)
	}
	var note string
	for _, r := range fake.sentReqs {
		if it := r.Msg.ItemList[0].TextItem; it != nil && strings.Contains(it.Text, "附件发送失败") {
			note = it.Text
		}
	}
	if !strings.Contains(note, "remote.png") {
		t.Fatalf("note = %q, want the failed attachment named", note)
	}
	if strings.Contains(note, string(os.PathSeparator)) {
		t.Fatalf("note = %q, want a label rather than a path", note)
	}
}

func TestSendFailsWhenEveryAttachmentFails(t *testing.T) {
	fake := &fakeILink{uploadReply: `{"ret":-1,"errcode":404,"errmsg":"no slot"}`}
	c := newOutboundChannel(t, fake, &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, nil, nil), nil
	}})

	err := c.Send(context.Background(), bus.OutboundMessage{
		Channel: "weixin", ChatID: "user1",
		Media: []bus.MediaAttachment{{URL: writeFile(t, "only.png", pngBytes()), ContentType: "image/png"}},
	})
	if err == nil || !strings.Contains(err.Error(), "attachment(s) failed") {
		t.Fatalf("error = %v, want an attachment-only reply that failed to report failure", err)
	}
}

// A dead session has to park the channel on the media path too: an upload against a -14 token
// would otherwise keep firing until WeChat noticed the bot misbehaving.
func TestMediaSendParksExpiredSession(t *testing.T) {
	fake := &fakeILink{uploadReply: `{"ret":-14,"errmsg":"session expired"}`}
	c := newOutboundChannel(t, fake, &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, nil, nil), nil
	}})

	problems := c.sendOutboundMedia(context.Background(), "user1",
		[]bus.MediaAttachment{{URL: writeFile(t, "x.png", pngBytes()), ContentType: "image/png"}}, "CTX")
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want the upload refusal announced", problems)
	}
	if _, _, paused := c.st.paused(); !paused {
		t.Fatal("a -14 on getuploadurl did not park the session")
	}
	if len(fake.sentReqs) != 0 {
		t.Fatal("nothing should be sent after the slot was refused")
	}
}
