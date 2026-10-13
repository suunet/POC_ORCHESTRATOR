package container_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gateflow/internal/container"
)

func TestSpec_Args(t *testing.T) {
	spec := container.Spec{
		Name:    "gateflow-p5-t-001",
		Image:   "gateflow-agent-sample",
		Network: "gateflow-sample-internal",
		User:    "agent",
		Workdir: "/work",
		Env:     map[string]string{"GOPROXY": "off", "B": "2"},
		Mounts: []container.Mount{
			{Type: container.Volume, Source: "gateflow-work-sample", Subpath: "T-001/repo", Target: "/work"},
			{Type: container.Volume, Source: "gateflow-work-sample", Subpath: "T-001/repo/.git", Target: "/work/.git", ReadOnly: true},
			{Type: container.Bind, Source: "/p/.gateflow/env.template", Target: "/work/.env", ReadOnly: true},
		},
		Cmd: []string{"claude", "-p", "x"},
	}
	got := spec.Args()
	want := []string{
		"run", "--rm", "--name", "gateflow-p5-t-001",
		"--network", "gateflow-sample-internal",
		"--user", "agent", "--workdir", "/work",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL",
		"--env", "B=2", "--env", "GOPROXY=off",
		"--mount", "type=volume,source=gateflow-work-sample,target=/work,volume-subpath=T-001/repo",
		"--mount", "type=volume,source=gateflow-work-sample,target=/work/.git,volume-subpath=T-001/repo/.git,readonly",
		"--mount", "type=bind,source=/p/.gateflow/env.template,target=/work/.env,readonly",
		"gateflow-agent-sample", "claude", "-p", "x",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args =\n%q\nwant\n%q", got, want)
	}
}

func TestSpec_ArgsStdinAndTTY(t *testing.T) {
	spec := container.Spec{Name: "n", Image: "img", Stdin: strings.NewReader("x")}
	if got := spec.Args(); !slices.Contains(got, "--interactive") || slices.Contains(got, "--tty") {
		t.Errorf("Args = %q, want --interactive だけ", got)
	}
	spec = container.Spec{Name: "n", Image: "img", TTY: true}
	if got := spec.Args(); !slices.Contains(got, "--interactive") || !slices.Contains(got, "--tty") {
		t.Errorf("Args = %q, want --interactive と --tty", got)
	}
}

// fakeDocker は、テストのプログラム自身を docker の代わりに起動する。
// run のときは mode のふるまいをし、それ以外（rm など）は引数を logFile に追記する。
func fakeDocker(t *testing.T, mode string) (container.Docker, string) {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "calls.log")
	d := container.Docker{
		Bin: "docker",
		Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=TestHelperProcess", "--"}, args...)...)
			cmd.Env = append(os.Environ(), "GATEFLOW_TEST_HELPER="+mode, "GATEFLOW_TEST_LOG="+logFile)
			return cmd
		},
	}
	return d, logFile
}

// TestHelperProcess は、fakeDocker から起動されたときだけ動く。
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("GATEFLOW_TEST_HELPER")
	if mode == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	if args[0] != "run" || slices.Contains(args, "--detach") {
		f, _ := os.OpenFile(os.Getenv("GATEFLOW_TEST_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
		if mode == "detached-no-network" && len(args) > 1 && args[0] == "network" && args[1] == "inspect" {
			os.Exit(1)
		}
		// network-race：最初の inspect はない、create は失敗（同時に別の実行が作った）、次の inspect はある
		if mode == "network-race" && len(args) > 1 && args[0] == "network" {
			calls, _ := os.ReadFile(os.Getenv("GATEFLOW_TEST_LOG"))
			if args[1] == "create" || (args[1] == "inspect" && strings.Count(string(calls), "network inspect") == 1) {
				os.Exit(1)
			}
		}
		if mode == "follow" && args[0] == "logs" {
			fmt.Println(`{"decision":"deny","target":"example.com:443"}`)
			fmt.Fprintln(os.Stderr, "stderr の行")
		}
		os.Exit(0)
	}
	switch mode {
	case "exit3":
		fmt.Println("out")
		fmt.Fprintln(os.Stderr, "err")
		os.Exit(3)
	case "silent":
		time.Sleep(10 * time.Second)
	case "chatty":
		for range 200 {
			fmt.Println("tick")
			time.Sleep(20 * time.Millisecond)
		}
	}
	os.Exit(0)
}

func TestDocker_RunExitCode(t *testing.T) {
	d, _ := fakeDocker(t, "exit3")
	var out, errOut bytes.Buffer
	code, err := d.Run(context.Background(), container.Spec{Name: "n", Image: "img"}, &out, &errOut)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 3 || out.String() != "out\n" || errOut.String() != "err\n" {
		t.Errorf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

// 出力が途切れたら打ち切り、コンテナを消す。
func TestDocker_RunIdleTimeout(t *testing.T) {
	d, logFile := fakeDocker(t, "silent")
	spec := container.Spec{Name: "gateflow-x", Image: "img", IdleTimeout: 200 * time.Millisecond, Timeout: 5 * time.Second}
	start := time.Now()
	_, err := d.Run(context.Background(), spec, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, container.ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("打ち切りが遅い: %v", time.Since(start))
	}
	assertRemoved(t, logFile, "gateflow-x")
}

// 出力が続いていても、全体の時間の上限で打ち切る。
func TestDocker_RunTimeout(t *testing.T) {
	d, logFile := fakeDocker(t, "chatty")
	spec := container.Spec{Name: "gateflow-y", Image: "img", IdleTimeout: time.Second, Timeout: 300 * time.Millisecond}
	_, err := d.Run(context.Background(), spec, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, container.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	assertRemoved(t, logFile, "gateflow-y")
}

func TestDocker_RunRequiresName(t *testing.T) {
	d, _ := fakeDocker(t, "exit3")
	if _, err := d.Run(context.Background(), container.Spec{Image: "img"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("名前のないコンテナを起動した")
	}
}

func assertRemoved(t *testing.T, logFile, name string) {
	t.Helper()
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "rm --force "+name) {
		t.Errorf("コンテナ %s を消していない。呼び出し:\n%s", name, data)
	}
}

// 呼び出し元が止めた（Ctrl+C など）ときも、コンテナを消す。
func TestDocker_RunCancelled(t *testing.T) {
	d, logFile := fakeDocker(t, "silent")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := d.Run(ctx, container.Spec{Name: "gateflow-z", Image: "img"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertRemoved(t, logFile, "gateflow-z")
}
