package claude_test

import (
	"slices"
	"strings"
	"testing"

	"gateflow/internal/agent"
	"gateflow/internal/agent/claude"
	"gateflow/internal/container"
)

var _ agent.Agent = claude.Agent{}

func TestCommand(t *testing.T) {
	cmd, stdin := claude.Agent{}.Command(agent.Request{System: "あなたは DEV", Prompt: "指示"})
	want := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--append-system-prompt", "あなたは DEV"}
	if !slices.Equal(cmd, want) {
		t.Errorf("cmd = %q, want %q", cmd, want)
	}
	if stdin != "指示" {
		t.Errorf("stdin = %q（プロンプトは標準入力で渡す）", stdin)
	}
	cmd, _ = claude.Agent{}.Command(agent.Request{System: "s", Prompt: "p", Model: "opus"})
	if i := slices.Index(cmd, "--model"); i < 0 || cmd[i+1] != "opus" {
		t.Errorf("モデルの指定がない: %q", cmd)
	}
}

func TestHosts(t *testing.T) {
	hosts := claude.Agent{}.Hosts()
	if len(hosts) == 0 {
		t.Fatal("許可リストのホストがない")
	}
	for _, h := range hosts {
		if !strings.HasSuffix(h, ":443") || !strings.Contains(h, "anthropic.com") {
			t.Errorf("ホスト %q は LLM の API か認証の HTTPS でない", h)
		}
	}
}

func TestAuthAndEnv(t *testing.T) {
	a := claude.Agent{}
	m := a.AuthMount()
	if m != (container.Mount{Type: container.Volume, Source: "gateflow-auth-claude", Target: "/auth"}) {
		t.Errorf("AuthMount = %+v", m)
	}
	env := a.Env()
	if env["CLAUDE_CONFIG_DIR"] != "/auth" || env["DISABLE_AUTOUPDATER"] != "1" || env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Errorf("Env = %v", env)
	}
	if a.Name() != "claude" || !slices.Equal(a.LoginCommand(), []string{"claude"}) {
		t.Errorf("Name, LoginCommand = %q, %q", a.Name(), a.LoginCommand())
	}
}
