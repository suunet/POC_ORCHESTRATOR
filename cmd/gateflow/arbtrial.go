package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gateflow/builtins"
	"gateflow/internal/engine"
	"gateflow/internal/output"
	"gateflow/internal/ticket"
)

// cmdARBTrial は、ARB の試験（M2.6）。チケットの過去の P2 の差し戻しの記録を入力に、ARB を本番と同じ形で動かし、
// 結果を .docs/_stash/tickets/<T-NNN>/arb-trial/<試験の時刻>/<日時>/ に置く（前の試験は消さず、パスを使い回さない）。
// 使い方には載せない（試験のための口）。
//
//	gateflow arb-trial <T-NNN> [<日時> ...] [--project <dir>]
//
// 日時（runs/ の下の名前）を省くと、P2 が差し戻した実行をすべて、古い順に試す。
func cmdARBTrial(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir, pos, ok := projectFlags("arb-trial", args, stderr)
	if !ok || len(pos) < 1 {
		fmt.Fprintln(stderr, "使い方: gateflow arb-trial <T-NNN> [<日時> ...] [--project <dir>]")
		return exitUsage
	}
	s, err := loadSession(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	tk, err := ticket.Load(dir, pos[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	runsDir := filepath.Join(filepath.Dir(tk.Path), "runs")
	runs := pos[1:]
	if len(runs) == 0 {
		runs = needsFixRuns(runsDir)
	}
	if len(runs) == 0 {
		fmt.Fprintln(stderr, "P2 が差し戻した実行の記録がない")
		return exitUsage
	}
	image, ws, egress, err := s.prepare(ctx, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	q, err := agentQueue(s.agent)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	code := exitOK
	batch := filepath.Join(filepath.Dir(tk.Path), "arb-trial", time.Now().UTC().Format("20060102-150405"))
	for _, run := range runs {
		src := p2StepDir(filepath.Join(runsDir, run))
		if src == "" {
			fmt.Fprintf(stderr, "%s：P2 の step の記録がない\n", run)
			code = exitFailed
			continue
		}
		e := &engine.Engine{Root: dir, Project: s.project, Personal: s.personal, Workflow: s.workflow, Ticket: tk,
			AgentImage: image, Agent: s.agent, Workspace: ws, Egress: egress.For(tk.ID + "-trial"), Runner: s.docker,
			Builtins: builtins.FS(), Rules: s.rules, Now: time.Now, Queue: q}
		dest := filepath.Join(batch, run)
		fmt.Fprintf(stdout, "%s：ARB を試しています…\n", run)
		start := time.Now()
		res, err := e.ARBTrial(ctx, src, dest)
		if err != nil {
			fmt.Fprintf(stderr, "%s：%v\n", run, err)
			code = exitFailed
			continue
		}
		var arb, human int
		if res.Result != nil {
			for _, d := range res.Result.Decisions {
				if d.Decider == "arb" {
					arb++
				} else {
					human++
				}
			}
		}
		fmt.Fprintf(stdout, "%s：裁定 %d・人へ %d、試み %d、出力の不正 %d、合格 %v（%d 秒）。結果：%s\n",
			run, arb, human, res.Attempts, len(res.Invalid), res.OK, int(time.Since(start).Seconds()), dest)
		if !res.OK {
			code = exitFailed
		}
	}
	return code
}

// needsFixRuns は、P2 が差し戻した（needs_fix）実行の記録の名前を、古い順に返す。
func needsFixRuns(runsDir string) []string {
	entries, _ := os.ReadDir(runsDir)
	var out []string
	for _, en := range entries {
		src := p2StepDir(filepath.Join(runsDir, en.Name()))
		if src == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, "out", "result.json"))
		if err != nil {
			continue
		}
		if r, err := output.ParseP2(data); err == nil && r.Outcome == "needs_fix" {
			out = append(out, en.Name())
		}
	}
	sort.Strings(out)
	return out
}

// p2StepDir は、実行の記録の中の、最後の p2-review の step の記録を返す。
func p2StepDir(runDir string) string {
	matches, _ := filepath.Glob(filepath.Join(runDir, "steps", "*-p2-review"))
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}
