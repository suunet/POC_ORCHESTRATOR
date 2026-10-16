package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/builtins"
	"gateflow/internal/agent/mock"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/engine"
	"gateflow/internal/idcheck"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
	"gateflow/internal/workspace"
)

const flowTicket = `# T-002 ログの基盤を作る

ステータス: ready-to-prepare

| 項目 | 値 |
| --- | --- |
| 種別 | 実装 |
| ブランチ | feature/t-002-log |

## §1 目的

- ログをそろえる

## §2 要求

- UC-001 のヘルスチェックの要求のログに、NFR-09 の必須の項目を入れる

## §3 変更前の現状（P3でAIが書く）

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

// 書き出しで展開するカタログ（ID の検査と根拠の確認に使う）。
var catalog = map[string]string{
	".docs/design/buc.md":       "| BUC | UC |\n|---|---|\n| BUC-S01 | UC-001（システムの死活を確認する） |\n",
	".docs/design/uc/UC-001.md": "# UC-001\n\n| ID |\n|---|\n| UC-001-AC-01 |\n| UC-001-AC-02 |\n",
	"backend/log.go":            "package log\n\nfunc A() {}\n",
}

const (
	p2Approved = `{"outcome": "approved", "summary": "問題なし"}`
	p3Done     = `{"outcome": "done", "summary": "準備した", "current": "ログの基盤はない", "plan": "common/log に作る",
	  "acceptance": [{"id": "UC-001-AC-02", "uc": "UC-001", "content": "ログに必須の項目がある"}],
	  "testCases": [{"test": "TestLog", "layer": "scenario", "targets": ["UC-001-AC-02"], "content": "項目を確かめる"}],
	  "sheets": [{"file": ".docs/design/uc/UC-001.md", "change": "受入条件を足す"}]}`
	p5TestDone = `{"outcome": "done", "summary": "テストを書いた", "tests": [{"test": "TestLog", "file": "backend/tests/scenario/log_test.go", "acceptance": ["UC-001-AC-02"]}]}`
	p5Done     = `{"outcome": "done", "summary": "実装した", "verify": [{"name": "build", "exitCode": 0}, {"name": "test", "exitCode": 0}, {"name": "lint", "exitCode": 0}], "tests": [{"test": "TestLog", "file": "backend/tests/scenario/log_test.go", "acceptance": ["UC-001-AC-02"]}]}`
	vApproved  = `{"outcome": "approved", "summary": "突き合わせた"}`
	vMust      = `{"outcome": "needs_fix", "summary": "直す", "findings": [{"label": "MUST", "finding": "エラーを無視している", "evidence": {"file": "backend/log.go", "line": 3}}]}`
)

// flowWorkspace は、workspace の偽物。書き出しでカタログを展開する。
type flowWorkspace struct {
	commits     []string
	imports     []string
	trees       []string // TreeHash が順に返す値。尽きたら呼ばれるたびに違う値
	treeCall    int
	prepared    workspace.Prepared
	prepareOpts []workspace.PrepareOptions
	tracked     map[string]bool
	workChanges map[string]string // ARB が作業ツリーで直したことにするファイル（Snapshot に重ねる）
	// workChangesOnce なら、最初の Snapshot（最初の ARB）でだけ直したことにする
	workChangesOnce bool
	snapshots       int
	resets          int
	prepareErr      error             // あれば Prepare が返す（clone の用意の失敗）
	beforePrepare   func()            // あれば Prepare の始めに呼ぶ（M2.8）
	mergedExtra     map[string]string // main と合わせた木で、カタログのファイルの末尾に足す行（M2.9）
	mergedConflict  bool
	testsChanged    []string // TestsChanged が返すファイル（M3）
	unchangedTests  bool     // p5-test のコミットで変更がなかったことにする（M3）
}

func (w *flowWorkspace) Prepare(_ context.Context, _, _, _ string, _ *config.Deps, opt workspace.PrepareOptions) (workspace.Prepared, error) {
	w.prepareOpts = append(w.prepareOpts, opt)
	if w.beforePrepare != nil {
		w.beforePrepare()
	}
	if w.prepareErr != nil {
		return workspace.Prepared{}, w.prepareErr
	}
	return w.prepared, nil
}
func (w *flowWorkspace) Snapshot(_ context.Context, _, dest string) error {
	for p, body := range catalog {
		os.MkdirAll(filepath.Dir(filepath.Join(dest, p)), 0o755)
		os.WriteFile(filepath.Join(dest, p), []byte(body), 0o644)
	}
	w.snapshots++
	if w.workChangesOnce && w.snapshots > 1 {
		return nil
	}
	for p, body := range w.workChanges {
		os.MkdirAll(filepath.Dir(filepath.Join(dest, p)), 0o755)
		os.WriteFile(filepath.Join(dest, p), []byte(body), 0o644)
	}
	return nil
}
func (w *flowWorkspace) Reset(context.Context, string) error {
	w.resets++
	return nil
}
func (w *flowWorkspace) AgentMounts(id string) []container.Mount {
	return []container.Mount{
		{Type: container.Volume, Source: "v", Subpath: id + "/repo", Target: "/work"},
		{Type: container.Volume, Source: "v", Subpath: id + "/repo/.git", Target: "/work/.git", ReadOnly: true},
	}
}
func (w *flowWorkspace) ReviewerMounts(id string) []container.Mount {
	return []container.Mount{{Type: container.Volume, Source: "v", Subpath: id + "/repo", Target: "/work", ReadOnly: true}}
}
func (w *flowWorkspace) CommitHash(_ context.Context, _, msg string, _ workspace.Author) (string, bool, error) {
	w.commits = append(w.commits, msg)
	if w.unchangedTests && strings.HasPrefix(msg, "test(") {
		return fmt.Sprintf("c%d", len(w.commits)), false, nil
	}
	return fmt.Sprintf("c%d", len(w.commits)), true, nil
}
func (w *flowWorkspace) Import(_ context.Context, _, branch string) error {
	w.imports = append(w.imports, branch)
	return nil
}

// MergedTree は、main と作業ブランチを合わせた木として、カタログに mergedExtra を重ねて書く（M2.9）。
func (w *flowWorkspace) MergedTree(_ context.Context, _, _, _, dest string) (bool, error) {
	if w.mergedConflict {
		return true, nil
	}
	for p, body := range catalog {
		os.MkdirAll(filepath.Dir(filepath.Join(dest, p)), 0o755)
		os.WriteFile(filepath.Join(dest, p), []byte(body+w.mergedExtra[p]), 0o644)
	}
	return false, nil
}

func (w *flowWorkspace) Export(_ context.Context, _, dest string) (string, error) {
	for p, body := range catalog {
		os.MkdirAll(filepath.Dir(filepath.Join(dest, p)), 0o755)
		os.WriteFile(filepath.Join(dest, p), []byte(body), 0o644)
	}
	return fmt.Sprintf("c%d", len(w.commits)), nil
}
func (w *flowWorkspace) Diff(context.Context, string, string) (string, error) {
	return "diff --git a/backend/log.go b/backend/log.go\n+code\ndiff --git a/.docs/design/uc/UC-001.md b/.docs/design/uc/UC-001.md\n+UC-001 のログの項目\n", nil
}
func (w *flowWorkspace) Tracked(_ context.Context, _, path string) bool { return w.tracked[path] }
func (w *flowWorkspace) TestsChanged(context.Context, string, string) ([]string, error) {
	return w.testsChanged, nil
}
func (w *flowWorkspace) TreeHash(context.Context, string) (string, error) {
	w.treeCall++
	if w.treeCall <= len(w.trees) {
		return w.trees[w.treeCall-1], nil
	}
	return fmt.Sprintf("tree-%d", w.treeCall), nil
}

// flowRunner は、エージェントの代わりに step ごとの出力を順に書き、検証コマンドには codes を順に返す。
type flowRunner struct {
	results   map[string][]string // step → 出力の並び（尽きたら最後を繰り返す）
	codes     []int               // 検証コマンドの終了コード（build・test・lint の順に呼ばれる）
	outputs   []string            // 検証コマンドの出力（codes と同じ順）
	calls     map[string]int
	specs     map[string][]container.Spec
	inputs    map[string][]map[string]string // step → 呼ばれるたびの /in のファイルの中身
	verifyIdx int
	// onRun は、エージェントの step が走るたびに呼ぶ（stepDir は step の記録のディレクトリ）。書けない状態を作るのに使う（M2.8）
	onRun      func(step, stepDir string)
	reds       int      // red の確かめの回数（M3）
	greens     int      // red の確かめのうち、最初の greens 回はテストが通ってしまったことにする
	verifyNets []string // 検証のコンテナのネットワーク（M3）
	onSpec     func(container.Spec)
}

func newRunner(results map[string][]string) *flowRunner {
	return &flowRunner{results: results, calls: map[string]int{}, specs: map[string][]container.Spec{}, inputs: map[string][]map[string]string{}}
}

func stepOf(name string) string {
	for _, s := range []string{"p5-verify", "p2-review", "p2-arbitrate", "p3-prepare", "p5-test", "p5-implement"} {
		if strings.HasSuffix(name, "-"+s) {
			return s
		}
	}
	return ""
}

func (f *flowRunner) Run(_ context.Context, spec container.Spec, stdout, _ io.Writer) (int, error) {
	step := stepOf(spec.Name)
	if f.onSpec != nil {
		f.onSpec(spec)
	}
	if strings.HasSuffix(spec.Name, "-red-test") { // red：新しいテストが失敗するか（M3）
		f.reds++
		if f.reds <= f.greens {
			io.WriteString(stdout, "PASS\n")
			return 0, nil
		}
		io.WriteString(stdout, "FAIL TestLog\n")
		return 1, nil
	}
	if step == "" { // 検証コマンド
		f.verifyNets = append(f.verifyNets, spec.Network)
		i := f.verifyIdx
		f.verifyIdx++
		code, out := 0, "ok\n"
		if i < len(f.codes) {
			code = f.codes[i]
		}
		if i < len(f.outputs) {
			out = f.outputs[i]
		}
		io.WriteString(stdout, out)
		return code, nil
	}
	f.specs[step] = append(f.specs[step], spec)
	in := map[string]string{}
	for _, m := range spec.Mounts {
		if strings.HasPrefix(m.Target, "/in/") {
			b, _ := os.ReadFile(m.Source)
			in[m.Target] = string(b)
		}
	}
	f.inputs[step] = append(f.inputs[step], in)
	if f.onRun != nil {
		for _, m := range spec.Mounts {
			if m.Target == "/out" {
				f.onRun(step, filepath.Dir(m.Source))
			}
		}
	}
	rs := f.results[step]
	if len(rs) > 0 {
		r := rs[min(f.calls[step], len(rs)-1)]
		for _, m := range spec.Mounts {
			if m.Target == "/out" {
				os.WriteFile(filepath.Join(m.Source, "result.json"), []byte(r), 0o644)
			}
		}
	}
	f.calls[step]++
	return 0, nil
}

type flowFixture struct {
	e      *engine.Engine
	ws     *flowWorkspace
	runner *flowRunner
	root   string
}

func newFlow(t *testing.T, body string, results map[string][]string) *flowFixture {
	t.Helper()
	root := t.TempDir()
	write := func(rel, b string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(b), 0o644)
	}
	write(".docs/_stash/tickets/T-002/ticket.md", body)
	write(".gateflow/env.template", "A=1\n")
	write(".docs/operations/policies/coding.md", "規約\n")
	tk, err := ticket.Load(root, "T-002")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := idcheck.NewRules(config.IDs{Scan: []string{".docs/design"}, Prefixes: []config.Prefix{
		{Prefix: "UC", Pattern: "UC-[0-9]{3}", Files: []string{".docs/design/buc.md"}, Find: "table-column:UC"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f := &flowFixture{ws: &flowWorkspace{}, runner: newRunner(results), root: root}
	f.e = &engine.Engine{
		Root: root,
		Project: &config.Project{
			Name: "sample", Branch: &config.Branch{Main: "main"},
			Agent:  &config.Agent{EnvTemplate: ".gateflow/env.template"},
			Verify: &config.Verify{Build: "go build ./...", Test: "go test ./...", Lint: "go vet ./..."},
			Policies: []config.Policy{
				{Path: ".docs/operations/policies/coding.md", Roles: []string{"dev"}, Purpose: "コーディング規約"},
			},
		},
		Workflow: wf, Ticket: tk, AgentImage: "img", Agent: &mock.Agent{},
		Workspace: f.ws, Egress: &fakeEgress{}, Runner: f.runner,
		Author: workspace.Author{Name: "人", Email: "h@example.invalid"}, Builtins: builtins.FS(), Rules: rules,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) },
	}
	return f
}

func (f *flowFixture) status(t *testing.T) ticket.Status {
	t.Helper()
	tk, err := ticket.Load(f.root, "T-002")
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func (f *flowFixture) reload(t *testing.T) {
	t.Helper()
	tk, err := ticket.Load(f.root, "T-002")
	if err != nil {
		t.Fatal(err)
	}
	f.e.Ticket = tk
}

func mustRun(t *testing.T, f *flowFixture) engine.Result {
	t.Helper()
	res, err := f.e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func readState(t *testing.T, runDir string) *engine.State {
	t.Helper()
	s, err := engine.LoadState(runDir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 関門以外の介入なしで、P2 → P3 → approve → P5 → 検証 → P5後 → ready-for-review まで進む。
func TestRun_FullFlow(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{
		"p2-review": {p2Approved}, "p3-prepare": {p3Done}, "p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved},
	})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.ReadyToApprove || !strings.Contains(res.Next, "approve T-002") {
		t.Fatalf("1回目 = %+v, status %s", res, f.status(t))
	}
	tk, _ := ticket.Load(f.root, "T-002")
	if len(tk.MissingSections()) != 0 || !strings.Contains(tk.Body, "| UC-001-AC-02 | UC-001 | ログに必須の項目がある |") {
		t.Errorf("§3・§4 が書かれていない:\n%s", tk.Body)
	}
	if len(f.ws.commits) != 1 || !strings.HasPrefix(f.ws.commits[0], "docs(T-002): ") || len(f.ws.imports) != 1 {
		t.Errorf("P3 のコミットと取り込み = %q, %q", f.ws.commits, f.ws.imports)
	}

	// 関門：§4.6 がすべてチェックされていなければ approve できない。
	if err := engine.Human(tk, f.e.Workflow, "approve", f.e.Now(), engine.Checks{}); err == nil {
		t.Fatal("§4.6 が未チェックなのに approve できた")
	}
	os.WriteFile(tk.Path, []byte(strings.ReplaceAll(tk.Body, "- [ ]", "- [x]")), 0o644)
	f.reload(t)
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "approve", f.e.Now(), engine.Checks{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	f.reload(t)

	res = mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.ReadyForReview {
		t.Fatalf("2回目 = %+v, status %s", res, f.status(t))
	}
	if len(f.ws.commits) != 3 || !strings.HasPrefix(f.ws.commits[1], "test(T-002): ") || !strings.HasPrefix(f.ws.commits[2], "feat(T-002): ") || len(f.ws.imports) != 2 {
		t.Errorf("P5 のコミットと取り込み = %q, %q", f.ws.commits, f.ws.imports)
	}
	if f.runner.reds != 1 {
		t.Errorf("red の確かめ = %d 回", f.runner.reds)
	}

	// BJ（P5後）に渡すのは、指示書・DEV の結果・diff・検証の出力だけで、clone は読み取り専用。
	in := f.runner.inputs["p5-verify"][0]
	for _, k := range []string{"/in/ticket.md", "/in/dev-result.json", "/in/diff.patch", "/in/verify.log"} {
		if _, ok := in[k]; !ok {
			t.Errorf("P5後に %s が渡されていない: %v", k, keys(in))
		}
	}
	for _, m := range f.runner.specs["p5-verify"][0].Mounts {
		// 結果の置き場所と、AI サービスの認証（ログインの情報の更新に要る）だけが書き込み可
		if m.Target != "/out" && m.Target != "/auth" && !m.ReadOnly {
			t.Errorf("BJ のマウント %s が書き込み可", m.Target)
		}
		if m.Target == "/work/.env" {
			t.Error("BJ に .env を渡している")
		}
	}
	// 記録：BJ が見た版と、ID の検査の版を残す。
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	for _, w := range []string{`"event":"reviewed-version"`, `"event":"check-ids"`, `"commit":"c2"`} {
		if !strings.Contains(string(log), w) {
			t.Errorf("記録に %s がない:\n%s", w, log)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// P2 の MUST と Q は質疑に変換して draft へ戻す。レビュー報告も作る。
func TestRun_P2NeedsFix(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {`{"outcome": "needs_fix", "summary": "穴がある",
	  "findings": [
	    {"label": "MUST", "finding": "UC-001 の要求とログの項目が対応していない", "evidence": {"id": "UC-001"}, "suggestion": "§2 に項目を書く"},
	    {"label": "Q", "finding": "出力先は標準出力でよいか", "evidence": {"file": "backend/log.go", "line": 1}},
	    {"label": "IMO", "finding": "名前", "evidence": {"file": "backend/log.go"}}]}`}})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.Draft || f.runner.calls["p2-arbitrate"] != 0 {
		t.Fatalf("res = %+v, status %s, ARB の呼び出し %d", res, f.status(t), f.runner.calls["p2-arbitrate"])
	}
	n, _ := f.e.Ticket.UnresolvedQuestions()
	if n != 2 {
		t.Errorf("未確定の質疑 = %d, want 2（MUST と Q。IMO は質疑にしない）", n)
	}
	q, _ := os.ReadFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002/questions.md"))
	if !strings.Contains(string(q), "指示書の修正が必要") || !strings.Contains(string(q), "§2 に項目を書く") {
		t.Errorf("質疑:\n%s", q)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".docs/_stash/tickets/T-002/reviews/01-p2-review.md")); err != nil {
		t.Errorf("レビュー報告がない: %v", err)
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "ready", f.e.Now(), engine.Checks{}); err == nil {
		t.Error("未確定の質疑があるのに ready できた")
	}
}

// 指摘の根拠が BJ の見た版にないときは、出力の不正として1回だけ再実行する。
func TestRun_EvidenceMissing(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{
		"p2-review":  {`{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "MUST", "finding": "f", "evidence": {"file": "nothing.go", "line": 1}}]}`, p2Approved},
		"p3-prepare": {p3Done},
	})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.runner.calls["p2-review"] != 2 {
		t.Fatalf("res = %+v, p2 の呼び出し = %d", res, f.runner.calls["p2-review"])
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), "nothing.go") {
		t.Errorf("根拠のない指摘の記録がない:\n%s", log)
	}
}

// 根拠が BJ に渡した入力（指示書・規約）を指すときは、入力の写しで確かめる（Q-22）。
// 規約はどちらのパスで書かれても受け付け、報告ではリポジトリのパスにそろえる。
func TestRun_EvidenceInputs(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{
		"p2-review": {`{"outcome": "approved", "summary": "s", "findings": [
		  {"label": "IMO", "finding": "a", "evidence": {"file": "/in/ticket.md", "line": 3}},
		  {"label": "IMO", "finding": "b", "evidence": {"file": "/in/policies/.docs/operations/policies/coding.md", "line": 1}},
		  {"label": "IMO", "finding": "c", "evidence": {"file": ".docs/operations/policies/coding.md", "line": 1}}]}`},
		// P2 の IMO には P3 が1件ずつ対応を書く（M2.7）
		"p3-prepare": {strings.Replace(p3Done, `"plan": "common/log に作る",`, `"plan": "common/log に作る", "imo": [
		  {"index": 0, "decision": "deferred", "detail": "範囲外"}, {"index": 1, "decision": "deferred", "detail": "範囲外"},
		  {"index": 2, "decision": "deferred", "detail": "範囲外"}],`, 1)},
	})
	f.e.Project.Policies[0].Roles = []string{"dev", "bj"}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.runner.calls["p2-review"] != 1 {
		t.Fatalf("res = %+v, p2 の呼び出し = %d", res, f.runner.calls["p2-review"])
	}
	steps, _ := filepath.Glob(filepath.Join(res.RunDir, "steps", "*p2-review", "in", "ticket.md"))
	if len(steps) != 1 {
		t.Errorf("指示書の写しがない: %v", steps)
	}
	review, _ := os.ReadFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002/reviews/01-p2-review.md"))
	if strings.Contains(string(review), "/in/policies/") || !strings.Contains(string(review), "/in/ticket.md:3") {
		t.Errorf("報告の根拠のパス:\n%s", review)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"checked":"input:ticket.md"`) || !strings.Contains(string(log), `"checked":"input:policies/.docs/operations/policies/coding.md"`) {
		t.Errorf("根拠の検査の記録がない:\n%s", log)
	}
}

// 入力の写しの行数を超える根拠は、出力の不正。
func TestRun_EvidenceInputLine(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{
		"p2-review":  {`{"outcome": "approved", "summary": "s", "findings": [{"label": "IMO", "finding": "a", "evidence": {"file": "/in/ticket.md", "line": 999}}]}`, p2Approved},
		"p3-prepare": {p3Done},
	})
	res := mustRun(t, f)
	if f.runner.calls["p2-review"] != 2 {
		t.Fatalf("res = %+v, p2 の呼び出し = %d", res, f.runner.calls["p2-review"])
	}
}

// 作業ブランチが main の進みを取り込めないときは、止めずに警告し、使った main を記録する（Q-23）。
func TestRun_MainBehindWarning(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.ws.prepared = workspace.Prepared{Main: "abcdef0123456789", Head: "1234567", Behind: true}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "abcdef0") {
		t.Fatalf("res = %+v", res)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"main":"abcdef0123456789"`) || !strings.Contains(string(log), `"behind":true`) {
		t.Errorf("clone の用意の記録がない:\n%s", log)
	}
}

// Git 管理の規約は clone の版（/work）を読ませ、ホストからは渡さない（Q-23）。
func TestRun_TrackedPolicyFromClone(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	f.e.Project.Policies[0].Roles = []string{"dev", "bj"}
	f.ws.tracked = map[string]bool{".docs/operations/policies/coding.md": true}
	res := mustRun(t, f)
	prompts, _ := filepath.Glob(filepath.Join(res.RunDir, "steps", "*p2-review", "prompt.md"))
	if len(prompts) != 1 {
		t.Fatalf("prompt.md = %v", prompts)
	}
	data, _ := os.ReadFile(prompts[0])
	if !strings.Contains(string(data), "/work/.docs/operations/policies/coding.md") || strings.Contains(string(data), "/in/policies/") {
		t.Errorf("規約の場所:\n%s", data)
	}
	if copies, _ := filepath.Glob(filepath.Join(res.RunDir, "steps", "*p2-review", "in", "policies")); len(copies) != 0 {
		t.Errorf("Git 管理の規約をホストから渡した: %v", copies)
	}
}

// 指示書の対象ID に挙げた予定の ID は、カタログになくても根拠に使える。報告に「（予定）」と出す（Q-24）。
func TestRun_EvidencePlannedID(t *testing.T) {
	body := strings.Replace(flowTicket, "| ブランチ |", "| 対象ID | UC-001、UC-009 |\n| ブランチ |", 1)
	f := newFlow(t, body, map[string][]string{
		"p2-review": {`{"outcome": "approved", "summary": "s", "findings": [
		  {"label": "FYI", "finding": "UC-009 はまだない", "evidence": {"id": "UC-009"}}]}`},
		"p3-prepare": {p3Done},
	})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.runner.calls["p2-review"] != 1 {
		t.Fatalf("res = %+v, p2 の呼び出し = %d", res, f.runner.calls["p2-review"])
	}
	review, _ := os.ReadFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002/reviews/01-p2-review.md"))
	if !strings.Contains(string(review), "UC-009（予定）") {
		t.Errorf("報告に予定の印がない:\n%s", review)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"checked":"planned"`) {
		t.Errorf("予定の ID の記録がない:\n%s", log)
	}
}

// 対象ID にない ID や、ID の形式に合わない ID は、予定として受け付けない。
func TestRun_EvidenceNotPlanned(t *testing.T) {
	body := strings.Replace(flowTicket, "| ブランチ |", "| 対象ID | UC-001、UC-009、XYZ-1 |\n| ブランチ |", 1)
	for _, id := range []string{"UC-008", "XYZ-1"} {
		f := newFlow(t, body, map[string][]string{
			"p2-review":  {`{"outcome": "approved", "summary": "s", "findings": [{"label": "FYI", "finding": "f", "evidence": {"id": "` + id + `"}}]}`, p2Approved},
			"p3-prepare": {p3Done},
		})
		mustRun(t, f)
		if f.runner.calls["p2-review"] != 2 {
			t.Errorf("%s: p2 の呼び出し = %d, want 2（出力の不正で再実行）", id, f.runner.calls["p2-review"])
		}
	}
}

// P5 後に、対象ID の予定の ID がカタログになければ、P5 へ差し戻す（Q-24）。
func TestRun_TargetIDMissingAfterP5(t *testing.T) {
	body := strings.Replace(approvedTicket(t), "| ブランチ |", "| 対象ID | UC-001、UC-009 |\n| ブランチ |", 1)
	f := newFlow(t, body, map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vApproved}})
	res := mustRun(t, f)
	if f.runner.calls["p5-implement"] < 2 || res.Outcome != engine.Stopped {
		t.Fatalf("res = %+v, P5 の呼び出し = %d", res, f.runner.calls["p5-implement"])
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"target-ids-missing"`) || !strings.Contains(string(log), "UC-009") {
		t.Errorf("対象ID の不足の記録がない:\n%s", log)
	}
	fb, _ := filepath.Glob(filepath.Join(res.RunDir, "steps", "*", "feedback.md"))
	found := false
	for _, p := range fb {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "UC-009") {
			found = true
		}
	}
	if !found {
		t.Errorf("差し戻しの内容に UC-009 がない: %v", fb)
	}
}

func approvedTicket(t *testing.T) string {
	t.Helper()
	body := strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1)
	return strings.ReplaceAll(body, "- [ ]", "- [x]")
}

// 検証に不合格なら、失敗の内容を付けて P5 へ戻す。
func TestRun_VerifyLoop(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vApproved}})
	f.runner.codes = []int{0, 1, 0, 0, 0, 0}
	f.runner.outputs = []string{"ok", "--- FAIL: TestLog (0.1s)", "ok", "ok", "ok", "ok"}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.runner.calls["p5-implement"] != 2 {
		t.Fatalf("res = %+v, P5 の呼び出し = %d", res, f.runner.calls["p5-implement"])
	}
	if fb := f.runner.inputs["p5-implement"][1]["/in/feedback.md"]; !strings.Contains(fb, "TestLog") {
		t.Errorf("2回目の P5 に差し戻しの内容が渡されていない: %q", fb)
	}
	if s := readState(t, res.RunDir); s.Loops["verify"] != 1 {
		t.Errorf("loops = %v", s.Loops)
	}
}

// ループの往復が上限を超えたら、再開の位置を残して止める。
func TestRun_LoopLimit(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}})
	f.e.Project.Limits = &config.Limits{Loops: 2}
	for i := range 30 {
		f.runner.codes = append(f.runner.codes, 1)
		f.runner.outputs = append(f.runner.outputs, fmt.Sprintf("--- FAIL: Test%d", i))
	}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "limit-loops" {
		t.Fatalf("res = %+v", res)
	}
	if f.runner.calls["p5-implement"] != 3 {
		t.Errorf("P5 の呼び出し = %d, want 3（最初と2往復）", f.runner.calls["p5-implement"])
	}
	if s := readState(t, res.RunDir); s.Resume != "p5-implement" || s.Stopped == nil {
		t.Errorf("state = %+v", s)
	}
}

// 同じ検証の失敗（所要時間などを除いて同じ出力）が続いたら、上限の前でも止める。
func TestRun_NoProgressVerify(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}})
	for i := range 30 {
		f.runner.codes = append(f.runner.codes, 1)
		f.runner.outputs = append(f.runner.outputs, fmt.Sprintf("--- FAIL: TestLog (0.%ds)", i))
	}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "no-progress" || f.runner.calls["p5-implement"] != 2 {
		t.Fatalf("res = %+v, P5 の呼び出し = %d", res, f.runner.calls["p5-implement"])
	}
	// 正規化の前と後の両方を記録に残す。
	matches, _ := filepath.Glob(filepath.Join(res.RunDir, "steps", "*", "fingerprint-*.txt"))
	if len(matches) < 2 {
		t.Errorf("指紋の記録 = %v", matches)
	}
}

// 前回と同じ MUST の指摘が残っていたら止める。
func TestRun_NoProgressReview(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vMust}})
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "no-progress" || f.runner.calls["p5-verify"] != 2 {
		t.Fatalf("res = %+v, P5後 の呼び出し = %d", res, f.runner.calls["p5-verify"])
	}
}

// 差し戻しの後の P5 で作業ツリーが変わらなければ止める。
func TestRun_NoProgressDiff(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}})
	f.ws.trees = []string{"same", "same"}
	f.runner.codes = []int{0, 1, 0, 0, 1, 0}
	f.runner.outputs = []string{"ok", "--- FAIL: A", "ok", "ok", "--- FAIL: B", "ok"}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "no-progress" {
		t.Fatalf("res = %+v", res)
	}
}

// step の数が上限を超えたら止める。
func TestRun_StepLimit(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vApproved}})
	f.e.Project.Limits = &config.Limits{Steps: 2}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "limit-steps" {
		t.Fatalf("res = %+v", res)
	}
	if s := readState(t, res.RunDir); s.Resume != "p5-verify" {
		t.Errorf("再開の位置 = %q, want p5-verify", s.Resume)
	}
}

// blocked で止まり、人が原因を直した後に resume で続けられる。
func TestRun_BlockedAndResume(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{
		"p5-implement": {`{"outcome": "blocked", "summary": "依存", "blockReason": {"kind": "dependency", "detail": "x がない"}}`, p5Done},
		"p5-verify":    {vApproved},
	})
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "blocked" || !strings.Contains(res.Next, "resume T-002") || f.status(t) != ticket.Implementing {
		t.Fatalf("res = %+v, status %s", res, f.status(t))
	}
	f.reload(t)
	res, err := f.e.Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Outcome != engine.Gate || f.status(t) != ticket.ReadyForReview {
		t.Fatalf("resume = %+v, status %s", res, f.status(t))
	}
}

// 出力の形が2回続けて違えば止める。
func TestRun_InvalidOutput(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {`{"outcome": "done"}`}})
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "invalid-output" || f.runner.calls["p5-implement"] != 2 {
		t.Fatalf("res = %+v, P5 の呼び出し = %d", res, f.runner.calls["p5-implement"])
	}
}

// 機械検証（受入条件の確認）に不合格なら、失敗の内容を付けて同じ step に差し戻す。
func TestRun_CheckFailsLoopsToSameStep(t *testing.T) {
	bad := strings.Replace(strings.Replace(p3Done, "UC-001-AC-02", "UC-001-AC-09", -1), "", "", 1)
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {bad, p3Done}})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.runner.calls["p3-prepare"] != 2 {
		t.Fatalf("res = %+v, P3 の呼び出し = %d", res, f.runner.calls["p3-prepare"])
	}
	if fb := f.runner.inputs["p3-prepare"][1]["/in/feedback.md"]; !strings.Contains(fb, "UC-001-AC-09") {
		t.Errorf("2回目の P3 に失敗の内容が渡されていない: %q", fb)
	}
}

// 人が手でステータスを書き換えていたら警告する（止めない）。
func TestRun_ManualEditWarning(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	mustRun(t, f) // gateflow が ready-to-approve を書いた
	tk, _ := ticket.Load(f.root, "T-002")
	os.WriteFile(tk.Path, []byte(strings.Replace(tk.Body, "ステータス: ready-to-approve", "ステータス: ready-to-prepare", 1)), 0o644)
	f.reload(t)
	res := mustRun(t, f)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "手で") {
		t.Errorf("警告がない: %+v", res)
	}
}

// 関門のコマンド：ready・cancel・hold／unhold。gateflow が書いたことを記録に残す。
func TestHuman(t *testing.T) {
	body := strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: draft", 1)
	f := newFlow(t, body, nil)
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "approve", f.e.Now(), engine.Checks{}); err == nil {
		t.Error("draft で approve できた")
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "ready", f.e.Now(), engine.Checks{}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if f.status(t) != ticket.ReadyToPrepare {
		t.Errorf("status = %s", f.status(t))
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "hold", f.e.Now(), engine.Checks{}); err != nil || !f.e.Ticket.Held() {
		t.Errorf("hold: %v, held %v", err, f.e.Ticket.Held())
	}
	res := mustRun(t, f)
	if res.Outcome != engine.NothingToDo || !strings.Contains(res.Message, "保留") {
		t.Errorf("保留のチケットを進めた: %+v", res)
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "unhold", f.e.Now(), engine.Checks{}); err != nil || f.e.Ticket.Held() {
		t.Errorf("unhold: %v", err)
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "cancel", f.e.Now(), engine.Checks{}); err != nil || f.status(t) != ticket.Cancelled {
		t.Errorf("cancel: %v, status %s", err, f.status(t))
	}
	data, _ := os.ReadFile(filepath.Join(f.root, ".docs/_stash/tickets/T-002", engine.EventsFile))
	var n int
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(l), &ev) == nil {
			n++
		}
	}
	if n != 4 {
		t.Errorf("関門の記録 = %d件, want 4:\n%s", n, data)
	}
}

// fakeEgress は、追跡で判定を1行書いて終わる。followErr があれば、追跡は何も書かずに失敗する。
type fakeEgress struct {
	started, stopped bool
	followErr        error
}

func (e *fakeEgress) Follow(_ context.Context, w io.Writer) error {
	if e.followErr != nil {
		return e.followErr
	}
	_, err := io.WriteString(w, "{\"decision\":\"deny\",\"target\":\"followed.example:443\"}\n")
	return err
}

func (e *fakeEgress) Start(context.Context, []string) (string, error) {
	e.started = true
	return "http://gateflow-proxy:3128", nil
}
func (e *fakeEgress) Stop(context.Context) ([]byte, error) {
	e.stopped = true
	return []byte("{\"decision\":\"deny\"}\n"), nil
}
func (e *fakeEgress) Network() string { return "gateflow-sample-internal" }

// done：作業ブランチが main に含まれているときだけ、ready-for-review から done にする（M2.5）。
func TestHuman_Done(t *testing.T) {
	merged := engine.Checks{BranchMerged: func(branch string) error {
		if branch != "feature/t-002-log" {
			t.Errorf("確かめたブランチ = %s", branch)
		}
		return nil
	}, MainIDsClean: func() error { return nil }}
	notMerged := engine.Checks{BranchMerged: func(string) error { return errors.New("main に含まれていない") }, MainIDsClean: func() error { return nil }}

	review := strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: ready-for-review", 1)
	f := newFlow(t, review, nil)
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), notMerged); err == nil || !strings.Contains(err.Error(), "main に含まれていない") {
		t.Errorf("未マージで done できた: %v", err)
	}
	if f.status(t) != ticket.ReadyForReview {
		t.Errorf("不合格なのにステータスが変わった: %s", f.status(t))
	}
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), merged); err != nil || f.status(t) != ticket.Done {
		t.Fatalf("done: %v, status %s", err, f.status(t))
	}
	if ev, ok := engine.LastEvent(f.e.Ticket); !ok || ev.Command != "done" || ev.By != "human" || ev.From != "ready-for-review" || ev.To != "done" {
		t.Errorf("記録 = %+v", ev)
	}
	if res := f.e.Describe(); res.Outcome != engine.Finished || res.Next != "" {
		t.Errorf("done の後 = %+v", res)
	}

	for _, status := range []string{"draft", "ready-to-approve", "implementing"} {
		f := newFlow(t, strings.Replace(flowTicket, "ステータス: ready-to-prepare", "ステータス: "+status, 1), nil)
		if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), merged); err == nil {
			t.Errorf("%s から done できた", status)
		}
	}
}

// testedTicket は、§4.3 にテストケースが1件ある承認済みの指示書（p5-test と red を通る。M3）。
func testedTicket(t *testing.T) string {
	t.Helper()
	return strings.Replace(approvedTicket(t), "| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |\n| --- | --- | --- | --- |\n",
		"| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |\n| --- | --- | --- | --- |\n| TestLog | シナリオ | UC-001-AC-02 | 項目を確かめる |\n", 1)
}

// §4.3 があれば、p5-test → red（テストが失敗する）→ p5-implement の順に進み、テストのコミットを分ける（M3）。
func TestRun_P5TestRedImplement(t *testing.T) {
	f := newFlow(t, testedTicket(t), map[string][]string{"p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved}})
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || f.runner.calls["p5-test"] != 1 || f.runner.reds != 1 {
		t.Fatalf("res = %+v, p5-test %d, red %d", res, f.runner.calls["p5-test"], f.runner.reds)
	}
	if len(f.ws.commits) != 2 || !strings.HasPrefix(f.ws.commits[0], "test(T-002): ") {
		t.Errorf("コミット = %q", f.ws.commits)
	}
	if s := readState(t, res.RunDir); len(s.TestCommits) != 1 || s.TestCommits[0] != "c1" {
		t.Errorf("TestCommits = %v", s.TestCommits)
	}
}

// 新しいテストが実装の前に通ってしまったら（green）、テストの出力を付けて p5-test へ戻す（M3）。
func TestRun_GreenLoopsToP5Test(t *testing.T) {
	f := newFlow(t, testedTicket(t), map[string][]string{"p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved}})
	f.runner.greens = 1
	mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || f.runner.calls["p5-test"] != 2 || f.runner.reds != 2 {
		t.Fatalf("p5-test %d, red %d, status %s", f.runner.calls["p5-test"], f.runner.reds, f.status(t))
	}
	if fb := f.runner.inputs["p5-test"][1]["/in/feedback.md"]; !strings.Contains(fb, "新しいテストが失敗していない") || !strings.Contains(fb, "PASS") {
		t.Errorf("2回目の p5-test に差し戻しの内容が渡されていない: %q", fb)
	}
}

// p5-implement が p5-test のテストのファイルを変えた（消した）ら、検証の前に人の確認待ちで止める（M3 の完了の基準）。
func TestRun_TestModifiedStops(t *testing.T) {
	f := newFlow(t, testedTicket(t), map[string][]string{"p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved}})
	f.ws.testsChanged = []string{"backend/tests/scenario/log_test.go"}
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "test-modified" || !strings.Contains(res.Message, "log_test.go") {
		t.Fatalf("res = %+v", res)
	}
	if f.runner.verifyIdx != 0 || len(f.ws.imports) != 0 {
		t.Errorf("止めた後に検証 %d 回・取り込み %v", f.runner.verifyIdx, f.ws.imports)
	}
	if s := readState(t, res.RunDir); s.Resume != "p5-implement" {
		t.Errorf("resume = %q", s.Resume)
	}
}

// p5-test がテストを1つも書かなかったら（コミットする変更がない）、p5-test へ戻す（M3）。
func TestRun_P5TestWithoutChange(t *testing.T) {
	f := newFlow(t, testedTicket(t), map[string][]string{"p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved}})
	f.ws.unchangedTests = true
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || f.runner.calls["p5-test"] < 2 || f.runner.reds != 0 {
		t.Fatalf("res = %+v, p5-test %d, red %d", res, f.runner.calls["p5-test"], f.runner.reds)
	}
}

// §4.3 が0件なら、p5-test と red を飛ばす（M3）。
func TestRun_SkipP5TestWithoutTestCases(t *testing.T) {
	f := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vApproved}})
	mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || f.runner.calls["p5-test"] != 0 || f.runner.reds != 0 {
		t.Fatalf("p5-test %d, red %d, status %s", f.runner.calls["p5-test"], f.runner.reds, f.status(t))
	}
}

type fakeServices struct{ ups int }

func (s *fakeServices) Up(context.Context) (string, error) {
	s.ups++
	return "gateflow-sample-services", nil
}

// 共有のサービスがあれば run の始めに起動し、検証のコンテナ（red を含む）をサービスの専用のネットワークに置く。
// なければネットワークなし（M3 の Q-10）。
func TestRun_ServicesNetworkForVerify(t *testing.T) {
	f := newFlow(t, testedTicket(t), map[string][]string{"p5-test": {p5TestDone}, "p5-implement": {p5Done}, "p5-verify": {vApproved}})
	sv := &fakeServices{}
	f.e.Services = sv
	var redNet string
	f.runner.onSpec = func(spec container.Spec) {
		if strings.HasSuffix(spec.Name, "-red-test") {
			redNet = spec.Network
		}
	}
	mustRun(t, f)
	if sv.ups != 1 || redNet != "gateflow-sample-services" || len(f.runner.verifyNets) == 0 {
		t.Fatalf("起動 %d 回、red のネットワーク %q、検証 %v", sv.ups, redNet, f.runner.verifyNets)
	}
	for _, n := range f.runner.verifyNets {
		if n != "gateflow-sample-services" {
			t.Errorf("検証のコンテナのネットワーク = %q", n)
		}
	}
	g := newFlow(t, approvedTicket(t), map[string][]string{"p5-implement": {p5Done}, "p5-verify": {vApproved}})
	mustRun(t, g)
	if g.runner.verifyNets[0] != "none" {
		t.Errorf("サービスがないのに、検証のコンテナのネットワーク = %q", g.runner.verifyNets[0])
	}
}
