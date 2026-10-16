// Package idcheck は、カタログのIDの整合を検査する（ID整合チェック）。
//
// IDの定義の場所と見つけ方は project.json の ids で決め、設定にないプレフィックスはIDとして扱わない。
package idcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	KindTestMissing  = "test-missing"  // 受入条件の ID を持つテストがない（M3）
	KindUnverified   = "unverified"    // 確かめ方のない非機能要求（M3）
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

	defined map[string]bool // 定義された ID（子は「親の ID＋区切り＋名前」）
	where   map[string]Location
	parent  map[string]string // 子の ID → 親の ID
}

// Location は、ID の定義の場所（リポジトリのルートからの相対パスと、1から数えた行）。
type Location struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Where は、ID の定義の場所を返す（M2.8。監視の UI の正本に使う）。子の ID は親の定義の場所を返す。
func (r Result) Where(id string) (Location, bool) {
	if l, ok := r.where[id]; ok {
		return l, true
	}
	l, ok := r.where[r.parent[id]]
	return l, ok
}

// Defined は、id が定義されているか（子の ID を含む）を返す。
func (r Result) Defined(id string) bool { return r.defined[id] }

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
	rule       *prefixRule
	method     string // 確認の方法（MethodColumn）
}

// Rules は、検査に使える状態に検証した ID の設定。NewRules からだけ作れ、作った後は変わらない。
type Rules struct {
	cfg      config.IDs
	prefixes []*prefixRule
}

type prefixRule struct {
	cfg config.Prefix
	re  *regexp.Regexp
}

// NewRules は、cfg の各プレフィックスの正規表現をコンパイルして Rules を作る。
func NewRules(cfg config.IDs) (*Rules, error) {
	rules := &Rules{cfg: cfg}
	for _, p := range cfg.Prefixes {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("%s の pattern が正しくない: %w", p.Prefix, err)
		}
		rules.prefixes = append(rules.prefixes, &prefixRule{cfg: p, re: re})
	}
	return rules, nil
}

// Matches は、id が設定のどれかのプレフィックスの形式に、全体で一致するかを返す。
func (r *Rules) Matches(id string) bool {
	for _, p := range r.prefixes {
		if p.fullMatch(id) {
			return true
		}
	}
	return false
}

// FindIDs は、text に出てくる ID（設定のプレフィックスの形式に合い、前後が別の語の一部でないもの）を、出てきた順に重ねずに返す。
func (r *Rules) FindIDs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range r.prefixes {
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			if !boundaryBefore(text, loc[0]) || !boundaryAfter(text, loc[1]) {
				continue
			}
			if id := text[loc[0]:loc[1]]; !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

type checker struct {
	root     string
	cfg      config.IDs
	rules    []*prefixRule
	children map[string]map[string]bool // 親のID → 子の名前
	defs     map[string]*definition
	// defAt は定義のセルの位置。defineFromTable が書き、scanLine が読んで、定義そのものを参照として数えないようにする。
	defAt    map[pos]bool
	refs     map[string]int
	files    map[string][]string
	findings []Finding
}

// Options は、検査の選び方。
type Options struct {
	// SkipTestMissing は、受入条件の ID を持つテストの検査を行わない（P3 の直後。テストは P5 で書く。M3 の Q-03）
	SkipTestMissing bool
}

// Run は、root をルートとして rules に従って検査する。ファイルの読み込みの誤りは error で返す。
func Run(root string, rules *Rules) (Result, error) { return RunWith(root, rules, Options{}) }

// RunWith は、opts に従って検査する。
func RunWith(root string, rules *Rules, opts Options) (Result, error) {
	c := &checker{
		root:     root,
		cfg:      rules.cfg,
		rules:    rules.prefixes,
		children: map[string]map[string]bool{},
		defs:     map[string]*definition{},
		defAt:    map[pos]bool{},
		refs:     map[string]int{},
		files:    map[string][]string{},
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
	if !opts.SkipTestMissing {
		if err := c.reportTestMissing(); err != nil {
			return Result{}, err
		}
	}

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
	res := Result{Findings: c.findings, Definitions: len(c.defs), defined: map[string]bool{}, where: map[string]Location{}, parent: map[string]string{}}
	for id, def := range c.defs {
		res.defined[id] = true
		res.where[id] = Location{File: def.file, Line: def.line}
	}
	for _, r := range c.rules {
		if r.cfg.Children == nil {
			continue
		}
		for parent, names := range c.children {
			if !r.fullMatch(parent) {
				continue
			}
			for name := range names {
				res.defined[parent+r.cfg.Children.Separator+name] = true
				res.parent[parent+r.cfg.Children.Separator+name] = parent
			}
		}
	}
	for _, names := range c.children {
		res.Children += len(names)
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
		files, err := c.expand(r.cfg.Files)
		if err != nil {
			return err
		}
		for _, f := range files {
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
					if c.children[parent.text] == nil {
						c.children[parent.text] = map[string]bool{}
					}
					c.children[parent.text][name.text] = true
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
	methodCol, verifyCol := -1, -1
	if r.cfg.MethodColumn != "" {
		methodCol = t.column(r.cfg.MethodColumn)
	}
	if r.cfg.VerifyColumn != "" {
		verifyCol = t.column(r.cfg.VerifyColumn)
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
		c.defAt[pos{file: file, line: row.line, off: cl.off}] = true
		if prev, dup := c.defs[id]; dup {
			c.add(Finding{Level: Required, Kind: KindDuplicate, ID: id, File: file, Line: row.line,
				Message: fmt.Sprintf("%s は %s:%d でも定義されている", id, prev.file, prev.line)})
			continue
		}
		c.defs[id] = &definition{file: file, line: row.line, name: name, deprecated: c.isDeprecated(row), rule: r}
		if m, ok := row.at(methodCol); ok && methodCol >= 0 {
			c.defs[id].method = m.text
		}
		if verifyCol >= 0 {
			if v, ok := row.at(verifyCol); !ok || isNoVerify(v.text) {
				c.add(Finding{Level: Recommended, Kind: KindUnverified, ID: id, File: file, Line: row.line,
					Message: fmt.Sprintf("%s に確かめ方がない", id)})
			}
		}
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
		files, err := c.expand(r.cfg.Files)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
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
			if c.defAt[pos{file: file, line: lineNo, off: start}] {
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
	// NOTE: 検査の順序が仕様。子の参照なら、括弧を名前とみなす検査（最後）は行わない。
	if c.checkChildRef(r, file, lineNo, line, id, end) {
		return
	}
	if name, ok := parenAt(line, end); ok && def.name != "" && name != def.name {
		c.add(Finding{Level: Recommended, Kind: KindNameMismatch, ID: id, File: file, Line: lineNo,
			Message: fmt.Sprintf("%s の名前「%s」が正本の「%s」と違う", id, name, def.name)})
	}
}

// checkChildRef は、line[end:] が子の区切りで始まるとき、子の参照として扱って true を返す。
// 定義済みの子の名前のうち最も長く一致するものをとり、なければ未定義の子として指摘する。
// 区切りの後に名前がないとき（文末の「.」など）は、子の参照とみなさない。
func (c *checker) checkChildRef(r *prefixRule, file string, lineNo int, line, id string, end int) bool {
	ch := r.cfg.Children
	if ch == nil || !strings.HasPrefix(line[end:], ch.Separator) {
		return false
	}
	rest := line[end+len(ch.Separator):]
	if longestPrefix(rest, c.children[id]) != "" {
		return true
	}
	token := leadingToken(rest)
	if token == "" {
		return false
	}
	child := id + ch.Separator + token
	c.add(Finding{Level: Required, Kind: KindUndefined, ID: child, File: file, Line: lineNo,
		Message: fmt.Sprintf("%s は定義されていない（%s の%sにない）", child, id, ch.NameColumn)})
	return true
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

// pos は、ファイルの中の位置（行は1始まり、off は行の中のバイトの位置）。
type pos struct {
	file      string
	line, off int
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

// isNoVerify は、確かめ方のセルが空か「—」か（「（案）」は確かめ方があるとみなす）。
func isNoVerify(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || s == "—" || s == "-" || s == "－"
}

// expand は、ファイルの一覧のグロブ（例：.docs/design/uc/*.md）を、ルートからの相対パスに展開する（M3）。
// グロブでないものはそのまま返す（ないファイルは読むときに誤りになる）。グロブに当たるものがなければ何も返さない。
func (c *checker) expand(files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		if !strings.ContainsAny(f, "*?[") {
			out = append(out, f)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(c.root, filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("files のグロブ %s が正しくない: %w", f, err)
		}
		sort.Strings(matches)
		for _, m := range matches {
			rel, err := filepath.Rel(c.root, m)
			if err != nil {
				return nil, err
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out, nil
}

// reportTestMissing は、確認の方法がテストの受入条件のうち、テストの置き場のファイルに ID が1つも出てこないものを
// 必須の指摘にする（テストの名前かコメントに受入条件の ID を入れる規則。M3）。テストの置き場の設定がなければ行わない。
func (c *checker) reportTestMissing() error {
	if len(c.cfg.Tests) == 0 {
		return nil
	}
	found := map[string]bool{}
	for _, dir := range c.cfg.Tests {
		abs := filepath.Join(c.root, filepath.FromSlash(dir))
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			continue // テストがまだない
		}
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, r := range c.rules {
				if r.cfg.MethodColumn == "" {
					continue
				}
				text := string(data)
				for _, loc := range r.re.FindAllStringIndex(text, -1) {
					if boundaryBefore(text, loc[0]) && boundaryAfter(text, loc[1]) {
						found[text[loc[0]:loc[1]]] = true
					}
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("テストの置き場 %s を読めない: %w", dir, err)
		}
	}
	methods := c.cfg.Methods()
	ids := make([]string, 0, len(c.defs))
	for id := range c.defs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		def := c.defs[id]
		if def.rule == nil || def.rule.cfg.MethodColumn == "" || def.deprecated || found[id] {
			continue
		}
		if def.method != "" && !slices.Contains(methods, strings.TrimSpace(def.method)) {
			continue // UAT・手動など、テストで確かめない受入条件（M3 の Q-02）
		}
		c.add(Finding{Level: Required, Kind: KindTestMissing, ID: id, File: def.file, Line: def.line,
			Message: fmt.Sprintf("%s を確かめるテストがない（テストの名前かコメントに %s を入れる）", id, id)})
	}
	return nil
}
