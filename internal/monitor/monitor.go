// Package monitor は、プロジェクトの tickets の下の記録を読み、監視の UI に出す状態にまとめる（M2.5）。
//
// 読み取り専用で、tickets のディレクトリの外は読まない。読むのは os.Root を通すので、
// シンボリックリンクや「..」で外へ出ることもない。.env、認証のボリューム、clone の中身は読まない。
package monitor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gateflow/internal/config"
	"gateflow/internal/engine"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
)

// 表示する件数と、エージェントの出力を末尾から読む大きさ。
const (
	eventCount = 12
	toolCount  = 10
	tailBytes  = 256 << 10
)

// Monitor は、1つのプロジェクトの記録を読む。Dir は tickets のディレクトリ。
type Monitor struct {
	Dir      string
	Project  *config.Project
	Workflow *workflow.Workflow
	Now      func() time.Time
	Alive    func(ticket.Running) bool // 実行中の印のプロセスが動いているか（テストで差し替える）
}

// Status は、画面の2段（人を待っているもの、実行中）。
type Status struct {
	Time    string    `json:"time"`
	Project string    `json:"project"`
	Waiting []Waiting `json:"waiting"`
	Running []Running `json:"running"`
	Errors  []string  `json:"errors,omitempty"` // 読めなかったチケット
}

// Waiting は、人を待っているチケット。Outcome は gate・stopped・runnable・nothing-to-do・finished・interrupted。
type Waiting struct {
	Ticket   string   `json:"ticket"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Outcome  string   `json:"outcome"`
	Reason   string   `json:"reason"`
	Next     string   `json:"next"`
	Run      string   `json:"run,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Running は、実行中の run。
type Running struct {
	Ticket      string         `json:"ticket"`
	Title       string         `json:"title"`
	Status      string         `json:"status"`
	Run         string         `json:"run"`
	Step        string         `json:"step"`
	StepNo      int            `json:"stepNo"`
	Attempt     int            `json:"attempt"`
	StepStarted string         `json:"stepStarted,omitempty"`
	StepSeconds int            `json:"stepSeconds"`
	Steps       int            `json:"steps"`
	Loops       map[string]int `json:"loops"`
	Limits      config.Limits  `json:"limits"`
	Events      []Event        `json:"events"`
	Denied      []Denied       `json:"denied"`
	Tools       []Tool         `json:"tools"`
}

// Event は、log.jsonl の1件の要約。Alert は目立たせるもの（出力の不正、根拠の検査の不合格、誤り）。
type Event struct {
	Time  string `json:"time"`
	Event string `json:"event"`
	Text  string `json:"text"`
	Alert bool   `json:"alert,omitempty"`
}

// Denied は、プロキシが拒否した接続のホストと件数。
type Denied struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// Tool は、エージェントのツールの呼び出しの名前と対象。入力の全文と AI の文章は持たない。
type Tool struct {
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

// Status は、今の状態を読む。tickets のディレクトリがなければ、空の状態を返す。
func (m *Monitor) Status() (Status, error) {
	st := Status{Time: m.Now().UTC().Format(time.RFC3339), Project: m.Project.Name, Waiting: []Waiting{}, Running: []Running{}}
	root, err := os.OpenRoot(m.Dir)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer root.Close()
	fsys := root.FS()
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return st, err
	}
	for _, en := range entries {
		if en.Type()&fs.ModeSymlink != 0 {
			st.Errors = append(st.Errors, en.Name()+"：リンクは読まない")
			continue
		}
		if !en.IsDir() {
			continue
		}
		if err := m.readTicket(fsys, en.Name(), &st); err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s：%v", en.Name(), err))
		}
	}
	return st, nil
}

func (m *Monitor) readTicket(fsys fs.FS, id string, st *Status) error {
	data, err := fs.ReadFile(fsys, path.Join(id, ticket.File))
	if err != nil {
		return err
	}
	t, err := ticket.Parse(id, path.Join(m.Dir, id, ticket.File), data)
	if err != nil {
		return err
	}
	run, state := latestRun(fsys, id)
	var mark *ticket.Running
	if run != "" {
		if b, err := fs.ReadFile(fsys, path.Join(id, "runs", run, ticket.RunningFile)); err == nil {
			if r, err := ticket.DecodeRunning(b); err == nil {
				mark = &r
			}
		}
	}
	if mark != nil && m.Alive(*mark) {
		st.Running = append(st.Running, m.running(fsys, t, run, state))
		return nil
	}

	res := engine.DescribeTicket(m.Project, m.Workflow, t, state, lastEvent(fsys, id))
	w := Waiting{Ticket: t.ID, Title: t.Title, Status: string(t.Status), Outcome: string(res.Outcome), Next: res.Next, Run: run, Warnings: res.Warnings}
	switch {
	case mark != nil:
		w.Outcome = "interrupted"
		w.Reason = fmt.Sprintf("中断（実行中の印が残っているが、プロセス %d がない）", mark.PID)
	case res.Outcome == engine.Finished && res.Next == "":
		return nil // 終わったチケット（cancelled など）は出さない
	case res.Outcome == engine.Stopped:
		w.Reason = res.Reason + "：" + res.Message
	case res.Outcome == engine.Gate:
		w.Reason = gateReason(fsys, id, run)
	default:
		w.Reason = res.Message
	}
	st.Waiting = append(st.Waiting, w)
	return nil
}

// latestRun は、名前の順で最後の、state.json のある実行の記録を返す。
func latestRun(fsys fs.FS, id string) (string, *engine.State) {
	entries, err := fs.ReadDir(fsys, path.Join(id, "runs"))
	if err != nil {
		return "", nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		if b, err := fs.ReadFile(fsys, path.Join(id, "runs", n, engine.StateFile)); err == nil {
			if s, err := engine.DecodeState(b); err == nil {
				return n, s
			}
		}
	}
	return "", nil
}

func lastEvent(fsys fs.FS, id string) *engine.Event {
	data, err := fs.ReadFile(fsys, path.Join(id, engine.EventsFile))
	if err != nil {
		return nil
	}
	var last *engine.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var ev engine.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			last = &ev
		}
	}
	return last
}

// gateReason は、関門で止まった理由の1行（最後の step の結果、BJ の指摘の件数と要約、未確定の質疑）。
func gateReason(fsys fs.FS, id, run string) string {
	var parts []string
	if run != "" {
		evs := readEvents(fsys, path.Join(id, "runs", run))
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i]["event"] != "step-end" {
				continue
			}
			step, _ := evs[i]["step"].(string)
			outcome, _ := evs[i]["outcome"].(string)
			parts = append(parts, step+" "+outcome)
			if r := stepResult(fsys, path.Join(id, "runs", run), step); r != "" {
				parts = append(parts, r)
			}
			break
		}
	}
	if b, err := fs.ReadFile(fsys, path.Join(id, ticket.QuestionsFile)); err == nil {
		if n := ticket.CountUnresolved(b); n > 0 {
			parts = append(parts, fmt.Sprintf("未確定の質疑 %d 件", n))
		}
	}
	if len(parts) == 0 {
		return "人間関門で待っている"
	}
	return strings.Join(parts, "。")
}

// stepResult は、run の中で最後の step の out/result.json から、ラベルごとの件数と要約を返す。
func stepResult(fsys fs.FS, runDir, step string) string {
	entries, err := fs.ReadDir(fsys, path.Join(runDir, "steps"))
	if err != nil {
		return ""
	}
	var dir string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-"+step) {
			dir = e.Name()
		}
	}
	if dir == "" {
		return ""
	}
	data, err := fs.ReadFile(fsys, path.Join(runDir, "steps", dir, "out", "result.json"))
	if err != nil {
		return ""
	}
	var r struct {
		Summary  string `json:"summary"`
		Findings []struct {
			Label string `json:"label"`
		} `json:"findings"`
	}
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	counts := map[string]int{}
	for _, f := range r.Findings {
		counts[f.Label]++
	}
	var labels []string
	for _, l := range []string{"MUST", "Q", "IMO", "NR", "NITS", "FYI"} {
		if counts[l] > 0 {
			labels = append(labels, fmt.Sprintf("%s %d", l, counts[l]))
		}
	}
	s := strings.Join(labels, "・")
	if sum := clip(r.Summary, 80); sum != "" {
		if s != "" {
			s += "："
		}
		s += sum
	}
	return s
}

func (m *Monitor) running(fsys fs.FS, t *ticket.Ticket, run string, state *engine.State) Running {
	runDir := path.Join(t.ID, "runs", run)
	r := Running{Ticket: t.ID, Title: t.Title, Status: string(t.Status), Run: run, Limits: m.Project.EffectiveLimits(),
		Loops: map[string]int{}, Events: []Event{}, Denied: []Denied{}, Tools: []Tool{}}
	if state != nil {
		r.Step, r.Steps = state.Step, state.Steps
		for k, v := range state.Loops {
			r.Loops[k] = v
		}
	}
	evs := readEvents(fsys, runDir)
	for _, e := range evs {
		switch e["event"] {
		case "step-start":
			r.StepStarted, _ = e["time"].(string)
			r.StepNo = toInt(e["n"])
			r.Attempt = 0
			if s, ok := e["step"].(string); ok {
				r.Step = s
			}
		case "agent-start":
			r.Attempt = toInt(e["attempt"])
		}
	}
	if ts, err := time.Parse(time.RFC3339, r.StepStarted); err == nil {
		r.StepSeconds = int(m.Now().Sub(ts).Seconds())
	}
	for _, e := range evs[max(0, len(evs)-eventCount):] {
		r.Events = append(r.Events, summarize(e))
	}
	r.Denied = denied(fsys, runDir)
	r.Tools = tools(fsys, runDir)
	return r
}

func readEvents(fsys fs.FS, runDir string) []map[string]any {
	data, err := fs.ReadFile(fsys, path.Join(runDir, "log.jsonl"))
	if err != nil {
		return nil
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// summarize は、記録の1件を1行の文にする。出力の不正・根拠の検査の不合格・誤りは目立たせる。
func summarize(e map[string]any) Event {
	name, _ := e["event"].(string)
	tm, _ := e["time"].(string)
	ev := Event{Time: tm, Event: name}
	str := func(k string) string { return fmt.Sprint(e[k]) }
	switch name {
	case "step-start":
		ev.Text = fmt.Sprintf("%s を始めた（%d 番目）", str("step"), toInt(e["n"]))
	case "agent-start":
		ev.Text = fmt.Sprintf("%s のエージェント（%d 回目）", str("step"), toInt(e["attempt"]))
	case "agent-end":
		ev.Text = fmt.Sprintf("%s のエージェントが終わった（終了コード %s）", str("step"), str("exitCode"))
	case "step-end":
		ev.Text = fmt.Sprintf("%s → %s（%d 秒）", str("step"), str("outcome"), int(toFloat(e["seconds"])))
	case "output-invalid":
		ev.Text = clip(firstLine(str("error")), 160)
		ev.Alert = true
	case "evidence-check":
		ok, _ := e["ok"].(bool)
		hits, _ := e["hits"].([]any)
		ev.Text = fmt.Sprintf("根拠の検査 %d 件", len(hits))
		ev.Alert = !ok
		if !ok {
			ev.Text += "（不合格あり）"
		}
	case "verify":
		ev.Text = fmt.Sprintf("検証 %s（終了コード %s）", str("name"), str("exitCode"))
		ev.Alert = toInt(e["exitCode"]) != 0
	case "error", "running-mark-error":
		ev.Text = clip(firstLine(str("error")), 160)
		ev.Alert = true
	default:
		var kv []string
		keys := make([]string, 0, len(e))
		for k := range e {
			if k != "time" && k != "event" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			kv = append(kv, k+"="+clip(fmt.Sprint(e[k]), 40))
		}
		ev.Text = strings.Join(kv, " ")
		ev.Alert = name == "stopped"
	}
	return ev
}

// denied は、proxy.log の拒否をホストごとに数える（proxy.log は実行の間に逐次書かれる）。
func denied(fsys fs.FS, runDir string) []Denied {
	out := []Denied{}
	data, err := fs.ReadFile(fsys, path.Join(runDir, engine.ProxyLogFile))
	if err != nil {
		return out
	}
	counts := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var l struct{ Decision, Target string }
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Decision == "deny" {
			counts[l.Target]++
		}
	}
	for h, n := range counts {
		out = append(out, Denied{Host: h, Count: n})
	}
	slices.SortFunc(out, func(a, b Denied) int { return strings.Compare(a.Host, b.Host) })
	return out
}

// tools は、最後の step の、最後のエージェントの出力を末尾から読み、ツールの呼び出しの名前と対象を直近から返す。
func tools(fsys fs.FS, runDir string) []Tool {
	out := []Tool{}
	steps, err := fs.ReadDir(fsys, path.Join(runDir, "steps"))
	if err != nil || len(steps) == 0 {
		return out
	}
	stepDir := path.Join(runDir, "steps", steps[len(steps)-1].Name())
	files, _ := fs.ReadDir(fsys, stepDir)
	best, bestN := "", 0
	for _, f := range files {
		n, ok := agentLogNumber(f.Name())
		if ok && n > bestN {
			best, bestN = f.Name(), n
		}
	}
	if best == "" {
		return out
	}
	data, err := tail(fsys, path.Join(stepDir, best), tailBytes)
	if err != nil {
		return out
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string         `json:"type"`
					Name  string         `json:"name"`
					Input map[string]any `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "assistant" {
			continue
		}
		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" {
				out = append(out, Tool{Name: c.Name, Target: toolTarget(c.Input)})
			}
		}
	}
	if len(out) > toolCount {
		out = out[len(out)-toolCount:]
	}
	slices.Reverse(out)
	return out
}

func agentLogNumber(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, "agent-")
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, ".jsonl")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// toolTarget は、ツールの入力から対象（ファイルのパス、コマンドの先頭など）だけを取り出す。
func toolTarget(in map[string]any) string {
	for _, k := range []string{"file_path", "path", "notebook_path", "pattern", "command", "url", "query"} {
		if v, ok := in[k].(string); ok && v != "" {
			return clip(firstLine(v), 80)
		}
	}
	return ""
}

// tail は、ファイルの末尾の n バイトを読む。途中から読んだときは、最初の不完全な行を捨てる。
func tail(fsys fs.FS, name string, n int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size > n {
		s, ok := f.(io.Seeker)
		if !ok {
			return nil, fmt.Errorf("%s を途中から読めない", name)
		}
		if _, err := s.Seek(size-n, io.SeekStart); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil, err
	}
	if size > n {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func toInt(v any) int { return int(toFloat(v)) }

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}
