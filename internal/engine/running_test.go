package engine_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/internal/container"
	"gateflow/internal/engine"
	"gateflow/internal/ticket"
)

// markRunner は、エージェントを動かすたびに、実行中の印があるかを記録する。cancel があれば、最初の呼び出しで取り消す。
type markRunner struct {
	inner  engine.Runner
	root   string
	seen   []bool
	proxy  []string // エージェントを動かす時点の proxy.log の中身
	cancel context.CancelFunc
}

func (r *markRunner) Run(ctx context.Context, spec container.Spec, stdout, stderr io.Writer) (int, error) {
	marks, _ := filepath.Glob(filepath.Join(r.root, ticket.Dir, "T-002", "runs", "*", ticket.RunningFile))
	r.seen = append(r.seen, len(marks) == 1)
	logs, _ := filepath.Glob(filepath.Join(r.root, ticket.Dir, "T-002", "runs", "*", engine.ProxyLogFile))
	var body string
	for range 200 { // NOTE: 追跡は別の goroutine で書くので、少し待つ
		if len(logs) == 1 {
			b, _ := os.ReadFile(logs[0])
			if body = string(b); body != "" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.proxy = append(r.proxy, body)
	if r.cancel != nil {
		r.cancel()
		return 0, ctx.Err()
	}
	return r.inner.Run(ctx, spec, stdout, stderr)
}

// 実行の間だけ実行中の印（このプロセスの ID と開始時刻）を置き、終わったら消す。step と試行の開始を記録する（M2.5）。
func TestRun_RunningMarkAndStartEvents(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	mr := &markRunner{inner: f.runner, root: f.root}
	f.e.Runner = mr
	res := mustRun(t, f)
	if len(mr.seen) == 0 || !mr.seen[0] {
		t.Fatalf("実行中に印がない: %v", mr.seen)
	}
	if _, err := os.Stat(filepath.Join(res.RunDir, ticket.RunningFile)); !os.IsNotExist(err) {
		t.Errorf("終わった後も印が残っている: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	for _, want := range []string{`"event":"step-start"`, `"event":"agent-start"`, `"attempt":1`} {
		if !strings.Contains(string(log), want) {
			t.Errorf("記録に %s がない:\n%s", want, log)
		}
	}
}

// 取り消し（SIGINT・SIGTERM で context が取り消される）で終わるときも、印を消す。
func TestRun_RunningMarkRemovedOnCancel(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mr := &markRunner{inner: f.runner, root: f.root, cancel: cancel}
	f.e.Runner = mr
	res, _ := f.e.Run(ctx)
	if len(mr.seen) == 0 || !mr.seen[0] {
		t.Fatalf("実行中に印がない: %v", mr.seen)
	}
	marks, _ := filepath.Glob(filepath.Join(f.root, ticket.Dir, "T-002", "runs", "*", ticket.RunningFile))
	if len(marks) != 0 {
		t.Errorf("取り消しの後も印が残っている: %v（%+v）", marks, res)
	}
}

// プロキシの判定は、実行の終わりを待たずに、実行の間に proxy.log へ逐次書く（M2.5。監視の UI の「実行中」に出すため）。
func TestRun_ProxyLogWrittenDuringRun(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	mr := &markRunner{inner: f.runner, root: f.root}
	f.e.Runner = mr
	res := mustRun(t, f)
	if len(mr.proxy) == 0 || !strings.Contains(mr.proxy[0], "followed.example:443") {
		t.Fatalf("実行中の proxy.log = %q", mr.proxy)
	}
	data, _ := os.ReadFile(filepath.Join(res.RunDir, engine.ProxyLogFile))
	if strings.Count(string(data), "followed.example") != 1 || strings.Contains(string(data), `{"decision":"deny"}`) {
		t.Errorf("終わった後の proxy.log（止めるときの記録を重ねて書かない）= %q", data)
	}
}

// 逐次の追跡が失敗したときは、止めるときに取り出した記録を書き、失敗を記録に残す。
func TestRun_ProxyLogFallbackWhenFollowFails(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.e.Egress = &fakeEgress{followErr: errors.New("docker logs が使えない")}
	res := mustRun(t, f)
	data, _ := os.ReadFile(filepath.Join(res.RunDir, engine.ProxyLogFile))
	if string(data) != "{\"decision\":\"deny\"}\n" {
		t.Errorf("proxy.log = %q", data)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), "proxy-follow-error") {
		t.Errorf("追跡の失敗の記録がない:\n%s", log)
	}
}
