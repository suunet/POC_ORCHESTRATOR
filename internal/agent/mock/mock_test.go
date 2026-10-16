package mock_test

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/agent"
	"gateflow/internal/agent/mock"
)

var _ agent.Agent = &mock.Agent{}

const scenario = `{
  "steps": {
    "p5": [
      {"result": {"outcome": "done", "summary": "1回目"}, "files": {"a.go": "package a // 1\n"}},
      {"result": {"outcome": "done", "summary": "2回目"}, "files": {"sub/b.go": "package b\n"}}
    ],
    "p5-verify": [
      {"result": {"outcome": "approved", "summary": "ok"}}
    ]
  }
}`

func TestParseScenario(t *testing.T) {
	if _, err := mock.ParseScenario([]byte(scenario)); err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
	for _, bad := range []string{`{}`, `{"steps": {"p5": []}}`, `{"steps": {"p5": [{"files": {}}]}}`, `{"steps": {"p5": [{"result": {}, "files": {"../x": ""}}]}}`, `{"steps": {"p5": [{"result": {}, "remove": ["../x"]}]}}`} {
		if _, err := mock.ParseScenario([]byte(bad)); err == nil {
			t.Errorf("ParseScenario(%s) が誤りにならない", bad)
		}
	}
}

// run は、mock のスクリプトを、/out と /work を一時ディレクトリに置き換えて実行する。
func run(t *testing.T, cmd []string) (out, work string) {
	t.Helper()
	dir := t.TempDir()
	out, work = filepath.Join(dir, "out"), filepath.Join(dir, "work")
	os.MkdirAll(out, 0o755)
	os.MkdirAll(work, 0o755)
	script := strings.NewReplacer("/out/", out+"/", "/work/", work+"/").Replace(cmd[2])
	if b, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("スクリプトの実行: %v\n%s", err, b)
	}
	return out, work
}

// step ごとに筋書きの出力を順に返し、尽きたら最後の出力を繰り返す。
func TestCommand_Sequence(t *testing.T) {
	sc, _ := mock.ParseScenario([]byte(scenario))
	a := &mock.Agent{Scenario: sc}

	cmd, _ := a.Command(agent.Request{Step: "p5"})
	out, work := run(t, cmd)
	if data, _ := os.ReadFile(filepath.Join(out, "result.json")); !strings.Contains(string(data), "1回目") {
		t.Errorf("1回目の result.json = %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(work, "a.go")); string(data) != "package a // 1\n" {
		t.Errorf("a.go = %q", data)
	}
	for range 2 {
		cmd, _ = a.Command(agent.Request{Step: "p5"})
	}
	out, work = run(t, cmd)
	if data, _ := os.ReadFile(filepath.Join(out, "result.json")); !strings.Contains(string(data), "2回目") {
		t.Errorf("3回目（筋書きの最後を繰り返す）の result.json = %s", data)
	}
	if _, err := os.Stat(filepath.Join(work, "sub", "b.go")); err != nil {
		t.Errorf("sub/b.go がない: %v", err)
	}
	// 中身はシェルの解釈を受けない形（base64）で埋め込む。
	if !strings.Contains(cmd[2], base64.StdEncoding.EncodeToString([]byte("package b\n"))) {
		t.Error("ファイルの中身が base64 で埋め込まれていない")
	}
}

// 筋書きにない step は、出力を書かない（出力の不正の経路を確かめるため）。
func TestCommand_UnknownStep(t *testing.T) {
	sc, _ := mock.ParseScenario([]byte(scenario))
	cmd, _ := (&mock.Agent{Scenario: sc}).Command(agent.Request{Step: "p2-review"})
	out, _ := run(t, cmd)
	if _, err := os.Stat(filepath.Join(out, "result.json")); err == nil {
		t.Error("筋書きにない step で result.json を書いた")
	}
}

func TestHostsAndName(t *testing.T) {
	a := &mock.Agent{}
	if len(a.Hosts()) != 0 || a.Name() != "mock" {
		t.Errorf("Hosts, Name = %v, %q", a.Hosts(), a.Name())
	}
}
