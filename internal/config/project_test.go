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
  "formatVersion": 2,
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
		{"levels の知らない検査", projectWith(t, "levels", `{"diff-size": "recommended"}`), []string{"levels", "diff-size"}},
		{"levels の知らないレベル", projectWith(t, "levels", `{"lint": "optional"}`), []string{"levels", "optional"}},
		{"touchMap の docs がない", projectWith(t, "touchMap", `[{"code": "backend/**"}]`), []string{"touchMap", "docs"}},
		{"touchMap のグロブの誤り", projectWith(t, "touchMap", `[{"code": "backend/[", "docs": ["x/"]}]`), []string{"touchMap", "backend/["}},
		{"書式のバージョン1は拒否する", projectWith(t, "formatVersion", "1"), []string{"formatVersion", "1"}},
		{"必須の項目がない", `{"formatVersion": 2}`, []string{"name", "branch", "agent", "verify", "ids", "policies"}},
		{"知らない項目", projectWith(t, "issue", `"x"`), []string{"issue"}},
		{"名前の形が違う", projectWith(t, "name", `"Article_App"`), []string{"Article_App"}},
		{"main のブランチがない", projectWith(t, "branch", `{}`), []string{"branch.main"}},
		{"エージェントの項目の誤り", projectWith(t, "agent", `{"dockerfile": "/etc/x", "envTemplate": "../env", "allow": ["example.com", "a b:443"]}`),
			[]string{"agent.dockerfile", "/etc/x", "agent.envTemplate", "../env", "example.com", "a b:443"}},
		{"依存の取得の項目の誤り", projectWith(t, "deps", `{"command": "", "cacheEnv": "go-mod"}`), []string{"deps.command", "go-mod"}},
		{"上限の誤り", projectWith(t, "limits", `{"steps": -5, "loops": -1}`), []string{"limits.steps", "limits.loops"}},
		{"Issue の誤り", projectWith(t, "issue", `{"tracker": "jira", "commitRef": "#{id}"}`), []string{"jira"}},
		{"Issue の書式に {id} がない", projectWith(t, "issue", `{"tracker": "github", "commitRef": "#123"}`), []string{"commitRef"}},
		{"遷移表の名前の誤り", projectWith(t, "workflow", `"../x"`), []string{"workflow"}},
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

// 上限・遷移表・Issue は任意。ない項目は設計の初期値を使う。
func TestLoadProject_Defaults(t *testing.T) {
	p, err := config.LoadProject(writeProject(t, validProject))
	if err != nil {
		t.Fatal(err)
	}
	if p.WorkflowName() != "aidd-impl" {
		t.Errorf("WorkflowName = %q", p.WorkflowName())
	}
	l := p.EffectiveLimits()
	if l.Steps != 20 || l.Loops != 3 || l.IdleMinutes != 10 || l.RunMinutes != 60 {
		t.Errorf("EffectiveLimits = %+v", l)
	}
	if p.Issue != nil {
		t.Errorf("Issue = %+v", p.Issue)
	}
}

func TestLoadProject_LimitsAndIssue(t *testing.T) {
	body := projectWith(t, "limits", `{"steps": 30, "idleMinutes": 5}`)
	root := writeProject(t, body)
	data, _ := os.ReadFile(filepath.Join(root, config.ProjectFile))
	var m map[string]json.RawMessage
	json.Unmarshal(data, &m)
	m["issue"] = json.RawMessage(`{"tracker": "github", "commitRef": "#{id}", "prRef": "Refs #{id}"}`)
	m["workflow"] = json.RawMessage(`"aidd-impl"`)
	out, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(root, config.ProjectFile), out, 0o644)

	p, err := config.LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	l := p.EffectiveLimits()
	if l.Steps != 30 || l.Loops != 3 || l.IdleMinutes != 5 || l.RunMinutes != 60 {
		t.Errorf("EffectiveLimits = %+v（ない項目は初期値）", l)
	}
	if got := p.Issue.Format(p.Issue.CommitRef, "123"); got != "#123" {
		t.Errorf("Format = %q", got)
	}
}

// 契約の ID の一覧は、完全一致と、末尾の * による前方一致。ARB の往復の上限の初期値は 2。
func TestProject_ContractsAndArbitrateLimit(t *testing.T) {
	p := &config.Project{Contracts: []string{"NFR-06", "SCR-*"}}
	for id, want := range map[string]bool{"NFR-06": true, "NFR-061": false, "SCR-18": true, "VAR-18": false} {
		if got := p.IsContract(id); got != want {
			t.Errorf("IsContract(%s) = %v", id, got)
		}
	}
	if l := p.EffectiveLimits(); l.ArbitrateLoops != 2 {
		t.Errorf("ArbitrateLoops = %d", l.ArbitrateLoops)
	}
}
