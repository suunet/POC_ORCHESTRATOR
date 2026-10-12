package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gateflow/internal/ticket"
	"gateflow/internal/workflow"
)

// StateFinding は、チケットの状態の検査の指摘（必須）。
type StateFinding struct {
	Ticket  string
	Status  string
	File    string // プロジェクトのルートからのパス
	Line    int
	Message string
}

// CheckTicketStates は、指示書のステータスが遷移表の状態の一覧にないチケットを報告する。
// ステータスは gateflow だけが書くので、こうなるのは遷移表を変えて古い状態が取り残されたときに限る。
// cancel の特例は作らず、遷移表に移行の行を足すか、人がステータスを直す（M2.5、人の決定）。
// 戻り値の n は、検査したチケットの数。
func CheckTicketStates(root string, wf *workflow.Workflow) (n int, findings []StateFinding, err error) {
	entries, err := os.ReadDir(filepath.Join(root, ticket.Dir))
	if os.IsNotExist(err) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(ticket.Dir, en.Name(), ticket.File))
		t, err := ticket.Load(root, en.Name())
		if err != nil {
			if os.IsNotExist(err) || strings.Contains(err.Error(), "読めない") {
				continue // 指示書のないディレクトリ（作業の残りなど）は対象にしない
			}
			findings = append(findings, StateFinding{Ticket: en.Name(), File: rel, Message: err.Error()})
			continue
		}
		n++
		if slices.Contains(wf.States, string(t.Status)) {
			continue
		}
		findings = append(findings, StateFinding{Ticket: t.ID, Status: string(t.Status), File: rel, Line: statusLine(t.Body),
			Message: fmt.Sprintf("%s の状態 %s は遷移表 %s にない。遷移表に移行の行を足すか、人がステータスを直す", t.ID, t.Status, wf.Name)})
	}
	return n, findings, nil
}

func statusLine(body string) int {
	for i, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "ステータス:") {
			return i + 1
		}
	}
	return 0
}
