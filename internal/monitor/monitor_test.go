package monitor_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/builtins"
	"gateflow/internal/config"
	"gateflow/internal/monitor"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
)

var now = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ticketMD(id, status string) string {
	return fmt.Sprintf("# %s 題名 %s\n\nステータス: %s\n\n| 項目 | 値 |\n| --- | --- |\n| 種別 | 実装 |\n| ブランチ | feature/x |\n", id, id, status)
}

func logLines(evs ...map[string]any) string {
	var b strings.Builder
	for _, e := range evs {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func toolLine(name string, input map[string]any) string {
	line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "考えの文章"}, map[string]any{"type": "tool_use", "name": name, "input": input}}}})
	return string(line) + "\n"
}

// fixture は、M2 の記録の形を写した tickets のディレクトリを作る。outside は tickets の外の秘密のファイル。
func fixture(t *testing.T) (dir string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "tickets")
	outside := filepath.Join(base, "outside")
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "ticket.md"), []byte(ticketMD("T-016", "draft")+"SECRET-OUTSIDE\n"), 0o644)
	os.WriteFile(filepath.Join(outside, "state.json"), []byte(`{"ticket":"T-017","step":"SECRET-OUTSIDE","status":"implementing"}`), 0o644)

	// T-010：P2 の差し戻しで draft の関門
	write("T-010/ticket.md", ticketMD("T-010", "draft"))
	write("T-010/events.jsonl", `{"time":"2026-10-12T08:00:00Z","by":"gateflow","from":"ready-to-prepare","to":"draft"}`+"\n")
	write("T-010/questions.md", "| # | 質問（要約） | 回答者 | 状態 | 反映先 |\n| --- | --- | --- | --- | --- |\n| Q-01 | a | 人 | 未回答 | |\n| Q-02 | b | 人 | 未回答 | |\n| Q-03 | c | 人 | 確定 | x |\n")
	r := "T-010/runs/20261012-080000/"
	write(r+"state.json", `{"ticket":"T-010","workflow":"aidd-impl","step":"p2-review","steps":1,"status":"draft"}`)
	write(r+"log.jsonl", logLines(
		map[string]any{"time": "2026-10-12T07:50:00Z", "event": "step-start", "step": "p2-review", "n": 1},
		map[string]any{"time": "2026-10-12T08:00:00Z", "event": "step-end", "step": "p2-review", "outcome": "needs_fix", "seconds": 600.5},
		map[string]any{"time": "2026-10-12T08:00:00Z", "event": "gate", "status": "draft"}))
	write(r+"steps/01-p2-review/out/result.json", `{"outcome":"needs_fix","summary":"穴が2つある","findings":[{"label":"MUST"},{"label":"MUST"},{"label":"Q"},{"label":"IMO"}]}`)

	// T-011：実行中（P5 の2回目の試行）
	write("T-011/ticket.md", ticketMD("T-011", "implementing"))
	r = "T-011/runs/20261012-083000/"
	write(r+"running.json", `{"pid":4242,"start":"S","since":"2026-10-12T08:30:00Z"}`)
	write(r+"state.json", `{"ticket":"T-011","workflow":"aidd-impl","step":"p5","steps":3,"loops":{"verify":1},"status":"implementing"}`)
	write(r+"log.jsonl", logLines(
		map[string]any{"time": "2026-10-12T08:30:00Z", "event": "step-start", "step": "p5", "n": 1},
		map[string]any{"time": "2026-10-12T08:40:00Z", "event": "step-end", "step": "p5", "outcome": "done", "seconds": 600},
		map[string]any{"time": "2026-10-12T08:40:00Z", "event": "verify", "name": "test", "exitCode": 1},
		map[string]any{"time": "2026-10-12T08:58:30Z", "event": "step-start", "step": "p5", "n": 3},
		map[string]any{"time": "2026-10-12T08:58:30Z", "event": "agent-start", "step": "p5", "attempt": 1},
		map[string]any{"time": "2026-10-12T08:58:40Z", "event": "output-invalid", "step": "p5", "attempt": 1, "error": "/out/result.json がない\n2行目"},
		map[string]any{"time": "2026-10-12T08:58:40Z", "event": "evidence-check", "ok": false, "hits": []any{1, 2}},
		map[string]any{"time": "2026-10-12T08:58:41Z", "event": "agent-start", "step": "p5", "attempt": 2}))
	var agent strings.Builder
	agent.WriteString(strings.Repeat(`{"type":"system","subtype":"padding","text":"`+strings.Repeat("x", 1000)+`"}`+"\n", 300))
	for i := range 12 {
		agent.WriteString(toolLine("Read", map[string]any{"file_path": fmt.Sprintf("/work/file%02d.go", i), "content": "全文は出さない"}))
	}
	agent.WriteString(toolLine("Bash", map[string]any{"command": "go test ./...\n2行目は出さない"}))
	write(r+"steps/03-p5/agent-1.jsonl", toolLine("Read", map[string]any{"file_path": "/work/old.go"}))
	write(r+"steps/03-p5/agent-2.jsonl", agent.String())
	write(r+"proxy.log", `{"decision":"deny","target":"example.com:443"}`+"\n"+`{"decision":"deny","target":"example.com:443"}`+"\n"+`{"decision":"allow","target":"api.example.invalid:443"}`+"\n")

	// T-012：中断（印はあるがプロセスがない）
	write("T-012/ticket.md", ticketMD("T-012", "implementing"))
	write("T-012/runs/20261012-070000/running.json", `{"pid":5555,"start":"S","since":"2026-10-12T07:00:00Z"}`)
	write("T-012/runs/20261012-070000/state.json", `{"ticket":"T-012","step":"p5","steps":1,"status":"implementing"}`)

	// T-013：上限で止まった
	write("T-013/ticket.md", ticketMD("T-013", "implementing"))
	write("T-013/runs/20261012-060000/state.json", `{"ticket":"T-013","step":"p5","steps":7,"status":"implementing","stopped":{"reason":"limit-loops","detail":"ループの往復が上限（3）に達した"},"resume":"p5"}`)

	// T-014：中止（出さない）、T-015：ready-for-review（コードレビューを待つ）
	write("T-014/ticket.md", ticketMD("T-014", "cancelled"))
	write("T-015/ticket.md", ticketMD("T-015", "ready-for-review"))

	// T-016：tickets の外を指すディレクトリのリンク。T-017：外の state.json を指すファイルのリンク
	if err := os.Symlink(outside, filepath.Join(dir, "T-016")); err != nil {
		t.Fatal(err)
	}
	write("T-017/ticket.md", ticketMD("T-017", "implementing"))
	os.MkdirAll(filepath.Join(dir, "T-017/runs/20261012-050000"), 0o755)
	if err := os.Symlink(filepath.Join(outside, "state.json"), filepath.Join(dir, "T-017/runs/20261012-050000/state.json")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newMonitor(t *testing.T, dir string) *monitor.Monitor {
	t.Helper()
	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatal(err)
	}
	return &monitor.Monitor{Dir: dir, Project: &config.Project{Name: "sample"}, Workflow: wf,
		Now: func() time.Time { return now }, Alive: func(r ticket.Running) bool { return r.PID == 4242 }}
}

func getStatus(t *testing.T, h http.Handler) (monitor.Status, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("code = %d, type = %q, body = %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	var st monitor.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st, rec.Body.String()
}

func findWaiting(st monitor.Status, id string) *monitor.Waiting {
	for i := range st.Waiting {
		if st.Waiting[i].Ticket == id {
			return &st.Waiting[i]
		}
	}
	return nil
}

// fixture に対する /api/status の応答：2段（人を待っているもの、実行中）の中身。
func TestStatus_Fixture(t *testing.T) {
	st, _ := getStatus(t, monitor.Handler(newMonitor(t, fixture(t)), []byte("page")))

	if w := findWaiting(st, "T-010"); w == nil || w.Outcome != "gate" ||
		!strings.Contains(w.Reason, "p2-review needs_fix") || !strings.Contains(w.Reason, "MUST 2・Q 1・IMO 1：穴が2つある") ||
		!strings.Contains(w.Reason, "未確定の質疑 2 件") || !strings.Contains(w.Next, "gateflow ready T-010") {
		t.Errorf("T-010 = %+v", w)
	}
	if w := findWaiting(st, "T-012"); w == nil || w.Outcome != "interrupted" || !strings.Contains(w.Reason, "5555") {
		t.Errorf("T-012 = %+v", w)
	}
	if w := findWaiting(st, "T-013"); w == nil || w.Outcome != "stopped" || !strings.Contains(w.Reason, "limit-loops：ループの往復") ||
		!strings.Contains(w.Next, "resume T-013") {
		t.Errorf("T-013 = %+v", w)
	}
	if w := findWaiting(st, "T-015"); w == nil || w.Outcome != "finished" || !strings.Contains(w.Next, "コードレビュー") {
		t.Errorf("T-015 = %+v", w)
	}
	if w := findWaiting(st, "T-014"); w != nil {
		t.Errorf("中止したチケットが出ている: %+v", w)
	}

	if len(st.Running) != 1 {
		t.Fatalf("実行中 = %+v", st.Running)
	}
	r := st.Running[0]
	if r.Ticket != "T-011" || r.Step != "p5" || r.StepNo != 3 || r.Attempt != 2 || r.StepSeconds != 90 || r.Steps != 3 || r.Loops["verify"] != 1 {
		t.Errorf("実行中 = %+v", r)
	}
	if r.Limits.Steps != 20 || r.Limits.Loops != 3 || r.Limits.IdleMinutes != 10 || r.Limits.RunMinutes != 60 {
		t.Errorf("上限 = %+v", r.Limits)
	}
	var alerts []string
	for _, e := range r.Events {
		if e.Alert {
			alerts = append(alerts, e.Event+":"+e.Text)
		}
	}
	if strings.Join(alerts, "|") != "verify:検証 test（終了コード 1）|output-invalid:/out/result.json がない|evidence-check:根拠の検査 2 件（不合格あり）" {
		t.Errorf("目立たせる記録 = %q", alerts)
	}
	if len(r.Denied) != 1 || r.Denied[0].Host != "example.com:443" || r.Denied[0].Count != 2 {
		t.Errorf("拒否 = %+v", r.Denied)
	}
	if len(r.Tools) != 10 || r.Tools[0].Name != "Bash" || r.Tools[0].Target != "go test ./..." || r.Tools[1].Target != "/work/file11.go" {
		t.Errorf("ツール = %+v", r.Tools)
	}
}

// エージェントの出力からは、ツールの名前と対象だけを出す（入力の全文、AI の文章、古い試行の出力は出さない）。
func TestStatus_ToolsOnlyNameAndTarget(t *testing.T) {
	_, body := getStatus(t, monitor.Handler(newMonitor(t, fixture(t)), nil))
	for _, s := range []string{"全文は出さない", "2行目は出さない", "考えの文章", "/work/old.go", "padding"} {
		if strings.Contains(body, s) {
			t.Errorf("応答に %q が出ている", s)
		}
	}
}

// tickets の外へのパス（ディレクトリのリンク、ファイルのリンク）は読めない。
func TestStatus_DoesNotReadOutside(t *testing.T) {
	st, body := getStatus(t, monitor.Handler(newMonitor(t, fixture(t)), nil))
	if strings.Contains(body, "SECRET-OUTSIDE") {
		t.Fatalf("tickets の外の中身が出ている:\n%s", body)
	}
	if w := findWaiting(st, "T-016"); w != nil {
		t.Errorf("外を指すチケットが出ている: %+v", w)
	}
	if !strings.Contains(strings.Join(st.Errors, "\n"), "T-016") {
		t.Errorf("外を指すチケットが読めなかったことの記録がない: %v", st.Errors)
	}
	if w := findWaiting(st, "T-017"); w != nil && strings.Contains(w.Reason, "SECRET") {
		t.Errorf("外の state.json を読んだ: %+v", w)
	}
}

// GET 以外は 405、知らないパスは 404。/ は埋め込みの HTML を返す。
func TestHandler_MethodsAndPaths(t *testing.T) {
	h := monitor.Handler(newMonitor(t, fixture(t)), []byte("<!doctype html>page"))
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodPost, "/api/status", http.StatusMethodNotAllowed},
		{http.MethodPut, "/", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/status", http.StatusMethodNotAllowed},
		{http.MethodHead, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/T-010/ticket.md", http.StatusNotFound},
		{http.MethodGet, "/api/status/../../etc/passwd", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.code)
		}
		if tc.code == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s %s の Allow = %q", tc.method, tc.path, rec.Header().Get("Allow"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "<!doctype html>page" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("/ = %q, header = %v", body, rec.Header())
	}
}

// tickets のディレクトリがまだなければ、空の状態を返す。
func TestStatus_NoTicketsDir(t *testing.T) {
	st, err := newMonitor(t, filepath.Join(t.TempDir(), "none")).Status()
	if err != nil || len(st.Waiting) != 0 || len(st.Running) != 0 {
		t.Fatalf("st = %+v, err = %v", st, err)
	}
}
