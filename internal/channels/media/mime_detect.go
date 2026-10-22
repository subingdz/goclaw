package media

import (
	"mime"
	"path/filepath"
	"strings"
)

// AudioSTTMime returns the MIME type to hand an audio attachment to a transcription
// provider. Platforms differ in what they report for a voice note — several send
// "application/octet-stream", some send nothing — and a provider that types the upload
// by MIME reads a blank as a decode error that looks like an engine fault. The fallback
// stays the caller's decision because each platform knows what it actually delivers
// (Telegram and Feishu ship opus in ogg, WeChat ships silk).
func AudioSTTMime(contentType, fallback string) string {
	mime := strings.TrimSpace(contentType)
	if mime == "" || strings.EqualFold(mime, "application/octet-stream") {
		return fallback
	}
	return mime
}

// DetectMIMEType returns the MIME type for a file based on its extension.
// Uses Go's mime.TypeByExtension with a fallback to "application/octet-stream".
func DetectMIMEType(fileName string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		return "application/octet-stream"
	}

	// mime.TypeByExtension returns "" if unknown.
	if mt := mime.TypeByExtension(ext); mt != "" {
		return mt
	}

	// Fallback for common types not always in Go's registry.
	switch ext {
	case ".opus":
		return "audio/ogg"
	case ".ogg":
		return "audio/ogg"
	case ".webp":
		return "image/webp"
	case ".flac":
		return "audio/flac"
	case ".mkv":
		return "video/x-matroska"
	case ".m4a":
		return "audio/mp4"
	default:
		return "application/octet-stream"
	}
}

// MediaKindFromMime returns the media kind ("image", "video", "audio", "document")
// based on MIME type prefix.
func MediaKindFromMime(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "document"
	}
}
