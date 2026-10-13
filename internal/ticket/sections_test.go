package ticket_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/ticket"
)

const fullTicket = `# T-002 ログの基盤を作る

ステータス: ready-to-prepare

| 項目 | 値 |
| --- | --- |
| 種別 | 実装 |
| ブランチ | feature/t-002-log |
| Issue | 42 |

## §1 目的

- ログをそろえる

## §2 要求

- NFR-09 を満たす

## §3 変更前の現状（P3でAIが書く）

- 影響の範囲（IDで検索した結果の要約）：

## §4 実行計画（P3でAIが書く）

### §4.1 方針

-

### §4.2 受入条件

| 受入条件 | ユースケース | 内容 |
| --- | --- | --- |
| UC-NNN-AC-01 | UC-NNN（） | |

### §4.3 テストケース

| テスト | 層（単体／シナリオ） | 受入条件・観点 | 内容 |
| --- | --- | --- | --- |
| | | UC-NNN-AC-01 | |

### §4.4 更新する文書

| ファイル | 更新の内容 |
| --- | --- |
| | |

### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）

- 

### §4.6 承認（人が書く。AIは書かない）

- [ ] §2の要求に合意した
- [ ] §3の現状の認識に合意した
- [ ] §4の計画（受入条件・テストケース・更新する文書）に合意した
`

func load(t *testing.T, body string) *ticket.Ticket {
	t.Helper()
	root := writeTicket(t, "T-002", body)
	tk, err := ticket.Load(root, "T-002")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestWriteSections(t *testing.T) {
	tk := load(t, fullTicket)
	if missing := tk.MissingSections(); len(missing) == 0 {
		t.Error("ひな形のままなのに埋まっていることになった")
	}
	err := tk.WriteSections(ticket.Sections{
		Current: "ログの基盤はまだない。",
		Plan:    "common/log に作る。",
		Acceptance: []ticket.AcceptanceRow{
			{ID: "UC-001-AC-02", UC: "UC-001（システムの死活を確認する）", Content: "要求のログに必須の項目がある"},
		},
		TestCases: []ticket.TestCaseRow{{Test: "TestLog_Fields", Layer: "シナリオ", Targets: "UC-001-AC-02", Content: "項目を確かめる"}},
		Sheets:    []ticket.SheetRow{{File: ".docs/design/uc/UC-001.md", Change: "受入条件を足す"}},
	})
	if err != nil {
		t.Fatalf("WriteSections: %v", err)
	}
	data, _ := os.ReadFile(tk.Path)
	body := string(data)
	for _, w := range []string{"ログの基盤はまだない。", "common/log に作る。", "| UC-001-AC-02 | UC-001（システムの死活を確認する） | 要求のログに必須の項目がある |",
		"| TestLog_Fields | シナリオ | UC-001-AC-02 | 項目を確かめる |", "| `.docs/design/uc/UC-001.md` | 受入条件を足す |"} {
		if !strings.Contains(body, w) {
			t.Errorf("書き込んだ指示書に %q がない:\n%s", w, body)
		}
	}
	if strings.Contains(body, "UC-NNN-AC-01") {
		t.Error("ひな形の行が残っている")
	}
	// §4.6 とステータスの行、§1・§2 は変えない。
	for _, w := range []string{"- [ ] §2の要求に合意した", "ステータス: ready-to-prepare", "- NFR-09 を満たす", "| Issue | 42 |"} {
		if !strings.Contains(body, w) {
			t.Errorf("%q が消えた", w)
		}
	}
	reloaded, _ := ticket.Load(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(tk.Path))))), "T-002")
	if missing := reloaded.MissingSections(); len(missing) != 0 {
		t.Errorf("書き込んだ後も埋まっていない節: %v", missing)
	}
}

func TestApprovalChecked(t *testing.T) {
	tk := load(t, fullTicket)
	if tk.ApprovalChecked() {
		t.Error("未チェックなのに承認済み")
	}
	tk = load(t, strings.ReplaceAll(fullTicket, "- [ ]", "- [x]"))
	if !tk.ApprovalChecked() {
		t.Error("すべてチェックしたのに承認済みでない")
	}
	tk = load(t, strings.Replace(fullTicket, "- [ ] §2", "- [x] §2", 1))
	if tk.ApprovalChecked() {
		t.Error("一部だけのチェックで承認済み")
	}
}

func TestHold(t *testing.T) {
	tk := load(t, fullTicket)
	if tk.Held() {
		t.Error("保留の印がないのに保留")
	}
	if err := tk.SetHold(true); err != nil {
		t.Fatal(err)
	}
	if !tk.Held() || !strings.Contains(tk.Body, "| 保留 | はい |") {
		t.Errorf("保留の印が付かない:\n%s", tk.Body)
	}
	if err := tk.SetHold(false); err != nil {
		t.Fatal(err)
	}
	if tk.Held() || strings.Contains(tk.Body, "保留") {
		t.Error("保留の印が外れない")
	}
}

func TestTargetIDs(t *testing.T) {
	body := strings.Replace(fullTicket, "| Issue | 42 |\n", "| Issue | 42 |\n| 対象ID | NFR-06、UC-001（システムの死活を確認する）, VAR-20 |\n", 1)
	got := load(t, body).TargetIDs()
	if strings.Join(got, " ") != "NFR-06 UC-001 VAR-20" {
		t.Errorf("TargetIDs = %q", got)
	}
	if got := load(t, fullTicket).TargetIDs(); got != nil {
		t.Errorf("TargetIDs = %q, want なし", got)
	}
}

func TestIssueID(t *testing.T) {
	if got := load(t, fullTicket).IssueID(); got != "42" {
		t.Errorf("IssueID = %q", got)
	}
	if got := load(t, strings.Replace(fullTicket, "| Issue | 42 |\n", "", 1)).IssueID(); got != "" {
		t.Errorf("IssueID = %q, want 空", got)
	}
}

// 種別が基盤（M2.7）：§4.5 のホストでの確かめを書き、中身が要る節は §3・§4.1・§4.5。
func TestWriteSections_HostCheck(t *testing.T) {
	tk := load(t, strings.Replace(fullTicket, "| 種別 | 実装 |", "| 種別 | 基盤 |", 1))
	if tk.Kind != ticket.KindInfrastructure || tk.HasHostCheck() {
		t.Fatalf("種別 %q、書く前のホストでの確かめ %v", tk.Kind, tk.HasHostCheck())
	}
	if missing := tk.MissingSections(); len(missing) != 3 {
		t.Errorf("書く前の埋まっていない節 = %v", missing)
	}
	err := tk.WriteSections(ticket.Sections{Current: "c", Plan: "p",
		HostCheck: &ticket.HostCheck{Review: []string{"compose を読む"}, Steps: []ticket.HostCheckStep{{Action: "docker compose up", Pass: "200"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !tk.HasHostCheck() || len(tk.MissingSections()) != 0 || tk.TestCaseCount() != 0 {
		t.Errorf("ホストでの確かめ %v、埋まっていない節 %v、テストケース %d\n%s", tk.HasHostCheck(), tk.MissingSections(), tk.TestCaseCount(), tk.Body)
	}
	if !strings.Contains(tk.Body, "| 1 | docker compose up | 200 |") || !strings.Contains(tk.Body, "- [ ] §2の要求に合意した") {
		t.Errorf("書いた指示書:\n%s", tk.Body)
	}
	// ホストでの確かめがなければ「なし」と書き、基盤では埋まっていないとみなす
	if err := tk.WriteSections(ticket.Sections{Current: "c", Plan: "p"}); err != nil {
		t.Fatal(err)
	}
	if tk.HasHostCheck() || len(tk.MissingSections()) != 1 {
		t.Errorf("「なし」: ホストでの確かめ %v、埋まっていない節 %v", tk.HasHostCheck(), tk.MissingSections())
	}
}

// 古い形の指示書（§4.5 が承認）を、ホストでの確かめと読み違えない。§4.3 のデータの行を数える。
func TestHostCheck_OldLayoutAndTestCount(t *testing.T) {
	old := strings.Replace(fullTicket, "### §4.5 ホストでの確かめ（P3でAIが書く。種別が基盤なら必須）\n\n- \n\n### §4.6 承認", "### §4.5 承認", 1)
	if tk := load(t, old); tk.HasHostCheck() || tk.HostCheckRecorded() {
		t.Error("古い形の承認の欄を、ホストでの確かめと読んだ")
	}
	tk := load(t, fullTicket)
	if err := tk.WriteSections(ticket.Sections{Current: "c", Plan: "p",
		TestCases: []ticket.TestCaseRow{{Test: "A", Layer: "単体", Content: "x"}, {Test: "B", Layer: "単体", Content: "y"}}}); err != nil {
		t.Fatal(err)
	}
	if n := tk.TestCaseCount(); n != 2 {
		t.Errorf("テストケース = %d, want 2", n)
	}
}

// 表のセルの | は \| にエスケープして書き、読み戻すと元の | に戻る（コマンドのパイプを壊さない。M2.7）。
func TestHostCheck_PipesRoundTrip(t *testing.T) {
	steps := []ticket.HostCheckStep{
		{Action: `ss -ltn | grep -E ':(8080|18080)\b'`, Pass: "出力に出ない"},
		{Action: `docker export c | tar -tf - | grep -E '(^|/)\.env$|(^|/)\.git/'`, Pass: "grep が0件"},
		{Action: `printf '%s' "$l" | jq -e . >/dev/null || echo "NOT JSON: $l"`, Pass: "`NOT JSON:`が出ない"},
		{Action: `grep 'a\|b' f`, Pass: "BRE の \\| も保つ"},
	}
	tk := load(t, strings.Replace(fullTicket, "| 種別 | 実装 |", "| 種別 | 基盤 |", 1))
	if err := tk.WriteSections(ticket.Sections{Current: "c", Plan: "p", HostCheck: &ticket.HostCheck{Review: []string{"r"}, Steps: steps}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tk.Body, `| 1 | ss -ltn \| grep -E ':(8080\|18080)\b' | 出力に出ない |`) || strings.Contains(tk.Body, "ss -ltn ／") {
		t.Errorf("| をエスケープしていない:\n%s", tk.Body)
	}
	got := tk.HostCheckSteps()
	if len(got) != len(steps) {
		t.Fatalf("読み戻した手順 = %d 件:\n%s", len(got), tk.Body)
	}
	for i := range steps {
		if got[i] != steps[i] {
			t.Errorf("手順 %d: 読み戻し %q, want %q", i+1, got[i], steps[i])
		}
	}
}
