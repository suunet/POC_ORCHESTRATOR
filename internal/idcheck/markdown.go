package idcheck

import (
	"regexp"
	"strings"
)

// cell は表の1つのセル。off は、行の中でトリムした文字列が始まる位置（バイト）。
type cell struct {
	text string
	off  int
}

type tableRow struct {
	line  int // 1始まり
	cells []cell
}

type table struct {
	header []string
	rows   []tableRow
}

// column は、見出しが name の列の番号を返す。なければ -1。
func (t table) column(name string) int {
	for i, h := range t.header {
		if h == name {
			return i
		}
	}
	return -1
}

func (r tableRow) at(i int) (cell, bool) {
	if i < 0 || i >= len(r.cells) {
		return cell{}, false
	}
	return r.cells[i], true
}

var separatorCell = regexp.MustCompile(`^:?-+:?$`)

// parseTables は、Markdownの行から表を取り出す。コードブロックの中は表として扱わない。
func parseTables(lines []string) []table {
	var tables []table
	var cur *table
	inFence := false
	flush := func() {
		if cur != nil {
			tables = append(tables, *cur)
			cur = nil
		}
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// NOTE: ``` と ~~~ を区別せずに開閉を切り替える。``` で開いたコードブロックを ~~~ でも閉じられる
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			flush()
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(trimmed, "|") {
			flush()
			continue
		}
		cells := splitRow(line)
		if cur == nil {
			// 見出しの行の次が区切りの行のときだけ表とみなす。
			if i+1 >= len(lines) || !isSeparator(splitRow(lines[i+1])) {
				continue
			}
			cur = &table{}
			for _, c := range cells {
				cur.header = append(cur.header, c.text)
			}
			continue
		}
		if isSeparator(cells) && len(cur.rows) == 0 {
			continue
		}
		cur.rows = append(cur.rows, tableRow{line: i + 1, cells: cells})
	}
	flush()
	return tables
}

func isSeparator(cells []cell) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !separatorCell.MatchString(c.text) {
			return false
		}
	}
	return true
}

// splitRow は、表の1行をセルに分ける。バッククォートの中の | は区切りとみなさない。
//
// NOTE: バッククォートが奇数個で閉じていないと、それ以降の | は行の終わりまで区切りとみなさない。
func splitRow(line string) []cell {
	start := strings.Index(line, "|")
	if start < 0 {
		return nil
	}
	var cells []cell
	inCode := false
	begin := start + 1
	for i := begin; i < len(line); i++ {
		switch line[i] {
		case '`':
			inCode = !inCode
		case '\\':
			i++ // エスケープされた文字は飛ばす
		case '|':
			if !inCode {
				cells = append(cells, trimCell(line, begin, i))
				begin = i + 1
			}
		}
	}
	// 行末の | の後ろに文字が残っていれば、それも1つのセルとする。
	if rest := strings.TrimSpace(line[begin:]); rest != "" {
		cells = append(cells, trimCell(line, begin, len(line)))
	}
	return cells
}

func trimCell(line string, from, to int) cell {
	raw := line[from:to]
	lead := len(raw) - len(strings.TrimLeft(raw, " \t"))
	return cell{text: strings.TrimSpace(raw), off: from + lead}
}
