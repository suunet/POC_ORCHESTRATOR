package engine_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/builtins"
	"gateflow/internal/agent/mock"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/engine"
	"gateflow/internal/ticket"
	"gateflow/internal/workspace"
)

const ticketBody = `# T-001 記事を公開する

ステータス: ready-to-implement

| 項目 | 値 |
| --- | --- |
| 種別 | 実装 |
| ブランチ | feature/t-001-publish |
`

const doneResult = `{"outcome": "done", "summary": "実装した", "tests": [{"test": "TestPublish", "file": "a_test.go", "acceptance": ["UC-011-AC-01"]}]}`

type fakeWorkspace struct {
	prepared bool
	commits  []string
	imports  []string
}

func (w *fakeWorkspace) Prepare(_ context.Context, _, _, _ string, _ *config.Deps) error {
	w.prepared = true
	return nil
}

func (w *fakeWorkspace) AgentMounts(ticketID string) []container.Mount {
	return []container.Mount{
		{Type: container.Volume, Source: "gateflow-work-sample", Subpath: ticketID + "/repo", Target: "/work"},
		{Type: container.Volume, Source: "gateflow-work-sample", Subpath: ticketID + "/repo/.git", Target: "/work/.git", ReadOnly: true},
	}
}

func (w *fakeWorkspace) Commit(_ context.Context, _, msg string, _ workspace.Author) error {
	w.commits = append(w.commits, msg)
	return nil
}

func (w *fakeWorkspace) Import(_ context.Context, _, branch string) error {
	w.imports = append(w.imports, branch)
	return nil
}

type fakeEgress struct{ started, stopped bool }

func (e *fakeEgress) Start(context.Context, []string) (string, error) {
	e.started = true
	return "http://gateflow-proxy:3128", nil
}
func (e *fakeEgress) Stop(context.Context) ([]byte, error) {
	e.stopped = true
	return []byte(`{"decision":"deny","target":"example.com:443"}` + "\n"), nil
}
func (e *fakeEgress) Network() string { return "gateflow-sample-internal" }

// fakeRunner は、エージェントの代わりに results を順に /out/result.json に書き、検証コマンドには verifyCode を返す。
type fakeRunner struct {
	results    []string
	verifyCode int
	agentSpecs []container.Spec
	verify     []container.Spec
	stdin      []string
}

func (f *fakeRunner) Run(_ context.Context, spec container.Spec, stdout, _ io.Writer) (int, error) {
	if !strings.HasSuffix(spec.Name, "-p5") {
		f.verify = append(f.verify, spec)
		return f.verifyCode, nil
	}
	f.agentSpecs = append(f.agentSpecs, spec)
	if spec.Stdin != nil {
		b, _ := io.ReadAll(spec.Stdin)
		f.stdin = append(f.stdin, string(b))
	}
	io.WriteString(stdout, `{"type":"result"}`+"\n")
	for _, m := range spec.Mounts {
		if m.Target == "/out" && len(f.results) > 0 {
			os.WriteFile(filepath.Join(m.Source, "result.json"), []byte(f.results[0]), 0o644)
			f.results = f.results[1:]
		}
	}
	return 0, nil
}

type fixture struct {
	p5     *engine.P5
	ws     *fakeWorkspace
	egress *fakeEgress
	runner *fakeRunner
	root   string
}

func newFixture(t *testing.T, results ...string) *fixture {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(body), 0o644)
	}
	write(".docs/_stash/tickets/T-001/ticket.md", ticketBody)
	write(".gateflow/env.template", "MAIL_ENABLED=false\n")
	write(".docs/operations/policies/coding.md", "規約\n")
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ws: &fakeWorkspace{}, egress: &fakeEgress{}, runner: &fakeRunner{results: results}, root: root}
	f.p5 = &engine.P5{
		Root: root,
		Project: &config.Project{
			Name:   "sample",
			Branch: &config.Branch{Main: "main"},
			Agent:  &config.Agent{EnvTemplate: ".gateflow/env.template", Allow: []string{"extra.example.com:443"}},
			Deps:   &config.Deps{Command: "go mod download", CacheEnv: "GOMODCACHE"},
			Verify: &config.Verify{Build: "go build ./...", Test: "go test ./...", Lint: "go vet ./..."},
			Policies: []config.Policy{
				{Path: ".docs/operations/policies/coding.md", Roles: []string{"dev"}, Purpose: "コーディング規約"},
				{Path: ".docs/operations/policies/missing.md", Roles: []string{"dev"}, Purpose: "ない規約"},
				{Path: ".docs/operations/policies/review.md", Roles: []string{"bj"}, Purpose: "BJ だけ"},
			},
		},
		Personal:   &config.Personal{Agent: "mock"},
		Ticket:     tk,
		AgentImage: "gateflow-agent-sample:v1",
		Agent:      mock.Agent{},
		Workspace:  f.ws,
		Egress:     f.egress,
		Runner:     f.runner,
		Author:     workspace.Author{Name: "人", Email: "h@example.invalid"},
		Builtins:   builtins.FS(),
		Now:        func() time.Time { return time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC) },
	}
	return f
}

func (f *fixture) status(t *testing.T) ticket.Status {
	t.Helper()
	tk, err := ticket.Load(f.root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func readLog(t *testing.T, runDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestP5_Done(t *testing.T) {
	f := newFixture(t, doneResult)
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != engine.Done {
		t.Fatalf("Outcome = %s (%s)", res.Outcome, res.Message)
	}
	if got := f.status(t); got != ticket.ReadyForReview {
		t.Errorf("ステータス = %s, want ready-for-review", got)
	}
	if len(f.ws.commits) != 1 || f.ws.commits[0] != "feat(T-001): 記事を公開する" {
		t.Errorf("commits = %q", f.ws.commits)
	}
	if len(f.ws.imports) != 1 || f.ws.imports[0] != "feature/t-001-publish" {
		t.Errorf("imports = %q", f.ws.imports)
	}
	if !f.ws.prepared || !f.egress.started || !f.egress.stopped {
		t.Errorf("prepared, started, stopped = %v, %v, %v", f.ws.prepared, f.egress.started, f.egress.stopped)
	}
	if len(f.runner.verify) != 3 {
		t.Errorf("検証コマンドの実行 = %d, want 3", len(f.runner.verify))
	}

	log := readLog(t, res.RunDir)
	for _, ev := range []string{`"event":"start"`, `"event":"prepare"`, `"event":"agent-end"`, `"event":"verify"`, `"event":"commit"`, `"event":"import"`, `"event":"done"`, `"event":"policy-missing"`} {
		if !strings.Contains(log, ev) {
			t.Errorf("記録に %s がない:\n%s", ev, log)
		}
	}
	step := filepath.Join(res.RunDir, "steps", "01-p5")
	for _, name := range []string{"prompt.md", "agent-1.jsonl", "out/result.json", "allowlist.txt", "proxy.log", "verify-build.log", "verify-test.log", "verify-lint.log"} {
		if _, err := os.Stat(filepath.Join(step, name)); err != nil {
			t.Errorf("記録の %s がない: %v", name, err)
		}
	}
	allow, _ := os.ReadFile(filepath.Join(step, "allowlist.txt"))
	if !strings.Contains(string(allow), "extra.example.com:443") {
		t.Errorf("allowlist = %q", allow)
	}
}

// エージェントのコンテナは、内部ネットワーク、.git と .env と指示書と規約は読み取り専用、外のモジュールは取得しない。
func TestP5_AgentSpec(t *testing.T) {
	f := newFixture(t, doneResult)
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	spec := f.runner.agentSpecs[0]
	if spec.Network != "gateflow-sample-internal" || spec.User != workspace.User || spec.Workdir != "/work" {
		t.Errorf("network, user, workdir = %q, %q, %q", spec.Network, spec.User, spec.Workdir)
	}
	for k, v := range map[string]string{
		"HTTPS_PROXY": "http://gateflow-proxy:3128", "GOPROXY": "off", "GOFLAGS": "-mod=readonly", "GOMODCACHE": "/cache",
	} {
		if spec.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, spec.Env[k], v)
		}
	}
	mounts := map[string]container.Mount{}
	for _, m := range spec.Mounts {
		mounts[m.Target] = m
	}
	for target, ro := range map[string]bool{
		"/work": false, "/work/.git": true, "/work/.env": true, "/out": false,
		"/in/ticket.md": true, "/in/policies/.docs/operations/policies/coding.md": true,
	} {
		m, ok := mounts[target]
		if !ok || m.ReadOnly != ro {
			t.Errorf("マウント %s = %+v（ある=%v）, want readonly=%v", target, m, ok, ro)
		}
	}
	if _, ok := mounts["/in/policies/.docs/operations/policies/review.md"]; ok {
		t.Error("BJ だけの規約を DEV に渡している")
	}
	if spec.Timeout != 60*time.Minute || spec.IdleTimeout != 10*time.Minute {
		t.Errorf("時間の上限 = %v, %v", spec.Timeout, spec.IdleTimeout)
	}
	data, _ := os.ReadFile(filepath.Join(res.RunDir, "steps", "01-p5", "prompt.md"))
	prompt := string(data)
	for _, w := range []string{"/in/ticket.md", "/out/result.json", "go test ./...", "/in/policies/.docs/operations/policies/coding.md"} {
		if !strings.Contains(prompt, w) {
			t.Errorf("プロンプトに %q がない", w)
		}
	}
	// 検証用のコンテナは外への経路がない。
	for _, v := range f.runner.verify {
		if v.Network != "none" || v.Env["GOPROXY"] != "off" {
			t.Errorf("検証のコンテナ = network %q, env %v", v.Network, v.Env)
		}
	}
}

func TestP5_Blocked(t *testing.T) {
	f := newFixture(t, `{"outcome": "blocked", "summary": "依存が要る", "blockReason": {"kind": "dependency", "detail": "フレームワークがない"}}`)
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != engine.Blocked || !strings.Contains(res.Message, "dependency") {
		t.Errorf("Outcome, Message = %s, %q", res.Outcome, res.Message)
	}
	if len(f.ws.commits) != 0 || len(f.ws.imports) != 0 || len(f.runner.verify) != 0 {
		t.Error("blocked なのに検証・コミット・取り込みをした")
	}
	if got := f.status(t); got != ticket.Implementing {
		t.Errorf("ステータス = %s, want implementing（人の確認待ち）", got)
	}
}

// 出力の形が違えば1回だけ再実行し、それでも違えば止める。
func TestP5_InvalidOutputTwice(t *testing.T) {
	f := newFixture(t, `{"outcome": "done"}`, `not json`)
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != engine.InvalidOutput || len(f.runner.agentSpecs) != 2 {
		t.Errorf("Outcome = %s, エージェントの実行 = %d", res.Outcome, len(f.runner.agentSpecs))
	}
	if len(f.ws.commits) != 0 {
		t.Error("出力が不正なのにコミットした")
	}
}

func TestP5_InvalidOutputThenValid(t *testing.T) {
	f := newFixture(t, `{}`, doneResult)
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != engine.Done || len(f.runner.agentSpecs) != 2 {
		t.Errorf("Outcome = %s, エージェントの実行 = %d", res.Outcome, len(f.runner.agentSpecs))
	}
	if !strings.Contains(readLog(t, res.RunDir), `"event":"output-invalid"`) {
		t.Error("出力の不正の記録がない")
	}
}

// 検証コマンドに不合格なら、差し戻さずに止める（M1）。
func TestP5_VerifyFailed(t *testing.T) {
	f := newFixture(t, doneResult)
	f.runner.verifyCode = 1
	res, err := f.p5.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != engine.VerifyFailed || len(f.ws.commits) != 0 || len(f.runner.agentSpecs) != 1 {
		t.Errorf("Outcome = %s, commits = %d, エージェントの実行 = %d", res.Outcome, len(f.ws.commits), len(f.runner.agentSpecs))
	}
	if got := f.status(t); got != ticket.Implementing {
		t.Errorf("ステータス = %s", got)
	}
}

func TestP5_WrongStatus(t *testing.T) {
	f := newFixture(t, doneResult)
	if err := f.p5.Ticket.SetStatus(ticket.ReadyToImplement, ticket.Draft); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p5.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("err = %v, want ステータスの誤り", err)
	}
	if f.ws.prepared || len(f.runner.agentSpecs) != 0 {
		t.Error("ステータスが違うのに実行した")
	}
}
