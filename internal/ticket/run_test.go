package ticket_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/internal/ticket"
)

func TestNewRun(t *testing.T) {
	root := writeTicket(t, "T-001", sample)
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 11, 9, 30, 5, 0, time.UTC)
	run, err := tk.NewRun(now)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	want := filepath.Join(root, ticket.Dir, "T-001", "runs", "20261011-093005")
	if run.Dir != want {
		t.Errorf("Dir = %s, want %s", run.Dir, want)
	}

	step, err := run.StepDir(1, "p5")
	if err != nil {
		t.Fatalf("StepDir: %v", err)
	}
	if step != filepath.Join(want, "steps", "01-p5") {
		t.Errorf("StepDir = %s", step)
	}
	if info, err := os.Stat(step); err != nil || !info.IsDir() {
		t.Errorf("step のディレクトリがない: %v", err)
	}

	if err := run.Log(now, "clone", map[string]any{"branch": "feature/t-001-publish"}); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := run.Log(now.Add(time.Second), "agent-start", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(want, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("log の行数 = %d, want 2", len(lines))
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["event"] != "clone" || ev["time"] != "2026-10-11T09:30:05Z" || ev["branch"] != "feature/t-001-publish" {
		t.Errorf("event = %v", ev)
	}
}

// 同じ時刻の実行は、別のディレクトリにする（上書きしない）。
func TestNewRun_SameSecond(t *testing.T) {
	root := writeTicket(t, "T-001", sample)
	tk, _ := ticket.Load(root, "T-001")
	now := time.Date(2026, 10, 11, 9, 30, 5, 0, time.UTC)
	a, err := tk.NewRun(now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := tk.NewRun(now)
	if err != nil {
		t.Fatal(err)
	}
	if a.Dir == b.Dir {
		t.Errorf("同じディレクトリ %s", a.Dir)
	}
}
