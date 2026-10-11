package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"gateflow/internal/engine"
)

func TestState_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := &engine.State{Ticket: "T-002", Workflow: "aidd-impl", Step: "p5", Steps: 3,
		Loops: map[string]int{"verify": 1}, Status: "implementing",
		Prints:  map[string]string{"verify": "abc"},
		Stopped: &engine.Stop{Reason: "limit-loops", Detail: "verify が3往復"}, Resume: "p5"}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := engine.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Step != "p5" || got.Loops["verify"] != 1 || got.Stopped.Reason != "limit-loops" || got.Resume != "p5" || got.Prints["verify"] != "abc" {
		t.Errorf("got = %+v", got)
	}
	// 一時ファイルは残らない。
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Errorf("ディレクトリの中身 = %v", entries)
	}
}

// 最新の実行の記録の状態を探す（runs/ の名前は日時なので、名前の順が新しさの順）。
func TestLatestState(t *testing.T) {
	runs := t.TempDir()
	for i, name := range []string{"20261012-090000", "20261012-100000", "20261011-235959"} {
		d := filepath.Join(runs, name)
		os.MkdirAll(d, 0o755)
		(&engine.State{Ticket: "T-002", Steps: i}).Save(d)
	}
	os.MkdirAll(filepath.Join(runs, "20261012-110000"), 0o755) // state.json のない記録は飛ばす
	s, dir, err := engine.LatestState(runs)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "20261012-100000" || s.Steps != 1 {
		t.Errorf("dir, steps = %s, %d", dir, s.Steps)
	}
	if _, _, err := engine.LatestState(t.TempDir()); err == nil {
		t.Error("記録がないのに見つかった")
	}
}
