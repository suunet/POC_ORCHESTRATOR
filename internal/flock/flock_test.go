package flock_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gateflow/internal/flock"
)

// 持っている間はほかが取れず、外せば取れる。中身は取った側のもので、外すと空になる。ファイルは残る。
func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, ok, err := flock.TryLock(path, 0o600, []byte("A"))
	if err != nil || !ok {
		t.Fatalf("1つ目 = %v, %v", ok, err)
	}
	if b, ok, err := flock.TryLock(path, 0o600, []byte("B")); err != nil || ok || b != nil {
		t.Fatalf("持たれているのに取れた: %v, %v", ok, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "A" {
		t.Errorf("中身 = %q（取れなかった側が書き替えた）", data)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(); err != nil {
		t.Errorf("2回目の Unlock = %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Errorf("外した後の印 = %q, %v（空で残る）", data, err)
	}
	b, ok, err := flock.TryLock(path, 0o600, []byte("BB"))
	if err != nil || !ok {
		t.Fatalf("外した後に取れない: %v, %v", ok, err)
	}
	defer b.Unlock()
	if data, _ := os.ReadFile(path); string(data) != "BB" {
		t.Errorf("中身 = %q", data)
	}
}

// 前の形式の印（中身があるが flock はない）は、そのまま取れる。
func TestTryLock_OldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	os.WriteFile(path, []byte(`{"pid":999999,"start":"S"}`), 0o600)
	l, ok, err := flock.TryLock(path, 0o600, []byte("new"))
	if err != nil || !ok {
		t.Fatalf("前の形式の印を取れない: %v, %v", ok, err)
	}
	l.Unlock()
}

// helper は、子のプロセスとして起動されたときだけ動く。
//   - contend：印が取れるまで取り直し、持っている間に共有の記録へ「入った・出た」を書く（rounds 回）
//   - hold：印を取って、殺されるまで持ち続ける
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("GATEFLOW_FLOCK_HELPER")
	if mode == "" {
		return
	}
	path, logPath := os.Getenv("GATEFLOW_FLOCK_PATH"), os.Getenv("GATEFLOW_FLOCK_LOG")
	me := strconv.Itoa(os.Getpid())
	switch mode {
	case "hold":
		if _, ok, err := flock.TryLock(path, 0o600, []byte(me)); err != nil || !ok {
			os.Exit(2)
		}
		fmt.Println("held")
		time.Sleep(time.Minute)
	case "contend":
		rounds, _ := strconv.Atoi(os.Getenv("GATEFLOW_FLOCK_ROUNDS"))
		for range rounds {
			var l *flock.Lock
			for {
				var ok bool
				var err error
				if l, ok, err = flock.TryLock(path, 0o600, []byte(me)); err != nil {
					os.Exit(2)
				} else if ok {
					break
				}
				time.Sleep(time.Millisecond)
			}
			appendLine(logPath, "in "+me)
			time.Sleep(2 * time.Millisecond)
			appendLine(logPath, "out "+me)
			l.Unlock()
		}
	}
	os.Exit(0)
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintln(f, line)
	f.Close()
}

func helper(t *testing.T, mode, path, logPath string, rounds int) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "GATEFLOW_FLOCK_HELPER="+mode, "GATEFLOW_FLOCK_PATH="+path, "GATEFLOW_FLOCK_LOG="+logPath,
		"GATEFLOW_FLOCK_ROUNDS="+strconv.Itoa(rounds))
	return cmd
}

// 前の持ち主が死んで印のファイルが残っている状態から、複数のプロセスが同時に取りにいっても、同時に2つが持つことはない（M3.1）。
func TestTryLock_ManyProcesses(t *testing.T) {
	dir := t.TempDir()
	path, logPath := filepath.Join(dir, "x.lock"), filepath.Join(dir, "log")
	os.WriteFile(path, []byte(`{"pid":999999,"start":"S"}`), 0o600) // 死んだ持ち主の印
	const procs, rounds = 6, 20
	var cmds []*exec.Cmd
	for range procs {
		c := helper(t, "contend", path, logPath, rounds)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("子のプロセス: %v", err)
		}
	}
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	holder, n := "", 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		op, who, _ := strings.Cut(sc.Text(), " ")
		switch {
		case op == "in" && holder != "":
			t.Fatalf("%s が持っている間に %s も持った（%d 行目）", holder, who, n+1)
		case op == "in":
			holder = who
		case op == "out" && holder != who:
			t.Fatalf("%s が持っていないのに外した（%d 行目）", who, n+1)
		case op == "out":
			holder = ""
		}
		n++
	}
	if n != procs*rounds*2 {
		t.Errorf("記録の行 = %d, want %d", n, procs*rounds*2)
	}
}

// 持ち主のプロセスが異常に終われば（kill）、OS が外すので、次の待ち手は待たずに取れる。
func TestTryLock_HolderKilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	c := helper(t, "hold", path, "", 0)
	out, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("子が印を取れない: %q", line)
	}
	if _, ok, _ := flock.TryLock(path, 0o600, []byte("me")); ok {
		t.Fatal("子が持っているのに取れた")
	}
	if data, _ := os.ReadFile(path); string(data) != strconv.Itoa(c.Process.Pid) {
		t.Errorf("持ち主の中身 = %q, want 子のプロセス ID", data)
	}
	c.Process.Kill()
	c.Wait()
	l, ok, err := flock.TryLock(path, 0o600, []byte("me"))
	if err != nil || !ok {
		t.Fatalf("持ち主が死んだのに取れない: %v, %v", ok, err)
	}
	l.Unlock()
}
