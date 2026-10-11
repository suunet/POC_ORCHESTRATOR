package container

import (
	"context"
	"fmt"
	"strings"
)

// ProxyAlias は、内部ネットワークの中でのプロキシの名前。
const ProxyAlias = "gateflow-proxy"

const proxyPort = "3128"

// Egress は、プロジェクトの内部ネットワーク（外への経路なし）と、その唯一の出口になるプロキシを扱う。
type Egress struct {
	Docker     Docker
	Project    string
	ProxyImage string
}

// Network は、エージェントを置く内部ネットワークの名前。
func (e Egress) Network() string { return "gateflow-" + e.Project + "-internal" }

func (e Egress) proxyName() string { return "gateflow-" + e.Project + "-proxy" }

// ProxySpec は、プロキシのコンテナの仕様。通常のネットワークで起動し、後から内部ネットワークにつなぐ。
// 判定の記録を取り出すため、終わっても自動では消さない。
func (e Egress) ProxySpec(allow []string) Spec {
	cmd := []string{"--listen", ":" + proxyPort}
	if len(allow) > 0 {
		cmd = append(cmd, "--allow", strings.Join(allow, ","))
	}
	return Spec{Name: e.proxyName(), Image: e.ProxyImage, Detach: true, Cmd: cmd}
}

// Start は、内部ネットワークを用意し、allow を許可リストとするプロキシを起動して、エージェントに渡すプロキシの URL を返す。
func (e Egress) Start(ctx context.Context, allow []string) (string, error) {
	if _, err := e.Docker.Exec(ctx, "network", "inspect", e.Network()); err != nil {
		if _, err := e.Docker.Exec(ctx, "network", "create", "--internal", e.Network()); err != nil {
			return "", fmt.Errorf("内部ネットワークを作れない: %w", err)
		}
	}
	// 前の実行のプロキシが残っていれば消す（なければ失敗するが構わない）
	e.Docker.Exec(ctx, "rm", "--force", e.proxyName())
	if _, err := e.Docker.Exec(ctx, e.ProxySpec(allow).Args()...); err != nil {
		return "", fmt.Errorf("プロキシを起動できない: %w", err)
	}
	if _, err := e.Docker.Exec(ctx, "network", "connect", "--alias", ProxyAlias, e.Network(), e.proxyName()); err != nil {
		e.Docker.Exec(ctx, "rm", "--force", e.proxyName())
		return "", fmt.Errorf("プロキシを内部ネットワークにつなげない: %w", err)
	}
	return "http://" + ProxyAlias + ":" + proxyPort, nil
}

// Stop は、プロキシの判定の記録を取り出してから、プロキシを消す。
func (e Egress) Stop(ctx context.Context) ([]byte, error) {
	logs, logErr := e.Docker.Exec(ctx, "logs", e.proxyName())
	if _, err := e.Docker.Exec(ctx, "rm", "--force", e.proxyName()); err != nil {
		return logs, fmt.Errorf("プロキシを消せない: %w", err)
	}
	return logs, logErr
}
