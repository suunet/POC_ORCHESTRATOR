package ticket_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/ticket"
)

// 質疑のファイルがなければ、未確定の質疑は0件。
func TestQuestions_NoFile(t *testing.T) {
	tk := load(t, fullTicket)
	n, err := tk.UnresolvedQuestions()
	if err != nil || n != 0 {
		t.Errorf("UnresolvedQuestions = %d, %v", n, err)
	}
}

func TestQuestions_AppendAndResolve(t *testing.T) {
	tk := load(t, fullTicket)
	err := tk.AppendQuestions("P2 のレビュー", []ticket.NewQuestion{
		{Summary: "NFR-09 の ctx の値", Body: "ctx に何を入れるか決まっていない。", Mark: "指示書の修正が必要", Evidence: "NFR-09", Suggestion: "§2 に ctx の値を書く"},
		{Summary: "ログの出力先", Body: "標準出力でよいか。"},
	})
	if err != nil {
		t.Fatalf("AppendQuestions: %v", err)
	}
	n, err := tk.UnresolvedQuestions()
	if err != nil || n != 2 {
		t.Fatalf("UnresolvedQuestions = %d, %v, want 2", n, err)
	}
	path := filepath.Join(filepath.Dir(tk.Path), "questions.md")
	data, _ := os.ReadFile(path)
	body := string(data)
	for _, w := range []string{"| Q-01 | NFR-09 の ctx の値 |", "| Q-02 | ログの出力先 |", "## Q-01 NFR-09 の ctx の値", "指示書の修正が必要", "NFR-09", "§2 に ctx の値を書く", "P2 のレビュー"} {
		if !strings.Contains(body, w) {
			t.Errorf("質疑に %q がない:\n%s", w, body)
		}
	}

	// 人が回答して「確定」にした行だけが確定。回答済みは未確定のまま。
	body = strings.Replace(body, "| Q-01 | NFR-09 の ctx の値 | 人 | 未回答 |", "| Q-01 | NFR-09 の ctx の値 | 人 | 確定 |", 1)
	body = strings.Replace(body, "| Q-02 | ログの出力先 | 人 | 未回答 |", "| Q-02 | ログの出力先 | 人 | 回答済み |", 1)
	os.WriteFile(path, []byte(body), 0o644)
	if n, _ := tk.UnresolvedQuestions(); n != 1 {
		t.Errorf("UnresolvedQuestions = %d, want 1", n)
	}

	// 追記は番号を続け、人の書いた行は変えない。
	if err := tk.AppendQuestions("P3 の準備", []ticket.NewQuestion{{Summary: "三つ目", Body: "b"}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "| Q-03 | 三つ目 |") || !strings.Contains(string(data), "| Q-01 | NFR-09 の ctx の値 | 人 | 確定 |") {
		t.Errorf("追記の結果:\n%s", data)
	}
}
