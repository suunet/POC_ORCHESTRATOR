package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gateflow/internal/agent"
	"gateflow/internal/container"
	"gateflow/internal/gate"
	"gateflow/internal/idcheck"
	"gateflow/internal/output"
	"gateflow/internal/prompt"
	"gateflow/internal/ticket"
)

// 出力の形が違うときの再実行の回数（設計：1回だけ再実行する）。
const outputRetries = 1

// stepDef は、エージェントの step の、Instruction・出力の説明・出力の読み込み。
type stepDef struct {
	instruction string
	describe    func() string
	parse       func(e *Engine, data []byte) (string, error)
}

var stepDefs = map[string]stepDef{
	"p2-review": {"prompts/instructions/p2-review.md", output.DescribeP2, func(e *Engine, d []byte) (string, error) {
		r, err := output.ParseP2(d)
		if err != nil {
			return "", err
		}
		e.lastReview = r
		return r.Outcome, nil
	}},
	"p3-prepare": {"prompts/instructions/p3-prepare.md", output.DescribeP3, func(e *Engine, d []byte) (string, error) {
		r, err := output.ParseP3(d, e.Ticket.Kind == ticket.KindInfrastructure)
		if err != nil {
			return "", err
		}
		e.lastP3 = r
		return r.Outcome, nil
	}},
	"p5": {"prompts/instructions/p5.md", output.DescribeP5, func(e *Engine, d []byte) (string, error) {
		r, err := output.ParseP5(d, e.Ticket.TestCaseCount() > 0)
		if err != nil {
			return "", err
		}
		e.lastBlock = ""
		if r.BlockReason != nil {
			e.lastBlock = r.BlockReason.Kind + ": " + r.BlockReason.Detail
		}
		return r.Outcome, nil
	}},
	"p2-arbitrate": {"prompts/instructions/p2-arbitrate.md", output.DescribeARB, func(e *Engine, d []byte) (string, error) {
		if e.lastReview == nil {
			return "", fmt.Errorf("ARB に渡す P2 の結果がない")
		}
		r, err := output.ParseARB(d, e.lastReview, e.arbForced)
		if err != nil {
			return "", err
		}
		e.lastARB = r
		return r.Outcome, nil
	}},
	"p5-verify": {"prompts/instructions/p5-verify.md", output.DescribeP5Verify, func(e *Engine, d []byte) (string, error) {
		r, err := output.ParseP5Verify(d)
		if err != nil {
			return "", err
		}
		e.lastReview = r
		return r.Outcome, nil
	}},
}

// execStep は、step を1つ実行して outcome を返す。人の確認待ちで止めるときは errStop を返す。
func (e *Engine) execStep(ctx context.Context, st *State, step, dir string) (string, error) {
	s := e.Workflow.Steps[step]
	if s.Check == "verify-commands" {
		return e.verify(ctx, st, dir)
	}
	outcome, err := e.agentStep(ctx, st, step, s.Agent, dir)
	if err != nil || step != "p5" || outcome != "done" {
		return outcome, err
	}
	// 差し戻しの後の P5 で作業ツリーが変わっていなければ、進展なし（Q-08）
	tree, err := e.Workspace.TreeHash(ctx, e.Ticket.ID)
	if err != nil {
		return "", err
	}
	prev := st.Prints["tree"]
	st.Prints["tree"] = tree
	if prev != "" && prev == tree {
		return "", stop("no-progress", "差し戻しの後の P5 で、作業ツリーが変わっていない（%s）", tree)
	}
	return outcome, nil
}

func (e *Engine) agentStep(ctx context.Context, st *State, step, role, dir string) (string, error) {
	def, ok := stepDefs[step]
	if !ok {
		return "", fmt.Errorf("step %s の出力の形が gateflow にない", step)
	}
	root, err := filepath.Abs(e.Root)
	if err != nil {
		return "", err
	}
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}

	// BJ が見る版を書き出して記録する（根拠の確認は、この版に対して行う。Q-20）。
	// ARB は作業ツリーを書くので、前の書きかけを消してから、直す前の版を書き出す（変更の検査の比べる元。M2.6）
	var seen string
	if role == "bj" || role == "arb" {
		if role == "arb" {
			if err := e.Workspace.Reset(ctx, e.Ticket.ID); err != nil {
				return "", err
			}
		}
		seen = filepath.Join(dir, "seen")
		hash, err := e.Workspace.Export(ctx, e.Ticket.ID, seen)
		if err != nil {
			return "", err
		}
		e.log("reviewed-version", map[string]any{"step": step, "commit": hash})
	}

	inputs, knowledge, err := e.inputs(ctx, st, step, dir)
	if err != nil {
		return "", err
	}
	var policies []prompt.PolicyFile
	for _, pol := range e.Project.Policies {
		if !slices.Contains(pol.Roles, role) {
			continue
		}
		// Git 管理の規約は clone の版を読ませる（clone と規約の版をそろえる。Q-23）。Git 管理外の規約はホストから渡す
		if e.Workspace.Tracked(ctx, e.Project.Branch.Main, filepath.ToSlash(pol.Path)) {
			policies = append(policies, prompt.PolicyFile{Path: "/work/" + filepath.ToSlash(pol.Path), Purpose: pol.Purpose})
			continue
		}
		src := filepath.Join(root, pol.Path)
		if _, err := os.Stat(src); err != nil {
			e.log("policy-missing", map[string]any{"path": pol.Path})
			continue
		}
		target := "/in/policies/" + filepath.ToSlash(pol.Path)
		inputs = append(inputs, container.Mount{Type: container.Bind, Source: src, Target: target, ReadOnly: true})
		policies = append(policies, prompt.PolicyFile{Path: target, Purpose: pol.Purpose})
	}
	if role == "bj" || role == "arb" {
		// BJ と ARB に渡す入力は写しにして、根拠の検査をその写しに対して行う（Q-22）
		if inputs, err = snapshotInputs(inputs, filepath.Join(dir, "in")); err != nil {
			return "", err
		}
	}
	if role == "bj" {
		knowledge = append(knowledge, "指摘の根拠のファイルは、作業場所のファイルならリポジトリの中のパス（例：`.docs/design/uc/UC-001.md`）、渡した入力なら`/in/`のパス（例：`/in/ticket.md`）で書く")
	}
	req, err := e.prompt(step, role, def, knowledge, policies)
	if err != nil {
		return "", err
	}
	os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("# システムプロンプト（追加分）\n\n"+req.System+"\n\n# ユーザーメッセージ\n\n"+req.Prompt), 0o644)

	mounts, envFile := e.Workspace.AgentMounts(e.Ticket.ID), filepath.Join(root, e.Project.Agent.EnvTemplate)
	switch role {
	case "bj":
		// BJ には clone を .git を含めて読み取り専用で渡し、.env は渡さない
		mounts, envFile = e.Workspace.ReviewerMounts(e.Ticket.ID), ""
	case "arb":
		// ARB はカタログを作業ツリーで直す（.git は読み取り専用）。.env は渡さない
		envFile = ""
	}
	limits := e.Project.EffectiveLimits()
	cmd, stdin := e.Agent.Command(req)
	spec := agentSpec(fmt.Sprintf("gateflow-%s-%s-%s", e.Project.Name, strings.ToLower(e.Ticket.ID), step),
		e.AgentImage, e.Egress.Network(), e.proxyURL, e.Agent, e.Project, mounts, envFile, outDir, inputs)
	spec.Cmd = cmd
	spec.Timeout = time.Duration(limits.RunMinutes) * time.Minute
	spec.IdleTimeout = time.Duration(limits.IdleMinutes) * time.Minute

	for attempt := 1; attempt <= 1+outputRetries; attempt++ {
		os.Remove(filepath.Join(outDir, "result.json"))
		if role == "arb" && attempt > 1 {
			if err := e.Workspace.Reset(ctx, e.Ticket.ID); err != nil {
				return "", err
			}
		}
		spec.Stdin = strings.NewReader(stdin)
		e.log("agent-start", map[string]any{"step": step, "attempt": attempt})
		code, err := e.runLogged(ctx, spec, dir, fmt.Sprintf("agent-%d", attempt))
		e.log("agent-end", map[string]any{"step": step, "attempt": attempt, "exitCode": code})
		if errors.Is(err, container.ErrTimeout) || errors.Is(err, container.ErrIdleTimeout) {
			return "", stop("timeout", "%s：%v", step, err)
		}
		if err != nil {
			return "", err
		}
		outcome, err := e.readOutput(ctx, step, def, dir, seen)
		if err == nil {
			if role == "bj" {
				e.writeReview(step)
			}
			switch step {
			case "p2-review":
				st.Review = filepath.Join(outDir, "result.json")
			case "p2-arbitrate":
				st.ArbDecided = e.arbDecided()
			}
			if step == "p5" {
				st.DevResult = filepath.Join(outDir, "result.json")
			}
			if st.Feedback != "" && (outcome == "done" || outcome == "approved") {
				st.Feedback = ""
			}
			return outcome, nil
		}
		e.log("output-invalid", map[string]any{"step": step, "attempt": attempt, "error": err.Error()})
	}
	return "", stop("invalid-output", "%s の出力の形が、再実行しても正しくない", step)
}

func (e *Engine) readOutput(ctx context.Context, step string, def stepDef, dir, seen string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "out", "result.json"))
	if err != nil {
		return "", fmt.Errorf("%s がない", output.ResultPath)
	}
	outcome, err := def.parse(e, data)
	if err != nil {
		return "", err
	}
	if step == "p2-arbitrate" {
		if err := e.checkARBStep(ctx, dir, seen); err != nil {
			return "", err
		}
		return e.lastARB.Outcome, nil // 契約の守りで人へ回したら escalated に変わる
	}
	if seen != "" {
		if err := e.checkEvidence(seen, filepath.Join(dir, "in")); err != nil {
			return "", err
		}
		if step == "p5-verify" && (outcome == "approved" || outcome == "needs_fix") {
			if missing, err := e.missingTargets(seen); err != nil {
				return "", err
			} else if len(missing) > 0 {
				// 予定の ID が P5 で書かれていなければ、P5 後の差し戻しにする（Q-24）
				e.log("target-ids-missing", map[string]any{"ids": missing})
				for _, id := range missing {
					e.lastReview.Findings = append(e.lastReview.Findings, output.Finding{Label: "MUST",
						Finding: fmt.Sprintf("指示書の対象ID の %s が、カタログにない（予定の ID を P5 で書いていない）", id)})
				}
				outcome = "needs_fix"
				e.lastReview.Outcome = outcome
			}
		}
		e.findings = FindingKeys(e.lastReview.Findings)
	}
	return outcome, nil
}

// checkEvidence は、BJ の指摘の根拠が、BJ の見た版に実在するかを確かめる（Q-20、Q-22）。
// 作業場所のファイルは書き出した版（seen）、/in/ の入力は写し（in）に対して確かめる。
// 規約はリポジトリのパスでも /in/policies/ のパスでも受け付け、リポジトリのパスにそろえる。
// どの根拠がどの写しに当たったかを、記録（evidence-check）に残す。
func (e *Engine) checkEvidence(seen, in string) error {
	var ids *idcheck.Result
	var errs []error
	var hits []map[string]any
	if e.planned == nil {
		e.planned = map[string]bool{}
	}
	for i, f := range e.lastReview.Findings {
		ev := f.Evidence
		if ev.File != "" {
			file, path, where, err := resolveEvidence(ev.File, seen, in)
			if err != nil {
				errs = append(errs, fmt.Errorf("findings[%d] %w", i, err))
			} else if data, err := os.ReadFile(path); err != nil {
				errs = append(errs, fmt.Errorf("findings[%d] の根拠のファイル %s が、BJ の見た版にない", i, ev.File))
			} else if ev.Line > 0 && ev.Line > countLines(data) {
				errs = append(errs, fmt.Errorf("findings[%d] の根拠の行 %s:%d が、ファイルの行数（%d）を超える", i, ev.File, ev.Line, countLines(data)))
			} else {
				hits = append(hits, map[string]any{"finding": i, "file": ev.File, "line": ev.Line, "checked": where})
				ev.File = file
			}
		}
		if ev.ID != "" {
			if ids == nil {
				r, err := idcheck.Run(seen, e.Rules)
				if err != nil {
					return err
				}
				ids = &r
			}
			switch {
			case ids.Defined(ev.ID) || appearsInCatalog(seen, ev.ID):
				hits = append(hits, map[string]any{"finding": i, "id": ev.ID, "checked": "catalog"})
			case e.plannedID(ev.ID):
				// 指示書の対象ID に挙げた、これから作る ID（Q-24）。P5 後に実在を確かめる
				e.planned[ev.ID] = true
				hits = append(hits, map[string]any{"finding": i, "id": ev.ID, "checked": "planned"})
			default:
				errs = append(errs, fmt.Errorf("findings[%d] の根拠の ID %s が、カタログにない", i, ev.ID))
			}
		}
	}
	err := errors.Join(errs...)
	e.log("evidence-check", map[string]any{"hits": hits, "ok": err == nil})
	return err
}

// resolveEvidence は、根拠のファイルの書き方から、報告に書くパス・確かめるファイル・当たった先の名前を返す。
func resolveEvidence(file, seen, in string) (string, string, string, error) {
	if rel, ok := strings.CutPrefix(file, "/in/"); ok {
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return "", "", "", fmt.Errorf("の根拠のファイル %s が作業場所の外を指す", file)
		}
		if pol, ok := strings.CutPrefix(rel, "policies/"); ok {
			return pol, filepath.Join(in, filepath.FromSlash(rel)), "input:" + rel, nil
		}
		return file, filepath.Join(in, filepath.FromSlash(rel)), "input:" + rel, nil
	}
	rel := strings.TrimPrefix(file, "/work/")
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", "", "", fmt.Errorf("の根拠のファイル %s が作業場所の外を指す", file)
	}
	if path := filepath.Join(seen, filepath.FromSlash(rel)); fileExists(path) {
		return rel, path, "seen", nil
	}
	if path := filepath.Join(in, "policies", filepath.FromSlash(rel)); fileExists(path) {
		return rel, path, "input:policies/" + rel, nil
	}
	return rel, filepath.Join(seen, filepath.FromSlash(rel)), "seen", nil
}

// snapshotInputs は、/in/ の入力を dir の下へ写し、マウントの元を写しに替える。
func snapshotInputs(inputs []container.Mount, dir string) ([]container.Mount, error) {
	out := make([]container.Mount, 0, len(inputs))
	for _, m := range inputs {
		rel, ok := strings.CutPrefix(m.Target, "/in/")
		if ok && m.Type == container.Bind {
			data, err := os.ReadFile(m.Source)
			if err != nil {
				return nil, err
			}
			dst := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return nil, err
			}
			m.Source = dst
		}
		out = append(out, m)
	}
	return out, nil
}

// plannedID は、id が指示書の対象ID に挙げてあり、ID の形式に合うか（予定の ID として根拠に使えるか。Q-24）を返す。
func (e *Engine) plannedID(id string) bool {
	return e.Rules != nil && e.Rules.Matches(id) && slices.Contains(e.Ticket.TargetIDs(), id)
}

// missingTargets は、指示書の対象ID のうち、ID の形式に合うのに BJ の見た版のカタログにないものを返す。
func (e *Engine) missingTargets(seen string) ([]string, error) {
	if e.Rules == nil {
		return nil, nil
	}
	r, err := idcheck.Run(seen, e.Rules)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, id := range e.Ticket.TargetIDs() {
		if e.Rules.Matches(id) && !r.Defined(id) && !appearsInCatalog(seen, id) {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

func countLines(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// appearsInCatalog は、id がカタログ（.docs/design）のどこかに書かれているかを返す（受入条件の ID など）。
func appearsInCatalog(root, id string) bool {
	found := false
	filepath.WalkDir(filepath.Join(root, ".docs", "design"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(id)) {
			found = true
		}
		return nil
	})
	return found
}

// inputs は、step に渡すファイル（/in の下に読み取り専用）と、プロンプトの知識を返す。
func (e *Engine) inputs(ctx context.Context, st *State, step, dir string) ([]container.Mount, []string, error) {
	t := e.Ticket
	bind := func(src, target string) container.Mount {
		return container.Mount{Type: container.Bind, Source: src, Target: target, ReadOnly: true}
	}
	mounts := []container.Mount{bind(t.Path, "/in/ticket.md")}
	knowledge := []string{
		fmt.Sprintf("指示書は`/in/ticket.md`（%s %s）", t.ID, t.Title),
		"作業場所は`/work`（プロジェクトのリポジトリの作業ツリー。作業ブランチは用意済み）",
		"カタログ（要件の正本）は`/work/.docs/design/`",
	}
	if q := filepath.Join(filepath.Dir(t.Path), "questions.md"); fileExists(q) {
		mounts = append(mounts, bind(q, "/in/questions.md"))
		knowledge = append(knowledge, "質疑は`/in/questions.md`（状態が「確定」のものが決まったこと）")
	}
	if st.Feedback != "" {
		mounts = append(mounts, bind(st.Feedback, "/in/feedback.md"))
		knowledge = append(knowledge, "前回の差し戻しの内容は`/in/feedback.md`。これを直すことを最初に行う")
	}
	switch step {
	case "p2-arbitrate":
		if st.Review == "" {
			return nil, nil, fmt.Errorf("ARB に渡す P2 の結果がない")
		}
		if e.lastReview == nil { // resume で ARB から再開したとき
			data, err := os.ReadFile(st.Review)
			if err != nil {
				return nil, nil, err
			}
			if e.lastReview, err = output.ParseP2(data); err != nil {
				return nil, nil, err
			}
		}
		e.arbForced = arbForced(e.lastReview, st.ArbDecided)
		forced, _ := json.Marshal(map[string][]int{"findings": e.arbForced})
		forcedPath := filepath.Join(dir, "forced.json")
		if err := os.WriteFile(forcedPath, forced, 0o644); err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, bind(st.Review, "/in/review.json"), bind(forcedPath, "/in/forced.json"))
		knowledge = append(knowledge, e.arbKnowledge()...)
	case "p3-prepare":
		tmpl, err := fs.ReadFile(e.Builtins, "templates/catalog/uc.md")
		if err != nil {
			return nil, nil, err
		}
		path := filepath.Join(dir, "uc-template.md")
		if err := os.WriteFile(path, tmpl, 0o644); err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, bind(path, "/in/templates/uc.md"))
	case "p5-verify":
		diff, err := e.Workspace.Diff(ctx, t.ID, e.Project.Branch.Main)
		if err != nil {
			return nil, nil, err
		}
		diffPath := filepath.Join(dir, "diff.patch")
		os.WriteFile(diffPath, []byte(diff), 0o644)
		mounts = append(mounts, bind(diffPath, "/in/diff.patch"))
		if st.DevResult != "" {
			mounts = append(mounts, bind(st.DevResult, "/in/dev-result.json"))
		}
		if st.VerifyLog != "" {
			mounts = append(mounts, bind(st.VerifyLog, "/in/verify.log"))
		}
		knowledge = append(knowledge, "作業ブランチの差分は`/in/diff.patch`、DEV の報告は`/in/dev-result.json`、検証のコマンドの出力は`/in/verify.log`")
	}
	return mounts, knowledge, nil
}

func (e *Engine) prompt(step, role string, def stepDef, knowledge []string, policies []prompt.PolicyFile) (agent.Request, error) {
	persona, err := prompt.LoadBuiltin(e.Builtins, "prompts/personas/"+role+".md")
	if err != nil {
		return agent.Request{}, err
	}
	instruction, err := prompt.LoadBuiltin(e.Builtins, def.instruction)
	if err != nil {
		return agent.Request{}, err
	}
	var builtin []string
	if step == "p5" {
		var v strings.Builder
		v.WriteString("\n### 検証\n\n実装の後に、次のコマンドを作業場所（/work）で実行し、すべて合格することを確かめる。gateflow も同じコマンドで確かめる。\n\n")
		for _, c := range gate.Commands(e.Project.Verify) {
			fmt.Fprintf(&v, "- %s：`%s`\n", c.Name, c.Cmd)
		}
		instruction += v.String()
		testFirst, err := prompt.LoadBuiltin(e.Builtins, "prompts/policies/test-first.md")
		if err != nil {
			return agent.Request{}, err
		}
		builtin = append(builtin, testFirst)
	}
	model := ""
	if e.Personal != nil {
		model = e.Personal.Model
	}
	req := prompt.Build(prompt.Input{
		Persona: persona, Knowledge: knowledge, Instruction: instruction,
		OutputContract: def.describe(), Policies: policies, BuiltinPolicy: builtin, Model: model,
	})
	req.Step = step
	return req, nil
}

// verify は、検証コマンドを実行する。不合格なら、出力の指紋を作る。
func (e *Engine) verify(ctx context.Context, st *State, dir string) (string, error) {
	root, err := filepath.Abs(e.Root)
	if err != nil {
		return "", err
	}
	g := gate.Gate{Runner: e.Runner, Base: container.Spec{
		Name:    fmt.Sprintf("gateflow-%s-%s-verify", e.Project.Name, strings.ToLower(e.Ticket.ID)),
		Image:   e.AgentImage,
		Network: "none",
		User:    "agent",
		Workdir: "/work",
		Env:     offlineEnv(e.Project),
		Mounts: append(e.Workspace.AgentMounts(e.Ticket.ID),
			container.Mount{Type: container.Bind, Source: filepath.Join(root, e.Project.Agent.EnvTemplate), Target: "/work/.env", ReadOnly: true}),
	}}
	results, passed, err := g.Run(ctx, gate.Commands(e.Project.Verify))
	var all strings.Builder
	failed := map[string]string{}
	for _, r := range results {
		os.WriteFile(filepath.Join(dir, "verify-"+r.Name+".log"), []byte(r.Output), 0o644)
		fmt.Fprintf(&all, "## %s（終了コード %d）：`%s`\n\n%s\n", r.Name, r.ExitCode, r.Cmd, r.Output)
		e.log("verify", map[string]any{"name": r.Name, "exitCode": r.ExitCode, "seconds": r.Duration.Seconds()})
		if r.ExitCode != 0 {
			failed[r.Name] = r.Output
		}
	}
	if err != nil {
		return "", err
	}
	logPath := filepath.Join(dir, "verify.log")
	os.WriteFile(logPath, []byte(all.String()), 0o644)
	st.VerifyLog = logPath
	if passed {
		return "passed", nil
	}
	e.print = VerifyFingerprint(failed)
	e.lastVerify = all.String()
	return "failed", nil
}

// feedbackFor は、ループで戻るときに次の step へ渡す差し戻しの内容を作る。
func (e *Engine) feedbackFor(step string) string {
	switch step {
	case "verify":
		return "# 検証のコマンドの不合格\n\n次の検証のコマンドが不合格だった。直してから、もう一度すべて合格することを確かめる。\n\n" + e.lastVerify
	case "p5-verify":
		var b strings.Builder
		b.WriteString("# P5後のレビューの指摘（MUST）\n\n次の指摘を直す。\n\n")
		for _, f := range e.lastReview.Findings {
			if f.Label == "MUST" {
				fmt.Fprintf(&b, "- %s（根拠：%s）", f.Finding, evidenceText(f.Evidence))
				if f.Suggestion != "" {
					fmt.Fprintf(&b, "。提案：%s", f.Suggestion)
				}
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	return ""
}

func evidenceText(ev *output.Evidence) string {
	if ev == nil {
		return ""
	}
	var parts []string
	if ev.File != "" {
		s := ev.File
		if ev.Line > 0 {
			s += fmt.Sprintf(":%d", ev.Line)
		}
		parts = append(parts, s)
	}
	if ev.ID != "" {
		parts = append(parts, ev.ID)
	}
	return strings.Join(parts, "、")
}

// runLogged は、コンテナを起動し、出力を step の記録に残す。
func (e *Engine) runLogged(ctx context.Context, spec container.Spec, dir, name string) (int, error) {
	stdout, err := os.Create(filepath.Join(dir, name+".jsonl"))
	if err != nil {
		return 0, err
	}
	defer stdout.Close()
	var stderr bytes.Buffer
	code, runErr := e.Runner.Run(ctx, spec, stdout, &stderr)
	os.WriteFile(filepath.Join(dir, name+".stderr"), stderr.Bytes(), 0o644)
	return code, runErr
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
