// gateflow は、AIDDのプロセスをローカルで自動で回すオーケストレーター。
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"gateflow/internal/config"
	"gateflow/internal/container"
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
	case "proxy":
		return proxy(args[1:], stdout, stderr)
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

// proxy は、許可リストのプロキシとして動く（内部用。プロキシのコンテナの中で動かす）。判定の記録は stdout に書く。
func proxy(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":3128", "待ち受けるアドレス")
	allow := fs.String("allow", "", "許可する host:port（カンマ区切り）")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *listen == "" {
		fmt.Fprintln(stderr, "使い方: gateflow proxy [--listen <アドレス>] [--allow <host:port>,...]")
		return exitUsage
	}
	var hosts []string
	if *allow != "" {
		hosts = strings.Split(*allow, ",")
	}
	p, err := container.NewProxy(hosts, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	if err := http.ListenAndServe(*listen, p); err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	return exitOK
}
