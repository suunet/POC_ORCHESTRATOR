// Package container は、エージェントと検証用のコンテナの起動、マウント、通信の制限を扱う。
package container

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Proxy は、許可リストのホストへの CONNECT だけを通すプロキシ。
//
// エージェントは外への経路のない内部ネットワークに置き、このプロキシだけを出口にする。
// HTTPS の中身は見えないので、判定はホストとポートの組だけで行う。平文の HTTP の転送は通さない。
//
// NOTE: プロキシは FROM scratch のイメージで動かす（docker/proxy.Dockerfile）。自分で TLS を張らないので
// CA 証明書は要らず、名前の解決は Go の純正のリゾルバで行う。外へ TLS を張る変更をしたら、イメージを見直す
type Proxy struct {
	allow map[string]bool
	dial  func(network, addr string) (net.Conn, error)

	mu  sync.Mutex
	log io.Writer
}

// NewProxy は、allow（host:port の一覧）を許可リストとするプロキシを作る。判定は log に JSON の行で記録する。
func NewProxy(allow []string, log io.Writer) (*Proxy, error) {
	p := &Proxy{allow: map[string]bool{}, log: log, dial: (&net.Dialer{Timeout: 10 * time.Second}).Dial}
	for _, a := range allow {
		key, err := normalize(a)
		if err != nil {
			return nil, fmt.Errorf("許可リストの %q: %w", a, err)
		}
		p.allow[key] = true
	}
	return p, nil
}

// Allowed は、target（host:port）が許可リストにあるかを返す。
func (p *Proxy) Allowed(target string) bool {
	key, err := normalize(target)
	return err == nil && p.allow[key]
}

func normalize(hostport string) (string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || host == "" || port == "" {
		return "", fmt.Errorf("host:port の形でない")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return net.JoinHostPort(host, port), nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Host != "" {
			target = r.URL.Host
		}
		p.record("deny", r.Method, target, "CONNECT でない要求は通さない")
		http.Error(w, "gateflow: CONNECT only", http.StatusForbidden)
		return
	}
	if !p.Allowed(target) {
		p.record("deny", r.Method, target, "許可リストにない")
		http.Error(w, "gateflow: host not allowed", http.StatusForbidden)
		return
	}
	upstream, err := p.dial("tcp", target)
	if err != nil {
		p.record("error", r.Method, target, err.Error())
		http.Error(w, "gateflow: upstream unreachable", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "gateflow: hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	p.record("allow", r.Method, target, "")
	io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go func() {
		// NOTE: Hijack の前にクライアントが送ったバイトが buf に残っていることがあるので、先に流す
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			upstream.Write(b)
		}
		io.Copy(upstream, client)
		upstream.Close()
	}()
	io.Copy(client, upstream)
	client.Close()
}

func (p *Proxy) record(decision, method, target, reason string) {
	rec := map[string]string{
		"time":     time.Now().UTC().Format(time.RFC3339),
		"decision": decision,
		"method":   method,
		"target":   target,
	}
	if reason != "" {
		rec["reason"] = reason
	}
	line, _ := json.Marshal(rec)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.log.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "gateflow-proxy: 判定の記録を書けない: %v\n", err)
	}
}
