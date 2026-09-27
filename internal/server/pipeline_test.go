package server

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ybouhjira/claude-code-tts/internal/tts"
)

// fakeProvider synthesizes "audio" whose latency varies with the text so
// completion order differs from submission order.
type fakeProvider struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeProvider) Name() string                    { return "fake" }
func (f *fakeProvider) DefaultVoice() string            { return "v" }
func (f *fakeProvider) Voices() []string                { return []string{"v"} }
func (f *fakeProvider) IsValidVoice(v string) bool      { return v == "v" }
func (f *fakeProvider) SynthesizeAudio(text, voice string) (tts.AudioResult, error) {
	// Later submissions finish first: job N sleeps (10-N) ms.
	var n int
	fmt.Sscanf(text, "job%d", &n)
	time.Sleep(time.Duration(10-n) * time.Millisecond)
	f.mu.Lock()
	f.calls = append(f.calls, text)
	f.mu.Unlock()
	return tts.AudioResult{Data: []byte(text), Format: tts.FormatWAV}, nil
}

// fakePlayer records playback order and simulates playback duration.
type fakePlayer struct {
	mu       sync.Mutex
	played   []string
	playing  bool
	playFor  time.Duration
	stopHook func()
}

func (f *fakePlayer) Play(b []byte) error {
	if f.stopHook != nil {
		f.stopHook()
	}
	f.mu.Lock()
	f.playing = true
	f.mu.Unlock()
	time.Sleep(f.playFor)
	f.mu.Lock()
	f.played = append(f.played, string(b))
	f.playing = false
	f.mu.Unlock()
	return nil
}
func (f *fakePlayer) IsPlaying() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.playing
}
func (f *fakePlayer) Stop() {}
func (f *fakePlayer) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.played...)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// Fragments must play strictly in submission order even when synthesis
// completes out of order (later jobs finish first here).
func TestPipeline_PlaysInSubmissionOrder(t *testing.T) {
	prov := &fakeProvider{}
	player := &fakePlayer{playFor: 5 * time.Millisecond}
	p := newPipeline(prov, time.Millisecond, player)
	p.Start(4, 50)
	defer p.Close()

	for i := 1; i <= 6; i++ {
		if _, err := p.Submit(fmt.Sprintf("job%d", i), "test", ""); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	waitFor(t, 3*time.Second, func() bool { return len(player.snapshot()) == 6 })

	want := []string{"job1", "job2", "job3", "job4", "job5", "job6"}
	got := player.snapshot()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("play order: got %v, want %v", got, want)
		}
	}
	if s := p.Status(); s.Processed != 6 {
		t.Fatalf("processed = %d, want 6", s.Processed)
	}
}

// Clear must drop queued fragments without playing them.
func TestPipeline_ClearDropsQueued(t *testing.T) {
	prov := &fakeProvider{}
	player := &fakePlayer{playFor: 30 * time.Millisecond}
	p := newPipeline(prov, time.Millisecond, player)
	p.Start(1, 50) // one synth worker, so jobs 2+ pile up behind job1
	defer p.Close()

	for i := 1; i <= 4; i++ {
		if _, err := p.Submit(fmt.Sprintf("job%d", i), "test", ""); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	// Let job1 start playing, then clear the rest.
	waitFor(t, 3*time.Second, func() bool { return player.IsPlaying() })
	p.Clear()
	waitFor(t, 3*time.Second, func() bool { return len(player.snapshot()) >= 1 })
	time.Sleep(100 * time.Millisecond)

	got := player.snapshot()
	if len(got) > 2 { // job1 may already have started; nothing after it
		t.Fatalf("played after clear: %v", got)
	}
	if s := p.Status(); s.Skipped == 0 {
		t.Fatalf("expected skipped > 0, got %+v", s)
	}
}
