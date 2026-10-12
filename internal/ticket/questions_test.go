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
	_, err := tk.AppendQuestions("P2 のレビュー", []ticket.NewQuestion{
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
	if _, err := tk.AppendQuestions("P3 の準備", []ticket.NewQuestion{{Summary: "三つ目", Body: "b"}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "| Q-03 | 三つ目 |") || !strings.Contains(string(data), "| Q-01 | NFR-09 の ctx の値 | 人 | 確定 |") {
		t.Errorf("追記の結果:\n%s", data)
	}
}

// ARB の裁定は決定者 ARB・状態「確定」で書き、人へ回すものは決定者 人・未回答で書く。番号を返す。
func TestQuestions_ARBRows(t *testing.T) {
	tk := load(t, fullTicket)
	ids, err := tk.AppendQuestions("ARB の裁定", []ticket.NewQuestion{
		{Summary: "裁定した", Body: "b", Decider: "ARB", Basis: "カタログ：UC-001", Answer: "こう決まる", Reflected: "指示書 §2"},
		{Summary: "人へ", Body: "q", Mark: "人へ回す（基準：範囲）"},
	})
	if err != nil || strings.Join(ids, ",") != "Q-01,Q-02" {
		t.Fatalf("ids = %v, err = %v", ids, err)
	}
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(tk.Path), ticket.QuestionsFile))
	st := ticket.QuestionStates(data)
	if st["Q-01"] != (ticket.QuestionInfo{State: "確定", Decider: "ARB"}) || st["Q-02"] != (ticket.QuestionInfo{State: "未回答", Decider: "人"}) {
		t.Errorf("状態 = %+v\n%s", st, data)
	}
	for _, w := range []string{"| Q-01 | 裁定した | ARB | 確定 | 指示書 §2 | ARB | カタログ：UC-001 |", "こう決まる（決定者：ARB。根拠：カタログ：UC-001）"} {
		if !strings.Contains(string(data), w) {
			t.Errorf("%q がない:\n%s", w, data)
		}
	}
	if n, _ := tk.UnresolvedQuestions(); n != 1 {
		t.Errorf("未確定 = %d", n)
	}
}

// 決定者と根拠の列がない古い表（M2.6 より前）は、書き足すときに2列を足し、既存の行の決定者を人にする。
func TestQuestions_UpgradeOldTable(t *testing.T) {
	tk := load(t, fullTicket)
	old := "# T-002 質疑\n\n## 一覧\n\n| # | 質問（要約） | 回答者 | 状態 | 反映先 |\n| --- | --- | --- | --- | --- |\n| Q-01 | 古い | 人 | 確定 | x |\n"
	path := filepath.Join(filepath.Dir(tk.Path), ticket.QuestionsFile)
	os.WriteFile(path, []byte(old), 0o644)
	if _, err := tk.AppendQuestions("ARB の裁定", []ticket.NewQuestion{{Summary: "新しい", Body: "b", Decider: "ARB", Basis: "既決：Q-01", Answer: "a"}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, w := range []string{"| # | 質問（要約） | 回答者 | 状態 | 反映先 | 決定者 | 根拠 |", "| --- | --- | --- | --- | --- | --- | --- |", "| Q-01 | 古い | 人 | 確定 | x | 人 | |", "| Q-02 | 新しい | ARB | 確定 |"} {
		if !strings.Contains(string(data), w) {
			t.Errorf("%q がない:\n%s", w, data)
		}
	}
}

// §2 の本文を置き換える。見出しの行は受け付けない。
func TestWriteSection2(t *testing.T) {
	tk := load(t, fullTicket)
	if err := tk.WriteSection2("- 新しい要求（Q-01）"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(tk.Path)
	if !strings.Contains(string(data), "## §2 要求\n\n- 新しい要求（Q-01）\n\n## §3") {
		t.Errorf("§2:\n%s", data)
	}
	if err := tk.WriteSection2("### 見出し\n- x"); err == nil {
		t.Error("見出しのある本文を受け付けた")
	}
}
