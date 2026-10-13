package engine

import (
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

	"gateflow/internal/container"
	"gateflow/internal/output"
	"gateflow/internal/prompt"
)

// TrialResult は、ARB の試験の1回の結果（M2.6）。
type TrialResult struct {
	Raw    *output.ARBResult `json:"raw,omitempty"` // ARB の出力そのもの（契約の守りの前）
	Result *output.ARBResult `json:"result,omitempty"`
	// Guarded は、契約の守りで人へ回した裁定の番号（試験の指標 D のうち、守りによるものを分けて数える）
	Guarded      []int            `json:"guarded,omitempty"`
	ContractHits map[int][]string `json:"contractHits,omitempty"`
	CatalogReset bool             `json:"catalogReset,omitempty"`
	Changes      []ARBChange      `json:"changes,omitempty"`
	Invalid      []string         `json:"invalid,omitempty"` // 出力の不正（試みごと）
	Attempts     int              `json:"attempts"`
	OK           bool             `json:"ok"`
}

// ARBTrial は、過去の P2 の差し戻しの記録（src：runs/<日時>/steps/NN-p2-review。その時点の写しがそろっている）を入力に、
// ARB を本番と同じコンテナと Instruction で動かし、本番と同じ検査をする（M2.6 の試験）。結果は dest に置く。
// dest は、まだない（一度も使っていない）パスにする。
// 作業場所の clone は使わず、src の seen（BJ が見た版）を写した作業ツリーで動かす。コミットも質疑の書き込みもしない。
func (e *Engine) ARBTrial(ctx context.Context, src, dest string) (TrialResult, error) {
	var res TrialResult
	data, err := os.ReadFile(filepath.Join(src, "out", "result.json"))
	if err != nil {
		return res, fmt.Errorf("P2 の出力がない: %w", err)
	}
	review, err := output.ParseP2(data)
	if err != nil {
		return res, err
	}
	if review.Outcome != "needs_fix" {
		return res, fmt.Errorf("%s の P2 は差し戻していない（%s）", src, review.Outcome)
	}
	e.lastReview = review
	// NOTE: Docker Desktop は、一度消したディレクトリの下を、作り直しても bind できないことがある
	// （bind source path does not exist）。前の試験を消して同じパスを使い回さない
	if _, err := os.Stat(dest); err == nil {
		return res, fmt.Errorf("試験の結果の置き場所 %s がすでにある（使い回さない）", dest)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return res, err
	}
	in := filepath.Join(dest, "in")
	if err := copyTree(filepath.Join(src, "in"), in); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	if err := os.MkdirAll(in, 0o755); err != nil {
		return res, err
	}
	// ARB に渡す入力なので、書けなければ試験を止める
	if err := os.WriteFile(filepath.Join(in, "review.json"), data, 0o644); err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(in, "forced.json"), []byte(`{"findings":[]}`), 0o644); err != nil {
		return res, err
	}

	bind := func(src, target string) container.Mount {
		return container.Mount{Type: container.Bind, Source: src, Target: target, ReadOnly: true}
	}
	var inputs []container.Mount
	knowledge := []string{"指示書は`/in/ticket.md`", "作業場所は`/work`（プロジェクトのリポジトリの作業ツリー）", "カタログ（要件の正本）は`/work/.docs/design/`"}
	for _, f := range []string{"ticket.md", "questions.md", "review.json", "forced.json"} {
		if fileExists(filepath.Join(in, f)) {
			inputs = append(inputs, bind(filepath.Join(in, f), "/in/"+f))
		}
	}
	if fileExists(filepath.Join(in, "questions.md")) {
		knowledge = append(knowledge, "質疑は`/in/questions.md`（状態が「確定」のものが決まったこと）")
	}
	knowledge = append(knowledge, e.arbKnowledge()...)
	var policies []prompt.PolicyFile
	for _, pol := range e.Project.Policies {
		if !slices.Contains(pol.Roles, "arb") {
			continue
		}
		rel := filepath.ToSlash(pol.Path)
		switch {
		case fileExists(filepath.Join(src, "seen", pol.Path)):
			policies = append(policies, prompt.PolicyFile{Path: "/work/" + rel, Purpose: pol.Purpose})
		case fileExists(filepath.Join(in, "policies", pol.Path)):
			inputs = append(inputs, bind(filepath.Join(in, "policies", pol.Path), "/in/policies/"+rel))
			policies = append(policies, prompt.PolicyFile{Path: "/in/policies/" + rel, Purpose: pol.Purpose})
		}
	}
	def := stepDefs["p2-arbitrate"]
	req, err := e.prompt("p2-arbitrate", "arb", def, knowledge, policies)
	if err != nil {
		return res, err
	}
	e.writeDiag(filepath.Join(dest, "prompt.md"), []byte("# システムプロンプト（追加分）\n\n"+req.System+"\n\n# ユーザーメッセージ\n\n"+req.Prompt))

	allow := append(slices.Clone(e.Agent.Hosts()), e.Project.Agent.Allow...)
	proxyURL, err := e.Egress.Start(ctx, allow)
	if err != nil {
		return res, err
	}
	e.proxyURL = proxyURL
	defer func() {
		logs, err := e.Egress.Stop(context.WithoutCancel(ctx))
		if err != nil {
			e.cleanupError("proxy", err)
		}
		e.writeDiag(filepath.Join(dest, ProxyLogFile), logs)
	}()

	limits := e.Project.EffectiveLimits()
	cmd, stdin := e.Agent.Command(req)
	for attempt := 1; attempt <= 1+outputRetries; attempt++ {
		res.Attempts = attempt
		work := filepath.Join(dest, fmt.Sprintf("work-%d", attempt))
		outDir := filepath.Join(dest, fmt.Sprintf("out-%d", attempt))
		if err := copyTree(filepath.Join(src, "seen"), work); err != nil {
			return res, err
		}
		if err := os.MkdirAll(outDir, 0o777); err != nil {
			return res, err
		}
		os.Chmod(outDir, 0o777)
		mounts := []container.Mount{{Type: container.Bind, Source: work, Target: "/work"}}
		spec := agentSpec(fmt.Sprintf("gateflow-%s-%s-trial-p2-arbitrate", e.Project.Name, strings.ToLower(e.Ticket.ID)),
			e.AgentImage, e.Egress.Network(), e.proxyURL, e.Agent, e.Project, mounts, "", outDir, inputs)
		spec.Cmd = cmd
		spec.Stdin = strings.NewReader(stdin)
		spec.Timeout = time.Duration(limits.RunMinutes) * time.Minute
		spec.IdleTimeout = time.Duration(limits.IdleMinutes) * time.Minute
		logFile, err := os.Create(filepath.Join(dest, fmt.Sprintf("agent-%d.jsonl", attempt)))
		if err != nil {
			return res, err
		}
		var stderr strings.Builder
		code, runErr := e.Runner.Run(ctx, spec, logFile, &stderr)
		if err := logFile.Close(); err != nil {
			e.writeError(logFile.Name(), err)
		}
		e.writeDiag(filepath.Join(dest, fmt.Sprintf("agent-%d.stderr", attempt)), []byte(stderr.String()))
		if runErr != nil {
			return res, runErr
		}
		out, err := os.ReadFile(filepath.Join(outDir, "result.json"))
		if err != nil {
			res.Invalid = append(res.Invalid, fmt.Sprintf("/out/result.json がない（終了コード %d）", code))
			continue
		}
		r, err := output.ParseARB(out, review, nil)
		if err != nil {
			res.Invalid = append(res.Invalid, err.Error())
			continue
		}
		raw, _ := output.ParseARB(out, review, nil)
		res.Raw = raw
		v, err := ValidateARB(ARBCheck{Result: r, Review: review, Seen: filepath.Join(src, "seen"), In: in, After: work, Project: e.Project, Rules: e.Rules})
		res.Changes = v.Changes
		if err != nil {
			res.Result = r
			res.Invalid = append(res.Invalid, err.Error())
			continue
		}
		res.Guarded, res.CatalogReset = applyContractGuard(r, v.ContractHits)
		res.ContractHits = v.ContractHits
		res.Result = r
		res.OK = true
		break
	}
	// 試験の結果は人が読む記録なので、書けなければ失敗を返す
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(dest, "trial.json"), b, 0o644); err != nil {
		return res, err
	}
	return res, nil
}

// copyTree は、src の下の通常のファイルを dst へ写す（リンクは写さない）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o777)
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0o666)
		}
		return nil
	})
}
