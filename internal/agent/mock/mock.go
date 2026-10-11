// Package mock は、テスト用のダミーのエージェント。決めた出力を書くだけで、外へは通信しない。
package mock

import (
	"gateflow/internal/agent"
	"gateflow/internal/container"
)

// Agent は、Result をそのまま /out/result.json に書くエージェント。
type Agent struct {
	Result string
}

func (Agent) Name() string { return "mock" }

func (a Agent) Command(agent.Request) ([]string, string) {
	return []string{"sh", "-c", "cat > /out/result.json"}, a.Result
}

func (Agent) Hosts() []string { return nil }

func (Agent) AuthMount() container.Mount {
	return container.Mount{Type: container.Volume, Source: "gateflow-auth-mock", Target: "/auth"}
}

func (Agent) Env() map[string]string { return map[string]string{} }

func (Agent) LoginCommand() []string { return []string{"true"} }
