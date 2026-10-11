package mock_test

import (
	"strings"
	"testing"

	"gateflow/internal/agent"
	"gateflow/internal/agent/mock"
)

var _ agent.Agent = mock.Agent{}

// mock は、決めた出力を /out/result.json に書くだけのエージェント。
func TestCommand(t *testing.T) {
	cmd, stdin := mock.Agent{Result: `{"outcome":"done"}`}.Command(agent.Request{Prompt: "p"})
	if strings.Join(cmd, " ") != "sh -c cat > /out/result.json" || stdin != `{"outcome":"done"}` {
		t.Errorf("cmd, stdin = %q, %q", cmd, stdin)
	}
	if len(mock.Agent{}.Hosts()) != 0 {
		t.Error("mock が外への通信を求めた")
	}
}
