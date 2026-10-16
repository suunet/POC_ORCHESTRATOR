package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gateflow/internal/idcheck"
	"gateflow/internal/output"
	"gateflow/internal/ticket"
)

// doActions は、遷移表の行き先の操作を順に行う。機械検証に不合格なら、その内容を failure で返す（同じ step に差し戻す）。
func (e *Engine) doActions(ctx context.Context, st *State, step string, actions []string) (failure string, err error) {
	for _, a := range actions {
		var fail string
		switch a {
		case "write-sections":
			err = e.writeSections()
		case "check-sections":
			if missing := e.Ticket.MissingSections(); len(missing) > 0 {
				fail = "指示書の次の節が埋まっていない：" + strings.Join(missing, "、")
			}
		case "commit":
			err = e.commit(ctx, st, step)
		case "commit-tests":
			fail, err = e.commitTests(ctx, st)
		case "check-tests-unchanged":
			err = e.checkTestsUnchanged(ctx, st)
		case "check-acceptance":
			fail, err = e.checkAcceptance(ctx)
		case "check-ids":
			fail, err = e.checkIDs(ctx, step)
		case "check-main-ids":
			err = e.checkMainIDs(ctx)
		case "recommended-report":
			err = e.recommendedReport()
		case "check-touch-docs":
			err = e.checkTouchDocs(ctx)
		case "import":
			if err = e.Workspace.Import(ctx, e.Ticket.ID, e.Ticket.Branch); err == nil {
				e.log("import", map[string]any{"branch": e.Ticket.Branch})
			}
		case "arb-answers":
			err = e.arbAnswers()
		case "arb-sections":
			if e.lastARB.Section2 != "" {
				err = e.Ticket.WriteSection2(e.lastARB.Section2)
			}
		case "arb-escalate":
			err = e.arbEscalate()
		case "arb-report":
			err = e.arbReport(ctx)
		case "findings-to-questions":
			err = e.findingsToQuestions(step)
		case "questions-from-output":
			err = e.p3Questions()
		default:
			err = fmt.Errorf("操作 %s は gateflow にない", a)
		}
		var se *errStop
		if errors.As(err, &se) {
			return "", err // 人の確認待ちで止める（理由はそのまま）
		}
		if err != nil {
			return "", fmt.Errorf("操作 %s：%w", a, err)
		}
		if fail != "" {
			return fail, nil
		}
	}
	return "", nil
}

func (e *Engine) writeSections() error {
	r := e.lastP3
	if r == nil {
		return fmt.Errorf("P3 の結果がない")
	}
	s := ticket.Sections{Current: r.Current, Plan: r.Plan}
	for _, a := range r.Acceptance {
		s.Acceptance = append(s.Acceptance, ticket.AcceptanceRow{ID: a.ID, UC: a.UC, Content: a.Content})
	}
	layer := map[string]string{"unit": "単体", "scenario": "シナリオ"}
	for _, tc := range r.TestCases {
		s.TestCases = append(s.TestCases, ticket.TestCaseRow{Test: tc.Test, Layer: layer[tc.Layer], Targets: strings.Join(tc.Targets, "、"), Content: tc.Content})
	}
	for _, sh := range r.Sheets {
		s.Sheets = append(s.Sheets, ticket.SheetRow{File: sh.File, Change: sh.Change})
	}
	decision := map[string]string{"planned": "計画に入れた", "deferred": "見送った"}
	for _, m := range r.IMO {
		if m.Index >= 0 && m.Index < len(e.p3IMO) {
			s.IMO = append(s.IMO, ticket.IMORow{Finding: e.p3IMO[m.Index].Finding, Decision: decision[m.Decision], Detail: m.Detail})
		}
	}
	if h := r.HostCheck; h != nil {
		s.HostCheck = &ticket.HostCheck{Review: h.Review}
		for _, st := range h.Steps {
			s.HostCheck.Steps = append(s.HostCheck.Steps, ticket.HostCheckStep{Action: st.Action, Pass: st.Pass})
		}
	}
	return e.Ticket.WriteSections(s)
}

func (e *Engine) commit(ctx context.Context, st *State, step string) error {
	t := e.Ticket
	msg := fmt.Sprintf("feat(%s): %s", t.ID, t.Title)
	switch step {
	case "p3-prepare":
		msg = fmt.Sprintf("docs(%s): %s（実装の準備）", t.ID, t.Title)
	case "p2-arbitrate":
		msg = e.arbCommitMessage()
	}
	if i := e.Project.Issue; i != nil && i.CommitRef != "" && t.IssueID() != "" {
		msg += " " + i.Format(i.CommitRef, t.IssueID())
	}
	hash, changed, err := e.Workspace.CommitHash(ctx, t.ID, msg, e.Author)
	if err != nil {
		return err
	}
	st.Commit = hash
	st.Commits = append(st.Commits, hash)
	e.log("commit", map[string]any{"message": msg, "commit": hash, "changed": changed})
	return nil
}

// commitTests は、p5-test が書いたテストをコミットして、その位置を覚える（M3）。変更がなければ差し戻す。
func (e *Engine) commitTests(ctx context.Context, st *State) (string, error) {
	t := e.Ticket
	msg := fmt.Sprintf("test(%s): %s（テスト）", t.ID, t.Title)
	if i := e.Project.Issue; i != nil && i.CommitRef != "" && t.IssueID() != "" {
		msg += " " + i.Format(i.CommitRef, t.IssueID())
	}
	hash, changed, err := e.Workspace.CommitHash(ctx, t.ID, msg, e.Author)
	if err != nil {
		return "", err
	}
	e.log("commit", map[string]any{"message": msg, "commit": hash, "changed": changed})
	if !changed {
		return "テストのファイルが1つも変わっていない。指示書の§4.3 のテストケースのテストを書く", nil
	}
	st.Commit = hash
	st.Commits = append(st.Commits, hash)
	st.TestCommits = append(st.TestCommits, hash)
	return "", nil
}

// stopTestModified は、p5-implement が p5-test のテストのファイルを変えた（消した）ときの止まる理由（M3）。
const stopTestModified = "test-modified"

// checkTestsUnchanged は、p5-test のコミットで書いたファイルを、p5-implement の後の作業ツリーと比べる（M3）。
// 変わって（消されて）いれば、テストを書き換えて通そうとした恐れがあるので、人の確認待ちで止める。テストのファイルを足すのはよい。
func (e *Engine) checkTestsUnchanged(ctx context.Context, st *State) error {
	var changed []string
	for _, c := range st.TestCommits {
		files, err := e.Workspace.TestsChanged(ctx, e.Ticket.ID, c)
		if err != nil {
			return err
		}
		for _, f := range files {
			if !slices.Contains(changed, f) {
				changed = append(changed, f)
			}
		}
	}
	e.log("check-tests-unchanged", map[string]any{"commits": st.TestCommits, "changed": changed})
	if len(changed) > 0 {
		return &errStop{reason: stopTestModified, detail: "p5-implement が、p5-test で書いたテストのファイルを変えた（消した）：" + strings.Join(changed, "、") +
			"。テストの誤りなら test_defect で報告する決まり。変更を確かめて、人が決める"}
	}
	return nil
}

// exportForCheck は、検査のために clone のコミット済みの版を実行の記録の下に書き出す（Q-07）。
func (e *Engine) exportForCheck(ctx context.Context) (string, string, error) {
	e.checks++
	dir := filepath.Join(e.run.Dir, "checks", fmt.Sprintf("%02d", e.checks))
	hash, err := e.Workspace.Export(ctx, e.Ticket.ID, dir)
	return dir, hash, err
}

// recommendedReport は、この実行で見つけた推奨の指摘を、実行の記録の recommended.md にまとめる（人のレビューで提示する。M3）。
func (e *Engine) recommendedReport() error {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s 推奨の指摘\n\n止めない指摘（project.json の levels で推奨のもの）。人のコードレビューで確かめる。\n\n", e.Ticket.ID)
	if len(e.recommended) == 0 {
		b.WriteString("なし\n")
	}
	for _, r := range e.recommended {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	path := filepath.Join(e.run.Dir, "recommended.md")
	if err := e.writeState(path, []byte(b.String())); err != nil {
		return err
	}
	e.recommendedPath = path
	e.log("recommended", map[string]any{"count": len(e.recommended), "path": e.relPath(path)})
	return nil
}

// stopIDDuplicate は、main と合わせると ID 整合チェックの必須の指摘が出るときの止まる理由（M2.9）。
const stopIDDuplicate = "id-duplicate"

// checkMainIDs は、作業ブランチへ取り込む前に、その時点の main と作業ブランチを合わせた木で ID 整合チェックを行う（M2.9）。
// 並行で先に main へ入った ID と重なれば（重複定義など、必須の指摘）、取り込まずに人の確認待ちで止める。
// 合わせると衝突するときは、検査できないので記録だけ残す（衝突は人のマージで必ず出て、done の検査も受け皿になる）。
func (e *Engine) checkMainIDs(ctx context.Context) error {
	e.checks++
	dir := filepath.Join(e.run.Dir, "checks", fmt.Sprintf("%02d-main", e.checks))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return e.writeFailed(dir, err)
	}
	conflict, err := e.Workspace.MergedTree(ctx, e.Ticket.ID, e.Ticket.Branch, e.Project.Branch.Main, dir)
	if err != nil {
		return err
	}
	if conflict {
		e.log("check-main-ids", map[string]any{"conflict": true})
		e.warnings = append(e.warnings, "作業ブランチを main と合わせると衝突するので、main との ID の重複を確かめていない（マージのときに人が解く。done で main を検査する）")
		return nil
	}
	res, err := idcheck.Run(dir, e.Rules)
	if err != nil {
		return err
	}
	var msgs []string
	for _, f := range res.Findings {
		if f.Level == idcheck.Required {
			msgs = append(msgs, fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Message))
		}
	}
	e.log("check-main-ids", map[string]any{"dir": dir, "required": len(msgs)})
	if len(msgs) > 0 {
		return &errStop{reason: stopIDDuplicate, detail: "main と合わせると、ID 整合チェックの必須の指摘がある（並行で先に main へ入った ID と重なっている）：" + strings.Join(msgs, "／")}
	}
	return nil
}

func (e *Engine) checkIDs(ctx context.Context, step string) (string, error) {
	dir, hash, err := e.exportForCheck(ctx)
	if err != nil {
		return "", err
	}
	// P3 の直後は、受入条件を発番したばかりでテストがまだない（P5 で書く）ので、テストの検査を外す（M3 の Q-03）
	res, err := idcheck.RunWith(dir, e.Rules, idcheck.Options{SkipTestMissing: step == "p3-prepare"})
	if err != nil {
		return "", err
	}
	var msgs []string
	for _, f := range res.Findings {
		if f.Level == idcheck.Required {
			msgs = append(msgs, fmt.Sprintf("- %s:%d %s", f.File, f.Line, f.Message))
		} else if step != "p3-prepare" {
			e.recommended = append(e.recommended, fmt.Sprintf("ID 整合チェック（%s）：%s:%d %s", f.Kind, f.File, f.Line, f.Message))
		}
	}
	e.log("check-ids", map[string]any{"commit": hash, "dir": dir, "required": len(msgs), "findings": len(res.Findings)})
	if len(msgs) > 0 {
		return "ID 整合チェックの必須の指摘（版 " + hash + "）：\n" + strings.Join(msgs, "\n"), nil
	}
	return "", nil
}

// checkAcceptance は、受入条件がすべて実在するユースケースに対応し、ユースケースの詳細に書かれていることを確かめる。
func (e *Engine) checkAcceptance(ctx context.Context) (string, error) {
	if e.lastP3 == nil {
		return "", fmt.Errorf("P3 の結果がない")
	}
	dir, hash, err := e.exportForCheck(ctx)
	if err != nil {
		return "", err
	}
	res, err := idcheck.Run(dir, e.Rules)
	if err != nil {
		return "", err
	}
	var msgs []string
	for _, a := range e.lastP3.Acceptance {
		if !res.Defined(a.UC) {
			msgs = append(msgs, fmt.Sprintf("- 受入条件 %s のユースケース %s がカタログにない", a.ID, a.UC))
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ".docs", "design", "uc", a.UC+".md"))
		if err != nil || !strings.Contains(string(data), a.ID) {
			msgs = append(msgs, fmt.Sprintf("- 受入条件 %s が、ユースケースの詳細 .docs/design/uc/%s.md にない", a.ID, a.UC))
		}
	}
	e.log("check-acceptance", map[string]any{"commit": hash, "failures": len(msgs)})
	if len(msgs) > 0 {
		return "受入条件の確認（版 " + hash + "）：\n" + strings.Join(msgs, "\n"), nil
	}
	return "", nil
}

// findingsToQuestions は、BJ の MUST と Q の指摘を、指摘1件につき1件の未回答の質疑にする（Q-05。M2.9 で BJ の質問をなくした）。
func (e *Engine) findingsToQuestions(step string) error {
	r := e.lastReview
	if r == nil {
		return fmt.Errorf("レビューの結果がない")
	}
	var qs []ticketQuestion
	for _, f := range r.Findings {
		if f.Label != "MUST" && f.Label != "Q" {
			continue
		}
		q := ticketQuestion{Summary: summarize(f.Finding), Body: f.Finding, Evidence: evidenceText(f.Evidence), Suggestion: f.Suggestion}
		if len(f.Options) > 0 {
			q.Body += "\n\n選択肢：" + strings.Join(f.Options, "／")
		}
		if f.Label == "MUST" {
			q.Mark = "指示書の修正が必要"
		}
		qs = append(qs, q)
	}
	_, err := e.appendQuestions(step, stepLabel(step), qs)
	return err
}

// appendQuestions は、質疑に登録し、登録した Q 番号を記録に残す（監視の UI の作業履歴から質疑をたどる。M2.8）。
func (e *Engine) appendQuestions(step, origin string, qs []ticketQuestion) ([]string, error) {
	ids, err := e.Ticket.AppendQuestions(origin, qs)
	if err != nil {
		return ids, err
	}
	if len(ids) > 0 {
		e.log("questions-added", map[string]any{"step": step, "ids": ids})
	}
	return ids, nil
}

func (e *Engine) p3Questions() error {
	r := e.lastP3
	if r == nil {
		return fmt.Errorf("P3 の結果がない")
	}
	qs := outputQuestions(r.Questions)
	for _, u := range r.Unfilled {
		qs = append(qs, ticketQuestion{Summary: summarize(u), Body: "値が決められない観点：" + u})
	}
	_, err := e.appendQuestions("p3-prepare", stepLabel("p3-prepare"), qs)
	return err
}

type ticketQuestion = ticket.NewQuestion

func outputQuestions(qs []output.Question) []ticketQuestion {
	var out []ticketQuestion
	for _, q := range qs {
		body := q.Text
		if len(q.Options) > 0 {
			body += "\n\n選択肢：" + strings.Join(q.Options, "／")
		}
		out = append(out, ticketQuestion{Summary: summarize(q.Text), Body: body})
	}
	return out
}

func summarize(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

func stepLabel(step string) string {
	switch step {
	case "p2-review":
		return "P2 のレビュー"
	case "p3-prepare":
		return "P3 の準備"
	case "p5-verify":
		return "P5後のレビュー"
	}
	return step
}

// writeReview は、BJ の出力からレビュー報告（reviews/NN-<step>.md）を作る（Q-20）。人が読む記録なので、
// 書けなければ止める（M2.8）。一覧が読めないと番号が 01 に戻り、既存の報告を上書きしうるので、それも止める。
func (e *Engine) writeReview(step string) error {
	r := e.lastReview
	dir := filepath.Join(filepath.Dir(e.Ticket.Path), "reviews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return e.writeFailed(dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return e.writeFailed(dir, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s レビュー報告（%s）\n\nこの報告は、BJ の出力（result.json）から gateflow が作った。\n\n| 項目 | 値 |\n| --- | --- |\n| step | %s |\n| 実施日時 | %s |\n| 結果（outcome） | %s |\n\n## 要約\n\n%s\n\n## 指摘\n\n",
		e.Ticket.ID, step, step, e.Now().UTC().Format("2006-01-02 15:04:05Z"), r.Outcome, r.Summary)
	if len(r.Findings) == 0 {
		b.WriteString("なし\n")
	} else {
		b.WriteString("| # | ラベル | 指摘 | 根拠（ファイル・行・ID） | 提案 | 対応 |\n| --- | --- | --- | --- | --- | --- |\n")
		for i, f := range r.Findings {
			ev := evidenceText(f.Evidence)
			if f.Evidence != nil && e.planned[f.Evidence.ID] {
				ev += "（予定）"
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | 未対応 |\n", i+1, f.Label, cellText(f.Finding), cellText(ev), cellText(f.Suggestion))
		}
	}
	b.WriteString("\nラベルの扱い：MUST と Q は遷移を止める。IMO は人のコードレビューで提示し、NR は将来の対応の候補として一覧にする。NITS・FYI は記録だけ。\n")
	path := filepath.Join(dir, fmt.Sprintf("%02d-%s.md", len(entries)+1, step))
	if err := e.writeState(path, []byte(b.String())); err != nil {
		return err
	}
	// 作業履歴から、その回のレビュー報告をたどれるようにする（M2.8）
	e.log("review-report", map[string]any{"step": step, "path": e.relPath(path)})
	return nil
}

func cellText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "), "|", `\|`)
}
