package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5" //nolint:gosec // the protocol asks for this checksum; nothing trusts it
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// Outbound rich media: the mirror image of the inbound CDN path.
//
// The sequence the protocol wants is: ask for a pre-signed upload slot, AES-ECB the payload
// under a key we generate and hand over up front, POST the ciphertext, and reference what came
// back in a sendmessage item. The download parameter arrives in a response header rather than
// the JSON body, and the size the service signs is the padded ciphertext, not the file — both
// are the parts a first attempt gets wrong.
//
// Voice is deliberately absent: the upload enum declares VOICE=4, but no implementation is known
// to send it, so a spoken reply travels as a file card. That is playable in the client and is an
// honest answer to "send me that as audio"; claiming a voice bar would be guessing at a shape
// nobody has observed.

const (
	// weixinMaxOutboundMediaBytes is a memory ceiling as much as a protocol one: the whole
	// plaintext is held in RAM to encrypt it.
	weixinMaxOutboundMediaBytes = 25 << 20

	// weixinUploadTimeout bounds one object (getuploadurl + POST). Send wraps the whole media
	// step in weixinMediaBudget, so the two must stay in order: an upload that could outlive
	// the step would be cut off mid-POST with nothing to show for it.
	weixinUploadTimeout  = 40 * time.Second
	weixinMediaSendSlack = 5 * time.Second
	weixinUploadTries    = 2
)

// uploadedMedia is one object that now exists on the CDN.
type uploadedMedia struct {
	downloadParam string
	keyHex        string
	fileSize      int64
	cipherSize    int64
}

// mediaRef builds the reference a message item carries. The key is base64 of the hex string the
// upload was told, which is the double encoding inbound media already uses.
func (u *uploadedMedia) ref() *cdnMedia {
	return &cdnMedia{
		EncryptQueryParam: u.downloadParam,
		AesKey:            base64.StdEncoding.EncodeToString([]byte(u.keyHex)),
		EncryptType:       1,
	}
}

// sendOutboundMedia uploads and sends each attachment, one sendmessage per file, and returns the
// labels of the ones that did not make it. Callers announce those instead of letting a reply
// claim an attachment that never arrived.
func (c *Channel) sendOutboundMedia(ctx context.Context, to string, files []bus.MediaAttachment, token string) []string {
	var problems []string
	for _, m := range files {
		if err := c.sendOneMedia(ctx, to, m, token); err != nil {
			slog.Warn("weixin: outbound media failed", "media", m.URL, "content_type", m.ContentType, "error", err)
			problems = append(problems, mediaLabel(m.URL))
		}
	}
	return problems
}

// mediaLabel names an attachment for the failure note without echoing a path back into the chat.
func mediaLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "(empty)"
	}
	if base := filepath.Base(strings.TrimRight(raw, `/\`)); base != "" && base != "." {
		return channels.Truncate(base, 60)
	}
	return channels.Truncate(raw, 60)
}

func (c *Channel) sendOneMedia(ctx context.Context, to string, m bus.MediaAttachment, token string) error {
	path := strings.TrimSpace(m.URL)
	if path == "" {
		return errors.New("attachment has no path")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		// Reaching this channel's CDN is one thing; fetching arbitrary remote bytes on the
		// way to a reply is a second network surface nobody asked for.
		return errors.New("remote attachment URLs are not supported by this channel")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat attachment: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("attachment %s is empty", filepath.Base(path))
	}
	if info.Size() > weixinMaxOutboundMediaBytes {
		return fmt.Errorf("attachment %s is %d bytes, over the %d byte cap",
			filepath.Base(path), info.Size(), weixinMaxOutboundMediaBytes)
	}

	plain, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read attachment: %w", err)
	}

	mediaType := c.outboundMediaType(m.ContentType, filepath.Base(path))
	up, err := c.uploadToCDN(ctx, to, plain, mediaType)
	if err != nil {
		return err
	}
	ref := up.ref()
	now := time.Now().UnixMilli()

	item := messageItem{ItemType: itemTypeFile, CreateTimeMs: now}
	switch mediaType {
	case uploadMediaImage:
		item = messageItem{ItemType: itemTypeImage, CreateTimeMs: now, ImageItem: &imageItem{
			Media: ref,
			// mid_size is the stored object's size, which is the ciphertext, not the file.
			MidSize: up.cipherSize,
		}}
	case uploadMediaVideo:
		item = messageItem{ItemType: itemTypeVideo, CreateTimeMs: now, VideoItem: &videoItem{
			Media: ref, VideoSize: up.cipherSize,
		}}
	default:
		sum := md5.Sum(plain) //nolint:gosec // protocol checksum of the plaintext, not a security use
		item.FileItem = &fileItem{
			Media:    ref,
			FileName: sanitizeMediaName(filepath.Base(path)),
			MD5:      hex.EncodeToString(sum[:]),
			Len:      strconv.FormatInt(up.fileSize, 10),
		}
	}

	return c.sendItem(ctx, to, item, token)
}

// uploadToCDN reserves an upload slot, posts the ciphertext, and returns what the message must
// reference plus the key it was encrypted under.
func (c *Channel) uploadToCDN(ctx context.Context, to string, plain []byte, mediaType int) (*uploadedMedia, error) {
	if c.cdnBase == nil {
		return nil, errors.New("weixin: CDN host is not configured")
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("weixin: generate media key: %w", err)
	}
	keyHex := hex.EncodeToString(key)
	fileKey := randomFileKey()
	sum := md5.Sum(plain) //nolint:gosec // protocol field

	uploadCtx, cancel := context.WithTimeout(ctx, weixinUploadTimeout)
	defer cancel()

	prepared, err := c.cli.getUploadURL(uploadCtx, getUploadURLReq{
		FileKey: fileKey, MediaType: mediaType, ToUserID: to,
		RawSize: len(plain), RawFileMD5: hex.EncodeToString(sum[:]),
		// The stored object is the padded ciphertext, and that is the size the service signs.
		FileSize: paddedLen(len(plain)), NoNeedThumb: true,
		AesKey: keyHex,
	})
	if err != nil {
		return nil, err
	}
	if prepared.failed() {
		if prepared.code() == errSessionExpired {
			c.st.pauseSession("iLink reported the session expired (-14); re-scan the QR code to re-activate", sessionPause)
		}
		return nil, fmt.Errorf("weixin: getuploadurl rejected (code=%d, errmsg=%s)", prepared.code(), prepared.Errmsg)
	}

	uploadURL, err := c.cdnUploadURL(prepared, fileKey)
	if err != nil {
		return nil, err
	}
	sealed, err := encryptAESMedia(key, plain)
	if err != nil {
		return nil, err
	}
	downloadParam, err := c.putCDN(uploadCtx, uploadURL, fileKey, sealed)
	if err != nil {
		return nil, err
	}

	return &uploadedMedia{
		downloadParam: downloadParam,
		keyHex:        keyHex,
		fileSize:      int64(len(plain)),
		cipherSize:    int64(len(sealed)),
	}, nil
}

// cdnUploadURL decides where the ciphertext is POSTed. An absolute URL from the service is only
// usable when it names the CDN host this instance pinned — the same rule the inbound path applies
// to peer-supplied URLs, here applied to a remote answer instead.
func (c *Channel) cdnUploadURL(resp *getUploadURLResp, fileKey string) (string, error) {
	if raw := strings.TrimSpace(resp.UploadFullURL); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("weixin: bad upload_full_url %q: %w", raw, err)
		}
		if u.Scheme != c.cdnBase.Scheme || !strings.EqualFold(u.Host, c.cdnBase.Host) {
			return "", fmt.Errorf("weixin: refusing upload_full_url off the pinned CDN host %s", c.cdnBase.Host)
		}
		return raw, nil
	}
	param := strings.TrimSpace(resp.UploadParam)
	if param == "" {
		return "", errors.New("weixin: getuploadurl returned neither an upload parameter nor a URL")
	}
	u := *c.cdnBase
	u.Path = strings.TrimRight(u.Path, "/") + "/upload"
	u.RawQuery = url.Values{
		"encrypted_query_param": []string{param},
		"filekey":               []string{fileKey},
	}.Encode()
	return u.String(), nil
}

// putCDN posts the ciphertext and reads the download parameter back out of a response header.
// The slot is reusable, so a server-side flap is retried against the same URL rather than
// re-signed; a 4xx is the operator's problem (bad token, dead session) and is reported as-is.
func (c *Channel) putCDN(ctx context.Context, uploadURL, fileKey string, sealed []byte) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= weixinUploadTries; attempt++ {
		param, retryable, err := c.postOnce(ctx, uploadURL, sealed)
		if err == nil {
			return param, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
		slog.Debug("weixin: CDN upload retrying", "filekey", fileKey, "attempt", attempt, "error", err)
	}
	return "", lastErr
}

func (c *Channel) postOnce(ctx context.Context, uploadURL string, sealed []byte) (string, bool, error) {
	if err := tools.CheckSSRF(uploadURL); err != nil {
		return "", false, fmt.Errorf("weixin: refused upload url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(sealed))
	if err != nil {
		return "", false, fmt.Errorf("weixin: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.uploadClient().Do(req)
	if err != nil {
		return "", true, fmt.Errorf("weixin: CDN upload: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests
		return "", retryable, fmt.Errorf("weixin: CDN upload http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	param := strings.TrimSpace(resp.Header.Get("X-Encrypted-Param"))
	if param == "" {
		return "", false, errors.New("weixin: CDN upload returned no download parameter")
	}
	return param, false, nil
}

// sendItem delivers one non-text item as its own reply. Like every send on this channel it must
// carry the peer's context token, which is why media can only follow a message, never start one.
func (c *Channel) sendItem(ctx context.Context, to string, item messageItem, token string) error {
	ctx, cancel := context.WithTimeout(ctx, weixinUploadTimeout+weixinMediaSendSlack)
	defer cancel()

	resp, err := c.cli.sendMessage(ctx, sendMessageReq{Msg: WeixinMessage{
		ToUserID: to, ClientID: newClientID(), MessageType: messageTypeBot,
		MessageState: messageStateFinish, ItemList: []messageItem{item}, ContextToken: token,
	}})
	if err != nil {
		return err
	}
	if resp.failed() {
		if resp.code() == errSessionExpired {
			c.st.pauseSession("iLink reported the session expired (-14); re-scan the QR code to re-activate", sessionPause)
		}
		return fmt.Errorf("weixin: media sendmessage rejected (code=%d, errmsg=%s)", resp.code(), resp.Errmsg)
	}
	return nil
}

// outboundMediaType maps to the upload enum. Anything not recognisably an image or an mp4 goes
// out as a file, which is the one category known to work for every format.
func (c *Channel) outboundMediaType(contentType, fileName string) int {
	m := strings.ToLower(strings.TrimSpace(contentType))
	if m == "" {
		m = mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	}
	switch {
	case strings.HasPrefix(m, "image/"):
		return uploadMediaImage
	case m == "video/mp4" || strings.EqualFold(filepath.Ext(fileName), ".mp4"):
		return uploadMediaVideo
	default:
		return uploadMediaFile
	}
}

// ttsVoiceForText strips the [[tts]] markup and, when a spoken reply was asked for, produces the
// audio as an attachment. This channel cannot build a voice bar, so the audio travels as a file —
// the text it returns is the stripped caption either way.
func (c *Channel) ttsVoiceForText(ctx context.Context, content, chatID string, voiceInbound bool) (string, *bus.MediaAttachment) {
	if c.audioMgr == nil || content == "" {
		return content, nil
	}
	res, err := c.audioMgr.AutoApplyToText(ctx, content, channels.TypeWeixin, voiceInbound, "")
	if err != nil {
		slog.Debug("weixin: tts auto-apply failed", "chat_id", chatID, "error", err)
	}
	if res == nil {
		return content, nil
	}
	if res.AudioPath == "" {
		return res.Text, nil
	}
	return res.Text, &bus.MediaAttachment{URL: res.AudioPath, ContentType: res.AudioMime}
}

// --- CDN upload crypto and sizing ---

// encryptAESMedia seals plaintext the way the CDN expects: AES-ECB with PKCS#7 padding. ECB is
// unauthenticated and leaks block-level patterns; that is the protocol's choice, and the inbound
// path reads exactly this shape back.
func encryptAESMedia(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	data := pkcs7Pad(plain, bs)
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += bs {
		block.Encrypt(out[off:off+bs], data[off:off+bs])
	}
	return out, nil
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	pad := blockSize - len(b)%blockSize
	out := make([]byte, len(b), len(b)+pad)
	copy(out, b)
	for i := 0; i < pad; i++ {
		out = append(out, byte(pad))
	}
	return out
}

// paddedLen is the size the upload service signs. PKCS#7 always appends at least one byte and a
// full block when the input already aligns, so this is never equal to n.
func paddedLen(n int) int {
	bs := aes.BlockSize
	return (n/bs + 1) * bs
}

func randomFileKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "goclaw-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
