package ticket

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gateflow/internal/flock"
)

// LockFile は、チケットを実行している gateflow の印。同じチケットの2つの run（人が誤って2回 run するなど）を止める（M2.8）。
// 実行の記録（runs/）の外に置く。排他は助言ロック（flock。M3.1）で、中身は running.json と同じ形（プロセス ID と
// その開始時刻）の持ち主の情報（表示に使う）。外すと中身は空になり、ファイルは残る。
const LockFile = "run.lock"

// ErrLocked は、ほかの gateflow がこのチケットを実行していること。
var ErrLocked = errors.New("ほかの gateflow がこのチケットを実行している")

// Lock は、このチケットの実行の印を取り、外す関数を返す。ほかの gateflow が持っていれば ErrLocked を返す。
// 前の実行が異常に終わった印は OS が外しているので、そのまま取れる。
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
	l, ok, err := flock.TryLock(path, 0o644, data)
	if err != nil {
		return nil, fmt.Errorf("実行の印 %s を取れない: %w", path, err)
	}
	if !ok {
		// 持ち主の情報は表示のためだけに読む（書きかけで読めなければ出さない）
		if held, rerr := os.ReadFile(path); rerr == nil {
			if r, derr := DecodeRunning(held); derr == nil {
				return nil, fmt.Errorf("%w（プロセス %d、%s から）", ErrLocked, r.PID, r.Since)
			}
		}
		return nil, ErrLocked
	}
	return l.Unlock, nil
}
