// Package queue は、AI の呼び出しの順番待ちの印を扱う（M2.9）。
//
// 複数の run は同時に動いてよいが、同じ認証ボリュームを使う AI の呼び出し（エージェントのコンテナ）は同時に1つに限る
// （トークンの更新の競合を避ける。人の決定）。印は、ホスト全体の場所に、認証ボリュームの名前ごとに置く。
// 作り方は run.lock と同じ：O_EXCL で作り、プロセス ID とその開始時刻を書き、そのプロセスが生きていなければ取り直す。
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gateflow/internal/ticket"
)

// Holder は、印を持っているもの（誰の、どの AI の呼び出しか）。login の画面と記録に出す。
type Holder struct {
	PID     int    `json:"pid"`
	Start   string `json:"start"` // OS が返すプロセスの開始時刻（プロセス ID の再利用を見分ける）
	Since   string `json:"since"`
	Project string `json:"project,omitempty"`
	Ticket  string `json:"ticket,omitempty"`
	Step    string `json:"step,omitempty"` // login なら "login"
}

// 取り直しの間隔と、中身の読めない印を残りとみなすまでの時間（書きかけの印を取り違えないため）。
const (
	DefaultInterval = 2 * time.Second
	unreadableAfter = 10 * time.Second
)

// Lock は、1つの認証ボリュームの順番待ちの印。
type Lock struct {
	Path     string
	Interval time.Duration
	Now      func() time.Time
	// Alive は、印のプロセスが今も同じプロセスとして動いているか（テストで差し替える）
	Alive func(Holder) bool
}

// New は、認証ボリューム volume の印を、ホスト全体の場所（利用者の設定のディレクトリ）に置く Lock を返す。
func New(volume string) (*Lock, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("順番待ちの印の置き場がない: %w", err)
	}
	return &Lock{Path: filepath.Join(dir, "gateflow", "agent-"+volume+".lock")}, nil
}

func (l *Lock) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Lock) alive(h Holder) bool {
	if l.Alive != nil {
		return l.Alive(h)
	}
	start, err := ticket.ProcessStart(h.PID)
	return err == nil && start == h.Start
}

// Self は、このプロセスの Holder を作る。
func Self(project, ticketID, step string, now time.Time) (Holder, error) {
	pid := os.Getpid()
	start, err := ticket.ProcessStart(pid)
	if err != nil {
		return Holder{}, err
	}
	return Holder{PID: pid, Start: start, Since: now.UTC().Format(time.RFC3339), Project: project, Ticket: ticketID, Step: step}, nil
}

// TryAcquire は、印を1回だけ取りにいく。取れたら外す関数を、ほかが持っていればその Holder を返す。
func (l *Lock) TryAcquire(h Holder) (release func() error, busy *Holder, err error) {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, werr := f.Write(data); werr != nil {
				f.Close()
				os.Remove(l.Path)
				return nil, nil, werr
			}
			if cerr := f.Close(); cerr != nil {
				os.Remove(l.Path)
				return nil, nil, cerr
			}
			return func() error {
				if err := os.Remove(l.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				return nil
			}, nil, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, nil, err
		}
		held, info, rerr := readHolder(l.Path)
		switch {
		case errors.Is(rerr, fs.ErrNotExist):
			continue // 持ち主がちょうど外した
		case rerr != nil && info != nil && l.now().Sub(info.ModTime()) < unreadableAfter:
			// 書きかけの印かもしれないので、持たれているものとして扱う
			return nil, &Holder{}, nil
		case rerr != nil:
			// 中身の読めない古い印は、残りとして消す
		case l.alive(held):
			return nil, &held, nil
		}
		if err := os.Remove(l.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
	}
	return nil, nil, fmt.Errorf("順番待ちの印 %s を取り直せない", l.Path)
}

// Acquire は、印が取れるまで待つ（失敗にしない。取り消しには従う）。ほかが持っている間は、持ち主が変わるたびに
// onWait を呼ぶ（記録や画面に出すため。黙って待たない）。取れたら、外す関数と待った時間を返す。
func (l *Lock) Acquire(ctx context.Context, h Holder, onWait func(Holder)) (release func() error, waited time.Duration, err error) {
	begin := l.now()
	interval := l.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	var last *Holder
	for {
		release, busy, err := l.TryAcquire(h)
		if err != nil {
			return nil, 0, err
		}
		if busy == nil {
			return release, l.now().Sub(begin), nil
		}
		if onWait != nil && (last == nil || last.PID != busy.PID || last.Ticket != busy.Ticket || last.Step != busy.Step) {
			onWait(*busy)
		}
		last = busy
		select {
		case <-ctx.Done():
			return nil, l.now().Sub(begin), context.Cause(ctx)
		case <-time.After(interval):
		}
	}
}

func readHolder(path string) (Holder, fs.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Holder{}, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Holder{}, info, err
	}
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil || h.PID <= 0 || h.Start == "" {
		return Holder{}, info, fmt.Errorf("順番待ちの印の中身が読めない")
	}
	return h, info, nil
}
