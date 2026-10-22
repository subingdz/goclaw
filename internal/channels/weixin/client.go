package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20 // 4MB: iLink answers are small; an unbounded read is a memory risk

// client is the iLink HTTP layer for one activated bot.
type client struct {
	baseURL string
	token   string
	proxy   string

	httpClient *http.Client
}

func newClient(baseURL, token, proxy string) (*client, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("weixin: base_url %q must be an absolute http(s) URL", baseURL)
	}

	c := &client{baseURL: baseURL, token: token, proxy: proxy}
	if proxy != "" {
		rt, err := proxyTransport(proxy)
		if err != nil {
			return nil, err
		}
		c.httpClient = &http.Client{Timeout: 90 * time.Second, Transport: rt}
	} else {
		c.httpClient = &http.Client{Timeout: 90 * time.Second}
	}
	return c, nil
}

// proxyTransport clones the default transport with an http(s) proxy. api_base and proxy are
// operator-level settings (the same trust level as Telegram's proxy field), but the scheme is
// still pinned: a typo or a hostile relay must not turn this client into a generic proxy for
// arbitrary schemes.
func proxyTransport(proxy string) (http.RoundTripper, error) {
	pu, err := url.Parse(proxy)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return nil, fmt.Errorf("weixin: proxy %q must be an absolute http(s) URL", proxy)
	}
	rt := http.DefaultTransport.(*http.Transport).Clone()
	rt.Proxy = http.ProxyURL(pu)
	return rt, nil
}

// randomUIN mimics the client field iLink expects on every authenticated call. It carries
// no meaning beyond "not constant", which is why it is random rather than derived.
func randomUIN() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	n := binary.BigEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(n), 10)))
}

func (c *client) setCommonHeaders(req *http.Request) {
	req.Header.Set("iLink-App-Id", appID)
	req.Header.Set("iLink-App-ClientVersion", strconv.Itoa(clientVersion))
}

func (c *client) setAuthHeaders(req *http.Request) {
	c.setCommonHeaders(req)
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	if uin := randomUIN(); uin != "" {
		req.Header.Set("X-WECHAT-UIN", uin)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// post performs one authenticated CGI call. The returned status is the caller's, so the
// poll loop can distinguish -14 (session expired) from an ordinary failure.
func (c *client) post(ctx context.Context, endpoint string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("weixin: encode %s: %w", endpoint, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.join(endpoint), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("weixin: build %s request: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("weixin: %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("weixin: read %s response: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("weixin: %s http %d: %s", endpoint, resp.StatusCode, truncate(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("weixin: decode %s: %w (body=%s)", endpoint, err, truncate(data))
	}
	return nil
}

// get performs the two unauthenticated QR endpoints: they are what turns a scan into a
// token, so they must be reachable before any token exists.
func (c *client) get(ctx context.Context, endpoint string, query map[string]string, out any) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}
	u.Path = path.Join(u.Path, endpoint)
	q := u.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("weixin: build %s request: %w", endpoint, err)
	}
	c.setCommonHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("weixin: %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("weixin: read %s response: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("weixin: %s http %d: %s", endpoint, resp.StatusCode, truncate(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("weixin: decode %s: %w (body=%s)", endpoint, err, truncate(data))
	}
	return nil
}

func (c *client) join(endpoint string) string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + endpoint
	}
	u.Path = path.Join(u.Path, endpoint)
	return u.String()
}

// --- CGI calls ---

func (c *client) getUpdates(ctx context.Context, req getUpdatesReq) (*getUpdatesResp, error) {
	req.BaseInfo = baseInfo{ChannelVersion: channelVersion}
	var resp getUpdatesResp
	if err := c.post(ctx, endpointGetUpdates, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *client) sendMessage(ctx context.Context, req sendMessageReq) (*sendMessageResp, error) {
	req.BaseInfo = baseInfo{ChannelVersion: channelVersion}
	var resp sendMessageResp
	if err := c.post(ctx, endpointSendMessage, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *client) getConfig(ctx context.Context, req getConfigReq) (*getConfigResp, error) {
	req.BaseInfo = baseInfo{ChannelVersion: channelVersion}
	var resp getConfigResp
	if err := c.post(ctx, endpointGetConfig, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// getUploadURL reserves a CDN upload slot for one outbound attachment. The service signs the
// padded ciphertext length, not the raw file length, which is why the caller pads before asking.
func (c *client) getUploadURL(ctx context.Context, req getUploadURLReq) (*getUploadURLResp, error) {
	req.BaseInfo = baseInfo{ChannelVersion: channelVersion}
	var resp getUploadURLResp
	if err := c.post(ctx, endpointGetUploadURL, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *client) sendTyping(ctx context.Context, req sendTypingReq) (*sendTypingResp, error) {
	req.BaseInfo = baseInfo{ChannelVersion: channelVersion}
	var resp sendTypingResp
	if err := c.post(ctx, endpointSendTyping, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- QR activation ---

func (c *client) requestQR(ctx context.Context) (*qrCodeResponse, error) {
	var resp qrCodeResponse
	if err := c.get(ctx, endpointGetQRCode, map[string]string{"bot_type": botType}, &resp); err != nil {
		return nil, err
	}
	if resp.Qrcode == "" || resp.QrcodeImgContent == "" {
		return nil, fmt.Errorf("weixin: QR response carried no qrcode")
	}
	return &resp, nil
}

// pollQR reads the scan state once. A redirect is not an error: it means the account was
// routed to another cell and only the *polling* must continue against that host.
func (c *client) pollQR(ctx context.Context, qrcode string) (*qrStatusResponse, error) {
	var resp qrStatusResponse
	if err := c.get(ctx, endpointQRStatus, map[string]string{"qrcode": qrcode}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func truncate(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
