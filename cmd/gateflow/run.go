package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gateflow/builtins"
	"gateflow/internal/agent"
	"gateflow/internal/agent/claude"
	"gateflow/internal/agent/mock"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/engine"
	"gateflow/internal/idcheck"
	agentq "gateflow/internal/queue"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
	"gateflow/internal/workspace"
)

// session は、プロジェクトで gateflow を動かすのに要るものをまとめる。
type session struct {
	root     string
	project  *config.Project
	personal *config.Personal
	agent    agent.Agent
	workflow *workflow.Workflow
	rules    *idcheck.Rules
	docker   container.Docker
	images   container.Images
}

// loadAgent は、使う AI サービスのアダプタを返す。
//
// NOTE: ダミーの AI は、環境変数 GATEFLOW_AGENT=mock と GATEFLOW_MOCK_SCENARIO（筋書きのファイル）でだけ選べる。
// 使い方の表示には載せない（個人の設定の agent には書かせない）
func loadAgent() (*config.Personal, agent.Agent, error) {
	if os.Getenv("GATEFLOW_AGENT") == "mock" {
		data, err := os.ReadFile(os.Getenv("GATEFLOW_MOCK_SCENARIO"))
		if err != nil {
			return nil, nil, fmt.Errorf("ダミーの AI の筋書きを読めない: %w", err)
		}
		sc, err := mock.ParseScenario(data)
		if err != nil {
			return nil, nil, err
		}
		return &config.Personal{Agent: "mock"}, &mock.Agent{Scenario: sc}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	personal, err := config.LoadPersonal(home)
	if err != nil {
		return nil, nil, err
	}
	switch personal.Agent {
	case config.AgentClaude:
		return personal, claude.Agent{}, nil
	default:
		return nil, nil, fmt.Errorf("AI サービス %q のアダプタがない", personal.Agent)
	}
}

func newImages(d container.Docker) (container.Images, error) {
	exe, err := os.Executable()
	if err != nil {
		return container.Images{}, err
	}
	tool, err := container.FindTool(exe, os.Getenv("GATEFLOW_HOME"))
	if err != nil {
		return container.Images{}, err
	}
	return container.Images{Docker: d, Tool: tool, Version: version}, nil
}

// loadProject は、プロジェクトの設定・遷移表・ID の規則を読む（docker と AI サービスは使わない）。
func loadProject(dir string) (*config.Project, *workflow.Workflow, *idcheck.Rules, error) {
	project, err := config.LoadProject(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	wf, err := workflow.LoadBuiltin(builtins.FS(), project.WorkflowName())
	if err != nil {
		return nil, nil, nil, err
	}
	rules, err := idcheck.NewRules(*project.IDs)
	if err != nil {
		return nil, nil, nil, err
	}
	return project, wf, rules.WithLevels(project.Levels), nil
}

func loadSession(dir string) (*session, error) {
	personal, ag, err := loadAgent()
	if err != nil {
		return nil, err
	}
	project, wf, rules, err := loadProject(dir)
	if err != nil {
		return nil, err
	}
	d := container.NewDocker()
	images, err := newImages(d)
	if err != nil {
		return nil, err
	}
	return &session{root: dir, project: project, personal: personal, agent: ag, workflow: wf, rules: rules, docker: d, images: images}, nil
}

// prepare は、プロキシとエージェントのイメージを作り、workspace と内部ネットワークを返す。
func (s *session) prepare(ctx context.Context, stdout io.Writer) (string, *workspace.Workspace, container.Egress, error) {
	fmt.Fprintln(stdout, "イメージを用意しています…")
	proxyTag, err := s.images.BuildProxy(ctx, runtime.GOARCH)
	if err != nil {
		return "", nil, container.Egress{}, err
	}
	agentTag, err := s.images.BuildAgent(ctx, s.root, s.project)
	if err != nil {
		return "", nil, container.Egress{}, err
	}
	ws := workspace.New(s.root, s.project.Name, agentTag, s.docker)
	egress := container.Egress{Docker: s.docker, Project: s.project.Name, ProxyImage: proxyTag}
	return agentTag, ws, egress, nil
}

// projectFlags は、--project を持つコマンドの引数を読み、位置の引数（チケットの ID など）を返す。
func projectFlags(name string, args []string, stderr io.Writer) (dir string, rest []string, ok bool) {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	d := fs.String("project", ".", "プロジェクトのルート")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return "", nil, false
	}
	return *d, pos, true
}

// cmdRun は、指定したチケット（なければ進められるチケットを順に）を人間関門まで進める。
func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir, ids, ok := projectFlags("run", args, stderr)
	if !ok || len(ids) > 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	return runTickets(ctx, dir, ids, false, stdout, stderr)
}

// cmdResume は、止まった実行を再開の位置から続ける。
func cmdResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir, ids, ok := projectFlags("resume", args, stderr)
	if !ok || len(ids) != 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	return runTickets(ctx, dir, ids, true, stdout, stderr)
}

func runTickets(ctx context.Context, dir string, ids []string, resume bool, stdout, stderr io.Writer) int {
	s, err := loadSession(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	var tickets []*ticket.Ticket
	if len(ids) == 1 {
		tk, err := ticket.Load(dir, ids[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		tickets = append(tickets, tk)
	} else {
		tickets, err = queue(dir, s.workflow)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		if len(tickets) == 0 {
			fmt.Fprintln(stdout, "進められるチケットはない")
			return exitOK
		}
	}
	author, err := gitAuthor(ctx, dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	image, ws, egress, err := s.prepare(ctx, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	q, err := agentQueue(s.agent)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	code := exitOK
	var summary []string
	for _, tk := range tickets {
		e := &engine.Engine{
			Root: dir, Project: s.project, Personal: s.personal, Workflow: s.workflow, Ticket: tk,
			AgentImage: image, Agent: s.agent, Workspace: ws, Egress: egress.For(tk.ID), Runner: s.docker,
			Author: author, Builtins: builtins.FS(), Rules: s.rules, Now: time.Now, Queue: q,
		}
		if sv, ok := projectServices(s.docker, dir, s.project); ok {
			e.Services = sv
		}
		fmt.Fprintf(stdout, "%s を進めています（%s、エージェント：%s）…\n", tk.ID, tk.Status, s.agent.Name())
		var res engine.Result
		if resume {
			res, err = e.Resume(ctx)
		} else {
			res, err = e.Run(ctx)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s：%v\n", tk.ID, err)
			if res.RunDir != "" {
				fmt.Fprintf(stderr, "実行の記録：%s\n", res.RunDir)
			}
			code = exitFailed
			summary = append(summary, fmt.Sprintf("%s：誤りで止まった（%v）", tk.ID, err))
			continue // 1件の停止で待ち行列全体を塞がない（Q-12）
		}
		printResult(stdout, tk.ID, res)
		if res.Outcome == engine.Stopped {
			code = exitFailed
		}
		summary = append(summary, summaryLine(tk.ID, res))
	}
	if len(tickets) > 1 {
		fmt.Fprintln(stdout, "\nまとめ：")
		for _, l := range summary {
			fmt.Fprintln(stdout, "- "+l)
		}
	}
	return code
}

// queue は、進められるチケット（自動で始まる状態で、保留でないもの）を、その状態になった時刻の古い順に返す。
func queue(dir string, wf *workflow.Workflow) ([]*ticket.Ticket, error) {
	entries, err := os.ReadDir(filepath.Join(dir, ticket.Dir))
	if err != nil {
		return nil, nil
	}
	type item struct {
		tk   *ticket.Ticket
		time string
	}
	var items []item
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		tk, err := ticket.Load(dir, en.Name())
		if err != nil {
			continue
		}
		if _, ok := wf.AutoFrom(string(tk.Status)); !ok || tk.Held() {
			continue
		}
		// 承認（ready・approve）で今の状態になった時刻。記録がなければ最後に回す
		when := "~"
		for _, ev := range engine.Events(tk) {
			if ev.To == string(tk.Status) {
				when = ev.Time
			}
		}
		items = append(items, item{tk, when})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].time != items[j].time {
			return items[i].time < items[j].time
		}
		return items[i].tk.ID < items[j].tk.ID
	})
	out := make([]*ticket.Ticket, 0, len(items))
	for _, it := range items {
		out = append(out, it.tk)
	}
	return out, nil
}

var outcomeText = map[engine.Outcome]string{
	engine.Gate: "人間関門で待っている", engine.Finished: "流れの終わりに着いた",
	engine.Stopped: "人の確認待ちで止まった", engine.NothingToDo: "進められない",
	engine.Runnable: "gateflow が進められる",
}

func printResult(w io.Writer, id string, res engine.Result) {
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "警告：%s\n", warn)
	}
	fmt.Fprintf(w, "%s：%s（ステータス：%s）", id, outcomeText[res.Outcome], res.Status)
	if res.Reason != "" {
		fmt.Fprintf(w, "。理由：%s", res.Reason)
	}
	fmt.Fprintln(w)
	if res.Message != "" {
		fmt.Fprintf(w, "  %s\n", res.Message)
	}
	if res.Next != "" {
		fmt.Fprintf(w, "  次：%s\n", res.Next)
	}
	if res.RunDir != "" {
		fmt.Fprintf(w, "  実行の記録：%s\n", res.RunDir)
	}
}

func summaryLine(id string, res engine.Result) string {
	s := fmt.Sprintf("%s：%s（%s）", id, outcomeText[res.Outcome], res.Status)
	if res.Reason != "" {
		s += "、理由：" + res.Reason
	}
	if res.Next != "" {
		s += "。次：" + res.Next
	}
	return s
}

// cmdHuman は、関門のコマンド（ready・approve・done・hold・unhold・cancel）を実行する。docker と AI サービスは使わない。
func cmdHuman(command string, args []string, stdout, stderr io.Writer) int {
	dir, ids, ok := projectFlags(command, args, stderr)
	if !ok || len(ids) != 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	project, wf, _, err := loadProject(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	tk, err := ticket.Load(dir, ids[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	checks := engine.Checks{
		BranchMerged: func(branch string) error { return branchMerged(dir, project.Branch.Main, branch) },
		MainIDsClean: func() error { return mainIDsClean(dir, project) },
	}
	if err := engine.Human(tk, wf, command, time.Now(), checks); err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	e := &engine.Engine{Project: project, Workflow: wf, Ticket: tk}
	printResult(stdout, tk.ID, e.Describe())
	return exitOK
}

// cmdStatus は、チケットの状態と、人が次にすることを表示する（run の最後と同じ内容）。
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	dir, ids, ok := projectFlags("status", args, stderr)
	if !ok || len(ids) > 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	project, wf, _, err := loadProject(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	var tickets []*ticket.Ticket
	if len(ids) == 1 {
		tk, err := ticket.Load(dir, ids[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		tickets = append(tickets, tk)
	} else {
		entries, _ := os.ReadDir(filepath.Join(dir, ticket.Dir))
		for _, en := range entries {
			if tk, err := ticket.Load(dir, en.Name()); err == nil {
				tickets = append(tickets, tk)
			}
		}
	}
	for _, tk := range tickets {
		e := &engine.Engine{Project: project, Workflow: wf, Ticket: tk}
		printResult(stdout, tk.ID, e.Describe())
	}
	return exitOK
}

// branchMerged は、プロジェクトの git で、作業ブランチの先頭が main に含まれている（祖先である）かを確かめる。
// 読むだけで、main には書き込まない（マージは人が git で行う）。
func branchMerged(dir, mainBranch, branch string) error {
	git := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	}
	head, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("作業ブランチ %s がない", branch)
	}
	if _, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+mainBranch+"^{commit}"); err != nil {
		return fmt.Errorf("main のブランチ %s がない", mainBranch)
	}
	_, err = git("merge-base", "--is-ancestor", "refs/heads/"+branch, "refs/heads/"+mainBranch)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return fmt.Errorf("作業ブランチ %s の先頭 %.7s が %s に含まれていない。git で %s へマージしてから done する", branch, strings.TrimSpace(string(head)), mainBranch, mainBranch)
	default:
		return fmt.Errorf("作業ブランチ %s のマージを確かめられない: %w", branch, err)
	}
}

// gitAuthor は、ホストの git の設定から、コミットの名義（人）を返す。
func gitAuthor(ctx context.Context, dir string) (workspace.Author, error) {
	get := func(key string) string {
		out, _ := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", key).Output()
		return strings.TrimSpace(string(out))
	}
	a := workspace.Author{Name: get("user.name"), Email: get("user.email")}
	if a.Name == "" || a.Email == "" {
		return a, fmt.Errorf("git の user.name と user.email が設定されていない（コミットの名義に使う）")
	}
	return a, nil
}

// cmdLogin は、AI サービスに対話の端末でログインし、認証の情報をボリュームに保存する。
func cmdLogin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	_, ag, err := loadAgent()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	d := container.NewDocker()
	images, err := newImages(d)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	fmt.Fprintln(stdout, "イメージを用意しています…")
	if err := images.BuildBase(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	// ログインも認証ボリュームを書くので、AI の呼び出しの順番を待つ。待つときは、何を待っているかを画面に出す（M2.9）
	q, err := agentQueue(ag)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	self, err := agentq.Self("", "", "login", time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	release, _, err := q.Acquire(ctx, self, func(h agentq.Holder) {
		fmt.Fprintf(stdout, "いま %s の AI が動いています。終わるのを待っています\n", holderName(h))
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	defer func() {
		if err := release(); err != nil {
			fmt.Fprintf(stderr, "順番待ちの印を外せない：%v\n", err)
		}
	}()
	fmt.Fprintf(stdout, "%s にログインします。ログインが終わったら終了してください。\n", ag.Name())
	spec := container.Spec{
		Name: "gateflow-login-" + ag.Name(), Image: images.BaseTag(), User: workspace.User,
		Env: ag.Env(), Mounts: []container.Mount{ag.AuthMount()}, Cmd: ag.LoginCommand(),
		Stdin: os.Stdin, TTY: true,
	}
	code, err := d.Run(ctx, spec, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	if code != 0 {
		return exitFailed
	}
	return exitOK
}

// cmdDoctor は、環境の点検。sandbox（エージェントと同じ構成で外へ出られないこと）。
func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "sandbox" {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	dir, rest, ok := projectFlags("doctor sandbox", args[1:], stderr)
	if !ok || len(rest) > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	s, err := loadSession(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	agentTag, ws, egress, err := s.prepare(ctx, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	fmt.Fprintln(stdout, "エージェントと同じ構成のサンドボックスで確かめています…")
	q, err := agentQueue(s.agent)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	sb := &engine.Sandbox{Root: dir, Project: s.project, AgentImage: agentTag, WorkVolume: ws.Volume(), Agent: s.agent,
		Workspace: ws, Egress: egress.For("doctor"), Runner: s.docker, Queue: q, Notify: func(event string, f map[string]any) {
			if event == "agent-wait" {
				fmt.Fprintf(stdout, "いま %s の AI が動いています。終わるのを待っています\n", holderName(agentq.Holder{
					Project: fmt.Sprint(f["holderProject"]), Ticket: fmt.Sprint(f["holderTicket"]), Step: fmt.Sprint(f["holderStep"])}))
			}
		}}
	results, err := sb.Run(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	failed := 0
	for _, r := range results {
		mark := "OK "
		if !r.Passed {
			mark = "NG "
			failed++
		}
		want := "失敗する"
		if r.Expect == "ok" {
			want = "成功する"
		}
		fmt.Fprintf(stdout, "%s [%s] %s（%sこと。終了コード %d）\n", mark, r.Group, r.Desc, want, r.ExitCode)
		if r.Advice != "" {
			fmt.Fprintf(stdout, "    直し方：%s\n", r.Advice)
		}
	}
	fmt.Fprintf(stdout, "サンドボックスの確認：%d件中 不合格 %d件\n", len(results), failed)
	printProxyLog(stdout, dir, sb.ProxyLog)
	if sb.CleanupErr != nil {
		fmt.Fprintf(stdout, "後片付けに失敗した（プロキシ）：%v\n", sb.CleanupErr)
	}
	if failed > 0 {
		return exitFailed
	}
	return exitOK
}

// printProxyLog は、プロキシの判定の記録を数えて表示し、Git 管理外の .docs/_stash/doctor/ に保存する。
func printProxyLog(w io.Writer, dir string, log []byte) {
	counts := map[string]int{}
	var keys []string
	for _, line := range strings.Split(string(log), "\n") {
		var rec struct{ Decision, Method, Target string }
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Decision == "" {
			continue
		}
		k := rec.Decision + " " + rec.Method + " " + rec.Target
		if counts[k] == 0 {
			keys = append(keys, k)
		}
		counts[k]++
	}
	sort.Strings(keys)
	fmt.Fprintln(w, "プロキシの判定：")
	for _, k := range keys {
		fmt.Fprintf(w, "  %3d %s\n", counts[k], k)
	}
	saveDir := filepath.Join(dir, ".docs", "_stash", "doctor")
	path := filepath.Join(saveDir, time.Now().UTC().Format("20060102-150405")+"-proxy.log")
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		fmt.Fprintf(w, "  記録を書けない：%v\n", err)
	} else if err := os.WriteFile(path, log, 0o644); err != nil {
		fmt.Fprintf(w, "  記録を書けない：%v\n", err)
	} else {
		fmt.Fprintf(w, "  記録：%s\n", path)
	}
}

// agentQueue は、エージェントの認証ボリュームの、AI の呼び出しの順番待ちの印を返す（M2.9）。
func agentQueue(ag agent.Agent) (*agentq.Lock, error) {
	return agentq.New(ag.AuthMount().Source)
}

// holderName は、順番待ちの印を持っているものを、人に見せる名前にする（チケット、なければ login など）。
func holderName(h agentq.Holder) string {
	switch {
	case h.Ticket != "" && h.Project != "":
		return h.Project + " の " + h.Ticket
	case h.Ticket != "":
		return h.Ticket
	case h.Step != "":
		return h.Step
	}
	return "ほかの gateflow"
}

// mainIDsClean は、main の木（git archive。ホストの作業ツリーは読まない）で ID 整合チェックを行い、必須の指摘があれば
// 理由を返す（done の検査。並行の実行で main に ID の重複が入っていないか。M2.9）。
func mainIDsClean(dir string, project *config.Project) error {
	if project.IDs == nil {
		return nil
	}
	rules, err := idcheck.NewRules(*project.IDs)
	if err != nil {
		return err
	}
	rules = rules.WithLevels(project.Levels)
	tmp, err := os.MkdirTemp("", "gateflow-main-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := workspace.ArchiveTree(ctx, dir, "refs/heads/"+project.Branch.Main, tmp); err != nil {
		return err
	}
	res, err := idcheck.Run(tmp, rules)
	if err != nil {
		return err
	}
	var msgs []string
	for _, f := range res.Findings {
		if f.Level == idcheck.Required {
			msgs = append(msgs, fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Message))
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("main の ID 整合チェックに必須の指摘がある（ほかのチケットと ID が重なっているかもしれない。main を直してから done する）：%s", strings.Join(msgs, "／"))
	}
	return nil
}

// projectServices は、project.json の services の共有のサービスを返す（なければ ok が false。M3）。
func projectServices(d container.Docker, dir string, p *config.Project) (container.Services, bool) {
	if p.Services == nil {
		return container.Services{}, false
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		root = dir
	}
	return container.Services{Docker: d, Project: p.Name, Root: root, Compose: p.Services.Compose, Names: p.Services.Names}, true
}

// cmdServices は、gateflow services down：共有のサービスを止めて消す（共有なので run は止めない。人が行う。M3）。
func cmdServices(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "down" {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet("services", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("project", ".", "プロジェクトのルート")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	p, err := config.LoadProject(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	sv, ok := projectServices(container.NewDocker(), *dir, p)
	if !ok {
		fmt.Fprintln(stdout, "project.json に services がないので、止めるサービスはない")
		return exitOK
	}
	if err := sv.Down(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "共有のサービス（%s）を止めた\n", strings.Join(p.Services.Names, "・"))
	return exitOK
}
