package config

// AudioConfig is the optional cfg.Audio block for STT/Music static defaults.
// Pointer-typed field on Config so absent/empty JSON silently decodes as nil —
// existing config.json files that predate Phase 1 continue to load unchanged.
//
// audio.stt is read by cmd/gateway_stt.go at startup and re-applied after
// system_configs overlay; changes take effect on restart, because the audio
// Manager is built once before the gateway serves any channel.
type AudioConfig struct {
	Stt   *AudioSTTConfig   `json:"stt,omitempty"`
	Music *AudioMusicConfig `json:"music,omitempty"`
}

// AudioSTTConfig configures the global speech-recognition backend. Empty Provider
// means "not configured", which leaves the legacy TTS-derived wiring in charge.
//
// Provider is a protocol, not a service: "openai_compat" reaches any
// POST {base_url}/audio/transcriptions endpoint — SiliconFlow, a self-hosted
// whisper server, or OpenAI itself — so a vendor name belongs in base_url, not here.
// Model is the primary provider's model id; a fallback keeps its own default.
// APIKey is stored in the config_secrets table, never on disk.
type AudioSTTConfig struct {
	Provider  string `json:"provider,omitempty"`   // openai_compat | elevenlabs | proxy
	APIKey    string `json:"api_key,omitempty"`    // stored in config_secrets, masked in config.get
	BaseURL   string `json:"base_url,omitempty"`   // service root, e.g. https://api.siliconflow.cn/v1
	Model     string `json:"model,omitempty"`      // primary provider's model id
	Language  string `json:"language,omitempty"`   // BCP-47 hint, empty = auto-detect
	Fallback  string `json:"fallback,omitempty"`   // provider name to try after the primary
	TimeoutMs int    `json:"timeout_ms,omitempty"` // 0 = provider default
	// SilkDecoder pins the SILK→WAV tool QQ and WeChat voice need ("off" to disable,
	// empty to probe silk_v3_decoder / silk_decoder / ffmpeg on PATH). There is no
	// pure-Go SILK decoder, so this is an external binary the host installs.
	SilkDecoder string `json:"silk_decoder,omitempty"`
}

// ensureAudioSTT returns the STT block, creating the pointers on the way. Callers
// that apply a DB secret or a system_config value need somewhere to write even when
// config.json never mentioned audio at all.
func (c *Config) ensureAudioSTT() *AudioSTTConfig {
	if c.Audio == nil {
		c.Audio = &AudioConfig{}
	}
	if c.Audio.Stt == nil {
		c.Audio.Stt = &AudioSTTConfig{}
	}
	return c.Audio.Stt
}

// AudioMusicConfig configures optional static Music defaults.
type AudioMusicConfig struct {
	Provider  string `json:"provider,omitempty"` // primary provider name (e.g. "elevenlabs")
	APIKey    string `json:"api_key,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Model     string `json:"model,omitempty"`
	Fallback  string `json:"fallback,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}
