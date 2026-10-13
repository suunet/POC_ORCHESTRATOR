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

// HostCheck は、gateflow の外でホストで行う確かめ（§4.5）。Review を先に行い、通ってから Steps を順に行う。
type HostCheck struct {
	Review []string        `json:"review" desc:"ホストで動かす前にコードレビューで読むファイルと、確かめること（ホストのフォルダを繋ぐ設定、特権での実行、外へ公開するポートが想定のとおりか）"`
	Steps  []HostCheckStep `json:"steps" desc:"コードレビューが通ってから、ホストで行う手順（この順で行う）"`
}

// HostCheckStep は、ホストでの手順の1つ。
type HostCheckStep struct {
	Action string `json:"action" desc:"行うこと（コマンドなど）"`
	Pass   string `json:"pass" desc:"何を見れば合格か"`
}

// IMOResponse は、P2 の IMO の1件への対応。
type IMOResponse struct {
	Index    int    `json:"index" desc:"/in/p2-imo.json の IMO の番号（0 から数える）"`
	Decision string `json:"decision" desc:"planned（計画に入れた）か deferred（見送った）" enum:"planned,deferred"`
	Detail   string `json:"detail" desc:"planned なら §4 のどこに入れたか、deferred なら見送った理由"`
}

// P3Result は、p3-prepare の出力。gateflow が指示書の§3・§4に書く（§4.6 は人が書く）。
type P3Result struct {
	Outcome    string        `json:"outcome" desc:"結果" enum:"done,needs_answer"`
	Summary    string        `json:"summary" desc:"行ったことの要約"`
	Questions  []Question    `json:"questions,omitempty" desc:"人に確かめたいこと（推測で埋めない）。needs_answer なら1件以上"`
	Notes      []string      `json:"notes,omitempty" desc:"補足"`
	Current    string        `json:"current,omitempty" desc:"§3 変更前の現状（影響の範囲、関係するカタログ）"`
	Plan       string        `json:"plan,omitempty" desc:"§4.1 方針"`
	Acceptance []Acceptance  `json:"acceptance,omitempty" desc:"§4.2 受入条件。種別が実装なら1件以上、基盤なら書かない"`
	TestCases  []TestCase    `json:"testCases,omitempty" desc:"§4.3 テストケース。受入条件ごとにシナリオテストが1件以上。種別が基盤なら0件でよい"`
	Sheets     []Sheet       `json:"sheets,omitempty" desc:"§4.4 P5 で更新してよい文書"`
	IMO        []IMOResponse `json:"imo,omitempty" desc:"P2 の IMO（/in/p2-imo.json）の1件ずつへの対応。IMO があれば全件に1件ずつ"`
	HostCheck  *HostCheck    `json:"hostCheck,omitempty" desc:"§4.5 ホストでの確かめ（gateflow の中で確かめられない動作を、ready-for-review の後にホストで確かめる）。種別が基盤なら必須、実装なら要るときだけ"`
	Unfilled   []string      `json:"unfilled,omitempty" desc:"埋められなかった観点の値（質疑にもする）"`
}

var acceptanceID = regexp.MustCompile(`^(UC-[0-9]{3})-AC-[0-9]{2}$`)

// ParseP3 は、p3-prepare の出力を読み込んで検査する。infra は指示書の種別が基盤か（M2.7）、
// imo は P3 に渡した P2 の IMO の件数（done なら、その1件ずつに対応が要る）。
func ParseP3(data []byte, infra bool, imo int) (*P3Result, error) {
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
		switch {
		case infra && len(r.Acceptance) > 0:
			add("種別が基盤なのに acceptance がある（基盤のチケットは受入条件を持たない）")
		case infra && r.HostCheck == nil:
			add("種別が基盤なのに hostCheck がない")
		case !infra && len(r.Acceptance) == 0:
			add("outcome が done なのに acceptance がない")
		}
		if !infra && len(r.TestCases) == 0 {
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
	seenIMO := map[int]int{}
	for i, m := range r.IMO {
		if m.Index < 0 || m.Index >= imo {
			add("imo[%d].index %d が P2 の IMO の範囲（%d 件）の外", i, m.Index, imo)
		}
		seenIMO[m.Index]++
		if m.Decision != "planned" && m.Decision != "deferred" {
			add("imo[%d].decision %q は planned・deferred のどちらでもない", i, m.Decision)
		}
		if strings.TrimSpace(m.Detail) == "" {
			add("imo[%d].detail がない（計画に入れた場所か、見送った理由）", i)
		}
	}
	if r.Outcome == "done" {
		for i := 0; i < imo; i++ {
			if seenIMO[i] != 1 {
				add("P2 の IMO %d への対応が imo にちょうど1件ない（%d 件）", i, seenIMO[i])
			}
		}
	}
	if h := r.HostCheck; h != nil {
		if len(h.Review) == 0 {
			add("hostCheck.review がない（ホストで動かす前のコードレビューを先に書く）")
		}
		if len(h.Steps) == 0 {
			add("hostCheck.steps がない")
		}
		for i, rv := range h.Review {
			if strings.TrimSpace(rv) == "" {
				add("hostCheck.review[%d] が空", i)
			}
		}
		for i, st := range h.Steps {
			if strings.TrimSpace(st.Action) == "" || strings.TrimSpace(st.Pass) == "" {
				add("hostCheck.steps[%d] の action か pass がない", i)
			}
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
