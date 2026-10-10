// Package idcheck は、カタログのIDの整合を検査する（ID整合チェック）。
//
// IDの定義の場所と見つけ方は project.json の ids で決め、設定にないプレフィックスはIDとして扱わない。
package idcheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gateflow/internal/config"
)

// Level は検査のレベル。必須は不合格なら遷移しない。推奨は止めずに記録する。
type Level int

const (
	Required Level = iota
	Recommended
)

func (l Level) String() string {
	if l == Required {
		return "必須"
	}
	return "推奨"
}

// 指摘の種類。
const (
	KindUndefined    = "undefined"     // 未定義の参照
	KindDuplicate    = "duplicate"     // 重複定義
	KindDeprecated   = "deprecated"    // 廃止されたIDの参照
	KindNameMismatch = "name-mismatch" // ID（名前）の名前が正本と違う
	KindUnreferenced = "unreferenced"  // 参照されていない定義
)

// Finding は1件の指摘。File はルートからの相対パス。
type Finding struct {
	Level   Level
	Kind    string
	ID      string
	File    string
	Line    int
	Message string
}

// Result は検査の結果。
type Result struct {
	Findings    []Finding
	Definitions int // 定義されたIDの数（子を除く）
	Children    int // 定義された子のIDの数
}

// HasErrors は、必須の指摘があるかを返す。
func (r Result) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Level == Required {
			return true
		}
	}
	return false
}

type definition struct {
	file       string
	line       int
	name       string
	deprecated bool
}

type prefixRule struct {
	cfg      config.Prefix
	re       *regexp.Regexp
	children map[string]map[string]bool // 親のID → 子の名前
}

type checker struct {
	root     string
	cfg      config.IDs
	rules    []*prefixRule
	defs     map[string]*definition
	defAt    map[string]bool // 定義の位置（file:line:off）。参照として数えない
	refs     map[string]int
	files    map[string][]string
	findings []Finding
}

// Run は、root をルートとして cfg に従って検査する。設定やファイルの読み込みの誤りは error で返す。
func Run(root string, cfg config.IDs) (Result, error) {
	c := &checker{
		root:  root,
		cfg:   cfg,
		defs:  map[string]*definition{},
		defAt: map[string]bool{},
		refs:  map[string]int{},
		files: map[string][]string{},
	}
	for _, p := range cfg.Prefixes {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return Result{}, fmt.Errorf("%s の pattern が正しくない: %w", p.Prefix, err)
		}
		c.rules = append(c.rules, &prefixRule{cfg: p, re: re})
	}
	if err := c.collectDefinitions(); err != nil {
		return Result{}, err
	}
	scan, err := c.scanFiles()
	if err != nil {
		return Result{}, err
	}
	for _, f := range scan {
		lines, err := c.read(f)
		if err != nil {
			return Result{}, err
		}
		for i, line := range lines {
			c.scanLine(f, i+1, line)
		}
	}
	c.reportUnreferenced()

	sort.SliceStable(c.findings, func(i, j int) bool {
		a, b := c.findings[i], c.findings[j]
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	res := Result{Findings: c.findings, Definitions: len(c.defs)}
	for _, r := range c.rules {
		for _, names := range r.children {
			res.Children += len(names)
		}
	}
	return res, nil
}

func (c *checker) read(rel string) ([]string, error) {
	if lines, ok := c.files[rel]; ok {
		return lines, nil
	}
	data, err := os.ReadFile(filepath.Join(c.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("%s を読めない: %w", rel, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	c.files[rel] = lines
	return lines, nil
}

func (c *checker) add(f Finding) {
	c.findings = append(c.findings, f)
}

func (c *checker) collectDefinitions() error {
	for _, r := range c.rules {
		for _, f := range r.cfg.Files {
			lines, err := c.read(f)
			if err != nil {
				return err
			}
			for _, t := range parseTables(lines) {
				c.defineFromTable(r, f, t)
			}
		}
	}
	// 子は親の定義がそろってから集める。
	for _, r := range c.rules {
		ch := r.cfg.Children
		if ch == nil {
			continue
		}
		r.children = map[string]map[string]bool{}
		for _, f := range ch.Files {
			lines, err := c.read(f)
			if err != nil {
				return err
			}
			for _, t := range parseTables(lines) {
				pc, nc := t.column(ch.ParentColumn), t.column(ch.NameColumn)
				if pc < 0 || nc < 0 {
					continue
				}
				for _, row := range t.rows {
					parent, ok1 := row.at(pc)
					name, ok2 := row.at(nc)
					if !ok1 || !ok2 || !r.fullMatch(parent.text) || isPlaceholder(name.text) {
						continue
					}
					if r.children[parent.text] == nil {
						r.children[parent.text] = map[string]bool{}
					}
					r.children[parent.text][name.text] = true
				}
			}
		}
	}
	return nil
}

func (c *checker) defineFromTable(r *prefixRule, file string, t table) {
	col := 0
	if r.cfg.IsTableColumn() {
		col = t.column(r.cfg.FindColumn())
		if col < 0 {
			return
		}
	}
	nameCol := -1
	if r.cfg.NameColumn != "" {
		nameCol = t.column(r.cfg.NameColumn)
	}
	for _, row := range t.rows {
		cl, ok := row.at(col)
		if !ok {
			continue
		}
		var id, name string
		if r.cfg.IsTableColumn() {
			// 「UC-002（アカウントを登録する）」の形。名前は括弧の中。
			loc := r.re.FindStringIndex(cl.text)
			if loc == nil || loc[0] != 0 || !boundaryAfter(cl.text, loc[1]) {
				continue
			}
			id = cl.text[:loc[1]]
			name, _ = parenAt(cl.text, loc[1])
		} else {
			if !r.fullMatch(cl.text) {
				continue
			}
			id = cl.text
		}
		if nc, ok := row.at(nameCol); ok && nameCol >= 0 {
			name = nc.text
		}
		c.defAt[posKey(file, row.line, cl.off)] = true
		if prev, dup := c.defs[id]; dup {
			c.add(Finding{Level: Required, Kind: KindDuplicate, ID: id, File: file, Line: row.line,
				Message: fmt.Sprintf("%s は %s:%d でも定義されている", id, prev.file, prev.line)})
			continue
		}
		c.defs[id] = &definition{file: file, line: row.line, name: name, deprecated: c.isDeprecated(row)}
	}
}

func (c *checker) isDeprecated(row tableRow) bool {
	if c.cfg.DeprecatedMarker == "" {
		return false
	}
	for _, cl := range row.cells {
		if strings.HasPrefix(cl.text, c.cfg.DeprecatedMarker) {
			return true
		}
	}
	return false
}

func (c *checker) scanFiles() ([]string, error) {
	set := map[string]bool{}
	for _, r := range c.rules {
		for _, f := range r.cfg.Files {
			set[f] = true
		}
	}
	for _, s := range c.cfg.Scan {
		abs := filepath.Join(c.root, filepath.FromSlash(s))
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("scan の %s がない: %w", s, err)
		}
		if !info.IsDir() {
			set[s] = true
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				rel, _ := filepath.Rel(c.root, path)
				set[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var out []string
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

func (c *checker) scanLine(file string, lineNo int, line string) {
	for _, r := range c.rules {
		for _, loc := range r.re.FindAllStringIndex(line, -1) {
			start, end := loc[0], loc[1]
			if !boundaryBefore(line, start) || !boundaryAfter(line, end) {
				continue
			}
			if c.defAt[posKey(file, lineNo, start)] {
				continue
			}
			c.recordRef(r, file, lineNo, line, line[start:end], end)
		}
	}
}

func (c *checker) recordRef(r *prefixRule, file string, lineNo int, line, id string, end int) {
	def, ok := c.defs[id]
	if !ok {
		c.add(Finding{Level: Required, Kind: KindUndefined, ID: id, File: file, Line: lineNo,
			Message: fmt.Sprintf("%s は定義されていない", id)})
		return
	}
	c.refs[id]++
	if def.deprecated {
		c.add(Finding{Level: Required, Kind: KindDeprecated, ID: id, File: file, Line: lineNo,
			Message: fmt.Sprintf("%s は廃止されている（%s:%d）", id, def.file, def.line)})
	}
	if ch := r.cfg.Children; ch != nil && strings.HasPrefix(line[end:], ch.Separator) {
		rest := line[end+len(ch.Separator):]
		if name := longestPrefix(rest, r.children[id]); name != "" {
			return // 子の参照。名前の検査は子の名前そのもので済んでいる
		}
		if token := leadingToken(rest); token != "" {
			child := id + ch.Separator + token
			c.add(Finding{Level: Required, Kind: KindUndefined, ID: child, File: file, Line: lineNo,
				Message: fmt.Sprintf("%s は定義されていない（%s の%sにない）", child, id, ch.NameColumn)})
			return
		}
	}
	if name, ok := parenAt(line, end); ok && def.name != "" && name != def.name {
		c.add(Finding{Level: Recommended, Kind: KindNameMismatch, ID: id, File: file, Line: lineNo,
			Message: fmt.Sprintf("%s の名前「%s」が正本の「%s」と違う", id, name, def.name)})
	}
}

func (c *checker) reportUnreferenced() {
	for id, def := range c.defs {
		if c.refs[id] == 0 && !def.deprecated {
			c.add(Finding{Level: Recommended, Kind: KindUnreferenced, ID: id, File: def.file, Line: def.line,
				Message: fmt.Sprintf("%s はどこからも参照されていない", id)})
		}
	}
}

func (r *prefixRule) fullMatch(s string) bool {
	loc := r.re.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}

func posKey(file string, line, off int) string {
	return fmt.Sprintf("%s:%d:%d", file, line, off)
}

// boundaryBefore は、IDの直前が英数字・下線・ハイフンでないか（別の語の一部でないか）を返す。
func boundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	b := s[i-1]
	return !(isASCIIAlnum(b) || b == '_' || b == '-')
}

// boundaryAfter は、IDの直後が英数字でないかを返す。「UC-003-AC-02」の「-」や子の区切りは許す。
func boundaryAfter(s string, i int) bool {
	return i >= len(s) || !isASCIIAlnum(s[i])
}

func isASCIIAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// parenAt は、s[i:] が全角の括弧で始まるとき、対応する閉じ括弧までの中身を返す。入れ子に対応する。
func parenAt(s string, i int) (string, bool) {
	const open, close = "（", "）"
	if !strings.HasPrefix(s[i:], open) {
		return "", false
	}
	depth := 0
	for j := i; j < len(s); {
		switch {
		case strings.HasPrefix(s[j:], open):
			depth++
			j += len(open)
		case strings.HasPrefix(s[j:], close):
			depth--
			if depth == 0 {
				return s[i+len(open) : j], true
			}
			j += len(close)
		default:
			_, size := utf8.DecodeRuneInString(s[j:])
			j += size
		}
	}
	return "", false
}

// isPlaceholder は、子の名前の列のうち定義とみなさない値（空、「—」、「（初期）」のような括弧書き）かを返す。
func isPlaceholder(s string) bool {
	return s == "" || s == "—" || s == "-" || strings.HasPrefix(s, "（")
}

func longestPrefix(s string, names map[string]bool) string {
	best := ""
	for n := range names {
		if strings.HasPrefix(s, n) && len(n) > len(best) {
			best = n
		}
	}
	return best
}

// leadingToken は、区切りの文字（空白、句読点、括弧、表の区切りなど）までの文字列を返す。
func leadingToken(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return strings.ContainsRune(" \t|`（）()、。,・[]「」:：", r)
	})
	if end < 0 {
		return s
	}
	return s[:end]
}
