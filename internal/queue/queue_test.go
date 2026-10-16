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

func newLock(t *testing.T) *queue.Lock {
	t.Helper()
	return &queue.Lock{Path: filepath.Join(t.TempDir(), "gateflow", "agent-auth.lock"), Interval: 5 * time.Millisecond}
}

// 1つ目が持つ間は2つ目が待ち（持ち主を知らせる）、外すと取れる。
func TestAcquire_WaitsForHolder(t *testing.T) {
	l := newLock(t)
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

// 前の形式の印や、異常に終わった持ち主の印（flock がない）は、そのまま取れる。持っている間に中身が読めなければ、
// 空の持ち主として持たれているものとして扱う。外すと中身は空になる（M3.1）。
func TestTryAcquire_StaleAndUnreadable(t *testing.T) {
	l := newLock(t)
	os.MkdirAll(filepath.Dir(l.Path), 0o700)
	stale, _ := json.Marshal(queue.Holder{PID: 99, Start: "S", Ticket: "T-009"})
	os.WriteFile(l.Path, stale, 0o600)
	release, busy, err := l.TryAcquire(queue.Holder{PID: 2, Start: "S", Ticket: "T-002"})
	if err != nil || busy != nil || release == nil {
		t.Fatalf("残りの印を取れない: %v, %+v", err, busy)
	}
	if _, busy, _ := l.TryAcquire(queue.Holder{PID: 3, Start: "S"}); busy == nil || busy.Ticket != "T-002" {
		t.Errorf("持ち主 = %+v, want T-002", busy)
	}
	os.WriteFile(l.Path, []byte("{"), 0o600) // 書きかけ
	if _, busy, err := l.TryAcquire(queue.Holder{PID: 3, Start: "S"}); err != nil || busy == nil || busy.PID != 0 {
		t.Errorf("書きかけの印 = %v, %+v, want 空の持ち主", err, busy)
	}
	release()
	if data, err := os.ReadFile(l.Path); err != nil || len(data) != 0 {
		t.Errorf("外した後の印 = %q, %v", data, err)
	}
}

// 待っている間の取り消しには従う。
func TestAcquire_Cancel(t *testing.T) {
	l := newLock(t)
	if _, _, err := l.Acquire(context.Background(), queue.Holder{PID: 1, Start: "S"}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := l.Acquire(ctx, queue.Holder{PID: 2, Start: "S"}, nil); err == nil {
		t.Fatal("取り消したのに待ち続けた")
	}
}
