package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
)

// cdnTransport answers CDN requests without leaving the process. The URLs still have to be
// public-looking because tools.CheckSSRF runs for real on them — that check is part of what is
// under test, so it is not bypassed.
type cdnTransport struct {
	requests   []string
	handler    func(req *http.Request) (*http.Response, error)
	roundTrips int
}

func (s *cdnTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, req.URL.String())
	s.roundTrips++
	return s.handler(req)
}

func cdnResponse(status int, body []byte, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// newMediaChannel builds a channel whose CDN is a fixed public host served by stub.
func newMediaChannel(t *testing.T, stub *cdnTransport) *Channel {
	t.Helper()
	c, err := New(Credentials{Token: "tok"}, InstanceConfig{
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
	return c
}

// ecbEncrypt mirrors what the CDN does to us: AES-ECB with PKCS#7.
func ecbEncrypt(key, plain []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	data := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += bs {
		block.Encrypt(out[off:off+bs], data[off:off+bs])
	}
	return out
}

var testAESKey = []byte("0123456789abcdef")

func pngBytes() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("i-dont-change"), 8)...)
}

func TestParseCDNBaseRules(t *testing.T) {
	tests := []struct {
		raw       string
		wantHost  string
		wantError bool
	}{
		{"", "novac2c.cdn.weixin.qq.com", false},
		{"https://mirror.example/c2c", "mirror.example", false},
		{"http://127.0.0.1:8080/c2c", "127.0.0.1:8080", false},
		{"http://mirror.example/c2c", "", true},
		{"ftp://mirror.example", "", true},
		{"https://", "", true},
	}
	for _, tt := range tests {
		u, err := parseCDNBase(tt.raw)
		if tt.wantError {
			if err == nil {
				t.Fatalf("parseCDNBase(%q) accepted %v, want refusal", tt.raw, u)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseCDNBase(%q) error = %v", tt.raw, err)
		}
		if u.Host != tt.wantHost {
			t.Fatalf("parseCDNBase(%q) host = %q, want %q", tt.raw, u.Host, tt.wantHost)
		}
	}
}

func TestCDNTargetPinsTheConfiguredHost(t *testing.T) {
	c := newMediaChannel(t, nil)

	same, err := c.cdnTarget(&cdnMedia{
		EncryptQueryParam: "abc",
		FullURL:           "https://8.8.8.8/c2c/download?encrypted_query_param=abc",
	})
	if err != nil {
		t.Fatalf("cdnTarget() error = %v", err)
	}
	if same != "https://8.8.8.8/c2c/download?encrypted_query_param=abc" {
		t.Fatalf("on-host full_url = %q, want it honored verbatim", same)
	}

	// A peer-supplied URL that names another host is rebuilt from the parameter instead.
	rebuilt, err := c.cdnTarget(&cdnMedia{
		EncryptQueryParam: "tok/en+d",
		FullURL:           "https://169.254.169.254/latest/meta-data",
	})
	if err != nil {
		t.Fatalf("cdnTarget() error = %v", err)
	}
	if !strings.HasPrefix(rebuilt, "https://8.8.8.8/c2c/download?encrypted_query_param=") {
		t.Fatalf("rebuilt = %q, want the pinned host", rebuilt)
	}
	if !strings.Contains(rebuilt, "tok%2Fen%2Bd") {
		t.Fatalf("parameter not escaped: %q", rebuilt)
	}

	if _, err := c.cdnTarget(&cdnMedia{FullURL: "https://evil.example/x"}); err == nil {
		t.Fatal("an off-host URL with no encrypted parameter must be refused")
	}
	if _, err := c.cdnTarget(&cdnMedia{}); err == nil {
		t.Fatal("an empty CDN object must be refused")
	}
}

func TestMediaKeyParsing(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(testAESKey)
	key, err := base64AESKey(b64)
	if err != nil || !bytes.Equal(key, testAESKey) {
		t.Fatalf("base64AESKey() = %v, %v", key, err)
	}

	// The doubled shape: 32 ASCII hex characters, base64-wrapped.
	inner := hex.EncodeToString(testAESKey)
	key, err = base64AESKey(base64.StdEncoding.EncodeToString([]byte(inner)))
	if err != nil || !bytes.Equal(key, testAESKey) {
		t.Fatalf("hex-in-base64 key = %v, %v", key, err)
	}

	if _, err := base64AESKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("a 5-byte key was accepted")
	}
	if _, err := base64AESKey("!!!not base64!!!"); err == nil {
		t.Fatal("non-base64 accepted")
	}
	if _, err := base64AESKey(base64.StdEncoding.EncodeToString([]byte("zz"))); err == nil {
		t.Fatal("32-byte payload that is not hex-16 accepted")
	}

	if k, err := hexAESKey(hex.EncodeToString(testAESKey)); err != nil || !bytes.Equal(k, testAESKey) {
		t.Fatalf("hexAESKey() = %v, %v", k, err)
	}
	if _, err := hexAESKey("abcd"); err == nil {
		t.Fatal("a 2-byte hex key accepted")
	}
}

func TestDecryptAESMedia(t *testing.T) {
	plain := []byte("the quick brown fox")
	enc := ecbEncrypt(testAESKey, plain)
	got, err := decryptAESMedia(testAESKey, enc)
	if err != nil {
		t.Fatalf("decryptAESMedia() error = %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip = %q, want %q", got, plain)
	}

	if _, err := decryptAESMedia(testAESKey, []byte("not block aligned")); err == nil {
		t.Fatal("a non-block-aligned ciphertext decrypted")
	}
	// A trailing byte claiming 9 bytes of padding where the others disagree is not padding.
	bad := ecbEncrypt(testAESKey, []byte("0123456789abcdef"))
	bad[len(bad)-1] = 9
	if _, err := decryptAESMedia(testAESKey, bad); err == nil {
		t.Fatal("invalid PKCS#7 padding accepted")
	}
}

func TestSelectInboundMedia(t *testing.T) {
	items := []messageItem{
		{ItemType: itemTypeText, TextItem: &textItem{Text: "look"}},
		{ItemType: itemTypeImage, ImageItem: &imageItem{
			Media:  &cdnMedia{EncryptQueryParam: "p1"},
			Aeskey: hex.EncodeToString(testAESKey),
		}},
		{ItemType: itemTypeVoice, VoiceItem: &voiceItem{Text: "server transcript"}}}

	c := newMediaChannel(t, nil)
	refs := c.selectInboundMedia(items)
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want only the image (server-transcribed voice is not fetched)", refs)
	}
	if refs[0].itemIndex != 1 || refs[0].kind != mediaKindImage {
		t.Fatalf("ref = %+v, want index 1 / image", refs[0])
	}
	if !bytes.Equal(refs[0].key, testAESKey) {
		t.Fatal("image key not parsed")
	}
	if !strings.HasPrefix(refs[0].target, "https://8.8.8.8/c2c/download?") {
		t.Fatalf("target = %q", refs[0].target)
	}

	// Voice without the service transcript is the case that needs the CDN and our own ASR.
	voiceOnly := []messageItem{{ItemType: itemTypeVoice, VoiceItem: &voiceItem{
		Media: &cdnMedia{EncryptQueryParam: "v1", AesKey: base64.StdEncoding.EncodeToString(testAESKey)},
	}}}
	refs = c.selectInboundMedia(voiceOnly)
	if len(refs) != 1 || refs[0].kind != mediaKindVoice || !bytes.Equal(refs[0].key, testAESKey) {
		t.Fatalf("voice refs = %+v", refs)
	}

	// A broken key leaves the item alone: the placeholder stays rather than a bogus file.
	badKey := []messageItem{{ItemType: itemTypeImage, ImageItem: &imageItem{
		Media: &cdnMedia{EncryptQueryParam: "p1"}, Aeskey: "xyz"}}}
	if refs := c.selectInboundMedia(badKey); len(refs) != 0 {
		t.Fatalf("bad key produced refs: %+v", refs)
	}

	// Many attachments are capped, not unbounded.
	var many []messageItem
	for i := 0; i < 10; i++ {
		many = append(many, messageItem{ItemType: itemTypeImage, ImageItem: &imageItem{
			Media: &cdnMedia{EncryptQueryParam: "p"}}})
	}
	if refs := c.selectInboundMedia(many); len(refs) != weixinMaxItems {
		t.Fatalf("refs = %d, want the %d-item cap", len(refs), weixinMaxItems)
	}
}

func TestProcessInboundMediaDecryptsAndTags(t *testing.T) {
	plain := pngBytes()
	blob := ecbEncrypt(testAESKey, plain)
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, blob, nil), nil
	}}
	c := newMediaChannel(t, stub)

	refs := c.selectInboundMedia([]messageItem{
		{ItemType: itemTypeText, TextItem: &textItem{Text: "看图"}},
		{ItemType: itemTypeImage, ImageItem: &imageItem{
			Media: &cdnMedia{EncryptQueryParam: "p1"}, Aeskey: hex.EncodeToString(testAESKey)}}})

	files, tags, handled := c.processInboundMedia(context.Background(), refs)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	defer os.Remove(files[0].Path)

	// BuildMediaTags renders `<media:image url="…">`, so match the open tag.
	if !strings.HasPrefix(tags, "<media:image") {
		t.Fatalf("tags = %q, want the media tag", tags)
	}
	if !handled[1] || handled[0] {
		t.Fatalf("handled = %v, want only the image item", handled)
	}
	if files[0].MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png decrypted from the magic bytes", files[0].MimeType)
	}
	body, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if !bytes.Equal(body, plain) {
		t.Fatal("stored bytes are not the decrypted plaintext")
	}
	if stub.roundTrips != 1 {
		t.Fatalf("round trips = %d, want one", stub.roundTrips)
	}
}

func TestProcessInboundMediaKeepsPlaceholderOnFailure(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*http.Request) (*http.Response, error)
	}{
		{"404 is not retried and not stored", func(*http.Request) (*http.Response, error) {
			return cdnResponse(http.StatusNotFound, []byte("gone"), nil), nil
		}},
		{"over the size cap", func(*http.Request) (*http.Response, error) {
			return cdnResponse(http.StatusOK, bytes.Repeat([]byte("x"), weixinMaxMediaBytes+1), nil), nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &cdnTransport{handler: tt.handler}
			c := newMediaChannel(t, stub)
			refs := c.selectInboundMedia([]messageItem{{ItemType: itemTypeImage, ImageItem: &imageItem{
				Media: &cdnMedia{EncryptQueryParam: "p"}, Aeskey: hex.EncodeToString(testAESKey)}}})
			files, tags, handled := c.processInboundMedia(context.Background(), refs)
			if len(files) != 0 || tags != "" || len(handled) != 0 {
				t.Fatalf("files=%+v tags=%q handled=%v, want nothing claimed", files, tags, handled)
			}
		})
	}
}

func TestDownloadRetriesServerErrorsOnce(t *testing.T) {
	var calls int
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return cdnResponse(http.StatusBadGateway, []byte("busy"), nil), nil
		}
		return cdnResponse(http.StatusOK, ecbEncrypt(testAESKey, pngBytes()), nil), nil
	}}
	c := newMediaChannel(t, stub)

	refs := c.selectInboundMedia([]messageItem{{ItemType: itemTypeImage, ImageItem: &imageItem{
		Media: &cdnMedia{EncryptQueryParam: "p"}, Aeskey: hex.EncodeToString(testAESKey)}}})
	files, _, _ := c.processInboundMedia(context.Background(), refs)
	if len(files) != 1 {
		t.Fatalf("files = %+v, want the retry to succeed", files)
	}
	defer os.Remove(files[0].Path)
	if calls != 2 {
		t.Fatalf("requests = %d, want one retry", calls)
	}
}

func TestMediaClientRefusesRedirectOffThePinnedHost(t *testing.T) {
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		resp := cdnResponse(http.StatusFound, nil, http.Header{"Location": []string{"https://mirror.internal/x"}})
		return resp, nil
	}}
	c := newMediaChannel(t, stub)

	_, err := c.downloadCDN(context.Background(), "https://8.8.8.8/c2c/download?encrypted_query_param=p")
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("error = %v, want the host pin to refuse the redirect", err)
	}
}

func TestFetchMediaWithoutKeyAcceptsRealBytesOnly(t *testing.T) {
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, pngBytes(), nil), nil
	}}
	c := newMediaChannel(t, stub)

	info, err := c.fetchMedia(context.Background(), inboundMedia{kind: mediaKindImage, target: "https://8.8.8.8/c2c/download?encrypted_query_param=p"})
	if err != nil {
		t.Fatalf("unencrypted image refused: %v", err)
	}
	defer os.Remove(info.FilePath)
	if info.ContentType != "image/png" || info.Type != media.TypeImage {
		t.Fatalf("info = %+v", info)
	}

	// The same bytes claimed as voice would be stored as an unopenable "voice.silk".
	if _, err := c.fetchMedia(context.Background(), inboundMedia{kind: mediaKindVoice, target: "https://8.8.8.8/c2c/download?x=1"}); err == nil {
		t.Fatal("a PNG stored as a voice note without a key should be refused")
	}
}

func TestNameForMediaVoiceIsSILK(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		wantName string
		wantMime string
	}{
		{"silk magic", append([]byte("#!SILK_V3\n"), bytes.Repeat([]byte{0x01}, 20)...), "voice.silk", "audio/silk"},
		{"wechat framed", append([]byte{0x01, 0xfe, 0x06}, bytes.Repeat([]byte{0x02}, 20)...), "voice.silk", "audio/silk"},
		{"unrecognized falls back to silk", []byte("nonsense bytes here"), "voice.silk", "audio/silk"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			n, m := nameForMedia(inboundMedia{kind: mediaKindVoice}, tt.data)
			if n != tt.wantName || m != tt.wantMime {
				t.Fatalf("nameForMedia() = %q/%q, want %q/%q", n, m, tt.wantName, tt.wantMime)
			}
		})
	}

	if n, m := nameForMedia(inboundMedia{kind: mediaKindFile, fileName: "报告.pdf"}, []byte("%PDF-1.4 ...")); n != "报告.pdf" || m != "application/pdf" {
		t.Fatalf("file naming = %q/%q", n, m)
	}
	// A path in the sender-supplied name must not survive into a local filename.
	if n, _ := nameForMedia(inboundMedia{kind: mediaKindFile, fileName: "../../../etc/passwd"}, []byte("data")); n != "passwd" {
		t.Fatalf("sanitized name = %q, want the base element only", n)
	}
}

func TestSanitizeMediaName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"..", ""},
		{"a/b/c.txt", "c.txt"},
		{"we:ird*name?.pdf", "we_ird_name_.pdf"},
		{"\x00tab.txt", "_tab.txt"},
	}
	for _, tt := range tests {
		if got := sanitizeMediaName(tt.in); got != tt.want {
			t.Errorf("sanitizeMediaName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	long := strings.Repeat("a", 200) + ".txt"
	if got := sanitizeMediaName(long); len(got) != 120 || filepath.Ext(got) != ".txt" {
		t.Fatalf("long name = %q (len %d), want 120 chars keeping the extension", got, len(got))
	}
}

// TestHandleInboundAttachesMediaWithoutPlaceholder is the end-to-end shape of the feature: a
// text+image message reaches the pipeline with the file attached, the media tag, and no stale
// "[图片]" claiming the image was unavailable.
func TestHandleInboundAttachesMediaWithoutPlaceholder(t *testing.T) {
	blob := ecbEncrypt(testAESKey, pngBytes())
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusOK, blob, nil), nil
	}}

	msgBus := bus.New()
	c, err := New(Credentials{Token: "tok"}, InstanceConfig{
		DMPolicy:   "open",
		StateDir:   t.TempDir(),
		CDNBaseURL: "https://8.8.8.8/c2c",
	}, msgBus, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.transport = stub
	c.SetName("weixin")

	done := make(chan bus.InboundMessage, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if msg, ok := msgBus.ConsumeInbound(ctx); ok {
			done <- msg
		} else {
			close(done)
		}
	}()

	c.handleInbound(WeixinMessage{
		FromUserID:   "peer-1",
		ContextToken: "ctx-1",
		ItemList: []messageItem{
			{ItemType: itemTypeText, TextItem: &textItem{Text: "这是什么"}},
			{ItemType: itemTypeImage, ImageItem: &imageItem{
				Media: &cdnMedia{EncryptQueryParam: "p1"}, Aeskey: hex.EncodeToString(testAESKey)}}},
	})

	msg, ok := <-done
	if !ok {
		t.Fatal("no inbound message published")
	}
	defer func() {
		for _, f := range msg.Media {
			os.Remove(f.Path)
		}
	}()

	if strings.Contains(msg.Content, "[图片]") {
		t.Fatalf("content still carries the placeholder for a fetched image: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "<media:image") || !strings.Contains(msg.Content, "这是什么") {
		t.Fatalf("content = %q, want the text plus the media tag for the fetched image", msg.Content)
	}
	if len(msg.Media) != 1 || msg.Media[0].MimeType != "image/png" {
		t.Fatalf("media = %+v", msg.Media)
	}
}

func TestHandleInboundKeepsPlaceholderWhenCDNFails(t *testing.T) {
	stub := &cdnTransport{handler: func(*http.Request) (*http.Response, error) {
		return cdnResponse(http.StatusForbidden, []byte("expired"), nil), nil
	}}
	msgBus := bus.New()
	c, err := New(Credentials{Token: "tok"}, InstanceConfig{
		DMPolicy: "open", StateDir: t.TempDir(), CDNBaseURL: "https://8.8.8.8/c2c",
	}, msgBus, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	c.transport = stub

	done := make(chan bus.InboundMessage, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if msg, ok := msgBus.ConsumeInbound(ctx); ok {
			done <- msg
		}
	}()

	c.handleInbound(WeixinMessage{FromUserID: "peer-1", ItemList: []messageItem{
		{ItemType: itemTypeImage, ImageItem: &imageItem{Media: &cdnMedia{EncryptQueryParam: "p"}}}}})

	msg := <-done
	if !strings.Contains(msg.Content, "[图片]") {
		t.Fatalf("content = %q, want the honest placeholder for a failed fetch", msg.Content)
	}
	if len(msg.Media) != 0 {
		t.Fatalf("media = %+v, want none", msg.Media)
	}
}
