package audio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrAllSTTProvidersFailed is returned when every provider in the chain fails.
var ErrAllSTTProvidersFailed = errors.New("all STT providers failed")

// defaultSTTChain is the built-in fallback order when no explicit chain is set.
var defaultSTTChain = []string{"elevenlabs", "proxy"}

// Transcribe tries providers in chain order. Returns first success.
// Wraps last error with ErrAllSTTProvidersFailed on total failure.
func (m *Manager) Transcribe(ctx context.Context, in STTInput, opts STTOptions) (*TranscriptResult, error) {
	chain := m.resolveSTTChain(ctx)
	if len(chain) == 0 {
		return nil, fmt.Errorf("%w: chain is empty", ErrAllSTTProvidersFailed)
	}

	opts = m.mergeSTTOptions(opts)

	// QQ and WeChat deliver voice as SILK, which no transcription engine reads. Decode it
	// once here so no channel has to know the format exists; with no decoder installed the
	// payload is passed through and the engine's own rejection is the answer.
	in, created, prepErr := m.prepareSTTInput(ctx, in)
	if created != "" {
		defer cleanupSTTInput(created)
	}
	if prepErr != nil {
		slog.Warn("audio.stt: silk transcode failed, passing the original payload", "error", prepErr)
	}

	var lastErr error
	for _, name := range chain {
		p, ok := m.sttProviders[name]
		if !ok {
			slog.Warn("audio.stt provider not registered, skipping", "provider", name)
			continue
		}
		res, err := p.Transcribe(ctx, in, opts)
		if err == nil {
			return res, nil
		}
		slog.Warn("audio.stt provider failed", "provider", name, "error", err)
		lastErr = err
	}

	if lastErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrAllSTTProvidersFailed, lastErr)
	}
	return nil, fmt.Errorf("%w: no providers matched in chain %v", ErrAllSTTProvidersFailed, chain)
}

// SetSTTChain sets an explicit provider order for STT dispatch.
// Call before first use; not thread-safe after concurrent Transcribe calls.
func (m *Manager) SetSTTChain(chain []string) {
	m.sttChain = chain
}

// SetSTTDefaults supplies the per-call STT options channels do not know about —
// the model ID and the language hint from config. Channels pass an empty
// STTOptions (they are transcribing whatever the peer sent), and a provider's
// own default is a built-in constant, so without this there is nowhere to put
// "use SenseVoiceSmall" or "the audio is Vietnamese".
//
// Only zero values are filled in: an explicit per-call option still wins.
func (m *Manager) SetSTTDefaults(opts STTOptions) {
	m.sttDefaults = opts
}

// mergeSTTOptions applies the configured defaults to a per-call option set.
func (m *Manager) mergeSTTOptions(opts STTOptions) STTOptions {
	if opts.Language == "" {
		opts.Language = m.sttDefaults.Language
	}
	if opts.ModelID == "" {
		opts.ModelID = m.sttDefaults.ModelID
	}
	if opts.TimeoutMs == 0 {
		opts.TimeoutMs = m.sttDefaults.TimeoutMs
	}
	return opts
}

// STTTimeout returns the deadline a channel should give one transcription call: the operator's
// audio.stt.timeout_ms when it is set, otherwise the channel's own default.
//
// Channels used to hard-code their number, which made timeout_ms above it a silent no-op: an
// operator raising the timeout to 30s for a long voice note still had the call cut at 10s, and
// the SILK decoder's own 15s ceiling could never be reached. A channel that wraps this call in a
// wider budget (QQ and WeChat bound their whole media step) is still the outer limit.
func (m *Manager) STTTimeout(fallback time.Duration) time.Duration {
	if m != nil && m.sttDefaults.TimeoutMs > 0 {
		return time.Duration(m.sttDefaults.TimeoutMs) * time.Millisecond
	}
	return fallback
}

// RegisterChannelSTT registers STT providers scoped to a specific channel name
// (e.g. "telegram"). Channel-scoped providers take precedence over the manager
// default chain when resolveSTTChain detects a matching channel in ctx.
func (m *Manager) RegisterChannelSTT(channelName string, providers ...STTProvider) {
	if m.channelSTTOverrides == nil {
		m.channelSTTOverrides = make(map[string][]string)
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		m.sttProviders[p.Name()+":"+channelName] = p
		names = append(names, p.Name()+":"+channelName)
	}
	m.channelSTTOverrides[channelName] = names
}

// resolveSTTChain returns the ordered provider names for the current call.
// Precedence: (1) channel override from ctx, (2) explicit m.sttChain,
// (3) defaultSTTChain.
//
// Every source is filtered against the registry. An explicit chain may name a
// provider that exists only for some channels ("proxy" is registered per channel,
// as "proxy:telegram"), and walking an unregistered name on every voice message
// turned a configuration gap into an unbounded stream of warnings. Misconfiguration
// is reported once at startup by whoever built the chain, not per call.
func (m *Manager) resolveSTTChain(ctx context.Context) []string {
	// (1) Channel override: check if ctx carries a channel name.
	if ch := channelFromCtx(ctx); ch != "" {
		if overrides, ok := m.channelSTTOverrides[ch]; ok {
			if filtered := m.registered(overrides); len(filtered) > 0 {
				return filtered
			}
		}
	}
	// (2) Explicit chain set via SetSTTChain.
	if len(m.sttChain) > 0 {
		return m.registered(m.sttChain)
	}
	// (3) Default: elevenlabs → proxy, filtered to what's registered.
	return m.registered(defaultSTTChain)
}

// registered keeps only the names present in the provider registry, preserving order.
func (m *Manager) registered(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := m.sttProviders[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// STTChain returns the providers a call carrying this context would try, in order.
// It is exactly what Transcribe resolves, so a status surface can report the backend
// actually in effect instead of what config claims.
func (m *Manager) STTChain(ctx context.Context) []string {
	return m.resolveSTTChain(ctx)
}

// STTAvailable reports whether this call — identified by the channel carried in ctx —
// has any provider that could transcribe it. Channels use it to skip both the audio
// download and the failure noise when no speech backend exists, which is the state of
// a gateway that never configured one.
func (m *Manager) STTAvailable(ctx context.Context) bool {
	return len(m.resolveSTTChain(ctx)) > 0
}
