package engine_test

import (
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
	f := newFlow(t, approved, map[string][]string{"p5": {`{"outcome": "done", "summary": "compose と Dockerfile を足した"}`}, "p5-verify": {vApproved}})
	res := mustRun(t, f)
	if f.status(t) != ticket.ReadyForReview || f.runner.calls["p5"] != 1 {
		t.Fatalf("res = %+v, status %s, P5 の呼び出し %d", res, f.status(t), f.runner.calls["p5"])
	}
}

// §4.5 にホストでの確かめがあるチケットは、§5 に結果を書くまで done にできない（M2.7 の Q-03）。
func TestHuman_DoneNeedsHostCheckResult(t *testing.T) {
	merged := engine.Checks{BranchMerged: func(string) error { return nil }}
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
