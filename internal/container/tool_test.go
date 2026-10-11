package container_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/container"
)

func TestFindTool_FromExecutable(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "bin", "gateflow-darwin-arm64")
	tool, err := container.FindTool(exe, "")
	if err != nil {
		t.Fatalf("FindTool: %v", err)
	}
	if tool.Root != root {
		t.Errorf("Root = %s, want %s", tool.Root, root)
	}
	if got := tool.ProxyBinary("arm64"); got != filepath.Join(root, "bin", "gateflow-linux-arm64") {
		t.Errorf("ProxyBinary = %s", got)
	}
	if got := tool.Dockerfile("agent-base"); got != filepath.Join(root, "docker", "agent-base.Dockerfile") {
		t.Errorf("Dockerfile = %s", got)
	}
}

// GATEFLOW_HOME があれば、実行ファイルの場所より優先する（go run での開発など）。
func TestFindTool_Home(t *testing.T) {
	home := t.TempDir()
	tool, err := container.FindTool("/tmp/go-build123/exe/gateflow", home)
	if err != nil || tool.Root != home {
		t.Fatalf("tool, err = %+v, %v", tool, err)
	}
}

// 実行ファイルが bin/ になく、GATEFLOW_HOME もなければ止める。
func TestFindTool_Unknown(t *testing.T) {
	if _, err := container.FindTool("/tmp/go-build123/exe/gateflow", ""); err == nil || !strings.Contains(err.Error(), "GATEFLOW_HOME") {
		t.Fatalf("err = %v, want GATEFLOW_HOME の案内", err)
	}
}

func TestVersionFromLdflags(t *testing.T) {
	cases := map[string]string{
		"-s -w -X main.version=v0.1.0-3-gabc":  "v0.1.0-3-gabc",
		`-s -w -X "main.version=v0.2.0"`:       "v0.2.0",
		"-X=main.version=dirty-1 -s":           "dirty-1",
		"-s -w":                                "",
		"-X gateflow/internal/x.version=v9 -s": "",
	}
	for in, want := range cases {
		if got := container.VersionFromLdflags(in); got != want {
			t.Errorf("VersionFromLdflags(%q) = %q, want %q", in, got, want)
		}
	}
}

// Linux 用の実行ファイルがなければ、scripts/build.sh の実行を案内して止める。
func TestTool_CheckProxyBinary_Missing(t *testing.T) {
	tool := container.Tool{Root: t.TempDir()}
	_, err := tool.CheckProxyBinary("arm64", "v1")
	if err == nil || !strings.Contains(err.Error(), "scripts/build.sh") {
		t.Fatalf("err = %v, want scripts/build.sh の案内", err)
	}
}

// 版が刻まれていない実行ファイル（このテストのプログラム自身）は、版が合わないものとして止める。
func TestTool_CheckProxyBinary_VersionMismatch(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	self, _ := os.ReadFile(os.Args[0])
	os.WriteFile(filepath.Join(root, "bin", "gateflow-linux-arm64"), self, 0o755)
	tool := container.Tool{Root: root}
	_, err := tool.CheckProxyBinary("arm64", "v1")
	if err == nil || !strings.Contains(err.Error(), "scripts/build.sh") || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("err = %v, want 版の不一致と scripts/build.sh の案内", err)
	}
}
