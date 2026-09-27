package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ybouhjira/claude-code-tts/internal/audio"
	"github.com/ybouhjira/claude-code-tts/internal/logging"
	"github.com/ybouhjira/claude-code-tts/internal/tts"
)

// PipeJob is one speech fragment flowing through the pipeline.
type PipeJob struct {
	Seq       int64     `json:"seq"`
	Text      string    `json:"text"`
	Voice     tts.Voice `json:"voice"`
	Source    string    `json:"source"` // "api" or "rollout:<sessionId>"
	RequestID string    `json:"request_id,omitempty"`
	Gen       int64     `json:"gen"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"` // pending, playing, completed, failed, skipped
}

type pipeResult struct {
	job  *PipeJob
	data []byte
	err  error
}

// playback is the subset of audio.Player the pipeline needs; it exists so
// tests can inject a recorder instead of spawning a real player.
type playback interface {
	Play([]byte) error
	IsPlaying() bool
	Stop()
}

// Pipeline synthesizes jobs with parallel workers but plays them strictly in
// submission order, with a configurable pause between fragments. While one
// fragment plays, the next ones are already being synthesized (prefetch), so
// the inter-fragment gap stays small and constant.
type Pipeline struct {
	provider tts.Provider
	player   playback
	gap      time.Duration

	jobs chan *PipeJob
	seq  atomic.Int64
	gen  atomic.Int64 // jobs with Gen < current are dropped without playing

	mu     sync.Mutex
	cond   *sync.Cond
	ready  map[int64]*pipeResult // synthesized, waiting for their turn
	next   int64                 // next seq the play loop wants
	closed bool

	processed atomic.Int64
	failed    atomic.Int64
	skipped   atomic.Int64
	rejected  atomic.Int64

	// OnSpoken, when set, is called with the job's RequestID once that
	// fragment is fully handled by playback: played to the end, or dropped
	// by Clear (user asked to not hear it). Failed jobs are NOT reported, so
	// a later daemon can retry them. Set before Start; not guarded.
	OnSpoken func(requestID string)

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewPipeline creates a pipeline using the given provider and inter-fragment gap.
func NewPipeline(provider tts.Provider, gap time.Duration) *Pipeline {
	return newPipeline(provider, gap, audio.NewPlayer())
}

func newPipeline(provider tts.Provider, gap time.Duration, player playback) *Pipeline {
	p := &Pipeline{
		provider: provider,
		player:   player,
		gap:      gap,
		ready:    make(map[int64]*pipeResult),
		next:     1, // job sequence numbers start at 1
		stop:     make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Start launches synthWorkers synthesis workers, one play loop, with a job
// queue of queueSize.
func (p *Pipeline) Start(synthWorkers, queueSize int) {
	p.jobs = make(chan *PipeJob, queueSize)
	for i := 0; i < synthWorkers; i++ {
		p.wg.Add(1)
		go p.synthWorker()
	}
	p.wg.Add(1)
	go p.playLoop()
	logging.Info("Pipeline started (synth_workers=%d, queue=%d, gap=%s)", synthWorkers, queueSize, p.gap)
}

// Submit enqueues a fragment. Returns an error when the queue is full.
func (p *Pipeline) Submit(text, source, requestID string) (*PipeJob, error) {
	job := &PipeJob{
		Seq:       p.seq.Add(1),
		Text:      text,
		Voice:     tts.Voice(p.provider.DefaultVoice()),
		Source:    source,
		RequestID: requestID,
		Gen:       p.gen.Load(),
		CreatedAt: time.Now(),
		Status:    "pending",
	}
	select {
	case p.jobs <- job:
		logging.Debug("Pipeline: job %d queued (source=%s, len=%d)", job.Seq, source, len(text))
		return job, nil
	default:
		p.rejected.Add(1)
		logging.Warn("Pipeline: queue full, rejecting job %d (source=%s)", job.Seq, source)
		return nil, fmt.Errorf("pipeline queue is full")
	}
}

func (p *Pipeline) synthWorker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.stop:
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			var (
				data []byte
				err  error
			)
			res, serr := p.provider.SynthesizeAudio(job.Text, string(job.Voice))
			if serr != nil {
				err = serr
			} else {
				data = res.Data
			}
			p.mu.Lock()
			p.ready[job.Seq] = &pipeResult{job: job, data: data, err: err}
			p.cond.Broadcast()
			p.mu.Unlock()
		}
	}
}

func (p *Pipeline) playLoop() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for p.ready[p.next] == nil && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		r := p.ready[p.next]
		delete(p.ready, p.next)
		p.next++
		p.mu.Unlock()

		if r.job.Gen < p.gen.Load() {
			r.job.Status = "skipped"
			p.skipped.Add(1)
			p.notifySpoken(r.job) // cleared by the user; do not resurrect later
			logging.Info("Pipeline: job %d skipped (cleared)", r.job.Seq)
			continue
		}
		if r.err != nil {
			r.job.Status = "failed"
			p.failed.Add(1)
			logging.Error("Pipeline: job %d synthesis failed: %v", r.job.Seq, r.err)
			continue
		}
		r.job.Status = "playing"
		if err := p.player.Play(r.data); err != nil {
			r.job.Status = "failed"
			p.failed.Add(1)
			logging.Error("Pipeline: job %d playback failed: %v", r.job.Seq, err)
			continue
		}
		r.job.Status = "completed"
		p.processed.Add(1)
		p.notifySpoken(r.job)
		logging.Info("Pipeline: job %d completed (source=%s)", r.job.Seq, r.job.Source)

		select {
		case <-time.After(p.gap):
		case <-p.stop:
		}
	}
}

func (p *Pipeline) notifySpoken(job *PipeJob) {
	if p.OnSpoken != nil && job.RequestID != "" {
		p.OnSpoken(job.RequestID)
	}
}

// Clear drops every queued and in-flight fragment. The audio currently
// playing keeps playing; use StopCurrent to cut it short.
func (p *Pipeline) Clear() int {
	p.gen.Add(1)
	n := 0
	for {
		select {
		case job := <-p.jobs:
			job.Status = "skipped"
			n++
		default:
			logging.Info("Pipeline: cleared %d pending jobs", n)
			return n
		}
	}
}

// StopCurrent kills the audio fragment that is playing right now; the queue
// continues with the next fragment after the usual gap.
func (p *Pipeline) StopCurrent() {
	p.player.Stop()
}

// PipelineStatus is the JSON shape returned by the daemon's /status.
type PipelineStatus struct {
	QueueSize    int   `json:"queue_size"`
	Pending      int   `json:"pending"`       // waiting for a synthesis worker
	ReadyToPlay  int   `json:"ready_to_play"` // synthesized, waiting their turn
	Processed    int64 `json:"total_processed"`
	Failed       int64 `json:"total_failed"`
	Skipped      int64 `json:"total_skipped"`
	Rejected     int64 `json:"total_rejected"`
	IsPlaying    bool  `json:"is_playing"`
	GapMs        int   `json:"gap_ms"`
	NextSeq      int64 `json:"next_seq"`
	LastSeq      int64 `json:"last_seq"`
}

// Status returns a snapshot of pipeline counters.
func (p *Pipeline) Status() PipelineStatus {
	p.mu.Lock()
	ready := len(p.ready)
	p.mu.Unlock()
	return PipelineStatus{
		QueueSize:   cap(p.jobs),
		Pending:     len(p.jobs),
		ReadyToPlay: ready,
		Processed:   p.processed.Load(),
		Failed:      p.failed.Load(),
		Skipped:     p.skipped.Load(),
		Rejected:    p.rejected.Load(),
		IsPlaying:   p.player.IsPlaying(),
		GapMs:       int(p.gap / time.Millisecond),
		NextSeq:     p.next,
		LastSeq:     p.seq.Load(),
	}
}

// Close shuts the pipeline down and waits for its goroutines.
func (p *Pipeline) Close() {
	p.player.Stop()
	close(p.stop)
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	p.wg.Wait()
	logging.Info("Pipeline stopped (processed=%d, failed=%d, skipped=%d)",
		p.processed.Load(), p.failed.Load(), p.skipped.Load())
}
