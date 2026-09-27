package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ybouhjira/claude-code-tts/internal/audio"
	"github.com/ybouhjira/claude-code-tts/internal/tts"
)

func main() {
	// Parse flags
	voice := flag.String("voice", "", fmt.Sprintf("Voice to use (provider-specific; default: %s for OpenAI, %s for MiniMax)", "alloy", "female-shaonv"))
	providerName := flag.String("provider", "", "TTS provider to use: openai or minimax (overrides TTS_PROVIDER env)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] TEXT\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Converts text to speech and plays it. Provider is selected via TTS_PROVIDER\n")
		fmt.Fprintf(os.Stderr, "(or auto-detected from OPENAI_API_KEY / MINIMAX_API_KEY).\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExample:\n")
		fmt.Fprintf(os.Stderr, "  %s \"Build completed\"\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  TTS_PROVIDER=minimax MINIMAX_API_KEY=... %s -voice presenter_female \"任务完成\"\n", os.Args[0])
	}
	flag.Parse()

	// Check for text argument
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}

	text := flag.Arg(0)

	// -provider flag overrides the environment
	if *providerName != "" {
		os.Setenv("TTS_PROVIDER", *providerName)
	}

	provider, err := tts.NewProvider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Validate environment for the selected provider
	switch provider.Name() {
	case "minimax":
		if os.Getenv("MINIMAX_API_KEY") == "" {
			fmt.Fprintf(os.Stderr, "Error: MINIMAX_API_KEY environment variable is required\n")
			os.Exit(1)
		}
	default:
		if os.Getenv("OPENAI_API_KEY") == "" {
			fmt.Fprintf(os.Stderr, "Error: OPENAI_API_KEY environment variable is required\n")
			os.Exit(1)
		}
	}

	// Default voice comes from the provider
	if *voice == "" {
		*voice = provider.DefaultVoice()
	}

	// Validate voice
	if !provider.IsValidVoice(*voice) {
		fmt.Fprintf(os.Stderr, "Error: invalid voice '%s'. Valid voices: %s\n", *voice, strings.Join(provider.Voices(), ", "))
		os.Exit(1)
	}

	// Synthesize speech
	result, err := provider.SynthesizeAudio(text, *voice)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error synthesizing speech: %v\n", err)
		os.Exit(1)
	}

	// Play audio
	player := audio.NewPlayer()
	if err := player.Play(result.Data); err != nil {
		fmt.Fprintf(os.Stderr, "Error playing audio: %v\n", err)
		os.Exit(1)
	}
}
