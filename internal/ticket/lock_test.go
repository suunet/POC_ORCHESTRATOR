package ticket_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/internal/ticket"
)

// 同じチケットの実行の印（M2.8）：取っている間は取れない。外せば取れる。プロセスがない印は取り直す。
func TestLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".docs/_stash/tickets/T-001")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "ticket.md"), []byte("# T-001 テスト\n\nステータス: draft\n\n| 項目 | 値 |\n| --- | --- |\n| 種別 | 実装 |\n| ブランチ | feature/x |\n"), 0o644)
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	unlock, err := tk.Lock(now)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := tk.Lock(now); !errors.Is(err, ticket.ErrLocked) || !strings.Contains(err.Error(), "プロセス") {
		t.Fatalf("2つ目の Lock = %v, want ErrLocked", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ticket.LockFile)); !os.IsNotExist(err) {
		t.Fatal("外した後も印が残っている")
	}

	// 前の実行が異常に終わって残った印（プロセスがない）は、取り直す
	stale, _ := json.Marshal(ticket.Running{PID: 999999, Start: "Thu Jan  1 00:00:00 1970", Since: "2026-10-13T00:00:00Z"})
	os.WriteFile(filepath.Join(dir, ticket.LockFile), stale, 0o644)
	unlock, err = tk.Lock(now)
	if err != nil {
		t.Fatalf("残った印を取り直せない: %v", err)
	}
	unlock()

	// 中身が読めない印（書きかけ）は、取り違えないよう生きているものとして扱う
	os.WriteFile(filepath.Join(dir, ticket.LockFile), []byte("{"), 0o644)
	if _, err := tk.Lock(now); !errors.Is(err, ticket.ErrLocked) {
		t.Fatalf("読めない印 = %v, want ErrLocked", err)
	}
}
