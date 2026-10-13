package monitor_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/builtins"
	"gateflow/internal/config"
	"gateflow/internal/monitor"
	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
)

var now = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func ticketMD(id, status, extra string) string {
	return fmt.Sprintf("# %s 題名 %s\n\nステータス: %s\n\n| 項目 | 値 |\n| --- | --- |\n| 種別 | 実装 |\n| 対象ID | %s |\n| ブランチ | feature/x |\n\n%s", id, id, status, "NFR-01、UC-001", extra)
}

func logLines(evs ...map[string]any) string {
	var b strings.Builder
	for _, e := range evs {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// p2Run は、P2 を1回走らせて差し戻した実行の記録を書く。
func p2Run(write func(string, string), id, run string, minute int) {
	r := id + "/runs/" + run + "/"
	start := fmt.Sprintf("2026-10-14T07:%02d:00Z", minute)
	end := fmt.Sprintf("2026-10-14T07:%02d:30Z", minute)
	write(r+"state.json", fmt.Sprintf(`{"ticket":%q,"workflow":"aidd-impl","step":"p2-review","steps":1,"status":"draft"}`, id))
	write(r+"log.jsonl", logLines(
		map[string]any{"time": start, "event": "step-start", "step": "p2-review", "n": 1},
		map[string]any{"time": end, "event": "review-report", "step": "p2-review", "path": "reviews/0" + run[len(run)-1:] + "-p2-review.md"},
		map[string]any{"time": end, "event": "step-end", "step": "p2-review", "outcome": "needs_fix", "seconds": 30},
		map[string]any{"time": end, "event": "questions-added", "step": "p2-review", "ids": []string{"Q-01"}},
		map[string]any{"time": end, "event": "gate", "status": "draft"}))
	write(r+"steps/01-p2-review/out/result.json", `{"outcome":"needs_fix","summary":"s","findings":[{"label":"MUST"},{"label":"Q"},{"label":"IMO"}]}`)
}

const questions = "| # | 質問（要約） | 回答者 | 状態 | 反映先 | 決定者 | 根拠 |\n| --- | --- | --- | --- | --- | --- | --- |\n" +
	"| Q-01 | 公開のアドレス | 人 | 未回答 |  | 人 |  |\n| Q-02 | 使用中のポート | 人 | 未回答 |  | 人 |  |\n| Q-03 | 済んだもの | 人 | 確定 | x | 人 |  |\n\n---\n\n" +
	"## Q-01 公開のアドレス\n\n出どころ：P2 のレビュー\n\n**質問**\n\nどのアドレスに公開しますか。\n\n選択肢：127.0.0.1 だけ／0.0.0.0\n\n推奨：127.0.0.1 だけ\n\n**回答**\n\n\n"

// fixture は、プロジェクトのルート（tickets とカタログ、tickets の外の .env）を作り、ルートを返す。
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ticket.Dir)
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(root, ".docs/design/uc"), 0o755)
	os.WriteFile(filepath.Join(root, ".docs/design/non-functional-requirement.md"), []byte("# 非機能要求\n\n| ID | 分類 | 要求 |\n| --- | --- | --- |\n| NFR-01 | x | y |\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".docs/design/buc.md"), []byte("# 業務\n\n| BUC | UC |\n| --- | --- |\n| a | UC-001（死活） |\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".docs/design/uc/UC-001.md"), []byte("# UC-001\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644)

	// T-010：P2 で2回差し戻され、質問2件（あなたの番・質問）
	write("T-010/ticket.md", ticketMD("T-010", "draft", ""))
	write("T-010/questions.md", questions)
	write("T-010/events.jsonl", `{"time":"2026-10-14T07:00:00Z","by":"human","command":"ready","from":"draft","to":"ready-to-prepare"}`+"\n"+
		`{"time":"2026-10-14T07:10:30Z","by":"gateflow","from":"ready-to-prepare","to":"draft"}`+"\n")
	p2Run(write, "T-010", "20261014-070001", 1)
	p2Run(write, "T-010", "20261014-070002", 10)
	write("T-010/reviews/01-p2-review.md", "# r1\n")
	write("T-010/reviews/02-p2-review.md", "# r2\n")

	// T-011：実行中（P5。出力の不正1回、プロキシの拒否1件）
	write("T-011/ticket.md", ticketMD("T-011", "implementing", ""))
	r := "T-011/runs/20261014-083000/"
	write(r+"running.json", `{"pid":4242,"start":"S","since":"2026-10-14T08:30:00Z"}`)
	write(r+"state.json", `{"ticket":"T-011","workflow":"aidd-impl","step":"p5","steps":1,"status":"implementing"}`)
	write(r+"log.jsonl", logLines(map[string]any{"time": "2026-10-14T08:50:00Z", "event": "step-start", "step": "p5", "n": 1},
		map[string]any{"time": "2026-10-14T08:51:00Z", "event": "output-invalid", "step": "p5"}))
	write(r+"proxy.log", `{"decision":"deny","target":"evil.example:443"}`+"\n")

	// T-019：実行中だが、AI の呼び出しの順番を待っている（M2.9）
	write("T-019/ticket.md", ticketMD("T-019", "ready-to-prepare", ""))
	r = "T-019/runs/20261014-085500/"
	write(r+"running.json", `{"pid":4242,"start":"S","since":"2026-10-14T08:55:00Z"}`)
	write(r+"state.json", `{"ticket":"T-019","workflow":"aidd-impl","step":"p2-review","steps":1,"status":"ready-to-prepare"}`)
	write(r+"log.jsonl", logLines(map[string]any{"time": "2026-10-14T08:55:00Z", "event": "step-start", "step": "p2-review", "n": 1},
		map[string]any{"time": "2026-10-14T08:55:01Z", "event": "agent-start", "step": "p2-review", "attempt": 1},
		map[string]any{"time": "2026-10-14T08:55:01Z", "event": "agent-wait", "step": "p2-review", "holderProject": "sample", "holderTicket": "T-011", "holderStep": "p5"}))

	// T-012：検証で進展なし（止まった。再開は実装から）
	write("T-012/ticket.md", ticketMD("T-012", "implementing", ""))
	r = "T-012/runs/20261014-080000/"
	write(r+"state.json", `{"ticket":"T-012","workflow":"aidd-impl","step":"verify","steps":4,"status":"implementing","stopped":{"reason":"no-progress","detail":"同じ失敗"},"resume":"p5"}`)
	write(r+"log.jsonl", logLines(
		map[string]any{"time": "2026-10-14T08:00:00Z", "event": "step-start", "step": "p5", "n": 1},
		map[string]any{"time": "2026-10-14T08:01:00Z", "event": "step-end", "step": "p5", "outcome": "done", "seconds": 60},
		map[string]any{"time": "2026-10-14T08:01:00Z", "event": "step-start", "step": "verify", "n": 2},
		map[string]any{"time": "2026-10-14T08:02:00Z", "event": "step-end", "step": "verify", "outcome": "failed", "seconds": 60},
		map[string]any{"time": "2026-10-14T08:02:00Z", "event": "step-start", "step": "p5", "n": 3},
		map[string]any{"time": "2026-10-14T08:03:00Z", "event": "step-end", "step": "p5", "outcome": "done", "seconds": 60},
		map[string]any{"time": "2026-10-14T08:03:00Z", "event": "step-start", "step": "verify", "n": 4},
		map[string]any{"time": "2026-10-14T08:04:00Z", "event": "step-end", "step": "verify", "outcome": "failed", "seconds": 60},
		map[string]any{"time": "2026-10-14T08:04:00Z", "event": "stopped", "reason": "no-progress"}))
	write(r+"steps/04-verify/verify.log", "## test（終了コード 1）\n\n--- FAIL: TestRefresh (0.01s)\n    refresh_test.go:12: got 200, want 401\nFAIL\n")

	// T-013：承認待ち（カタログの差分 +2 −1、IMO 2件のうち1件を計画に入れた）
	plan := "## §3 変更前の現状（P3でAIが書く）\n\nc\n\n## §4 実行計画（P3でAIが書く）\n\n### §4.1 方針\n\np\n\n**P2 の IMO への対応**\n\n| IMO | 対応 | 内容 |\n| --- | --- | --- |\n| a | x | y |\n| b | x | y |\n\n" +
		"### §4.2 受入条件\n\n| 受入条件 | ユースケース | 内容 |\n| --- | --- | --- |\n| UC-001-AC-01 | UC-001 | c |\n\n" +
		"### §4.3 テストケース\n\n| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |\n| --- | --- | --- | --- |\n| T1 | 単体 | x | y |\n| T2 | シナリオ | x | y |\n\n" +
		"### §4.4 更新する文書\n\n| ファイル | 更新の内容 |\n| --- | --- |\n| `a.md` | b |\n"
	write("T-013/ticket.md", ticketMD("T-013", "ready-to-approve", plan))
	r = "T-013/runs/20261014-060000/"
	write(r+"state.json", `{"ticket":"T-013","workflow":"aidd-impl","step":"p3-prepare","steps":2,"status":"ready-to-approve"}`)
	write(r+"log.jsonl", logLines(map[string]any{"time": "2026-10-14T06:30:00Z", "event": "gate", "status": "ready-to-approve"}))
	write(r+"arb-report.md", "# r\n\n```diff\n--- a/x\n+++ b/x\n+| NFR-02 |\n+| NFR-03 |\n-| old |\n```\n")
	write(r+"steps/02-p3-prepare/out/result.json", `{"imo":[{"index":0,"decision":"planned","detail":"x"},{"index":1,"decision":"deferred","detail":"y"}]}`)

	// T-014：レビュー待ち。T-015：run を待つ。T-018：ready を待つ
	write("T-014/ticket.md", ticketMD("T-014", "ready-for-review", ""))
	write("T-015/ticket.md", ticketMD("T-015", "ready-to-prepare", ""))
	write("T-018/ticket.md", ticketMD("T-018", "draft", ""))

	// T-016：done（最近終わったもの）。T-017：cancelled（出さない）
	write("T-016/ticket.md", ticketMD("T-016", "done", ""))
	write("T-016/events.jsonl", `{"time":"2026-10-14T05:00:00Z","by":"human","command":"ready","from":"draft","to":"ready-to-prepare"}`+"\n"+
		`{"time":"2026-10-14T06:00:00Z","by":"human","command":"done","from":"ready-for-review","to":"done"}`+"\n")
	write("T-016/questions.md", questions)
	write("T-017/ticket.md", ticketMD("T-017", "cancelled", ""))
	return root
}

func newMonitor(t *testing.T, root string) *monitor.Monitor {
	t.Helper()
	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatal(err)
	}
	ids := &config.IDs{Scan: []string{".docs/design"}, Prefixes: []config.Prefix{
		{Prefix: "NFR", Pattern: "NFR-[0-9]{2}", Files: []string{".docs/design/non-functional-requirement.md"}, Find: "table-first-column"},
		{Prefix: "UC", Pattern: "UC-[0-9]{3}", Files: []string{".docs/design/buc.md"}, Find: "table-column:UC"},
	}}
	return &monitor.Monitor{Dir: filepath.Join(root, ticket.Dir), Root: root, Project: &config.Project{Name: "sample", IDs: ids}, Workflow: wf,
		Now: func() time.Time { return now }, Alive: func(r ticket.Running) bool { return r.PID == 4242 }}
}

func status(t *testing.T, root string) monitor.Status {
	t.Helper()
	st, err := newMonitor(t, root).Status()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// あなたの番は、止まった → 承認待ち → 質問 → そのほかの順。中身（失敗の要約、件数、1件目の質問）も出す。
func TestStatus_Cards(t *testing.T) {
	st := status(t, fixture(t))
	var got []string
	for _, c := range st.Cards {
		got = append(got, c.Kind+" "+c.Ticket)
	}
	if want := "stopped T-012,approve T-013,questions T-010,other T-014"; strings.Join(got, ",") != want {
		t.Fatalf("カード = %v, want %s", got, want)
	}
	stop := st.Cards[0]
	if stop.Reason != "no-progress" || stop.Failure == nil || stop.Failure.Tests[0] != "TestRefresh" || !strings.Contains(strings.Join(stop.Failure.Lines, "\n"), "want 401") {
		t.Errorf("止まった = %+v, 失敗 %+v", stop, stop.Failure)
	}
	if len(stop.Links) == 0 || stop.Links[0].Kind != "verify-log" || !strings.HasSuffix(stop.Links[0].Path, "steps/04-verify/verify.log") {
		t.Errorf("止まったのリンク = %+v", stop.Links)
	}
	a := st.Cards[1].Approve
	if a == nil || a.Acceptance != 1 || a.TestCases != 2 || a.Sheets != 1 || a.DiffAdd != 2 || a.DiffDel != 1 || a.IMO != 2 || a.IMOPlanned != 1 {
		t.Errorf("承認待ち = %+v", a)
	}
	q := st.Cards[2].Questions
	if q == nil || q.Count != 2 || q.First.ID != "Q-01" || len(q.First.Options) != 2 || q.First.Recommend != "127.0.0.1 だけ" ||
		!strings.Contains(q.First.Body, "どのアドレス") || len(q.Others) != 1 || q.Others[0].ID != "Q-02" {
		t.Errorf("質問 = %+v, 1件目 %+v", q, q.First)
	}
	if st.Cards[2].Step != "p2-review" || st.Cards[2].Iteration != 2 {
		t.Errorf("質問の段と回数 = %s %d", st.Cards[2].Step, st.Cards[2].Iteration)
	}
}

// 流れは、AI が作業中 → あなたの番 → 待機、塊の中は番号順。done・cancelled は出さない。段の状態と往復の回数。
func TestStatus_Lanes(t *testing.T) {
	st := status(t, fixture(t))
	var got []string
	for _, l := range st.Lanes {
		got = append(got, l.Group+" "+l.Ticket+" "+l.Note)
	}
	want := "run T-011 run,run T-019 wait-ai,human T-010 questions,human T-012 stopped,human T-013 approve,human T-014 review,wait T-015 wait-run,wait T-018 wait-ready"
	if strings.Join(got, ",") != want {
		t.Fatalf("流れ = %v\nwant %s", got, want)
	}
	byID := map[string]monitor.Lane{}
	for _, l := range st.Lanes {
		byID[l.Ticket] = l
	}
	// T-010：要件の審査を2回走らせ、その段で人の番（質問）
	l := byID["T-010"]
	if l.Current != 1 || l.Segments[0].Kind != "done" || l.Segments[1].Kind != "human" || l.Segments[1].Count != 2 || l.Segments[2].Kind != "todo" || l.Count != 2 {
		t.Errorf("T-010 = %+v", l)
	}
	// T-012：止まった（再開は実装から）。実装2回・検証2回
	l = byID["T-012"]
	if l.Current != 4 || l.Segments[4].Kind != "stop" || l.Segments[4].Count != 2 || l.Segments[5].Count != 2 || l.Role != "dev" || l.Reason != "no-progress" {
		t.Errorf("T-012 = %+v", l)
	}
	// T-011：実装を AI が作業中（走っている回も数える）
	l = byID["T-011"]
	if l.Current != 4 || l.Segments[4].Kind != "run" || l.Segments[4].Count != 1 || l.Role != "dev" || l.Started == "" {
		t.Errorf("T-011 = %+v", l)
	}
	if len(st.Running) != 2 || st.Running[0].Ticket != "T-011" || st.Running[0].Invalid != 1 || st.Running[0].Denied != 1 || st.Running[0].Iteration != 1 {
		t.Errorf("AI が作業中 = %+v", st.Running)
	}
	if st.Display == nil || len(st.Display.Stages) != 9 {
		t.Error("表示名（display）を返していない")
	}
}

// 最近終わったもの：かかった時間、要件の審査の回数、人が決めた質疑の件数。
func TestStatus_Recent(t *testing.T) {
	st := status(t, fixture(t))
	if len(st.Recent) != 1 || st.Recent[0].Ticket != "T-016" || st.Recent[0].Seconds != 3600 || st.Recent[0].HumanDecided != 3 || len(st.DoneTimes) != 1 {
		t.Errorf("最近終わったもの = %+v, done %v", st.Recent, st.DoneTimes)
	}
}

// 詳細：正本（指示書、定義の行、UC の詳細）と、作業履歴（新しい順、レビュー報告と質疑への参照、人の操作）。
func TestTicket_Detail(t *testing.T) {
	m := newMonitor(t, fixture(t))
	d, err := m.Ticket("T-010")
	if err != nil {
		t.Fatal(err)
	}
	var src []string
	for _, s := range d.Sources {
		src = append(src, fmt.Sprintf("%s %s %s:%d", s.Kind, s.ID, s.Path, s.Line))
	}
	want := "ticket  .docs/_stash/tickets/T-010/ticket.md:0,definition NFR-01 .docs/design/non-functional-requirement.md:5,definition UC-001 .docs/design/buc.md:5,detail UC-001 .docs/design/uc/UC-001.md:0"
	if strings.Join(src, ",") != want {
		t.Errorf("正本 = %v\nwant %s", src, want)
	}
	if len(d.History) != 3 {
		t.Fatalf("作業履歴 = %+v", d.History)
	}
	h := d.History[0] // 新しい順：2回目の P2
	if h.Step != "p2-review" || h.Iteration != 2 || h.Role != "bj" || h.Outcome != "needs_fix" || h.Findings["MUST"] != 1 || h.Seconds != 30 ||
		len(h.Links) != 2 || h.Links[0].Path != ".docs/_stash/tickets/T-010/reviews/02-p2-review.md" || h.Links[1].Kind != "questions" || h.Inferred {
		t.Errorf("1行目 = %+v", h)
	}
	if last := d.History[2]; last.Role != "human" || last.Command != "ready" {
		t.Errorf("人の操作 = %+v", last)
	}
	if _, err := m.Ticket("../x"); err == nil {
		t.Error("チケットの ID の形を確かめていない")
	}
}

// 本文は、tickets の下とカタログだけを読む。範囲の外（..、.env、絶対パス）は断る。
func TestSource_Range(t *testing.T) {
	m := newMonitor(t, fixture(t))
	f, err := m.Source(".docs/design/non-functional-requirement.md")
	if err != nil || len(f.Lines) != 5 || f.Lines[4] != "| NFR-01 | x | y |" {
		t.Fatalf("カタログ = %+v, %v", f, err)
	}
	if _, err := m.Source(".docs/_stash/tickets/T-012/runs/20261014-080000/steps/04-verify/verify.log"); err != nil {
		t.Errorf("tickets の下の記録を読めない: %v", err)
	}
	for _, p := range []string{".env", "../x", "/etc/passwd", ".docs/design/../../.env", ".docs/_stash/tickets/../../../.env", ""} {
		if _, err := m.Source(p); err == nil || !strings.Contains(err.Error(), "範囲の外") {
			t.Errorf("Source(%q) = %v, want 範囲の外", p, err)
		}
	}
}

// 口は GET だけ。/api/ticket と /api/source を足した（M2.8）。
func TestHandler_Routes(t *testing.T) {
	h := monitor.Handler(newMonitor(t, fixture(t)), []byte("<html>"))
	for _, c := range []struct {
		method, url string
		code        int
		has         string
	}{
		{http.MethodGet, "/", 200, "<html>"},
		{http.MethodGet, "/api/status", 200, `"lanes"`},
		{http.MethodGet, "/api/ticket?id=T-010", 200, `"history"`},
		{http.MethodGet, "/api/ticket?id=T-999", 404, ""},
		{http.MethodGet, "/api/source?path=.docs/design/buc.md", 200, "UC-001"},
		{http.MethodGet, "/api/source?path=.env", 403, ""},
		{http.MethodPost, "/api/status", 405, ""},
		{http.MethodGet, "/api/other", 404, ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.url, nil))
		b, _ := io.ReadAll(w.Result().Body)
		if w.Code != c.code || !strings.Contains(string(b), c.has) {
			t.Errorf("%s %s = %d %q", c.method, c.url, w.Code, string(b)[:min(len(b), 80)])
		}
	}
}

// tickets のディレクトリがなければ、空の状態を返す。
func TestStatus_NoTicketsDir(t *testing.T) {
	st, err := newMonitor(t, t.TempDir()).Status()
	if err != nil || len(st.Lanes) != 0 || len(st.Cards) != 0 {
		t.Errorf("st = %+v, err = %v", st, err)
	}
}

// AI の呼び出しの順番を待っている run は、流れで「AI の順番待ち」（帯は待ちの色）、AI が作業中の欄に待ちとして出す（M2.9）。
func TestStatus_AgentWaiting(t *testing.T) {
	st := status(t, fixture(t))
	var lane *monitor.Lane
	for i := range st.Lanes {
		if st.Lanes[i].Ticket == "T-019" {
			lane = &st.Lanes[i]
		}
	}
	if lane == nil || lane.Note != "wait-ai" || lane.Holder != "sample の T-011" || lane.Segments[lane.Current].Kind != "wait" {
		t.Fatalf("流れ = %+v", lane)
	}
	for _, r := range st.Running {
		if r.Ticket == "T-019" && (!r.Waiting || r.WaitSince == "" || r.Holder != "sample の T-011") {
			t.Errorf("AI が作業中 = %+v", r)
		}
		if r.Ticket == "T-011" && r.Waiting {
			t.Error("T-011 は待っていない")
		}
	}
}
