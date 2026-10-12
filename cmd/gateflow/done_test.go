package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.invalid",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// done は、作業ブランチの先頭が main に含まれているときだけ done にする。main には書き込まない。
func TestRun_Done(t *testing.T) {
	root := project(t, actorTable)
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, root, "branch", "feature/x")
	path := writeTicket(t, root, "T-001", "ready-for-review")

	// 未マージ：作業ブランチに main にないコミットがある
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "on main") // main を進めても、作業ブランチは祖先のまま
	gitIn(t, root, "switch", "-q", "feature/x")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "work")
	gitIn(t, root, "switch", "-q", "main")
	mainBefore, _ := exec.Command("git", "-C", root, "rev-parse", "main").Output()
	code, _, errOut := runCmd("done", "T-001", "--project", root)
	if code != exitFailed || !strings.Contains(errOut, "main に含まれていない") || !strings.Contains(errOut, "feature/x") {
		t.Fatalf("未マージ: code=%d err=%q", code, errOut)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "ステータス: ready-for-review") {
		t.Errorf("未マージなのにステータスが変わった:\n%s", data)
	}

	// マージ済み（人が git でマージする）
	gitIn(t, root, "merge", "-q", "--no-edit", "feature/x")
	mainAfter, _ := exec.Command("git", "-C", root, "rev-parse", "main").Output()
	code, out, errOut := runCmd("done", "T-001", "--project", root)
	if code != exitOK || !strings.Contains(out, "done") {
		t.Fatalf("マージ済み: code=%d out=%q err=%q", code, out, errOut)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "ステータス: done") {
		t.Errorf("ステータス:\n%s", data)
	}
	if after, _ := exec.Command("git", "-C", root, "rev-parse", "main").Output(); string(after) != string(mainAfter) || string(mainBefore) == string(mainAfter) {
		t.Errorf("gateflow が main を書き換えた")
	}

	// ready-for-review 以外からは失敗する
	writeTicket(t, root, "T-002", "draft")
	if code, _, errOut := runCmd("done", "T-002", "--project", root); code != exitFailed || !strings.Contains(errOut, "done できない") {
		t.Errorf("draft から: code=%d err=%q", code, errOut)
	}
}

// 作業ブランチがなければ、理由を示して失敗する。
func TestRun_DoneMissingBranch(t *testing.T) {
	root := project(t, actorTable)
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "one")
	writeTicket(t, root, "T-001", "ready-for-review")
	if code, _, errOut := runCmd("done", "T-001", "--project", root); code != exitFailed || !strings.Contains(errOut, "作業ブランチ feature/x がない") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// check は、遷移表にない状態のチケットを必須の指摘として報告し、不合格で終わる。check ids は ID の検査だけ。
func TestRun_CheckTicketStates(t *testing.T) {
	root := project(t, actorTable)
	writeTicket(t, root, "T-001", "ready-for-review")
	if code, out, errOut := runCmd("check", "--project", root); code != exitOK || !strings.Contains(out, "チケットの状態の検査: チケット 1件、必須 0件") {
		t.Fatalf("表にある状態: code=%d out=%q err=%q", code, out, errOut)
	}
	writeTicket(t, root, "T-002", "in-uat")
	code, out, _ := runCmd("check", "--project", root)
	if code != exitFailed || !strings.Contains(out, "[ticket-state] .docs/_stash/tickets/T-002/ticket.md:3 T-002 の状態 in-uat は遷移表 aidd-impl にない") ||
		!strings.Contains(out, "遷移表に移行の行を足すか、人がステータスを直す") || strings.Contains(out, "T-001 の状態") {
		t.Errorf("表にない状態: code=%d out=%q", code, out)
	}
	if code, out, _ := runCmd("check", "ids", "--project", root); code != exitOK || strings.Contains(out, "ticket-state") {
		t.Errorf("check ids: code=%d out=%q", code, out)
	}
}
