package gate_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/gate"
)

type fakeRunner struct {
	specs []container.Spec
	codes map[string]int // コマンドの文字列 → 終了コード
}

func (f *fakeRunner) Run(_ context.Context, spec container.Spec, stdout, _ io.Writer) (int, error) {
	f.specs = append(f.specs, spec)
	cmd := spec.Cmd[len(spec.Cmd)-1]
	io.WriteString(stdout, "output of "+cmd+"\n")
	return f.codes[cmd], nil
}

func TestCommands(t *testing.T) {
	cmds := gate.Commands(&config.Verify{Build: "go build ./...", Test: "go test ./...", Lint: "go vet ./..."})
	var got []string
	for _, c := range cmds {
		got = append(got, c.Name+"="+c.Cmd)
	}
	if strings.Join(got, ",") != "build=go build ./...,test=go test ./...,lint=go vet ./..." {
		t.Errorf("Commands = %v", got)
	}
}

func TestRun(t *testing.T) {
	r := &fakeRunner{codes: map[string]int{"go test ./...": 1}}
	base := container.Spec{Name: "gateflow-sample-t-001-verify", Image: "img", Network: "none", Env: map[string]string{"GOPROXY": "off"}}
	g := gate.Gate{Runner: r, Base: base}

	results, passed, err := g.Run(context.Background(), gate.Commands(&config.Verify{Build: "go build ./...", Test: "go test ./...", Lint: "go vet ./..."}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if passed {
		t.Error("test が不合格なのに passed")
	}
	// 1つが不合格でも、残りも実行する。
	if len(results) != 3 || len(r.specs) != 3 {
		t.Fatalf("results = %d, specs = %d, want 3", len(results), len(r.specs))
	}
	if results[1].Name != "test" || results[1].ExitCode != 1 || !strings.Contains(results[1].Output, "output of go test") {
		t.Errorf("results[1] = %+v", results[1])
	}
	for i, s := range r.specs {
		if s.Image != "img" || s.Network != "none" || s.Env["GOPROXY"] != "off" || s.Timeout == 0 || s.IdleTimeout == 0 {
			t.Errorf("spec[%d] = %+v", i, s)
		}
		if s.Cmd[0] != "sh" || s.Cmd[1] != "-c" {
			t.Errorf("spec[%d].Cmd = %q", i, s.Cmd)
		}
	}
	if r.specs[0].Name == r.specs[1].Name {
		t.Error("コンテナの名前が同じ")
	}
}

func TestRun_AllPassed(t *testing.T) {
	g := gate.Gate{Runner: &fakeRunner{}, Base: container.Spec{Name: "n", Image: "img"}}
	_, passed, err := g.Run(context.Background(), gate.Commands(&config.Verify{Build: "b", Test: "t", Lint: "l"}))
	if err != nil || !passed {
		t.Errorf("passed, err = %v, %v", passed, err)
	}
}
