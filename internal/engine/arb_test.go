package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/engine"
	"gateflow/internal/ticket"
)

// P2 の差し戻し：MUST（UC-001 が根拠）と Q（出力先）と、Q と同じ内容の BJ の質問。IMO は裁定の対象にしない。
const p2Fix = `{"outcome": "needs_fix", "summary": "穴がある",
  "findings": [
    {"label": "MUST", "finding": "UC-001 の要求とログの項目が対応していない", "evidence": {"id": "UC-001"}, "suggestion": "§2 に項目を書く"},
    {"label": "Q", "finding": "出力先は標準出力でよいか", "evidence": {"file": "backend/log.go", "line": 1}},
    {"label": "IMO", "finding": "名前", "evidence": {"file": "backend/log.go"}}],
  "questions": [{"text": "出力先は標準出力でよいか"}]}`

// ARB：MUST は UC-001 を根拠に裁定し、Q と BJ の質問は束ねて人へ回す。
const arbEscalated = `{"outcome": "escalated", "summary": "1件を裁定、1件を人へ",
  "decisions": [
    {"findings": [0], "decider": "arb", "answer": "§2 に UC-001 のログの項目を書く", "basis": {"kind": "catalog", "ref": "UC-001"}},
    {"findings": [1], "questions": [0], "decider": "human", "criterion": "scope", "question": "出力先をどうするか。1. 標準出力 2. ファイル。推奨 1"}],
  "section2": "- UC-001 のログの項目を出す（Q-01）"}`

// ARB：すべて裁定する。
const arbResolved = `{"outcome": "resolved", "summary": "すべて裁定",
  "decisions": [
    {"findings": [0], "decider": "arb", "answer": "§2 に UC-001 のログの項目を書く", "basis": {"kind": "catalog", "ref": "UC-001"}, "edits": ["UC-001"]},
    {"findings": [1], "questions": [0], "decider": "arb", "answer": "標準出力に出す", "basis": {"kind": "standard", "ref": "十二要素のアプリのログ"}}]}`

func questionsText(t *testing.T, root string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(root, ".docs/_stash/tickets/T-002", ticket.QuestionsFile))
	return string(data)
}

// escalated：裁定したものは決定者 ARB・確定、人へ回すものだけ未回答で残し、§2 を直してコミットし、draft で止まる。
func TestARB_Escalated(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}, "p2-arbitrate": {arbEscalated}})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.Draft {
		log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
		t.Fatalf("res = %+v, status %s\n%s", res, f.status(t), log)
	}
	q := questionsText(t, f.root)
	states := ticket.QuestionStates([]byte(q))
	if states["Q-01"].Decider != "ARB" || states["Q-01"].State != "確定" || states["Q-02"].Decider != "人" || states["Q-02"].State != "未回答" || len(states) != 2 {
		t.Errorf("質疑の状態 = %+v\n%s", states, q)
	}
	for _, want := range []string{"カタログ：UC-001", "人へ回す（基準：範囲）", "（BJ の質問）出力先は標準出力でよいか", "| 決定者 | 根拠 |"} {
		if !strings.Contains(q, want) {
			t.Errorf("質疑に %q がない:\n%s", want, q)
		}
	}
	body, _ := os.ReadFile(f.e.Ticket.Path)
	if !strings.Contains(string(body), "- UC-001 のログの項目を出す（Q-01）") || strings.Contains(string(body), "UC-001 のヘルスチェックの要求のログに") {
		t.Errorf("§2 を置き換えていない:\n%s", body)
	}
	if len(f.ws.commits) != 1 || f.ws.commits[0] != "docs(T-002): ARB の裁定を反映（Q-01）" || len(f.ws.imports) != 1 {
		t.Errorf("コミット = %v、取り込み = %v", f.ws.commits, f.ws.imports)
	}
	if f.ws.resets == 0 {
		t.Error("ARB の前に作業ツリーを戻していない")
	}
}

// resolved：すべて裁定したら、draft に戻さず P2 をもう一度回し、通れば P3 まで進む。
func TestARB_ResolvedLoopsToP2(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix, p2Approved}, "p2-arbitrate": {arbResolved}, "p3-prepare": {p3Done}})
	f.ws.workChanges = map[string]string{".docs/design/uc/UC-001.md": "# UC-001\\n\\n| ID |\\n|---|\\n| UC-001-AC-01 |\\n| UC-001-AC-02 |\\n\\nUC-001 のログの項目\\n"}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.ReadyToApprove || f.runner.calls["p2-review"] != 2 {
		t.Fatalf("res = %+v, status %s, P2 の呼び出し %d", res, f.status(t), f.runner.calls["p2-review"])
	}
	if n, _ := f.e.Ticket.UnresolvedQuestions(); n != 0 {
		t.Errorf("未確定の質疑 = %d", n)
	}
	if q := questionsText(t, f.root); !strings.Contains(q, "規格（記憶による）：十二要素のアプリのログ") {
		t.Errorf("規格の印がない:\n%s", q)
	}
	// 承認の前の監査：ARB の裁定の一覧と、カタログの差分（カタログの外の差分は出さない）
	report, err := os.ReadFile(filepath.Join(res.RunDir, "arb-report.md"))
	if err != nil || !strings.Contains(string(report), "## 裁定（2 件）") || !strings.Contains(string(report), "| Q-02 |") ||
		!strings.Contains(string(report), "+UC-001 のログの項目") || strings.Contains(string(report), "backend/log.go") {
		t.Errorf("arb-report.md（%v）:\n%s", err, report)
	}
	if !strings.Contains(res.Next, "arb-report.md") {
		t.Errorf("次の操作 = %q", res.Next)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"counter":"arbitrate"`) || !strings.Contains(string(log), `"changed":[".docs/design/uc/UC-001.md"]`) {
		t.Errorf("記録:\n%s", log)
	}
}

// 前の回に ARB が裁定したのに残った指摘は、ARB に裁定させず人へ回す（裁定したら出力の不正）。
func TestARB_ForcedHumanWhenFindingRemains(t *testing.T) {
	again := strings.Replace(arbResolved, `"outcome": "resolved"`, `"outcome": "resolved"`, 1)
	forcedHuman := `{"outcome": "escalated", "summary": "残った指摘を人へ",
  "decisions": [
    {"findings": [0], "decider": "human", "criterion": "ambiguous", "question": "裁定が BJ に通らなかった。どうするか"},
    {"findings": [1], "questions": [0], "decider": "human", "criterion": "ambiguous", "question": "裁定が BJ に通らなかった。どうするか"}]}`
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}, "p2-arbitrate": {arbResolved, again, forcedHuman}})
	f.ws.workChanges = map[string]string{".docs/design/uc/UC-001.md": "# UC-001\\n\\n| ID |\\n|---|\\n| UC-001-AC-01 |\\n| UC-001-AC-02 |\\n\\nUC-001 のログの項目\\n"}
	f.ws.workChangesOnce = true
	f.e.Project.Limits = &config.Limits{ArbitrateLoops: 3}
	res := mustRun(t, f)
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if res.Outcome != engine.Gate || f.status(t) != ticket.Draft {
		t.Fatalf("res = %+v\n%s", res, log)
	}
	if !strings.Contains(string(log), "前の回に ARB が裁定した指摘が残ったもの") {
		t.Errorf("残った指摘を裁定したのに出力の不正にならなかった:\n%s", log)
	}
}

// ARB の往復が上限に達したら、ARB に裁定させず、BJ の指摘をそのまま人へ回す。
func TestARB_LimitEscalatesToHuman(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}, "p2-arbitrate": {arbResolved}})
	f.ws.workChanges = map[string]string{".docs/design/uc/UC-001.md": "# UC-001\\n\\n| ID |\\n|---|\\n| UC-001-AC-01 |\\n| UC-001-AC-02 |\\n\\nUC-001 のログの項目\\n"}
	f.e.Project.Limits = &config.Limits{ArbitrateLoops: 1}
	f.e.Project.Contracts = nil
	// 2回目の P2 の指摘は、1回目と本文を変えて進展なしに当てない
	f.runner.results["p2-review"] = []string{p2Fix, strings.Replace(p2Fix, "対応していない", "まだ対応していない", 1)}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.Draft || !strings.Contains(res.Message, "上限（1）") || f.runner.calls["p2-arbitrate"] != 1 {
		t.Fatalf("res = %+v, ARB の呼び出し %d", res, f.runner.calls["p2-arbitrate"])
	}
	// ARB を通らないので、BJ の指摘（MUST・Q）と質問がそのまま質疑になる（束ねは ARB が行う）
	if n, _ := f.e.Ticket.UnresolvedQuestions(); n != 3 {
		t.Errorf("人へ回した質疑 = %d, want 3", n)
	}
}

// 契約の ID に触れる裁定は、ARB の判断によらず gateflow が人へ回す（Q-05。機械による二重の止め）。
// 人へ回した裁定がカタログを直していたら、ARB のカタログの変更を戻し、§2 の書き換えも反映しない。
func TestARB_ContractGuardEscalates(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}, "p2-arbitrate": {arbResolved}})
	f.ws.workChanges = map[string]string{".docs/design/uc/UC-001.md": "# UC-001\n\n| ID |\n|---|\n| UC-001-AC-01 |\n| UC-001-AC-02 |\n\nUC-001 のログの項目\n"}
	f.e.Project.Contracts = []string{"UC-*"}
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.Draft || f.runner.calls["p2-arbitrate"] != 1 {
		t.Fatalf("res = %+v, ARB の呼び出し %d", res, f.runner.calls["p2-arbitrate"])
	}
	q := questionsText(t, f.root)
	states := ticket.QuestionStates([]byte(q))
	if states["Q-01"].Decider != "ARB" || states["Q-02"].Decider != "人" || states["Q-02"].State != "未回答" {
		t.Errorf("状態 = %+v\n%s", states, q)
	}
	if !strings.Contains(q, "契約の ID（UC-001）に触れるので、gateflow が人へ回した") || !strings.Contains(q, "人へ回す（基準：外から見える契約）") {
		t.Errorf("質疑:\n%s", q)
	}
	log, _ := os.ReadFile(filepath.Join(res.RunDir, "log.jsonl"))
	if !strings.Contains(string(log), `"event":"arb-contract"`) || !strings.Contains(string(log), `"catalogReset":true`) || f.ws.resets < 2 {
		t.Errorf("守りの記録・カタログの戻し（resets %d）:\n%s", f.ws.resets, log)
	}
}

// ARB が直してよいのはカタログだけ。変えた行の ID は edits に挙げたものだけ。
func TestARB_ChangesOutsideCatalogAreRejected(t *testing.T) {
	for name, changes := range map[string]map[string]string{
		"カタログの外":       {"backend/log.go": "package log\n\nfunc B() {}\n"},
		"edits にない ID": {".docs/design/buc.md": "| BUC | UC |\n|---|---|\n| BUC-S01 | UC-001（システムの死活を確認する） |\n| BUC-S02 | UC-002 |\n"},
	} {
		f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Fix}, "p2-arbitrate": {arbResolved}})
		f.ws.workChanges = changes
		res := mustRun(t, f)
		if res.Outcome != engine.Stopped || res.Reason != "invalid-output" {
			t.Errorf("%s: res = %+v", name, res)
		}
	}
}

// P2 の段階（ready-to-prepare）では、作業ブランチに自分のコミットがあっても main をマージする。P3 の後はしない。
func TestRun_MergeMainOnlyInP2Stage(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Done}})
	mustRun(t, f)
	if len(f.ws.prepareOpts) != 1 || !f.ws.prepareOpts[0].MergeMain || f.ws.prepareOpts[0].Author.Name == "" {
		t.Errorf("P2 の段階 = %+v", f.ws.prepareOpts)
	}
	g := newFlow(t, approvedTicket(t), map[string][]string{"p5": {p5Done}, "p5-verify": {vApproved}})
	mustRun(t, g)
	if len(g.ws.prepareOpts) != 1 || g.ws.prepareOpts[0].MergeMain {
		t.Errorf("P5 の段階 = %+v", g.ws.prepareOpts)
	}
}

// 試験：過去の P2 の差し戻しの記録（その時点の写し）を入力に、本番と同じ検査で ARB を1回動かす。コミットも質疑の書き込みもしない。
func TestARBTrial(t *testing.T) {
	f := newFlow(t, flowTicket, map[string][]string{"p2-arbitrate": {arbResolved}})
	src := filepath.Join(t.TempDir(), "runs/20261011-210543/steps/01-p2-review")
	writeTree(t, src, map[string]string{
		"out/result.json":                p2Fix,
		"in/ticket.md":                   flowTicket,
		"seen/.docs/design/buc.md":       "| BUC | UC |\n|---|---|\n| BUC-S01 | UC-001（システムの死活を確認する） |\n",
		"seen/.docs/design/uc/UC-001.md": "# UC-001\n",
	})
	dest := filepath.Join(t.TempDir(), "trial")
	res, err := f.e.ARBTrial(t.Context(), src, dest)
	if err != nil || !res.OK || res.Attempts != 1 || res.Result == nil || res.Result.Outcome != "resolved" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	in := f.runner.inputs["p2-arbitrate"][0]
	if !strings.Contains(in["/in/review.json"], "UC-001 の要求とログの項目") || in["/in/ticket.md"] == "" {
		t.Errorf("入力 = %v", in)
	}
	for _, p := range []string{"latest", "20261012-090000/trial.json", "20261012-090000/prompt.md", "20261012-090000/work-1/.docs/design/buc.md"} {
		if _, err := os.Stat(filepath.Join(dest, p)); err != nil {
			t.Errorf("%s がない", p)
		}
	}
	if len(f.ws.commits) != 0 {
		t.Errorf("試験でコミットした: %v", f.ws.commits)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".docs/_stash/tickets/T-002", "questions.md")); err == nil {
		t.Error("試験で質疑を書いた")
	}
}
