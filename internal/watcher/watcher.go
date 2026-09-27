// Package watcher tails ZCode's rollout logs (model-io-<session>.jsonl) and
// emits every assistant text segment as it lands on disk. Each line of those
// files is one completed model API call; response.text is the assistant's
// visible text for that call, which includes all intermediate segments a turn
// produces between tool calls.
package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ybouhjira/claude-code-tts/internal/logging"
)

// Line is one cleaned assistant segment extracted from a rollout log.
type Line struct {
	SessionID   string
	TurnID      string
	RequestID   string
	CompletedAt string // RFC3339 UTC, sortable as a string
	Text        string // cleaned, ready to speak
	File        string
}

// Options configures a Watcher.
type Options struct {
	Dir       string             // directory holding model-io-*.jsonl
	PollEvery time.Duration      // directory poll interval
	MaxChars  int                // per-fragment speech cap (runes); 0 = unlimited
	OnLines   func(lines []Line) // called in order each tick that produced output
	Logf      func(format string, args ...interface{})

	// AdoptCutoff is how far before startup a pre-existing file's tail is
	// still replayed on the first tick (segments produced while no daemon
	// was running). Defaults to 10 minutes.
	AdoptCutoff time.Duration

	// SpokenFilter, when set, suppresses lines whose requestId was already
	// spoken (according to the caller's persistent record). Applied to
	// adopted lines AND live appends, so a restart never repeats audio.
	SpokenFilter func(requestID string) bool

	// Sessions, when non-empty, restricts speech to these session IDs only
	// (TTS_SESSIONS env, comma-separated). Empty = speak every session.
	Sessions []string
}

var (
	codeBlockRe = regexp.MustCompile("(?s)```.*?```")
	urlRe       = regexp.MustCompile(`https?://\S+`)
	noiseRe     = regexp.MustCompile("[#*`>|_\\[\\]()~]")
	wsRe        = regexp.MustCompile(`\s+`)
)

// CleanText normalizes an assistant segment for speech: code blocks and URLs
// become short spoken placeholders, markdown decoration is stripped, and the
// result is capped at maxChars runes when maxChars > 0.
func CleanText(text string, maxChars int) string {
	text = codeBlockRe.ReplaceAllString(text, "，代码块略过。")
	text = urlRe.ReplaceAllString(text, "，链接略过。")
	text = noiseRe.ReplaceAllString(text, " ")
	text = wsRe.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	if maxChars > 0 {
		r := []rune(text)
		if len(r) > maxChars {
			text = string(r[:maxChars])
		}
	}
	return text
}

// looksLikeMarkup suppresses non-prose segments that leak into response.text:
// raw JSON blobs and tool-invocation banners (e.g. "Z.ai Built-in Tool: web
// search prime Input: …"), which would otherwise be spoken as garbled noise
// when several ZCode sessions run at once.
func looksLikeMarkup(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		return true
	}
	return strings.Contains(t, "Built-in Tool:")
}

type rolloutLine struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	TurnID      string `json:"turnId"`
	RequestID   string `json:"requestId"`
	CompletedAt string `json:"completedAt"`
	Response    struct {
		Text          string `json:"text"`
		ReasoningText string `json:"reasoningText"`
	} `json:"response"`
}

// reqRing is a fixed-capacity set of recently seen request IDs used to
// deduplicate retried/re-written log lines.
type reqRing struct {
	mu    map[string]struct{}
	order []string
	cap   int
}

func newReqRing(capacity int) *reqRing {
	return &reqRing{mu: make(map[string]struct{}, capacity), cap: capacity}
}

func (r *reqRing) seenOrAdd(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := r.mu[id]; ok {
		return true
	}
	r.mu[id] = struct{}{}
	r.order = append(r.order, id)
	if len(r.order) > r.cap {
		delete(r.mu, r.order[0])
		r.order = r.order[1:]
	}
	return false
}

// Watcher tails every model-io-*.jsonl in a directory.
type Watcher struct {
	opt       Options
	offsets   map[string]int64  // file -> bytes already read
	partial   map[string][]byte // file -> trailing bytes of an incomplete line
	seen      *reqRing
	startTime time.Time // daemon start; anchors the adopt replay window
	started   bool      // false until the first tick has adopted existing files
}

// New creates a Watcher. Files that already exist when the first tick runs
// are adopted: their history is skipped, except lines that completed within
// adoptCutoff before daemon startup — those may have landed while the daemon
// was booting (e.g. ZCode cold start) and are replayed so they are not lost.
// Files that appear later are read from the beginning.
func New(opt Options) *Watcher {
	if opt.PollEvery <= 0 {
		opt.PollEvery = 400 * time.Millisecond
	}
	if opt.MaxChars < 0 {
		opt.MaxChars = 0 // 0 = unlimited
	}
	if opt.Logf == nil {
		opt.Logf = logging.Info
	}
	if opt.AdoptCutoff <= 0 {
		opt.AdoptCutoff = 10 * time.Minute
	}
	return &Watcher{
		opt:       opt,
		offsets:   make(map[string]int64),
		partial:   make(map[string][]byte),
		seen:      newReqRing(4096),
		startTime: time.Now().UTC(),
	}
}

// Run polls until ctx is done. It never calls OnLines from two ticks at once.
func (w *Watcher) Run(ctx context.Context) {
	capDesc := "unlimited"
	if w.opt.MaxChars > 0 {
		capDesc = fmt.Sprintf("%d chars", w.opt.MaxChars)
	}
	w.opt.Logf("Watcher: watching %s (poll=%s, cap=%s)", w.opt.Dir, w.opt.PollEvery, capDesc)
	t := time.NewTicker(w.opt.PollEvery)
	defer t.Stop()
	for {
		w.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watcher) tick() {
	matches, err := filepath.Glob(filepath.Join(w.opt.Dir, "model-io-*.jsonl"))
	if err != nil {
		return
	}
	sort.Strings(matches)

	var out []Line
	for _, path := range matches {
		lines := w.readNew(path)
		out = append(out, lines...)
	}
	// completedAt is RFC3339 UTC, so a string sort is chronological across
	// files; within a file order is already append order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CompletedAt < out[j].CompletedAt
	})
	w.started = true
	if len(out) > 0 && w.opt.OnLines != nil {
		w.opt.OnLines(out)
	}
}

// readNew consumes bytes appended to one file since the last tick.
func (w *Watcher) readNew(path string) []Line {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	off, known := w.offsets[path]
	if !known {
		if !w.started {
			// Pre-existing file on the very first tick: adopt it.
			return w.adoptFile(path, st)
		}
		off = 0
	}
	if st.Size() < off {
		// Truncated or replaced; reread from the start. The requestId ring
		// keeps already-spoken lines from being repeated.
		off = 0
	}
	if st.Size() == off {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil
	}
	w.offsets[path] = st.Size()

	data := append(w.partial[path], buf...)
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		w.partial[path] = data
		return nil
	}
	w.partial[path] = data[lastNL+1:]

	var out []Line
	for _, raw := range strings.Split(string(data[:lastNL+1]), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if raw == "" {
			continue
		}
		if l, ok := w.parseLine(raw, path); ok {
			out = append(out, l)
		}
	}
	return out
}

// adoptFile initializes cursor state for a file that existed before the
// watcher started: history is skipped, recent lines are replayed.
func (w *Watcher) adoptFile(path string, st os.FileInfo) []Line {
	data, err := os.ReadFile(path)
	if err != nil {
		w.offsets[path] = st.Size()
		return nil
	}
	w.offsets[path] = st.Size()
	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		w.partial[path] = data
		return nil
	}
	w.partial[path] = data[lastNL+1:]

	cutoff := w.startTime.Add(-w.opt.AdoptCutoff)
	var out []Line
	for _, raw := range strings.Split(string(data[:lastNL+1]), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if raw == "" {
			continue
		}
		var rl rolloutLine
		if json.Unmarshal([]byte(raw), &rl) != nil || rl.Type != "model_io" || rl.CompletedAt == "" {
			continue
		}
		ts, perr := time.Parse(time.RFC3339, rl.CompletedAt)
		if perr != nil || ts.Before(cutoff) {
			continue
		}
		if l, ok := w.parseLine(raw, path); ok {
			out = append(out, l)
		}
	}
	if len(out) > 0 {
		w.opt.Logf("Watcher: adopted %d recent segment(s) from %s", len(out), filepath.Base(path))
	}
	return out
}

func (w *Watcher) parseLine(raw, path string) (Line, bool) {
	raw = strings.TrimPrefix(raw, "\ufeff") // tolerate a UTF-8 BOM on the first line
	var rl rolloutLine
	if err := json.Unmarshal([]byte(raw), &rl); err != nil {
		return Line{}, false
	}
	if rl.Type != "model_io" {
		return Line{}, false
	}
	// Lines without a session ID are internal tool model calls (e.g. the
	// web-reader's summarizer), not assistant replies — never speak them.
	if rl.SessionID == "" {
		return Line{}, false
	}
	if len(w.opt.Sessions) > 0 {
		allowed := false
		for _, s := range w.opt.Sessions {
			if s == rl.SessionID {
				allowed = true
				break
			}
		}
		if !allowed {
			return Line{}, false
		}
	}
	if w.opt.SpokenFilter != nil && w.opt.SpokenFilter(rl.RequestID) {
		// Already spoken in a previous daemon life; never repeat audio.
		return Line{}, false
	}
	if w.seen.seenOrAdd(rl.RequestID) {
		return Line{}, false
	}
	text := CleanText(rl.Response.Text, w.opt.MaxChars)
	if text == "" || looksLikeMarkup(rl.Response.Text) {
		return Line{}, false
	}
	return Line{
		SessionID:   rl.SessionID,
		TurnID:      rl.TurnID,
		RequestID:   rl.RequestID,
		CompletedAt: rl.CompletedAt,
		Text:        text,
		File:        filepath.Base(path),
	}, true
}
