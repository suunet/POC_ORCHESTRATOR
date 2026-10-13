package workflow_test

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"gateflow/builtins"
	"gateflow/internal/workflow"
)

// 組み込みの遷移表は、読み込み時の検証に合格する。
func TestLoadBuiltin_AiddImpl(t *testing.T) {
	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if wf.Name != "aidd-impl" {
		t.Errorf("Name = %q", wf.Name)
	}
	h, ok := wf.HumanCommand("ready", "draft")
	if !ok || h.To != "ready-to-prepare" || len(h.Guards) == 0 {
		t.Errorf("ready = %+v, %v", h, ok)
	}
	a, ok := wf.AutoFrom("ready-to-implement")
	if !ok || a.To != "implementing" || a.Start != "p5" {
		t.Errorf("auto ready-to-implement = %+v, %v", a, ok)
	}
	if _, ok := wf.AutoFrom("draft"); ok {
		t.Error("draft は人の関門なのに自動で進む")
	}
	p5, ok := wf.Steps["p5"]
	if !ok || p5.Agent != "dev" {
		t.Fatalf("p5 = %+v", p5)
	}
	if r := p5.On["blocked"]; r.Stop != "human" {
		t.Errorf("p5 の blocked = %+v, want 人の確認待ち", r)
	}
}

// valid は、検証に合格する最小の遷移表。テストではこれを壊して使う。
const valid = `{
  "name": "mini",
  "states": ["draft", "ready-to-prepare", "ready-to-approve", "cancelled"],
  "terminal": ["cancelled"],
  "human": [
    {"command": "ready", "from": ["draft"], "to": "ready-to-prepare", "guards": ["questions-resolved"]},
    {"command": "cancel", "from": ["draft", "ready-to-prepare", "ready-to-approve"], "to": "cancelled"}
  ],
  "auto": [
    {"from": "ready-to-prepare", "start": "p2-review"}
  ],
  "steps": {
    "p2-review": {"agent": "bj", "on": {
      "approved": {"to": "ready-to-approve"},
      "needs_fix": {"actions": ["findings-to-questions"], "to": "draft"}
    }}
  }
}`

func mutate(t *testing.T, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(valid), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, _ := json.Marshal(m)
	return b
}

func TestParse_Valid(t *testing.T) {
	if _, err := workflow.Parse([]byte(valid)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestParse_Invalid(t *testing.T) {
	steps := func(m map[string]any) map[string]any { return m["steps"].(map[string]any) }
	p2On := func(m map[string]any) map[string]any {
		return steps(m)["p2-review"].(map[string]any)["on"].(map[string]any)
	}
	cases := []struct {
		name string
		data []byte
		want []string
	}{
		// Q-01：step のあり得る outcome すべてに遷移が要る（漏れは読み込み時に落とす）
		{"outcome の漏れ", mutate(t, func(m map[string]any) { delete(p2On(m), "needs_fix") }), []string{"p2-review", "needs_fix"}},
		{"知らない outcome", mutate(t, func(m map[string]any) { p2On(m)["maybe"] = map[string]any{"to": "draft"} }), []string{"maybe"}},
		{"ない状態への遷移", mutate(t, func(m map[string]any) { p2On(m)["approved"] = map[string]any{"to": "nowhere"} }), []string{"nowhere"}},
		{"ない step へ", mutate(t, func(m map[string]any) { p2On(m)["approved"] = map[string]any{"next": "p9"} }), []string{"p9"}},
		{"知らない操作", mutate(t, func(m map[string]any) {
			p2On(m)["needs_fix"] = map[string]any{"actions": []any{"rm-rf"}, "to": "draft"}
		}), []string{"rm-rf"}},
		{"知らない機械検証", mutate(t, func(m map[string]any) {
			m["human"].([]any)[0].(map[string]any)["guards"] = []any{"trust-me"}
		}), []string{"trust-me"}},
		{"行き先が複数", mutate(t, func(m map[string]any) {
			p2On(m)["approved"] = map[string]any{"to": "ready-to-approve", "next": "p2-review"}
		}), []string{"approved"}},
		{"行き先がない", mutate(t, func(m map[string]any) { p2On(m)["approved"] = map[string]any{} }), []string{"approved"}},
		{"到達できない状態", mutate(t, func(m map[string]any) {
			m["states"] = append(m["states"].([]any), "orphan")
		}), []string{"orphan"}},
		{"担当が不明", mutate(t, func(m map[string]any) { steps(m)["p2-review"].(map[string]any)["agent"] = "pm" }), []string{"pm"}},
		{"自動の開始がない step", mutate(t, func(m map[string]any) {
			m["auto"].([]any)[0].(map[string]any)["start"] = "p0"
		}), []string{"p0"}},
		{"関門のコマンドが重複", mutate(t, func(m map[string]any) {
			m["human"] = append(m["human"].([]any), map[string]any{"command": "ready", "from": []any{"draft"}, "to": "ready-to-prepare"})
		}), []string{"ready"}},
		{"知らない項目", []byte(strings.Replace(valid, `"name": "mini",`, `"name": "mini", "extra": 1,`, 1)), []string{"extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := workflow.Parse(tc.data)
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

// ループの戻り先は step で、数える名前（counter）が要る。
func TestParse_Loop(t *testing.T) {
	data := mutate(t, func(m map[string]any) {
		m["steps"].(map[string]any)["p2-review"].(map[string]any)["on"].(map[string]any)["needs_fix"] = map[string]any{"loop": "p2-review"}
	})
	if _, err := workflow.Parse(data); err == nil || !strings.Contains(err.Error(), "counter") {
		t.Fatalf("err = %v, want counter の誤り", err)
	}
}

// 組み込みの遷移表は、監視の UI の段と表示名を持ち、どの状態・step も段に入る（M2.8）。
func TestBuiltin_Display(t *testing.T) {
	wf, err := workflow.LoadBuiltin(builtins.FS(), "aidd-impl")
	if err != nil {
		t.Fatal(err)
	}
	d := wf.Display
	if d == nil || len(d.Stages) != 9 || d.Stages[1].Label != "要件審査" || d.Stages[1].Name != "要件の審査" {
		t.Fatalf("display = %+v", d)
	}
	if d.StageOfStep("p5-verify") != 6 || d.StageOfState("ready-for-review") != 7 || d.StageOfState("cancelled") != -1 {
		t.Errorf("段の番号が違う")
	}
	if wf.StepRole("verify") != workflow.RoleGateflow || wf.StepRole("p2-review") != "bj" {
		t.Errorf("担当が違う")
	}
}

// 表示名の抜け、段に入らない状態・step、重なる段は、読み込みで拒む（画面で見つけるのでなく）。
func TestParse_DisplayErrors(t *testing.T) {
	data, err := fs.ReadFile(builtins.FS(), "workflows/aidd-impl.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ from, to, want string }{
		"結果の表示名がない":    {`"needs_fix": "差し戻し", `, ``, `結果 "needs_fix"`},
		"止まる理由の表示名がない": {`"write-failed": "記録を書けない", `, ``, `止まる理由 "write-failed"`},
		"step が段に入らない": {`, "steps": ["p5"]}`, `}`, `step "p5" がどの段にも入っていない`},
		"状態が2つの段に入る":   {`"states": ["done"]}`, `"states": ["done", "draft"]}`, `状態 "draft" は段`},
		"コマンドの表示名がない":  {`"cancel": "中止"`, `"x": "x"`, `コマンド "cancel"`},
	}
	for name, tc := range cases {
		s := string(data)
		if !strings.Contains(s, tc.from) {
			t.Fatalf("%s：置き換える文がない %q", name, tc.from)
		}
		_, err := workflow.Parse([]byte(strings.Replace(s, tc.from, tc.to, 1)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
