package engine_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/internal/config"
	"gateflow/internal/engine"
	"gateflow/internal/idcheck"
	"gateflow/internal/queue"
	"gateflow/internal/ticket"
)

// 書けない状態は、書く先の名前に同じ名前のディレクトリを先に置いて作る（root でも書けない。M2.8 の Q-04）。
func blockWith(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil { // 中身のあるディレクトリは消せない
		t.Fatal(err)
	}
}

// runDir は、実行の記録のディレクトリ（step の記録のディレクトリの2つ上）。
func runDir(stepDir string) string { return filepath.Dir(filepath.Dir(stepDir)) }

// 状態（state.json）を保存できなければ、実行を止める（遷移表の to の後の保存。engine の3か所のうちの1つ）。
func TestRun_StateSaveFailureStops(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.runner.onRun = func(step, stepDir string) {
		if step == "p3-prepare" {
			blockWith(t, filepath.Join(runDir(stepDir), engine.StateFile))
		}
	}
	var stderr bytes.Buffer
	f.e.Stderr = &stderr
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "write-failed" || !strings.Contains(res.Message, "state.json") {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(stderr.String(), "state.json") {
		t.Errorf("標準エラーに出していない: %q", stderr.String())
	}
}

// ARB の往復の上限で人へ回すときの保存も、失敗したら止める（engine の3か所のうちの1つ）。
func TestRun_StateSaveFailureStopsAtARBLimit(t *testing.T) {
	f := newARBFlow(t, flowTicket, map[string][]string{"p2-arbitrate": {arbResolved}})
	f.ws.workChanges = map[string]string{".docs/design/uc/UC-001.md": "# UC-001\\n\\n| ID |\\n|---|\\n| UC-001-AC-01 |\\n| UC-001-AC-02 |\\n\\nUC-001 のログの項目\\n"}
	f.e.Project.Limits = &config.Limits{ArbitrateLoops: 1}
	f.e.Project.Contracts = nil
	f.runner.results["p2-review"] = []string{p2Fix, strings.Replace(p2Fix, "対応していない", "まだ対応していない", 1)}
	f.runner.onRun = func(step, stepDir string) {
		if step == "p2-review" && f.runner.calls["p2-review"] == 1 {
			blockWith(t, filepath.Join(runDir(stepDir), engine.StateFile))
		}
	}
	f.e.Stderr = &bytes.Buffer{}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "write-failed" {
		t.Fatalf("res = %+v", res)
	}
}

// エラーで止まるときの保存が失敗したら、元のエラーと保存の失敗の両方を返し、標準エラーにも出す（engine の3か所のうちの1つ）。
func TestRun_StateSaveFailureOnErrorPath(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}})
	f.ws.prepareErr = errors.New("clone を用意できない")
	f.ws.beforePrepare = func() {
		dirs, _ := filepath.Glob(filepath.Join(f.root, ".docs/_stash/tickets/T-002/runs/*"))
		for _, d := range dirs {
			blockWith(t, filepath.Join(d, engine.StateFile))
		}
	}
	var stderr bytes.Buffer
	f.e.Stderr = &stderr
	_, err := f.e.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "clone を用意できない") || !strings.Contains(err.Error(), "state.json") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr.String(), "state.json") {
		t.Errorf("標準エラーに出していない: %q", stderr.String())
	}
}

// レビュー報告（人が読む記録）が書けなければ止め、その step から再開できる。
func TestRun_ReviewWriteFailureStops(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	// reviews をファイルで塞ぐ（レビュー報告のディレクトリを作れない）
	if err := os.WriteFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002/reviews"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "write-failed" || !strings.Contains(res.Message, "reviews") {
		t.Fatalf("res = %+v", res)
	}
	st := readState(t, res.RunDir)
	if st.Stopped == nil || st.Stopped.Reason != "write-failed" || st.Resume != "p2-review" || f.runner.calls["p3-prepare"] != 0 {
		t.Errorf("state = %+v, P3 の呼び出し %d", st, f.runner.calls["p3-prepare"])
	}
}

// 実行中の印（running.json）が書けなければ、何もせずに止まる（M2.8 の Q-02）。
func TestRun_RunningMarkFailureStops(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}})
	f.e.MarkRunning = func(*ticket.Run, time.Time) (func() error, error) {
		return nil, errors.New("disk full")
	}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "write-failed" || !strings.Contains(res.Message, "running.json") {
		t.Fatalf("res = %+v", res)
	}
	if len(f.runner.calls) != 0 || len(f.ws.prepareOpts) != 0 {
		t.Errorf("印が書けないのに進んだ: %v", f.runner.calls)
	}
}

// 診断用の記録（エージェントの標準エラー）が書けなくても続け、log.jsonl に write-error を残す。
func TestRun_DiagWriteErrorContinues(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.runner.onRun = func(step, stepDir string) {
		if step == "p2-review" {
			blockWith(t, filepath.Join(stepDir, "agent-1.stderr"))
		}
	}
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyToApprove {
		t.Fatalf("res = %+v", res)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"event":"write-error"`) || !strings.Contains(string(log), "agent-1.stderr") {
		t.Errorf("write-error がない:\n%s", log)
	}
}

// log.jsonl 自体に書けなければ、標準エラーに出して続ける。
func TestRun_LogWriteFailureGoesToStderr(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.runner.onRun = func(step, stepDir string) {
		if step == "p2-review" {
			blockWith(t, filepath.Join(runDir(stepDir), "log.jsonl"))
		}
	}
	var stderr bytes.Buffer
	f.e.Stderr = &stderr
	mustRun(t, f)
	if f.status(t) != ticket.ReadyToApprove || !strings.Contains(stderr.String(), "log.jsonl") {
		t.Fatalf("status %s, 標準エラー %q", f.status(t), stderr.String())
	}
}

// 次のエージェントの入力（差し戻しの内容 feedback.md）が書けなければ止める（M2.8 の Q-01）。
func TestRun_FeedbackWriteFailureStops(t *testing.T) {
	approved := strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1)
	approved = strings.ReplaceAll(approved, "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5": {p5Done}, "p5-verify": {vMust, vApproved}})
	f.runner.onRun = func(step, stepDir string) {
		if step == "p5-verify" {
			blockWith(t, filepath.Join(stepDir, "feedback.md"))
		}
	}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "write-failed" || !strings.Contains(res.Message, "feedback.md") {
		t.Fatalf("res = %+v", res)
	}
	if f.runner.calls["p5"] != 1 {
		t.Errorf("差し戻しの内容が書けないのに P5 をやり直した")
	}
}

// 同じチケットを実行中の gateflow があれば、run も resume も始めない（ステータスを書く前に止める。M2.8）。
func TestRun_LockedTicketDoesNotRun(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	unlock, err := f.e.Ticket.Lock(f.e.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := f.e.Run(t.Context()); !errors.Is(err, ticket.ErrLocked) {
		t.Fatalf("Run = %v, want ErrLocked", err)
	}
	if _, err := f.e.Resume(t.Context()); !errors.Is(err, ticket.ErrLocked) {
		t.Fatalf("Resume = %v, want ErrLocked", err)
	}
	if len(f.runner.calls) != 0 || f.status(t) != ticket.ReadyToPrepare {
		t.Errorf("ロック中に進んだ: %v, %s", f.runner.calls, f.status(t))
	}
	unlock()
	if res := mustRun(t, f); f.status(t) != ticket.ReadyToApprove {
		t.Errorf("外した後に進めない: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".docs/_stash/tickets/T-002", ticket.LockFile)); !os.IsNotExist(err) {
		t.Error("実行の後に印が残っている")
	}
}

// 作業履歴のために、レビュー報告のパスと、質疑に登録した Q 番号を記録に残す（M2.8）。
func TestRun_LogsReviewReportAndQuestions(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}})
	res := mustRun(t, f)
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	for _, w := range []string{`"event":"review-report"`, `"path":"reviews/01-p2-review.md"`, `"event":"questions-added"`, `"ids":["Q-01","Q-02"]`} {
		if !strings.Contains(string(log), w) {
			t.Errorf("記録に %s がない:\n%s", w, log)
		}
	}
}

// AI の呼び出しの順番待ち（M2.9）：ほかが印を持っている間は失敗にせず待ち、待ったことを記録に残す。
func TestRun_WaitsForAgentQueue(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	q := &queue.Lock{Path: filepath.Join(t.TempDir(), "agent-auth.lock"), Interval: 5 * time.Millisecond,
		Alive: func(h queue.Holder) bool { return h.PID == 1 }}
	release, busy, err := q.TryAcquire(queue.Holder{PID: 1, Start: "S", Project: "other", Ticket: "T-099", Step: "p5"})
	if err != nil || busy != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		release()
	}()
	f.e.Queue = q
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyToApprove {
		t.Fatalf("res = %+v", res)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	for _, w := range []string{`"event":"agent-wait"`, `"holderTicket":"T-099"`, `"event":"agent-acquired"`} {
		if !strings.Contains(string(log), w) {
			t.Errorf("記録に %s がない:\n%s", w, log)
		}
	}
	if _, err := os.Stat(q.Path); !os.IsNotExist(err) {
		t.Error("実行の後に順番待ちの印が残っている")
	}
}

// 取り込みの前に、main と作業ブランチを合わせた木で ID 整合チェックを行い、重なれば取り込まずに止める（M2.9）。
func TestRun_MainIDsDuplicateStopsBeforeImport(t *testing.T) {
	approved := strings.ReplaceAll(strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5": {p5Done}, "p5-verify": {vApproved}})
	f.ws.mergedExtra = map[string]string{".docs/design/buc.md": "| BUC-S02 | UC-001（ほかのチケットの UC） |\n"}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "id-duplicate" || !strings.Contains(res.Message, "UC-001") {
		t.Fatalf("res = %+v", res)
	}
	if len(f.ws.imports) != 0 || f.status(t) != ticket.Implementing {
		t.Errorf("重なっているのに取り込んだ: %v, %s", f.ws.imports, f.status(t))
	}
}

// 合わせると衝突するときは、検査できないので警告を残して進む（衝突は人のマージで出る。done で main を検査する）。
func TestRun_MainIDsConflictWarns(t *testing.T) {
	approved := strings.ReplaceAll(strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5": {p5Done}, "p5-verify": {vApproved}})
	f.ws.mergedConflict = true
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || len(f.ws.imports) != 1 || len(res.Warnings) == 0 {
		t.Fatalf("res = %+v, 取り込み %v", res, f.ws.imports)
	}
}

// done の検査は、main の ID 整合チェックに必須の指摘があれば done にしない（M2.9）。
func TestHuman_DoneNeedsCleanMainIDs(t *testing.T) {
	body := strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-for-review", 1)
	f := newFlow(t, body, nil)
	checks := engine.Checks{BranchMerged: func(string) error { return nil }, MainIDsClean: func() error { return errors.New("VAR-21 が2回定義されている") }}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), checks); err == nil || !strings.Contains(err.Error(), "VAR-21") {
		t.Fatalf("done = %v", err)
	}
	checks.MainIDsClean = func() error { return nil }
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), checks); err != nil {
		t.Fatalf("done: %v", err)
	}
}

// Q の指摘の選択肢は、質疑の本文に「選択肢：」として入る。質疑は指摘1件につき1件（M2.9）。
func TestRun_QuestionOptionsInQuestions(t *testing.T) {
	p2 := `{"outcome": "needs_fix", "summary": "s", "findings": [
	  {"label": "Q", "finding": "公開のアドレス", "evidence": {"id": "UC-001"}, "options": ["127.0.0.1 だけ", "0.0.0.0"], "suggestion": "127.0.0.1 だけを推奨"}]}`
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2}})
	mustRun(t, f)
	q, _ := os.ReadFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002/questions.md"))
	if n, _ := f.e.Ticket.UnresolvedQuestions(); n != 1 || !strings.Contains(string(q), "選択肢：127.0.0.1 だけ／0.0.0.0") {
		t.Errorf("質疑 %d 件:\n%s", n, q)
	}
}

// P3 の直後は、受入条件のテストがまだないので「テストがない」で止めない（M3 の Q-03）。
func TestRun_P3SkipsTestMissing(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	rules, err := idcheck.NewRules(config.IDs{Scan: []string{".docs/design"}, Tests: []string{"backend/tests"}, Prefixes: []config.Prefix{
		{Prefix: "UC", Pattern: "UC-[0-9]{3}", Files: []string{".docs/design/buc.md"}, Find: "table-column:UC"},
		{Prefix: "AC", Pattern: "UC-[0-9]{3}-AC-[0-9]{2}", Files: []string{".docs/design/uc/*.md"}, Find: "table-first-column", MethodColumn: "確認の方法"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.e.Rules = rules
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyToApprove {
		log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
		t.Fatalf("res = %+v\n%s", res, log)
	}
}

// levels で推奨にした検証のコマンドは、不合格でも止めずに推奨の指摘として残し、レビューで提示する（M3）。
func TestRun_RecommendedVerify(t *testing.T) {
	approved := strings.ReplaceAll(strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5": {p5Done}, "p5-verify": {vApproved}})
	f.e.Project.Levels = map[string]string{"lint": "recommended"}
	f.runner.codes = []int{0, 0, 1} // build・test は合格、lint は不合格
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || !strings.Contains(res.Next, "推奨の指摘 1 件") {
		t.Fatalf("res = %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(res.RunDir, "recommended.md"))
	if !strings.Contains(string(b), "検証の lint が不合格") {
		t.Errorf("recommended.md:\n%s", b)
	}
	// 必須のまま（既定）なら、同じ不合格で P5 へ戻る
	g := newFlow(t, approved, map[string][]string{"p5": {p5Done}, "p5-verify": {vApproved}})
	g.runner.codes = []int{0, 0, 1, 0, 0, 0}
	mustRun(t, g)
	if g.runner.calls["p5"] != 2 {
		t.Errorf("必須の lint の不合格で P5 へ戻っていない（P5 の呼び出し %d）", g.runner.calls["p5"])
	}
}
