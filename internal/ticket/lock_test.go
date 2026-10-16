package ticket_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gateflow/internal/ticket"
)

// 同じチケットの実行の印（M2.8。M3.1 で flock）：取っている間は取れない。外せば取れる。
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
	if data, err := os.ReadFile(filepath.Join(dir, ticket.LockFile)); err != nil || len(data) != 0 {
		t.Fatalf("外した後の印 = %q, %v（中身は空で、ファイルは残る）", data, err)
	}
	unlock, err = tk.Lock(now)
	if err != nil {
		t.Fatalf("外した後に取れない: %v", err)
	}
	unlock()

	// 前の実行が異常に終わって残った印（flock がない）や、中身の読めない印は、そのまま取れる（M3.1）
	for _, old := range []string{`{"pid":999999,"start":"Thu Jan  1 00:00:00 1970","since":"2026-10-13T00:00:00Z"}`, "{"} {
		os.WriteFile(filepath.Join(dir, ticket.LockFile), []byte(old), 0o644)
		unlock, err = tk.Lock(now)
		if err != nil {
			t.Fatalf("残った印 %q を取れない: %v", old, err)
		}
		unlock()
	}
}
