package ticket_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gateflow/internal/ticket"
)

// 印は一時ファイルからの改名で置き、このプロセスの ID と開始時刻の組を持つ。消す関数で消える。
func TestMarkRunning(t *testing.T) {
	run := &ticket.Run{Dir: t.TempDir()}
	unmark, err := run.MarkRunning(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(run.Dir, ticket.RunningFile))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ticket.DecodeRunning(data)
	if err != nil || r.PID != os.Getpid() || !r.Alive() {
		t.Fatalf("印 = %+v, err = %v, Alive = %v", r, err, r.Alive())
	}
	if left, _ := filepath.Glob(filepath.Join(run.Dir, ticket.RunningFile+".*")); len(left) != 0 {
		t.Errorf("一時ファイルが残っている: %v", left)
	}
	unmark()
	if _, err := os.Stat(filepath.Join(run.Dir, ticket.RunningFile)); !os.IsNotExist(err) {
		t.Errorf("消した後も残っている: %v", err)
	}
}

// プロセス ID が同じでも開始時刻が違えば（ID が別のプロセスに再利用された）、動いていないとみなす。プロセスがなければ動いていない。
func TestRunning_AliveRejectsReusedPID(t *testing.T) {
	start, err := ticket.ProcessStart(os.Getpid())
	if err != nil {
		t.Skip("ps が使えない:", err)
	}
	for name, r := range map[string]ticket.Running{
		"開始時刻が違う": {PID: os.Getpid(), Start: "Thu Jan  1 00:00:00 1970"},
		"プロセスがない": {PID: 999999, Start: start},
	} {
		if r.Alive() {
			t.Errorf("%s: Alive = true", name)
		}
	}
}

func TestDecodeRunning_Invalid(t *testing.T) {
	for _, s := range []string{`{`, `{"pid":0,"start":"x"}`, `{"pid":1}`} {
		if _, err := ticket.DecodeRunning([]byte(s)); err == nil {
			t.Errorf("%s を受け付けた", s)
		}
	}
	b, _ := json.Marshal(ticket.Running{PID: 1, Start: "x"})
	if _, err := ticket.DecodeRunning(b); err != nil {
		t.Error(err)
	}
}
