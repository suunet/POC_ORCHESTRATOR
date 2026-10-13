package output

import (
	"errors"
	"fmt"
	"slices"
)

// ARB の裁定の決定者、根拠の種類、人へ回す基準（M2.6）。
var (
	arbDeciders   = []string{"arb", "human"}
	basisKinds    = []string{"catalog", "policy", "standard", "question"}
	arbCriteria   = []string{"contract", "scope", "new-requirement", "overturn", "ambiguous"}
	arbOutcomes   = []string{"resolved", "escalated"}
	stoppingLabel = []string{"MUST", "Q"}
)

// Basis は、ARB の裁定の根拠（ちょうど1つ）。
type Basis struct {
	Kind string `json:"kind" desc:"根拠の種類。catalog（カタログの ID）、policy（規約のファイルと行）、standard（公開の規格。記憶による）、question（このチケットの確定した質疑）" enum:"catalog,policy,standard,question"`
	Ref  string `json:"ref" desc:"根拠の参照。catalog は ID（例：NFR-09）、policy はリポジトリの中のパスと行（例：.docs/operations/policies/coding.md:18）、standard は規格の名前と節、question は Q 番号（例：Q-37）"`
}

// Decision は、BJ の指摘（と質問）の1件ごとの裁定。同じ内容の指摘と質問は1件に束ねる。
type Decision struct {
	Findings  []int    `json:"findings,omitempty" desc:"束ねた BJ の指摘の番号（/in/review.json の findings の 0 から数えた位置。MUST と Q だけ）"`
	Decider   string   `json:"decider" desc:"決定者。arb（ARB が裁定する）か human（人へ回す）" enum:"arb,human"`
	Answer    string   `json:"answer,omitempty" desc:"arb のとき：裁定の文（質疑の回答として残る）"`
	Basis     *Basis   `json:"basis,omitempty" desc:"arb のとき：根拠（ちょうど1つ）"`
	Criterion string   `json:"criterion,omitempty" desc:"human のとき：人へ回す基準。contract（外から見える契約）、scope（範囲）、new-requirement（根拠のない新しい要求）、overturn（人の決定を覆す）、ambiguous（根拠を1つに絞れない）" enum:"contract,scope,new-requirement,overturn,ambiguous"`
	Question  string   `json:"question,omitempty" desc:"human のとき：人への質問（背景、番号付きの選択肢、推奨）"`
	Edits     []string `json:"edits,omitempty" desc:"arb のとき：この裁定で作業ツリーのカタログを直した ID（直さないなら書かない）"`
}

// ARBResult は、p2-arbitrate の出力。
type ARBResult struct {
	Outcome   string     `json:"outcome" desc:"結果"`
	Summary   string     `json:"summary" desc:"裁定の要約"`
	Decisions []Decision `json:"decisions" desc:"裁定（BJ の MUST・Q の指摘と、BJ の質問のすべてを、どれか1件に入れる）"`
	Section2  string     `json:"section2,omitempty" desc:"裁定を反映した指示書の §2 の本文（見出しの行は含めない）。変えないなら書かない"`
	Notes     []string   `json:"notes,omitempty" desc:"補足"`
}

// Human は、人へ回す裁定があるかを返す。
func (r *ARBResult) Human() bool {
	return slices.ContainsFunc(r.Decisions, func(d Decision) bool { return d.Decider == "human" })
}

// ParseARB は、p2-arbitrate の出力を読み込んで形を検査する。review は ARB に渡した BJ の出力、
// forced は進展なしで人へ回すと決めた指摘の番号（その指摘は human でなければならない）。
// 根拠の実在、契約、作業ツリーの変更の検査は、ファイルを読むので engine で行う。
func ParseARB(data []byte, review *ReviewResult, forced []int) (*ARBResult, error) {
	var r ARBResult
	if err := decodeStrict(data, &r); err != nil {
		return nil, err
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !slices.Contains(arbOutcomes, r.Outcome) {
		add("outcome %q は resolved・escalated のどちらでもない", r.Outcome)
	}
	if r.Summary == "" {
		add("summary がない")
	}
	findingSeen := map[int]int{}
	for i, d := range r.Decisions {
		where := fmt.Sprintf("decisions[%d]", i)
		if len(d.Findings) == 0 {
			add("%s：findings がない", where)
		}
		for _, f := range d.Findings {
			if f < 0 || f >= len(review.Findings) {
				add("%s：指摘の番号 %d が範囲の外", where, f)
				continue
			}
			if !slices.Contains(stoppingLabel, review.Findings[f].Label) {
				add("%s：指摘 %d は %s で、裁定の対象（MUST・Q）でない", where, f, review.Findings[f].Label)
			}
			findingSeen[f]++
		}
		switch d.Decider {
		case "arb":
			if d.Answer == "" {
				add("%s：arb なのに answer がない", where)
			}
			if d.Basis == nil || !slices.Contains(basisKinds, d.Basis.Kind) || d.Basis.Ref == "" {
				add("%s：arb なのに根拠（kind と ref）がない", where)
			}
			if d.Criterion != "" || d.Question != "" {
				add("%s：arb なのに criterion か question がある", where)
			}
			for _, f := range d.Findings {
				if slices.Contains(forced, f) {
					add("%s：指摘 %d は、前の回に ARB が裁定した指摘が残ったもので、人へ回さなければならない", where, f)
				}
			}
		case "human":
			if !slices.Contains(arbCriteria, d.Criterion) {
				add("%s：human なのに基準（criterion）が正しくない：%q", where, d.Criterion)
			}
			if d.Question == "" {
				add("%s：human なのに人への質問（question）がない", where)
			}
			if d.Answer != "" || d.Basis != nil || len(d.Edits) > 0 {
				add("%s：human なのに answer・basis・edits がある", where)
			}
		default:
			add("%s：decider %q は arb・human のどちらでもない", where, d.Decider)
		}
	}
	for i, f := range review.Findings {
		if slices.Contains(stoppingLabel, f.Label) && findingSeen[i] != 1 {
			add("BJ の指摘 %d（%s）が、ちょうど1件の裁定に入っていない（%d 件）", i, f.Label, findingSeen[i])
		}
	}
	if want := map[bool]string{true: "escalated", false: "resolved"}[r.Human()]; r.Outcome != "" && r.Outcome != want {
		add("outcome が %s だが、人へ回す裁定の有無からは %s", r.Outcome, want)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &r, nil
}

// DescribeARB は、p2-arbitrate の出力形式の説明。
func DescribeARB() string {
	return describe(ResultPath, ARBResult{}) + "\n`outcome`の選択肢：`resolved`・`escalated`。人へ回す裁定（decider が human）が1件でもあれば`escalated`、なければ`resolved`。\n"
}
