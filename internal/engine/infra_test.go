package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gateflow/internal/engine"
	"gateflow/internal/ticket"
)

// 種別が基盤のチケット（M2.7）：受入条件とテストケースは0件で、ホストでの確かめ（§4.5）を持つ。
var infraTicket = strings.Replace(flowTicket, "| 種別 | 実装 |", "| 種別 | 基盤 |", 1)

const p3Infra = `{"outcome": "done", "summary": "準備した", "current": "compose はない", "plan": "compose と Dockerfile を足す",
  "hostCheck": {"review": ["docker-compose.yaml と Dockerfile：ホストのフォルダを繋ぐ設定、特権での実行、公開するポート"],
    "steps": [{"action": "docker compose up -d", "pass": "GET /health が 200 を返す"}]}}`

// 基盤のチケットは、受入条件なしで P3 を通り、§4.5 にホストでの確かめが書かれる。
func TestRun_InfraP3(t *testing.T) {
	f := newFlow(t, infraTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Infra}})
	res := mustRun(t, f)
	if res.Outcome != engine.Gate || f.status(t) != ticket.ReadyToApprove {
		t.Fatalf("res = %+v, status %s", res, f.status(t))
	}
	f.reload(t)
	for _, w := range []string{"### §4.5 ホストでの確かめ", "**ホストで動かす前のコードレビュー**", "- docker-compose.yaml と Dockerfile", "| 1 | docker compose up -d | GET /health が 200 を返す |", "### §4.6 承認"} {
		if !strings.Contains(f.e.Ticket.Body, w) {
			t.Errorf("指示書に %q がない:\n%s", w, f.e.Ticket.Body)
		}
	}
	if !f.e.Ticket.HasHostCheck() || len(f.e.Ticket.MissingSections()) != 0 {
		t.Errorf("ホストでの確かめ %v、埋まっていない節 %v", f.e.Ticket.HasHostCheck(), f.e.Ticket.MissingSections())
	}
}

// 基盤のチケットでホストでの確かめがない出力は、1回だけ再実行し、それでもなければ人の確認待ち。
func TestRun_InfraP3WithoutHostCheck(t *testing.T) {
	noCheck := `{"outcome": "done", "summary": "s", "current": "c", "plan": "p"}`
	f := newFlow(t, infraTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {noCheck, noCheck}})
	res := mustRun(t, f)
	if f.status(t) == ticket.ReadyToApprove || f.runner.calls["p3-prepare"] != 2 {
		t.Fatalf("res = %+v, status %s, P3 の呼び出し %d", res, f.status(t), f.runner.calls["p3-prepare"])
	}
}

// §4.3 にテストケースがなければ、P5 はテスト0件で done を出せる。
func TestRun_P5WithoutTestCases(t *testing.T) {
	approved := strings.ReplaceAll(strings.Replace(infraTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5-implement": {`{"outcome": "done", "summary": "compose と Dockerfile を足した", "verify": [{"name": "build", "exitCode": 0}, {"name": "test", "exitCode": 0}, {"name": "lint", "exitCode": 0}]}`}, "p5-verify": {vApproved}})
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || f.runner.calls["p5-implement"] != 1 {
		t.Fatalf("res = %+v, status %s, P5 の呼び出し %d", res, f.status(t), f.runner.calls["p5-implement"])
	}
}

// §4.5 にホストでの確かめがあるチケットは、§5 に結果を書くまで done にできない（M2.7 の Q-03）。
func TestHuman_DoneNeedsHostCheckResult(t *testing.T) {
	merged := engine.Checks{BranchMerged: func(string) error { return nil }, MainIDsClean: func() error { return nil }}
	body := strings.Replace(infraTicket, "ステータス: ready-to-prepare", "ステータス: ready-for-review", 1)
	body = strings.Replace(body, "### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）\n\n- \n",
		"### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）\n\n**ホストで動かす前のコードレビュー**\n\n- compose を読む\n", 1)
	body += "\n## §5 ホストでの確かめの結果（ready-for-review の後に書く）\n\n§4.5 があるときだけ書く。§4.5 があるのに空なら、`gateflow done`できない。\n\n- \n"
	f := newFlow(t, body, nil)
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), merged); err == nil || !strings.Contains(err.Error(), "§5") {
		t.Fatalf("§5 が空なのに done できた: %v", err)
	}
	f = newFlow(t, strings.Replace(body, "できない。\n\n- \n", "できない。\n\n- 2026-10-13：GET /health が 200 を返した\n", 1), nil)
	if err := engine.Human(f.e.Ticket, f.e.Workflow, "done", f.e.Now(), merged); err != nil || f.status(t) != ticket.Done {
		t.Fatalf("done: %v, status %s", err, f.status(t))
	}
}

// P2 の IMO は P3 に渡り、P3 の対応が §4.1 の末尾の表に書かれる。対応がなければ出力の不正。
func TestRun_P2IMOToP3(t *testing.T) {
	p2IMO := `{"outcome": "approved", "summary": "止める指摘なし",
	  "findings": [{"label": "IMO", "finding": "COPY は名指しにする", "evidence": {"id": "UC-001"}}]}`
	p3 := strings.Replace(p3Infra, `"plan": "compose と Dockerfile を足す",`, `"plan": "compose と Dockerfile を足す", "imo": [{"index": 0, "decision": "planned", "detail": "§4.1 の Dockerfile の方針"}],`, 1)
	f := newFlow(t, infraTicket, map[string][]string{"p2-review": {p2IMO}, "p3-prepare": {p3Infra, p3}})
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyToApprove || f.runner.calls["p3-prepare"] != 2 {
		t.Fatalf("res = %+v, status %s, P3 の呼び出し %d（1回目は IMO への対応がなく不正）", res, f.status(t), f.runner.calls["p3-prepare"])
	}
	if in := f.runner.inputs["p3-prepare"][0]["/in/p2-imo.json"]; !strings.Contains(in, "COPY は名指しにする") {
		t.Errorf("P3 に IMO が渡っていない: %q", in)
	}
	f.reload(t)
	if !strings.Contains(f.e.Ticket.Body, "| COPY は名指しにする | 計画に入れた | §4.1 の Dockerfile の方針 |") {
		t.Errorf("§4.1 に IMO への対応がない:\n%s", f.e.Ticket.Body)
	}
}

// P5 後の BJ が人の確認待ち（Q）で止めたとき：P5 のコミットは作業ブランチへ取り込まない。resume は、止まった実行で
// gateflow がコミットした記録を clone の検査に渡し、P5 後の BJ からやり直す。取り込むのは承認の後だけ（M2.7）。
func TestResume_AfterP5VerifyNeedsHuman(t *testing.T) {
	vQ := `{"outcome": "needs_human", "summary": "人の判断が要る", "findings": [{"label": "Q", "finding": "手順の文言", "evidence": {"file": "backend/log.go", "line": 1}}]}`
	approved := strings.ReplaceAll(strings.Replace(infraTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5-implement": {`{"outcome": "done", "summary": "s", "verify": [{"name": "build", "exitCode": 0}, {"name": "test", "exitCode": 0}, {"name": "lint", "exitCode": 0}]}`}, "p5-verify": {vQ, vApproved}})
	res := mustRun(t, f)
	if res.Outcome != engine.Stopped || res.Reason != "needs_human" {
		t.Fatalf("res = %+v", res)
	}
	if len(f.ws.imports) != 0 {
		t.Fatalf("BJ の承認の前に作業ブランチへ取り込んだ: %v", f.ws.imports)
	}
	f.reload(t)
	res, err := f.e.Resume(context.Background())
	if err != nil || f.status(t) != ticket.ReadyForReview {
		t.Fatalf("resume: %v, %+v, status %s", err, res, f.status(t))
	}
	last := f.ws.prepareOpts[len(f.ws.prepareOpts)-1]
	if len(last.Own) != 1 || last.Own[0] != "c1" {
		t.Errorf("resume の clone の検査に渡した記録 = %v, want [c1]", last.Own)
	}
	if len(f.ws.imports) != 1 || f.runner.calls["p5-implement"] != 1 {
		t.Errorf("取り込み %v、P5 の呼び出し %d（承認の後に1回だけ取り込み、P5 はやり直さない）", f.ws.imports, f.runner.calls["p5-implement"])
	}
}

// clone の用意で失敗した実行も、同じ step から再開できる（step を始める前の失敗）。
func TestResume_AfterPrepareError(t *testing.T) {
	vQ := `{"outcome": "needs_human", "summary": "s", "findings": [{"label": "Q", "finding": "q", "evidence": {"file": "backend/log.go", "line": 1}}]}`
	approved := strings.ReplaceAll(strings.Replace(infraTicket, "ステータス: ready-to-prepare", "ステータス: ready-to-implement", 1), "- [ ]", "- [x]")
	f := newFlow(t, approved, map[string][]string{"p5-implement": {`{"outcome": "done", "summary": "s", "verify": [{"name": "build", "exitCode": 0}, {"name": "test", "exitCode": 0}, {"name": "lint", "exitCode": 0}]}`}, "p5-verify": {vQ, vApproved}})
	mustRun(t, f)
	f.reload(t)
	f.ws.prepareErr = errors.New("clone を用意できない")
	if _, err := f.e.Resume(context.Background()); err == nil {
		t.Fatal("clone の用意の失敗が返らなかった")
	}
	f.ws.prepareErr = nil
	f.reload(t)
	if _, err := f.e.Resume(context.Background()); err != nil || f.status(t) != ticket.ReadyForReview {
		t.Fatalf("2回目の resume: %v, status %s", err, f.status(t))
	}
	if last := f.ws.prepareOpts[len(f.ws.prepareOpts)-1]; len(last.Own) != 1 || last.Own[0] != "c1" {
		t.Errorf("記録が引き継がれていない: %v", last.Own)
	}
}

// P3 には、ホストの OS（gateflow が動いている OS）を伝える（ホストでの確かめのコマンドのため。M2.9）。
func TestRun_P3KnowsHostOS(t *testing.T) {
	f := newFlow(t, infraTicket, map[string][]string{"p2-review": {p2Approved}, "p3-prepare": {p3Infra}})
	res := mustRun(t, f)
	prompt, _ := os.ReadFile(filepath.Join(res.RunDir, "steps", "02-p3-prepare", "prompt.md"))
	if !strings.Contains(string(prompt), "ホストの OS は") || !strings.Contains(string(prompt), "（"+runtime.GOOS+"）") {
		t.Errorf("P3 のプロンプトにホストの OS がない:\n%s", prompt)
	}
}
