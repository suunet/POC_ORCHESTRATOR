package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/config"
)

func writeProject(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".gateflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const validProject = `{
  "formatVersion": 0,
  "ids": {
    "scan": [".docs/design"],
    "deprecatedMarker": "廃止",
    "prefixes": [
      {"prefix": "ACT", "pattern": "ACT-[0-9]{2}", "files": [".docs/design/actor.md"], "find": "table-first-column", "nameColumn": "アクター"},
      {"prefix": "UC", "pattern": "UC-[0-9]{3}", "files": [".docs/design/buc.md"], "find": "table-column:UC"},
      {"prefix": "STM", "pattern": "STM-[0-9]{2}", "files": [".docs/design/state.md"], "find": "table-first-column", "nameColumn": "状態モデル",
       "children": {"separator": ".", "files": [".docs/design/state.md"], "parentColumn": "状態モデル", "nameColumn": "状態"}}
    ]
  },
  "policies": [
    {"path": ".docs/operations/policies/coding.md", "roles": ["dev", "bj"], "purpose": "コーディング規約"}
  ]
}`

func TestLoadProject_Valid(t *testing.T) {
	root := writeProject(t, validProject)

	p, err := config.LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if len(p.IDs.Prefixes) != 3 {
		t.Errorf("prefixes = %d, want 3", len(p.IDs.Prefixes))
	}
	if p.IDs.Prefixes[1].FindColumn() != "UC" {
		t.Errorf("FindColumn = %q, want UC", p.IDs.Prefixes[1].FindColumn())
	}
	if p.IDs.Prefixes[2].Children == nil {
		t.Error("children not loaded")
	}
	if p.Policies[0].Path != ".docs/operations/policies/coding.md" {
		t.Errorf("policy path = %q", p.Policies[0].Path)
	}
}

func TestLoadProject_Missing(t *testing.T) {
	_, err := config.LoadProject(t.TempDir())
	if err == nil {
		t.Fatal("want error for missing project.json")
	}
}

func TestLoadProject_Invalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "書式のバージョンがない",
			body: `{"ids": {"prefixes": []}, "policies": []}`,
			want: []string{"formatVersion"},
		},
		{
			name: "書式のバージョンが違う",
			body: `{"formatVersion": 1, "ids": {"prefixes": []}, "policies": []}`,
			want: []string{"formatVersion"},
		},
		{
			name: "必須の項目がない",
			body: `{"formatVersion": 0}`,
			want: []string{"ids", "policies"},
		},
		{
			name: "知らない項目",
			body: `{"formatVersion": 0, "ids": {"prefixes": []}, "policies": [], "branch": "main"}`,
			want: []string{"branch"},
		},
		{
			name: "プレフィックスの誤りをまとめて報告する",
			body: `{"formatVersion": 0, "ids": {"prefixes": [
				{"prefix": "ACT", "pattern": "ACT-([", "files": ["a.md"], "find": "table-first-column"},
				{"prefix": "UC", "pattern": "XX-[0-9]{3}", "files": [], "find": "somewhere"},
				{"prefix": "ACT", "pattern": "ACT-[0-9]{2}", "files": ["a.md"], "find": "table-column:"}
			]}, "policies": []}`,
			want: []string{"ACT-([", "XX-[0-9]{3}", "files", "somewhere", "重複", "table-column:"},
		},
		{
			name: "子の設定の誤り",
			body: `{"formatVersion": 0, "ids": {"prefixes": [
				{"prefix": "STM", "pattern": "STM-[0-9]{2}", "files": ["s.md"], "find": "table-first-column", "children": {"files": ["s.md"]}}
			]}, "policies": []}`,
			want: []string{"separator", "parentColumn", "nameColumn"},
		},
		{
			name: "規約の誤り",
			body: `{"formatVersion": 0, "ids": {"prefixes": []}, "policies": [{"path": "", "roles": ["pm"]}]}`,
			want: []string{"path", "pm"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeProject(t, tc.body)
			_, err := config.LoadProject(root)
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
