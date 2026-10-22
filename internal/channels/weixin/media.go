package weixin

import (
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// Inbound rich media on iLink travels through an AES-encrypted CDN with a per-message key:
// the message carries an `encrypt_query_param` and an `aes_key`, and the bytes only become a
// viewable image, file, video or SILK voice note after ECB decryption. Voice is the special
// case — iLink usually also sends its own transcription text, which is free, so the CDN is only
// touched when that text is missing and GoClaw's own ASR chain takes over.
const (
	defaultCDNBase = "https://novac2c.cdn.weixin.qq.com/c2c"

	// weixinMaxMediaBytes is a per-object ceiling. WeChat's own limits are lower for voice
	// and around 100 MB for files; this sits nearer the useful end because the bytes are
	// handed to an agent, not archived, and a huge "file" is a memory bill with no upside.
	weixinMaxMediaBytes = 25 << 20

	// weixinMaxItems caps how many attachments of one message are fetched. A message with
	// five photos is answered with the first five, not the first one, because "I only see
	// one of them" is the more confusing failure.
	weixinMaxItems = 4

	weixinDownloadTimeout = 30 * time.Second
	weixinSTTTimeout      = 10 * time.Second
	weixinDownloadTries   = 2
	weixinRetryDelay      = 300 * time.Millisecond

	// weixinMediaBudget bounds the whole media step for one message. Long-polling is
	// sequential, so an unbounded fetch here delays every message behind it; the worst case
	// this allows is one slow CDN costing 45 seconds of receive latency, not four minutes.
	weixinMediaBudget = 45 * time.Second
)

// parseCDNBase resolves the CDN host one instance may talk to.
//
// https only, except loopback, which is what makes the download path testable without turning
// the rule into "whatever the config says". An unusable value is refused at construction rather
// than discovered at the first attachment.
func parseCDNBase(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultCDNBase
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("weixin: invalid cdn_base_url: %w", err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, fmt.Errorf("weixin: cdn_base_url must be https (or http on loopback), got %q", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("weixin: cdn_base_url has no host: %q", raw)
	}
	return u, nil
}

// isLoopbackHost mirrors the registry clients' rule so "local for testing" means the same thing
// in both places.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// inboundMedia is one attachment resolved enough to be fetched.
type inboundMedia struct {
	itemIndex int
	kind      string // image | video | file | voice
	target    string // pinned CDN URL
	key       []byte // nil when the message advertised no key
	fileName  string // sender-supplied name, files only
}

// selectInboundMedia turns an item list into fetchable attachments.
//
// Voice items that already carry iLink's own transcription text are deliberately not returned:
// renderText put that text in the message, and downloading a SILK blob to re-transcribe it
// would cost an API call to say the same thing.
func (c *Channel) selectInboundMedia(items []messageItem) []inboundMedia {
	refs := make([]inboundMedia, 0, len(items))

	for i, it := range items {
		var (
			kind  string
			media *cdnMedia
			key   []byte
			name  string
			skip  bool
		)
		switch {
		case it.ItemType == itemTypeImage && it.ImageItem != nil:
			kind, media = mediaKindImage, it.ImageItem.Media
			if it.ImageItem.Aeskey != "" {
				parsed, err := hexAESKey(it.ImageItem.Aeskey)
				if err != nil {
					slog.Warn("weixin: image key unusable", "error", err)
					skip = true
				} else {
					key = parsed
				}
			}
		case it.ItemType == itemTypeVideo && it.VideoItem != nil:
			kind, media = mediaKindVideo, it.VideoItem.Media
		case it.ItemType == itemTypeFile && it.FileItem != nil:
			kind, media, name = mediaKindFile, it.FileItem.Media, sanitizeMediaName(it.FileItem.FileName)
		case it.ItemType == itemTypeVoice && it.VoiceItem != nil:
			if it.VoiceItem.Text != "" {
				continue // already transcribed by the service
			}
			kind, media = mediaKindVoice, it.VoiceItem.Media
		default:
			continue
		}
		if skip {
			continue
		}
		if media == nil {
			continue
		}
		if key == nil && media.AesKey != "" {
			parsed, err := base64AESKey(media.AesKey)
			if err != nil {
				slog.Warn("weixin: media key unusable", "kind", kind, "error", err)
				continue
			}
			key = parsed
		}

		target, err := c.cdnTarget(media)
		if err != nil {
			slog.Warn("weixin: media target refused", "kind", kind, "error", err)
			continue
		}
		refs = append(refs, inboundMedia{itemIndex: i, kind: kind, target: target, key: key, fileName: name})
		if len(refs) >= weixinMaxItems {
			break
		}
	}
	return refs
}

// cdnTarget resolves one CDN object against the instance's pinned CDN host.
//
// full_url arrives inside a message about somebody else's upload, so it is honored only when it
// names exactly the host the operator configured; otherwise the object is rebuilt from its
// encrypted query parameter. That closes the "attachment URL points at 169.254.169.254" hole that
// a straight port of the reference client leaves open.
func (c *Channel) cdnTarget(m *cdnMedia) (string, error) {
	if c.cdnBase == nil {
		return "", errors.New("weixin: CDN host is not configured")
	}
	if m.FullURL != "" {
		u, err := url.Parse(m.FullURL)
		if err == nil && strings.EqualFold(u.Host, c.cdnBase.Host) && u.Scheme == c.cdnBase.Scheme {
			return m.FullURL, nil
		}
	}
	if m.EncryptQueryParam == "" {
		if m.FullURL != "" {
			return "", fmt.Errorf("full_url is off the pinned host %q and no encrypted query parameter was given", c.cdnBase.Host)
		}
		return "", errors.New("weixin: CDN object carries neither an encrypted query parameter nor a URL")
	}
	u := *c.cdnBase
	u.Path = strings.TrimRight(u.Path, "/") + "/download"
	u.RawQuery = url.Values{"encrypted_query_param": []string{m.EncryptQueryParam}}.Encode()
	return u.String(), nil
}

// processInboundMedia fetches, decrypts and stores the attachments of one message.
//
// Items that cannot be obtained keep their placeholder in the text: an agent told nothing
// answers "you sent me nothing", which is worse than an honest [图片].
func (c *Channel) processInboundMedia(ctx context.Context, refs []inboundMedia) (files []bus.MediaFile, tags string, handled map[int]bool) {
	handled = map[int]bool{}
	if len(refs) == 0 {
		return nil, "", handled
	}

	infos := make([]media.MediaInfo, 0, len(refs))
	for _, r := range refs {
		info, err := c.fetchMedia(ctx, r)
		if err != nil {
			slog.Warn("weixin: inbound media unavailable", "kind", r.kind, "error", err)
			continue
		}
		if info.Type == media.TypeVoice || info.Type == media.TypeAudio {
			c.transcribe(ctx, info)
		}
		handled[r.itemIndex] = true
		infos = append(infos, *info)
		files = append(files, bus.MediaFile{
			Path:     info.FilePath,
			MimeType: info.ContentType,
			Filename: info.FileName,
		})
	}

	return files, media.BuildMediaTags(infos), handled
}

// fetchMedia downloads one CDN object, decrypts it if a key was advertised, and writes it to a
// temp file for the pipeline to persist.
func (c *Channel) fetchMedia(ctx context.Context, r inboundMedia) (*media.MediaInfo, error) {
	enc, err := c.downloadCDN(ctx, r.target)
	if err != nil {
		return nil, err
	}

	data := enc
	if r.key != nil {
		data, err = decryptAESMedia(r.key, enc)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", r.kind, err)
		}
	}

	name, mime := nameForMedia(r, data)
	if r.key == nil && !plausibleMedia(mime, r.kind) {
		// No key was advertised. Unencrypted media is real, but "we got bytes that are not
		// any media type and the item claims to be an image" is a broken download, and
		// storing it as image.png would give the agent a file that cannot be opened.
		return nil, fmt.Errorf("%s has no key and its bytes look like %q", r.kind, mime)
	}

	f, err := os.CreateTemp("", "goclaw_weixin_*"+path.Ext(name))
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("write: %w", err)
	}

	return &media.MediaInfo{
		Type:        mediaKindToType(r.kind),
		FilePath:    f.Name(),
		SourceURL:   r.target,
		ContentType: mime,
		FileName:    name,
		FileSize:    int64(len(data)),
	}, nil
}

func (c *Channel) downloadCDN(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < weixinDownloadTries; attempt++ {
		if attempt > 0 {
			if sleepCtx(ctx, weixinRetryDelay) {
				return nil, ctx.Err()
			}
		}
		data, retryable, err := c.fetchOnce(ctx, rawURL)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
		slog.Debug("weixin: CDN fetch retrying", "attempt", attempt+1, "error", err)
	}
	return nil, lastErr
}

func (c *Channel) fetchOnce(ctx context.Context, rawURL string) ([]byte, bool, error) {
	if err := tools.CheckSSRF(rawURL); err != nil {
		return nil, false, fmt.Errorf("refused CDN url: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, weixinDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.mediaClient().Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		retryable := resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests
		return nil, retryable, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, weixinMaxMediaBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("read: %w", err)
	}
	if len(data) == 0 {
		return nil, false, errors.New("empty CDN body")
	}
	if len(data) > weixinMaxMediaBytes {
		return nil, false, fmt.Errorf("media exceeds the %d byte cap", weixinMaxMediaBytes)
	}
	return data, false, nil
}

// mediaClient is created lazily so an instance that never receives an attachment holds no
// connection pool. Redirects stay on the pinned CDN host.
func (c *Channel) mediaClient() *http.Client {
	if c.mediaHTTP != nil {
		return c.mediaHTTP
	}
	c.mediaHTTP = c.newCDNClient(weixinDownloadTimeout)
	return c.mediaHTTP
}

// newCDNClient builds a CDN client with a deadline of its own. Uploads need a longer one than
// downloads: the whole file is POSTed, and a 30-second client timeout would kill a large upload
// that the request context was happy to wait for.
func (c *Channel) newCDNClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: c.transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if c.cdnBase != nil && !strings.EqualFold(req.URL.Host, c.cdnBase.Host) {
				return fmt.Errorf("weixin: refusing redirect off CDN host %s to %s", c.cdnBase.Host, req.URL.Host)
			}
			return tools.CheckSSRF(req.URL.String())
		},
	}
}

// uploadClient is the outbound twin of mediaClient, with the upload timeout.
func (c *Channel) uploadClient() *http.Client {
	if c.uploadHTTP != nil {
		return c.uploadHTTP
	}
	c.uploadHTTP = c.newCDNClient(weixinUploadTimeout)
	return c.uploadHTTP
}

// transcribe runs GoClaw's own ASR chain over a downloaded voice note. This is the fallback
// path: it only happens when iLink did not supply voice_item.text.
func (c *Channel) transcribe(ctx context.Context, info *media.MediaInfo) {
	if c.audioMgr == nil || !c.audioMgr.STTAvailable(ctx) {
		slog.Debug("weixin: no STT backend available, leaving voice untranscribed")
		return
	}

	sttCtx, cancel := context.WithTimeout(ctx, c.audioMgr.STTTimeout(weixinSTTTimeout))
	defer cancel()
	sttCtx = audio.WithChannel(sttCtx, channels.TypeWeixin)

	res, err := c.audioMgr.Transcribe(sttCtx, audio.STTInput{
		FilePath: info.FilePath,
		MimeType: media.AudioSTTMime(info.ContentType, "audio/silk"),
		Filename: info.FileName,
	}, audio.STTOptions{})
	if err != nil {
		slog.Warn("weixin: transcription failed", "file", info.FileName, "error", err)
		return
	}
	if res != nil && res.Text != "" {
		info.Transcript = res.Text
	}
}

// --- CDN media crypto ---

// decryptAESMedia decrypts a CDN object.
//
// ECB is what the protocol uses, so it is what this implements: it is unauthenticated and leaks
// plaintext patterns, which is a property of WeChat's media format, not a choice made here.
func decryptAESMedia(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(data) == 0 || len(data)%bs != 0 {
		return nil, fmt.Errorf("ciphertext is %d bytes, not a multiple of the %d-byte block", len(data), bs)
	}
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += bs {
		block.Decrypt(out[off:off+bs], data[off:off+bs])
	}
	return pkcs7Unpad(out, bs)
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, fmt.Errorf("data length %d is not block aligned", len(b))
	}
	pad := int(b[len(b)-1])
	if pad <= 0 || pad > blockSize || pad > len(b) {
		return nil, fmt.Errorf("invalid PKCS#7 padding length %d", pad)
	}
	for _, x := range b[len(b)-pad:] {
		if int(x) != pad {
			return nil, errors.New("invalid PKCS#7 padding bytes")
		}
	}
	return b[:len(b)-pad], nil
}

// base64AESKey reads the generic media key. Two shapes appear: 16 raw bytes, and a base64
// string of 32 ASCII hex characters, which is really a 16-byte key encoded twice.
func base64AESKey(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("aes_key is not base64: %w", err)
	}
	switch len(decoded) {
	case 16:
		return decoded, nil
	case 32:
		if inner, err := hex.DecodeString(string(decoded)); err == nil && len(inner) == 16 {
			return inner, nil
		}
		return nil, errors.New("aes_key is 32 bytes but not a hex-encoded 16-byte key")
	default:
		return nil, fmt.Errorf("aes_key decodes to %d bytes, want 16 or 32 hex characters", len(decoded))
	}
}

// hexAESKey reads image_item.aeskey, which is hex rather than base64.
func hexAESKey(raw string) ([]byte, error) {
	decoded, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("aeskey is not hex: %w", err)
	}
	if len(decoded) != 16 {
		return nil, fmt.Errorf("aeskey decodes to %d bytes, want 16", len(decoded))
	}
	return decoded, nil
}

// --- naming and type detection ---

const (
	mediaKindImage = "image"
	mediaKindVideo = "video"
	mediaKindFile  = "file"
	mediaKindVoice = "voice"
)

// nameForMedia chooses the stored filename and MIME type. The CDN hands back bytes and no
// content type worth trusting, so the magic prefix decides; voice is forced to SILK because
// `#!SILK_V3` and WeChat's \x01 framing both sniff as text or octet-stream.
func nameForMedia(r inboundMedia, data []byte) (string, string) {
	if r.kind == mediaKindVoice {
		if ext, mime := sniffMagic(data); mime != "" {
			// Known audio is kept; a known non-audio format is reported as what it is, so a
			// "voice" item carrying a PNG is refused instead of stored as voice.silk and handed
			// to a transcription engine.
			return "voice" + ext, mime
		}
		if mime := sniffAudio(data); mime != "" {
			return "voice" + extForMime(mime), mime
		}
		// Unrecognised bytes are still SILK: that is what this item type carries, and both
		// `#!SILK_V3` and WeChat's framing sniff as text or octet-stream.
		return "voice.silk", "audio/silk"
	}
	if ext, mime := sniffMagic(data); mime != "" {
		base := sanitizeMediaName(r.fileName)
		if base == "" {
			base = r.kind + ext
		}
		return base, mime
	}
	if name := sanitizeMediaName(r.fileName); name != "" {
		return name, media.DetectMIMEType(name)
	}
	return r.kind + ".bin", "application/octet-stream"
}

func mediaKindToType(kind string) string {
	switch kind {
	case mediaKindImage:
		return media.TypeImage
	case mediaKindVideo:
		return media.TypeVideo
	case mediaKindVoice:
		return media.TypeVoice
	default:
		return media.TypeDocument
	}
}

func plausibleMedia(mimeType, kind string) bool {
	switch kind {
	case mediaKindImage:
		return strings.HasPrefix(mimeType, "image/")
	case mediaKindVideo:
		return strings.HasPrefix(mimeType, "video/")
	case mediaKindVoice:
		return strings.HasPrefix(mimeType, "audio/")
	default:
		// A document is any bytes: there is no shape it must have, so nothing to reject.
		return true
	}
}

// sniffMagic recognises the formats WeChat media actually arrives in. http.DetectContentType
// is used as the fallback, but it maps a SILK payload to text/plain and a WEBP to RIFF, which
// would store an unopenable "image".
func sniffMagic(data []byte) (ext, mimeType string) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "#!SILK_V":
		return ".silk", "audio/silk"
	case len(data) >= 4 && data[0] == 0x01 && data[1] == 0xfe && data[2] == 0x06:
		return ".silk", "audio/silk"
	case len(data) >= 6 && (string(data[:3]) == "AMR" || string(data[:9]) == "#!AMR-WB"):
		return ".amr", "audio/amr"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return ".png", "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return ".jpg", "image/jpeg"
	case len(data) >= 6 && string(data[:6]) == "GIF87a" || len(data) >= 6 && string(data[:6]) == "GIF89a":
		return ".gif", "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return ".webp", "image/webp"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return ".mp4", "video/mp4"
	case len(data) >= 4 && string(data[:4]) == "%PDF":
		return ".pdf", "application/pdf"
	case len(data) >= 4 && string(data[:4]) == "Rar!":
		return ".rar", "application/x-rar-compressed"
	case len(data) >= 4 && string(data[:4]) == "PK\x03\x04":
		return ".zip", "application/zip"
	case len(data) >= 2 && string(data[:2]) == "MZ":
		return ".exe", "application/x-msdownload"
	}
	return "", ""
}

func sniffAudio(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "audio/") {
		return ct
	}
	return ""
}

func extForMime(mimeType string) string {
	switch mimeType {
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/mp4", "audio/m4a":
		return ".m4a"
	case "audio/amr":
		return ".amr"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	default:
		return ".bin"
	}
}

// sanitizeMediaName keeps a sender-supplied filename usable as a local name. Names arrive from
// the peer, so anything path-shaped is reduced to its last element rather than cleaned.
func sanitizeMediaName(name string) string {
	name = strings.TrimSpace(filepath.Base(filepath.FromSlash(name)))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return '_'
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		default:
			return r
		}
	}, name)
	if len(name) > 120 {
		ext := filepath.Ext(name)
		name = name[:120-len(ext)] + ext
	}
	return name
}
