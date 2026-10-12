package output_test

import (
	"strings"
	"testing"

	"gateflow/internal/output"
)

var arbReview = &output.ReviewResult{Outcome: "needs_fix", Summary: "s",
	Findings: []output.Finding{
		{Label: "MUST", Finding: "a", Evidence: &output.Evidence{ID: "UC-001"}},
		{Label: "Q", Finding: "b", Evidence: &output.Evidence{ID: "UC-001"}},
		{Label: "IMO", Finding: "c", Evidence: &output.Evidence{ID: "UC-001"}},
	},
	Questions: []output.Question{{Text: "b と同じ"}},
}

func TestParseARB_OK(t *testing.T) {
	r, err := output.ParseARB([]byte(`{"outcome": "escalated", "summary": "s", "decisions": [
	  {"findings": [0], "decider": "arb", "answer": "x", "basis": {"kind": "catalog", "ref": "UC-001"}, "edits": ["UC-001"]},
	  {"findings": [1], "questions": [0], "decider": "human", "criterion": "contract", "question": "どうするか"}]}`), arbReview, nil)
	if err != nil || !r.Human() {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
}

// MUST・Q の指摘と BJ の質問は、どれもちょうど1件の裁定に入れる。決定者ごとの項目、outcome の整合、人へ回すと決めた指摘。
func TestParseARB_Invalid(t *testing.T) {
	for name, tc := range map[string]struct {
		json   string
		forced []int
		want   string
	}{
		"指摘の漏れ": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0], "questions": [0], "decider": "arb", "answer": "x", "basis": {"kind": "catalog", "ref": "UC-001"}}]}`, nil, "指摘 1"},
		"重ねて入れた": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0, 1], "questions": [0], "decider": "arb", "answer": "x", "basis": {"kind": "catalog", "ref": "UC-001"}},
		  {"findings": [1], "decider": "arb", "answer": "x", "basis": {"kind": "catalog", "ref": "UC-001"}}]}`, nil, "（2 件）"},
		"IMO を裁定": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0, 1, 2], "questions": [0], "decider": "arb", "answer": "x", "basis": {"kind": "catalog", "ref": "UC-001"}}]}`, nil, "IMO"},
		"arb の根拠なし": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0, 1], "questions": [0], "decider": "arb", "answer": "x"}]}`, nil, "根拠"},
		"human の基準なし": {`{"outcome": "escalated", "summary": "s", "decisions": [
		  {"findings": [0, 1], "questions": [0], "decider": "human", "question": "q"}]}`, nil, "基準"},
		"outcome の食い違い": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0, 1], "questions": [0], "decider": "human", "criterion": "scope", "question": "q"}]}`, nil, "escalated"},
		"人へ回すべき指摘を裁定": {`{"outcome": "resolved", "summary": "s", "decisions": [
		  {"findings": [0, 1], "questions": [0], "decider": "arb", "answer": "x", "basis": {"kind": "standard", "ref": "RFC"}}]}`, []int{1}, "人へ回さなければならない"},
		"知らない項目": {`{"outcome": "resolved", "summary": "s", "decisions": [], "extra": 1}`, nil, "JSON"},
	} {
		_, err := output.ParseARB([]byte(tc.json), arbReview, tc.forced)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestDescribeARB(t *testing.T) {
	d := output.DescribeARB()
	for _, w := range []string{"decisions", "decider", "basis", "criterion", "edits", "section2", "escalated"} {
		if !strings.Contains(d, w) {
			t.Errorf("説明に %q がない", w)
		}
	}
}
