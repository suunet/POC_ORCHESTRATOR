package workflow

import (
	"fmt"
	"slices"
	"sort"
)

// Display は、監視の UI に出す段と表示名（M2.8）。日本語の表示名は、画面と Go のコードに直書きせず、ここに持たせる。
type Display struct {
	// Stages は、流れの段（左から順）。段は、状態（States）と step（Steps）で決める
	Stages []Stage `json:"stages"`
	// Roles は、担当の表示名（step の agent・検査の gateflow・人の human）
	Roles map[string]string `json:"roles"`
	// States は、状態の表示名
	States map[string]string `json:"states"`
	// Outcomes は、step の結果の表示名
	Outcomes map[string]string `json:"outcomes"`
	// Stops は、gateflow が止まる理由の表示名（step の結果で止まるものは Outcomes の表示名を使う）
	Stops map[string]string `json:"stops"`
	// Commands は、人のコマンドの表示名
	Commands map[string]string `json:"commands"`
}

// Stage は、流れの1つの段。Label は帯に出す短い名前、Name は文の中で使う名前。
type Stage struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Name   string   `json:"name"`
	States []string `json:"states,omitempty"`
	Steps  []string `json:"steps,omitempty"`
}

// 担当の名前。step の担当は遷移表の agent、検査の step は gateflow、人の関門の状態は人。
const (
	RoleGateflow = "gateflow"
	RoleHuman    = "human"
)

// EngineStops は、gateflow が自分で止まる理由（step の結果で止まるものを除く）。表示名が要る。
var EngineStops = []string{"error", "invalid-output", "limit-loops", "limit-steps", "no-progress", "timeout", "write-failed", "interrupted"}

// StageOfStep は、step の段の番号を返す（なければ -1）。
func (d *Display) StageOfStep(step string) int {
	return slices.IndexFunc(d.Stages, func(s Stage) bool { return slices.Contains(s.Steps, step) })
}

// StageOfState は、状態の段の番号を返す（なければ -1）。
func (d *Display) StageOfState(state string) int {
	return slices.IndexFunc(d.Stages, func(s Stage) bool { return slices.Contains(s.States, state) })
}

// StepRole は、step の担当（dev・bj・arb・gateflow）を返す。
func (w *Workflow) StepRole(step string) string {
	st, ok := w.Steps[step]
	switch {
	case !ok:
		return ""
	case st.Check != "":
		return RoleGateflow
	default:
		return st.Agent
	}
}

func (w *Workflow) validateDisplay() []error {
	d := w.Display
	if d == nil {
		return nil
	}
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	ids := map[string]bool{}
	stateStage := map[string]string{}
	stepStage := map[string]string{}
	for _, s := range d.Stages {
		where := "display の段 " + s.ID
		if s.ID == "" || s.Label == "" || s.Name == "" {
			add("%s：id・label・name のどれかがない", where)
		}
		if ids[s.ID] {
			add("%s：段の id が重複している", where)
		}
		ids[s.ID] = true
		for _, st := range s.States {
			if !slices.Contains(w.States, st) {
				add("%s：状態 %q は states にない", where, st)
			}
			if prev, ok := stateStage[st]; ok {
				add("%s：状態 %q は段 %s にもある", where, st, prev)
			}
			stateStage[st] = s.ID
		}
		for _, st := range s.Steps {
			if _, ok := w.Steps[st]; !ok {
				add("%s：step %q がない", where, st)
			}
			if prev, ok := stepStage[st]; ok {
				add("%s：step %q は段 %s にもある", where, st, prev)
			}
			stepStage[st] = s.ID
		}
	}
	// 終わりの状態（cancelled など、流れに出さないもの）を除き、すべての状態と step がちょうど1つの段に入る
	for _, s := range w.States {
		if _, ok := stateStage[s]; !ok && !slices.Contains(w.Terminal, s) {
			add("display：状態 %q がどの段にも入っていない", s)
		}
	}
	steps := make([]string, 0, len(w.Steps))
	for n := range w.Steps {
		steps = append(steps, n)
	}
	sort.Strings(steps)
	need := func(table map[string]string, kind, key string) {
		if table[key] == "" {
			add("display：%s %q の表示名がない", kind, key)
		}
	}
	for _, n := range steps {
		if _, ok := stepStage[n]; !ok {
			add("display：step %q がどの段にも入っていない", n)
		}
		need(d.Roles, "担当", w.StepRole(n))
		for o := range w.Steps[n].On {
			need(d.Outcomes, "結果", o)
		}
	}
	need(d.Roles, "担当", RoleHuman)
	for _, s := range w.States {
		need(d.States, "状態", s)
	}
	for _, s := range EngineStops {
		need(d.Stops, "止まる理由", s)
	}
	for _, h := range w.Human {
		need(d.Commands, "コマンド", h.Command)
	}
	return errs
}
