package audio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const silkPayload = "#!SILK_V3\nfake-silk-bytes"

func TestIsSILK(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"header at offset 0", []byte(silkPayload), true},
		{"wechat style byte prefix", append([]byte{0x01}, []byte(silkPayload)...), true},
		{"wav file is not silk", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), false},
		{"short read is not silk", []byte("#!SI"), false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSILK(tt.data); got != tt.want {
				t.Fatalf("isSILK() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSILKFile(t *testing.T) {
	dir := t.TempDir()
	silkPath := filepath.Join(dir, "voice.silk")
	if err := os.WriteFile(silkPath, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write silk: %v", err)
	}
	wavPath := filepath.Join(dir, "voice.wav")
	if err := os.WriteFile(wavPath, []byte("RIFFxxxxWAVEfmt "), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}

	if !isSILKFile(silkPath) {
		t.Fatal("a SILK file on disk was not recognised")
	}
	if isSILKFile(wavPath) {
		t.Fatal("a WAV file was misdetected as SILK")
	}
	// A missing or unreadable path is a question about the file, not an error to fail on.
	if isSILKFile(filepath.Join(dir, "nope.silk")) {
		t.Fatal("missing file reported as SILK")
	}
}

func TestDerivedWavName(t *testing.T) {
	tests := []struct {
		orig string
		want string
	}{
		{"voice.silk", "voice_silk.wav"},
		{"../../etc/passwd", "etc_passwd.wav"},
		{"语音消息", "语音消息.wav"},
		{"...", "voice.wav"},
		{"", "voice.wav"},
	}
	for _, tt := range tests {
		if got := derivedWavName(tt.orig); got != tt.want {
			t.Fatalf("derivedWavName(%q) = %q, want %q", tt.orig, got, tt.want)
		}
	}
}

func TestLookupSILKDecoderExplicit(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "silk_v3_decoder")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	dec, ok := LookupSILKDecoder(bin)
	if !ok {
		t.Fatal("an existing explicit decoder was refused")
	}
	if dec.Path() != bin {
		t.Fatalf("path = %q, want %q", dec.Path(), bin)
	}
	if got := dec.candidate.args("in.silk", "out.wav"); strings.Join(got, " ") != "in.silk out.wav 24000" {
		t.Fatalf("args = %v, want the silk decoder argv", got)
	}

	ffmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	dec, ok = LookupSILKDecoder(ffmpeg)
	if !ok {
		t.Fatal("an explicitly pinned ffmpeg was refused")
	}
	if got := strings.Join(dec.candidate.args("in", "out"), " "); got != "-y -i in out" {
		t.Fatalf("args = %q, want the ffmpeg form", got)
	}

	if _, ok := LookupSILKDecoder(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("a nonexistent pinned decoder must not report available")
	}
	// "off" is the escape hatch for operators who want the raw bytes to reach the engine.
	if _, ok := LookupSILKDecoder("off"); ok {
		t.Fatal("silk_decoder=off must disable decoding")
	}
}

func newStubDecoder(t *testing.T, run func(ctx context.Context, path string, args ...string) ([]byte, error)) *SILKDecoder {
	t.Helper()
	return &SILKDecoder{path: "/usr/bin/silk_v3_decoder", candidate: silkCandidates[0], run: run}
}

func TestDecodeWAVSuccess(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var calledArgs []string
	dec := newStubDecoder(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calledArgs = args
		// Real decoders write the second argument; the stub does the same.
		return nil, os.WriteFile(args[1], []byte("RIFFxxxxWAVEfmt pcm-ish"), 0o600)
	})

	out, err := dec.DecodeWAV(context.Background(), src)
	if err != nil {
		t.Fatalf("DecodeWAV() error = %v", err)
	}
	t.Cleanup(func() { os.Remove(out) })

	if strings.Join(calledArgs, " ") != src+" "+out+" 24000" {
		t.Fatalf("decoder args = %v, want [in out 24000]", calledArgs)
	}
	if !strings.HasSuffix(out, ".wav") {
		t.Fatalf("output = %q, want a .wav path", out)
	}
}

func TestDecodeWAVRejectsNonSILKBeforeRunningAnything(t *testing.T) {
	src := filepath.Join(t.TempDir(), "plain.wav")
	if err := os.WriteFile(src, []byte("RIFFxxxxWAVEfmt "), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ran := false
	dec := newStubDecoder(t, func(context.Context, string, ...string) ([]byte, error) {
		ran = true
		return nil, errors.New("should not have run")
	})

	if _, err := dec.DecodeWAV(context.Background(), src); err == nil {
		t.Fatal("DecodeWAV() accepted a non-SILK input")
	}
	if ran {
		t.Fatal("a decoder was invoked for a payload that is not SILK")
	}
}

// A decoder that exits 0 but writes nothing would otherwise hand the provider an empty file
// and an unusable error several HTTP calls later.
func TestDecodeWAVRejectsEmptyOutput(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dec := newStubDecoder(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		f, err := os.Create(args[1])
		if err == nil {
			f.Close()
		}
		return []byte("done"), nil
	})

	if _, err := dec.DecodeWAV(context.Background(), src); err == nil {
		t.Fatal("DecodeWAV() accepted an empty decode result")
	}
	// The failed output must not be left behind.
	leftovers, _ := filepath.Glob(src + "*.wav")
	if len(leftovers) > 0 {
		t.Fatalf("empty wav left behind: %v", leftovers)
	}
}

func TestDecodeWAVSurfacesDecoderStderr(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dec := newStubDecoder(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("unsupported sample rate"), errors.New("exit status 1")
	})

	_, err := dec.DecodeWAV(context.Background(), src)
	if err == nil {
		t.Fatal("a failing decoder must return an error")
	}
	if !strings.Contains(err.Error(), "unsupported sample rate") {
		t.Fatalf("error = %v, want it to carry the decoder's own output", err)
	}
}

func TestPrepareSTTInputWithoutDecoderPassesThrough(t *testing.T) {
	m := newTestManager()
	in := STTInput{FilePath: "/tmp/voice.silk", MimeType: "audio/silk"}

	out, created, err := m.prepareSTTInput(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created != "" {
		t.Fatalf("created = %q, want nothing to clean up", created)
	}
	if out.MimeType != "audio/silk" {
		t.Fatalf("mime = %q, want the original", out.MimeType)
	}
}

func TestPrepareSTTInputFilePathBranch(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := newTestManager()
	m.SetSILKDecoder(newStubDecoder(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		return nil, os.WriteFile(args[1], []byte("RIFFxxxxWAVEfmt"), 0o600)
	}))

	out, created, err := m.prepareSTTInput(context.Background(), STTInput{
		FilePath: src, MimeType: "audio/silk", Filename: "voice.silk",
	})
	if err != nil {
		t.Fatalf("prepareSTTInput() error = %v", err)
	}
	if created == "" {
		t.Fatal("the decoded wav must be reported for cleanup")
	}
	defer cleanupSTTInput(created)

	if _, err := os.Stat(created); err != nil {
		t.Fatalf("decoded file missing: %v", err)
	}
	if out.MimeType != "audio/wav" || out.FilePath != created {
		t.Fatalf("input = %+v, want the wav substituted in", out)
	}
	if out.Filename != "voice_silk.wav" {
		t.Fatalf("filename = %q, want the derived wav name", out.Filename)
	}

	cleanupSTTInput(created)
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left %s behind", created)
	}
}

func TestPrepareSTTInputBytesBranchCleansBothFiles(t *testing.T) {
	m := newTestManager()
	var inArg string
	m.SetSILKDecoder(newStubDecoder(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		inArg = args[0]
		return nil, os.WriteFile(args[1], []byte("RIFFxxxxWAVEfmt"), 0o600)
	}))

	out, created, err := m.prepareSTTInput(context.Background(), STTInput{Bytes: []byte(silkPayload), MimeType: "audio/silk"})
	if err != nil {
		t.Fatalf("prepareSTTInput() error = %v", err)
	}
	defer cleanupSTTInput(created)

	if out.Bytes != nil || out.FilePath == "" {
		t.Fatalf("input = %+v, want bytes replaced by the decoded file", out)
	}
	listed := strings.Split(created, "\n")
	if len(listed) != 2 {
		t.Fatalf("created = %q, want both the temp silk and the wav reported", created)
	}
	if inArg != listed[0] {
		t.Fatalf("decoder input = %q, want the temp silk file", inArg)
	}

	cleanupSTTInput(created)
	for _, p := range listed {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived cleanup", p)
		}
	}
}

// Transcribe is what channels call, so the decode has to happen there rather than in a
// helper every channel has to remember.
func TestTranscribeDecodesSILKBeforeCallingProvider(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	capture := &mockSTT{name: "capture", result: &TranscriptResult{Text: "你好"}}
	m := newTestManager()
	m.RegisterSTT(capture)
	m.SetSTTChain([]string{capture.name})
	m.SetSILKDecoder(newStubDecoder(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		return nil, os.WriteFile(args[1], []byte("RIFFxxxxWAVEfmt"), 0o600)
	}))

	res, err := m.Transcribe(context.Background(), STTInput{FilePath: src, MimeType: "audio/silk"}, STTOptions{})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if res.Text != "你好" {
		t.Fatalf("text = %q", res.Text)
	}
	if capture.gotInput.MimeType != "audio/wav" || capture.gotInput.FilePath == src {
		t.Fatalf("provider received %+v, want the decoded wav", capture.gotInput)
	}
}

// With a decoder configured but failing, the audio still reaches the engine: a provider that
// happens to accept SILK should still work, and the failure must be visible in the log path
// rather than turning the message into a silent error.
func TestTranscribeFallsBackToOriginalWhenDecodeFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "voice.silk")
	if err := os.WriteFile(src, []byte(silkPayload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	capture := &mockSTT{name: "capture", result: &TranscriptResult{Text: "ok"}}
	m := newTestManager()
	m.RegisterSTT(capture)
	m.SetSTTChain([]string{capture.name})
	m.SetSILKDecoder(newStubDecoder(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 127")
	}))

	if _, err := m.Transcribe(context.Background(), STTInput{FilePath: src, MimeType: "audio/silk"}, STTOptions{}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if capture.gotInput.FilePath != src {
		t.Fatalf("provider got %+v, want the original silk path after a failed decode", capture.gotInput)
	}
}
