package output_test

import (
	"strings"
	"testing"

	"gateflow/internal/output"
)

func TestParseP5_Done(t *testing.T) {
	data := `{
	  "outcome": "done",
	  "summary": "ヘルスチェックを実装した",
	  "questions": [],
	  "notes": ["補足"],
	  "tests": [{"test": "TestHealth_OK", "file": "backend/tests/unit/health_test.go", "acceptance": ["UC-001-AC-01"]}],
	  "changedSheets": [".docs/design/uc/UC-001.md"]
	}`
	r, err := output.ParseP5([]byte(data))
	if err != nil {
		t.Fatalf("ParseP5: %v", err)
	}
	if r.Outcome != output.OutcomeDone || len(r.Tests) != 1 || r.Tests[0].Acceptance[0] != "UC-001-AC-01" {
		t.Errorf("result = %+v", r)
	}
}

func TestParseP5_Blocked(t *testing.T) {
	data := `{"outcome": "blocked", "summary": "依存の追加が必要", "blockReason": "dependency"}`
	r, err := output.ParseP5([]byte(data))
	if err != nil {
		t.Fatalf("ParseP5: %v", err)
	}
	if r.Outcome != output.OutcomeBlocked || r.BlockReason != output.BlockDependency {
		t.Errorf("result = %+v", r)
	}
}

func TestParseP5_Invalid(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []string
	}{
		{"JSON でない", `not json`, []string{"JSON"}},
		{"知らない項目", `{"outcome": "done", "summary": "s", "tests": [{"test": "T", "file": "f", "acceptance": ["A"]}], "extra": 1}`, []string{"extra"}},
		{"outcome がない・summary がない", `{}`, []string{"outcome", "summary"}},
		{"outcome が選択肢にない", `{"outcome": "approved", "summary": "s"}`, []string{"approved"}},
		{"done なのにテストがない", `{"outcome": "done", "summary": "s"}`, []string{"tests"}},
		{"テストの項目が欠けている", `{"outcome": "done", "summary": "s", "tests": [{"test": ""}]}`, []string{"tests[0].test", "tests[0].file"}},
		{"blocked なのに理由がない", `{"outcome": "blocked", "summary": "s"}`, []string{"blockReason"}},
		{"blockReason が選択肢にない", `{"outcome": "blocked", "summary": "s", "blockReason": "tired"}`, []string{"tired"}},
		{"done なのに blockReason がある", `{"outcome": "done", "summary": "s", "tests": [{"test": "T", "file": "f"}], "blockReason": "scope"}`, []string{"blockReason"}},
		{"質問の本文がない", `{"outcome": "test_defect", "summary": "s", "questions": [{"text": ""}]}`, []string{"questions[0].text"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := output.ParseP5([]byte(tc.data))
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

// 出力形式の説明は構造体から作る。項目の名前、選択肢、説明がすべて入る。
func TestDescribeP5(t *testing.T) {
	d := output.DescribeP5()
	for _, w := range []string{
		"/out/result.json",
		"`outcome`", "`done`", "`test_defect`", "`blocked`",
		"`summary`", "`questions`", "`notes`",
		"`tests`", "`tests[].test`", "`tests[].file`", "`tests[].acceptance`",
		"`changedSheets`", "`blockReason`", "`dependency`", "`scope`",
	} {
		if !strings.Contains(d, w) {
			t.Errorf("説明に %s がない:\n%s", w, d)
		}
	}
}
