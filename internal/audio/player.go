package audio

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
)

// Player handles audio playback with mutex protection
type Player struct {
	mu        sync.Mutex // serializes Play calls
	isPlaying atomic.Bool

	// cmdMu guards current/stopRequested so Stop() can kill the playback
	// subprocess without waiting for Play()'s mutex to release.
	cmdMu         sync.Mutex
	current       *exec.Cmd
	stopRequested bool
}

// NewPlayer creates a new audio player
func NewPlayer() *Player {
	return &Player{}
}

// detectFormat sniffs the container from the audio header: RIFF....WAVE is a
// WAV file; ID3 or an MPEG frame sync is MP3.
func detectFormat(data []byte) string {
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return "wav"
	}
	if len(data) >= 3 && string(data[0:3]) == "ID3" {
		return "mp3"
	}
	if len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0 {
		return "mp3"
	}
	// Legacy default: the OpenAI provider returns raw MP3.
	return "mp3"
}

// hasMP3FrameSync reports whether the data plausibly contains an MP3 stream:
// an ID3v2 tag near the start, or an MPEG frame sync (0xFF Ex) within the
// first 64KB. Used to fail fast on garbage input before spawning a player.
func hasMP3FrameSync(data []byte) bool {
	if len(data) >= 3 && string(data[0:3]) == "ID3" {
		return true
	}
	limit := len(data)
	if limit > 65536 {
		limit = 65536
	}
	for i := 0; i+1 < limit; i++ {
		if data[i] == 0xFF && data[i+1]&0xE0 == 0xE0 {
			return true
		}
	}
	return false
}

// Play plays the given audio data.
// Only one audio can play at a time (mutex protected)
func (p *Player) Play(audioData []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.isPlaying.Store(true)
	defer func() { p.isPlaying.Store(false) }()

	format := detectFormat(audioData)
	if format == "mp3" && !hasMP3FrameSync(audioData) {
		return fmt.Errorf("audio data is not valid MP3")
	}

	// Create temporary file
	tmpFile, err := os.CreateTemp("", "tts-*."+format)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(audioData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write audio data: %w", err)
	}
	tmpFile.Close()

	cmd, err := playbackCommand(tmpFile.Name(), format)
	if err != nil {
		return err
	}
	hideConsole(cmd)

	p.setCmd(cmd)
	runErr := cmd.Run()
	p.setCmd(nil)

	if runErr != nil {
		p.cmdMu.Lock()
		killed := p.stopRequested
		p.stopRequested = false
		p.cmdMu.Unlock()
		if killed {
			// Playback was cut short by Stop(); that is a requested
			// skip, not a failure.
			return nil
		}
		return fmt.Errorf("audio playback failed: %w", runErr)
	}

	return nil
}

func (p *Player) setCmd(cmd *exec.Cmd) {
	p.cmdMu.Lock()
	p.current = cmd
	p.cmdMu.Unlock()
}

// Stop kills the in-flight playback subprocess, if any. Play() returns
// shortly after without reporting an error.
func (p *Player) Stop() {
	p.cmdMu.Lock()
	defer p.cmdMu.Unlock()
	if p.current != nil && p.current.Process != nil {
		p.stopRequested = true
		_ = p.current.Process.Kill()
	}
}

// playbackCommand builds the platform-specific playback command.
func playbackCommand(path, format string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		// afplay handles both WAV and MP3.
		return exec.Command("afplay", path), nil
	case "linux":
		if format == "wav" {
			if _, err := exec.LookPath("paplay"); err == nil {
				return exec.Command("paplay", path), nil
			}
			if _, err := exec.LookPath("aplay"); err == nil {
				return exec.Command("aplay", "-q", path), nil
			}
		}
		if _, err := exec.LookPath("mpv"); err == nil {
			return exec.Command("mpv", "--no-video", "--really-quiet", path), nil
		}
		if _, err := exec.LookPath("ffplay"); err == nil {
			return exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", path), nil
		}
		if _, err := exec.LookPath("mpg123"); err == nil {
			return exec.Command("mpg123", "-q", path), nil
		}
		return nil, fmt.Errorf("no suitable audio player found on Linux (install mpv, ffplay, mpg123, paplay or aplay)")
	case "windows":
		if format == "wav" {
			// SoundPlayer plays WAV synchronously.
			return exec.Command("powershell", "-NoProfile", "-Command",
				fmt.Sprintf(`(New-Object Media.SoundPlayer '%s').PlaySync()`, path)), nil
		}
		// MP3: SoundPlayer is WAV-only, so fall back to Windows Media Player.
		// Wait based on the media duration rather than polling play state so
		// that unplayable files exit quickly.
		return exec.Command("powershell", "-NoProfile", "-Command", fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$p = New-Object -ComObject WMPlayer.OCX.7
$p.URL = '%s'
$deadline = (Get-Date).AddSeconds(3)
while ($null -eq $p.currentMedia -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 50 }
$dur = 0
if ($null -ne $p.currentMedia) {
    $wait = (Get-Date).AddSeconds(3)
    while ($p.currentMedia.duration -le 0 -and (Get-Date) -lt $wait) { Start-Sleep -Milliseconds 50 }
    $dur = $p.currentMedia.duration
}
if ($dur -gt 0) {
    $p.controls.play()
    Start-Sleep -Milliseconds ([int]($dur * 1000) + 500)
} else {
    exit 1
}
`, path)), nil
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// IsPlaying returns whether audio is currently playing. It never blocks,
// even while Play() is running.
func (p *Player) IsPlaying() bool {
	return p.isPlaying.Load()
}
