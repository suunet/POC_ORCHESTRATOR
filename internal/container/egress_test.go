package container_test

import (
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
