// gateflow は、AIDDのプロセスをローカルで自動で回すオーケストレーター。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
  gateflow login                            使う AI サービスにログインする（個人の設定の agent）
  gateflow run [<T-NNN>] [--project <dir>]  チケットを人間関門まで進める（省くと進められるチケットを順に）
  gateflow resume <T-NNN> [--project <dir>] 止まった実行を再開の位置から続ける
  gateflow status [<T-NNN>] [--project <dir>]  状態と、次にすることを表示する
  gateflow monitor [--project <dir>] [--port <N>]  実行の様子を見る読み取り専用の画面（127.0.0.1 だけ。既定のポート 7420）
  gateflow ready <T-NNN> [--project <dir>]  質疑がすべて確定したら P2 へ進める
  gateflow approve <T-NNN> [--project <dir>]  §4.5 がすべてチェックされたら実装へ進める
  gateflow done <T-NNN> [--project <dir>]   作業ブランチが main に含まれていれば完了にする（マージは git で行う）
  gateflow hold|unhold <T-NNN> [--project <dir>]  保留の印を付ける・外す
  gateflow cancel <T-NNN> [--project <dir>] チケットを中止する
  gateflow doctor sandbox [--project <dir>] エージェントが外へ出られないことを確かめる
  gateflow check [--project <dir>]      すべての検証（M0ではID整合チェックだけ）
  gateflow check ids [--project <dir>]  ID整合チェックだけ
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// signalContext は、Ctrl+C と SIGTERM で取り消される context を返す。
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	case "run", "resume", "login", "doctor", "monitor":
		ctx, cancel := signalContext()
		defer cancel()
		switch args[0] {
		case "run":
			return cmdRun(ctx, args[1:], stdout, stderr)
		case "resume":
			return cmdResume(ctx, args[1:], stdout, stderr)
		case "login":
			return cmdLogin(ctx, args[1:], stdout, stderr)
		case "monitor":
			return cmdMonitor(ctx, args[1:], stdout, stderr)
		default:
			return cmdDoctor(ctx, args[1:], stdout, stderr)
		}
	case "ready", "approve", "done", "hold", "unhold", "cancel":
		return cmdHuman(args[0], args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
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
