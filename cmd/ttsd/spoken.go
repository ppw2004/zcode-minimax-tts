package main

import (
	"bytes"
	"os"
	"sync"

	"github.com/ybouhjira/claude-code-tts/internal/logging"
)

// spokenStore persistently records which rollout requestIds have already
// been spoken, so a restarted daemon never repeats audio that a previous
// daemon life already played (or that the user explicitly cleared).
//
// The file is one id per line. It is compacted (rewritten with only the most
// recent ids) once it grows past compactAt lines, so it stays small forever.
type spokenStore struct {
	mu   sync.Mutex
	path string
	ids  map[string]struct{}
	ord  []string

	compactAt int
	keepOn    int
}

const (
	spokenCompactAt = 20000
	spokenKeep      = 10000
)

func newSpokenStore(path string) *spokenStore {
	s := &spokenStore{
		path:      path,
		ids:       make(map[string]struct{}),
		compactAt: spokenCompactAt,
		keepOn:    spokenKeep,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		id := string(bytes.TrimSpace(line))
		if id == "" {
			continue
		}
		if _, dup := s.ids[id]; dup {
			continue
		}
		s.ids[id] = struct{}{}
		s.ord = append(s.ord, id)
	}
	logging.Info("ttsd: spoken store loaded %d ids from %s", len(s.ord), path)
	return s
}

func (s *spokenStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}

func (s *spokenStore) add(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[id]; ok {
		return
	}
	s.ids[id] = struct{}{}
	s.ord = append(s.ord, id)

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logging.Warn("ttsd: cannot append spoken id: %v", err)
		return
	}
	f.Write([]byte(id + "\n"))
	f.Close()

	if len(s.ord) >= s.compactAt {
		s.compactLocked()
	}
}

// compactLocked rewrites the file keeping only the most recent keepOn ids;
// caller must hold mu.
func (s *spokenStore) compactLocked() {
	keep := s.ord
	if len(keep) > s.keepOn {
		keep = keep[len(keep)-s.keepOn:]
	}
	s.ord = append([]string(nil), keep...)
	s.ids = make(map[string]struct{}, len(keep))
	for _, id := range keep {
		s.ids[id] = struct{}{}
	}
	buf := make([]byte, 0, len(keep)*40)
	for _, id := range keep {
		buf = append(buf, id...)
		buf = append(buf, '\n')
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0644); err != nil {
		logging.Warn("ttsd: spoken compaction write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		logging.Warn("ttsd: spoken compaction rename failed: %v", err)
	}
	logging.Info("ttsd: spoken store compacted to %d ids", len(s.ord))
}
