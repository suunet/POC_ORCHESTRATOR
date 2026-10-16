// Package e2e は、本物の Docker と git とダミーの AI で、使い捨てのプロジェクトを流す試験（M3）。
// 時間がかかり Docker が要るので、環境変数 GATEFLOW_E2E=1 のときだけ走る（ふだんの go test ./... では飛ばす）。
//
//	GATEFLOW_E2E=1 go test ./internal/e2e/ -v -timeout 30m
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// project は、試験のプロジェクトの名前（Docker の名前に使う。試験の前後に、この名前の残りを消す）。
const project = "gfe2e"

const version = "e2e"

// env は、試験の道具（gateflow の実行ファイルとプロジェクトの場所）。
type env struct {
	t    *testing.T
	exe  string
	root string // プロジェクトのルート
	tmp  string
}

func setup(t *testing.T) *env {
	t.Helper()
	if os.Getenv("GATEFLOW_E2E") != "1" {
		t.Skip("GATEFLOW_E2E=1 のときだけ走る（本物の Docker を使う）")
	}
	tmp := t.TempDir()
	e := &env{t: t, tmp: tmp, root: filepath.Join(tmp, "project")}
	e.buildTool()
	cleanDocker()
	t.Cleanup(cleanDocker)
	e.initProject()
	return e
}

// buildTool は、ツール本体の形（bin/ の実行ファイルと docker/ の Dockerfile）を一時の場所に作る。
// プロキシ用の Linux の実行ファイルは、ホストの実行ファイルと同じ版を刻む。
func (e *env) buildTool() {
	repo, err := filepath.Abs("../..")
	if err != nil {
		e.t.Fatal(err)
	}
	tool := filepath.Join(e.tmp, "tool")
	os.MkdirAll(filepath.Join(tool, "bin"), 0o755)
	os.MkdirAll(filepath.Join(tool, "docker"), 0o755)
	for _, name := range []string{"agent-base.Dockerfile", "proxy.Dockerfile"} {
		data, err := os.ReadFile(filepath.Join(repo, "docker", name))
		if err != nil {
			e.t.Fatal(err)
		}
		os.WriteFile(filepath.Join(tool, "docker", name), data, 0o644)
	}
	e.exe = filepath.Join(tool, "bin", "gateflow")
	ldflags := "-X main.version=" + version
	build := func(out string, env ...string) {
		cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "./cmd/gateflow")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), env...)
		if b, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("go build: %v\n%s", err, b)
		}
	}
	build(e.exe)
	build(filepath.Join(tool, "bin", "gateflow-linux-"+runtime.GOARCH), "GOOS=linux", "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
}

// cleanDocker は、試験のプロジェクトの Docker の残り（作業のボリューム、内部ネットワーク）を消す。
func cleanDocker() {
	exec.Command("docker", "volume", "rm", "-f", "gateflow-work-"+project).Run()
	exec.Command("docker", "network", "rm", "gateflow-"+project+"-internal").Run()
}

func (e *env) write(rel, body string) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", e.root}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, b)
	}
	return strings.TrimSpace(string(b))
}

const projectJSON = `{
  "formatVersion": 2,
  "name": "` + project + `",
  "branch": {"main": "main"},
  "agent": {"dockerfile": ".gateflow/agent.Dockerfile", "envTemplate": ".gateflow/env.template", "allow": []},
  "verify": {"build": "go build ./...", "test": "go test ./...", "lint": "go vet ./..."},
  "ids": {
    "scan": [".docs/design"],
    "tests": ["calc"],
    "prefixes": [
      {"prefix": "UC", "pattern": "UC-[0-9]{3}", "files": [".docs/design/buc.md"], "find": "table-column:UC"},
      {"prefix": "AC", "pattern": "UC-[0-9]{3}-AC-[0-9]{2}", "files": [".docs/design/uc/*.md"], "find": "table-first-column", "methodColumn": "確認の方法"}
    ]
  },
  "policies": []
}
`

const agentDockerfile = `ARG BASE
FROM golang:1.26.5-bookworm AS go

FROM ${BASE}
USER root
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local
USER agent
`

const ucDetail = `# UC-001 計算する

## 受入条件

| 受入条件 | 内容 | 確認の方法 |
| --- | --- | --- |
| UC-001-AC-01 | Add で足し算する | シナリオテスト |
`

// initProject は、使い捨てのプロジェクト（小さな Go のモジュールとカタログ）を git の main に作る。
func (e *env) initProject() {
	e.write(".gitignore", ".docs/_stash/\n.env\n")
	e.write(".gateflow/project.json", projectJSON)
	e.write(".gateflow/agent.Dockerfile", agentDockerfile)
	e.write(".gateflow/env.template", "A=1\n")
	e.write("go.mod", "module example.com/e2e\n\ngo 1.26\n")
	e.write("calc/add.go", "package calc\n\n// Add は足し算をする。\nfunc Add(a, b int) int { return a + b }\n")
	e.write("calc/add_test.go", "package calc\n\nimport \"testing\"\n\n// UC-001-AC-01\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"Add\")\n\t}\n}\n")
	e.write(".docs/design/buc.md", "| BUC | UC |\n| --- | --- |\n| BUC-S01 | UC-001（計算する） |\n")
	e.write(".docs/design/uc/UC-001.md", ucDetail)
	e.git("init", "-q", "-b", "main")
	e.git("config", "user.name", "e2e")
	e.git("config", "user.email", "e2e@example.invalid")
	e.git("add", "-A")
	e.git("commit", "-q", "-m", "chore: 初め")
}

// ticketBody は、指示書。plan があれば §3・§4 を埋めて承認済みにする（P5 から始めるチケット）。
func ticketBody(id, title, status, targets, plan string) string {
	b := fmt.Sprintf(`# %s %s

ステータス: %s

| 項目 | 値 |
| --- | --- |
| 種別 | 実装 |
| 対象ID | %s |
| ブランチ | feature/%s |

## §1 目的

- 計算を足す

## §2 要求

- UC-001 に引き算などを足す

`, id, title, status, targets, strings.ToLower(id))
	if plan == "" {
		return b + `## §3 変更前の現状（P3でAIが書く）

- 影響の範囲（IDで検索した結果の要約）：

## §4 実行計画（P3でAIが書く）

### §4.1 方針

-

### §4.2 受入条件

| 受入条件 | ユースケース | 内容 |
| --- | --- | --- |

### §4.3 テストケース

| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |
| --- | --- | --- | --- |

### §4.4 更新する文書

| ファイル | 更新の内容 |
| --- | --- |

### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）

-

### §4.6 承認（人が書く。AIは書かない）

- [ ] §2の要求に合意した
- [ ] §3の現状の認識に合意した
- [ ] §4の計画（受入条件・テストケース・更新する文書）に合意した
`
	}
	return b + plan
}

// planFor は、P5 から始めるチケットの §3・§4（承認済み）。
func planFor(test, ac string) string {
	return fmt.Sprintf(`## §3 変更前の現状（P3でAIが書く）

- calc に足し算だけがある

## §4 実行計画（P3でAIが書く）

### §4.1 方針

- calc に関数を足す

### §4.2 受入条件

| 受入条件 | ユースケース | 内容 |
| --- | --- | --- |
| %[2]s | UC-001 | 計算する |

### §4.3 テストケース

| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |
| --- | --- | --- | --- |
| %[1]s | シナリオ | %[2]s | 計算の結果を確かめる |

### §4.4 更新する文書

| ファイル | 更新の内容 |
| --- | --- |

### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）

-

### §4.6 承認（人が書く。AIは書かない）

- [x] §2の要求に合意した
- [x] §3の現状の認識に合意した
- [x] §4の計画（受入条件・テストケース・更新する文書）に合意した
`, test, ac)
}

func (e *env) ticket(id, body string) {
	e.write(".docs/_stash/tickets/"+id+"/ticket.md", body)
}

// scenario は、ダミーの AI の筋書きを書いて、その場所を返す。
func (e *env) scenario(name string, steps map[string][]map[string]any) string {
	data, err := json.MarshalIndent(map[string]any{"steps": steps}, "", "  ")
	if err != nil {
		e.t.Fatal(err)
	}
	p := filepath.Join(e.tmp, name+".json")
	os.WriteFile(p, data, 0o644)
	return p
}

// gateflow は、gateflow を実行して、出力を返す（終了コードは問わない。結果は指示書と実行の記録で確かめる）。
func (e *env) gateflow(scenario string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command(e.exe, append(args, "--project", e.root)...)
	cmd.Env = append(os.Environ(), "GATEFLOW_AGENT=mock", "GATEFLOW_MOCK_SCENARIO="+scenario, "GATEFLOW_HOME=")
	b, _ := cmd.CombinedOutput()
	e.t.Logf("gateflow %s:\n%s", strings.Join(args, " "), b)
	return string(b)
}

var statusLine = regexp.MustCompile(`(?m)^ステータス: (\S+)`)

func (e *env) status(id string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.root, ".docs/_stash/tickets", id, "ticket.md"))
	if err != nil {
		e.t.Fatal(err)
	}
	m := statusLine.FindStringSubmatch(string(data))
	if m == nil {
		e.t.Fatalf("%s のステータスがない", id)
	}
	return m[1]
}

// lastRun は、チケットの最後の実行の記録の場所を返す。
func (e *env) lastRun(id string) string {
	e.t.Helper()
	runs, _ := filepath.Glob(filepath.Join(e.root, ".docs/_stash/tickets", id, "runs", "*"))
	if len(runs) == 0 {
		e.t.Fatalf("%s の実行の記録がない", id)
	}
	return runs[len(runs)-1]
}

type state struct {
	Stopped *struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	} `json:"stopped"`
	Resume string `json:"resume"`
}

func (e *env) state(id string) state {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.lastRun(id), "state.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// events は、最後の実行の log.jsonl の、名前が name の出来事を返す。
func (e *env) events(id, name string) []map[string]any {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.lastRun(id), "log.jsonl"))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(string(data), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == name {
			out = append(out, m)
		}
	}
	return out
}

func lineOf(body, text string) int {
	for i, l := range strings.Split(body, "\n") {
		if strings.Contains(l, text) {
			return i + 1
		}
	}
	return 0
}

func res(r map[string]any) map[string]any { return map[string]any{"result": r} }

var verifyOK = []map[string]any{{"name": "build", "exitCode": 0}, {"name": "test", "exitCode": 0}, {"name": "lint", "exitCode": 0}}

// P2 から ready-for-review まで、本物の Docker と git で流す。M2 で実際の AI で初めて見つかった場面
// （指示書を根拠にする指摘、main の進み、予定の ID を根拠にする指摘）と、P5 の2分割を通す（M3 の Q-12）。
func TestE2E_FullFlow(t *testing.T) {
	e := setup(t)
	body := ticketBody("T-001", "引き算を足す", "ready-to-prepare", "UC-001、UC-001-AC-02", "")
	e.ticket("T-001", body)

	// 1回目：P2（指示書の行と、予定の ID を根拠にする IMO）→ P3 → 承認待ち
	sc1 := e.scenario("t001-1", map[string][]map[string]any{
		"p2-review": {res(map[string]any{"outcome": "approved", "summary": "問題なし", "findings": []map[string]any{
			{"label": "IMO", "finding": "§2 の要求に、負の数の扱いを書くとよい", "evidence": map[string]any{"file": "/in/ticket.md", "line": lineOf(body, "引き算などを足す")}},
			{"label": "IMO", "finding": "予定の受入条件に、結果の範囲を書くとよい", "evidence": map[string]any{"id": "UC-001-AC-02"}},
		}})},
		"p3-prepare": {{"result": map[string]any{"outcome": "done", "summary": "準備した", "current": "calc に足し算だけがある", "plan": "calc に Sub を足す",
			"acceptance": []map[string]any{{"id": "UC-001-AC-02", "uc": "UC-001", "content": "Sub で引き算する"}},
			"testCases":  []map[string]any{{"test": "TestSub", "layer": "scenario", "targets": []string{"UC-001-AC-02"}, "content": "引き算の結果を確かめる"}},
			"sheets":     []map[string]any{{"file": ".docs/design/uc/UC-001.md", "change": "受入条件を足す"}},
			"imo":        []map[string]any{{"index": 0, "decision": "planned", "detail": "§4.1 に入れた"}, {"index": 1, "decision": "deferred", "detail": "範囲は求めない"}},
		}, "files": map[string]string{".docs/design/uc/UC-001.md": ucDetail + "| UC-001-AC-02 | Sub で引き算する | シナリオテスト |\n"}}},
	})
	e.gateflow(sc1, "run", "T-001")
	if s := e.status("T-001"); s != "ready-to-approve" {
		t.Fatalf("1回目の後のステータス = %s、止まった理由 %+v", s, e.state("T-001").Stopped)
	}

	// 人：承認する。その間に main が進む（別のコミット）
	tk := filepath.Join(e.root, ".docs/_stash/tickets/T-001/ticket.md")
	data, _ := os.ReadFile(tk)
	os.WriteFile(tk, []byte(strings.ReplaceAll(string(data), "- [ ]", "- [x]")), 0o644)
	e.gateflow(sc1, "approve", "T-001")
	e.write("README.md", "e2e\n")
	e.git("add", "README.md")
	e.git("commit", "-q", "-m", "docs: README")
	main := e.git("rev-parse", "main")

	// 2回目：p5-test → red → p5-implement（新しいテストのファイルを足すのはよい）→ 検証 → P5後 → レビュー待ち
	sc2 := e.scenario("t001-2", map[string][]map[string]any{
		"p5-test": {{"result": map[string]any{"outcome": "done", "summary": "テストを書いた",
			"tests": []map[string]any{{"test": "TestSub", "file": "calc/sub_test.go", "acceptance": []string{"UC-001-AC-02"}}}},
			"files": map[string]string{"calc/sub_test.go": "package calc\n\nimport \"testing\"\n\n// UC-001-AC-02\nfunc TestSub(t *testing.T) {\n\tif Sub(3, 1) != 2 {\n\t\tt.Fatal(\"Sub\")\n\t}\n}\n"}}},
		"p5-implement": {{"result": map[string]any{"outcome": "done", "summary": "実装した", "verify": verifyOK},
			"files": map[string]string{
				"calc/sub.go":        "package calc\n\n// Sub は引き算をする。\nfunc Sub(a, b int) int { return a - b }\n",
				"calc/extra_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestSubZero(t *testing.T) {\n\tif Sub(1, 1) != 0 {\n\t\tt.Fatal(\"Sub\")\n\t}\n}\n",
			}}},
		"p5-verify": {res(map[string]any{"outcome": "approved", "summary": "突き合わせた"})},
	})
	e.gateflow(sc2, "run", "T-001")
	if s := e.status("T-001"); s != "ready-for-review" {
		t.Fatalf("2回目の後のステータス = %s、止まった理由 %+v", s, e.state("T-001").Stopped)
	}
	if ws := e.events("T-001", "workspace"); len(ws) != 1 || ws[0]["main"] != main {
		t.Errorf("clone が進んだ main（%s）を見ていない: %v", main, ws)
	}
	if red := e.events("T-001", "red"); len(red) != 1 || red[0]["exitCode"] == float64(0) {
		t.Errorf("red の確かめ = %v", red)
	}
	if rec, err := os.ReadFile(filepath.Join(e.lastRun("T-001"), "recommended.md")); err == nil {
		t.Logf("推奨の指摘:\n%s", rec)
	}
	log := e.git("log", "--format=%s", "main..feature/t-001")
	for _, w := range []string{"test(T-001): ", "feat(T-001): ", "docs(T-001): "} {
		if !strings.Contains(log, w) {
			t.Errorf("作業ブランチに %q のコミットがない:\n%s", w, log)
		}
	}
}

// 完了の基準：p5-implement が p5-test のテストを書き換えたら（消したら）、検証の前に test-modified で止まる（M3）。
func TestE2E_TestModifiedStops(t *testing.T) {
	e := setup(t)
	testFile := func(fn, op string) string {
		return fmt.Sprintf("package calc\n\nimport \"testing\"\n\n// UC-001-AC-02\nfunc Test%[1]s(t *testing.T) {\n\tif %[1]s(6, 3) != %[2]s {\n\t\tt.Fatal(%[1]q)\n\t}\n}\n", fn, op)
	}
	cases := []struct {
		id, fn, op string
		implement  map[string]any
	}{
		{"T-002", "Mul", "18", map[string]any{
			"result": map[string]any{"outcome": "done", "summary": "実装した", "verify": verifyOK},
			"files": map[string]string{
				"calc/mul.go":      "package calc\n\nfunc Mul(a, b int) int { return 0 }\n",
				"calc/mul_test.go": "package calc\n\nimport \"testing\"\n\n// UC-001-AC-02\nfunc TestMul(t *testing.T) {}\n", // テストを弱めて通そうとする
			}}},
		{"T-003", "Div", "2", map[string]any{
			"result": map[string]any{"outcome": "done", "summary": "実装した", "verify": verifyOK},
			"files":  map[string]string{"calc/div.go": "package calc\n\nfunc Div(a, b int) int { return 0 }\n"},
			"remove": []string{"calc/div_test.go"}, // テストを消して通そうとする
		}},
	}
	for _, c := range cases {
		e.ticket(c.id, ticketBody(c.id, c.fn+" を足す", "ready-to-implement", "UC-001", planFor("Test"+c.fn, "UC-001-AC-02")))
		sc := e.scenario(c.id, map[string][]map[string]any{
			"p5-test": {{"result": map[string]any{"outcome": "done", "summary": "テストを書いた",
				"tests": []map[string]any{{"test": "Test" + c.fn, "file": "calc/" + strings.ToLower(c.fn) + "_test.go", "acceptance": []string{"UC-001-AC-02"}}}},
				"files": map[string]string{"calc/" + strings.ToLower(c.fn) + "_test.go": testFile(c.fn, c.op)}}},
			"p5-implement": {c.implement},
		})
		e.gateflow(sc, "run", c.id)
		st := e.state(c.id)
		if st.Stopped == nil || st.Stopped.Reason != "test-modified" || !strings.Contains(st.Stopped.Detail, strings.ToLower(c.fn)+"_test.go") {
			t.Errorf("%s：止まった理由 = %+v", c.id, st.Stopped)
			continue
		}
		if st.Resume != "p5-implement" || len(e.events(c.id, "verify")) != 0 {
			t.Errorf("%s：resume %q、検証 %d 回（止めた後に検証しない）", c.id, st.Resume, len(e.events(c.id, "verify")))
		}
	}
}
