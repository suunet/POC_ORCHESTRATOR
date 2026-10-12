package ticket

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// QuestionsFile は、指示書の隣の質疑のファイル名。
const QuestionsFile = "questions.md"

// 質疑の状態。「確定」だけが確定で、受け入れ（回答済み → 確定）は人が行う。
const resolved = "確定"

var questionRow = regexp.MustCompile(`^\| Q-([0-9]{2,}) \|`)

// NewQuestion は、gateflow が質疑に足す1件（P2 の指摘や P3 の質問、ARB の裁定から作る）。
// Answer があれば、決定者（ARB）の裁定として状態を「確定」で書く。なければ未回答で、決定者は人。
type NewQuestion struct {
	Summary    string
	Body       string
	Mark       string // 例：指示書の修正が必要（P2 の MUST）
	Evidence   string
	Suggestion string
	Decider    string // 人 か ARB（空なら人）
	Basis      string // 決定の根拠（ARB の裁定のとき）
	Answer     string // ARB の裁定の文
	Reflected  string // 反映先（ARB の裁定のとき）
}

// 質疑の一覧の表の見出し（M2.6 で決定者と根拠の列を足した）。
const questionHeader = "| # | 質問（要約） | 回答者 | 状態 | 反映先 | 決定者 | 根拠 |\n| --- | --- | --- | --- | --- | --- | --- |\n"

// QuestionInfo は、質疑の一覧の表の1行の状態と決定者。
type QuestionInfo struct {
	State   string
	Decider string
}

// QuestionStates は、質疑のファイルの中身から、Q 番号ごとの状態と決定者を返す（決定者の列がなければ人）。
func QuestionStates(data []byte) map[string]QuestionInfo {
	out := map[string]QuestionInfo{}
	stateCol, deciderCol := -1, -1
	for _, l := range strings.Split(string(data), "\n") {
		cells := splitCells(l)
		if len(cells) > 1 && cells[0] == "#" {
			for i, c := range cells {
				switch c {
				case "状態":
					stateCol = i
				case "決定者":
					deciderCol = i
				}
			}
			continue
		}
		if !questionRow.MatchString(strings.TrimSpace(l)) || stateCol < 0 || stateCol >= len(cells) {
			continue
		}
		info := QuestionInfo{State: cells[stateCol], Decider: "人"}
		if deciderCol >= 0 && deciderCol < len(cells) && cells[deciderCol] != "" {
			info.Decider = cells[deciderCol]
		}
		out[cells[0]] = info
	}
	return out
}

func splitCells(l string) []string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func (t *Ticket) questionsPath() string {
	return filepath.Join(filepath.Dir(t.Path), QuestionsFile)
}

// UnresolvedQuestions は、一覧の表の状態が「確定」でない質疑の数を返す。ファイルがなければ0。
func (t *Ticket) UnresolvedQuestions() (int, error) {
	data, err := os.ReadFile(t.questionsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return CountUnresolved(data), nil
}

// CountUnresolved は、質疑のファイルの中身から、一覧の表の状態が「確定」でない質疑の数を返す。
func CountUnresolved(data []byte) int {
	statusCol := -1
	n := 0
	for _, l := range strings.Split(string(data), "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(l), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) > 1 && cells[0] == "#" {
			for i, c := range cells {
				if c == "状態" {
					statusCol = i
				}
			}
			continue
		}
		if questionRow.MatchString(strings.TrimSpace(l)) && statusCol >= 0 && statusCol < len(cells) && cells[statusCol] != resolved {
			n++
		}
	}
	return n
}

// AppendQuestions は、qs を未回答の質疑として足す。番号は続きから振り、人の書いた行は変えない。
func (t *Ticket) AppendQuestions(source string, qs []NewQuestion) ([]string, error) {
	path := t.questionsPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data = []byte(fmt.Sprintf("# %s 質疑\n\n合意はここに書いて確定させる。未確定（状態が「確定」でない）の質問が残っている間は、`gateflow ready`で次へ進めない。回答したら状態を「回答済み」にし、受け入れて反映先へ反映したら「確定」にする。決定者が ARB の行は、ARB が裁定して確定にしたもの（人は承認の前に確かめる）。\n\n## 一覧\n\n%s", t.ID, questionHeader))
	} else if err != nil {
		return nil, err
	}
	lines := upgradeQuestionTable(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
	last, max := -1, 0
	for i, l := range lines {
		if m := questionRow.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			last = i
			var n int
			fmt.Sscanf(m[1], "%d", &n)
			if n > max {
				max = n
			}
		}
	}
	if last < 0 {
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "| --- |") {
				last = i
			}
		}
	}
	if last < 0 {
		return nil, fmt.Errorf("質疑のファイル %s に一覧の表がない", path)
	}
	var rows, sections, ids []string
	for i, q := range qs {
		id := fmt.Sprintf("Q-%02d", max+i+1)
		ids = append(ids, id)
		decider, state, reflected := "人", "未回答", ""
		if q.Answer != "" {
			decider, state, reflected = q.Decider, resolved, q.Reflected
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |", id, cell(q.Summary), decider, state, cell(reflected), decider, cell(q.Basis)))
		var b strings.Builder
		fmt.Fprintf(&b, "\n---\n\n## %s %s\n\n出どころ：%s", id, q.Summary, source)
		if q.Mark != "" {
			fmt.Fprintf(&b, "。印：**%s**", q.Mark)
		}
		fmt.Fprintf(&b, "\n\n**質問**\n\n%s\n", strings.TrimSpace(q.Body))
		if q.Evidence != "" {
			fmt.Fprintf(&b, "\n根拠：%s\n", q.Evidence)
		}
		if q.Suggestion != "" {
			fmt.Fprintf(&b, "\n提案：%s\n", q.Suggestion)
		}
		if q.Answer != "" {
			fmt.Fprintf(&b, "\n**回答**\n\n%s（決定者：%s。根拠：%s）\n\n**受け入れ**\n\n- 判断：確定（決定者：%s。人は承認の前に確かめる）\n- 反映先と反映した内容：%s\n",
				strings.TrimSpace(q.Answer), decider, q.Basis, decider, reflected)
		} else {
			b.WriteString("\n**回答**\n\n\n**受け入れ**\n\n- 判断：確定 ／ 追加の質問あり\n- 反映先と反映した内容：\n")
		}
		sections = append(sections, b.String())
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, rows...)
	out = append(out, lines[last+1:]...)
	body := strings.Join(out, "\n") + "\n" + strings.Join(sections, "")
	return ids, writeFileAtomic(path, []byte(body))
}

// upgradeQuestionTable は、決定者と根拠の列がない一覧の表（M2.6 より前）に2列を足す。既存の行の決定者は人。
func upgradeQuestionTable(lines []string) []string {
	head := -1
	for i, l := range lines {
		cells := splitCells(l)
		if len(cells) > 1 && cells[0] == "#" && strings.HasPrefix(strings.TrimSpace(l), "|") {
			head = i
			break
		}
	}
	if head < 0 || strings.Contains(lines[head], "決定者") {
		return lines
	}
	out := append([]string{}, lines...)
	out[head] = strings.TrimRight(out[head], " ") + " 決定者 | 根拠 |"
	if head+1 < len(out) && strings.HasPrefix(strings.TrimSpace(out[head+1]), "| ---") {
		out[head+1] = strings.TrimRight(out[head+1], " ") + " --- | --- |"
	}
	for i := head + 2; i < len(out); i++ {
		if !questionRow.MatchString(strings.TrimSpace(out[i])) {
			if !strings.HasPrefix(strings.TrimSpace(out[i]), "|") {
				break
			}
			continue
		}
		out[i] = strings.TrimRight(out[i], " ") + " 人 | |"
	}
	return out
}
