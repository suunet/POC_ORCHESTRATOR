package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
)

// EventsFile は、指示書の隣の、gateflow がステータスを書いた記録（関門のコマンドと、gateflow の遷移）。
// 手での書き換えの検出と、待ち行列の順（承認の古い順）に使う。
const EventsFile = "events.jsonl"

// Event は、ステータスを書いた記録の1件。
type Event struct {
	Time    string `json:"time"`
	By      string `json:"by"`                // human（関門のコマンド）か gateflow
	Command string `json:"command,omitempty"` // 関門のコマンド
	From    string `json:"from"`
	To      string `json:"to"`
}

// Checks は、関門の機械検証のうち、指示書の外（プロジェクトの git など）を見るもの。
type Checks struct {
	// BranchMerged は、作業ブランチの先頭が main に含まれている（祖先である）かを確かめ、含まれていなければ理由を返す
	BranchMerged func(branch string) error
	// MainIDsClean は、main の ID 整合チェックに必須の指摘がないかを確かめ、あれば理由を返す（M2.9）
	MainIDsClean func() error
}

// Human は、関門のコマンドを実行する。機械検証（guards）に合格したときだけステータスを書く。
// hold・unhold は、ステータスを変えずに保留の印を付け外しする。
func Human(t *ticket.Ticket, wf *workflow.Workflow, command string, now time.Time, checks Checks) error {
	status := string(t.Status)
	switch command {
	case "hold", "unhold":
		if err := t.SetHold(command == "hold"); err != nil {
			return err
		}
		return RecordEvent(t, "human", command, status, status, now)
	}
	h, ok := wf.HumanCommand(command, status)
	if !ok {
		return fmt.Errorf("%s は状態 %s では %s できない", t.ID, status, command)
	}
	for _, g := range h.Guards {
		if err := checkGuard(t, g, checks); err != nil {
			return fmt.Errorf("%s の %s の機械検証に不合格：%w", t.ID, command, err)
		}
	}
	if err := t.SetStatus(t.Status, ticket.Status(h.To)); err != nil {
		return err
	}
	return RecordEvent(t, "human", command, status, h.To, now)
}

func checkGuard(t *ticket.Ticket, guard string, checks Checks) error {
	switch guard {
	case "questions-resolved":
		n, err := t.UnresolvedQuestions()
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("未確定の質疑が %d 件ある（%s）", n, filepath.Join(filepath.Dir(t.Path), ticket.QuestionsFile))
		}
	case "approval-checked":
		if !t.ApprovalChecked() {
			return fmt.Errorf("§4.6 のチェックがすべて入っていない")
		}
	case "host-checked":
		if t.HasHostCheck() && !t.HostCheckRecorded() {
			return fmt.Errorf("§4.5 にホストでの確かめがあるのに、§5 に結果が書かれていない")
		}
	case "ids-clean":
		if checks.MainIDsClean == nil {
			return fmt.Errorf("main の ID 整合チェックを行う手段がない")
		}
		if err := checks.MainIDsClean(); err != nil {
			return err
		}
	case "branch-merged":
		// NOTE: マージそのものは人が git で行う。gateflow は main に書き込まず、含まれていることだけを確かめる
		if checks.BranchMerged == nil {
			return fmt.Errorf("作業ブランチのマージを確かめる手段がない")
		}
		if err := checks.BranchMerged(t.Branch); err != nil {
			return err
		}
	default:
		return fmt.Errorf("機械検証 %s は gateflow にない", guard)
	}
	return nil
}

// RecordEvent は、ステータスを書いた記録を足す。
func RecordEvent(t *ticket.Ticket, by, command, from, to string, now time.Time) error {
	line, err := json.Marshal(Event{Time: now.UTC().Format(time.RFC3339), By: by, Command: command, From: from, To: to})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(t.Path), EventsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Events は、ステータスを書いた記録を古い順に返す。
func Events(t *ticket.Ticket) []Event {
	f, err := os.Open(filepath.Join(filepath.Dir(t.Path), EventsFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// LastEvent は、最後の記録を返す。
func LastEvent(t *ticket.Ticket) (Event, bool) {
	evs := Events(t)
	if len(evs) == 0 {
		return Event{}, false
	}
	return evs[len(evs)-1], true
}
