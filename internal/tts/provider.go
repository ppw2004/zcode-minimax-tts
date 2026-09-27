package tts

import (
	"fmt"
	"os"
)

// AudioFormat identifies the container format of synthesized audio.
type AudioFormat string

const (
	FormatMP3 AudioFormat = "mp3"
	FormatWAV AudioFormat = "wav"
)

// AudioResult is the output of a synthesis call.
type AudioResult struct {
	Data   []byte
	Format AudioFormat
}

// Provider is a pluggable TTS backend.
type Provider interface {
	// Name is a short identifier, e.g. "openai" or "minimax".
	Name() string
	// DefaultVoice is used when no voice is specified.
	DefaultVoice() string
	// Voices lists suggested voices for this provider (not necessarily exhaustive).
	Voices() []string
	// IsValidVoice reports whether the voice id is usable by this provider.
	IsValidVoice(v string) bool
	// SynthesizeAudio converts text to speech.
	SynthesizeAudio(text, voice string) (AudioResult, error)
}

// NewProvider returns the TTS provider selected via the TTS_PROVIDER
// environment variable ("openai" or "minimax"). When unset, it auto-detects
// from whichever API key is configured, preferring OpenAI for backwards
// compatibility.
func NewProvider() (Provider, error) {
	switch os.Getenv("TTS_PROVIDER") {
	case "minimax":
		return NewMiniMaxProvider(), nil
	case "openai":
		return NewClient(), nil
	case "":
		if os.Getenv("MINIMAX_API_KEY") != "" && os.Getenv("OPENAI_API_KEY") == "" {
			return NewMiniMaxProvider(), nil
		}
		return NewClient(), nil
	default:
		return nil, fmt.Errorf("unknown TTS_PROVIDER %q (expected \"openai\" or \"minimax\")", os.Getenv("TTS_PROVIDER"))
	}
}
