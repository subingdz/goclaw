package qq

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
)

// Outbound rich media.
//
// What kept this channel text-only in the first version was a sentence in the docs: media had
// to be uploaded from a URL QQ's servers could fetch, which a gateway running on a LAN does not
// have. The current endpoint also takes the bytes themselves (`file_data`, base64), so that
// premise was documentation, not a platform limit, and it is corrected here.
//
// Limits per the media overview: image 20 MB / video 30 MB / voice 20 MB soft and 200 MB hard,
// with over-soft uploads downgraded to a file card by QQ itself. The ceiling below is ours: a
// base64 body means the whole file is held in memory twice, so "the platform tolerates 200 MB"
// is not a reason to spend 270 MB of RAM on somebody's cron output.
const (
	qqMaxOutboundMediaBytes = 20 << 20
	qqMediaUploadTimeout    = 60 * time.Second
	qqMediaSendTimeout      = 30 * time.Second
)

// file_type values for the upload endpoints.
const (
	fileTypeImage = 1
	fileTypeVideo = 2
	fileTypeVoice = 3
	fileTypeFile  = 4
)

// localMedia keeps only the attachments this path can carry: files on disk. MediaAttachment is
// deliberately loose ("file path or URL"), but a remote URL would have to be fetched here, which
// is a different feature with its own SSRF surface — so those are reported as undeliverable by
// the send below rather than dropped without a word.
func localMedia(files []bus.MediaAttachment) []bus.MediaAttachment {
	out := make([]bus.MediaAttachment, 0, len(files))
	for _, m := range files {
		if strings.TrimSpace(m.URL) == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// qqFileTypeFor maps a media type to the file_type QQ expects. Anything outside the formats the
// docs list for a rendered card is sent as a file, which is what QQ does with unsupported
// formats anyway — and it beats an 850019 rejection of the whole send.
func qqFileTypeFor(contentType, fileName string) int {
	mime := strings.ToLower(strings.TrimSpace(contentType))
	if mime == "" {
		mime = mimeByExt[strings.ToLower(filepath.Ext(fileName))]
	}
	switch {
	case strings.HasPrefix(mime, "image/"):
		return fileTypeImage
	case strings.HasPrefix(mime, "video/"):
		if strings.EqualFold(filepath.Ext(fileName), ".mp4") || mime == "video/mp4" {
			return fileTypeVideo
		}
		return fileTypeFile
	case strings.HasPrefix(mime, "audio/"):
		// The voice card renders for silk, mp3, wav and ogg, which is what makes a TTS reply
		// a voice message here rather than an attachment the user has to open.
		return fileTypeVoice
	default:
		return fileTypeFile
	}
}

// mimeByExt covers the extensions GoClaw's own media producers emit, for the callers that hand
// over a path with no content type at all.
var mimeByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp",
	".mp4": "video/mp4",
	".ogg": "audio/ogg", ".opus": "audio/ogg", ".mp3": "audio/mpeg",
	".wav": "audio/wav", ".silk": "audio/silk", ".m4a": "audio/mp4",
	".pdf": "application/pdf",
}

// uploadMedia sends one file's bytes to the conversation-scoped upload endpoint and returns the
// file_info a msg_type=7 message has to reference.
//
// Uploads are not interchangeable between C2C and group — the docs say so explicitly — so the
// kind decides the endpoint and nothing is reused across kinds.
func (c *Channel) uploadMedia(ctx context.Context, kind, target, fileName string, data []byte, fileType int) (string, error) {
	switch {
	case len(data) == 0:
		return "", fmt.Errorf("qq: refusing to upload empty media %s", fileName)
	case len(data) > qqMaxOutboundMediaBytes:
		return "", fmt.Errorf("qq: media %s is %d bytes, over this channel's %d byte send cap",
			fileName, len(data), qqMaxOutboundMediaBytes)
	}

	var path string
	switch kind {
	case "c2c":
		path = "/v2/users/" + url.PathEscape(target) + "/files"
	case "group":
		path = "/v2/groups/" + url.PathEscape(target) + "/files"
	default:
		return "", fmt.Errorf("qq: unknown conversation kind %q", kind)
	}

	req := mediaUploadRequest{
		FileType: fileType,
		FileData: base64.StdEncoding.EncodeToString(data),
		FileName: fileName,
		// false = hand back a file_info for us to send; true would deliver it as an active
		// message, spending proactive quota and giving us no id to reason about.
		SrvSendMsg: false,
	}

	uploadCtx, cancel := context.WithTimeout(ctx, qqMediaUploadTimeout)
	defer cancel()

	var resp mediaUploadResponse
	if err := c.api.call(uploadCtx, http.MethodPost, path, req, &resp); err != nil {
		return "", err
	}
	if resp.FileInfo == "" {
		return "", fmt.Errorf("qq: upload of %s returned no file_info", fileName)
	}
	slog.Debug("qq: media uploaded", "kind", kind, "file", fileName, "file_type", fileType,
		"bytes", len(data), "ttl_seconds", resp.TTL)
	return resp.FileInfo, nil
}

// deliverMedia uploads and sends each attachment as its own msg_type=7 message, and returns the
// names of the ones that could not be delivered. A failed attachment is reported in the chat:
// "here is the chart" with no chart reads as a broken agent, not as a failed upload.
func (c *Channel) deliverMedia(ctx context.Context, kind, target string, files []bus.MediaAttachment, firstSeq int) []string {
	var problems []string
	for i, m := range files {
		if err := c.sendMediaFile(ctx, kind, target, m, firstSeq+i); err != nil {
			slog.Warn("qq: media send failed", "media", m.URL, "content_type", m.ContentType, "error", err)
			problems = append(problems, filepath.Base(strings.TrimSpace(m.URL)))
		}
	}
	return problems
}

func (c *Channel) sendMediaFile(ctx context.Context, kind, target string, m bus.MediaAttachment, msgSeq int) error {
	path := strings.TrimSpace(m.URL)
	if path == "" {
		return fmt.Errorf("media carries no local path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read media %s: %w", path, err)
	}
	fileName := filepath.Base(path)
	info, err := c.uploadMedia(ctx, kind, target, fileName, data, qqFileTypeFor(m.ContentType, fileName))
	if err != nil {
		return err
	}

	sendCtx, cancel := context.WithTimeout(ctx, qqMediaSendTimeout)
	defer cancel()
	return c.api.send(sendCtx, kind, target, sendRequest{
		MsgType: msgTypeMedia,
		Content: m.Caption,
		Media:   &mediaRef{FileInfo: info},
		MsgSeq:  msgSeq,
	})
}

// ttsVoiceForText speaks a reply when the tenant or agent asked for it, the way Telegram,
// Discord and WhatsApp do. It returns the text with any [[tts]] markers removed — a channel
// that never calls this delivers those markers to the user literally.
func (c *Channel) ttsVoiceForText(ctx context.Context, content, chatID string, voiceInbound bool) (string, *bus.MediaAttachment) {
	if c.audioMgr == nil || content == "" {
		return content, nil
	}
	res, err := c.audioMgr.AutoApplyToText(ctx, content, channels.TypeQQ, voiceInbound, "")
	if err != nil {
		slog.Debug("qq: tts auto-apply failed", "chat_id", chatID, "error", err)
	}
	if res == nil {
		return content, nil
	}
	if res.AudioPath == "" {
		return res.Text, nil
	}
	return res.Text, &bus.MediaAttachment{URL: res.AudioPath, ContentType: res.AudioMime}
}
