package container

import (
	"context"
	"fmt"
	"path/filepath"

	"gateflow/internal/config"
)

// Images は、エージェントとプロキシのイメージを作る。タグには gateflow の版を付ける。
type Images struct {
	Docker  Docker
	Tool    Tool
	Version string
}

// BaseTag は、エージェントの土台のイメージのタグ。
func (i Images) BaseTag() string { return "gateflow-agent-base:" + i.Version }

// AgentTag は、プロジェクトのエージェントのイメージのタグ。
func (i Images) AgentTag(project string) string { return "gateflow-agent-" + project + ":" + i.Version }

// ProxyTag は、プロキシのイメージのタグ。
func (i Images) ProxyTag() string { return "gateflow-proxy:" + i.Version }

// BuildAgent は、土台のイメージと、プロジェクトの Dockerfile（土台を BASE として受け取る）のイメージを作り、後者のタグを返す。
// プロジェクトのビルドの文脈は Dockerfile のあるディレクトリだけにし、プロジェクトのフォルダ全体（.env など）を送らない。
func (i Images) BuildAgent(ctx context.Context, root string, p *config.Project) (string, error) {
	baseFile := i.Tool.Dockerfile("agent-base")
	if _, err := i.Docker.Exec(ctx, "build", "--tag", i.BaseTag(), "--file", baseFile, filepath.Dir(baseFile)); err != nil {
		return "", fmt.Errorf("土台のイメージを作れない: %w", err)
	}
	file := filepath.Join(root, p.Agent.Dockerfile)
	tag := i.AgentTag(p.Name)
	if _, err := i.Docker.Exec(ctx, "build", "--tag", tag, "--build-arg", "BASE="+i.BaseTag(), "--file", file, filepath.Dir(file)); err != nil {
		return "", fmt.Errorf("プロジェクトのエージェントのイメージを作れない: %w", err)
	}
	return tag, nil
}

// ProxyBuildArgs は、プロキシのイメージを作る docker build の引数。文脈はツール本体の bin/。
func (i Images) ProxyBuildArgs(arch string) []string {
	return []string{"build", "--tag", i.ProxyTag(), "--build-arg", "BINARY=gateflow-linux-" + arch,
		"--file", i.Tool.Dockerfile("proxy"), filepath.Join(i.Tool.Root, "bin")}
}

// BuildProxy は、版の合う Linux 用の実行ファイルを確かめてから、プロキシのイメージを作ってタグを返す。
func (i Images) BuildProxy(ctx context.Context, arch string) (string, error) {
	if _, err := i.Tool.CheckProxyBinary(arch, i.Version); err != nil {
		return "", err
	}
	if _, err := i.Docker.Exec(ctx, i.ProxyBuildArgs(arch)...); err != nil {
		return "", fmt.Errorf("プロキシのイメージを作れない: %w", err)
	}
	return i.ProxyTag(), nil
}
