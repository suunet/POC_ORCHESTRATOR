// Package engine は、遷移の評価、状態の2層管理、上限と進展なしの検知を扱う。
//
// M1 では、人が書いた指示書から P5 だけを実行する（遷移表は M2）。
package engine

import (
	"bytes"
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
	"gateflow/internal/gate"
	"gateflow/internal/output"
	"gateflow/internal/prompt"
	"gateflow/internal/ticket"
	"gateflow/internal/workspace"
)

// エージェントの時間の上限（設計の「暴走防止と通知」の初期値。M1 では固定）。
const (
	agentTimeout     = 60 * time.Minute
	agentIdleTimeout = 10 * time.Minute
	// 出力の形が違うときの再実行の回数。
	outputRetries = 1
)

// Outcome は、P5 の実行の結果。
type Outcome string

const (
	Done          Outcome = "done"           // 検証に合格し、コミットして取り込んだ
	Blocked       Outcome = "blocked"        // エージェントが範囲外・依存の追加などで止まった（人の確認待ち）
	TestDefect    Outcome = "test_defect"    // エージェントがテストの誤りを報告した（人の確認待ち）
	InvalidOutput Outcome = "invalid_output" // 出力の形が再実行しても違う（人の確認待ち）
	VerifyFailed  Outcome = "verify_failed"  // 検証コマンドに不合格（M1 では差し戻さず止める）
	TimedOut      Outcome = "timeout"        // 時間の上限に達した（人の確認待ち）
)

// Result は、P5 の実行の結果と記録の場所。
type Result struct {
	Outcome Outcome
	Message string
	RunDir  string
}

// Workspace は、タスクごとの clone の操作。
type Workspace interface {
	Prepare(ctx context.Context, ticketID, branch, mainBranch string, deps *config.Deps) error
	AgentMounts(ticketID string) []container.Mount
	Commit(ctx context.Context, ticketID, message string, author workspace.Author) error
	Import(ctx context.Context, ticketID, branch string) error
}

// Egress は、内部ネットワークと許可リストのプロキシ。
type Egress interface {
	Start(ctx context.Context, allow []string) (string, error)
	Stop(ctx context.Context) ([]byte, error)
	Network() string
}

// Runner は、コンテナを起動する。
type Runner interface {
	Run(ctx context.Context, spec container.Spec, stdout, stderr io.Writer) (int, error)
}

// P5 は、1件の指示書の P5 を実行する。
type P5 struct {
	Root       string // プロジェクトのルート（ホスト）
	Project    *config.Project
	Personal   *config.Personal
	Ticket     *ticket.Ticket
	AgentImage string
	Agent      agent.Agent
	Workspace  Workspace
	Egress     Egress
	Runner     Runner
	Author     workspace.Author
	Builtins   fs.FS
	Now        func() time.Time
}

// Run は、P5 を実行する。止まった場合（blocked など）も Result で返し、続けられない誤りだけを error で返す。
func (p *P5) Run(ctx context.Context) (Result, error) {
	if err := p.Ticket.SetStatus(ticket.ReadyToImplement, ticket.Implementing); err != nil {
		return Result{}, err
	}
	run, err := p.Ticket.NewRun(p.Now())
	if err != nil {
		return Result{}, err
	}
	res, err := p.run(ctx, run)
	res.RunDir = run.Dir
	if err != nil {
		p.log(run, "error", map[string]any{"error": err.Error()})
		return res, err
	}
	p.log(run, string(res.Outcome), map[string]any{"message": res.Message})
	return res, nil
}

func (p *P5) run(ctx context.Context, run *ticket.Run) (Result, error) {
	t := p.Ticket
	p.log(run, "start", map[string]any{"ticket": t.ID, "branch": t.Branch, "agent": p.Agent.Name(), "image": p.AgentImage})
	step, err := run.StepDir(1, "p5")
	if err != nil {
		return Result{}, err
	}
	outDir := filepath.Join(step, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Result{}, err
	}
	root, err := filepath.Abs(p.Root)
	if err != nil {
		return Result{}, err
	}
	envFile := filepath.Join(root, p.Project.Agent.EnvTemplate)
	if _, err := os.Stat(envFile); err != nil {
		return Result{}, fmt.Errorf(".env のひな形 %s がない: %w", p.Project.Agent.EnvTemplate, err)
	}

	start := p.Now()
	if err := p.Workspace.Prepare(ctx, t.ID, t.Branch, p.Project.Branch.Main, p.Project.Deps); err != nil {
		return Result{}, err
	}
	p.log(run, "prepare", map[string]any{"seconds": p.Now().Sub(start).Seconds()})

	allow := append(slices.Clone(p.Agent.Hosts()), p.Project.Agent.Allow...)
	if err := os.WriteFile(filepath.Join(step, "allowlist.txt"), []byte(strings.Join(allow, "\n")+"\n"), 0o644); err != nil {
		return Result{}, err
	}
	proxyURL, err := p.Egress.Start(ctx, allow)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		logs, err := p.Egress.Stop(context.WithoutCancel(ctx))
		os.WriteFile(filepath.Join(step, "proxy.log"), logs, 0o644)
		if err != nil {
			p.log(run, "proxy-stop-error", map[string]any{"error": err.Error()})
		}
	}()

	inputs := []container.Mount{{Type: container.Bind, Source: t.Path, Target: "/in/ticket.md", ReadOnly: true}}
	var policies []prompt.PolicyFile
	for _, pol := range p.Project.Policies {
		if !slices.Contains(pol.Roles, "dev") {
			continue
		}
		src := filepath.Join(root, pol.Path)
		if _, err := os.Stat(src); err != nil {
			// 設計の「規約の目次」：ファイルがない規約は渡さず、実行の記録に残す
			p.log(run, "policy-missing", map[string]any{"path": pol.Path})
			continue
		}
		target := "/in/policies/" + filepath.ToSlash(pol.Path)
		inputs = append(inputs, container.Mount{Type: container.Bind, Source: src, Target: target, ReadOnly: true})
		policies = append(policies, prompt.PolicyFile{Path: target, Purpose: pol.Purpose})
	}

	req, err := p.prompt(policies)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(step, "prompt.md"), []byte("# システムプロンプト（追加分）\n\n"+req.System+"\n\n# ユーザーメッセージ\n\n"+req.Prompt), 0o644); err != nil {
		return Result{}, err
	}

	cmd, stdin := p.Agent.Command(req)
	spec := agentSpec(fmt.Sprintf("gateflow-%s-%s-p5", p.Project.Name, strings.ToLower(t.ID)), p.AgentImage,
		p.Egress.Network(), proxyURL, p.Agent, p.Project, p.Workspace.AgentMounts(t.ID), envFile, outDir, inputs)
	spec.Cmd = cmd
	spec.Timeout, spec.IdleTimeout = agentTimeout, agentIdleTimeout

	var out *output.P5Result
	for attempt := 1; attempt <= 1+outputRetries; attempt++ {
		os.Remove(filepath.Join(outDir, "result.json"))
		spec.Stdin = strings.NewReader(stdin)
		begin := p.Now()
		code, err := p.runLogged(ctx, spec, step, fmt.Sprintf("agent-%d", attempt))
		p.log(run, "agent-end", map[string]any{"attempt": attempt, "exitCode": code, "seconds": p.Now().Sub(begin).Seconds()})
		if errors.Is(err, container.ErrTimeout) || errors.Is(err, container.ErrIdleTimeout) {
			return Result{Outcome: TimedOut, Message: err.Error()}, nil
		}
		if err != nil {
			return Result{}, err
		}
		data, readErr := os.ReadFile(filepath.Join(outDir, "result.json"))
		if readErr == nil {
			out, err = output.ParseP5(data)
		} else {
			err = fmt.Errorf("%s がない", output.ResultPath)
		}
		if err == nil {
			break
		}
		p.log(run, "output-invalid", map[string]any{"attempt": attempt, "error": err.Error()})
		out = nil
	}
	if out == nil {
		return Result{Outcome: InvalidOutput, Message: "出力の形が再実行しても正しくない"}, nil
	}
	switch out.Outcome {
	case output.OutcomeBlocked:
		return Result{Outcome: Blocked, Message: fmt.Sprintf("%s: %s", out.BlockReason.Kind, out.BlockReason.Detail)}, nil
	case output.OutcomeTestDefect:
		return Result{Outcome: TestDefect, Message: out.Summary}, nil
	}

	g := gate.Gate{Runner: p.Runner, Base: container.Spec{
		Name:    fmt.Sprintf("gateflow-%s-%s-verify", p.Project.Name, strings.ToLower(t.ID)),
		Image:   p.AgentImage,
		Network: "none",
		User:    workspace.User,
		Workdir: "/work",
		Env:     offlineEnv(p.Project),
		Mounts: append(p.Workspace.AgentMounts(t.ID),
			container.Mount{Type: container.Bind, Source: envFile, Target: "/work/.env", ReadOnly: true}),
	}}
	results, passed, err := g.Run(ctx, gate.Commands(p.Project.Verify))
	var failed []string
	for _, r := range results {
		os.WriteFile(filepath.Join(step, "verify-"+r.Name+".log"), []byte(r.Output), 0o644)
		p.log(run, "verify", map[string]any{"name": r.Name, "exitCode": r.ExitCode, "seconds": r.Duration.Seconds()})
		if r.ExitCode != 0 {
			failed = append(failed, r.Name)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if !passed {
		return Result{Outcome: VerifyFailed, Message: "不合格: " + strings.Join(failed, "、")}, nil
	}

	msg := fmt.Sprintf("%s(%s): %s", commitType(t.Kind), t.ID, t.Title)
	if err := p.Workspace.Commit(ctx, t.ID, msg, p.Author); err != nil {
		return Result{}, err
	}
	p.log(run, "commit", map[string]any{"message": msg})
	if err := p.Workspace.Import(ctx, t.ID, t.Branch); err != nil {
		return Result{}, err
	}
	p.log(run, "import", map[string]any{"branch": t.Branch})
	if err := t.SetStatus(ticket.Implementing, ticket.ReadyForReview); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Done, Message: fmt.Sprintf("作業ブランチ %s を取り込んだ", t.Branch)}, nil
}

func (p *P5) prompt(policies []prompt.PolicyFile) (agent.Request, error) {
	persona, err := prompt.LoadBuiltin(p.Builtins, "prompts/personas/dev.md")
	if err != nil {
		return agent.Request{}, err
	}
	instruction, err := prompt.LoadBuiltin(p.Builtins, "prompts/instructions/p5.md")
	if err != nil {
		return agent.Request{}, err
	}
	testFirst, err := prompt.LoadBuiltin(p.Builtins, "prompts/policies/test-first.md")
	if err != nil {
		return agent.Request{}, err
	}
	var verify strings.Builder
	verify.WriteString("\n### 検証\n\n実装の後に、次のコマンドを作業場所（/work）で実行し、すべて合格することを確かめる。gateflow も同じコマンドで確かめる。\n\n")
	for _, c := range gate.Commands(p.Project.Verify) {
		fmt.Fprintf(&verify, "- %s：`%s`\n", c.Name, c.Cmd)
	}
	model := ""
	if p.Personal != nil {
		model = p.Personal.Model
	}
	return prompt.Build(prompt.Input{
		Persona: persona,
		Knowledge: []string{
			fmt.Sprintf("指示書は`/in/ticket.md`（%s %s）", p.Ticket.ID, p.Ticket.Title),
			"作業場所は`/work`（プロジェクトのリポジトリの作業ツリー。作業ブランチは用意済み）",
			"カタログ（要件の正本）は`/work/.docs/design/`",
			"依存のキャッシュは用意済みで、外からは取得できない",
		},
		Instruction:    instruction + verify.String(),
		OutputContract: output.DescribeP5(),
		Policies:       policies,
		BuiltinPolicy:  []string{testFirst},
		Model:          model,
	}), nil
}

// runLogged は、コンテナを起動し、標準出力と標準エラーを step の <name>.jsonl と <name>.stderr に残す。
func (p *P5) runLogged(ctx context.Context, spec container.Spec, step, name string) (int, error) {
	stdout, err := os.Create(filepath.Join(step, name+".jsonl"))
	if err != nil {
		return 0, err
	}
	defer stdout.Close()
	var stderr bytes.Buffer
	code, runErr := p.Runner.Run(ctx, spec, stdout, &stderr)
	os.WriteFile(filepath.Join(step, name+".stderr"), stderr.Bytes(), 0o644)
	return code, runErr
}

func (p *P5) log(run *ticket.Run, event string, fields map[string]any) {
	// NOTE: 記録の失敗で実行は止めない（記録はあとで調べるためのもの）
	_ = run.Log(p.Now(), event, fields)
}

func commitType(k ticket.Kind) string {
	if k == ticket.KindDocument {
		return "docs"
	}
	return "feat"
}
