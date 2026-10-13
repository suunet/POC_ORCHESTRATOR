package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gateflow/internal/idcheck"
	"gateflow/internal/ticket"
)

// sourceLimit は、/api/source で返すファイルの大きさの上限。
const sourceLimit = 2 << 20

// Detail は、チケットの詳細（正本と作業履歴）。
type Detail struct {
	Ticket  string    `json:"ticket"`
	Title   string    `json:"title"`
	Status  string    `json:"status"`
	Kind    string    `json:"kind"`
	Sources []Source  `json:"sources"`
	History []HistRow `json:"history"`
}

// Source は、正本の1件。指示書、または対象 ID の定義（ファイルと行）と、UC の詳細のファイル。
type Source struct {
	ID   string `json:"id,omitempty"`   // 対象 ID（指示書は空）
	Kind string `json:"kind"`           // ticket・definition・detail・missing
	Path string `json:"path,omitempty"` // プロジェクトのルートからの相対パス
	Line int    `json:"line,omitempty"`
}

// HistRow は、作業履歴の1行（段とその繰り返しごと、または人の操作）。
type HistRow struct {
	Time      string         `json:"time"`
	Stage     int            `json:"stage"`
	Step      string         `json:"step,omitempty"`
	Iteration int            `json:"iteration,omitempty"`
	Role      string         `json:"role"`
	Outcome   string         `json:"outcome,omitempty"` // step の結果の id
	Findings  map[string]int `json:"findings,omitempty"`
	Invalid   int            `json:"invalid,omitempty"` // 出力の不正の回数
	Stop      string         `json:"stop,omitempty"`    // 止まった理由の id
	Command   string         `json:"command,omitempty"` // 人の操作
	From      string         `json:"from,omitempty"`
	To        string         `json:"to,omitempty"`
	Seconds   int            `json:"seconds,omitempty"`
	Running   bool           `json:"running,omitempty"`
	Links     []Link         `json:"links,omitempty"`
	Inferred  bool           `json:"inferred,omitempty"` // レビュー報告への参照を、記録の順から推定した
}

var ucID = regexp.MustCompile(`^UC-[0-9]{3}$`)

// Ticket は、チケットの詳細を返す。正本の行は、ID 整合チェックの設定で、カタログを読んで引く。
func (m *Monitor) Ticket(id string) (Detail, error) {
	if !validTicketID(id) {
		return Detail{}, fmt.Errorf("チケットの ID %q の形が正しくない", id)
	}
	root, err := os.OpenRoot(m.Dir)
	if err != nil {
		return Detail{}, err
	}
	defer root.Close()
	tr, err := m.load(root.FS(), id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Ticket: tr.t.ID, Title: tr.t.Title, Status: string(tr.t.Status), Kind: string(tr.t.Kind),
		Sources: []Source{{Kind: "ticket", Path: m.ticketPath(id, ticket.File)}}, History: m.history(tr)}
	d.Sources = append(d.Sources, m.sources(tr.t.TargetIDs())...)
	return d, nil
}

func validTicketID(id string) bool {
	return regexp.MustCompile(`^T-[0-9]{3,}$`).MatchString(id)
}

// sources は、対象 ID ごとの定義の場所と、UC の詳細のファイルを返す。
func (m *Monitor) sources(ids []string) []Source {
	var out []Source
	var res idcheck.Result
	ok := false
	if m.Project.IDs != nil && m.Root != "" {
		if rules, err := idcheck.NewRules(*m.Project.IDs); err == nil {
			if r, err := idcheck.Run(m.Root, rules); err == nil {
				res, ok = r, true
			}
		}
	}
	for _, id := range ids {
		loc, found := Location{}, false
		if ok {
			if l, f := res.Where(id); f {
				loc, found = Location{File: l.File, Line: l.Line}, true
			}
		}
		if found {
			out = append(out, Source{ID: id, Kind: "definition", Path: loc.File, Line: loc.Line})
		} else {
			out = append(out, Source{ID: id, Kind: "missing"})
		}
		if ucID.MatchString(id) {
			p := path.Join(".docs/design/uc", id+".md")
			if _, err := os.Stat(filepath.Join(m.Root, filepath.FromSlash(p))); err == nil && m.catalogAllows(p) {
				out = append(out, Source{ID: id, Kind: "detail", Path: p})
			}
		}
	}
	return out
}

// Location は、ファイルと行。
type Location struct {
	File string
	Line int
}

// history は、段と、その繰り返しごとの作業履歴と、人の操作を、新しい順で返す。
func (m *Monitor) history(tr *tracked) []HistRow {
	d := m.Workflow.Display
	var rows []HistRow
	iter := map[string]int{}
	hasReportEvents := false
	for _, r := range tr.runs {
		for _, e := range r.log {
			if e["event"] == "review-report" {
				hasReportEvents = true
			}
		}
	}
	reviews := m.reviewFiles(tr)
	bjDone := 0
	for ri := range tr.runs {
		r := &tr.runs[ri]
		var cur *HistRow
		alive := r.mark != nil && m.Alive(*r.mark)
		for _, e := range r.log {
			switch e["event"] {
			case "step-start":
				step := str(e["step"])
				iter[step]++
				rows = append(rows, HistRow{Time: str(e["time"]), Stage: d.StageOfStep(step), Step: step,
					Iteration: iter[step], Role: m.Workflow.StepRole(step), Running: alive})
				cur = &rows[len(rows)-1]
				cur.Findings = stepFindings(tr, r, toInt(e["n"]), step)
			case "output-invalid":
				if cur != nil {
					cur.Invalid++
				}
			case "review-report":
				if cur != nil {
					cur.Links = append(cur.Links, Link{Kind: "review", Path: m.ticketPath(tr.t.ID, str(e["path"]))})
				}
			case "step-end":
				if cur != nil {
					cur.Outcome, cur.Seconds, cur.Running = str(e["outcome"]), int(toFloat(e["seconds"])), false
					// 記録を足す前の実行は、BJ の step の順にレビュー報告の番号を推定する
					if !hasReportEvents && cur.Outcome != "" && m.Workflow.StepRole(cur.Step) == "bj" {
						if bjDone < len(reviews) {
							cur.Links = append(cur.Links, Link{Kind: "review", Path: reviews[bjDone]})
							cur.Inferred = true
						}
						bjDone++
					}
				}
			case "questions-added":
				if cur != nil {
					cur.Links = append(cur.Links, Link{Kind: "questions", Path: m.ticketPath(tr.t.ID, ticket.QuestionsFile)})
				}
			case "stopped", "error":
				reason := str(e["reason"])
				if e["event"] == "error" {
					reason = "error"
				}
				if cur != nil && cur.Stop == "" {
					cur.Stop = reason
					cur.Running = false
				}
			}
		}
	}
	for _, ev := range tr.events {
		if ev.By != "human" {
			continue
		}
		rows = append(rows, HistRow{Time: ev.Time, Stage: d.StageOfState(ev.From), Role: "human", Command: ev.Command, From: ev.From, To: ev.To})
	}
	slices.SortStableFunc(rows, func(a, b HistRow) int { return strings.Compare(b.Time, a.Time) })
	if rows == nil {
		rows = []HistRow{}
	}
	return rows
}

// reviewFiles は、レビュー報告のファイル（番号の順）を返す。
func (m *Monitor) reviewFiles(tr *tracked) []string {
	entries, _ := fs.ReadDir(tr.fsys, path.Join(tr.t.ID, "reviews"))
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, m.ticketPath(tr.t.ID, path.Join("reviews", e.Name())))
		}
	}
	slices.Sort(out)
	return out
}

// stepFindings は、BJ の step の出力の指摘を、ラベルごとに数える。
func stepFindings(tr *tracked, r *runRec, n int, step string) map[string]int {
	b, err := fs.ReadFile(tr.fsys, path.Join(tr.runDir(r), "steps", fmt.Sprintf("%02d-%s", n, step), "out", "result.json"))
	if err != nil {
		return nil
	}
	var res struct {
		Findings []struct {
			Label string `json:"label"`
		} `json:"findings"`
	}
	if json.Unmarshal(b, &res) != nil || len(res.Findings) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, f := range res.Findings {
		counts[f.Label]++
	}
	return counts
}

func toInt(v any) int { return int(toFloat(v)) }

// SourceFile は、/api/source の応答。
type SourceFile struct {
	Path  string   `json:"path"`
	Lines []string `json:"lines"`
}

// ErrOutside は、読める範囲（tickets の下とカタログ）の外のパス。
var ErrOutside = errors.New("読める範囲の外")

// Source は、プロジェクトのルートからの相対パスのファイルを読む。読めるのは tickets の下と、カタログ（ids.scan の下）だけ。
func (m *Monitor) Source(p string) (SourceFile, error) {
	clean := path.Clean(p)
	if p == "" || path.IsAbs(p) || clean != p || strings.HasPrefix(clean, "../") || clean == ".." {
		return SourceFile{}, ErrOutside
	}
	var base, rel string
	if r, ok := strings.CutPrefix(clean, ticket.Dir+"/"); ok {
		base, rel = m.Dir, r
	} else if dir := m.catalogDir(clean); dir != "" {
		base, rel = filepath.Join(m.Root, filepath.FromSlash(dir)), strings.TrimPrefix(clean, dir+"/")
	} else {
		return SourceFile{}, ErrOutside
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return SourceFile{}, err
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return SourceFile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SourceFile{}, err
	}
	if info.IsDir() || info.Size() > sourceLimit {
		return SourceFile{}, fmt.Errorf("%s は読めない（ディレクトリか、大きすぎる）", p)
	}
	// Read は大きさの分を一度に返す約束がないので、ReadFull で全部読む（足りなければ失敗。M2.9）
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return SourceFile{}, fmt.Errorf("%s を読み切れない: %w", p, err)
	}
	return SourceFile{Path: clean, Lines: strings.Split(strings.TrimRight(string(data), "\n"), "\n")}, nil
}

// catalogDir は、パスが入っているカタログのディレクトリ（ids.scan の1つ）を返す。
func (m *Monitor) catalogDir(p string) string {
	if m.Project.IDs == nil || m.Root == "" {
		return ""
	}
	for _, s := range m.Project.IDs.Scan {
		s = path.Clean(s)
		if strings.HasPrefix(p, s+"/") {
			return s
		}
	}
	return ""
}

func (m *Monitor) catalogAllows(p string) bool { return m.catalogDir(p) != "" }
