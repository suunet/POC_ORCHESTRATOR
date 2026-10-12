package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RunningFile は、実行の間だけ実行の記録に置く印のファイル。監視の UI が、実行中と中断を見分けるのに使う。
const RunningFile = "running.json"

// Running は、実行中の印。プロセス ID は再利用されるので、OS が返すプロセスの開始時刻と組で同じプロセスかを確かめる。
type Running struct {
	PID   int    `json:"pid"`
	Start string `json:"start"` // OS が返すプロセスの開始時刻（ps の lstart）
	Since string `json:"since"` // 印を置いた時刻（表示用）
}

// MarkRunning は、このプロセスの実行中の印を置き、消す関数を返す。書き込みは一時ファイルからの改名で行う。
func (r *Run) MarkRunning(now time.Time) (func(), error) {
	pid := os.Getpid()
	start, err := ProcessStart(pid)
	if err != nil {
		return func() {}, err
	}
	data, err := json.Marshal(Running{PID: pid, Start: start, Since: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return func() {}, err
	}
	path := filepath.Join(r.Dir, RunningFile)
	tmp, err := os.CreateTemp(r.Dir, RunningFile+".*")
	if err != nil {
		return func() {}, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return func() {}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return func() {}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return func() {}, err
	}
	return func() { os.Remove(path) }, nil
}

// DecodeRunning は、実行中の印の中身を読む。
func DecodeRunning(data []byte) (Running, error) {
	var r Running
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("%s の形式が正しくない: %w", RunningFile, err)
	}
	if r.PID <= 0 || r.Start == "" {
		return r, errors.New(RunningFile + " にプロセス ID か開始時刻がない")
	}
	return r, nil
}

// Alive は、印のプロセスが今も同じプロセスとして動いているかを返す（プロセス ID の再利用を見分ける）。
func (r Running) Alive() bool {
	start, err := ProcessStart(r.PID)
	return err == nil && start == r.Start
}

// ProcessStart は、プロセスの開始時刻を OS から得る。プロセスがなければ error を返す。
func ProcessStart(pid int) (string, error) {
	// NOTE: 標準ライブラリにはほかのプロセスの開始時刻を得る手段がないので、macOS と Linux にある ps を使う
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	start := strings.Join(strings.Fields(string(out)), " ")
	if err != nil || start == "" {
		return "", fmt.Errorf("プロセス %d がない", pid)
	}
	return start, nil
}
