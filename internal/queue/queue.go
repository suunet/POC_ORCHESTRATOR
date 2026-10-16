// Package queue は、AI の呼び出しの順番待ちの印を扱う（M2.9）。
//
// 複数の run は同時に動いてよいが、同じ認証ボリュームを使う AI の呼び出し（エージェントのコンテナ）は同時に1つに限る
// （トークンの更新の競合を避ける。人の決定）。印は、ホスト全体の場所に、認証ボリュームの名前ごとに置く。
// 作り方は run.lock と同じ：排他は助言ロック（flock。プロセスが終われば OS が外す。M3.1）で、中身は持ち主の情報
// （login の画面と、順番待ちの記録の表示に使う）。外すと中身は空になり、ファイルは残る。
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gateflow/internal/flock"
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

// DefaultInterval は、取り直しの間隔。
const DefaultInterval = 2 * time.Second

// Lock は、1つの認証ボリュームの順番待ちの印。
type Lock struct {
	Path     string
	Interval time.Duration
	Now      func() time.Time
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

// Self は、このプロセスの Holder を作る。
func Self(project, ticketID, step string, now time.Time) (Holder, error) {
	pid := os.Getpid()
	start, err := ticket.ProcessStart(pid)
	if err != nil {
		return Holder{}, err
	}
	return Holder{PID: pid, Start: start, Since: now.UTC().Format(time.RFC3339), Project: project, Ticket: ticketID, Step: step}, nil
}

// TryAcquire は、印を1回だけ取りにいく。取れたら外す関数を、ほかが持っていればその Holder を返す
// （中身が書きかけで読めなければ、空の Holder）。
func (l *Lock) TryAcquire(h Holder) (release func() error, busy *Holder, err error) {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return nil, nil, err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	fl, ok, err := flock.TryLock(l.Path, 0o600, data)
	if err != nil {
		return nil, nil, fmt.Errorf("順番待ちの印 %s を取れない: %w", l.Path, err)
	}
	if ok {
		return fl.Unlock, nil, nil
	}
	held, err := readHolder(l.Path)
	if err != nil {
		return nil, &Holder{}, nil
	}
	return nil, &held, nil
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

func readHolder(path string) (Holder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Holder{}, err
	}
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil || h.PID <= 0 {
		return Holder{}, fmt.Errorf("順番待ちの印の中身が読めない")
	}
	return h, nil
}
