package ticket_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/ticket"
)

const sample = `# T-001 記事を公開する

ステータス: ready-to-implement

| 項目 | 値 |
| --- | --- |
| 種別 | 実装 |
| 対象ID | UC-011 |
| ブランチ | feature/t-001-publish |
| 起票日 | 2026-10-11 |

## §1 目的

- 記事を公開できるようにする
`

func writeTicket(t *testing.T, id, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ticket.Dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ticket.File), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoad(t *testing.T) {
	root := writeTicket(t, "T-001", sample)
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tk.ID != "T-001" || tk.Title != "記事を公開する" || tk.Status != ticket.ReadyToImplement ||
		tk.Kind != ticket.KindImplementation || tk.Branch != "feature/t-001-publish" {
		t.Errorf("ticket = %+v", tk)
	}
	if !strings.Contains(tk.Body, "§1 目的") {
		t.Error("Body に本文がない")
	}
	if tk.Path != filepath.Join(root, ticket.Dir, "T-001", ticket.File) {
		t.Errorf("Path = %s", tk.Path)
	}
}

func TestLoad_Invalid(t *testing.T) {
	cases := []struct {
		name string
		id   string
		body string
		want []string
	}{
		{"ID の形が違う", "T1", sample, []string{"T1"}},
		{"見出しの ID が違う", "T-002", sample, []string{"T-001"}},
		{"ステータスがない", "T-001", strings.Replace(sample, "ステータス: ready-to-implement\n", "", 1), []string{"ステータス"}},
		{"ステータスが2行ある", "T-001", strings.Replace(sample, "ステータス: ready-to-implement\n", "ステータス: draft\nステータス: done\n", 1), []string{"ステータス"}},
		{"知らないステータス", "T-001", strings.Replace(sample, "ready-to-implement", "almost-done", 1), []string{"almost-done"}},
		{"ブランチがない", "T-001", strings.Replace(sample, "| ブランチ | feature/t-001-publish |\n", "", 1), []string{"ブランチ"}},
		{"種別が選択肢にない", "T-001", strings.Replace(sample, "| 種別 | 実装 |", "| 種別 | 実装 ／ ドキュメント |", 1), []string{"種別"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTicket(t, tc.id, tc.body)
			_, err := ticket.Load(root, tc.id)
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestSetStatus(t *testing.T) {
	root := writeTicket(t, "T-001", sample)
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	if err := tk.SetStatus(ticket.ReadyToImplement, ticket.Implementing); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if tk.Status != ticket.Implementing {
		t.Errorf("Status = %s", tk.Status)
	}
	again, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != ticket.Implementing {
		t.Errorf("ファイルの Status = %s", again.Status)
	}
	// ステータスの行のほかは変わらない。
	data, _ := os.ReadFile(tk.Path)
	if string(data) != strings.Replace(sample, "ready-to-implement", "implementing", 1) {
		t.Errorf("ファイルの中身が変わった:\n%s", data)
	}
}

// ファイルのステータスが想定と違えば書き換えない（人が手で変えた場合など）。
func TestSetStatus_Conflict(t *testing.T) {
	root := writeTicket(t, "T-001", sample)
	tk, err := ticket.Load(root, "T-001")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tk.Path, []byte(strings.Replace(sample, "ready-to-implement", "cancelled", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tk.SetStatus(ticket.ReadyToImplement, ticket.Implementing); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v, want 現在のステータス cancelled を含む誤り", err)
	}
}
