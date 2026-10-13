package ticket

import (
	"fmt"
	"os"
	"strings"
)

// AcceptanceRow・TestCaseRow・SheetRow は、§4.2〜§4.4 の表の1行。
type AcceptanceRow struct{ ID, UC, Content string }
type TestCaseRow struct{ Test, Layer, Targets, Content string }
type SheetRow struct{ File, Change string }

// HostCheck は、§4.5 のホストでの確かめ。Review（ホストで動かす前のコードレビュー）を先に行い、
// 通ってから Steps を順に行う。
type HostCheck struct {
	Review []string
	Steps  []HostCheckStep
}
type HostCheckStep struct{ Action, Pass string }

// Sections は、P3 の結果から gateflow が書く§3・§4.1〜§4.5 の中身。§4.6 は人が書くので持たない。
type Sections struct {
	Current    string
	Plan       string
	Acceptance []AcceptanceRow
	TestCases  []TestCaseRow
	Sheets     []SheetRow
	HostCheck  *HostCheck // なければ §4.5 に「なし」と書く
}

// 節の見出しの始まり。中身は見出しの次の行から、次の見出しの前まで。
const (
	headCurrent    = "## §3"
	headPlan       = "### §4.1"
	headAcceptance = "### §4.2"
	headTestCases  = "### §4.3"
	headSheets     = "### §4.4"
	headHostCheck  = "### §4.5 ホストでの確かめ"
	headApproval   = "### §4.6"
	headHostResult = "## §5"
)

// noHostCheck は、ホストでの確かめがないときに §4.5 に書く文。
const noHostCheck = "なし"

// 中身が要る節。実装は §3・§4.1〜§4.4、基盤は §3・§4.1・§4.5（受入条件とテストケースは0件でよく、
// 更新する文書がないこともある。M2.7）。
var requiredSections = map[Kind][]string{
	KindImplementation: {headCurrent, headPlan, headAcceptance, headTestCases, headSheets},
	KindInfrastructure: {headCurrent, headPlan, headHostCheck},
}

// WriteSections は、§3・§4.1〜§4.5 を書き換える。ステータスの行、§1・§2、§4.6、§5 は変えない。
func (t *Ticket) WriteSections(s Sections) error {
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return err
	}
	var acc, tests, sheets strings.Builder
	acc.WriteString("| 受入条件 | ユースケース | 内容 |\n| --- | --- | --- |\n")
	for _, a := range s.Acceptance {
		fmt.Fprintf(&acc, "| %s | %s | %s |\n", cell(a.ID), cell(a.UC), cell(a.Content))
	}
	tests.WriteString("| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |\n| --- | --- | --- | --- |\n")
	for _, tc := range s.TestCases {
		fmt.Fprintf(&tests, "| %s | %s | %s | %s |\n", cell(tc.Test), cell(tc.Layer), cell(tc.Targets), cell(tc.Content))
	}
	sheets.WriteString("P5で更新してよいのは、ここに挙げたシートだけ。\n\n| ファイル | 更新の内容 |\n| --- | --- |\n")
	for _, sh := range s.Sheets {
		fmt.Fprintf(&sheets, "| `%s` | %s |\n", cell(sh.File), cell(sh.Change))
	}
	body := string(data)
	for head, content := range map[string]string{
		headCurrent:    strings.TrimSpace(s.Current) + "\n",
		headPlan:       strings.TrimSpace(s.Plan) + "\n",
		headAcceptance: acc.String(),
		headTestCases:  tests.String(),
		headSheets:     sheets.String(),
		headHostCheck:  hostCheckText(s.HostCheck),
	} {
		var ok bool
		if body, ok = replaceSection(body, head, content); !ok {
			return fmt.Errorf("指示書 %s に節 %s がない", t.ID, head)
		}
	}
	if err := writeFileAtomic(t.Path, []byte(body)); err != nil {
		return err
	}
	t.Body = body
	return nil
}

// hostCheckText は、§4.5 の本文。コードレビューの箇条書きを先に、ホストでの手順の表を後に書く。
func hostCheckText(h *HostCheck) string {
	if h == nil {
		return noHostCheck + "\n"
	}
	var b strings.Builder
	b.WriteString("**ホストで動かす前のコードレビュー**\n\n")
	for _, r := range h.Review {
		fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(strings.TrimSpace(r), "\n", " "))
	}
	b.WriteString("\n**ホストでの手順（コードレビューが通ってから、この順で行う）**\n\n| 順 | 行うこと | 合格の基準 |\n| --- | --- | --- |\n")
	for i, st := range h.Steps {
		fmt.Fprintf(&b, "| %d | %s | %s |\n", i+1, cell(st.Action), cell(st.Pass))
	}
	return b.String()
}

// cell は、表のセルに入れる文字列から改行と | を除く。
func cell(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	return strings.ReplaceAll(s, "|", "／")
}

// replaceSection は、見出し head の節の中身を content に替える。
// headRequirements は、§2（要求）の見出し。
const headRequirements = "## §2"

// WriteSection2 は、§2（要求）の本文を置き換える（ARB の裁定の反映。M2.6）。見出しの行は変えない。
// 本文に見出し（# で始まる行）があると、節の区切りが崩れるので受け付けない。
func (t *Ticket) WriteSection2(text string) error {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "#") {
			return fmt.Errorf("§2 の本文に見出しの行がある：%q", l)
		}
	}
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return err
	}
	body, ok := replaceSection(string(data), headRequirements, strings.TrimSpace(text)+"\n")
	if !ok {
		return fmt.Errorf("指示書 %s に節 %s がない", t.ID, headRequirements)
	}
	if err := writeFileAtomic(t.Path, []byte(body)); err != nil {
		return err
	}
	t.Body = body
	return nil
}

func replaceSection(body, head, content string) (string, bool) {
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, head) {
			start = i
			break
		}
	}
	if start < 0 {
		return body, false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start+1]...)
	out = append(out, "")
	out = append(out, strings.Split(strings.TrimRight(content, "\n"), "\n")...)
	out = append(out, "")
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), true
}

func sectionLines(body, head string) []string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, head) {
			continue
		}
		var out []string
		for _, s := range lines[i+1:] {
			if strings.HasPrefix(s, "#") {
				break
			}
			out = append(out, s)
		}
		return out
	}
	return nil
}

// ひな形の文言（中身とみなさない行）。
var placeholderLines = map[string]bool{
	"-": true, "- 影響の範囲（IDで検索した結果の要約）：": true, "- 関係するカタログ・ユースケースの詳細：": true,
	"P5で更新してよいのは、ここに挙げたシートだけ。": true,
	"gateflow の中で確かめられない動作を、ready-for-review の後に、人の代わりにホストで作業する AI（gateflow の外）が確かめる。ホストで動かす前に、コードレビューでホストに影響する設定を確かめる。結果は §5 に書く。": true,
	"§4.5 があるときだけ書く。§4.5 があるのに空なら、`gateflow done`できない。": true,
}

// MissingSections は、種別ごとに中身が要る節のうち、ひな形のまま（中身がない）節の見出しを返す。
// 基盤の §4.5 は「なし」も中身がないとみなす。
func (t *Ticket) MissingSections() []string {
	var missing []string
	for _, head := range requiredSections[t.Kind] {
		ok := hasContent(sectionLines(t.Body, head))
		if head == headHostCheck {
			ok = t.HasHostCheck()
		}
		if !ok {
			missing = append(missing, head)
		}
	}
	return missing
}

// HasHostCheck は、§4.5 にホストでの確かめ（「なし」でない中身）があるかを返す。
func (t *Ticket) HasHostCheck() bool {
	lines := sectionLines(t.Body, headHostCheck)
	return hasContent(lines) && strings.TrimSpace(strings.Join(lines, "\n")) != noHostCheck
}

// HostCheckRecorded は、§5（ホストでの確かめの結果）に中身があるかを返す。
func (t *Ticket) HostCheckRecorded() bool {
	return hasContent(sectionLines(t.Body, headHostResult))
}

// TestCaseCount は、§4.3 のテストケースの表のデータの行の数を返す（ひな形の行は数えない）。
func (t *Ticket) TestCaseCount() int {
	n := 0
	for _, l := range sectionLines(t.Body, headTestCases) {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "|") && !isHeaderRow(s) && hasContent([]string{s}) {
			n++
		}
	}
	return n
}

func hasContent(lines []string) bool {
	for _, l := range lines {
		s := strings.TrimSpace(l)
		switch {
		case s == "" || placeholderLines[s]:
		case strings.HasPrefix(s, "|"):
			cells := strings.Split(strings.Trim(s, "|"), "|")
			filled := false
			for _, c := range cells {
				c = strings.TrimSpace(c)
				if c != "" && !strings.HasPrefix(c, "---") && !strings.Contains(c, "NNN") {
					filled = true
				}
			}
			// 見出しの行（項目名だけ）は中身とみなさないので、2行目以降のデータの行で判断する
			if filled && !isHeaderRow(s) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func isHeaderRow(s string) bool {
	for _, h := range []string{"| 受入条件 | ユースケース |", "| テスト | 層", "| ファイル | 更新の内容 |", "| 順 | 行うこと |"} {
		if strings.HasPrefix(s, h) {
			return true
		}
	}
	return false
}

// ApprovalChecked は、§4.6 のチェックが1つ以上あり、すべて入っているかを返す。
func (t *Ticket) ApprovalChecked() bool {
	n := 0
	for _, l := range sectionLines(t.Body, headApproval) {
		s := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(s, "- [ ]"):
			return false
		case strings.HasPrefix(s, "- [x]"), strings.HasPrefix(s, "- [X]"):
			n++
		}
	}
	return n > 0
}

// Held は、保留の印（項目の表の「保留」が「はい」）があるかを返す。
func (t *Ticket) Held() bool {
	for _, l := range strings.Split(t.Body, "\n") {
		if v, ok := tableValue(l, "保留"); ok && v == "はい" {
			return true
		}
	}
	return false
}

// SetHold は、保留の印を付ける（on）か外す。印は項目の表の「ブランチ」の行の次に置く。
func (t *Ticket) SetHold(on bool) error {
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return err
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if _, ok := tableValue(l, "保留"); ok {
			continue
		}
		out = append(out, l)
		if _, ok := tableValue(l, "ブランチ"); ok && on {
			out = append(out, "| 保留 | はい |")
		}
	}
	body := strings.Join(out, "\n")
	if err := writeFileAtomic(t.Path, []byte(body)); err != nil {
		return err
	}
	t.Body = body
	return nil
}

// TargetIDs は、項目の表の「対象ID」の欄の ID を返す。「、」やカンマで区切り、「（名前）」は除く。なければ空。
func (t *Ticket) TargetIDs() []string {
	for _, l := range strings.Split(t.Body, "\n") {
		v, ok := tableValue(l, "対象ID")
		if !ok {
			continue
		}
		var ids []string
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == '、' || r == ',' || r == '，' || r == ' ' }) {
			if i := strings.IndexAny(f, "（("); i >= 0 {
				f = f[:i]
			}
			if f = strings.TrimSpace(f); f != "" {
				ids = append(ids, f)
			}
		}
		return ids
	}
	return nil
}

// IssueID は、項目の表の「Issue」の欄（外部の課題の番号。人が書く）を返す。なければ空。
func (t *Ticket) IssueID() string {
	for _, l := range strings.Split(t.Body, "\n") {
		if v, ok := tableValue(l, "Issue"); ok {
			return v
		}
	}
	return ""
}
