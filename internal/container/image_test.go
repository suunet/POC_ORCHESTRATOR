package container_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/container"
)

func TestImages_BuildAgent(t *testing.T) {
	d, logFile := fakeDocker(t, "detached")
	tool := container.Tool{Root: "/tool"}
	imgs := container.Images{Docker: d, Tool: tool, Version: "v1"}
	p := &config.Project{Name: "sample", Agent: &config.Agent{Dockerfile: ".gateflow/agent.Dockerfile"}}

	tag, err := imgs.BuildAgent(context.Background(), "/proj", p)
	if err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}
	if tag != "gateflow-agent-sample:v1" {
		t.Errorf("tag = %s", tag)
	}
	data, _ := os.ReadFile(logFile)
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(calls) != 2 {
		t.Fatalf("docker の呼び出し = %q, want 2（土台とプロジェクト）", calls)
	}
	base := "build --tag gateflow-agent-base:v1 --file /tool/docker/agent-base.Dockerfile /tool/docker"
	if calls[0] != base {
		t.Errorf("土台のビルド = %q, want %q", calls[0], base)
	}
	// プロジェクトのビルドの文脈は Dockerfile のあるディレクトリだけにする（プロジェクトのフォルダ全体を送らない）。
	proj := "build --tag gateflow-agent-sample:v1 --build-arg BASE=gateflow-agent-base:v1 --file /proj/.gateflow/agent.Dockerfile /proj/.gateflow"
	if calls[1] != proj {
		t.Errorf("プロジェクトのビルド = %q, want %q", calls[1], proj)
	}
}

func TestImages_BuildProxy(t *testing.T) {
	d, logFile := fakeDocker(t, "detached")
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	imgs := container.Images{Docker: d, Tool: container.Tool{Root: root}, Version: "v1"}

	// 版の合わない実行ファイルではビルドしない。
	self, _ := os.ReadFile(os.Args[0])
	os.WriteFile(filepath.Join(root, "bin", "gateflow-linux-arm64"), self, 0o755)
	if _, err := imgs.BuildProxy(context.Background(), "arm64"); err == nil {
		t.Fatal("版の合わない実行ファイルでプロキシのイメージを作った")
	}
	if data, _ := os.ReadFile(logFile); len(data) != 0 {
		t.Errorf("docker を呼んだ: %s", data)
	}
}

func TestImages_ProxyBuildArgs(t *testing.T) {
	imgs := container.Images{Tool: container.Tool{Root: "/tool"}, Version: "v1"}
	got := strings.Join(imgs.ProxyBuildArgs("arm64"), " ")
	want := "build --tag gateflow-proxy:v1 --build-arg BINARY=gateflow-linux-arm64 --file /tool/docker/proxy.Dockerfile /tool/bin"
	if got != want {
		t.Errorf("ProxyBuildArgs = %q, want %q", got, want)
	}
}
