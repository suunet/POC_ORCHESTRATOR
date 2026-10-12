package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"gateflow/builtins"
	"gateflow/internal/monitor"
	"gateflow/internal/ticket"
)

const defaultMonitorPort = 7420

// cmdMonitor は、読み取り専用の監視の UI を 127.0.0.1 だけで待ち受ける（M2.5）。取り消し（Ctrl+C）で終わる。
func cmdMonitor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	srv, err := newMonitorServer(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, err)
		}
		return exitUsage
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fmt.Fprintf(stderr, "待ち受けられない: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "監視の UI：http://%s/（読み取り専用。Ctrl+C で終わる）\n", srv.Addr)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	return exitOK
}

// newMonitorServer は、引数から監視の UI のサーバーを作る。待ち受けは 127.0.0.1 に限る。
func newMonitorServer(args []string, stderr io.Writer) (*http.Server, error) {
	fset := flag.NewFlagSet("monitor", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dir := fset.String("project", ".", "プロジェクトのルート")
	port := fset.Int("port", defaultMonitorPort, "待ち受けるポート")
	if err := fset.Parse(args); err != nil {
		return nil, err
	}
	if fset.NArg() > 0 || *port < 1 || *port > 65535 {
		return nil, errors.New(usage)
	}
	project, wf, _, err := loadProject(*dir)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return nil, err
	}
	page, err := fs.ReadFile(builtins.FS(), "ui/index.html")
	if err != nil {
		return nil, err
	}
	m := &monitor.Monitor{Dir: filepath.Join(root, ticket.Dir), Project: project, Workflow: wf, Now: time.Now,
		Alive: func(r ticket.Running) bool { return r.Alive() }}
	return &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)),
		Handler:           monitor.Handler(m, page),
		ReadHeaderTimeout: 5 * time.Second,
	}, nil
}
