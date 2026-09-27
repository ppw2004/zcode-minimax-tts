package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLine(t *testing.T, path, session, req, text string) string {
	t.Helper()
	return writeLineAt(t, path, session, req, text, time.Date(2026, 9, 27, 7, 0, 0, 0, time.UTC))
}

func writeLineAt(t *testing.T, path, session, req, text string, completedAt time.Time) string {
	t.Helper()
	ts := completedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	line := fmt.Sprintf(`{"type":"model_io","sessionId":%q,"turnId":"turn-1","requestId":%q,"completedAt":%q,"response":{"text":%q,"reasoningText":"think"}}`+"\n",
		session, req, ts, text)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line)
	f.Close()
	return line
}

func collect() (*[]Line, func([]Line)) {
	var got []Line
	return &got, func(l []Line) { got = append(got, l...) }
}

// The first tick must seek pre-existing files to EOF; only bytes appended
// after the watcher started produce lines.
func TestWatcher_SkipsBacklogThenEmitsAppends(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s1.jsonl")
	writeLine(t, file, "s1", "r0", "历史内容不应播报")

	got, onLines := collect()
	w := New(Options{Dir: dir, PollEvery: time.Hour, OnLines: onLines, Logf: func(string, ...interface{}) {}})
	w.tick()
	if len(*got) != 0 {
		t.Fatalf("backlog emitted: %v", *got)
	}

	writeLine(t, file, "s1", "r1", "新增片段")
	w.tick()
	if len(*got) != 1 || (*got)[0].Text != "新增片段" {
		t.Fatalf("append not emitted: %+v", *got)
	}
}

// A line split across two writes must be held back until its newline lands.
func TestWatcher_HoldsPartialLines(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s2.jsonl")
	writeLine(t, file, "s2", "r1", "分段写入")
	body, _ := os.ReadFile(file)

	os.WriteFile(file, body[:len(body)/2], 0644) // truncate to half a line
	got, onLines := collect()
	w := New(Options{Dir: dir, PollEvery: time.Hour, OnLines: onLines, Logf: func(string, ...interface{}) {}})
	w.tick()
	w.offsets[file] = 0 // force reread of the partial line
	w.partial[file] = nil
	w.tick()
	if len(*got) != 0 {
		t.Fatalf("partial line emitted early: %+v", *got)
	}

	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString(string(body[len(body)/2:]))
	f.Close()
	w.tick()
	if len(*got) != 1 || (*got)[0].Text != "分段写入" {
		t.Fatalf("completed line not emitted: %+v", *got)
	}
}

// Duplicated requestIds (retries) and non-speech lines must be filtered.
func TestWatcher_DedupAndFilters(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s3.jsonl")
	got, onLines := collect()
	w := New(Options{Dir: dir, PollEvery: time.Hour, MaxChars: 400, OnLines: onLines, Logf: func(string, ...interface{}) {}})
	w.started = true // treat file as newly discovered

	writeLine(t, file, "s3", "r1", "第一段")
	f, _ := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString(`{"type":"other","sessionId":"s3","requestId":"r2","response":{"text":"非model_io"}}` + "\n")
	f.WriteString(`{"type":"model_io","sessionId":"s3","requestId":"r3","response":{"text":""}}` + "\n") // tool-call only
	f.Close()
	w.tick()
	writeLine(t, file, "s3", "r1", "第一段重试") // same requestId, new line
	w.tick()

	if len(*got) != 1 || (*got)[0].Text != "第一段" {
		t.Fatalf("got %+v, want single 第一段", *got)
	}
}

// On the first tick, a pre-existing file's history is skipped, but lines that
// completed shortly before daemon startup (the cold-start window) are
// replayed so segments landing during daemon boot are not lost.
func TestWatcher_AdoptReplaysColdStartWindow(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s4.jsonl")
	writeLineAt(t, file, "s4", "r-old", "两小时前的历史段落", time.Now().Add(-2*time.Hour))
	writeLineAt(t, file, "s4", "r-new", "冷启动窗口内的最新段落", time.Now().Add(-5*time.Second))

	got, onLines := collect()
	w := New(Options{Dir: dir, PollEvery: time.Hour, OnLines: onLines, Logf: func(string, ...interface{}) {}})
	w.tick()
	if len(*got) != 1 || (*got)[0].Text != "冷启动窗口内的最新段落" {
		t.Fatalf("adopt result: %+v, want only the recent segment", *got)
	}

	// After adoption, only genuinely new appends are emitted.
	writeLineAt(t, file, "s4", "r-append", "启动后的新增段落", time.Now())
	w.tick()
	if len(*got) != 2 || (*got)[1].Text != "启动后的新增段落" {
		t.Fatalf("post-adopt appends: %+v", *got)
	}
}

// SpokenFilter suppresses lines whose requestId was already spoken, both in
// adopted (cold-start replay) lines and in live appends.
func TestWatcher_SpokenFilterSuppresses(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s5.jsonl")
	writeLineAt(t, file, "s5", "r-played", "守护进程重启前已播过的段落", time.Now().Add(-5*time.Second))
	writeLineAt(t, file, "s5", "r-unplayed", "重启前没播到的新段落", time.Now().Add(-2*time.Second))

	spoken := map[string]bool{"r-played": true}
	got, onLines := collect()
	w := New(Options{
		Dir:          dir,
		PollEvery:    time.Hour,
		OnLines:      onLines,
		Logf:         func(string, ...interface{}) {},
		SpokenFilter: func(id string) bool { return spoken[id] },
	})
	w.tick()
	if len(*got) != 1 || (*got)[0].Text != "重启前没播到的新段落" {
		t.Fatalf("adopt with spoken filter: %+v", *got)
	}

	// Live append of an already-spoken id (e.g. rewritten line) is dropped too.
	writeLineAt(t, file, "s5", "r-played", "守护进程重启前已播过的段落", time.Now())
	w.tick()
	if len(*got) != 1 {
		t.Fatalf("spoken id re-emitted on append: %+v", *got)
	}
}

// Tool-invocation banners and raw JSON that leak into response.text must not
// be spoken.
func TestWatcher_SuppressesMarkup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model-io-s6.jsonl")
	got, onLines := collect()
	w := New(Options{Dir: dir, PollEvery: time.Hour, OnLines: onLines, Logf: func(string, ...interface{}) {}})
	w.started = true

	f, _ := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	for _, text := range []string{
		`?? Z.ai Built-in Tool: web search prime Input: something Execut...`,
		`{"title":"查找zcode语音输入插件"}`,
		`正常的中文播报段落`,
	} {
		ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		fmt.Fprintf(f, `{"type":"model_io","sessionId":"s","turnId":"t","requestId":"r-%s","completedAt":%q,"response":{"text":%q}}`+"\n", text, ts, text)
	}
	f.Close()
	w.tick()

	if len(*got) != 1 || (*got)[0].Text != "正常的中文播报段落" {
		t.Fatalf("markup not suppressed: %+v", *got)
	}
}

func TestCleanText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# 标题 **加粗**", "标题 加粗"},
		{"先看配置：\n```go\nfmt.Println()\n```\n再继续", "先看配置： ，代码块略过。 再继续"},
		{"详情见 https://example.com/x 文档", "详情见 ，链接略过。 文档"},
		{"[链接](http://a.com) 和 `code`", "链接 ，链接略过。 和 code"},
		{strings.Repeat("长", 10), strings.Repeat("长", 10)},
	}
	for _, c := range cases {
		if got := CleanText(c.in, 400); got != c.want {
			t.Errorf("CleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := CleanText(strings.Repeat("长", 20), 10); len([]rune(got)) != 10 {
		t.Errorf("cap not applied: %d runes", len([]rune(got)))
	}
	if got := CleanText("```\nblock\n```", 400); got != "，代码块略过。" {
		t.Errorf("code-only text: %q", got)
	}
}
