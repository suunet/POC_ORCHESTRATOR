// Package flock は、助言ロック（flock）で取る印のファイルを扱う（M3.1）。
//
// 排他は flock が受け持つ。プロセスが終われば OS が外すので、死んだ印を見つけて消す処理は持たない（消して取り直す作りには、
// 2つの待ち手が両方取れる窓があった）。ファイルの中身は持ち主の情報（表示に使う）で、取ったときに書き、外すときに空にする。
// ファイルは消さない（消すと、古いファイルを開いて待つ側と、新しいファイルを作った側が別のものを取る）。
//
// NOTE: syscall.Flock は macOS と Linux にある。配布物はこの2つだけ（scripts/build.sh）。
package flock

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// Lock は、取った印。
type Lock struct {
	mu sync.Mutex
	f  *os.File
}

// TryLock は、path の印を1回だけ取りにいく。取れたら中身を content に書き替えて Lock を返す。
// ほかが持っていれば nil と false を返す（待たない）。
func TryLock(path string, perm fs.FileMode, content []byte) (*Lock, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, perm)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	l := &Lock{f: f}
	if err := l.write(content); err != nil {
		l.Unlock()
		return nil, false, err
	}
	return l, true, nil
}

func (l *Lock) write(content []byte) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(content, 0)
	return err
}

// Unlock は、中身を空にしてから印を外す。2回目以降は何もしない。
func (l *Lock) Unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	err := f.Truncate(0)
	if uerr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err == nil {
		err = uerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
