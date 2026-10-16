package container_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"gateflow/internal/container"
)

// 共有のサービスを起動し、内部ネットワークと専用のネットワークの両方へサービスの名前でつなぐ（M3）。
func TestServices_Up(t *testing.T) {
	d, logFile := fakeDocker(t, "services")
	s := container.Services{Docker: d, Project: "sample", Root: "/p", Compose: "docker-compose.yaml", Names: []string{"db"}}
	n, err := s.Up(context.Background())
	if err != nil || n != "gateflow-sample-services" {
		t.Fatalf("Up = %q, %v", n, err)
	}
	data, _ := os.ReadFile(logFile)
	for _, w := range []string{
		"compose --project-directory /p -f /p/docker-compose.yaml -p gateflow-sample-services up -d --wait db",
		"network connect --alias db gateflow-sample-internal cid-db",
		"network connect --alias db gateflow-sample-services cid-db",
	} {
		if !strings.Contains(string(data), w) {
			t.Errorf("docker の呼び出しに %q がない:\n%s", w, data)
		}
	}
}

// 専用のネットワークがなければ、外への経路のない（--internal）ネットワークとして作る。つながっていれば、つなぎ直さない。
func TestServices_CreatesNetworkAndSkipsConnected(t *testing.T) {
	d, logFile := fakeDocker(t, "services-no-network")
	s := container.Services{Docker: d, Project: "sample", Root: "/p", Compose: "c.yaml", Names: []string{"db"}}
	if _, err := s.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "network create --internal gateflow-sample-services") {
		t.Errorf("専用のネットワークを作っていない:\n%s", data)
	}
	d, logFile = fakeDocker(t, "services-connected")
	s.Docker = d
	if _, err := s.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(logFile)
	if strings.Contains(string(data), "network connect") {
		t.Errorf("つながっているのに、つなぎ直した:\n%s", data)
	}
}

func TestServices_Down(t *testing.T) {
	d, logFile := fakeDocker(t, "services")
	s := container.Services{Docker: d, Project: "sample", Root: "/p", Compose: "c.yaml", Names: []string{"db"}}
	if err := s.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logFile)
	if !strings.Contains(string(data), "-p gateflow-sample-services down") || !strings.Contains(string(data), "network rm gateflow-sample-services") {
		t.Errorf("docker の呼び出し:\n%s", data)
	}
}
