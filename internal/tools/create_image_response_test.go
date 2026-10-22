package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/security"
)

// The bug these tests pin: every domestic image gateway clones the OpenAI request shape and then
// answers in its own shape. GoClaw read one field (data[0].b64_json) and reported everything else
// as "no image data in response".

func pngFixture() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte("-payload")...)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestPickGeneratedImageShapes(t *testing.T) {
	png := pngFixture()
	enc := b64(png)

	cases := []struct {
		name     string
		body     string
		wantB64  string
		wantURL  string
		wantKind string
	}{
		{"openai base64", `{"data":[{"b64_json":"` + enc + `"}]}`, enc, "", ""},
		{"zhipu cogview url", `{"data":[{"url":"https://sfile.chatglm.cn/a.png"}]}`, "", "https://sfile.chatglm.cn/a.png", ""},
		{"siliconflow images array", `{"images":[{"url":"https://c.siliconflow.cn/b.png","kind":"generated"}]}`, "", "https://c.siliconflow.cn/b.png", "generated"},
		{"data typed as string array", `{"data":["https://sfile.chatglm.cn/c.png"]}`, "", "https://sfile.chatglm.cn/c.png", ""},
		{"b64_json holding a data url", `{"data":[{"b64_json":"data:image/png;base64,` + enc + `"}]}`, "", "data:image/png;base64," + enc, ""},
		{"empty entry skipped for the next one", `{"data":[{},{"url":"https://x.test/d.png"}]}`, "", "https://x.test/d.png", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickGeneratedImage([]byte(tc.body))
			if err != nil {
				t.Fatalf("pickGeneratedImage() error = %v", err)
			}
			if got.b64 != tc.wantB64 || got.url != tc.wantURL {
				t.Fatalf("got b64=%q url=%q, want b64=%q url=%q", got.b64, got.url, tc.wantB64, tc.wantURL)
			}
			if tc.wantKind != "" && got.imgKind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.imgKind, tc.wantKind)
			}
		})
	}
}

func TestPickGeneratedImagePrefersGeneratedOverOriginal(t *testing.T) {
	// SiliconFlow's image-to-image answer can carry the echoed input as well as the output.
	body := `{"images":[{"url":"https://c.test/input.png","kind":"original"},{"url":"https://c.test/out.png","kind":"generated"}]}`
	got, err := pickGeneratedImage([]byte(body))
	if err != nil {
		t.Fatalf("pickGeneratedImage() error = %v", err)
	}
	if got.url != "https://c.test/out.png" {
		t.Fatalf("url = %q, want the generated entry, not the echoed input", got.url)
	}

	// Position in the array is not a signal; the reversed order must choose the same entry.
	reversed := `{"images":[{"url":"https://c.test/out.png","kind":"generated"},{"url":"https://c.test/input.png","kind":"original"}]}`
	got, err = pickGeneratedImage([]byte(reversed))
	if err != nil || got.url != "https://c.test/out.png" {
		t.Fatalf("reversed order: got %q (err %v)", got.url, err)
	}
}

func TestPickGeneratedImageErrorNamesTheShapes(t *testing.T) {
	_, err := pickGeneratedImage([]byte(`{"code":"Throttling","message":"rate limited"}`))
	if err == nil {
		t.Fatal("a body with no image was accepted")
	}
	for _, want := range []string{"data[].b64_json", "data[].url", "images[].url", "rate limited"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDecodeImageBase64AcceptsUnpadded(t *testing.T) {
	want := "\x89PNG"
	padded := b64([]byte{0x89, 'P', 'N', 'G'})
	unpadded := strings.TrimRight(padded, "=")

	for _, s := range []string{padded, unpadded} {
		data, err := decodeImageBase64(s)
		if err != nil || string(data) != want {
			t.Fatalf("decodeImageBase64(%q) = %q, err %v, want %q", s, data, err, want)
		}
	}
	if _, err := decodeImageBase64("!!! not base64 !!!"); err == nil {
		t.Fatal("garbage decoded")
	}
}

// cdnServer serves the bytes a provider's URL points at.
func cdnServer(payload []byte, status int, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(payload)
	}))
}

func TestGeneratedImageBytesDownloadsAURLResponse(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	png := pngFixture()
	var hits atomic.Int32
	cdn := cdnServer(png, 0, &hits)
	t.Cleanup(cdn.Close)

	tool := NewCreateImageTool(nil)
	got, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"created":1700000000,"data":[{"url":"`+cdn.URL+`/image.png?sig=abc"}]}`))
	if err != nil {
		t.Fatalf("generatedImageBytes() error = %v", err)
	}
	if string(got) != string(png) {
		t.Fatalf("bytes = %q, want the CDN payload", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("cdn hits = %d, want 1", hits.Load())
	}
}

func TestGeneratedImageFollowsSameHostRedirectOnly(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	png := pngFixture()
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("SECRET-OTHER-HOST"))
	}))
	t.Cleanup(other.Close)

	// One server plays both roles: /redirect hands off to its own /image, and to `other`.
	var sameHostHits, crossHostHits atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-same-host":
			sameHostHits.Add(1)
			http.Redirect(w, r, "/image", http.StatusFound)
		case "/redirect-cross-host":
			crossHostHits.Add(1)
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/image":
			sameHostHits.Add(1)
			_, _ = w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gw.Close)

	tool := NewCreateImageTool(nil)

	data, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"data":[{"url":"`+gw.URL+`/redirect-same-host"}]}`))
	if err != nil {
		t.Fatalf("same-host redirect failed: %v", err)
	}
	if string(data) != string(png) {
		t.Fatalf("bytes = %q, want the payload behind the redirect", data)
	}

	if _, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"data":[{"url":"`+gw.URL+`/redirect-cross-host"}]}`)); err == nil {
		t.Fatal("a redirect to another host was followed")
	} else if !strings.Contains(err.Error(), "refused redirect") {
		t.Fatalf("error = %v, want the host pin to refuse it", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the other host was dialled %d times, want 0", hits.Load())
	}
}

// The SSRF guard is not bypassed by the loopback test switch: these targets must be refused
// before anything is dialled, so this test deliberately does not allow loopback.
func TestGeneratedImageDownloadRefusesHostileLocations(t *testing.T) {
	tool := NewCreateImageTool(nil)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"cloud metadata", `{"data":[{"url":"http://169.254.169.254/latest/meta-data/"}]}`, "refused image URL"},
		{"private range", `{"data":[{"url":"http://10.0.0.5/x.png"}]}`, "refused image URL"},
		{"non-http scheme", `{"data":[{"url":"ftp://cdn.test/x.png"}]}`, "unusable image location"},
		{"empty url", `{"data":[{"url":""}]}`, "no image in response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.generatedImageBytes(context.Background(), []byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestGeneratedImageDownloadReportsHTTPSErrors(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	var hits atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "expired", http.StatusForbidden)
	}))
	t.Cleanup(forbidden.Close)

	tool := NewCreateImageTool(nil)
	_, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"images":[{"url":"`+forbidden.URL+`/x.png","kind":"generated"}]}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %v, want the upstream status to be reported", err)
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(empty.Close)
	if _, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"data":[{"url":"`+empty.URL+`/x.png"}]}`)); err == nil {
		t.Fatal("an empty payload was accepted")
	}
}

func TestGeneratedImageDownloadSizeCap(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	orig := generatedImageMaxBytes
	generatedImageMaxBytes = 8
	defer func() { generatedImageMaxBytes = orig }()

	var hits atomic.Int32
	cdn := cdnServer([]byte(strings.Repeat("A", 512)), 0, &hits)
	t.Cleanup(cdn.Close)

	tool := NewCreateImageTool(nil)
	_, err := tool.generatedImageBytes(context.Background(),
		[]byte(`{"data":[{"url":"`+cdn.URL+`/big.png"}]}`))
	if err == nil || !strings.Contains(err.Error(), "exceeds the 8 byte limit") {
		t.Fatalf("error = %v, want the size cap to reject the payload", err)
	}
}

// The point of the change: a CogView-shaped answer, end to end through the standard path.
func TestCallStandardImageGenAPIAcceptsZhipuAndSiliconFlow(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	png := pngFixture()
	cases := []struct {
		name string
		body string
	}{
		{"cogview", `{"created":"1700000000","data":[{"url":"{{SRV}}/cdn/image.png"}]}`},
		{"siliconflow", `{"images":[{"url":"{{SRV}}/cdn/image.png","kind":"generated"}]}`},
		{"openai still works", `{"data":[{"b64_json":"` + b64(png) + `"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/images/generations" {
					seen.Add(1)
					var req map[string]any
					_ = json.NewDecoder(r.Body).Decode(&req)
					if req["model"] != "cogview-4-250304" || req["prompt"] == "" {
						t.Errorf("request = %v, want model and prompt", req)
					}
					fmt.Fprint(w, strings.ReplaceAll(tc.body, "{{SRV}}", "http://"+r.Host))
					return
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(png)
			}))
			t.Cleanup(srv.Close)

			tool := NewCreateImageTool(nil)
			data, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL+"/v1",
				"cogview-4-250304", "一只猫", nil)
			if err != nil {
				t.Fatalf("callStandardImageGenAPI() error = %v", err)
			}
			if string(data) != string(png) {
				t.Fatalf("bytes = %q, want the generated image", data)
			}
			if seen.Load() != 1 {
				t.Fatalf("generate calls = %d, want 1", seen.Load())
			}
		})
	}
}

func TestCallStandardImageGenAPIForwardsOnlyKnownParams(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + b64(pngFixture()) + `"}]}`))
	}))
	t.Cleanup(srv.Close)

	tool := NewCreateImageTool(nil)
	params := map[string]any{
		"size":                "1344x768",
		"image_size":          "1024x1024",
		"guidance_scale":      7.5,
		"num_inference_steps": 4,
		"prompt_extend":       true, // not a documented generations key: must not be sent
		"response_format":     "off",
		"_provider_type":      "openai_compat",
		"aspect_ratio":        "16:9",
	}
	if _, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL, "m", "p", params); err != nil {
		t.Fatalf("callStandardImageGenAPI() error = %v", err)
	}

	for _, key := range []string{"size", "image_size", "guidance_scale", "num_inference_steps"} {
		if _, ok := lastBody[key]; !ok {
			t.Errorf("%s was not forwarded; body = %v", key, lastBody)
		}
	}
	for _, key := range []string{"prompt_extend", "_provider_type", "aspect_ratio", "response_format"} {
		if _, ok := lastBody[key]; ok {
			t.Errorf("%s must not reach the request body; body = %v", key, lastBody)
		}
	}
}

func TestCallStandardImageGenAPIKeepsResponseFormatByDefault(t *testing.T) {
	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + b64(pngFixture()) + `"}]}`))
	}))
	t.Cleanup(srv.Close)

	tool := NewCreateImageTool(nil)
	if _, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL, "dall-e-3", "p", nil); err != nil {
		t.Fatalf("error = %v", err)
	}
	if lastBody["response_format"] != "b64_json" {
		t.Fatalf("response_format = %v, want the OpenAI default kept", lastBody["response_format"])
	}
}

// OpenAI's gpt-image family answers 400 Unknown parameter for response_format, and GoClaw's
// default OpenAI image model is in that family — so the field must not be sent there.
func TestDefaultResponseFormatSkipsTheGPTImageFamily(t *testing.T) {
	for _, model := range []string{"gpt-image-1", "gpt-image-1-mini", "gpt-image-1.5", "gpt-image-2", "chatgpt-image-latest"} {
		if got := defaultResponseFormat(model); got != "off" {
			t.Errorf("defaultResponseFormat(%q) = %q, want the parameter omitted", model, got)
		}
	}
	for _, model := range []string{"dall-e-3", "dall-e-2", "cogview-4-250304", "Kwai-Kolors/Kolors", ""} {
		if got := defaultResponseFormat(model); got != "b64_json" {
			t.Errorf("defaultResponseFormat(%q) = %q, want b64_json", model, got)
		}
	}

	security.SetAllowLoopbackForTest(true)
	defer security.SetAllowLoopbackForTest(false)

	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + b64(pngFixture()) + `"}]}`))
	}))
	t.Cleanup(srv.Close)
	tool := NewCreateImageTool(nil)

	if _, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL, "gpt-image-1.5", "p", nil); err != nil {
		t.Fatalf("error = %v", err)
	}
	if _, ok := lastBody["response_format"]; ok {
		t.Fatalf("body = %v, want no response_format for a gpt-image model", lastBody)
	}

	// The operator's explicit choice still wins in both directions.
	if _, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL, "gpt-image-1.5", "p",
		map[string]any{"response_format": "b64_json"}); err != nil {
		t.Fatalf("error = %v", err)
	}
	if lastBody["response_format"] != "b64_json" {
		t.Fatalf("body = %v, want the explicit override honored", lastBody)
	}
}

func TestCallStandardImageGenAPIReportsAnUnexpectedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	t.Cleanup(srv.Close)

	tool := NewCreateImageTool(nil)
	_, _, err := tool.callStandardImageGenAPI(context.Background(), "k", srv.URL, "nope", "p", nil)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("error = %v, want the upstream message to be visible", err)
	}
}

// imageChainProvider stands in for "the operator registered Zhipu in the panel": a registered
// API-key provider with no image-specific configuration anywhere.
type imageChainProvider struct {
	name  string
	model string
}

func (p *imageChainProvider) Chat(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
	return &providers.ChatResponse{Content: "text only"}, nil
}

func (p *imageChainProvider) ChatStream(ctx context.Context, req providers.ChatRequest, _ func(providers.StreamChunk)) (*providers.ChatResponse, error) {
	return p.Chat(ctx, req)
}

func (p *imageChainProvider) DefaultModel() string { return p.model }
func (p *imageChainProvider) Name() string         { return p.name }
func (p *imageChainProvider) APIKey() string       { return "sk-test" }
func (p *imageChainProvider) APIBase() string      { return "https://open.bigmodel.cn/api/paas/v4" }

// bareProvider implements Provider and nothing else: no static credentials, the way a
// Codex/ChatGPT OAuth provider has none. Spending a person's subscription on an auto-guessed
// chain entry is not the same as spending an API key, so these stay out.
type bareProvider struct{ name string }

func (p *bareProvider) Chat(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
	return &providers.ChatResponse{}, nil
}
func (p *bareProvider) ChatStream(ctx context.Context, req providers.ChatRequest, _ func(providers.StreamChunk)) (*providers.ChatResponse, error) {
	return p.Chat(ctx, req)
}
func (p *bareProvider) DefaultModel() string { return "text-only-model" }
func (p *bareProvider) Name() string         { return p.name }

func TestBuildDefaultChainAppendsRegisteredKeyProviders(t *testing.T) {
	registry := providers.NewRegistry(func(context.Context) uuid.UUID { return uuid.Nil })
	// A priority-list vendor that is actually registered wins the front slot and is not repeated
	// by the append pass.
	registry.Register(&imageChainProvider{name: "openai", model: "text-embedding-3"})
	registry.Register(&imageChainProvider{name: "zhipu", model: "cogview-4-250304"})
	registry.Register(&imageChainProvider{name: "siliconflow", model: "Kwai-Kolors/Kolors"})
	registry.Register(&imageChainProvider{name: "no-model", model: ""})
	registry.Register(&bareProvider{name: "chat-glm"}) // no API credentials → never auto-tried

	chain := buildDefaultChain(context.Background(),
		[]string{"openai", chainWildcard},
		map[string]string{"openai": "gpt-image-1.5"},
		registry)

	var got []string
	for _, e := range chain {
		got = append(got, e.Provider+":"+e.Model)
	}
	want := []string{"openai:gpt-image-1.5", "siliconflow:Kwai-Kolors/Kolors", "zhipu:cogview-4-250304"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("chain = %v, want %v (known vendor first, then the rest sorted by name)", got, want)
	}
	for _, e := range chain {
		if e.Timeout != 600 || e.MaxRetries != 1 {
			t.Fatalf("entry %+v did not get the chain defaults", e)
		}
	}
}

func TestBuildDefaultChainWithoutWildcardStaysClosed(t *testing.T) {
	registry := providers.NewRegistry(func(context.Context) uuid.UUID { return uuid.Nil })
	registry.Register(&imageChainProvider{name: "zhipu", model: "cogview-4"})

	// create_video and the read_* tools keep their explicit vendor lists untouched.
	chain := buildDefaultChain(context.Background(), []string{"byteplus"}, map[string]string{}, registry)
	if len(chain) != 0 {
		t.Fatalf("chain = %+v, want no auto-appended provider for a tool that did not ask", chain)
	}
}

func TestExecuteWithChainFallsBackToProviderDefaultModel(t *testing.T) {
	registry := providers.NewRegistry(func(context.Context) uuid.UUID { return uuid.Nil })
	registry.Register(&imageChainProvider{name: "zhipu", model: "cogview-4-250304"})

	var seenModel string
	res, err := ExecuteWithChain(context.Background(),
		[]MediaProviderEntry{{Provider: "zhipu", Enabled: true, Timeout: 5, MaxRetries: 1}},
		registry,
		func(_ context.Context, cp credentialProvider, _, model string, _ map[string]any) ([]byte, *providers.Usage, error) {
			seenModel = model
			if cp == nil {
				t.Error("an API-key provider must reach the call as a credential provider")
			}
			return []byte("png"), nil, nil
		})
	if err != nil {
		t.Fatalf("ExecuteWithChain() error = %v", err)
	}
	if seenModel != "cogview-4-250304" || res.Model != "cogview-4-250304" {
		t.Fatalf("model = %q (result %q), want the provider default when the chain entry has none", seenModel, res.Model)
	}
}
