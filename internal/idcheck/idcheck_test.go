package idcheck_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/idcheck"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testConfig() config.IDs {
	return config.IDs{
		Scan:             []string{"docs"},
		DeprecatedMarker: "廃止",
		Prefixes: []config.Prefix{
			{Prefix: "ACT", Pattern: "ACT-[0-9]{2}", Files: []string{"docs/actor.md"}, Find: "table-first-column", NameColumn: "アクター"},
			{Prefix: "BUC", Pattern: "BUC-[PQ][0-9]{2}", Files: []string{"docs/buc.md"}, Find: "table-first-column", NameColumn: "BUC"},
			{Prefix: "UC", Pattern: "UC-[0-9]{3}", Files: []string{"docs/buc.md"}, Find: "table-column:UC"},
			{Prefix: "STM", Pattern: "STM-[0-9]{2}", Files: []string{"docs/state.md"}, Find: "table-first-column", NameColumn: "状態モデル",
				Children: &config.Children{Separator: ".", Files: []string{"docs/state.md"}, ParentColumn: "状態モデル", NameColumn: "状態"}},
		},
	}
}

// 題材は架空の記事管理。プレフィックスの区分（P・Q）や名前に意味はない。

const actor = `# アクター

| ID | アクター群 | アクター | 説明 |
| --- | --- | --- | --- |
| ACT-01 | 編集部 | 執筆者 | 記事を書く人 |
| ACT-02 | 編集部 | 編集者 | 記事を公開する人 |
`

const buc = "# 業務\n\n" +
	"| 業務 | BUC | アクティビティ | UC | 関連 |\n" +
	"|---|---|---|---|---|\n" +
	"| BUC-P01 | 記事の執筆 | 書く | UC-002（記事を書く） | ACT-01（執筆者） |\n" +
	"|  |  | 校閲する | UC-003（記事を校閲する） | STM-01.校閲済みへ |\n" +
	"| BUC-Q01 | 記事の公開 | 公開する | UC-011（記事を公開する） | ACT-02（編集者） |\n"

const state = "# 状態\n\n" +
	"| ID | 状態モデル | 対象 |\n" +
	"|---|---|---|\n" +
	"| STM-01 | 記事状態 | — |\n\n" +
	"| コンテキスト | 状態モデル | 状態 | 遷移UC | 遷移先状態 |\n" +
	"|---|---|---|---|---|\n" +
	"| 記事 | STM-01 | （初期） | UC-002（記事を書く） | STM-01.下書き |\n" +
	"| 記事 | STM-01 | 下書き | UC-003（記事を校閲する） | STM-01.校閲済み |\n" +
	"| 記事 | STM-01 | 校閲済み | UC-011（記事を公開する） | — |\n\n" +
	"```mermaid\nstateDiagram-v2\n  [*] --> A : UC-002\n```\n"

func baseFiles() map[string]string {
	return map[string]string{
		"docs/actor.md": actor,
		"docs/buc.md":   buc,
		"docs/state.md": state,
	}
}

func run(t *testing.T, files map[string]string) idcheck.Result {
	t.Helper()
	root := writeFiles(t, files)
	res, err := idcheck.Run(root, mustRules(t, testConfig()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func mustRules(t *testing.T, cfg config.IDs) *idcheck.Rules {
	t.Helper()
	rules, err := idcheck.NewRules(cfg)
	if err != nil {
		t.Fatalf("NewRules: %v", err)
	}
	return rules
}

// kinds は、指摘の種類とIDを「種類 ID」の形で並べて返す。
func kinds(res idcheck.Result, level idcheck.Level) []string {
	var out []string
	for _, f := range res.Findings {
		if f.Level == level {
			out = append(out, f.Kind+" "+f.ID)
		}
	}
	sort.Strings(out)
	return out
}

func TestRun_Clean(t *testing.T) {
	res := run(t, baseFiles())
	if got := kinds(res, idcheck.Required); len(got) != 0 {
		t.Errorf("必須の指摘 = %v, want なし", got)
	}
	if res.HasErrors() {
		t.Error("HasErrors = true")
	}
	// ACT 2・BUC 2・UC 3・STM 1。子は STM-01 の下書き・校閲済み（初期は数えない）。
	if res.Definitions != 8 || res.Children != 2 {
		t.Errorf("Definitions, Children = %d, %d, want 8, 2", res.Definitions, res.Children)
	}
}

func TestRun_Undefined(t *testing.T) {
	files := baseFiles()
	files["docs/note.md"] = "参照：ACT-09（誰か）、UC-099、STM-01.存在しない状態、STM-02\n"
	res := run(t, files)
	want := []string{"undefined ACT-09", "undefined STM-01.存在しない状態", "undefined STM-02", "undefined UC-099"}
	if got := kinds(res, idcheck.Required); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("必須の指摘 = %v, want %v", got, want)
	}
	if !res.HasErrors() {
		t.Error("HasErrors = false")
	}
	for _, f := range res.Findings {
		if f.ID == "UC-099" && (f.File != "docs/note.md" || f.Line != 1) {
			t.Errorf("場所 = %s:%d, want docs/note.md:1", f.File, f.Line)
		}
	}
}

func TestRun_Duplicate(t *testing.T) {
	files := baseFiles()
	files["docs/actor.md"] = actor + "| ACT-02 | 編集部 | 別の編集者 | 重複 |\n"
	res := run(t, files)
	if got := kinds(res, idcheck.Required); strings.Join(got, ",") != "duplicate ACT-02" {
		t.Errorf("必須の指摘 = %v, want [duplicate ACT-02]", got)
	}
}

func TestRun_Deprecated(t *testing.T) {
	files := baseFiles()
	files["docs/actor.md"] = actor + "| ACT-03 | 廃止 | 旧執筆者 | 廃止（統合） |\n"
	files["docs/note.md"] = "ACT-03（旧執筆者）を参照\n"
	res := run(t, files)
	if got := kinds(res, idcheck.Required); strings.Join(got, ",") != "deprecated ACT-03" {
		t.Errorf("必須の指摘 = %v, want [deprecated ACT-03]", got)
	}
}

func TestRun_Recommended(t *testing.T) {
	files := baseFiles()
	files["docs/actor.md"] = actor + "| ACT-03 | 外部 | 読者 | 参照されない |\n"
	files["docs/note.md"] = "ACT-01（書き手）、UC-002（記事を作る）\n"
	res := run(t, files)
	want := []string{"name-mismatch ACT-01", "name-mismatch UC-002", "unreferenced ACT-03"}
	got := kinds(res, idcheck.Recommended)
	// BUC は参照されないので unreferenced に出る。比較の対象からは除く。
	var filtered []string
	for _, g := range got {
		if !strings.HasPrefix(g, "unreferenced BUC-") {
			filtered = append(filtered, g)
		}
	}
	if strings.Join(filtered, ",") != strings.Join(want, ",") {
		t.Errorf("推奨の指摘 = %v, want %v", filtered, want)
	}
	if res.HasErrors() {
		t.Error("推奨だけで HasErrors = true")
	}
}

func TestRun_Boundaries(t *testing.T) {
	files := baseFiles()
	// 長い数字、別のプレフィックスの一部、受入条件の形は、未定義の参照として誤検出しない。
	files["docs/note.md"] = "UC-0021 と XUC-999 と BUC-P01 と UC-002-AC-01、SHA-256\n"
	res := run(t, files)
	if got := kinds(res, idcheck.Required); len(got) != 0 {
		t.Errorf("必須の指摘 = %v, want なし", got)
	}
}

func TestRun_NestedParenName(t *testing.T) {
	files := baseFiles()
	files["docs/note.md"] = "ACT-01（執筆者）（補足（入れ子））\n"
	res := run(t, files)
	if got := kinds(res, idcheck.Recommended); strings.Contains(strings.Join(got, ","), "name-mismatch") {
		t.Errorf("推奨の指摘 = %v, want 名前の不一致なし", got)
	}
}

func TestRun_MissingFile(t *testing.T) {
	files := baseFiles()
	delete(files, "docs/state.md")
	root := writeFiles(t, files)
	if _, err := idcheck.Run(root, mustRules(t, testConfig())); err == nil {
		t.Fatal("定義のファイルがないのに error が返らない")
	}
}

func TestNewRules_InvalidPattern(t *testing.T) {
	cfg := testConfig()
	cfg.Prefixes[0].Pattern = "ACT-(["
	if _, err := idcheck.NewRules(cfg); err == nil || !strings.Contains(err.Error(), "ACT") {
		t.Fatalf("err = %v, want ACT の pattern の誤り", err)
	}
}

// 同じ Rules で検査を繰り返しても、前回の定義が持ち越されない。
func TestRun_RulesReusable(t *testing.T) {
	rules := mustRules(t, testConfig())
	full := writeFiles(t, baseFiles())
	if _, err := idcheck.Run(full, rules); err != nil {
		t.Fatalf("Run: %v", err)
	}
	files := baseFiles()
	files["docs/state.md"] = "| ID | 状態モデル |\n|---|---|\n| STM-01 | 記事状態 |\n"
	res, err := idcheck.Run(writeFiles(t, files), rules)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Children != 0 {
		t.Errorf("Children = %d, want 0（前回の子が残っている）", res.Children)
	}
}

// 検査の結果は、定義された ID（子を含む）の一覧を持つ。
func TestRun_DefinedIDs(t *testing.T) {
	res := run(t, baseFiles())
	for _, id := range []string{"ACT-01", "BUC-P01", "UC-002", "STM-01", "STM-01.下書き"} {
		if !res.Defined(id) {
			t.Errorf("%s が定義されていない", id)
		}
	}
	if res.Defined("UC-099") {
		t.Error("定義のない UC-099 が定義されている")
	}
}

// Matches は、設定のプレフィックスの形式に全体で一致する ID だけを受け付ける。
func TestRules_Matches(t *testing.T) {
	rules, err := idcheck.NewRules(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"UC-001": true, "ACT-01": true, "UC-01": false, "UC-0012": false, "XYZ-01": false, "SHA-256": false} {
		if got := rules.Matches(id); got != want {
			t.Errorf("Matches(%q) = %v, want %v", id, got, want)
		}
	}
}

// FindIDs は、文の中の ID を重ねずに返し、別の語の一部や形式に合わないものは拾わない。
func TestRules_FindIDs(t *testing.T) {
	rules, err := idcheck.NewRules(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := rules.FindIDs("UC-001 と ACT-01（UC-001 の再掲）。XUC-002、UC-0012、SHA-256 は拾わない")
	if strings.Join(got, ",") != "ACT-01,UC-001" && strings.Join(got, ",") != "UC-001,ACT-01" {
		t.Errorf("FindIDs = %v", got)
	}
}

// Where は、ID の定義のファイルと行を返す（M2.8）。子の ID は親の定義の場所。
func TestResult_Where(t *testing.T) {
	res := run(t, baseFiles())
	for id, want := range map[string]idcheck.Location{
		"ACT-02":     {File: "docs/actor.md", Line: 6},
		"UC-003":     {File: "docs/buc.md", Line: 6},
		"STM-01":     {File: "docs/state.md", Line: 5},
		"STM-01.下書き": {File: "docs/state.md", Line: 5},
	} {
		if got, ok := res.Where(id); !ok || got != want {
			t.Errorf("Where(%s) = %+v, %v, want %+v", id, got, ok, want)
		}
	}
	if _, ok := res.Where("ACT-99"); ok {
		t.Error("定義のない ID の場所を返した")
	}
}

// 受入条件（ユースケースの詳細の表。グロブ）と、テストの置き場（M3）。確認の方法がテストの受入条件に、
// テストの名前かコメントの ID がなければ必須。UAT は除く。テストの検査を外す選び方もある（P3 の直後）。
func TestRun_TestMissing(t *testing.T) {
	cfg := testConfig()
	cfg.Tests = []string{"tests"}
	cfg.Prefixes = append(cfg.Prefixes, config.Prefix{Prefix: "AC", Pattern: "UC-[0-9]{3}-AC-[0-9]{2}", Files: []string{"docs/uc/*.md"},
		Find: "table-first-column", MethodColumn: "確認の方法"})
	files := baseFiles()
	files["docs/uc/UC-002.md"] = "# UC-002\n\n| ID | 期待 | 確認の方法 |\n|---|---|---|\n| UC-002-AC-01 | a | シナリオテスト |\n| UC-002-AC-02 | b | 単体テスト |\n| UC-002-AC-03 | 画面 | UAT |\n"
	files["docs/uc/UC-003.md"] = "# UC-003\n\n| ID | 期待 | 確認の方法 |\n|---|---|---|\n| UC-003-AC-01 | c | シナリオテスト |\n"
	files["tests/scenario/a_test.go"] = "package scenario\n\n// UC-002-AC-01\nfunc TestA(t *testing.T) {}\n\nfunc TestUC003AC01(t *testing.T) {} // UC-003-AC-01\n"
	root := writeFiles(t, files)
	res, err := idcheck.Run(root, mustRules(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res, idcheck.Required); strings.Join(got, ",") != "test-missing UC-002-AC-02" {
		t.Errorf("必須の指摘 = %v（UC-002-AC-02 だけがテストなし。UAT の AC-03 は除く）", got)
	}
	if !res.Defined("UC-003-AC-01") {
		t.Error("グロブのファイルの受入条件を定義として読んでいない")
	}
	res, err = idcheck.RunWith(root, mustRules(t, cfg), idcheck.Options{SkipTestMissing: true})
	if err != nil || len(kinds(res, idcheck.Required)) != 0 {
		t.Errorf("テストの検査を外したのに指摘がある: %v, %v", kinds(res, idcheck.Required), err)
	}
}

// 確かめ方のない非機能要求は推奨（M3）。空と「—」は指摘、「（案）」は確かめ方があるとみなす。
func TestRun_Unverified(t *testing.T) {
	cfg := testConfig()
	cfg.Prefixes = append(cfg.Prefixes, config.Prefix{Prefix: "NFR", Pattern: "NFR-[0-9]{2}", Files: []string{"docs/nfr.md"},
		Find: "table-first-column", VerifyColumn: "確かめ方"})
	files := baseFiles()
	files["docs/nfr.md"] = "# 非機能要求\n\n| ID | 要求 | 確かめ方 |\n|---|---|---|\n| NFR-01 | a | テスト |\n| NFR-02 | b |  |\n| NFR-03 | c | — |\n| NFR-04 | d | （案）手動 |\n\nNFR-01 NFR-02 NFR-03 NFR-04\n"
	res := run2(t, files, cfg)
	var got []string
	for _, f := range res.Findings {
		if f.Kind == idcheck.KindUnverified {
			got = append(got, f.Level.String()+" "+f.ID)
		}
	}
	if strings.Join(got, ",") != "推奨 NFR-02,推奨 NFR-03" {
		t.Errorf("確かめ方のない非機能要求 = %v", got)
	}
}

func run2(t *testing.T, files map[string]string, cfg config.IDs) idcheck.Result {
	t.Helper()
	res, err := idcheck.Run(writeFiles(t, files), mustRules(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	return res
}
