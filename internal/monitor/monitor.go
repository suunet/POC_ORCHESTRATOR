// Package monitor は、プロジェクトの tickets の下の記録とカタログを読み、監視の UI に出す状態にまとめる（M2.5、M2.8）。
//
// 読み取り専用で、tickets のディレクトリとカタログ（ids.scan の下）の外は読まない。読むのは os.Root を通すので、
// シンボリックリンクや「..」で外へ出ることもない。.env、認証のボリューム、clone の中身は読まない。
// 日本語の表示名（段・状態・担当・結果・止まる理由・コマンド）は遷移表の display に持たせ、ここでは id だけを扱う。
package monitor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// Monitor は、1つのプロジェクトの記録を読む。Dir は tickets のディレクトリ、Root はプロジェクトのルート（カタログを読む）。
type Monitor struct {
	Dir      string
	Root     string
	Project  *config.Project
	Workflow *workflow.Workflow
	Now      func() time.Time
	Alive    func(ticket.Running) bool // 実行中の印のプロセスが動いているか（テストで差し替える）
}

// 流れの行の塊（並びの順）と、あなたの番のカードの種類（並びの順）。
const (
	GroupRun   = "run"
	GroupHuman = "human"
	GroupWait  = "wait"

	CardStopped   = "stopped"
	CardApprove   = "approve"
	CardQuestions = "questions"
	CardOther     = "other"
)

// 段の帯の1つの状態。
const (
	SegDone  = "done"
	SegRun   = "run"
	SegHuman = "human"
	SegStop  = "stop"
	SegWait  = "wait"
	SegTodo  = "todo"
)

// recentCount は、最近終わったものの件数。
const recentCount = 3

// Status は、画面の全体。
type Status struct {
	Time      string            `json:"time"`
	Project   string            `json:"project"`
	Display   *workflow.Display `json:"display"`
	Cards     []Card            `json:"cards"`   // あなたの番（止まった → 承認待ち → 質問 → そのほか）
	Lanes     []Lane            `json:"lanes"`   // 流れ（AI が作業中 → あなたの番 → 待機、塊の中は番号順）
	Running   []RunRow          `json:"running"` // AI が作業中の1行
	Recent    []Recent          `json:"recent"`  // 最近終わったもの
	DoneTimes []string          `json:"doneTimes"`
	Errors    []string          `json:"errors,omitempty"` // 読めなかったチケット
}

// Link は、画面から開けるファイル（プロジェクトのルートからの相対パス。/api/source で読む）。
type Link struct {
	Kind string `json:"kind"` // ticket・questions・verify-log・diff・catalog-diff・review
	Path string `json:"path"`
}

// Card は、あなたの番の1件。
type Card struct {
	Kind      string         `json:"kind"`
	Ticket    string         `json:"ticket"`
	Title     string         `json:"title"`
	Status    string         `json:"status"`
	Since     string         `json:"since,omitempty"`
	Reason    string         `json:"reason,omitempty"` // 止まった理由の id（display.stops か display.outcomes）
	Detail    string         `json:"detail,omitempty"` // 止まったときの記録の文
	Step      string         `json:"step,omitempty"`   // 最後の step と、その段の何回目か
	Iteration int            `json:"iteration,omitempty"`
	Failure   *Failure       `json:"failure,omitempty"`
	Denied    []Denied       `json:"denied,omitempty"`
	Approve   *ApproveInfo   `json:"approve,omitempty"`
	Questions *QuestionsInfo `json:"questions,omitempty"`
	Next      string         `json:"next"`
	Links     []Link         `json:"links,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
}

// Failure は、検証の失敗の要約（失敗したテストの名前と、出力の最初の数行）。
type Failure struct {
	Tests []string `json:"tests"`
	Lines []string `json:"lines"`
}

// Denied は、プロキシが拒否した接続の数（ホストごと）。
type Denied struct {
	Host  string `json:"host"`
	Count int    `json:"count"`
}

// ApproveInfo は、承認待ちの計画の件数。
type ApproveInfo struct {
	Acceptance int  `json:"acceptance"`
	TestCases  int  `json:"testCases"`
	Sheets     int  `json:"sheets"`
	DiffAdd    int  `json:"diffAdd"`
	DiffDel    int  `json:"diffDel"`
	HasDiff    bool `json:"hasDiff"`
	IMO        int  `json:"imo"`
	IMOPlanned int  `json:"imoPlanned"`
}

// QuestionsInfo は、未確定の質疑。1件目は本文・選択肢・推奨まで、残りは題名だけ。
type QuestionsInfo struct {
	Count  int        `json:"count"`
	First  *Question  `json:"first,omitempty"`
	Others []QuestRef `json:"others"`
}

// Question は、質疑の1件の中身。
type Question struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Options   []string `json:"options"`
	Recommend string   `json:"recommend,omitempty"`
}

// QuestRef は、質疑の番号と題名。
type QuestRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Lane は、流れの1行。
type Lane struct {
	Ticket   string    `json:"ticket"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Group    string    `json:"group"`
	Current  int       `json:"current"` // 今の段の番号
	Segments []Segment `json:"segments"`
	Role     string    `json:"role"`              // 今の段の担当（display.roles の id）
	Note     string    `json:"note"`              // 様子の種類：run・stopped・approve・questions・review・wait-ready・wait-run・held・other
	Reason   string    `json:"reason,omitempty"`  // 止まった理由の id
	Count    int       `json:"count,omitempty"`   // 質問の件数
	Started  string    `json:"started,omitempty"` // 実行中の step の開始
	Since    string    `json:"since,omitempty"`
	Holder   string    `json:"holder,omitempty"` // 順番待ちのとき、いま AI を動かしているチケット
}

// Segment は、段の帯の1つ。Count はその段の往復の回数（その段の step を走らせた回数）。
type Segment struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// RunRow は、AI が作業中のチケットの1行。
type RunRow struct {
	Ticket    string `json:"ticket"`
	Title     string `json:"title"`
	Stage     int    `json:"stage"`
	Step      string `json:"step"`
	Role      string `json:"role"`
	Iteration int    `json:"iteration"`
	Started   string `json:"started,omitempty"`
	Invalid   int    `json:"invalid"` // この step の出力の不正の回数
	Denied    int    `json:"denied"`  // この実行のプロキシの拒否の数
	// Waiting は、AI の呼び出しの順番を待っていること（M2.9）。Holder はいま AI を動かしているもの
	Waiting   bool   `json:"waiting,omitempty"`
	WaitSince string `json:"waitSince,omitempty"`
	Holder    string `json:"holder,omitempty"`
}

// Recent は、最近終わったもの。
type Recent struct {
	Ticket       string `json:"ticket"`
	Title        string `json:"title"`
	DoneAt       string `json:"doneAt"`
	Seconds      int    `json:"seconds"`
	SpecReviews  int    `json:"specReviews"`  // 要件の審査（P2）を走らせた回数
	HumanDecided int    `json:"humanDecided"` // 決定者が人の質疑の数
}

// Status は、tickets の下を読み、画面の全体を返す。
func (m *Monitor) Status() (Status, error) {
	st := Status{Time: m.Now().UTC().Format(time.RFC3339), Project: m.Project.Name, Display: m.Workflow.Display,
		Cards: []Card{}, Lanes: []Lane{}, Running: []RunRow{}, Recent: []Recent{}, DoneTimes: []string{}}
	if m.Workflow.Display == nil {
		return st, fmt.Errorf("遷移表 %s に display（段と表示名）がない", m.Workflow.Name)
	}
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
		tr, err := m.load(fsys, en.Name())
		if err != nil {
			st.Errors = append(st.Errors, fmt.Sprintf("%s：%v", en.Name(), err))
			continue
		}
		m.add(&st, tr)
	}
	group := map[string]int{GroupRun: 0, GroupHuman: 1, GroupWait: 2}
	slices.SortStableFunc(st.Lanes, func(a, b Lane) int {
		if d := group[a.Group] - group[b.Group]; d != 0 {
			return d
		}
		return strings.Compare(a.Ticket, b.Ticket)
	})
	card := map[string]int{CardStopped: 0, CardApprove: 1, CardQuestions: 2, CardOther: 3}
	slices.SortStableFunc(st.Cards, func(a, b Card) int {
		if d := card[a.Kind] - card[b.Kind]; d != 0 {
			return d
		}
		return strings.Compare(a.Ticket, b.Ticket)
	})
	slices.SortFunc(st.Running, func(a, b RunRow) int { return strings.Compare(a.Ticket, b.Ticket) })
	slices.SortFunc(st.Recent, func(a, b Recent) int { return strings.Compare(b.DoneAt, a.DoneAt) })
	if len(st.Recent) > recentCount {
		st.Recent = st.Recent[:recentCount]
	}
	return st, nil
}

// tracked は、1件のチケットの記録を読んだもの。
type tracked struct {
	fsys      fs.FS
	t         *ticket.Ticket
	runs      []runRec
	events    []engine.Event
	questions []byte
}

// runRec は、1回の実行の記録。
type runRec struct {
	name  string
	state *engine.State
	log   []map[string]any
	mark  *ticket.Running
}

func (m *Monitor) load(fsys fs.FS, id string) (*tracked, error) {
	data, err := fs.ReadFile(fsys, path.Join(id, ticket.File))
	if err != nil {
		return nil, err
	}
	t, err := ticket.Parse(id, path.Join(m.Dir, id, ticket.File), data)
	if err != nil {
		return nil, err
	}
	tr := &tracked{fsys: fsys, t: t, events: readStatusEvents(fsys, id)}
	tr.questions, _ = fs.ReadFile(fsys, path.Join(id, ticket.QuestionsFile))
	entries, _ := fs.ReadDir(fsys, path.Join(id, "runs"))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := path.Join(id, "runs", e.Name())
		r := runRec{name: e.Name(), log: readLog(fsys, dir)}
		if b, err := fs.ReadFile(fsys, path.Join(dir, engine.StateFile)); err == nil {
			if s, err := engine.DecodeState(b); err == nil {
				r.state = s
			}
		}
		if b, err := fs.ReadFile(fsys, path.Join(dir, ticket.RunningFile)); err == nil {
			if mk, err := ticket.DecodeRunning(b); err == nil {
				r.mark = &mk
			}
		}
		tr.runs = append(tr.runs, r)
	}
	sort.Slice(tr.runs, func(i, j int) bool { return tr.runs[i].name < tr.runs[j].name })
	return tr, nil
}

// latest は、state.json のある最後の実行を返す。
func (tr *tracked) latest() *runRec {
	for i := len(tr.runs) - 1; i >= 0; i-- {
		if tr.runs[i].state != nil {
			return &tr.runs[i]
		}
	}
	return nil
}

func (tr *tracked) runDir(r *runRec) string { return path.Join(tr.t.ID, "runs", r.name) }

// add は、1件のチケットを、流れ・あなたの番・AI が作業中・最近終わったもののどれかに入れる。
func (m *Monitor) add(st *Status, tr *tracked) {
	t := tr.t
	d := m.Workflow.Display
	if t.Status == ticket.Done {
		if at := tr.doneAt(); at != "" {
			st.DoneTimes = append(st.DoneTimes, at)
			st.Recent = append(st.Recent, m.recent(tr, at))
		}
		return
	}
	if slices.Contains(m.Workflow.Terminal, string(t.Status)) {
		return // cancelled など
	}
	counts := m.stageCounts(tr)
	lane := Lane{Ticket: t.ID, Title: t.Title, Status: string(t.Status), Current: d.StageOfState(string(t.Status))}
	last := tr.latest()

	// 実行中
	if last != nil && last.mark != nil && m.Alive(*last.mark) {
		step, started, invalid := currentStep(last)
		if step == "" && last.state != nil {
			step = last.state.Step
		}
		if i := d.StageOfStep(step); i >= 0 {
			lane.Current = i
			counts[i]++ // 走っている回も数える
		}
		lane.Group, lane.Note, lane.Role, lane.Started = GroupRun, "run", m.Workflow.StepRole(step), started
		kind := SegRun
		waitSince, holder, waiting := agentWaiting(last)
		if waiting {
			// AI の呼び出しの順番を待っている（M2.9）
			lane.Note, lane.Holder, kind = "wait-ai", holder, SegWait
		}
		lane.Segments = segments(len(d.Stages), lane.Current, kind, counts)
		st.Lanes = append(st.Lanes, lane)
		st.Running = append(st.Running, RunRow{Ticket: t.ID, Title: t.Title, Stage: lane.Current, Step: step,
			Role: lane.Role, Iteration: m.iteration(tr, step), Started: started, Invalid: invalid,
			Denied: total(denied(tr.fsys, tr.runDir(last))), Waiting: waiting, WaitSince: waitSince, Holder: holder})
		return
	}

	res := engine.DescribeTicket(m.Project, m.Workflow, t, stateOf(last), lastEvent(tr.events))
	card := Card{Ticket: t.ID, Title: t.Title, Status: string(t.Status), Next: res.Next, Warnings: res.Warnings,
		Since: tr.since(last)}
	ticketLink := Link{Kind: "ticket", Path: m.ticketPath(t.ID, ticket.File)}
	lane.Role = workflow.RoleHuman
	switch {
	case last != nil && last.mark != nil:
		// 実行中の印が残っているが、プロセスがない（gateflow が異常に終わった）
		card.Kind, card.Reason = CardStopped, "interrupted"
		card.Detail = fmt.Sprintf("pid %d", last.mark.PID)
		card.Next = "gateflow resume " + t.ID
		m.stopAt(&lane, last)
	case !slices.Contains(m.Workflow.States, string(t.Status)):
		card.Kind, card.Reason, card.Detail = CardOther, "unknown-state", string(t.Status)
		lane.Note = "other"
	case res.Outcome == engine.Stopped:
		card.Kind, card.Reason, card.Detail = CardStopped, res.Reason, res.Message
		card.Step, card.Iteration = last.state.Step, m.iteration(tr, last.state.Step)
		card.Failure = verifyFailure(tr.fsys, tr.runDir(last))
		card.Denied = denied(tr.fsys, tr.runDir(last))
		card.Links = m.stopLinks(tr, last)
		m.stopAt(&lane, last)
		lane.Reason = res.Reason
	case res.Outcome == engine.Gate && t.Status == ticket.ReadyToApprove:
		card.Kind = CardApprove
		card.Approve = m.approveInfo(tr)
		card.Links = append([]Link{ticketLink}, m.reportLinks(tr)...)
		lane.Note = "approve"
	case res.Outcome == engine.Gate && t.Status == ticket.Draft && ticket.CountUnresolved(tr.questions) > 0:
		card.Kind = CardQuestions
		card.Questions = questionsInfo(tr.questions)
		card.Step, card.Iteration = m.lastStep(tr)
		card.Links = []Link{{Kind: "questions", Path: m.ticketPath(t.ID, ticket.QuestionsFile)}, ticketLink}
		lane.Note, lane.Count = "questions", card.Questions.Count
		// 質問は、それを出した段（要件の審査など）の人の番として出す（見本のとおり）
		if i := d.StageOfStep(card.Step); i >= 0 {
			lane.Current = i
		}
	case res.Outcome == engine.Gate && t.Status == ticket.Draft:
		lane.Group, lane.Note = GroupWait, "wait-ready"
	case res.Outcome == engine.Gate:
		card.Kind = CardOther
		card.Links = []Link{ticketLink}
		lane.Note = "review"
	case res.Outcome == engine.Runnable:
		lane.Group, lane.Note, lane.Role = GroupWait, "wait-run", m.Workflow.StepRole(m.autoStep(t))
	case t.Held():
		lane.Group, lane.Note = GroupWait, "held"
	default:
		card.Kind, card.Detail = CardOther, res.Message
		lane.Note = "other"
	}
	if lane.Group == "" {
		lane.Group = GroupHuman
	}
	lane.Since = card.Since
	kind := map[string]string{GroupHuman: SegHuman, GroupWait: SegWait}[lane.Group]
	if card.Kind == CardStopped {
		kind = SegStop
	}
	lane.Segments = segments(len(d.Stages), lane.Current, kind, counts)
	st.Lanes = append(st.Lanes, lane)
	if card.Kind != "" {
		st.Cards = append(st.Cards, card)
	}
}

// stopAt は、止まった段（止まったときの step の段）を流れの今の段にする。
func (m *Monitor) stopAt(lane *Lane, last *runRec) {
	lane.Note = "stopped"
	if last.state != nil {
		step := last.state.Step
		if last.state.Resume != "" {
			step = last.state.Resume
		}
		if i := m.Workflow.Display.StageOfStep(step); i >= 0 {
			lane.Current = i
			lane.Role = m.Workflow.StepRole(step)
		}
	}
}

func (m *Monitor) autoStep(t *ticket.Ticket) string {
	if a, ok := m.Workflow.AutoFrom(string(t.Status)); ok {
		return a.Start
	}
	return ""
}

func (m *Monitor) ticketPath(id, name string) string { return path.Join(ticket.Dir, id, name) }

// segments は、段の帯を作る。今の段より前は済んだ段、今の段は kind、後はまだ。
func segments(n, current int, kind string, counts []int) []Segment {
	out := make([]Segment, n)
	for i := range out {
		switch {
		case i < current:
			out[i].Kind = SegDone
		case i == current:
			out[i].Kind = kind
		default:
			out[i].Kind = SegTodo
		}
		out[i].Count = counts[i]
	}
	return out
}

// stageCounts は、段ごとに、その段の step を走らせた回数（すべての実行の step-end の数）を返す。
func (m *Monitor) stageCounts(tr *tracked) []int {
	counts := make([]int, len(m.Workflow.Display.Stages))
	for _, r := range tr.runs {
		for _, e := range r.log {
			if e["event"] == "step-end" {
				if i := m.Workflow.Display.StageOfStep(str(e["step"])); i >= 0 {
					counts[i]++
				}
			}
		}
	}
	return counts
}

// iteration は、step をすべての実行で何回走らせたか（走っている回を含む）を返す。
func (m *Monitor) iteration(tr *tracked, step string) int {
	n := 0
	for _, r := range tr.runs {
		for _, e := range r.log {
			if e["event"] == "step-start" && str(e["step"]) == step {
				n++
			}
		}
	}
	return n
}

// lastStep は、最後に終わった step と、その回数を返す。
func (m *Monitor) lastStep(tr *tracked) (string, int) {
	for i := len(tr.runs) - 1; i >= 0; i-- {
		log := tr.runs[i].log
		for j := len(log) - 1; j >= 0; j-- {
			if log[j]["event"] == "step-end" {
				step := str(log[j]["step"])
				return step, m.iteration(tr, step)
			}
		}
	}
	return "", 0
}

// currentStep は、実行中の実行の、走っている step・その開始・その step の出力の不正の回数を返す。
func currentStep(r *runRec) (step, started string, invalid int) {
	for _, e := range r.log {
		switch e["event"] {
		case "step-start":
			step, started, invalid = str(e["step"]), str(e["time"]), 0
		case "output-invalid":
			invalid++
		}
	}
	return step, started, invalid
}

// agentWaiting は、実行中の実行が AI の呼び出しの順番を待っているか（最後の agent-wait の後に、取れた記録も
// エージェントの起動の記録もない）と、待ち始めた時刻、いま AI を動かしているものを返す。
func agentWaiting(r *runRec) (since, holder string, waiting bool) {
	for _, e := range r.log {
		switch e["event"] {
		case "agent-wait":
			if !waiting {
				since = str(e["time"])
			}
			waiting = true
			holder = str(e["holderTicket"])
			if holder == "" {
				holder = str(e["holderStep"])
			}
			if p := str(e["holderProject"]); p != "" && holder != "" {
				holder = p + " の " + holder
			}
		case "agent-acquired", "agent-start", "step-end":
			waiting, since, holder = false, "", ""
		}
	}
	return since, holder, waiting
}

// since は、人を待ち始めた時刻（止まった・関門に着いた記録の時刻、なければステータスの最後の記録）。
func (tr *tracked) since(last *runRec) string {
	if last != nil {
		for j := len(last.log) - 1; j >= 0; j-- {
			switch last.log[j]["event"] {
			case "stopped", "gate", "error", "finished":
				return str(last.log[j]["time"])
			}
		}
	}
	if len(tr.events) > 0 {
		return tr.events[len(tr.events)-1].Time
	}
	return ""
}

// doneAt は、done になった時刻（ステータスの記録の最後の done）。
func (tr *tracked) doneAt() string {
	for i := len(tr.events) - 1; i >= 0; i-- {
		if tr.events[i].To == string(ticket.Done) {
			return tr.events[i].Time
		}
	}
	return ""
}

func (m *Monitor) recent(tr *tracked, at string) Recent {
	r := Recent{Ticket: tr.t.ID, Title: tr.t.Title, DoneAt: at}
	start := ""
	if len(tr.events) > 0 {
		start = tr.events[0].Time
	}
	if len(tr.runs) > 0 {
		if first := runStart(tr.runs[0]); first != "" && (start == "" || first < start) {
			start = first
		}
	}
	if a, err := time.Parse(time.RFC3339, start); err == nil {
		if b, err := time.Parse(time.RFC3339, at); err == nil {
			r.Seconds = int(b.Sub(a).Seconds())
		}
	}
	for _, run := range tr.runs {
		for _, e := range run.log {
			if e["event"] == "step-end" && str(e["step"]) == "p2-review" {
				r.SpecReviews++
			}
		}
	}
	for _, q := range ticket.QuestionRows(tr.questions) {
		if q.Decider == "人" {
			r.HumanDecided++
		}
	}
	return r
}

func runStart(r runRec) string {
	if len(r.log) > 0 {
		return str(r.log[0]["time"])
	}
	return ""
}

func stateOf(r *runRec) *engine.State {
	if r == nil {
		return nil
	}
	return r.state
}

// approveInfo は、承認待ちの計画の件数（指示書の §4、カタログの差分、P2 の IMO への対応）。
func (m *Monitor) approveInfo(tr *tracked) *ApproveInfo {
	t := tr.t
	a := &ApproveInfo{Acceptance: t.AcceptanceCount(), TestCases: t.TestCaseCount(), Sheets: t.SheetCount(), IMO: len(t.IMOResponses())}
	if p := m.findStepFile(tr, "p3-prepare", "out/result.json"); p != "" {
		if b, err := fs.ReadFile(tr.fsys, p); err == nil {
			var r struct {
				IMO []struct {
					Decision string `json:"decision"`
				} `json:"imo"`
			}
			if json.Unmarshal(b, &r) == nil {
				for _, x := range r.IMO {
					if x.Decision == "planned" {
						a.IMOPlanned++
					}
				}
			}
		}
	}
	if p := m.findRunFile(tr, "arb-report.md"); p != "" {
		if b, err := fs.ReadFile(tr.fsys, p); err == nil {
			a.HasDiff = true
			a.DiffAdd, a.DiffDel = diffLines(b)
		}
	}
	return a
}

// reportLinks は、承認の前の報告（カタログの差分）へのリンク。
func (m *Monitor) reportLinks(tr *tracked) []Link {
	if p := m.findRunFile(tr, "arb-report.md"); p != "" {
		return []Link{{Kind: "catalog-diff", Path: path.Join(ticket.Dir, p)}}
	}
	return nil
}

// stopLinks は、止まった実行の検証の記録と差分へのリンク。
func (m *Monitor) stopLinks(tr *tracked, last *runRec) []Link {
	var links []Link
	if p := lastStepFile(tr.fsys, tr.runDir(last), "verify.log"); p != "" {
		links = append(links, Link{Kind: "verify-log", Path: path.Join(ticket.Dir, p)})
	}
	if p := lastStepFile(tr.fsys, tr.runDir(last), "diff.patch"); p != "" {
		links = append(links, Link{Kind: "diff", Path: path.Join(ticket.Dir, p)})
	}
	return append(links, Link{Kind: "ticket", Path: m.ticketPath(tr.t.ID, ticket.File)})
}

// findRunFile は、新しい実行から順に、実行のディレクトリにある name を探す（tickets からの相対パス）。
func (m *Monitor) findRunFile(tr *tracked, name string) string {
	for i := len(tr.runs) - 1; i >= 0; i-- {
		p := path.Join(tr.runDir(&tr.runs[i]), name)
		if _, err := fs.Stat(tr.fsys, p); err == nil {
			return p
		}
	}
	return ""
}

// findStepFile は、新しい実行から順に、step の記録にある rel を探す。
func (m *Monitor) findStepFile(tr *tracked, step, rel string) string {
	for i := len(tr.runs) - 1; i >= 0; i-- {
		entries, _ := fs.ReadDir(tr.fsys, path.Join(tr.runDir(&tr.runs[i]), "steps"))
		for j := len(entries) - 1; j >= 0; j-- {
			if strings.HasSuffix(entries[j].Name(), "-"+step) {
				p := path.Join(tr.runDir(&tr.runs[i]), "steps", entries[j].Name(), rel)
				if _, err := fs.Stat(tr.fsys, p); err == nil {
					return p
				}
			}
		}
	}
	return ""
}

// lastStepFile は、実行の中で、name を持つ最後の step の記録のファイルを返す。
func lastStepFile(fsys fs.FS, runDir, name string) string {
	entries, _ := fs.ReadDir(fsys, path.Join(runDir, "steps"))
	for j := len(entries) - 1; j >= 0; j-- {
		p := path.Join(runDir, "steps", entries[j].Name(), name)
		if _, err := fs.Stat(fsys, p); err == nil {
			return p
		}
	}
	return ""
}

// verifyFailure は、実行の最後の検証が不合格なら、失敗したテストの名前と出力の最初の数行を返す。
func verifyFailure(fsys fs.FS, runDir string) *Failure {
	p := lastStepFile(fsys, runDir, "verify.log")
	if p == "" {
		return nil
	}
	b, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil
	}
	f := &Failure{Tests: []string{}, Lines: []string{}}
	lines := strings.Split(string(b), "\n")
	first := -1
	for i, l := range lines {
		if name, ok := strings.CutPrefix(strings.TrimSpace(l), "--- FAIL: "); ok {
			if first < 0 {
				first = i
			}
			name, _, _ = strings.Cut(name, " ")
			f.Tests = append(f.Tests, name)
		}
	}
	if first < 0 {
		return nil
	}
	for _, l := range lines[first+1:] {
		if len(f.Lines) == 5 {
			break
		}
		if s := strings.TrimRight(l, " \t\r"); s != "" {
			f.Lines = append(f.Lines, s)
		}
	}
	return f
}

// diffLines は、報告の diff の区画の、足した行と消した行の数を返す。
func diffLines(b []byte) (add, del int) {
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, "```diff"):
			in = true
		case strings.HasPrefix(l, "```"):
			in = false
		case !in, strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

// questionsInfo は、未確定の質疑の件数と、1件目の中身、残りの題名を返す。
func questionsInfo(data []byte) *QuestionsInfo {
	info := &QuestionsInfo{Others: []QuestRef{}}
	for _, r := range ticket.QuestionRows(data) {
		if r.State == "確定" {
			continue
		}
		info.Count++
		if info.First == nil {
			info.First = questionBody(data, r)
			continue
		}
		info.Others = append(info.Others, QuestRef{ID: r.ID, Title: r.Summary})
	}
	return info
}

// questionBody は、質疑の節（## Q-NN）から、質問の本文・選択肢・推奨（または提案）を読む。
func questionBody(data []byte, r ticket.QuestionRow) *Question {
	q := &Question{ID: r.ID, Title: r.Summary, Options: []string{}}
	var body []string
	in, part := false, ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "## ") {
			in = strings.HasPrefix(l, "## "+r.ID+" ")
			part = ""
			continue
		}
		if !in {
			continue
		}
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, "**") && strings.HasSuffix(s, "**") {
			part = strings.Trim(s, "*")
			continue
		}
		if part != "質問" || s == "" {
			continue
		}
		if v, ok := cutLabel(s, "選択肢"); ok {
			for _, o := range strings.Split(v, "／") {
				if o = strings.TrimSpace(o); o != "" {
					q.Options = append(q.Options, o)
				}
			}
			continue
		}
		if v, ok := cutLabel(s, "推奨"); ok {
			q.Recommend = v
			continue
		}
		if v, ok := cutLabel(s, "提案"); ok && q.Recommend == "" {
			q.Recommend = v
			continue
		}
		if _, ok := cutLabel(s, "根拠"); ok {
			continue
		}
		body = append(body, s)
	}
	q.Body = strings.Join(body, "\n")
	return q
}

func cutLabel(s, label string) (string, bool) {
	for _, sep := range []string{"：", ":"} {
		if v, ok := strings.CutPrefix(s, label+sep); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func readStatusEvents(fsys fs.FS, id string) []engine.Event {
	data, err := fs.ReadFile(fsys, path.Join(id, engine.EventsFile))
	if err != nil {
		return nil
	}
	var out []engine.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var ev engine.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func lastEvent(evs []engine.Event) *engine.Event {
	if len(evs) == 0 {
		return nil
	}
	return &evs[len(evs)-1]
}

func readLog(fsys fs.FS, runDir string) []map[string]any {
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

func total(ds []Denied) int {
	n := 0
	for _, d := range ds {
		n += d.Count
	}
	return n
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

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
