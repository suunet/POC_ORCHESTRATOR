package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gateflow/builtins"
	"gateflow/internal/agent"
	"gateflow/internal/agent/claude"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/engine"
	"gateflow/internal/ticket"
	"gateflow/internal/workspace"
)

// session は、プロジェクトで gateflow を動かすのに要るものをまとめる。
type session struct {
	root     string
	project  *config.Project
	personal *config.Personal
	agent    agent.Agent
	docker   container.Docker
	images   container.Images
}

// loadPersonal は、個人の設定を読み、使う AI サービスのアダプタを返す。
func loadPersonal() (*config.Personal, agent.Agent, error) {
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

func loadSession(dir string) (*session, error) {
	personal, ag, err := loadPersonal()
	if err != nil {
		return nil, err
	}
	project, err := config.LoadProject(dir)
	if err != nil {
		return nil, err
	}
	d := container.NewDocker()
	images, err := newImages(d)
	if err != nil {
		return nil, err
	}
	return &session{root: dir, project: project, personal: personal, agent: ag, docker: d, images: images}, nil
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

// cmdRun は、指示書 1 件の P5 を実行する（M1）。
func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	id := args[0]
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("project", ".", "プロジェクトのルート")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	s, err := loadSession(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	tk, err := ticket.Load(*dir, id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	author, err := gitAuthor(ctx, *dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	agentTag, ws, egress, err := s.prepare(ctx, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "%s の P5 を実行しています（エージェント：%s）…\n", tk.ID, s.agent.Name())
	p5 := &engine.P5{
		Root: *dir, Project: s.project, Personal: s.personal, Ticket: tk,
		AgentImage: agentTag, Agent: s.agent, Workspace: ws, Egress: egress, Runner: s.docker,
		Author: author, Builtins: builtins.FS(), Now: time.Now,
	}
	res, err := p5.Run(ctx)
	if res.RunDir != "" {
		fmt.Fprintf(stdout, "実行の記録：%s\n", res.RunDir)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "結果：%s %s\n", res.Outcome, res.Message)
	if res.Outcome != engine.Done {
		return exitFailed
	}
	return exitOK
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
	_, ag, err := loadPersonal()
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

// cmdDoctor は、環境の点検。M1 では sandbox（エージェントと同じ構成で外へ出られないこと）だけ。
func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "sandbox" {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet("doctor sandbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("project", ".", "プロジェクトのルート")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	s, err := loadSession(*dir)
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
	sb := &engine.Sandbox{Root: *dir, Project: s.project, AgentImage: agentTag, Agent: s.agent,
		Workspace: ws, Egress: egress, Runner: s.docker}
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
	}
	fmt.Fprintf(stdout, "サンドボックスの確認：%d件中 不合格 %d件\n", len(results), failed)
	if failed > 0 {
		return exitFailed
	}
	return exitOK
}
