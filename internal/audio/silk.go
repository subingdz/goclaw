package audio

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// WeChat and QQ deliver voice as SILK v3. No transcription engine accepts it — the format
// is Skype's codec with a vendor header — and there is no maintained pure-Go decoder, only
// the C SDK wrapped by command-line binaries. So decoding stays what it has always been in
// this ecosystem: an external tool, used if the operator installed one.
const (
	silkMagic = "#!SILK_V3"

	// silkHeaderScan bounds the header search. WeChat prefixes the payload with a byte
	// before the magic on some files, so an offset-0 comparison misses real SILK.
	silkHeaderScan = 16

	// silkDecodeTimeout matches the reference implementation: a stuck decoder must not
	// hold a channel's inbound goroutine.
	silkDecodeTimeout = 15 * time.Second

	// silkMaxOutputBytes bounds what we read back from a decoder. A runaway or malicious
	// tool must not fill memory on a 2 GB board.
	silkMaxOutputBytes = 64 << 20
)

// silkCandidate is one decoder and the argv shape it expects.
type silkCandidate struct {
	bin  string
	args func(in, out string) []string
}

// silkCandidates is the probe order, and each entry is a tool the operator installs, not
// something GoClaw builds. ffmpeg is last and does not actually work here: it cannot decode
// SILK, and DecodeWAV is only ever reached for a payload that sniffs as SILK, so the "mislabeled
// regular format" role this list used to claim is not reachable. It is kept only so an operator
// who pinned ffmpeg explicitly is not silently ignored; a host with ffmpeg and no silk decoder
// will log "decoder available" and then fail every voice note.
var silkCandidates = []silkCandidate{
	{bin: "silk_v3_decoder", args: func(in, out string) []string { return []string{in, out, "24000"} }},
	{bin: "silk_decoder", args: func(in, out string) []string { return []string{in, out, "24000"} }},
	{bin: "ffmpeg", args: func(in, out string) []string { return []string{"-y", "-i", in, out} }},
}

// SILKDecoder resolves an external SILK-to-WAV converter.
type SILKDecoder struct {
	// path is the resolved binary; candidate is its argv shape.
	path      string
	candidate silkCandidate
	// run executes the command; replaced in tests so no binary is required.
	run func(ctx context.Context, path string, args ...string) ([]byte, error)
}

// isSILK reports whether the payload carries the SILK v3 header.
func isSILK(data []byte) bool {
	limit := min(len(data), silkHeaderScan)
	return bytes.Contains(data[:limit], []byte(silkMagic))
}

// isSILKFile sniffs a file on disk. A short or unreadable file is not SILK; that is a
// question about the file, not an error worth failing a message over.
func isSILKFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	head := make([]byte, silkHeaderScan)
	n, err := f.Read(head)
	if err != nil || n == 0 {
		return false
	}
	return isSILK(head[:n])
}

// LookupSILKDecoder finds a decoder. An explicit path is used as given (and is the right
// choice when the binary lives outside PATH); otherwise the candidate list is probed in
// order. ok=false means no speech tool is installed, which is a normal state, not an error.
//
// "off" and "none" stop the search: SILK payloads then reach the engine unchanged, which is
// what an operator wants if they are debugging what the provider actually received.
func LookupSILKDecoder(explicit string) (dec *SILKDecoder, ok bool) {
	if v := strings.ToLower(strings.TrimSpace(explicit)); v == "off" || v == "none" {
		return nil, false
	}
	lookPath := exec.LookPath
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			slog.Warn("audio.stt: configured silk decoder not found", "path", explicit, "error", err)
			return nil, false
		}
		// A pinned binary gets the silk argv shape: it is a silk decoder by definition,
		// unless it is literally ffmpeg, which needs its own flags.
		cand := silkCandidates[0]
		if strings.EqualFold(filepath.Base(explicit), "ffmpeg") {
			cand = silkCandidates[2]
		}
		return &SILKDecoder{path: explicit, candidate: cand, run: runCommand}, true
	}

	for _, c := range silkCandidates {
		if p, err := lookPath(c.bin); err == nil {
			return &SILKDecoder{path: p, candidate: c, run: runCommand}, true
		}
	}
	return nil, false
}

func runCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
}

// Path reports the resolved binary, for the startup log and any later status surface.
func (d *SILKDecoder) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// DecodeWAV converts a SILK file to WAV and returns the path of the temporary result.
// The caller owns that file and must remove it.
func (d *SILKDecoder) DecodeWAV(ctx context.Context, srcPath string) (string, error) {
	if d == nil {
		return "", fmt.Errorf("audio.stt: no silk decoder configured")
	}
	in, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("audio.stt: read silk: %w", err)
	}
	if !isSILK(in) {
		return "", fmt.Errorf("audio.stt: %s is not a SILK v3 payload", filepath.Base(srcPath))
	}

	outPath, err := os.CreateTemp("", "goclaw-silk-*.wav")
	if err != nil {
		return "", fmt.Errorf("audio.stt: create wav temp: %w", err)
	}
	out := outPath.Name()
	_ = outPath.Close()

	runCtx, cancel := context.WithTimeout(ctx, silkDecodeTimeout)
	defer cancel()
	combined, runErr := d.run(runCtx, d.path, d.candidate.args(srcPath, out)...)
	if runErr != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("audio.stt: %s failed: %w (%s)",
			filepath.Base(d.path), runErr, clipTail(combined, 200))
	}

	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		_ = os.Remove(out)
		return "", fmt.Errorf("audio.stt: %s produced no output", filepath.Base(d.path))
	}
	if info.Size() > silkMaxOutputBytes {
		_ = os.Remove(out)
		return "", fmt.Errorf("audio.stt: decoded wav too large (%d bytes, max %d)", info.Size(), silkMaxOutputBytes)
	}
	return out, nil
}

func clipTail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// derivedWavName names the decoded file after the platform's original. The name arrives
// from a remote message, so it is filtered rather than trusted: separators, dots and
// control bytes cannot survive into a multipart filename.
func derivedWavName(orig string) string {
	var b strings.Builder
	for _, r := range orig {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r > 0x7f:
			b.WriteRune(r) // vendor filenames are routinely Chinese
		default:
			b.WriteRune('_')
		}
	}
	stem := strings.Trim(b.String(), "_")
	if stem == "" {
		return "voice.wav"
	}
	return stem + ".wav"
}

// SetSILKDecoder installs the converter used before any provider is called. Without it,
// SILK audio is passed through untouched and the engine decides what to do with it.
func (m *Manager) SetSILKDecoder(d *SILKDecoder) {
	m.silk = d
}

// prepareSTTInput transcodes SILK into a WAV the providers can actually read. It returns
// the path this call created (empty when nothing was created) so the caller can clean up.
func (m *Manager) prepareSTTInput(ctx context.Context, in STTInput) (STTInput, string, error) {
	if m.silk == nil {
		return in, "", nil
	}

	switch {
	case in.FilePath != "":
		if !isSILKFile(in.FilePath) {
			return in, "", nil
		}
		wav, err := m.silk.DecodeWAV(ctx, in.FilePath)
		if err != nil {
			return in, "", err
		}
		out := in
		out.FilePath, out.MimeType = wav, "audio/wav"
		// The decoded file carries no useful original name; keep the extension honest
		// instead of sending a .silk name with wav bytes.
		if in.Filename != "" {
			out.Filename = derivedWavName(in.Filename)
		}
		return out, wav, nil

	case len(in.Bytes) > 0:
		if !isSILK(in.Bytes) {
			return in, "", nil
		}
		tmp, err := os.CreateTemp("", "goclaw-silk-in-*.silk")
		if err != nil {
			return in, "", fmt.Errorf("audio.stt: create silk temp: %w", err)
		}
		src := tmp.Name()
		// Two files are created here; both are reported so the caller can remove each.
		if _, err := tmp.Write(in.Bytes); err != nil {
			tmp.Close()
			os.Remove(src)
			return in, "", fmt.Errorf("audio.stt: write silk temp: %w", err)
		}
		_ = tmp.Close()

		wav, decErr := m.silk.DecodeWAV(ctx, src)
		combined := src
		if wav != "" {
			combined = src + "\n" + wav
		}
		os.Remove(src)
		if decErr != nil {
			return in, combined, decErr
		}
		out := in
		out.Bytes, out.FilePath, out.MimeType = nil, wav, "audio/wav"
		return out, combined, nil
	}

	return in, "", nil
}

// cleanupSTTInput removes the files prepareSTTInput created. One path, or the newline pair
// the bytes branch reports.
func cleanupSTTInput(created string) {
	for _, p := range strings.Split(created, "\n") {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}
