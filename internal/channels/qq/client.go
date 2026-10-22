package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIBase is the unified host the current v2 docs name
	// ("统一请求地址 https://api.bot.qq.com"). api.sgroup.qq.com only survives in the
	// legacy guild-era pages, so it is not used and not offered as a default.
	defaultAPIBase = "https://api.bot.qq.com"

	tokenPath   = "/app/getAppAccessToken"
	gatewayPath = "/gateway"

	maxResponseBytes = 1 << 20 // 1MB: QQ answers are small; an unbounded read is a memory risk
)

// api is the REST client for one bot instance, including the access-token lifecycle.
// It owns the token because every entry point (send, gateway lookup, the websocket
// handshake and its resumes) needs one and they must not race each other for refreshes.
type api struct {
	base      string
	appID     string
	appSecret string

	httpClient *http.Client

	mu      sync.Mutex
	token   string
	renewAt time.Time
}

func newAPI(base, appID, appSecret string) (*api, error) {
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("qq: app_id and app_secret are required")
	}
	if base == "" {
		base = defaultAPIBase
	}
	base = strings.TrimRight(base, "/")

	// api_base exists for a self-hosted relay or a test server, the same role Telegram's
	// api_server plays, so it is operator-level configuration rather than a value derived
	// from inbound data. Two things are still pinned here: only http/https may be
	// dialled, and the host chosen at construction is the only host a redirect may move
	// the conversation to — otherwise a compromised or misconfigured relay could point
	// this client at a link-local or metadata endpoint and get the body back in an error.
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("qq: invalid api_base %q: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("qq: api_base %q must be an absolute http(s) URL", base)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	host := u.Host
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != host {
			return fmt.Errorf("qq: refusing redirect from %s to %s", host, req.URL.Host)
		}
		if len(via) >= 3 {
			return fmt.Errorf("qq: too many redirects")
		}
		return nil
	}

	return &api{base: base, appID: appID, appSecret: appSecret, httpClient: client}, nil
}

// --- Access token ---

// tokenResponse tolerates both shapes the docs print for expires_in (the number 7200 and
// the string "7200"), which is why it is a RawMessage rather than an int.
type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

// parseExpiresIn converts the expires_in field to seconds. QQ's own pages show it both
// unquoted and quoted, so either must work; anything unparsable or non-positive is an
// error rather than a silent default, because a wrong lifetime here means either a
// request storm or a channel that stops authenticating.
func parseExpiresIn(raw json.RawMessage) (int, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" {
		return 0, fmt.Errorf("qq: token response has no expires_in")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("qq: unparsable expires_in %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("qq: non-positive expires_in %d", n)
	}
	return n, nil
}

// accessToken returns a usable token, renewing inside the final accessTokenSkewSecs of the
// current one. That boundary is the documented behaviour, not a guess: a request made
// earlier does not renew the token, while one made inside the window issues a new token
// and leaves the old valid for the rest of the window — so renewing exactly there is both
// early enough to be safe and late enough to actually get a new token.
func (a *api) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && time.Now().Before(a.renewAt) {
		return a.token, nil
	}

	body, err := json.Marshal(map[string]string{"appId": a.appID, "clientSecret": a.appSecret})
	if err != nil {
		return "", fmt.Errorf("qq: encode token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+tokenPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("qq: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("qq: token request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("qq: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qq: token http %d: %s", resp.StatusCode, truncateForLog(data))
	}

	var tr tokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", fmt.Errorf("qq: decode token response: %w (body=%s)", err, truncateForLog(data))
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("qq: token response has no access_token: %s", truncateForLog(data))
	}
	secs, err := parseExpiresIn(tr.ExpiresIn)
	if err != nil {
		return "", err
	}

	a.token = tr.AccessToken
	a.renewAt = time.Now().Add(time.Duration(secs)*time.Second - accessTokenSkewSecs*time.Second)
	if a.renewAt.Before(time.Now()) {
		// A token shorter than the renewal skew would otherwise be refetched per call.
		// Keep it, but renew at half its life so progress is still possible.
		a.renewAt = time.Now().Add(time.Duration(secs) * time.Second / 2)
	}
	return a.token, nil
}

// invalidate drops the cached token so the next call fetches one. Used when the API
// rejects the current token.
func (a *api) invalidate() {
	a.mu.Lock()
	a.token = ""
	a.renewAt = time.Time{}
	a.mu.Unlock()
}

// --- Generic authenticated call ---

// call performs one authenticated JSON request. The token is attached as the documented
// `QQBot {token}` value; a 401 clears it and retries once, because an expired token can
// outlive our own clock skew by a request or two.
func (a *api) call(ctx context.Context, method, path string, in, out any) error {
	err := a.callOnce(ctx, method, path, in, out)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "http 401") {
		a.invalidate()
		return a.callOnce(ctx, method, path, in, out)
	}
	return err
}

func (a *api) callOnce(ctx context.Context, method, path string, in, out any) error {
	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("qq: encode %s: %w", path, err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return fmt.Errorf("qq: build %s request: %w", path, err)
	}
	req.Header.Set("Authorization", "QQBot "+token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("qq: %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("qq: read %s response: %w", path, err)
	}

	// Every v2 payload carries err_code, including the ones returned with a 2xx status,
	// so the body is always inspected. Judging by message instead of err_code is what the
	// docs explicitly forbid.
	var apiErr apiError
	if json.Unmarshal(data, &apiErr) == nil && apiErr.code() != 0 {
		return &apiErr
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("qq: %s rate limited (http 429): %s", path, truncateForLog(data))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qq: %s http %d: %s", path, resp.StatusCode, truncateForLog(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("qq: decode %s: %w (body=%s)", path, err, truncateForLog(data))
	}
	return nil
}

// gatewayURL asks QQ for this bot's websocket endpoint. The docs return it per request, so
// it is not cached forever — but a reconnect storm must not re-query either, so the
// channel keeps the last value it was given.
func (a *api) gatewayURL(ctx context.Context) (string, error) {
	var gr gatewayResponse
	if err := a.call(ctx, http.MethodGet, gatewayPath, nil, &gr); err != nil {
		return "", err
	}
	if gr.URL == "" {
		return "", fmt.Errorf("qq: gateway response carried no url")
	}
	return gr.URL, nil
}

// send posts one message to the endpoint for the conversation kind. The openid is
// path-escaped because it is opaque data that originated in an inbound event.
func (a *api) send(ctx context.Context, kind, target string, req sendRequest) error {
	var path string
	switch kind {
	case "c2c":
		path = "/v2/users/" + url.PathEscape(target) + "/messages"
	case "group":
		path = "/v2/groups/" + url.PathEscape(target) + "/messages"
	default:
		return fmt.Errorf("qq: unknown conversation kind %q", kind)
	}
	return a.call(ctx, http.MethodPost, path, req, nil)
}

func truncateForLog(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
