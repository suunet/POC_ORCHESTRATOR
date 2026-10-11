package config_test

import (
	"encoding/json"
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
  "formatVersion": 1,
  "name": "article-app",
  "branch": {"main": "main"},
  "agent": {"dockerfile": ".gateflow/agent.Dockerfile", "envTemplate": ".gateflow/env.template", "allow": ["example.internal:443"]},
  "deps": {"command": "go mod download", "cacheEnv": "GOMODCACHE"},
  "verify": {"build": "go build ./...", "test": "go test ./...", "lint": "go vet ./..."},
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

// projectWith は、validProject の最上位の項目 key を raw（JSON）に替えたものを返す。raw が空なら項目を消す。
func projectWith(t *testing.T, key, raw string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(validProject), &m); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		delete(m, key)
	} else {
		m[key] = json.RawMessage(raw)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestLoadProject_Valid(t *testing.T) {
	root := writeProject(t, validProject)

	p, err := config.LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if p.Name != "article-app" || p.Branch.Main != "main" {
		t.Errorf("name, branch = %q, %+v", p.Name, p.Branch)
	}
	if p.Agent.Dockerfile != ".gateflow/agent.Dockerfile" || p.Agent.EnvTemplate != ".gateflow/env.template" ||
		len(p.Agent.Allow) != 1 || p.Agent.Allow[0] != "example.internal:443" {
		t.Errorf("agent = %+v", p.Agent)
	}
	if p.Deps == nil || p.Deps.Command != "go mod download" || p.Deps.CacheEnv != "GOMODCACHE" {
		t.Errorf("deps = %+v", p.Deps)
	}
	if p.Verify.Build != "go build ./..." || p.Verify.Test != "go test ./..." || p.Verify.Lint != "go vet ./..." {
		t.Errorf("verify = %+v", p.Verify)
	}
	if len(p.IDs.Prefixes) != 3 || p.IDs.Prefixes[1].FindColumn() != "UC" || p.IDs.Prefixes[2].Children == nil {
		t.Errorf("ids = %+v", p.IDs)
	}
	if p.Policies[0].Path != ".docs/operations/policies/coding.md" {
		t.Errorf("policy path = %q", p.Policies[0].Path)
	}
}

// deps は任意。なければ依存の取得をしない。
func TestLoadProject_NoDeps(t *testing.T) {
	root := writeProject(t, projectWith(t, "deps", ""))
	p, err := config.LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if p.Deps != nil {
		t.Errorf("deps = %+v, want nil", p.Deps)
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
		{"書式のバージョンがない", projectWith(t, "formatVersion", ""), []string{"formatVersion"}},
		{"書式のバージョン0は拒否する", projectWith(t, "formatVersion", "0"), []string{"formatVersion", "0"}},
		{"必須の項目がない", `{"formatVersion": 1}`, []string{"name", "branch", "agent", "verify", "ids", "policies"}},
		{"知らない項目", projectWith(t, "issue", `"x"`), []string{"issue"}},
		{"名前の形が違う", projectWith(t, "name", `"Article_App"`), []string{"Article_App"}},
		{"main のブランチがない", projectWith(t, "branch", `{}`), []string{"branch.main"}},
		{"エージェントの項目の誤り", projectWith(t, "agent", `{"dockerfile": "/etc/x", "envTemplate": "../env", "allow": ["example.com", "a b:443"]}`),
			[]string{"agent.dockerfile", "/etc/x", "agent.envTemplate", "../env", "example.com", "a b:443"}},
		{"依存の取得の項目の誤り", projectWith(t, "deps", `{"command": "", "cacheEnv": "go-mod"}`), []string{"deps.command", "go-mod"}},
		{"検証コマンドがない", projectWith(t, "verify", `{"build": "go build ./..."}`), []string{"verify.test", "verify.lint"}},
		{
			name: "プレフィックスの誤りをまとめて報告する",
			body: projectWith(t, "ids", `{"prefixes": [
				{"prefix": "ACT", "pattern": "ACT-([", "files": ["a.md"], "find": "table-first-column"},
				{"prefix": "UC", "pattern": "XX-[0-9]{3}", "files": [], "find": "somewhere"},
				{"prefix": "ACT", "pattern": "ACT-[0-9]{2}", "files": ["a.md"], "find": "table-column:"}
			]}`),
			want: []string{"ACT-([", "XX-[0-9]{3}", "files", "somewhere", "重複", "table-column:"},
		},
		{
			name: "子の設定の誤り",
			body: projectWith(t, "ids", `{"prefixes": [
				{"prefix": "STM", "pattern": "STM-[0-9]{2}", "files": ["s.md"], "find": "table-first-column", "children": {"files": ["s.md"]}}
			]}`),
			want: []string{"separator", "parentColumn", "nameColumn"},
		},
		{"規約の誤り", projectWith(t, "policies", `[{"path": "", "roles": ["pm"]}]`), []string{"path", "pm"}},
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
