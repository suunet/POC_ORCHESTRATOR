package container_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gateflow/internal/container"
)

// echoServer は、受け取ったバイト列をそのまま返す TCP のサーバーを起動して、host:port を返す。
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// syncBuffer は、並行に書かれる記録を受け取る。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startProxy(t *testing.T, allow []string) (string, *syncBuffer) {
	t.Helper()
	log := &syncBuffer{}
	p, err := container.NewProxy(allow, log)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), log
}

// connect は、プロキシに CONNECT を送り、応答の状態コードと、つながった接続を返す。
func connect(t *testing.T, proxy, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("応答を読めない: %v", err)
	}
	return resp.StatusCode, c, br
}

func TestProxy_AllowedConnect(t *testing.T) {
	target := echoServer(t)
	proxy, log := startProxy(t, []string{target})

	code, c, br := connect(t, proxy, target)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	io.WriteString(c, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("トンネルが通らない: %q %v", got, err)
	}
	if !strings.Contains(log.String(), `"decision":"allow"`) {
		t.Errorf("許可の記録がない: %s", log)
	}
}

func TestProxy_DeniedConnect(t *testing.T) {
	target := echoServer(t)
	proxy, log := startProxy(t, []string{"api.example.com:443"})

	code, _, _ := connect(t, proxy, target)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	var rec map[string]any
	line := strings.TrimSpace(log.String())
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("記録が JSON でない: %q", line)
	}
	if rec["decision"] != "deny" || rec["target"] != target || rec["method"] != "CONNECT" {
		t.Errorf("記録 = %v", rec)
	}
}

// 許可リストはホストとポートの組で比べる。大文字・小文字と末尾の点は区別しない。
func TestProxy_HostMatching(t *testing.T) {
	p, err := container.NewProxy([]string{"API.Example.com:443"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]bool{
		"api.example.com:443":      true,
		"api.example.com.:443":     true,
		"api.example.com:80":       false,
		"evil.api.example.com:443": false,
		"api.example.com.evil:443": false,
		"93.184.216.34:443":        false,
	} {
		if got := p.Allowed(target); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", target, got, want)
		}
	}
}

// CONNECT でない要求（平文の HTTP の転送）は、許可リストのホストでも通さない。
func TestProxy_PlainHTTPDenied(t *testing.T) {
	target := echoServer(t)
	proxy, log := startProxy(t, []string{target})
	req, _ := http.NewRequest(http.MethodPost, "http://"+target+"/", strings.NewReader("data"))
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req.WriteProxy(c)
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(log.String(), `"method":"POST"`) {
		t.Errorf("拒否の記録がない: %s", log)
	}
}

func TestNewProxy_InvalidAllow(t *testing.T) {
	if _, err := container.NewProxy([]string{"no-port"}, io.Discard); err == nil {
		t.Fatal("ポートのない許可を受け付けた")
	}
}
