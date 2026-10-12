package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/engine"
	"gateflow/internal/idcheck"
	"gateflow/internal/output"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 根拠の実在（確定した質疑、定義のある ID、規約のファイルと行）と、作業ツリーの変更を確かめる。
func TestValidateARB(t *testing.T) {
	base := t.TempDir()
	seen, in, after := filepath.Join(base, "seen"), filepath.Join(base, "in"), filepath.Join(base, "after")
	cat := map[string]string{
		".docs/design/buc.md":                 "| BUC | UC |\n|---|---|\n| BUC-S01 | UC-001（死活） |\n",
		".docs/design/nfr.md":                 "| ID | 要求 |\n|---|---|\n| NFR-06 | エラーの応答 |\n",
		".docs/operations/policies/coding.md": "1\n2\n3\n",
	}
	writeTree(t, seen, cat)
	writeTree(t, after, cat)
	writeTree(t, in, map[string]string{
		"questions.md": "| # | 質問（要約） | 回答者 | 状態 | 反映先 | 決定者 | 根拠 |\n| --- | --- | --- | --- | --- | --- | --- |\n| Q-01 | a | 人 | 確定 | x | 人 | |\n| Q-02 | b | 人 | 未回答 | | 人 | |\n",
		"policies/.docs/operations/policies/coding-go.md": "1\n",
	})
	rules, err := idcheck.NewRules(config.IDs{Scan: []string{".docs/design"}, Prefixes: []config.Prefix{
		{Prefix: "UC", Pattern: "UC-[0-9]{3}", Files: []string{".docs/design/buc.md"}, Find: "table-column:UC"},
		{Prefix: "NFR", Pattern: "NFR-[0-9]{2}", Files: []string{".docs/design/nfr.md"}, Find: "table-first-column"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	review := &output.ReviewResult{Findings: []output.Finding{{Label: "MUST", Finding: "x", Evidence: &output.Evidence{ID: "UC-001"}}}}
	var hits map[int][]string
	check := func(basis output.Basis, edits []string, contracts []string) error {
		r := &output.ARBResult{Outcome: "resolved", Summary: "s", Decisions: []output.Decision{
			{Findings: []int{0}, Decider: "arb", Answer: "NFR-06 には触れない", Basis: &basis, Edits: edits}}}
		v, err := engine.ValidateARB(engine.ARBCheck{Result: r, Review: review, Seen: seen, In: in, After: after,
			Project: &config.Project{Contracts: contracts}, Rules: rules})
		hits = v.ContractHits
		return err
	}
	for name, tc := range map[string]struct {
		basis     output.Basis
		contracts []string
		want      string
	}{
		"確定した質疑":        {output.Basis{Kind: "question", Ref: "Q-01"}, nil, ""},
		"未確定の質疑":        {output.Basis{Kind: "question", Ref: "Q-02"}, nil, "確定していない"},
		"ない質疑":          {output.Basis{Kind: "question", Ref: "Q-09"}, nil, "質疑にない"},
		"定義のある ID":      {output.Basis{Kind: "catalog", Ref: "UC-001"}, nil, ""},
		"ない ID":         {output.Basis{Kind: "catalog", Ref: "NFR-99"}, nil, "カタログにない"},
		"Git 管理の規約と行":   {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/coding.md:3"}, nil, ""},
		"Git 管理外の規約の写し": {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/coding-go.md"}, nil, ""},
		"規約の行の範囲":       {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/coding.md:2-3"}, nil, ""},
		"規約の行が多すぎる":     {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/coding.md:9"}, nil, "行数"},
		"規約の行の範囲が多すぎる":  {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/coding.md:2-9"}, nil, "行数"},
		"ない規約":          {output.Basis{Kind: "policy", Ref: ".docs/operations/policies/none.md"}, nil, "ない"},
		"規格（記憶による）":     {output.Basis{Kind: "standard", Ref: "W3C Trace Context"}, nil, ""},
	} {
		err := check(tc.basis, nil, tc.contracts)
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	// 契約の守り：(b) 根拠に契約の ID（前方一致）。答えの文に出てくるだけ（NFR-06）は数えない
	if err := check(output.Basis{Kind: "catalog", Ref: "UC-001"}, nil, []string{"UC-*", "NFR-06"}); err != nil || len(hits[0]) != 1 || hits[0][0] != "UC-001" {
		t.Errorf("根拠の契約: hits = %v, err = %v", hits, err)
	}

	// 作業ツリーの変更：カタログの外、edits にない ID、契約の ID の行
	writeTree(t, after, map[string]string{".docs/design/buc.md": cat[".docs/design/buc.md"] + "| UC-002 | 新しい |\n"})
	if err := check(output.Basis{Kind: "catalog", Ref: "UC-001"}, []string{"UC-002"}, nil); err != nil || len(hits) != 0 {
		t.Errorf("edits に挙げた変更: hits = %v, err = %v", hits, err)
	}
	// 表の行は、その行が定義している ID（1列目）で数える。本文で参照している ID（UC-001）は数えない
	writeTree(t, after, map[string]string{".docs/design/buc.md": cat[".docs/design/buc.md"] + "| UC-002 | UC-001 を参照する |\n"})
	if err := check(output.Basis{Kind: "standard", Ref: "RFC"}, []string{"UC-002"}, []string{"UC-001"}); err != nil || len(hits) != 0 {
		t.Errorf("行の中の参照: hits = %v, err = %v", hits, err)
	}
	writeTree(t, after, map[string]string{".docs/design/buc.md": cat[".docs/design/buc.md"] + "| UC-002 | 新しい |\n"})
	// (a) 変えた行に契約の ID：edits でその裁定を特定する
	if err := check(output.Basis{Kind: "standard", Ref: "RFC"}, []string{"UC-002"}, []string{"UC-002"}); err != nil || len(hits[0]) != 1 || hits[0][0] != "UC-002" {
		t.Errorf("変えた行の契約: hits = %v, err = %v", hits, err)
	}
	if err := check(output.Basis{Kind: "catalog", Ref: "UC-001"}, nil, nil); err == nil || !strings.Contains(err.Error(), "UC-002 が、どの裁定の edits にもない") {
		t.Errorf("edits にない ID: %v", err)
	}
	writeTree(t, after, map[string]string{"backend/x.go": "package x\n"})
	if err := check(output.Basis{Kind: "catalog", Ref: "UC-001"}, []string{"UC-002"}, nil); err == nil || !strings.Contains(err.Error(), "backend/x.go") {
		t.Errorf("カタログの外: %v", err)
	}
}
