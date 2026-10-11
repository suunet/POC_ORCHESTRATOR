package output

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Acceptance は、受入条件の1件。ID は UC-NNN-AC-NN の形。
type Acceptance struct {
	ID      string `json:"id" desc:"受入条件の ID（例：UC-001-AC-02）。ユースケースの詳細の受入条件の節で発番する"`
	UC      string `json:"uc" desc:"ユースケースの ID（例：UC-001）"`
	Content string `json:"content" desc:"受入条件の内容"`
}

// TestCase は、テストケースの1件。
type TestCase struct {
	Test    string   `json:"test" desc:"テストの名前"`
	Layer   string   `json:"layer" desc:"テストの層" enum:"unit,scenario"`
	Targets []string `json:"targets,omitempty" desc:"確かめる受入条件の ID や観点"`
	Content string   `json:"content" desc:"テストの内容"`
}

// Sheet は、P5 で更新してよい文書の1件。
type Sheet struct {
	File   string `json:"file" desc:"文書のファイル（リポジトリのルートからの相対パス）"`
	Change string `json:"change" desc:"更新の内容"`
}

// P3Result は、p3-prepare の出力。gateflow が指示書の§3・§4に書く（§4.5 は人が書く）。
type P3Result struct {
	Outcome    string       `json:"outcome" desc:"結果" enum:"done,needs_answer"`
	Summary    string       `json:"summary" desc:"行ったことの要約"`
	Questions  []Question   `json:"questions,omitempty" desc:"人に確かめたいこと（推測で埋めない）。needs_answer なら1件以上"`
	Notes      []string     `json:"notes,omitempty" desc:"補足"`
	Current    string       `json:"current,omitempty" desc:"§3 変更前の現状（影響の範囲、関係するカタログ）"`
	Plan       string       `json:"plan,omitempty" desc:"§4.1 方針"`
	Acceptance []Acceptance `json:"acceptance,omitempty" desc:"§4.2 受入条件"`
	TestCases  []TestCase   `json:"testCases,omitempty" desc:"§4.3 テストケース。受入条件ごとにシナリオテストが1件以上"`
	Sheets     []Sheet      `json:"sheets,omitempty" desc:"§4.4 P5 で更新してよい文書"`
	Unfilled   []string     `json:"unfilled,omitempty" desc:"埋められなかった観点の値（質疑にもする）"`
}

var acceptanceID = regexp.MustCompile(`^(UC-[0-9]{3})-AC-[0-9]{2}$`)

// ParseP3 は、p3-prepare の出力を読み込んで検査する。
func ParseP3(data []byte) (*P3Result, error) {
	var r P3Result
	if err := decodeStrict(data, &r); err != nil {
		return nil, err
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch r.Outcome {
	case "done":
		for _, f := range []struct{ name, v string }{{"current", r.Current}, {"plan", r.Plan}} {
			if strings.TrimSpace(f.v) == "" {
				add("outcome が done なのに %s がない", f.name)
			}
		}
		if len(r.Acceptance) == 0 {
			add("outcome が done なのに acceptance がない")
		}
		if len(r.TestCases) == 0 {
			add("outcome が done なのに testCases がない")
		}
	case "needs_answer":
		if len(r.Questions) == 0 {
			add("outcome が needs_answer なのに questions がない")
		}
	default:
		add("outcome %q は done・needs_answer のどれでもない", r.Outcome)
	}
	if r.Summary == "" {
		add("summary がない")
	}
	scenario := map[string]bool{}
	for i, tc := range r.TestCases {
		if tc.Test == "" || tc.Content == "" {
			add("testCases[%d] の test か content がない", i)
		}
		if tc.Layer != "unit" && tc.Layer != "scenario" {
			add("testCases[%d].layer %q は unit・scenario のどれでもない", i, tc.Layer)
		}
		if tc.Layer == "scenario" {
			for _, t := range tc.Targets {
				scenario[t] = true
			}
		}
	}
	for i, a := range r.Acceptance {
		m := acceptanceID.FindStringSubmatch(a.ID)
		switch {
		case m == nil:
			add("acceptance[%d].id %q は UC-NNN-AC-NN の形でない", i, a.ID)
		case m[1] != a.UC:
			add("acceptance[%d].id %q が uc %q のものでない", i, a.ID, a.UC)
		case !scenario[a.ID]:
			add("受入条件 %s を確かめるシナリオテストが testCases にない", a.ID)
		}
		if a.Content == "" {
			add("acceptance[%d].content がない", i)
		}
	}
	for i, s := range r.Sheets {
		if s.File == "" {
			add("sheets[%d].file がない", i)
		}
	}
	errs = append(errs, validateQuestions(r.Questions)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("出力の内容が正しくない:\n%w", errors.Join(errs...))
	}
	return &r, nil
}

// AcceptanceIDs は、受入条件の ID の一覧を返す。
func (r *P3Result) AcceptanceIDs() []string {
	ids := make([]string, 0, len(r.Acceptance))
	for _, a := range r.Acceptance {
		ids = append(ids, a.ID)
	}
	return slices.Clip(ids)
}

// DescribeP3 は、p3-prepare の出力形式の説明。
func DescribeP3() string {
	return describe(ResultPath, P3Result{})
}
