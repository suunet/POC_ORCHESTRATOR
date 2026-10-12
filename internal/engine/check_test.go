package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/builtins"
	"gateflow/internal/engine"
	"gateflow/internal/workflow"
)

// 指示書のステータスが遷移表の状態の一覧にないチケットを報告する。表にある状態なら報告しない。
func TestCheckTicketStates(t *testing.T) {
	root := t.TempDir()
	write := func(id, status string) {
		p := filepath.Join(root, ".docs/_stash/tickets", id, "ticket.md")
		os.MkdirAll(filepath.Dir(p), 0o755)
		body := "# " + id + " 題名\n\nステータス: " + status + "\n\n| 項目 | 値 |\n| --- | --- |\n| 種別 | 実装 |\n| ブランチ | feature/x |\n"
		os.WriteFile(p, []byte(body), 0o644)
	}
	for id, status := range map[string]string{"T-001": "draft", "T-002": "ready-for-review", "T-003": "done", "T-004": "cancelled"} {
		write(id, status)
	}
	write("T-005", "in-uat") // 指示書の状態としてはあるが、遷移表 aidd-impl にない
	os.MkdirAll(filepath.Join(root, ".docs/_stash/tickets", "T-006", "runs"), 0o755)

	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatal(err)
	}
	n, findings, err := engine.CheckTicketStates(root, wf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || len(findings) != 1 {
		t.Fatalf("n = %d, findings = %+v", n, findings)
	}
	f := findings[0]
	if f.Ticket != "T-005" || f.Status != "in-uat" || f.Line != 3 || f.File != ".docs/_stash/tickets/T-005/ticket.md" ||
		!strings.Contains(f.Message, "遷移表に移行の行を足すか、人がステータスを直す") {
		t.Errorf("指摘 = %+v", f)
	}

	// チケットがなければ、指摘もない
	if n, findings, err := engine.CheckTicketStates(t.TempDir(), wf); err != nil || n != 0 || len(findings) != 0 {
		t.Errorf("空: n = %d, findings = %+v, err = %v", n, findings, err)
	}
}
