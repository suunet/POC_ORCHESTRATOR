// Package gate は、検証コマンド（build・test・lint）の実行と判定を扱う。
package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"gateflow/internal/config"
	"gateflow/internal/container"
)

// 検証コマンドの1回の時間の上限。
const (
	timeout     = 20 * time.Minute
	idleTimeout = 10 * time.Minute
)

// Runner は、コンテナを起動する。
type Runner interface {
	Run(ctx context.Context, spec container.Spec, stdout, stderr io.Writer) (int, error)
}

// Command は、検証コマンドの1つ。
type Command struct {
	Name string
	Cmd  string
}

// Commands は、検証コマンドの対応表を実行の順（build・test・lint）に並べる。
func Commands(v *config.Verify) []Command {
	return []Command{{"build", v.Build}, {"test", v.Test}, {"lint", v.Lint}}
}

// Result は、検証コマンドの1つの結果。
type Result struct {
	Name     string
	Cmd      string
	ExitCode int
	Output   string
	Duration time.Duration
}

// Gate は、検証用のコンテナで検証コマンドを実行する。Base は、名前とコマンドを除いた起動の仕様。
type Gate struct {
	Runner Runner
	Base   container.Spec
}

// Run は、cmds を順に別のコンテナで実行する。1つが不合格でも残りを実行し、すべて合格なら passed を返す。
func (g Gate) Run(ctx context.Context, cmds []Command) (results []Result, passed bool, err error) {
	passed = true
	for _, c := range cmds {
		spec := g.Base
		spec.Name = g.Base.Name + "-" + c.Name
		spec.Cmd = []string{"sh", "-c", c.Cmd}
		spec.Timeout, spec.IdleTimeout = timeout, idleTimeout
		var out bytes.Buffer
		start := time.Now()
		code, err := g.Runner.Run(ctx, spec, &out, &out)
		if err != nil {
			return results, false, fmt.Errorf("検証コマンド %s を実行できない: %w", c.Name, err)
		}
		results = append(results, Result{Name: c.Name, Cmd: c.Cmd, ExitCode: code, Output: out.String(), Duration: time.Since(start)})
		if code != 0 {
			passed = false
		}
	}
	return results, passed, nil
}
