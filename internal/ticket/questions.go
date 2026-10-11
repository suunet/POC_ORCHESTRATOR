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

// NewQuestion は、gateflow が質疑に足す1件（P2 の指摘や P3 の質問から作る）。
type NewQuestion struct {
	Summary    string
	Body       string
	Mark       string // 例：指示書の修正が必要（P2 の MUST）
	Evidence   string
	Suggestion string
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
	return n, nil
}

// AppendQuestions は、qs を未回答の質疑として足す。番号は続きから振り、人の書いた行は変えない。
func (t *Ticket) AppendQuestions(source string, qs []NewQuestion) error {
	path := t.questionsPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data = []byte(fmt.Sprintf("# %s 質疑\n\n合意はここに書いて確定させる。未確定（状態が「確定」でない）の質問が残っている間は、`gateflow ready`で次へ進めない。回答したら状態を「回答済み」にし、受け入れて反映先へ反映したら「確定」にする。\n\n## 一覧\n\n| # | 質問（要約） | 回答者 | 状態 | 反映先 |\n| --- | --- | --- | --- | --- |\n", t.ID))
	} else if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
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
		return fmt.Errorf("質疑のファイル %s に一覧の表がない", path)
	}
	var rows, sections []string
	for i, q := range qs {
		id := fmt.Sprintf("Q-%02d", max+i+1)
		rows = append(rows, fmt.Sprintf("| %s | %s | 人 | 未回答 | |", id, cell(q.Summary)))
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
		b.WriteString("\n**回答**\n\n\n**受け入れ**\n\n- 判断：確定 ／ 追加の質問あり\n- 反映先と反映した内容：\n")
		sections = append(sections, b.String())
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, rows...)
	out = append(out, lines[last+1:]...)
	body := strings.Join(out, "\n") + "\n" + strings.Join(sections, "")
	return writeFileAtomic(path, []byte(body))
}
