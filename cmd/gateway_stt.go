package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/audio"
	"github.com/nextlevelbuilder/goclaw/internal/audio/elevenlabs"
	"github.com/nextlevelbuilder/goclaw/internal/audio/openaicompat"
	"github.com/nextlevelbuilder/goclaw/internal/audio/proxy_stt"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// The closed set accepted by audio.stt.provider. Names like "siliconflow" are
// services, not protocols: they are reached through openai_compat by pointing
// base_url at them, so they are deliberately not listed here.
const (
	sttProviderOpenAICompat = "openai_compat"
	sttProviderElevenLabs   = "elevenlabs"
	sttProviderProxy        = "proxy"
)

// registerSTTProviders builds the speech-recognition chain.
//
// audio.stt is the supported place to configure it. When the block is absent or
// names no provider, the historical sources are used instead — Tts.OpenAICompat for
// an OpenAI-compatible endpoint plus the ElevenLabs TTS key — so a deployment that
// already transcribed voice keeps transcribing after an upgrade without anyone
// reading release notes.
func registerSTTProviders(cfg *config.Config, mgr *audio.Manager, ellKey, ellBase string) {
	var st *config.AudioSTTConfig
	if cfg.Audio != nil {
		st = cfg.Audio.Stt
	}
	if st == nil || strings.TrimSpace(st.Provider) == "" {
		// A fallback with no primary is an operator mistake that would otherwise look
		// like "STT configured" while the legacy wiring decides everything.
		if st != nil && strings.TrimSpace(st.Fallback) != "" {
			slog.Warn("audio.stt: fallback ignored without a provider — set audio.stt.provider",
				"fallback", strings.TrimSpace(st.Fallback))
		}
		registerLegacySTTProviders(cfg, mgr, ellKey, ellBase)
		return
	}

	chain := make([]string, 0, 2)
	for _, name := range sttChainNames(st) {
		p, err := newSTTProvider(name, st, ellKey, ellBase)
		if err != nil {
			slog.Warn("audio.stt: provider not registered", "provider", name, "error", err)
			continue
		}
		mgr.RegisterSTT(p)
		// Role is decided by position, and it must be read *before* this entry lands in
		// the chain: asking afterwards always answers "primary" and the fallback never
		// appears in the log at all.
		role := "primary"
		if len(chain) > 0 {
			role = "fallback"
		}
		chain = append(chain, p.Name())
		slog.Info("audio.stt: provider registered", "provider", p.Name(), "role", role)
	}

	if len(chain) == 0 {
		// Loud on purpose: this is the state where a gateway accepts voice messages,
		// downloads them, and then has nothing to transcribe them with.
		slog.Warn("audio.stt: audio.stt.provider is set but nothing registered — voice messages will not be transcribed",
			"provider", st.Provider, "fallback", st.Fallback)
		return
	}

	// The configured model belongs to the primary provider only: a fallback keeps its
	// own default, because the model id of one engine means nothing to another.
	defaults := audio.STTOptions{Language: st.Language, TimeoutMs: st.TimeoutMs}
	if chain[0] == sttProviderElevenLabs && st.Model != "" {
		defaults.ModelID = st.Model
	}
	mgr.SetSTTDefaults(defaults)

	mgr.SetSTTChain(chain)
	slog.Info("audio.stt: chain configured", "chain", chain, "language", st.Language, "timeout_ms", st.TimeoutMs)
}

// attachSILKDecoder resolves the external SILK→WAV tool that QQ and WeChat voice need.
//
// There is no pure-Go SILK decoder, so this is a tool the host installs. Without one the
// payload reaches the engine unchanged and the provider's own rejection is the answer —
// which is why the outcome is logged rather than left for somebody to discover from a
// silent [语音] placeholder three channels later.
func attachSILKDecoder(cfg *config.Config, mgr *audio.Manager) {
	explicit := ""
	if cfg.Audio != nil && cfg.Audio.Stt != nil {
		explicit = cfg.Audio.Stt.SilkDecoder
	}
	dec, ok := audio.LookupSILKDecoder(explicit)
	if !ok {
		if explicit != "" {
			slog.Warn("audio.stt: configured silk decoder unavailable — QQ/WeChat voice will not be decoded",
				"configured", explicit)
		} else {
			slog.Info("audio.stt: no silk decoder on PATH — QQ/WeChat voice stays undecoded " +
				"(install silk_v3_decoder, or ffmpeg for non-SILK payloads)")
		}
		return
	}
	mgr.SetSILKDecoder(dec)
	slog.Info("audio.stt: silk decoder available", "binary", dec.Path())
}

// sttChainNames returns the primary then the fallback, without repeats or empties.
// With no primary there is no chain: a fallback alone is not a backend order.
func sttChainNames(st *config.AudioSTTConfig) []string {
	primary := strings.TrimSpace(st.Provider)
	if primary == "" {
		return nil
	}
	names := []string{primary}
	if f := strings.TrimSpace(st.Fallback); f != "" && f != primary {
		names = append(names, f)
	}
	return names
}

func newSTTProvider(name string, st *config.AudioSTTConfig, ellKey, ellBase string) (audio.STTProvider, error) {
	switch name {
	case sttProviderOpenAICompat:
		// NewSTTProvider rejects an empty base: the endpoint is the whole configuration.
		return openaicompat.NewSTTProvider(openaicompat.Config{
			APIBase:   st.BaseURL,
			APIKey:    st.APIKey,
			STTModel:  st.Model,
			TimeoutMs: st.TimeoutMs,
		})

	case sttProviderElevenLabs:
		key, base := st.APIKey, st.BaseURL
		if key == "" {
			key = ellKey // an ElevenLabs account already configured for TTS needs no second key
		}
		if key == "" {
			return nil, errors.New("no API key (set audio.stt.api_key or tts.elevenlabs.api_key)")
		}
		if base == "" {
			base = ellBase
		}
		return elevenlabs.NewSTTProvider(elevenlabs.Config{APIKey: key, BaseURL: base}), nil

	case sttProviderProxy:
		if st.BaseURL == "" {
			return nil, errors.New("proxy needs audio.stt.base_url as the proxy endpoint")
		}
		return proxy_stt.NewProvider(media.STTConfig{
			ProxyURL:       st.BaseURL,
			APIKey:         st.APIKey,
			TenantID:       "",
			TimeoutSeconds: sttTimeoutSeconds(st.TimeoutMs),
		}), nil

	default:
		return nil, fmt.Errorf("unknown audio.stt.provider %q (supported: %s, %s, %s)",
			name, sttProviderOpenAICompat, sttProviderElevenLabs, sttProviderProxy)
	}
}

// sttTimeoutSeconds rounds milliseconds up so a sub-second timeout cannot become 0,
// which the proxy provider would read as "no timeout".
func sttTimeoutSeconds(ms int) int {
	if ms <= 0 {
		return 0 // provider default
	}
	return (ms + 999) / 1000
}

// registerLegacySTTProviders is the pre-audio.stt wiring: an OpenAI-compatible
// endpoint borrowed from the TTS block, and ElevenLabs STT on the TTS key.
//
// It no longer appends a bare "proxy" name. That entry was never registered
// globally — the legacy proxy is registered per channel as "proxy:telegram" by
// BridgeLegacySTT and reached through the channel context — so keeping it here only
// produced a warning on every voice message.
func registerLegacySTTProviders(cfg *config.Config, mgr *audio.Manager, ellKey, ellBase string) {
	var sttChain []string

	if base := cfg.Tts.OpenAICompat.APIBase; base != "" {
		provider, err := openaicompat.NewSTTProvider(openaicompat.Config{
			APIBase:   base,
			APIKey:    cfg.Tts.OpenAICompat.APIKey,
			STTModel:  cfg.Tts.OpenAICompat.STTModel,
			TimeoutMs: cfg.Tts.TimeoutMs,
		})
		if err != nil {
			slog.Warn("audio.stt: openai_compat not registered", "error", err)
		} else {
			mgr.RegisterSTT(provider)
			sttChain = append(sttChain, provider.Name())
			slog.Info("audio.stt: openai_compat registered", "api_base", base)
		}
	}

	if ellKey != "" {
		mgr.RegisterSTT(elevenlabs.NewSTTProvider(elevenlabs.Config{
			APIKey:  ellKey,
			BaseURL: ellBase,
		}))
		sttChain = append(sttChain, "elevenlabs")
		slog.Info("audio.stt: elevenlabs registered")
	}

	if len(sttChain) > 0 {
		mgr.SetSTTChain(sttChain)
		slog.Info("audio.stt: chain configured", "chain", sttChain)
	}
}
