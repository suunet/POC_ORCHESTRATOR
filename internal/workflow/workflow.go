// Package workflow は、遷移表（JSON の定義）の読み込みと検証を扱う。
//
// 遷移表は、人の遷移（関門のコマンド）、自動の開始（その状態で gateflow が始める step）、
// step（担当と、outcome ごとの行き先）を持つ。機械検証と操作は名前で参照し、中身は gateflow のコードが持つ。
// 誤り（outcome の漏れを含む）は、実行時でなく読み込み時に拒む。
package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
)

// Workflow は、遷移表。
type Workflow struct {
	Name     string          `json:"name"`
	States   []string        `json:"states"`   // 先頭が初期の状態
	Terminal []string        `json:"terminal"` // そこで流れが終わる状態（その後は M4 以降の関門）
	Human    []Human         `json:"human"`
	Auto     []Auto          `json:"auto"`
	Steps    map[string]Step `json:"steps"`
	Display  *Display        `json:"display,omitempty"` // 監視の UI の段と表示名（M2.8）
}

// Human は、人が関門のコマンドで行う遷移。Guards は遷移の前に確かめる機械検証の名前。
type Human struct {
	Command string   `json:"command"`
	From    []string `json:"from"`
	To      string   `json:"to"`
	Guards  []string `json:"guards,omitempty"`
}

// Auto は、状態 From のチケットを gateflow が進めるときの開始。To があれば、始める前にその状態へ移す。
type Auto struct {
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
	Start string `json:"start"`
}

// Step は、エージェント（Agent：dev か bj）か機械の検査（Check）の1つ。On は outcome ごとの行き先。
type Step struct {
	Agent string           `json:"agent,omitempty"`
	Check string           `json:"check,omitempty"`
	On    map[string]Route `json:"on"`
	// SkipIf と SkipTo：条件が成り立てば、この step を飛ばして SkipTo の step から続ける（M3）。
	// 条件は no-test-cases（指示書の §4.3 のテストケースが0件）だけ
	SkipIf string `json:"skipIf,omitempty"`
	SkipTo string `json:"skipTo,omitempty"`
}

// Route は、outcome の行き先。Actions を順に行ってから、To・Next・Loop・Stop のどれか1つへ進む。
type Route struct {
	Actions []string `json:"actions,omitempty"`
	To      string   `json:"to,omitempty"`      // 状態を移して、この実行を終える
	Next    string   `json:"next,omitempty"`    // 次の step
	Loop    string   `json:"loop,omitempty"`    // 戻る step（差し戻し。Counter で往復を数える）
	Counter string   `json:"counter,omitempty"` // ループの往復を数える名前
	Stop    string   `json:"stop,omitempty"`    // human：人の確認待ちで止まる
}

// stepOutcomes は、エージェントの step ごとのあり得る outcome（出力の構造体の選択肢と同じ）。
var stepOutcomes = map[string][]string{
	"p2-review":    {"approved", "needs_fix"},
	"p2-arbitrate": {"resolved", "escalated"},
	"p3-prepare":   {"done", "needs_answer"},
	"p5-test":      {"done", "test_defect", "blocked"},
	"p5-implement": {"done", "test_defect", "blocked"},
	"p5-verify":    {"approved", "needs_fix", "needs_human"},
}

// checkOutcomes は、機械の検査の step のあり得る outcome。
var checkOutcomes = map[string][]string{
	"verify-commands": {"passed", "failed"},
	"tests-fail":      {"red", "green"}, // 新しいテストが失敗する（red）か、通ってしまう（green）か（M3）
}

// SkipNoTestCases は、指示書の §4.3 のテストケースが0件という step を飛ばす条件（M3）。
const SkipNoTestCases = "no-test-cases"

// KnownGuards は、人の遷移の前に確かめる機械検証の名前（中身は gateflow のコード）。
var KnownGuards = []string{"questions-resolved", "approval-checked", "branch-merged", "host-checked", "ids-clean"}

// KnownActions は、outcome の行き先で行う操作の名前（中身は gateflow のコード）。
var KnownActions = []string{
	"findings-to-questions", "questions-from-output", "write-sections",
	"check-sections", "check-acceptance", "check-ids", "check-main-ids", "recommended-report", "commit", "import",
	"commit-tests", "check-tests-unchanged", "check-touch-docs",
	"arb-answers", "arb-sections", "arb-escalate", "arb-report",
}

// Outcomes は、step のあり得る outcome を返す。
func (s Step) Outcomes(name string) []string {
	if s.Check != "" {
		return checkOutcomes[s.Check]
	}
	return stepOutcomes[name]
}

// LoadBuiltin は、組み込みの遷移表 workflows/<name>.json を読み込んで検証する。
func LoadBuiltin(fsys fs.FS, name string) (*Workflow, error) {
	data, err := fs.ReadFile(fsys, "workflows/"+name+".json")
	if err != nil {
		return nil, fmt.Errorf("遷移表 %s を読めない: %w", name, err)
	}
	return Parse(data)
}

// Parse は、遷移表を読み込んで検証する。誤りはまとめて返す。
func Parse(data []byte) (*Workflow, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w Workflow
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("遷移表の形式が正しくない: %w", err)
	}
	if errs := w.validate(); len(errs) > 0 {
		return nil, fmt.Errorf("遷移表 %s が正しくない:\n%w", w.Name, errors.Join(errs...))
	}
	return &w, nil
}

// HumanCommand は、状態 from で使える関門のコマンドを返す。
func (w *Workflow) HumanCommand(command, from string) (Human, bool) {
	for _, h := range w.Human {
		if h.Command == command && slices.Contains(h.From, from) {
			return h, true
		}
	}
	return Human{}, false
}

// AutoFrom は、状態 from で gateflow が始める step を返す。
func (w *Workflow) AutoFrom(from string) (Auto, bool) {
	for _, a := range w.Auto {
		if a.From == from {
			return a, true
		}
	}
	return Auto{}, false
}

func (w *Workflow) validate() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if w.Name == "" {
		add("name がない")
	}
	if len(w.States) == 0 {
		add("states がない")
		return errs
	}
	state := map[string]bool{}
	for _, s := range w.States {
		state[s] = true
	}
	checkState := func(where, s string) {
		if !state[s] {
			add("%s：状態 %q は states にない", where, s)
		}
	}
	for _, s := range w.Terminal {
		checkState("terminal", s)
	}

	commands := map[string]bool{}
	for _, h := range w.Human {
		where := "human " + h.Command
		if commands[h.Command] {
			add("%s：関門のコマンドが重複している", where)
		}
		commands[h.Command] = true
		for _, f := range h.From {
			checkState(where, f)
		}
		checkState(where, h.To)
		for _, g := range h.Guards {
			if !slices.Contains(KnownGuards, g) {
				add("%s：機械検証 %q は知らない", where, g)
			}
		}
	}
	for _, a := range w.Auto {
		where := "auto " + a.From
		checkState(where, a.From)
		if a.To != "" {
			checkState(where, a.To)
		}
		if _, ok := w.Steps[a.Start]; !ok {
			add("%s：開始の step %q がない", where, a.Start)
		}
	}

	names := make([]string, 0, len(w.Steps))
	for n := range w.Steps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := w.Steps[n]
		where := "step " + n
		switch {
		case st.Agent != "" && st.Check != "":
			add("%s：agent と check の両方がある", where)
		case st.Agent != "" && st.Agent != "dev" && st.Agent != "bj" && st.Agent != "arb":
			add("%s：担当 %q は dev・bj・arb のどれでもない", where, st.Agent)
		case st.Check != "" && checkOutcomes[st.Check] == nil:
			add("%s：検査 %q は知らない", where, st.Check)
		case st.Agent == "" && st.Check == "":
			add("%s：agent も check もない", where)
		}
		outcomes := st.Outcomes(n)
		if st.Agent != "" && outcomes == nil {
			add("%s：この step の出力の形が gateflow にない", where)
		}
		// Q-01：あり得る outcome すべてに行き先が要る
		for _, o := range outcomes {
			if _, ok := st.On[o]; !ok {
				add("%s：outcome %q の行き先がない", where, o)
			}
		}
		if st.SkipIf != "" || st.SkipTo != "" {
			if st.SkipIf != SkipNoTestCases {
				add("%s：skipIf %q は知らない（%s だけ）", where, st.SkipIf, SkipNoTestCases)
			}
			if _, ok := w.Steps[st.SkipTo]; !ok || st.SkipTo == n {
				add("%s：skipTo の step %q がない", where, st.SkipTo)
			}
		}
		for o, r := range st.On {
			rw := fmt.Sprintf("%s の outcome %q", where, o)
			if outcomes != nil && !slices.Contains(outcomes, o) {
				add("%s：この step にない outcome", rw)
			}
			errs = append(errs, w.validateRoute(rw, r, state)...)
		}
	}

	errs = append(errs, w.validateDisplay()...)

	// 初期の状態から到達できない状態を拒む
	reach := w.reachable()
	for _, s := range w.States {
		if !reach[s] {
			add("状態 %q は初期の状態から到達できない", s)
		}
	}
	return errs
}

func (w *Workflow) validateRoute(where string, r Route, state map[string]bool) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	n := 0
	for _, v := range []string{r.To, r.Next, r.Loop, r.Stop} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		add("%s：行き先（to・next・loop・stop）がちょうど1つでない", where)
	}
	if r.To != "" && !state[r.To] {
		add("%s：状態 %q は states にない", where, r.To)
	}
	for _, s := range []string{r.Next, r.Loop} {
		if s != "" {
			if _, ok := w.Steps[s]; !ok {
				add("%s：step %q がない", where, s)
			}
		}
	}
	if r.Loop != "" && r.Counter == "" {
		add("%s：ループに counter（往復を数える名前）がない", where)
	}
	if r.Stop != "" && r.Stop != "human" {
		add("%s：stop %q は human でない", where, r.Stop)
	}
	for _, a := range r.Actions {
		if !slices.Contains(KnownActions, a) {
			add("%s：操作 %q は知らない", where, a)
		}
	}
	return errs
}

func (w *Workflow) reachable() map[string]bool {
	reach := map[string]bool{w.States[0]: true}
	for changed := true; changed; {
		changed = false
		mark := func(s string) {
			if s != "" && !reach[s] {
				reach[s] = true
				changed = true
			}
		}
		for _, h := range w.Human {
			for _, f := range h.From {
				if reach[f] {
					mark(h.To)
				}
			}
		}
		for _, a := range w.Auto {
			if !reach[a.From] {
				continue
			}
			mark(a.To)
			for _, s := range w.stepsFrom(a.Start) {
				for _, r := range w.Steps[s].On {
					mark(r.To)
				}
			}
		}
	}
	return reach
}

// stepsFrom は、start から next・loop でたどれる step を返す。
func (w *Workflow) stepsFrom(start string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(s string) {
		if seen[s] {
			return
		}
		if _, ok := w.Steps[s]; !ok {
			return
		}
		seen[s] = true
		for _, r := range w.Steps[s].On {
			walk(r.Next)
			walk(r.Loop)
		}
	}
	walk(start)
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}
