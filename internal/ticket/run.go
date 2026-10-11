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

// Run は、チケットの1回の実行の記録（runs/<日時>/）。
type Run struct {
	Dir string
}

// NewRun は、now の時刻で実行の記録のディレクトリを作る。同じ名前があれば末尾に番号を付ける。
func (t *Ticket) NewRun(now time.Time) (*Run, error) {
	base := filepath.Join(filepath.Dir(t.Path), "runs", now.UTC().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return nil, err
	}
	dir := base
	for i := 2; ; i++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return &Run{Dir: dir}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("実行の記録のディレクトリを作れない: %w", err)
		}
		dir = fmt.Sprintf("%s-%d", base, i)
	}
}

// StepDir は、n 番目の step の記録のディレクトリ（steps/<番号>-<名前>/）を作って返す。
func (r *Run) StepDir(n int, name string) (string, error) {
	dir := filepath.Join(r.Dir, "steps", fmt.Sprintf("%02d-%s", n, name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("step の記録のディレクトリを作れない: %w", err)
	}
	return dir, nil
}

// Log は、出来事を log.jsonl に1行で追記する。fields は time と event に加えて書く項目。
func (r *Run) Log(now time.Time, event string, fields map[string]any) error {
	rec := map[string]any{}
	for k, v := range fields {
		rec[k] = v
	}
	rec["time"] = now.UTC().Format(time.RFC3339)
	rec["event"] = event
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, "log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
