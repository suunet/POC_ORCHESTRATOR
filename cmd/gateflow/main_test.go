package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func project(t *testing.T, actor string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".gateflow/project.json": `{"formatVersion": 2, "name": "sample", "branch": {"main": "main"},
			"agent": {"dockerfile": ".gateflow/agent.Dockerfile", "envTemplate": ".gateflow/env.template", "allow": []},
			"verify": {"build": "go build ./...", "test": "go test ./...", "lint": "go vet ./..."},
			"ids": {"scan": ["docs"], "prefixes": [
			{"prefix": "ACT", "pattern": "ACT-[0-9]{2}", "files": ["docs/actor.md"], "find": "table-first-column", "nameColumn": "アクター"}
		]}, "policies": []}`,
		"docs/actor.md": actor,
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const actorTable = "| ID | アクター |\n|---|---|\n| ACT-01 | ユーザー |\n\nACT-01（ユーザー）\n"

func runCmd(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun_Version(t *testing.T) {
	code, out, _ := runCmd("version")
	if code != 0 || !strings.HasPrefix(out, "gateflow ") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestRun_Usage(t *testing.T) {
	for _, args := range [][]string{{}, {"unknown"}, {"check", "unknown"}} {
		code, _, errOut := runCmd(args...)
		if code != exitUsage || !strings.Contains(errOut, "使い方") {
			t.Errorf("args=%v code=%d err=%q", args, code, errOut)
		}
	}
}

func TestRun_CheckIDs_OK(t *testing.T) {
	root := project(t, actorTable)
	for _, args := range [][]string{{"check", "ids", "--project", root}, {"check", "--project", root}} {
		code, out, errOut := runCmd(args...)
		if code != exitOK {
			t.Errorf("args=%v code=%d out=%q err=%q", args, code, out, errOut)
		}
		if !strings.Contains(out, "必須 0件") {
			t.Errorf("集計がない: %q", out)
		}
	}
}

func TestRun_CheckIDs_Fail(t *testing.T) {
	root := project(t, actorTable+"ACT-02 を参照\n")
	code, out, _ := runCmd("check", "ids", "--project", root)
	if code != exitFailed {
		t.Errorf("code=%d, want %d", code, exitFailed)
	}
	if !strings.Contains(out, "docs/actor.md:6") || !strings.Contains(out, "ACT-02") {
		t.Errorf("指摘の場所がない: %q", out)
	}
}

func TestRun_CheckIDs_BadConfig(t *testing.T) {
	code, _, errOut := runCmd("check", "ids", "--project", t.TempDir())
	if code != exitUsage || !strings.Contains(errOut, "project.json") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// proxy は内部用なので、使い方には出さない。
func TestRun_ProxyHiddenFromUsage(t *testing.T) {
	_, _, errOut := runCmd()
	if strings.Contains(errOut, "proxy") {
		t.Errorf("使い方に proxy が出ている: %q", errOut)
	}
}

func TestRun_ProxyInvalidArgs(t *testing.T) {
	for _, args := range [][]string{
		{"proxy", "--allow", "no-port"},
		{"proxy", "--listen", ""},
		{"proxy", "extra"},
	} {
		code, _, errOut := runCmd(args...)
		if code != exitUsage || errOut == "" || strings.Contains(errOut, "知らないコマンド") {
			t.Errorf("args=%v code=%d err=%q", args, code, errOut)
		}
	}
}

func TestRun_UsageListsCommands(t *testing.T) {
	_, _, errOut := runCmd()
	for _, w := range []string{"gateflow run", "gateflow login", "gateflow doctor sandbox"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("使い方に %q がない: %q", w, errOut)
		}
	}
}

func TestRun_RunArgs(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "T-001", "T-002"}, {"doctor"}, {"doctor", "other"}} {
		code, _, errOut := runCmd(args...)
		if code != exitUsage || strings.Contains(errOut, "知らないコマンド") {
			t.Errorf("args=%v code=%d err=%q", args, code, errOut)
		}
	}
}

// 個人の設定（使う AI サービス）がなければ、docker を使う前に止まる。
func TestRun_RunWithoutPersonalConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := project(t, actorTable)
	for _, args := range [][]string{
		{"run", "T-001", "--project", root},
		{"doctor", "sandbox", "--project", root},
		{"login"},
	} {
		code, _, errOut := runCmd(args...)
		if code != exitUsage || !strings.Contains(errOut, "agent") {
			t.Errorf("args=%v code=%d err=%q, want 個人の設定の agent の案内", args, code, errOut)
		}
	}
}

// writeTicket は、project の下に指示書を書く。
func writeTicket(t *testing.T, root, id, status string) string {
	t.Helper()
	path := filepath.Join(root, ".docs/_stash/tickets", id, "ticket.md")
	os.MkdirAll(filepath.Dir(path), 0o755)
	body := "# " + id + " テスト\n\nステータス: " + status + "\n\n| 項目 | 値 |\n| --- | --- |\n| 種別 | 実装 |\n| ブランチ | feature/x |\n\n### §4.5 承認\n\n- [x] 合意した\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRun_UsageListsM2Commands(t *testing.T) {
	_, _, errOut := runCmd()
	for _, w := range []string{"gateflow ready", "gateflow approve", "gateflow hold", "gateflow cancel", "gateflow status", "gateflow resume"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("使い方に %q がない", w)
		}
	}
	// ダミーの AI の指定は使い方に載せない（Q-13）
	for _, w := range []string{"mock", "GATEFLOW_AGENT", "SCENARIO", "arb-trial"} {
		if strings.Contains(errOut, w) {
			t.Errorf("使い方に %q が出ている", w)
		}
	}
}

// 関門のコマンドは docker を使わずに、機械検証をしてステータスを書く。
func TestRun_HumanCommands(t *testing.T) {
	root := project(t, actorTable)
	path := writeTicket(t, root, "T-001", "draft")
	code, out, errOut := runCmd("ready", "T-001", "--project", root)
	if code != exitOK || !strings.Contains(out, "ready-to-prepare") {
		t.Fatalf("ready: code=%d out=%q err=%q", code, out, errOut)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "ステータス: ready-to-prepare") {
		t.Errorf("指示書 = %s", data)
	}
	// 状態に合わないコマンドは拒む
	if code, _, errOut := runCmd("approve", "T-001", "--project", root); code != exitFailed || !strings.Contains(errOut, "approve") {
		t.Errorf("approve: code=%d err=%q", code, errOut)
	}
	if code, _, _ := runCmd("hold", "T-001", "--project", root); code != exitOK {
		t.Errorf("hold: code=%d", code)
	}
	code, out, _ = runCmd("status", "T-001", "--project", root)
	if code != exitOK || !strings.Contains(out, "T-001") || !strings.Contains(out, "保留") {
		t.Errorf("status: code=%d out=%q", code, out)
	}
	if code, _, _ := runCmd("cancel", "T-001", "--project", root); code != exitOK {
		t.Errorf("cancel: code=%d", code)
	}
}

func TestRun_StatusAll(t *testing.T) {
	root := project(t, actorTable)
	writeTicket(t, root, "T-001", "draft")
	writeTicket(t, root, "T-002", "ready-to-approve")
	code, out, _ := runCmd("status", "--project", root)
	if code != exitOK || !strings.Contains(out, "T-001") || !strings.Contains(out, "T-002") || !strings.Contains(out, "approve T-002") {
		t.Errorf("status: code=%d out=%q", code, out)
	}
}
