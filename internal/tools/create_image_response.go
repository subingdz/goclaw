package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/security"
)

// Image providers do not agree on how a finished image is handed back. OpenAI inlines base64
// only when asked; Zhipu CogView documents "the output is an image URL, you must download it";
// SiliconFlow does not even use the same array name. All of them are the same call from
// GoClaw's point of view — one POST to {base}/images/generations — so the response is read
// tolerantly rather than per vendor. The shapes covered here, each observed in a provider's own
// documentation:
//
//	data: [{"b64_json": "..."}]      OpenAI, MiniMax, most western gateways
//	data: [{"url": "https://..."}]   Zhipu CogView, Alibaba Bailian, Volcengine Ark, 302.AI
//	data: ["https://..."]            the shape Zhipu's own doc table types `data` as
//	images: [{"url": "...", "kind": "generated"|"original"}]   SiliconFlow
//	url / b64_json carrying a data:image/...;base64,... string  proxy gateways
//
// A URL has to become bytes here and now, not be handed along: every one of these links expires
// (24 hours for Bailian and 302.AI, 30 days for CogView), and the agent's workspace copy is the
// only thing that outlives that window.

const (
	// generatedImageDownloadTimeout bounds one fetch of a provider-returned image URL.
	generatedImageDownloadTimeout = 60 * time.Second

	// generatedImageKindOriginal marks an echoed input image in an image-to-image response;
	// SiliconFlow labels every entry either "generated" or "original".
	generatedImageKindOriginal = "original"
)

// generatedImageMaxBytes caps one downloaded generated image. A var so tests can exercise the
// overflow path without allocating 20 MB.
var generatedImageMaxBytes int64 = 20 * 1024 * 1024

// generatedImage is one image reference in whichever shape the provider used.
type generatedImage struct {
	b64     string
	url     string
	imgKind string
}

func (g generatedImage) empty() bool { return g.b64 == "" && g.url == "" }

// wireImageItem is one entry of a generation response array. Entries are usually objects, and
// sometimes plain strings, so decoding tries both.
type wireImageItem struct {
	URL     string `json:"url"`
	B64JSON string `json:"b64_json"`
	Kind    string `json:"kind"`
}

type wireImageResponse struct {
	Data   []json.RawMessage `json:"data"`
	Images []json.RawMessage `json:"images"`
}

// generatedImageBytes parses a generation (or edit) response body and returns the image bytes,
// downloading the URL when the provider returned a location instead of the image.
func (t *CreateImageTool) generatedImageBytes(ctx context.Context, body []byte) ([]byte, error) {
	img, err := pickGeneratedImage(body)
	if err != nil {
		return nil, err
	}
	return t.resolveGeneratedImage(ctx, img)
}

// pickGeneratedImage selects the entry that actually carries an image, preferring a generated
// one over an echoed input (image-to-image responses can include both).
func pickGeneratedImage(body []byte) (generatedImage, error) {
	var payload wireImageResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return generatedImage{}, fmt.Errorf("parse image response: %w", err)
	}

	// `data` first: it is the OpenAI name and the one the majority of gateways use.
	entries := append([]json.RawMessage{}, payload.Data...)
	entries = append(entries, payload.Images...)

	var items []generatedImage
	for _, raw := range entries {
		item, ok := decodeImageEntry(raw)
		if !ok {
			continue
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return generatedImage{}, fmt.Errorf(
			"no image in response (looked for data[].b64_json, data[].url and images[].url): %s",
			truncateBytes(body, 200))
	}
	for _, item := range items {
		if item.imgKind != generatedImageKindOriginal {
			return item, nil
		}
	}
	// Every entry was an echoed input. Better than nothing, and the log will say so.
	return items[0], nil
}

// decodeImageEntry reads one array element as an object or as a bare URL string.
func decodeImageEntry(raw json.RawMessage) (generatedImage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return generatedImage{}, false
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return generatedImage{}, false
		}
		return imageRefFromStrings("", s)
	}

	var item wireImageItem
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return generatedImage{}, false
	}
	ref, ok := imageRefFromStrings(item.B64JSON, item.URL)
	if !ok {
		return generatedImage{}, false
	}
	ref.imgKind = strings.ToLower(strings.TrimSpace(item.Kind))
	return ref, true
}

// imageRefFromStrings normalizes the two payload fields. A b64_json that turns out to hold a
// full data URL is treated as the URL it is rather than base64-decoded as-is.
func imageRefFromStrings(b64, link string) (generatedImage, bool) {
	b64 = strings.TrimSpace(b64)
	link = strings.TrimSpace(link)

	if strings.HasPrefix(b64, "data:") {
		b64, link = "", b64
	}
	switch {
	case b64 != "":
		return generatedImage{b64: b64}, true
	case link != "":
		return generatedImage{url: link}, true
	default:
		return generatedImage{}, false
	}
}

// resolveGeneratedImage turns one selected reference into bytes.
func (t *CreateImageTool) resolveGeneratedImage(ctx context.Context, img generatedImage) ([]byte, error) {
	if img.empty() {
		return nil, fmt.Errorf("image reference is empty")
	}
	if img.b64 != "" {
		return decodeImageBase64(img.b64)
	}
	if strings.HasPrefix(strings.ToLower(img.url), "data:") {
		return decodeDataURL(img.url)
	}
	if !isHTTPURL(img.url) {
		return nil, fmt.Errorf("provider returned an unusable image location: %q", redactImageURL(img.url))
	}
	return downloadGeneratedImage(ctx, img.url)
}

// decodeImageBase64 accepts standard and unpadded base64. Providers that omit the trailing "="
// are common enough that a single strict decode is the difference between a working image and a
// confusing error.
func decodeImageBase64(b64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err == nil {
		return data, nil
	}
	if raw, err2 := base64.RawStdEncoding.DecodeString(b64); err2 == nil {
		return raw, nil
	}
	return nil, fmt.Errorf("decode base64 image: %w", err)
}

// downloadGeneratedImage fetches an image URL that came back from a provider.
//
// The URL is not peer-supplied, but it is still remote-controlled: an attacker who can point a
// tenant's base_url at their own server controls what this fetch dials, so the same SSRF guard
// used for caller-supplied reference images applies — validate, pin the resolved IP, and only
// follow a redirect that stays on the same host (the pinned dial keeps the destination fixed, so
// a same-host hop cannot reach a new private address).
func downloadGeneratedImage(ctx context.Context, rawURL string) ([]byte, error) {
	u, pinnedIP, err := security.Validate(rawURL)
	if err != nil {
		return nil, fmt.Errorf("refused image URL: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, generatedImageDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(security.WithPinnedIP(reqCtx, pinnedIP), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build image download request: %w", err)
	}

	client := security.NewSafeClient(generatedImageDownloadTimeout)
	client.CheckRedirect = sameHostRedirect(u.Host)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download generated image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("generated image download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, generatedImageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read generated image: %w", err)
	}
	if int64(len(data)) > generatedImageMaxBytes {
		return nil, fmt.Errorf("generated image exceeds the %d byte limit", generatedImageMaxBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("generated image is empty")
	}
	return data, nil
}

// sameHostRedirect allows a redirect only when it names the host that was already validated and
// pinned. Anything else — a different host, or a scheme change — is returned to the caller as
// the terminal response and rejected by the status check.
func sameHostRedirect(host string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return fmt.Errorf("too many redirects while downloading a generated image")
		}
		if !strings.EqualFold(req.URL.Host, host) {
			return fmt.Errorf("refused redirect from %s to %s while downloading a generated image", host, req.URL.Host)
		}
		return nil
	}
}

// redactImageURL keeps a diagnostic string free of signed query parameters, which is what these
// vendor URLs usually are.
func redactImageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.Fragment = ""
	u.RawQuery = ""
	u.User = nil
	return u.String()
}
