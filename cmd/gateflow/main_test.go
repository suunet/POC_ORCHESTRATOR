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
		".gateflow/project.json": `{"formatVersion": 0, "ids": {"scan": ["docs"], "prefixes": [
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
