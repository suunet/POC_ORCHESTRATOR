package container

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// ProxyAlias は、内部ネットワークの中でのプロキシの名前の頭。持ち主（チケット）ごとに後ろに名前を付ける（M2.9）。
const ProxyAlias = "gateflow-proxy"

const proxyPort = "3128"

// Egress は、プロジェクトの内部ネットワーク（外への経路なし）と、その唯一の出口になるプロキシを扱う。
// 内部ネットワークはプロジェクトで共有し、プロキシのコンテナの名前とネットワークの別名は持ち主（Owner。チケット、
// ARB の試験、doctor）ごとにする（M2.9）。同じチケットの2つの実行は run.lock で止まるので、チケットごとの名前で
// 実行ごとに分かれ、後片付け（残りの削除、終わりの削除）は自分の名前にだけ行う。
type Egress struct {
	Docker     Docker
	Project    string
	ProxyImage string
	Owner      string
}

// For は、持ち主 owner のプロキシを扱う Egress を返す。
func (e Egress) For(owner string) Egress {
	e.Owner = strings.ToLower(owner)
	return e
}

// Network は、エージェントを置く内部ネットワークの名前（プロジェクトで共有）。
func (e Egress) Network() string { return "gateflow-" + e.Project + "-internal" }

func (e Egress) proxyName() string {
	if e.Owner == "" {
		return "gateflow-" + e.Project + "-proxy"
	}
	return "gateflow-" + e.Project + "-" + e.Owner + "-proxy"
}

// Alias は、内部ネットワークの中でのこの持ち主のプロキシの名前（エージェントに渡す URL のホスト）。
func (e Egress) Alias() string {
	if e.Owner == "" {
		return ProxyAlias
	}
	return ProxyAlias + "-" + e.Owner
}

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
	if err := ensureInternalNetwork(ctx, e.Docker, e.Network()); err != nil {
		return "", fmt.Errorf("内部ネットワークを作れない: %w", err)
	}
	// 自分の名前のプロキシ（同じ持ち主の前の実行の残り）だけを消す。ほかの実行のプロキシは消さない
	e.Docker.Exec(ctx, "rm", "--force", e.proxyName())
	if _, err := e.Docker.Exec(ctx, e.ProxySpec(allow).Args()...); err != nil {
		return "", fmt.Errorf("プロキシを起動できない: %w", err)
	}
	if _, err := e.Docker.Exec(ctx, "network", "connect", "--alias", e.Alias(), e.Network(), e.proxyName()); err != nil {
		e.Docker.Exec(ctx, "rm", "--force", e.proxyName())
		return "", fmt.Errorf("プロキシを内部ネットワークにつなげない: %w", err)
	}
	return "http://" + e.Alias() + ":" + proxyPort, nil
}

// Follow は、プロキシの判定の記録を、プロキシが消えるか ctx が取り消されるまで w へ逐次書く（実行中の記録のため）。
func (e Egress) Follow(ctx context.Context, w io.Writer) error {
	return e.Docker.Stream(ctx, w, "logs", "--follow", e.proxyName())
}

// Stop は、プロキシの判定の記録を取り出してから、プロキシを消す。
func (e Egress) Stop(ctx context.Context) ([]byte, error) {
	logs, logErr := e.Docker.Exec(ctx, "logs", e.proxyName())
	if _, err := e.Docker.Exec(ctx, "rm", "--force", e.proxyName()); err != nil {
		return logs, fmt.Errorf("プロキシを消せない: %w", err)
	}
	return logs, logErr
}
