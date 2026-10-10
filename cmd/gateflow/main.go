// gateflow は、AIDDのプロセスをローカルで自動で回すオーケストレーター。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"gateflow/internal/config"
	"gateflow/internal/idcheck"
)

// version はビルド時に -ldflags で刻む。
var version = "dev"

// 終了コード。
const (
	exitOK     = 0
	exitFailed = 1 // 検査に不合格
	exitUsage  = 2 // 使い方や設定の誤り
)

const usage = `使い方:
  gateflow version
  gateflow check [--project <dir>]      すべての検証（M0ではID整合チェックだけ）
  gateflow check ids [--project <dir>]  ID整合チェックだけ
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "gateflow %s\n", version)
		return exitOK
	case "check":
		rest := args[1:]
		if len(rest) > 0 && rest[0] == "ids" {
			rest = rest[1:]
		}
		return checkIDs(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "知らないコマンド: %s\n%s", args[0], usage)
		return exitUsage
	}
}

func checkIDs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("project", ".", "プロジェクトのルート")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	p, err := config.LoadProject(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	rules, err := idcheck.NewRules(*p.IDs)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	res, err := idcheck.Run(*dir, rules)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	required, recommended := 0, 0
	for _, f := range res.Findings {
		if f.Level == idcheck.Required {
			required++
		} else {
			recommended++
		}
		fmt.Fprintf(stdout, "%s [%s] %s:%d %s\n", f.Level, f.Kind, f.File, f.Line, f.Message)
	}
	fmt.Fprintf(stdout, "ID整合チェック: 定義 %d件（子 %d件）、必須 %d件、推奨 %d件\n",
		res.Definitions, res.Children, required, recommended)
	if res.HasErrors() {
		return exitFailed
	}
	return exitOK
}
