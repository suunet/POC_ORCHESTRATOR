// Package claude は、Claude Code の非対話実行（claude -p）のアダプタ。
package claude

import (
	"gateflow/internal/agent"
	"gateflow/internal/container"
)

// Agent は、Claude Code のエージェント。
type Agent struct{}

func (Agent) Name() string { return "claude" }

// Command は、claude -p の呼び出しを返す。プロンプトは標準入力で渡し、逐次の出力を JSON で受け取る。
// 権限の確認は省く（安全性はコンテナと通信の制限で担保する。設計の「社内方針と構造による担保」）。
func (Agent) Command(req agent.Request) ([]string, string) {
	cmd := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--append-system-prompt", req.System}
	if req.Model != "" {
		cmd = append(cmd, "--model", req.Model)
	}
	return cmd, req.Prompt
}

// Hosts は、LLM の API と認証の更新に必要なホスト。
// NOTE: アクセストークンの更新は platform.claude.com へ行く。許可しないと、トークンが切れた後の実行がすべて 401 で失敗する
func (Agent) Hosts() []string {
	return []string{"api.anthropic.com:443", "console.anthropic.com:443", "platform.claude.com:443"}
}

const authDir = "/auth"

// AuthMount は、ログインの情報（設定を含む）を置くボリューム。CLAUDE_CONFIG_DIR でここを指す。
func (Agent) AuthMount() container.Mount {
	return container.Mount{Type: container.Volume, Source: "gateflow-auth-claude", Target: authDir}
}

func (Agent) Env() map[string]string {
	return map[string]string{
		"CLAUDE_CONFIG_DIR":                        authDir,
		"DISABLE_AUTOUPDATER":                      "1",
		"DISABLE_TELEMETRY":                        "1",
		"DISABLE_ERROR_REPORTING":                  "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		// アカウントの外部の連携（MCP の中継）を読み込まない。プロキシの拒否は構造の担保として残す
		"ENABLE_CLAUDEAI_MCP_SERVERS": "false",
	}
}

func (Agent) LoginCommand() []string { return []string{"claude"} }
