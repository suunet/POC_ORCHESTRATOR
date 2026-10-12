package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gateflow/internal/config"
	"gateflow/internal/idcheck"
	"gateflow/internal/output"
	"gateflow/internal/ticket"
)

// arbitrateCounter は、ARB の裁定から P2 へ戻る往復の数え方（上限は limits.arbitrateLoops。M2.6）。
const arbitrateCounter = "arbitrate"

// catalogDir は、ARB が直してよいカタログの置き場所（M2.6 の Q-02）。
const catalogDir = ".docs/design/"

// ARBCheck は、ARB の出力の、ファイルを読む検査の材料（本番と試験で同じ検査を使う）。
type ARBCheck struct {
	Result  *output.ARBResult
	Review  *output.ReviewResult
	Seen    string // ARB の前の作業ブランチの版（カタログと Git 管理の規約）
	In      string // ARB に渡した入力の写し（questions.md、policies/）
	After   string // ARB の後の作業ツリー
	Project *config.Project
	Rules   *idcheck.Rules
}

// ARBChange は、ARB が作業ツリーで変えた1つのファイル。Added・Removed は、増えた行と消えた行。
type ARBChange struct {
	Path    string   `json:"path"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// ValidateARB は、ARB の裁定の根拠の実在、契約（Q-05）、作業ツリーの変更を確かめ、変更の一覧を返す。
// 不合格は出力の不正として扱う（1回だけやり直させる）。
func ValidateARB(c ARBCheck) ([]ARBChange, error) {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	var defined *idcheck.Result
	if c.Rules != nil {
		r, err := idcheck.Run(c.Seen, c.Rules)
		if err != nil {
			return nil, err
		}
		defined = &r
	}
	questions := map[string]ticket.QuestionInfo{}
	if data, err := os.ReadFile(filepath.Join(c.In, ticket.QuestionsFile)); err == nil {
		questions = ticket.QuestionStates(data)
	}
	findIDs := func(s string) []string {
		if c.Rules == nil {
			return nil
		}
		return c.Rules.FindIDs(s)
	}

	edits := map[string]bool{}
	for i, d := range c.Result.Decisions {
		if d.Decider != "arb" {
			continue
		}
		where := fmt.Sprintf("decisions[%d]", i)
		for _, id := range d.Edits {
			edits[id] = true
		}
		b := d.Basis
		switch b.Kind {
		case "question":
			q, ok := questions[strings.TrimSpace(b.Ref)]
			switch {
			case !ok:
				add("%s：根拠の質疑 %s が質疑にない", where, b.Ref)
			case q.State != "確定":
				add("%s：根拠の質疑 %s は確定していない（%s）", where, b.Ref, q.State)
			}
		case "catalog":
			ids := findIDs(b.Ref)
			if len(ids) == 0 {
				add("%s：根拠 %q にカタログの ID がない", where, b.Ref)
			} else if defined != nil && !defined.Defined(ids[0]) && !appearsInCatalog(c.Seen, ids[0]) {
				add("%s：根拠の ID %s がカタログにない", where, ids[0])
			}
		case "policy":
			if err := policyExists(b.Ref, c.Seen, c.In); err != nil {
				add("%s：%v", where, err)
			}
		}
		for _, id := range findIDs(b.Ref + "\n" + d.Answer + "\n" + strings.Join(d.Edits, "\n")) {
			if c.Project.IsContract(id) {
				add("%s：契約の ID %s に触れる裁定は、人へ回す（基準1。project.json の contracts）", where, id)
			}
		}
	}

	changes, err := treeChanges(c.Seen, c.After)
	if err != nil {
		return nil, err
	}
	for _, ch := range changes {
		if !strings.HasPrefix(ch.Path, catalogDir) {
			add("作業ツリーの %s を変えている（ARB が直してよいのは %s の下だけ）", ch.Path, catalogDir)
			continue
		}
		for _, id := range findIDs(strings.Join(append(append([]string{}, ch.Added...), ch.Removed...), "\n")) {
			if c.Project.IsContract(id) {
				add("%s の変えた行に契約の ID %s がある（基準1。人へ回す）", ch.Path, id)
			} else if !edits[id] {
				add("%s の変えた行の ID %s が、どの裁定の edits にもない", ch.Path, id)
			}
		}
	}
	return changes, errors.Join(errs...)
}

// policyExists は、規約の参照（リポジトリの中のパスと、任意で行）が、ARB の見た版か渡した写しにあるかを確かめる。
func policyExists(ref, seen, in string) error {
	path, line := strings.TrimSpace(ref), 0
	if i := strings.LastIndex(path, ":"); i > 0 {
		if n, err := strconv.Atoi(path[i+1:]); err == nil {
			path, line = path[:i], n
		}
	}
	rel := filepath.FromSlash(strings.TrimPrefix(path, "/work/"))
	if strings.HasPrefix(rel, "/in/policies/") {
		rel = strings.TrimPrefix(rel, "/in/policies/")
	}
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("根拠の規約 %s が作業場所の外を指す", ref)
	}
	for _, base := range []string{seen, filepath.Join(in, "policies")} {
		data, err := os.ReadFile(filepath.Join(base, rel))
		if err != nil {
			continue
		}
		if line > countLines(data) {
			return fmt.Errorf("根拠の規約 %s の行が、ファイルの行数（%d）を超える", ref, countLines(data))
		}
		return nil
	}
	return fmt.Errorf("根拠の規約 %s がない", ref)
}

// treeChanges は、before と after の2つのディレクトリを比べ、変わったファイルと、増えた行・消えた行を返す。
func treeChanges(before, after string) ([]ARBChange, error) {
	files := map[string]bool{}
	for _, root := range []string{before, after} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				rel, _ := filepath.Rel(root, p)
				files[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	var out []ARBChange
	for _, name := range names {
		a, _ := os.ReadFile(filepath.Join(before, filepath.FromSlash(name)))
		b, _ := os.ReadFile(filepath.Join(after, filepath.FromSlash(name)))
		if bytes.Equal(a, b) {
			continue
		}
		out = append(out, ARBChange{Path: name, Added: lineDiff(b, a), Removed: lineDiff(a, b)})
	}
	return out, nil
}

// lineDiff は、x にあって y にない行（数を含めて）を返す。
func lineDiff(x, y []byte) []string {
	count := map[string]int{}
	for _, l := range strings.Split(string(y), "\n") {
		count[l]++
	}
	var out []string
	for _, l := range strings.Split(string(x), "\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		out = append(out, l)
	}
	return out
}

// arbForced は、P2 の指摘のうち、直前の ARB が裁定したのに残った指摘の番号を返す（その指摘は人へ回す）。
func arbForced(review *output.ReviewResult, decided []string) []int {
	var out []int
	for i, f := range review.Findings {
		if (f.Label == "MUST" || f.Label == "Q") && slices.Contains(decided, findingKey(f)) {
			out = append(out, i)
		}
	}
	return out
}

// checkARBStep は、本番の ARB の出力を検査する（作業ツリーを書き出して ValidateARB に渡す）。
func (e *Engine) checkARBStep(ctx context.Context, dir, seen string) error {
	after := filepath.Join(dir, "after")
	os.RemoveAll(after)
	if err := e.Workspace.Snapshot(ctx, e.Ticket.ID, after); err != nil {
		return err
	}
	changes, err := ValidateARB(ARBCheck{Result: e.lastARB, Review: e.lastReview, Seen: seen, In: filepath.Join(dir, "in"),
		After: after, Project: e.Project, Rules: e.Rules})
	var paths []string
	for _, ch := range changes {
		paths = append(paths, ch.Path)
	}
	var arb, human int
	for _, d := range e.lastARB.Decisions {
		if d.Decider == "arb" {
			arb++
		} else {
			human++
		}
	}
	e.log("arb-check", map[string]any{"arb": arb, "human": human, "changed": paths, "ok": err == nil})
	return err
}

// arbDecided は、ARB が裁定した指摘の指紋（次の P2 で残ったら人へ回す）。
func (e *Engine) arbDecided() []string {
	var keys []string
	for _, d := range e.lastARB.Decisions {
		if d.Decider != "arb" {
			continue
		}
		for _, f := range d.Findings {
			keys = append(keys, findingKey(e.lastReview.Findings[f]))
		}
	}
	return keys
}

func basisText(b *output.Basis) string {
	if b == nil {
		return ""
	}
	label := map[string]string{"catalog": "カタログ", "policy": "規約", "standard": "規格（記憶による）", "question": "既決"}[b.Kind]
	return label + "：" + b.Ref
}

// decisionBody は、裁定に束ねた BJ の指摘と質問の本文。
func (e *Engine) decisionBody(d output.Decision) string {
	var b strings.Builder
	for _, f := range d.Findings {
		fd := e.lastReview.Findings[f]
		fmt.Fprintf(&b, "- （BJ の %s）%s", fd.Label, strings.TrimSpace(fd.Finding))
		if ev := evidenceText(fd.Evidence); ev != "" {
			fmt.Fprintf(&b, "（根拠：%s）", ev)
		}
		b.WriteString("\n")
	}
	for _, q := range d.Questions {
		fmt.Fprintf(&b, "- （BJ の質問）%s\n", strings.TrimSpace(e.lastReview.Questions[q].Text))
	}
	return b.String()
}

func (e *Engine) decisionSummary(d output.Decision) string {
	if len(d.Findings) > 0 {
		return summarize(e.lastReview.Findings[d.Findings[0]].Finding)
	}
	return summarize(e.lastReview.Questions[d.Questions[0]].Text)
}

// arbAnswers は、ARB が裁定したものを、決定者 ARB・状態「確定」で質疑に書く。
func (e *Engine) arbAnswers() error {
	r := e.lastARB
	var qs []ticketQuestion
	for _, d := range r.Decisions {
		if d.Decider != "arb" {
			continue
		}
		var reflected []string
		if r.Section2 != "" {
			reflected = append(reflected, "指示書 §2")
		}
		if len(d.Edits) > 0 {
			reflected = append(reflected, "カタログ（作業ブランチ）："+strings.Join(d.Edits, "・"))
		}
		qs = append(qs, ticketQuestion{Summary: e.decisionSummary(d), Body: e.decisionBody(d), Decider: "ARB",
			Basis: basisText(d.Basis), Answer: d.Answer, Reflected: strings.Join(reflected, "、")})
	}
	if len(qs) == 0 {
		e.arbQIDs = nil
		return nil
	}
	ids, err := e.Ticket.AppendQuestions("ARB の裁定（P2 の指摘）", qs)
	e.arbQIDs = ids
	return err
}

// arbEscalate は、人へ回すものを、決定者 人・状態「未回答」で質疑に書く。
func (e *Engine) arbEscalate() error {
	var qs []ticketQuestion
	for _, d := range e.lastARB.Decisions {
		if d.Decider != "human" {
			continue
		}
		qs = append(qs, ticketQuestion{Summary: summarize(d.Question), Body: strings.TrimSpace(d.Question) + "\n\n対象の指摘：\n\n" + e.decisionBody(d),
			Mark: "人へ回す（基準：" + criterionText(d.Criterion) + "）"})
	}
	_, err := e.Ticket.AppendQuestions("ARB が人へ回したもの（P2 の指摘）", qs)
	return err
}

func criterionText(c string) string {
	return map[string]string{"contract": "外から見える契約", "scope": "範囲", "new-requirement": "根拠のない新しい要求",
		"overturn": "人の決定を覆す", "ambiguous": "根拠を1つに絞れない"}[c]
}

// arbCommitMessage は、ARB の裁定を反映したコミットのメッセージ。
func (e *Engine) arbCommitMessage() string {
	msg := fmt.Sprintf("docs(%s): ARB の裁定を反映", e.Ticket.ID)
	if len(e.arbQIDs) > 0 {
		msg += "（" + strings.Join(e.arbQIDs, "、") + "）"
	}
	return msg
}

// arbReport は、承認の前の監査のために、ARB の裁定の一覧と、作業ブランチのカタログの差分を実行の記録に書く（M2.6）。
func (e *Engine) arbReport(ctx context.Context) error {
	var rows []ticket.QuestionRow
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(e.Ticket.Path), ticket.QuestionsFile)); err == nil {
		for _, r := range ticket.QuestionRows(data) {
			if r.Decider == "ARB" {
				rows = append(rows, r)
			}
		}
	}
	diff, err := e.Workspace.Diff(ctx, e.Ticket.ID, e.Project.Branch.Main)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s ARB の裁定（承認の前の監査）\n\n質疑の決定者が ARB の行の一覧と、作業ブランチのカタログの差分（%s との比較、%s の下だけ）。反対なら、質疑の回答を書き直して決定者を人にし、gateflow ready で P2 からやり直す。\n\n## 裁定（%d 件）\n\n",
		e.Ticket.ID, e.Project.Branch.Main, catalogDir, len(rows))
	if len(rows) == 0 {
		b.WriteString("なし\n")
	} else {
		b.WriteString("| 質疑 | 要約 | 根拠 |\n| --- | --- | --- |\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.ID, r.Summary, r.Basis)
		}
	}
	catalog := catalogDiff(diff)
	b.WriteString("\n## カタログの差分\n\n")
	if catalog == "" {
		b.WriteString("なし\n")
	} else {
		b.WriteString("```diff\n" + catalog + "```\n")
	}
	path := filepath.Join(e.run.Dir, "arb-report.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	e.log("arb-report", map[string]any{"decisions": len(rows), "path": path})
	return nil
}

// catalogDiff は、git の差分から、カタログ（.docs/design/）のファイルの部分だけを取り出す。
func catalogDiff(diff string) string {
	var b strings.Builder
	keep := false
	for _, l := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(l, "diff --git ") {
			keep = strings.HasPrefix(strings.TrimPrefix(l, "diff --git "), "a/"+catalogDir)
		}
		if keep {
			b.WriteString(l)
		}
	}
	return b.String()
}

// arbKnowledge は、ARB に渡す知識（本番と試験で同じ）。
func (e *Engine) arbKnowledge() []string {
	k := []string{
		"BJ の P2 のレビューの出力（findings と questions）は`/in/review.json`。番号は 0 から数える",
		"`/in/forced.json`の findings の番号は、前の回に ARB が裁定したのに BJ の指摘が残ったもの。必ず人へ回す（decider を human にする）",
		"カタログ（`/work/.docs/design/`）は作業ツリーで直してよい。それ以外のファイルは変えない。コミットは gateflow が行う",
		"外のサイトは読めない。規格を根拠にするときは、記憶によることを前提に kind を standard にする",
	}
	if len(e.Project.Contracts) > 0 {
		k = append(k, "契約の ID の一覧（これに触れる裁定は人へ回す。末尾の * は前方一致）："+strings.Join(e.Project.Contracts, "、"))
	}
	return k
}
