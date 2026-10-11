package engine_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/agent/mock"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/engine"
)

// probeRunner は、確認のスクリプトの代わりに、決めた結果の行を返す。
type probeRunner struct {
	spec   container.Spec
	result func(id string) int // 確認の ID → 終了コード
}

func (r *probeRunner) Run(_ context.Context, spec container.Spec, stdout, _ io.Writer) (int, error) {
	r.spec = spec
	for _, c := range engine.SandboxChecks() {
		fmt.Fprintf(stdout, "CHECK %s %d\n", c.ID, r.result(c.ID))
	}
	return 0, nil
}

func newSandbox(t *testing.T, r *probeRunner) *engine.Sandbox {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".gateflow"), 0o755)
	os.WriteFile(filepath.Join(root, ".gateflow/env.template"), []byte("A=1\n"), 0o644)
	return &engine.Sandbox{
		Root: root,
		Project: &config.Project{
			Name:   "sample",
			Branch: &config.Branch{Main: "main"},
			Agent:  &config.Agent{EnvTemplate: ".gateflow/env.template"},
			Deps:   &config.Deps{Command: "go mod download", CacheEnv: "GOMODCACHE"},
		},
		AgentImage: "gateflow-agent-sample:v1",
		Agent:      mock.Agent{},
		Workspace:  &fakeWorkspace{},
		Egress:     &fakeEgress{},
		Runner:     r,
	}
}

// 期待どおり（外へ出る手段は失敗、許可したホストへの接続は成功）なら、すべて合格。
func TestSandbox_AllPass(t *testing.T) {
	expect := map[string]string{}
	for _, c := range engine.SandboxChecks() {
		expect[c.ID] = c.Expect
	}
	r := &probeRunner{result: func(id string) int {
		if expect[id] == "ok" {
			return 0
		}
		return 1
	}}
	checks, err := newSandbox(t, r).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// コンテナの中の確認と、ホストで行う .env の比較（env-unchanged）。
	if len(checks) != len(engine.SandboxChecks())+1 {
		t.Fatalf("確認の数 = %d", len(checks))
	}
	for _, c := range checks {
		if !c.Passed {
			t.Errorf("%s が不合格", c.ID)
		}
	}
	// エージェントと同じ構成で試す。
	s := r.spec
	mounts := map[string]bool{}
	for _, m := range s.Mounts {
		mounts[m.Target] = m.ReadOnly
	}
	if s.Network != "gateflow-sample-internal" || s.Env["HTTPS_PROXY"] == "" || s.Env["GOPROXY"] != "off" ||
		!mounts["/work/.git"] || !mounts["/work/.env"] || mounts["/work"] {
		t.Errorf("サンドボックスの構成がエージェントと違う: %+v", s)
	}
}

// 外へ出られてしまった確認は不合格になる。
func TestSandbox_Leak(t *testing.T) {
	r := &probeRunner{result: func(id string) int { return 0 }}
	checks, err := newSandbox(t, r).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var failed []string
	for _, c := range checks {
		if !c.Passed {
			failed = append(failed, c.ID)
		}
	}
	for _, want := range []string{"git-push-remote", "post-not-allowed", "env-write", "git-dir-write-hook", "go-get-default"} {
		if !strings.Contains(strings.Join(failed, ","), want) {
			t.Errorf("%s が不合格になっていない: %v", want, failed)
		}
	}
}

// 確認は、完了の基準の 4.2（push）・4.3（POST とモジュール）・4.4（.env）をすべて含む。
func TestSandboxChecks_Coverage(t *testing.T) {
	groups := map[string]bool{}
	for _, c := range engine.SandboxChecks() {
		groups[c.Group] = true
		if c.Cmd == "" || c.Desc == "" {
			t.Errorf("%s の中身がない", c.ID)
		}
	}
	for _, g := range []string{"push", "post", "module", "env"} {
		if !groups[g] {
			t.Errorf("%s の確認がない", g)
		}
	}
}
