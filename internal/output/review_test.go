package output_test

import (
	"strings"
	"testing"

	"gateflow/internal/output"
)

func TestParseP2_Approved(t *testing.T) {
	r, err := output.ParseP2([]byte(`{"outcome": "approved", "summary": "問題なし",
	  "findings": [{"label": "IMO", "finding": "名前が長い", "evidence": {"file": "a.md", "line": 3}, "suggestion": "短くする"}]}`))
	if err != nil {
		t.Fatalf("ParseP2: %v", err)
	}
	if r.Outcome != "approved" || len(r.Findings) != 1 || r.Findings[0].Evidence.Line != 3 {
		t.Errorf("r = %+v", r)
	}
}

func TestParseP2_Invalid(t *testing.T) {
	cases := []struct {
		name, data string
		want       []string
	}{
		{"MUST があるのに approved", `{"outcome": "approved", "summary": "s", "findings": [{"label": "MUST", "finding": "f", "evidence": {"id": "UC-001"}}]}`, []string{"MUST", "needs_fix"}},
		{"Q があるのに approved", `{"outcome": "approved", "summary": "s", "findings": [{"label": "Q", "finding": "f", "evidence": {"id": "UC-001"}}]}`, []string{"needs_fix"}},
		{"止める指摘がないのに needs_fix", `{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "NITS", "finding": "f", "evidence": {"file": "a"}}]}`, []string{"needs_fix"}},
		{"根拠がない", `{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "MUST", "finding": "f", "evidence": {}}]}`, []string{"findings[0].evidence"}},
		{"行だけの根拠", `{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "MUST", "finding": "f", "evidence": {"line": 3}}]}`, []string{"findings[0].evidence"}},
		{"知らないラベル", `{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "HIGH", "finding": "f", "evidence": {"file": "a"}}]}`, []string{"HIGH"}},
		{"指摘の本文がない", `{"outcome": "needs_fix", "summary": "s", "findings": [{"label": "MUST", "finding": "", "evidence": {"file": "a"}}]}`, []string{"findings[0].finding"}},
		{"P2 にない outcome", `{"outcome": "needs_human", "summary": "s"}`, []string{"needs_human"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := output.ParseP2([]byte(tc.data))
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// P5後：MUST があれば needs_fix、Q だけなら needs_human、止める指摘がなければ approved。
func TestParseP5Verify_LabelOutcome(t *testing.T) {
	f := func(label string) string {
		return `{"label": "` + label + `", "finding": "f", "evidence": {"file": "a.go", "line": 1}}`
	}
	cases := []struct {
		outcome, findings string
		ok                bool
	}{
		{"needs_fix", f("MUST") + "," + f("Q"), true},
		{"needs_human", f("MUST"), false},
		{"needs_human", f("Q"), true},
		{"needs_fix", f("Q"), false},
		{"approved", f("IMO") + "," + f("NR"), true},
		{"approved", f("Q"), false},
	}
	for _, tc := range cases {
		_, err := output.ParseP5Verify([]byte(`{"outcome": "` + tc.outcome + `", "summary": "s", "findings": [` + tc.findings + `]}`))
		if (err == nil) != tc.ok {
			t.Errorf("outcome %s / %s: err = %v, want ok=%v", tc.outcome, tc.findings, err, tc.ok)
		}
	}
}

func TestParseP3_Done(t *testing.T) {
	r, err := output.ParseP3([]byte(`{
	  "outcome": "done", "summary": "準備した",
	  "current": "ログの基盤はない", "plan": "common/log に作る",
	  "acceptance": [{"id": "UC-001-AC-02", "uc": "UC-001", "content": "ログに必須の項目がある"}],
	  "testCases": [{"test": "TestLog_Fields", "layer": "scenario", "targets": ["UC-001-AC-02"], "content": "項目を確かめる"}],
	  "sheets": [{"file": ".docs/design/uc/UC-001.md", "change": "受入条件を足す"}],
	  "unfilled": []
	}`), false, 0)
	if err != nil {
		t.Fatalf("ParseP3: %v", err)
	}
	if r.Current == "" || r.Plan == "" || r.Acceptance[0].ID != "UC-001-AC-02" || r.TestCases[0].Layer != "scenario" || r.Sheets[0].File == "" {
		t.Errorf("r = %+v", r)
	}
}

func TestParseP3_Invalid(t *testing.T) {
	cases := []struct {
		name, data string
		want       []string
	}{
		{"done なのに中身がない", `{"outcome": "done", "summary": "s"}`, []string{"current", "plan", "acceptance", "testCases"}},
		{"受入条件の ID の形", `{"outcome": "done", "summary": "s", "current": "c", "plan": "p",
		  "acceptance": [{"id": "AC-1", "uc": "UC-001", "content": "x"}],
		  "testCases": [{"test": "T", "layer": "scenario", "targets": ["AC-1"], "content": "x"}]}`, []string{"AC-1"}},
		{"受入条件の ID と UC が合わない", `{"outcome": "done", "summary": "s", "current": "c", "plan": "p",
		  "acceptance": [{"id": "UC-002-AC-01", "uc": "UC-001", "content": "x"}],
		  "testCases": [{"test": "T", "layer": "scenario", "targets": ["UC-002-AC-01"], "content": "x"}]}`, []string{"UC-002-AC-01", "UC-001"}},
		{"テストの層の誤り", `{"outcome": "done", "summary": "s", "current": "c", "plan": "p",
		  "acceptance": [{"id": "UC-001-AC-01", "uc": "UC-001", "content": "x"}],
		  "testCases": [{"test": "T", "layer": "e2e", "targets": [], "content": "x"}]}`, []string{"e2e"}},
		{"受入条件にテストがない", `{"outcome": "done", "summary": "s", "current": "c", "plan": "p",
		  "acceptance": [{"id": "UC-001-AC-01", "uc": "UC-001", "content": "x"}],
		  "testCases": [{"test": "T", "layer": "unit", "targets": [], "content": "x"}]}`, []string{"UC-001-AC-01"}},
		{"needs_answer なのに質問がない", `{"outcome": "needs_answer", "summary": "s"}`, []string{"questions"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := output.ParseP3([]byte(tc.data), false, 0)
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

// 種別が基盤：受入条件は0件、テストケースは0件でよく、ホストでの確かめが必須（M2.7）。
const p3Infra = `{"outcome": "done", "summary": "準備した", "current": "c", "plan": "p",
  "hostCheck": {"review": ["docker-compose.yaml と Dockerfile：volumes・privileged・公開するポート"],
    "steps": [{"action": "docker compose up -d", "pass": "GET /health が 200"}]}}`

func TestParseP3_Infra(t *testing.T) {
	r, err := output.ParseP3([]byte(p3Infra), true, 0)
	if err != nil {
		t.Fatalf("ParseP3: %v", err)
	}
	if r.HostCheck == nil || len(r.HostCheck.Review) != 1 || r.HostCheck.Steps[0].Pass != "GET /health が 200" {
		t.Errorf("r = %+v", r)
	}
	// 種別が実装なら、受入条件がない出力は今のとおり不正
	if _, err := output.ParseP3([]byte(p3Infra), false, 0); err == nil || !strings.Contains(err.Error(), "acceptance") {
		t.Errorf("実装で受入条件なし: %v", err)
	}
}

func TestParseP3_InfraInvalid(t *testing.T) {
	cases := []struct {
		name, data string
		want       []string
	}{
		{"受入条件がある", strings.Replace(p3Infra, `"plan": "p",`, `"plan": "p", "acceptance": [{"id": "UC-001-AC-01", "uc": "UC-001", "content": "x"}],`, 1), []string{"基盤なのに acceptance"}},
		{"ホストでの確かめがない", `{"outcome": "done", "summary": "s", "current": "c", "plan": "p"}`, []string{"hostCheck がない"}},
		{"コードレビューがない", strings.Replace(p3Infra, `["docker-compose.yaml と Dockerfile：volumes・privileged・公開するポート"]`, `[]`, 1), []string{"hostCheck.review"}},
		{"手順の合格の基準がない", strings.Replace(p3Infra, `, "pass": "GET /health が 200"`, ``, 1), []string{"hostCheck.steps[0]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := output.ParseP3([]byte(tc.data), true, 0)
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestDescribe_NewSteps(t *testing.T) {
	for name, d := range map[string]string{
		"p2": output.DescribeP2(), "p3": output.DescribeP3(), "p5-verify": output.DescribeP5Verify(),
	} {
		for _, w := range []string{"/out/result.json", "`outcome`", "`summary`"} {
			if !strings.Contains(d, w) {
				t.Errorf("%s の説明に %s がない", name, w)
			}
		}
	}
	if !strings.Contains(output.DescribeP2(), "`findings[].evidence.file`") || !strings.Contains(output.DescribeP3(), "`acceptance[].id`") {
		t.Error("入れ子の項目の説明がない")
	}
}

// P2 の IMO には、P3 が1件ずつ対応（計画に入れた・見送った）を書く。漏れ・重なり・理由なしは不正。
func TestParseP3_IMO(t *testing.T) {
	ok := strings.Replace(p3Infra, `"plan": "p",`, `"plan": "p", "imo": [{"index": 0, "decision": "planned", "detail": "§4.1 に書いた"}, {"index": 1, "decision": "deferred", "detail": "範囲外"}],`, 1)
	r, err := output.ParseP3([]byte(ok), true, 2)
	if err != nil || len(r.IMO) != 2 {
		t.Fatalf("ParseP3: %v", err)
	}
	cases := map[string]struct {
		data string
		want string
	}{
		"1件漏れ":   {strings.Replace(ok, `, {"index": 1, "decision": "deferred", "detail": "範囲外"}`, ``, 1), "IMO 1"},
		"範囲の外":   {strings.Replace(ok, `"index": 1`, `"index": 2`, 1), "範囲"},
		"理由がない":  {strings.Replace(ok, `"detail": "範囲外"`, `"detail": ""`, 1), "imo[1].detail"},
		"対応の選択肢": {strings.Replace(ok, `"deferred"`, `"later"`, 1), "later"},
	}
	for name, tc := range cases {
		if _, err := output.ParseP3([]byte(tc.data), true, 2); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// IMO がなければ、imo は書かなくてよい
	if _, err := output.ParseP3([]byte(p3Infra), true, 0); err != nil {
		t.Errorf("IMO なし: %v", err)
	}
}
