package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gateflow/internal/queue"
)

func newLock(t *testing.T, alive map[int]bool) *queue.Lock {
	t.Helper()
	return &queue.Lock{Path: filepath.Join(t.TempDir(), "gateflow", "agent-auth.lock"), Interval: 5 * time.Millisecond,
		Alive: func(h queue.Holder) bool { return alive[h.PID] }}
}

// 1つ目が持つ間は2つ目が待ち（持ち主を知らせる）、外すと取れる。
func TestAcquire_WaitsForHolder(t *testing.T) {
	l := newLock(t, map[int]bool{1: true, 2: true})
	first := queue.Holder{PID: 1, Start: "S", Ticket: "T-001", Step: "p5"}
	release, _, err := l.Acquire(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []queue.Holder
	done := make(chan error, 1)
	go func() {
		rel, waited, err := l.Acquire(context.Background(), queue.Holder{PID: 2, Start: "S", Ticket: "T-002", Step: "p2-review"}, func(h queue.Holder) {
			mu.Lock()
			seen = append(seen, h)
			mu.Unlock()
		})
		if err == nil {
			if waited <= 0 {
				err = errors.New("待った時間が 0")
			}
			rel()
		}
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("1つ目が持っているのに取れた: %v", err)
	default:
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Ticket != "T-001" || seen[0].Step != "p5" {
		t.Errorf("待っている間に知らせた持ち主 = %+v（持ち主が変わらなければ1回だけ）", seen)
	}
}

// 印のプロセスがない（異常に終わった）印は取り直す。中身の読めない新しい印は持たれているものとして扱う。
func TestTryAcquire_StaleAndUnreadable(t *testing.T) {
	l := newLock(t, map[int]bool{})
	os.MkdirAll(filepath.Dir(l.Path), 0o700)
	stale, _ := json.Marshal(queue.Holder{PID: 99, Start: "S", Ticket: "T-009"})
	os.WriteFile(l.Path, stale, 0o600)
	release, busy, err := l.TryAcquire(queue.Holder{PID: 2, Start: "S"})
	if err != nil || busy != nil || release == nil {
		t.Fatalf("残りの印を取り直せない: %v, %+v", err, busy)
	}
	release()
	os.WriteFile(l.Path, []byte("{"), 0o600)
	if _, busy, err := l.TryAcquire(queue.Holder{PID: 2, Start: "S"}); err != nil || busy == nil {
		t.Errorf("書きかけの印 = %v, %+v, want 持たれている", err, busy)
	}
}

// 待っている間の取り消しには従う。
func TestAcquire_Cancel(t *testing.T) {
	l := newLock(t, map[int]bool{1: true})
	if _, _, err := l.Acquire(context.Background(), queue.Holder{PID: 1, Start: "S"}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := l.Acquire(ctx, queue.Holder{PID: 2, Start: "S"}, nil); err == nil {
		t.Fatal("取り消したのに待ち続けた")
	}
}
