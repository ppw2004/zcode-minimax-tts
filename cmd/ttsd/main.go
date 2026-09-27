// Command ttsd is the standalone TTS daemon for ZCode auto-speech.
//
// It runs outside ZCode's process tree (started via a scheduled task so
// ZCode's hook job object never waits on it), tails ZCode's rollout logs for
// every assistant text segment, and speaks them through an ordered pipeline
// with a short pause between fragments. A small HTTP API on 127.0.0.1 allows
// manual say/clear/skip/status and a liveness ping used by the SessionStart
// hook to revive the daemon when needed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
    "strings"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/ybouhjira/claude-code-tts/internal/logging"
	"github.com/ybouhjira/claude-code-tts/internal/server"
	"github.com/ybouhjira/claude-code-tts/internal/watcher"
)

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	if err := logging.Init(); err != nil {
		fmt.Printf("warning: file logging unavailable: %v\n", err)
	}

	port := envInt("TTS_PORT", 9750)
	gap := time.Duration(envInt("TTS_GAP_MS", 500)) * time.Millisecond
	poll := time.Duration(envInt("TTS_POLL_MS", 400)) * time.Millisecond
	maxChars := envInt("TTS_MAX_CHARS", 0) // 0 = unlimited; set a positive number to cap each fragment
	synthWorkers := envInt("TTS_SYNTH_WORKERS", 2)
	queueSize := envInt("TTS_QUEUE_SIZE", 50)
	var sessions []string
	if v := os.Getenv("TTS_SESSIONS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				sessions = append(sessions, s)
			}
		}
	}

	rolloutDir := os.Getenv("TTS_ROLLOUT_DIR")
	if rolloutDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logging.Fatal("ttsd: cannot determine home directory: %v", err)
		}
		rolloutDir = filepath.Join(home, ".zcode", "cli", "rollout")
	}

	// Single instance: if a daemon already answers /ping, exit quietly.
	if pingDaemon(port) {
		logging.Info("ttsd: another daemon already listening on 127.0.0.1:%d, exiting", port)
		return
	}

	provider, err := server.ResolveProvider()
	if err != nil {
		logging.Fatal("ttsd: %v", err)
	}
	logging.Info("ttsd: starting (pid=%d, provider=%s, port=%d, rollout=%s, gap=%s, poll=%s, max_chars=%d)",
		os.Getpid(), provider.Name(), port, rolloutDir, gap, poll, maxChars)

	pipe := server.NewPipeline(provider, gap)
	spoken := newSpokenStore(spokenFilePath())
	pipe.OnSpoken = spoken.add
	pipe.Start(synthWorkers, queueSize)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := watcher.New(watcher.Options{
		Dir:       rolloutDir,
		PollEvery: poll,
		MaxChars:  maxChars,
		Sessions:  sessions,
		SpokenFilter: func(requestID string) bool {
			return spoken.has(requestID)
		},
		OnLines: func(lines []watcher.Line) {
			for _, l := range lines {
				job, err := pipe.Submit(l.Text, "rollout:"+l.SessionID, l.RequestID)
				if err != nil {
					logging.Warn("ttsd: drop segment from %s: %v", l.File, err)
					continue
				}
				logging.Info("ttsd: queued segment %d from %s (turn=%s, %d chars): %.60s",
					job.Seq, l.File, l.TurnID, len([]rune(l.Text)), l.Text)
			}
		},
	})
	go w.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, "pong")
	})
	mux.HandleFunc("/status", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(pipe.Status())
	})
	mux.HandleFunc("/say", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Text  string `json:"text"`
			Voice string `json:"voice"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
			http.Error(rw, `{"error":"text is required"}`, http.StatusBadRequest)
			return
		}
		job, err := pipe.Submit(body.Text, "api", "")
		if err != nil {
			http.Error(rw, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusTooManyRequests)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(rw, `{"queued":true,"seq":%d}`+"\n", job.Seq)
	})
	mux.HandleFunc("/clear", func(rw http.ResponseWriter, r *http.Request) {
		n := pipe.Clear()
		fmt.Fprintf(rw, `{"cleared":%d}`+"\n", n)
	})
	mux.HandleFunc("/skip", func(rw http.ResponseWriter, r *http.Request) {
		pipe.StopCurrent()
		fmt.Fprint(rw, `{"skipped":true}`+"\n")
	})

	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		logging.Fatal("ttsd: http server failed: %v", err)
	case s := <-sig:
		logging.Info("ttsd: received %v, shutting down", s)
	}

	cancel()
	shutdownCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	_ = srv.Shutdown(shutdownCtx)
	pipe.Close()
	logging.Info("ttsd: stopped")
}

func pingDaemon(port int) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// spokenFilePath resolves the persistent spoken-ids file: next to the
// executable (i.e. ~/.zcode/hooks), overridable via TTS_SPOKEN_FILE.
func spokenFilePath() string {
	if v := os.Getenv("TTS_SPOKEN_FILE"); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "ttsd-spoken.jsonl")
	}
	return filepath.Join(os.TempDir(), "ttsd-spoken.jsonl")
}
