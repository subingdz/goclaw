package qq

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

const (
	// qqMaxMediaBytes matches the other channels' inbound caps. QQ's own file limit is
	// smaller than this for voice, so the ceiling only matters for files and video.
	qqMaxMediaBytes = 20 << 20

	qqDownloadTimeout = 30 * time.Second

	// qqSTTTimeout is the same wall clock Feishu and Discord use; a voice note is short and
	// a slow engine should not hold the inbound goroutine.
	qqSTTTimeout = 10 * time.Second
)

// audioExtensions is the set QQ is seen to use for voice and music attachments. The wire
// carries content_type, but it is frequently empty or generic on this API, so the filename
// is the practical signal.
var audioExtensions = map[string]bool{
	".slk": true, ".silk": true, ".amr": true, ".wav": true, ".mp3": true,
	".ogg": true, ".oga": true, ".opus": true, ".m4a": true, ".aac": true,
	".flac": true, ".wma": true,
}

// attachmentKind classifies one attachment: audio | image | video | file.
func attachmentKind(a attachmentRef) string {
	ct := strings.ToLower(a.ContentType)
	switch {
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	}
	ext := strings.ToLower(filepath.Ext(a.Filename))
	if ext == "" && a.URL != "" {
		if u, err := urlPath(a.URL); err == nil {
			ext = strings.ToLower(path.Ext(u))
		}
	}
	switch {
	case audioExtensions[ext]:
		return "audio"
	case ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".bmp":
		return "image"
	case ext == ".mp4" || ext == ".mov" || ext == ".avi" || ext == ".webm" || ext == ".mkv":
		return "video"
	default:
		return "file"
	}
}

func urlPath(raw string) (string, error) {
	u, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	return u.URL.Path, nil
}

// processAttachments fetches what the event points at and returns the files for the pipeline
// plus the media tags that replace the "there were attachments" placeholder.
//
// Attachments we cannot fetch stay as a note: an agent that is told nothing will answer a
// voice message with "I didn't receive anything", which is worse than admitting the gap.
func (c *Channel) processAttachments(ctx context.Context, atts []attachmentRef) ([]bus.MediaFile, string) {
	if len(atts) == 0 {
		return nil, ""
	}

	var (
		infos  []media.MediaInfo
		files  []bus.MediaFile
		failed []string
	)

	for _, a := range atts {
		info, err := c.downloadAttachment(ctx, a)
		if err != nil {
			slog.Warn("qq: inbound attachment download failed",
				"kind", attachmentKind(a), "filename", a.Filename, "error", err)
			failed = append(failed, a.kindLabel())
			continue
		}
		if info.Type == media.TypeAudio || info.Type == media.TypeVoice {
			c.transcribe(ctx, info)
		}
		infos = append(infos, *info)
		files = append(files, bus.MediaFile{
			Path:     info.FilePath,
			MimeType: info.ContentType,
			Filename: info.FileName,
		})
	}

	tags := media.BuildMediaTags(infos)
	if len(failed) > 0 {
		note := fmt.Sprintf("[附件 %d 个未能获取: %s]", len(failed), strings.Join(failed, ", "))
		tags = strings.TrimSpace(tags + "\n" + note)
	}
	return files, tags
}

func (a attachmentRef) kindLabel() string {
	if a.Filename != "" {
		return a.Filename
	}
	return attachmentKind(a)
}

// downloadAttachment fetches one QQ CDN URL into a temp file.
//
// The URL arrives inside an event about a stranger's message, so it is checked for internal
// targets before anything is requested, and the response is capped rather than trusted.
func (c *Channel) downloadAttachment(ctx context.Context, a attachmentRef) (*media.MediaInfo, error) {
	if a.URL == "" {
		return nil, fmt.Errorf("attachment has no url")
	}
	if a.Size > qqMaxMediaBytes {
		return nil, fmt.Errorf("attachment too large (%d bytes, max %d)", a.Size, qqMaxMediaBytes)
	}
	if err := tools.CheckSSRF(a.URL); err != nil {
		return nil, fmt.Errorf("refused attachment url: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, qqDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.mediaClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	ext := attachmentExt(a)
	name := a.Filename
	if name == "" {
		name = "attachment" + ext
	}

	f, err := os.CreateTemp("", "goclaw_qq_*"+ext)
	if err != nil {
		return nil, fmt.Errorf("create temp: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(resp.Body, qqMaxMediaBytes))
	if err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("write: %w", err)
	}
	if n == 0 {
		os.Remove(f.Name())
		return nil, fmt.Errorf("empty attachment body")
	}
	if n == qqMaxMediaBytes {
		// The limit is generous enough that what survived is still useful to the agent;
		// saying so out loud beats pretending the file is complete.
		slog.Warn("qq: attachment reached the size cap", "filename", name, "bytes", n)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = media.DetectMIMEType(name)
	}
	info := &media.MediaInfo{
		Type:        mediaKindToType(attachmentKind(a)),
		FilePath:    f.Name(),
		SourceURL:   a.URL,
		ContentType: contentType,
		FileName:    name,
		FileSize:    n,
	}
	slog.Debug("qq: attachment downloaded", "kind", info.Type, "bytes", n, "mime", contentType)
	return info, nil
}

func attachmentExt(a attachmentRef) string {
	if ext := safeExt(filepath.Ext(a.Filename)); ext != "" {
		return ext
	}
	if a.URL != "" {
		if p, err := urlPath(a.URL); err == nil {
			if ext := safeExt(path.Ext(p)); ext != "" {
				return ext
			}
		}
	}
	return ".bin"
}

func mediaKindToType(kind string) string {
	switch kind {
	case "audio":
		return media.TypeVoice
	case "image":
		return media.TypeImage
	case "video":
		return media.TypeVideo
	default:
		return media.TypeDocument
	}
}

// transcribe runs the shared STT chain over one audio attachment. QQ voice is SILK, which the
// Manager decodes before calling any provider — the channel never needs to know that.
func (c *Channel) transcribe(ctx context.Context, info *media.MediaInfo) {
	if c.audioMgr == nil || !c.audioMgr.STTAvailable(ctx) {
		slog.Debug("qq: no STT provider available, leaving voice untranscribed")
		return
	}

	sttCtx, cancel := context.WithTimeout(ctx, c.audioMgr.STTTimeout(qqSTTTimeout))
	defer cancel()
	sttCtx = audio.WithChannel(sttCtx, channels.TypeQQ)

	res, err := c.audioMgr.Transcribe(sttCtx, audio.STTInput{
		FilePath: info.FilePath,
		MimeType: media.AudioSTTMime(info.ContentType, "audio/silk"),
		Filename: info.FileName,
	}, audio.STTOptions{})
	if err != nil {
		slog.Warn("qq: STT transcription failed", "file", info.FileName, "error", err)
		return
	}
	if res != nil && res.Text != "" {
		info.Transcript = res.Text
	}
}

// mediaClient is the download HTTP client, created lazily so a channel that never receives
// an attachment holds no connection pool.
func (c *Channel) mediaClient() *http.Client {
	if c.mediaHTTP != nil {
		return c.mediaHTTP
	}
	c.mediaHTTP = &http.Client{Timeout: qqDownloadTimeout, CheckRedirect: c.checkRedirect}
	return c.mediaHTTP
}

// checkRedirect re-validates every hop. The URL in the event is only the first destination we
// would contact, and a CDN that answers with a redirect to a private address is the classic
// way to turn "fetch the attachment" into "read the metadata endpoint".
func (c *Channel) checkRedirect(req *http.Request, _ []*http.Request) error {
	if err := tools.CheckSSRF(req.URL.String()); err != nil {
		return fmt.Errorf("refused redirect target: %w", err)
	}
	return nil
}

// safeExt keeps an attachment extension usable in a temp-file pattern. The extension arrives
// from a remote filename, so anything that could act as a path separator or traverse a
// directory is dropped rather than cleaned.
func safeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" || ext[0] != '.' || len(ext) > 6 {
		return ""
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return ext
}
