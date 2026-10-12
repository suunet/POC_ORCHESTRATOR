// Package engine は、遷移の評価、状態の2層管理、上限と進展なしの検知を扱う。
//
// 遷移表（workflow）に従って、人間関門まで step を実行する。ライフサイクルは指示書のステータス、
// 実行中の状態（今の step、ループの往復、再開の位置）は実行の記録の state.json に持つ。
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gateflow/internal/agent"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/idcheck"
	"gateflow/internal/output"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
	"gateflow/internal/workspace"
)

// Outcome は、Run の結果の種類。
type Outcome string

const (
	Gate        Outcome = "gate"          // 人間関門で止まった（人の操作を待つ）
	Finished    Outcome = "finished"      // 流れの終わり（done・cancelled）に着いた
	Stopped     Outcome = "stopped"       // 人の確認待ちで止まった（blocked、上限、進展なしなど）
	NothingToDo Outcome = "nothing-to-do" // 進められる状態でない（保留など）
	Runnable    Outcome = "runnable"      // gateflow が進められる状態（run を待つ。Describe だけが返す）
)

// Result は、Run の結果。Next は、人が次にすること。
type Result struct {
	Outcome  Outcome
	Status   ticket.Status
	Reason   string
	Message  string
	Next     string
	RunDir   string
	Warnings []string
}

// Workspace は、タスクごとの clone の操作。
type Workspace interface {
	Prepare(ctx context.Context, ticketID, branch, mainBranch string, deps *config.Deps, opt workspace.PrepareOptions) (workspace.Prepared, error)
	AgentMounts(ticketID string) []container.Mount
	ReviewerMounts(ticketID string) []container.Mount
	CommitHash(ctx context.Context, ticketID, message string, author workspace.Author) (string, bool, error)
	Import(ctx context.Context, ticketID, branch string) error
	Export(ctx context.Context, ticketID, dest string) (string, error)
	Diff(ctx context.Context, ticketID, base string) (string, error)
	TreeHash(ctx context.Context, ticketID string) (string, error)
	Snapshot(ctx context.Context, ticketID, dest string) error
	Reset(ctx context.Context, ticketID string) error
	Tracked(ctx context.Context, mainBranch, path string) bool
}

// Egress は、内部ネットワークと許可リストのプロキシ。
type Egress interface {
	Start(ctx context.Context, allow []string) (string, error)
	Follow(ctx context.Context, w io.Writer) error
	Stop(ctx context.Context) ([]byte, error)
	Network() string
}

// Runner は、コンテナを起動する。
type Runner interface {
	Run(ctx context.Context, spec container.Spec, stdout, stderr io.Writer) (int, error)
}

// Engine は、1件のチケットを遷移表に従って進める。
type Engine struct {
	Root       string
	Project    *config.Project
	Personal   *config.Personal
	Workflow   *workflow.Workflow
	Ticket     *ticket.Ticket
	AgentImage string
	Agent      agent.Agent
	Workspace  Workspace
	Egress     Egress
	Runner     Runner
	Author     workspace.Author
	Builtins   fs.FS
	Rules      *idcheck.Rules
	Now        func() time.Time

	// 1回の実行の中で、step の結果を後の操作に渡す
	run        *ticket.Run
	proxyURL   string
	lastP3     *output.P3Result
	lastReview *output.ReviewResult
	print      Fingerprint     // 直前の step が作った、進展なしの判定の指紋
	findings   []string        // 直前の BJ の指摘の指紋
	lastBlock  string          // 直前の P5 の blocked の理由
	lastVerify string          // 直前の検証の出力
	warnings   []string        // 実行の中で見つけた、止めない警告
	planned    map[string]bool // BJ の根拠に使われた、予定の ID（Q-24）
	lastARB    *output.ARBResult
	arbForced  []int    // 進展なしで人へ回す指摘の番号（M2.6）
	arbQIDs    []string // ARB の裁定を書いた質疑の番号（コミットのメッセージに使う）
	checks     int
}

// errStop は、step の実行が人の確認待ちで止まることを表す。
type errStop struct{ reason, detail string }

func (e *errStop) Error() string { return e.reason + ": " + e.detail }

// followProxy は、プロキシの判定を実行の記録の proxy.log へ逐次書き始め、プロキシを止めて書き終える関数を返す。
// 逐次の書き込みが1行も書けなかったときは、止めるときに取り出した記録を書く。
func (e *Engine) followProxy(ctx context.Context, runDir string) func() {
	f, err := os.OpenFile(filepath.Join(runDir, ProxyLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		e.log("proxy-follow-error", map[string]any{"error": err.Error()})
	}
	w := &countWriter{w: f}
	followCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan error, 1)
	if f != nil {
		go func() { done <- e.Egress.Follow(followCtx, w) }()
	} else {
		done <- nil
	}
	return func() {
		logs, stopErr := e.Egress.Stop(context.WithoutCancel(ctx))
		var followErr error
		timedOut := false
		select {
		case followErr = <-done: // プロキシが消えると、記録の追跡も終わる
		case <-time.After(5 * time.Second):
			timedOut = true
			cancel()
			followErr = <-done
		}
		cancel()
		if f != nil {
			if w.n == 0 && len(logs) > 0 {
				f.Write(logs)
			}
			f.Close()
		}
		if followErr != nil && !timedOut {
			e.log("proxy-follow-error", map[string]any{"error": followErr.Error()})
		}
		if stopErr != nil {
			e.log("proxy-stop-error", map[string]any{"error": stopErr.Error()})
		}
	}
}

// countWriter は、書いたバイト数を数える。
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// short は、コミットのハッシュの先頭7文字を返す（表示用）。
func short(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

func stop(reason, format string, args ...any) error {
	return &errStop{reason: reason, detail: fmt.Sprintf(format, args...)}
}

// Run は、チケットを遷移表に従って人間関門まで進める。
func (e *Engine) Run(ctx context.Context) (Result, error) {
	warnings := e.manualEditWarnings()
	if e.Ticket.Held() {
		return Result{Outcome: NothingToDo, Status: e.Ticket.Status, Message: "保留の印があるので進めない", Next: "gateflow unhold " + e.Ticket.ID, Warnings: warnings}, nil
	}
	status := string(e.Ticket.Status)
	auto, ok := e.Workflow.AutoFrom(status)
	if !ok {
		res := e.idle()
		res.Warnings = warnings
		return res, nil
	}
	st := &State{Ticket: e.Ticket.ID, Workflow: e.Workflow.Name, Loops: map[string]int{}, Prints: map[string]string{}, Status: status}
	if auto.To != "" {
		if err := e.setStatus(st, auto.To); err != nil {
			return Result{}, err
		}
	}
	res, err := e.loop(ctx, st, auto.Start)
	res.Warnings = append(warnings, res.Warnings...)
	return res, err
}

// Resume は、止まった実行を再開の位置から続ける（人が原因を直した後）。ループの往復と step の数は数え直す。
func (e *Engine) Resume(ctx context.Context) (Result, error) {
	prev, dir, err := LatestState(e.runsDir())
	if err != nil {
		return Result{}, fmt.Errorf("%s に再開できる実行がない", e.Ticket.ID)
	}
	if prev.Stopped == nil || prev.Resumed || prev.Resume == "" {
		return Result{}, fmt.Errorf("%s の最後の実行（%s）は止まっていないか、すでに再開した", e.Ticket.ID, filepath.Base(dir))
	}
	if prev.Status != string(e.Ticket.Status) {
		return Result{}, fmt.Errorf("%s のステータスが、止まったとき（%s）から %s に変わっている", e.Ticket.ID, prev.Status, e.Ticket.Status)
	}
	prev.Resumed = true
	if err := prev.Save(dir); err != nil {
		return Result{}, err
	}
	st := &State{Ticket: e.Ticket.ID, Workflow: e.Workflow.Name, Loops: map[string]int{}, Prints: map[string]string{},
		Status: prev.Status, Feedback: prev.Feedback}
	return e.loop(ctx, st, prev.Resume)
}

func (e *Engine) runsDir() string { return filepath.Join(filepath.Dir(e.Ticket.Path), "runs") }

// DescribeTicket は、記録を読み込み済みの状態から、今の状態と人が次にすることを返す（ファイルを読まない）。
// latest は最新の実行の記録の状態、last は最後のステータスの書き込み（なければ nil）。監視の UI で使う。
func DescribeTicket(project *config.Project, wf *workflow.Workflow, t *ticket.Ticket, latest *State, last *Event) Result {
	e := &Engine{Project: project, Workflow: wf, Ticket: t}
	warnings := editWarnings(t, last)
	if t.Held() {
		return Result{Outcome: NothingToDo, Status: t.Status, Message: "保留", Next: "gateflow unhold " + t.ID, Warnings: warnings}
	}
	if _, ok := wf.AutoFrom(string(t.Status)); ok {
		return Result{Outcome: Runnable, Status: t.Status, Message: "gateflow が次の手順を実行する", Next: "gateflow run " + t.ID, Warnings: warnings}
	}
	res := e.idleWith(latest)
	res.Warnings = warnings
	return res
}

// Describe は、実行せずに、今の状態と人が次にすることを返す（status で使う）。
func (e *Engine) Describe() Result {
	s, _, _ := LatestState(e.runsDir())
	var last *Event
	if ev, ok := LastEvent(e.Ticket); ok {
		last = &ev
	}
	return DescribeTicket(e.Project, e.Workflow, e.Ticket, s, last)
}

// idle は、自動で進められない状態のときの結果（関門で待っている、終わっている、止まっている）。
func (e *Engine) idle() Result {
	s, _, _ := LatestState(e.runsDir())
	return e.idleWith(s)
}

func (e *Engine) idleWith(s *State) Result {
	status := string(e.Ticket.Status)
	if slices.Contains(e.Workflow.Terminal, status) {
		return Result{Outcome: Finished, Status: e.Ticket.Status, Message: "流れの終わりに着いている", Next: e.next(status)}
	}
	if s != nil && s.Stopped != nil && !s.Resumed && s.Status == status {
		return Result{Outcome: Stopped, Status: e.Ticket.Status, Reason: s.Stopped.Reason, Message: s.Stopped.Detail,
			Next: "原因を直してから gateflow resume " + e.Ticket.ID}
	}
	for _, h := range e.Workflow.Human {
		if slices.Contains(h.From, status) && h.Command != "cancel" {
			return Result{Outcome: Gate, Status: e.Ticket.Status, Message: "人間関門で待っている", Next: e.next(status)}
		}
	}
	return Result{Outcome: NothingToDo, Status: e.Ticket.Status, Message: fmt.Sprintf("状態 %s は gateflow が進める状態でない", status)}
}

// next は、状態 status で人が次にすることの案内。
func (e *Engine) next(status string) string {
	id := e.Ticket.ID
	switch status {
	case string(ticket.Draft):
		return fmt.Sprintf("質疑（%s）に回答し、状態を「確定」にしてから gateflow ready %s", filepath.Join(filepath.Dir(e.Ticket.Path), ticket.QuestionsFile), id)
	case string(ticket.ReadyToApprove):
		return fmt.Sprintf("作業ブランチ %s の差分、指示書の§3・§4、カタログの差分（実行の記録の arb-report.md）を確かめ、§4.5 をチェックしてから gateflow approve %s", e.Ticket.Branch, id)
	case string(ticket.ReadyForReview):
		s := fmt.Sprintf("作業ブランチ %s をコードレビューする", e.Ticket.Branch)
		if i := e.Project.Issue; i != nil && i.PRRef != "" && e.Ticket.IssueID() != "" {
			s += fmt.Sprintf("（PR の本文：%s）", i.Format(i.PRRef, e.Ticket.IssueID()))
		}
		return s + fmt.Sprintf("。main へ git でマージしてから gateflow done %s", id)
	}
	for _, h := range e.Workflow.Human {
		if slices.Contains(h.From, status) && h.Command != "cancel" {
			return fmt.Sprintf("gateflow %s %s", h.Command, id)
		}
	}
	return ""
}

// loop は、step から始めて、関門・終わり・止まるところまで進める。
func (e *Engine) loop(ctx context.Context, st *State, step string) (Result, error) {
	run, err := e.Ticket.NewRun(e.Now())
	if err != nil {
		return Result{}, err
	}
	e.run = run
	// 実行の間だけ印を置く（監視の UI が実行中と中断を見分ける）。取り消し（SIGINT・SIGTERM）で終わるときも消す
	unmark, err := run.MarkRunning(e.Now())
	if err != nil {
		e.log("running-mark-error", map[string]any{"error": err.Error()})
	}
	defer unmark()
	res, err := e.loopIn(ctx, run, st, step)
	res.Warnings = append(res.Warnings, e.warnings...)
	res.RunDir = run.Dir
	if res.Status == "" {
		res.Status = ticket.Status(st.Status)
	}
	if err != nil {
		e.log("error", map[string]any{"error": err.Error()})
		st.Stopped = &Stop{Reason: "error", Detail: err.Error()}
		st.Resume = st.Step
		st.Save(run.Dir)
		return res, err
	}
	e.log(string(res.Outcome), map[string]any{"reason": res.Reason, "message": res.Message, "status": string(res.Status)})
	return res, nil
}

func (e *Engine) loopIn(ctx context.Context, run *ticket.Run, st *State, step string) (Result, error) {
	limits := e.Project.EffectiveLimits()
	e.log("start", map[string]any{"ticket": e.Ticket.ID, "step": step, "status": st.Status, "agent": e.Agent.Name(), "image": e.AgentImage})
	if err := st.Save(run.Dir); err != nil {
		return Result{}, err
	}
	// P2 の段階では、作業ブランチに自分のコミット（ARB と人の決定）があっても main の進みをマージする（M2.6 の Q-01）
	mergeMain := slices.Contains([]string{string(ticket.Draft), string(ticket.ReadyToPrepare)}, st.Status)
	prep, err := e.Workspace.Prepare(ctx, e.Ticket.ID, e.Ticket.Branch, e.Project.Branch.Main, e.Project.Deps,
		workspace.PrepareOptions{MergeMain: mergeMain, Author: e.Author})
	if err != nil {
		return Result{}, err
	}
	e.log("workspace", map[string]any{"main": prep.Main, "head": prep.Head, "forwarded": prep.Forwarded, "behind": prep.Behind,
		"merged": prep.Merged, "synced": prep.Synced})
	if prep.Merged {
		// マージのコミットは clone にしかないので、プロジェクトの作業ブランチへ取り込む
		if err := e.Workspace.Import(ctx, e.Ticket.ID, e.Ticket.Branch); err != nil {
			return Result{}, err
		}
		e.log("import", map[string]any{"branch": e.Ticket.Branch, "reason": "merge-main"})
	}
	if prep.Behind {
		e.warnings = append(e.warnings, fmt.Sprintf("main が進んでいる（%s）が、作業ブランチに自分のコミットがあるので取り込んでいない。作業ブランチは main の古い版の上にある", short(prep.Main)))
	}
	allow := append(slices.Clone(e.Agent.Hosts()), e.Project.Agent.Allow...)
	os.WriteFile(filepath.Join(run.Dir, "allowlist.txt"), []byte(strings.Join(allow, "\n")+"\n"), 0o644)
	proxyURL, err := e.Egress.Start(ctx, allow)
	if err != nil {
		return Result{}, err
	}
	e.proxyURL = proxyURL
	stopProxy := e.followProxy(ctx, run.Dir)
	defer stopProxy()

	stopped := func(reason, detail, resume string) (Result, error) {
		st.Stopped = &Stop{Reason: reason, Detail: detail}
		st.Resume = resume
		if err := st.Save(run.Dir); err != nil {
			return Result{}, err
		}
		return Result{Outcome: Stopped, Reason: reason, Message: detail, Next: "原因を直してから gateflow resume " + e.Ticket.ID}, nil
	}

	for {
		if st.Steps >= limits.Steps {
			return stopped("limit-steps", fmt.Sprintf("step の数が上限（%d）に達した", limits.Steps), step)
		}
		if step == "p2-arbitrate" && st.Loops[arbitrateCounter] >= limits.ArbitrateLoops {
			// ARB の往復が上限に達したら、ARB に裁定させず、BJ の指摘をそのまま人へ回す（M2.6）
			if err := e.findingsToQuestions("p2-review"); err != nil {
				return Result{}, err
			}
			if err := e.setStatus(st, string(ticket.Draft)); err != nil {
				return Result{}, err
			}
			st.Stopped, st.Resume = nil, ""
			st.Save(run.Dir)
			e.log("arb-limit", map[string]any{"limit": limits.ArbitrateLoops})
			return Result{Outcome: Gate, Message: fmt.Sprintf("ARB の往復が上限（%d）に達したので、BJ の指摘を人へ回した", limits.ArbitrateLoops),
				Next: e.next(string(ticket.Draft))}, nil
		}
		st.Steps++
		st.Step = step
		if err := st.Save(run.Dir); err != nil {
			return Result{}, err
		}
		dir, err := run.StepDir(st.Steps, step)
		if err != nil {
			return Result{}, err
		}
		e.log("step-start", map[string]any{"step": step, "n": st.Steps})
		begin := e.Now()
		outcome, err := e.execStep(ctx, st, step, dir)
		e.log("step-end", map[string]any{"step": step, "outcome": outcome, "seconds": e.Now().Sub(begin).Seconds()})
		var se *errStop
		if errors.As(err, &se) {
			return stopped(se.reason, se.detail, step)
		}
		if err != nil {
			return Result{}, err
		}

		route := e.Workflow.Steps[step].On[outcome]
		failure, err := e.doActions(ctx, st, step, route.Actions)
		if err != nil {
			return Result{}, err
		}
		if failure != "" {
			// 機械検証に不合格なら、失敗の内容を付けて同じ step に差し戻す
			e.print = Fingerprint{Hash: hash(failure), Raw: failure, Normalized: failure}
			route = workflow.Route{Loop: step, Counter: "check-" + step}
			e.log("check-failed", map[string]any{"step": step, "failure": failure})
			e.writeFeedback(st, dir, failure)
		}

		switch {
		case route.Stop != "":
			detail := fmt.Sprintf("%s の結果が %s", step, outcome)
			if step == "p5" && e.lastBlock != "" {
				detail = e.lastBlock
			}
			return stopped(outcome, detail, step)
		case route.Next != "":
			step = route.Next
		case route.Loop != "":
			st.Loops[route.Counter]++
			n := st.Loops[route.Counter]
			e.log("loop", map[string]any{"counter": route.Counter, "count": n, "to": route.Loop})
			limit := limits.Loops
			if route.Counter == arbitrateCounter {
				limit = limits.ArbitrateLoops
			}
			if n > limit {
				return stopped("limit-loops", fmt.Sprintf("%s の往復が上限（%d）を超えた", route.Counter, limit), route.Loop)
			}
			if reason := e.noProgress(st, route.Counter, dir); reason != "" {
				return stopped("no-progress", reason, route.Loop)
			}
			if failure == "" {
				e.writeFeedback(st, dir, e.feedbackFor(step))
			}
			step = route.Loop
		case route.To != "":
			if err := e.setStatus(st, route.To); err != nil {
				return Result{}, err
			}
			st.Stopped, st.Resume = nil, ""
			st.Save(run.Dir)
			if auto, ok := e.Workflow.AutoFrom(route.To); ok && !e.Ticket.Held() {
				if auto.To != "" {
					if err := e.setStatus(st, auto.To); err != nil {
						return Result{}, err
					}
				}
				step = auto.Start
				continue
			}
			if slices.Contains(e.Workflow.Terminal, route.To) {
				return Result{Outcome: Finished, Message: "流れの終わりに着いた", Next: e.next(route.To)}, nil
			}
			return Result{Outcome: Gate, Message: "人間関門で止まった", Next: e.next(route.To)}, nil
		}
	}
}

// noProgress は、ループの往復で前回から進んでいなければ、その理由を返す（Q-08）。
func (e *Engine) noProgress(st *State, counter, dir string) string {
	if counter == arbitrateCounter {
		return "" // ARB の往復の進展なしは、裁定した指摘が残ったら人へ回す形で扱う（p2-arbitrate の forced）
	}
	if counter == "review" {
		prev := st.Findings
		st.Findings = e.findings
		if len(prev) > 0 && Overlaps(prev, e.findings) {
			return "前回と同じ指摘が残っている"
		}
		return ""
	}
	os.WriteFile(filepath.Join(dir, "fingerprint-raw.txt"), []byte(e.print.Raw), 0o644)
	os.WriteFile(filepath.Join(dir, "fingerprint-normalized.txt"), []byte(e.print.Normalized), 0o644)
	prev := st.Prints[counter]
	st.Prints[counter] = e.print.Hash
	if prev != "" && prev == e.print.Hash {
		return fmt.Sprintf("%s で前回と同じ失敗が続いている（指紋 %s）", counter, e.print.Hash)
	}
	return ""
}

func (e *Engine) writeFeedback(st *State, dir, text string) {
	if text == "" {
		st.Feedback = "" // 差し戻しの内容がない戻り（ARB の裁定の後の P2 など）は、差し戻しとして渡さない
		return
	}
	path := filepath.Join(dir, "feedback.md")
	os.WriteFile(path, []byte(text), 0o644)
	st.Feedback = path
}

// setStatus は、gateflow がステータスを書き、関門の記録にも残す。
func (e *Engine) setStatus(st *State, to string) error {
	from := e.Ticket.Status
	if err := e.Ticket.SetStatus(from, ticket.Status(to)); err != nil {
		return err
	}
	st.Status = to
	e.log("status", map[string]any{"from": string(from), "to": to})
	return RecordEvent(e.Ticket, "gateflow", "", string(from), to, e.Now())
}

// manualEditWarnings は、gateflow が最後に書いたステータスと指示書のステータスが違えば警告を返す（止めない）。
func (e *Engine) manualEditWarnings() []string {
	if ev, ok := LastEvent(e.Ticket); ok {
		return editWarnings(e.Ticket, &ev)
	}
	return nil
}

func editWarnings(t *ticket.Ticket, last *Event) []string {
	if last == nil || last.To == string(t.Status) {
		return nil
	}
	return []string{fmt.Sprintf("指示書のステータスが手で書き換えられている（gateflow が最後に書いたのは %s、今は %s）", last.To, t.Status)}
}

func (e *Engine) log(event string, fields map[string]any) {
	if e.run != nil {
		// NOTE: 記録の失敗で実行は止めない
		_ = e.run.Log(e.Now(), event, fields)
	}
}
