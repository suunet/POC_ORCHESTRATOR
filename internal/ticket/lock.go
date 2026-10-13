package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// LockFile は、チケットを実行している gateflow の印。同じチケットの2つの run（人が誤って2回 run するなど）を止める（M2.8）。
// 実行の記録（runs/）の外に置く。中身は running.json と同じ形（プロセス ID とその開始時刻）。
const LockFile = "run.lock"

// ErrLocked は、ほかの gateflow がこのチケットを実行していること。
var ErrLocked = errors.New("ほかの gateflow がこのチケットを実行している")

// Lock は、このチケットの実行の印を O_EXCL で作り、外す関数を返す。印があり、そのプロセスが生きていれば ErrLocked を返す。
// 生きていなければ（前の実行が異常に終わった）、印を取り直す。
func (t *Ticket) Lock(now time.Time) (unlock func() error, err error) {
	pid := os.Getpid()
	start, err := ProcessStart(pid)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(Running{PID: pid, Start: start, Since: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(t.Path), LockFile)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			if _, werr := f.Write(data); werr != nil {
				f.Close()
				os.Remove(path)
				return nil, werr
			}
			if cerr := f.Close(); cerr != nil {
				os.Remove(path)
				return nil, cerr
			}
			return func() error {
				if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				return nil
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		held, rerr := os.ReadFile(path)
		if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, rerr
		}
		if rerr == nil {
			// 中身が読めない印（書きかけ）は、生きているものとして扱う（取り違えて2つ走らせない）
			r, derr := DecodeRunning(held)
			if derr != nil {
				return nil, fmt.Errorf("%w（%s の中身が読めない：%v。ほかの gateflow がなければ消してよい）", ErrLocked, path, derr)
			}
			if r.Alive() {
				return nil, fmt.Errorf("%w（プロセス %d、%s から）", ErrLocked, r.PID, r.Since)
			}
			// 前の実行の印が残っている（プロセスはもうない）ので、取り直す
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("%w（%s を取り直せない）", ErrLocked, path)
}
