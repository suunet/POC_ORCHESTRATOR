package container_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"gateflow/internal/container"
)

func TestEgress_StartStop(t *testing.T) {
	d, logFile := fakeDocker(t, "detached")
	e := container.Egress{Docker: d, Project: "sample", ProxyImage: "gateflow-proxy:v1"}

	proxyURL, err := e.Start(context.Background(), []string{"api.example.com:443", "auth.example.com:443"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if proxyURL != "http://"+container.ProxyAlias+":3128" {
		t.Errorf("proxyURL = %s", proxyURL)
	}
	if e.Network() != "gateflow-sample-internal" {
		t.Errorf("Network = %s", e.Network())
	}
	if _, err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	data, _ := os.ReadFile(logFile)
	calls := string(data)
	for _, w := range []string{
		"network inspect gateflow-sample-internal",
		"network connect --alias " + container.ProxyAlias + " gateflow-sample-internal gateflow-sample-proxy",
		"logs gateflow-sample-proxy",
		"rm --force gateflow-sample-proxy",
	} {
		if !strings.Contains(calls, w) {
			t.Errorf("docker の呼び出しに %q がない:\n%s", w, calls)
		}
	}
}

// 内部ネットワークがなければ、外への経路のない（--internal）ネットワークとして作る。
func TestEgress_CreatesInternalNetwork(t *testing.T) {
	d, logFile := fakeDocker(t, "detached-no-network")
	e := container.Egress{Docker: d, Project: "sample", ProxyImage: "gateflow-proxy:v1"}
	if _, err := e.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "network create --internal gateflow-sample-internal") {
		t.Errorf("内部ネットワークを作っていない:\n%s", data)
	}
}

func TestEgress_ProxySpec(t *testing.T) {
	e := container.Egress{Project: "sample", ProxyImage: "gateflow-proxy:v1"}
	args := e.ProxySpec([]string{"a.example.com:443"}).Args()
	got := strings.Join(args, " ")
	for _, w := range []string{"--detach", "--name gateflow-sample-proxy", "gateflow-proxy:v1", "--listen :3128", "--allow a.example.com:443"} {
		if !strings.Contains(got, w) {
			t.Errorf("プロキシの起動に %q がない: %s", w, got)
		}
	}
	if strings.Contains(got, "--rm") {
		t.Errorf("プロキシは記録を取り出してから消すので --rm を付けない: %s", got)
	}
}

// Follow は、プロキシの判定の記録を docker logs --follow で逐次 w へ書く（標準出力と標準エラーの両方）。
func TestEgress_Follow(t *testing.T) {
	d, logFile := fakeDocker(t, "follow")
	e := container.Egress{Docker: d, Project: "sample", ProxyImage: "gateflow-proxy:v1"}
	var buf bytes.Buffer
	if err := e.Follow(context.Background(), &buf); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if !strings.Contains(buf.String(), `"decision":"deny"`) || !strings.Contains(buf.String(), "stderr の行") {
		t.Errorf("書いた内容 = %q", buf.String())
	}
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "logs --follow gateflow-sample-proxy") {
		t.Errorf("docker の呼び出し:\n%s", data)
	}
}

// プロキシの名前と別名は持ち主（チケット）ごと。後片付けは自分の名前だけ（M2.9）。
func TestEgress_PerOwner(t *testing.T) {
	d, logFile := fakeDocker(t, "detached")
	base := container.Egress{Docker: d, Project: "sample", ProxyImage: "gateflow-proxy:v1"}
	a, b := base.For("T-003"), base.For("T-004")
	urlA, err := a.Start(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	urlB, _ := b.Start(context.Background(), nil)
	if urlA != "http://gateflow-proxy-t-003:3128" || urlB != "http://gateflow-proxy-t-004:3128" || a.Network() != b.Network() {
		t.Errorf("URL = %s, %s、ネットワーク %s, %s", urlA, urlB, a.Network(), b.Network())
	}
	a.Stop(context.Background())
	data, _ := os.ReadFile(logFile)
	calls := string(data)
	for _, w := range []string{
		"network connect --alias gateflow-proxy-t-003 gateflow-sample-internal gateflow-sample-t-003-proxy",
		"network connect --alias gateflow-proxy-t-004 gateflow-sample-internal gateflow-sample-t-004-proxy",
	} {
		if !strings.Contains(calls, w) {
			t.Errorf("docker の呼び出しに %q がない:\n%s", w, calls)
		}
	}
	// T-003 の Start と Stop は、T-004 のプロキシを消さない
	if strings.Contains(calls, "rm --force gateflow-sample-t-004-proxy\n"+"network connect --alias gateflow-proxy-t-003") ||
		strings.Count(calls, "rm --force gateflow-sample-t-004-proxy") != 1 {
		t.Errorf("T-004 のプロキシを消したのは、T-004 の Start の前の1回だけのはず:\n%s", calls)
	}
}

// 内部ネットワークの作成が同時に重なって失敗しても、すでにあれば使う。
func TestEgress_NetworkRace(t *testing.T) {
	d, _ := fakeDocker(t, "network-race")
	if _, err := (container.Egress{Docker: d, Project: "sample", ProxyImage: "p"}).For("T-001").Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
}
