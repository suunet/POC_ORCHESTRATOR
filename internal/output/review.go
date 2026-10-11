package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// レビューのラベル（設計の「レビューのラベル」）。MUST と Q は遷移を止める。
var labels = []string{"MUST", "Q", "IMO", "NR", "NITS", "FYI"}

// Evidence は、指摘の根拠。ファイル（と行）か ID のどれかが要る。行はファイルと組で使う。
type Evidence struct {
	File string `json:"file,omitempty" desc:"根拠のファイル（リポジトリのルートからの相対パス）"`
	Line int    `json:"line,omitempty" desc:"根拠の行（file と組で使う）"`
	ID   string `json:"id,omitempty" desc:"根拠のカタログの ID"`
}

// Finding は、BJ の指摘の1件。
type Finding struct {
	Label      string    `json:"label" desc:"ラベル。MUST と Q は遷移を止める" enum:"MUST,Q,IMO,NR,NITS,FYI"`
	Finding    string    `json:"finding" desc:"指摘の本文"`
	Evidence   *Evidence `json:"evidence" desc:"根拠（必須。ファイル・行・ID のいずれか）"`
	Suggestion string    `json:"suggestion,omitempty" desc:"提案（修正はしない）"`
}

// ReviewResult は、BJ の step（p2-review・p5-verify）の出力。
type ReviewResult struct {
	Outcome   string     `json:"outcome" desc:"結果"`
	Summary   string     `json:"summary" desc:"レビューの要約"`
	Questions []Question `json:"questions,omitempty" desc:"人に確かめたいこと"`
	Notes     []string   `json:"notes,omitempty" desc:"補足"`
	Findings  []Finding  `json:"findings,omitempty" desc:"指摘"`
}

// Stopping は、遷移を止めるラベル（MUST・Q）の指摘があるかを返す。
func (r *ReviewResult) Stopping() (must, q bool) {
	for _, f := range r.Findings {
		switch f.Label {
		case "MUST":
			must = true
		case "Q":
			q = true
		}
	}
	return must, q
}

// ParseP2 は、p2-review の出力を読み込んで検査する。MUST か Q があれば needs_fix、なければ approved。
func ParseP2(data []byte) (*ReviewResult, error) {
	return parseReview(data, []string{"approved", "needs_fix"}, func(must, q bool) string {
		if must || q {
			return "needs_fix"
		}
		return "approved"
	})
}

// ParseP5Verify は、p5-verify の出力を読み込んで検査する。MUST があれば needs_fix、Q だけなら needs_human、なければ approved。
func ParseP5Verify(data []byte) (*ReviewResult, error) {
	return parseReview(data, []string{"approved", "needs_fix", "needs_human"}, func(must, q bool) string {
		switch {
		case must:
			return "needs_fix"
		case q:
			return "needs_human"
		}
		return "approved"
	})
}

func parseReview(data []byte, outcomes []string, expected func(must, q bool) string) (*ReviewResult, error) {
	var r ReviewResult
	if err := decodeStrict(data, &r); err != nil {
		return nil, err
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !slices.Contains(outcomes, r.Outcome) {
		add("outcome %q は %v のどれでもない", r.Outcome, outcomes)
	}
	if r.Summary == "" {
		add("summary がない")
	}
	for i, f := range r.Findings {
		if !slices.Contains(labels, f.Label) {
			add("findings[%d].label %q は %v のどれでもない", i, f.Label, labels)
		}
		if f.Finding == "" {
			add("findings[%d].finding がない", i)
		}
		if e := f.Evidence; e == nil || (e.File == "" && e.ID == "") {
			add("findings[%d].evidence にファイルか ID がない", i)
		}
	}
	errs = append(errs, validateQuestions(r.Questions)...)
	if len(errs) == 0 {
		must, q := r.Stopping()
		if want := expected(must, q); r.Outcome != want {
			add("指摘のラベル（MUST：%v、Q：%v）からは outcome は %s のはずが %s", must, q, want, r.Outcome)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("出力の内容が正しくない:\n%w", errors.Join(errs...))
	}
	return &r, nil
}

// DescribeP2 は、p2-review の出力形式の説明。
func DescribeP2() string {
	return describe(ResultPath, ReviewResult{}) + "\n`outcome`の選択肢：`approved`・`needs_fix`。MUST か Q の指摘があれば`needs_fix`、なければ`approved`。\n"
}

// DescribeP5Verify は、p5-verify の出力形式の説明。
func DescribeP5Verify() string {
	return describe(ResultPath, ReviewResult{}) + "\n`outcome`の選択肢：`approved`・`needs_fix`・`needs_human`。MUST の指摘があれば`needs_fix`、Q だけなら`needs_human`、なければ`approved`。\n"
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("出力が JSON として正しくない: %w", err)
	}
	return nil
}

func validateQuestions(qs []Question) []error {
	var errs []error
	for i, q := range qs {
		if q.Text == "" {
			errs = append(errs, fmt.Errorf("questions[%d].text がない", i))
		}
	}
	return errs
}
