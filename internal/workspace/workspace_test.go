package workspace_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/workspace"
)

// gitEnv は、ホストの git の設定に頼らずに git を動かす環境変数。
var gitEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
	"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo は、main に1コミットあるリポジトリを作る。
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// fakeRunner は、コンテナの起動の仕様を記録し、決めた出力と終了コードを返す。
type fakeRunner struct {
	specs   []container.Spec
	stdin   [][]byte
	replies []reply
}

type reply struct {
	stdout []byte
	code   int
}

func (f *fakeRunner) Run(_ context.Context, spec container.Spec, stdout, _ io.Writer) (int, error) {
	f.specs = append(f.specs, spec)
	var in []byte
	if spec.Stdin != nil {
		in, _ = io.ReadAll(spec.Stdin)
	}
	f.stdin = append(f.stdin, in)
	if len(f.replies) == 0 {
		return 0, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	stdout.Write(r.stdout)
	return r.code, nil
}

func newWorkspace(t *testing.T, project string, r *fakeRunner) *workspace.Workspace {
	t.Helper()
	ws := workspace.New(project, "sample", "gateflow-agent-sample", r)
	ws.GitEnv = gitEnv
	return ws
}

func mountFor(spec container.Spec, target string) (container.Mount, bool) {
	for _, m := range spec.Mounts {
		if m.Target == target {
			return m, true
		}
	}
	return container.Mount{}, false
}

func TestPrepare_NewClone(t *testing.T) {
	project := newRepo(t)
	main := git(t, project, "rev-parse", "main")
	r := &fakeRunner{replies: []reply{{stdout: []byte("created " + main + " " + main + " clean " + main + " current 0 -\n")}, {}}}
	ws := newWorkspace(t, project, r)

	deps := &config.Deps{Command: "go mod download", CacheEnv: "GOMODCACHE"}
	if _, err := ws.Prepare(context.Background(), "T-001", "feature/t-001-x", "main", deps, workspace.PrepareOptions{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(r.specs) != 2 {
		t.Fatalf("コンテナの起動 = %d 回, want 2（clone と依存の取得）", len(r.specs))
	}

	clone := r.specs[0]
	if clone.Network != "none" || clone.User != workspace.User {
		t.Errorf("clone の network, user = %q, %q", clone.Network, clone.User)
	}
	if m, ok := mountFor(clone, "/vol"); !ok || m.Source != "gateflow-work-sample" || m.ReadOnly {
		t.Errorf("clone のマウント = %+v", clone.Mounts)
	}
	if clone.Env["TICKET"] != "T-001" || clone.Env["BRANCH"] != "feature/t-001-x" || clone.Env["MAIN"] != "main" {
		t.Errorf("clone の env = %v", clone.Env)
	}
	script := strings.Join(clone.Cmd, " ")
	for _, w := range []string{"git clone", "remote remove origin", `-c "$BRANCH"`} {
		if !strings.Contains(script, w) {
			t.Errorf("clone のスクリプトに %q がない:\n%s", w, script)
		}
	}
	if len(r.stdin[0]) == 0 || !bytes.HasPrefix(r.stdin[0], []byte("# v2 git bundle")) && !bytes.HasPrefix(r.stdin[0], []byte("# v3 git bundle")) {
		t.Errorf("標準入力に main の bundle がない: %q", r.stdin[0][:min(20, len(r.stdin[0]))])
	}

	dep := r.specs[1]
	if dep.Network != "" {
		t.Errorf("依存の取得は通常のネットワークで行う: network = %q", dep.Network)
	}
	if m, ok := mountFor(dep, "/main"); !ok || m.Subpath != "T-001/main" || m.ReadOnly {
		t.Errorf("依存の取得の /main = %+v", m)
	}
	if m, ok := mountFor(dep, "/cache"); !ok || m.Subpath != "T-001/cache" {
		t.Errorf("依存の取得の /cache = %+v", m)
	}
	if _, ok := mountFor(dep, "/work"); ok {
		t.Error("依存の取得に、エージェントが書き換える clone をマウントしている")
	}
	if dep.Env["GOMODCACHE"] != "/cache" || dep.Workdir != "/main" || !slices.Contains(dep.Cmd, "go mod download") {
		t.Errorf("依存の取得 = env %v, workdir %q, cmd %q", dep.Env, dep.Workdir, dep.Cmd)
	}
}

func TestPrepare_NoDeps(t *testing.T) {
	project := newRepo(t)
	main := git(t, project, "rev-parse", "main")
	r := &fakeRunner{replies: []reply{{stdout: []byte("created " + main + " " + main + " clean " + main + " current 0 -\n")}}}
	if _, err := newWorkspace(t, project, r).Prepare(context.Background(), "T-001", "feature/x", "main", nil, workspace.PrepareOptions{}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(r.specs) != 1 {
		t.Errorf("コンテナの起動 = %d 回, want 1", len(r.specs))
	}
}

func TestPrepare_NoMainBranch(t *testing.T) {
	project := newRepo(t)
	if _, err := newWorkspace(t, project, &fakeRunner{}).Prepare(context.Background(), "T-001", "feature/x", "develop", nil, workspace.PrepareOptions{}); err == nil {
		t.Fatal("main のブランチがないのに進んだ")
	}
}

// clone がすでにあれば使い回す。ただし取り込まれていない変更があれば止める。
// main の進みは、自分のコミットがなければ早送りし、あれば警告の印を返す（Q-23）。P2 の段階ならマージする（M2.6 の Q-01）。
func TestPrepare_ExistingClone(t *testing.T) {
	project := newRepo(t)
	main := git(t, project, "rev-parse", "main")
	line := func(h0, head, st, mode, synced string) string {
		return strings.Join([]string{"exists", h0, head, st, main, mode, synced, "-"}, " ") + "\n"
	}
	cases := []struct {
		name    string
		stdout  string
		want    workspace.Prepared
		wantErr string
	}{
		{"変更なし", line(main, main, "clean", "current", "0"), workspace.Prepared{Main: main, Head: main}, ""},
		{"早送りした", line(main, main, "clean", "forwarded", "0"), workspace.Prepared{Main: main, Head: main, Forwarded: true}, ""},
		{"main が進んだが自分のコミットがある", line(main, main, "clean", "behind", "0"), workspace.Prepared{Main: main, Head: main, Behind: true}, ""},
		{"main をマージした", line(main, "abc1234", "clean", "merged", "0"), workspace.Prepared{Main: main, Head: "abc1234", Merged: true}, ""},
		{"プロジェクトの作業ブランチへ早送りした", line(main, main, "clean", "current", "1"), workspace.Prepared{Main: main, Head: main, Synced: true}, ""},
		{"マージが衝突した", line(main, main, "clean", "conflict", "0"), workspace.Prepared{}, "衝突"},
		{"作業ツリーに変更がある", line(main, main, "dirty", "behind", "0"), workspace.Prepared{}, "取り込まれていない"},
		{"取り込まれていないコミットがある", line("0123456789abcdef0123456789abcdef01234567", main, "clean", "current", "0"), workspace.Prepared{}, "取り込まれていない"},
		{"出力の形が古い", "exists " + main + " clean " + main + " current\n", workspace.Prepared{}, "読めない"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{replies: []reply{{stdout: []byte(tc.stdout)}}}
			got, err := newWorkspace(t, project, r).Prepare(context.Background(), "T-001", "feature/x", "main", nil, workspace.PrepareOptions{})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// resume で、clone に取り込まれていないコミットがあるとき（M2.7）。止まった実行で gateflow が最後にコミットした
// と記録したものと先頭が一致し、作業ブランチの先頭からの並びがすべて gateflow の記録にあるときだけ続ける。
func TestPrepare_ResumeOwnCommit(t *testing.T) {
	project := newRepo(t)
	main := git(t, project, "rev-parse", "main")
	own1 := "1111111111111111111111111111111111111111"
	own2 := "2222222222222222222222222222222222222222"
	other := "3333333333333333333333333333333333333333"
	line := func(h0, pend string) string {
		return strings.Join([]string{"exists", h0, h0, "clean", main, "current", "0", pend}, " ") + "\n"
	}
	cases := []struct {
		name, stdout string
		own          []string
		wantErr      string
	}{
		{"記録と一致すれば続ける", line(own2, own1+","+own2), []string{own1, own2}, ""},
		{"記録がなければ止める（resume でない実行）", line(own2, own2), nil, "記録がない"},
		{"記録と違う先頭なら止める", line(other, other), []string{own1, own2}, "最後にコミットしたと記録したもの"},
		{"記録にないコミットが途中に挟まっていれば止める", line(own2, other+","+own2), []string{own1, own2}, "記録にないコミット 3333333"},
		{"作業ブランチの先頭から続いていなければ止める", line(own2, "x"), []string{own1, own2}, "続いていない"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{replies: []reply{{stdout: []byte(tc.stdout)}}}
			got, err := newWorkspace(t, project, r).Prepare(context.Background(), "T-001", "feature/x", "main", nil, workspace.PrepareOptions{Own: tc.own})
			if tc.wantErr == "" {
				if err != nil || got.Head != own2 {
					t.Fatalf("Prepare: %v, %+v", err, got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "取り込まれていない") {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	// 作業ブランチ（プロジェクト側）は変えない：Prepare はプロジェクトのリポジトリに書き込まない
	if got := git(t, project, "rev-parse", "main"); got != main {
		t.Errorf("プロジェクトの main が変わった: %s", got)
	}
}

// main をマージするときは、マージのコミットの名義が要る。
func TestPrepare_MergeMainNeedsAuthor(t *testing.T) {
	project := newRepo(t)
	_, err := newWorkspace(t, project, &fakeRunner{}).Prepare(context.Background(), "T-001", "feature/x", "main", nil, workspace.PrepareOptions{MergeMain: true})
	if err == nil || !strings.Contains(err.Error(), "名義") {
		t.Fatalf("err = %v", err)
	}
}

func TestAgentMounts(t *testing.T) {
	ws := newWorkspace(t, newRepo(t), &fakeRunner{})
	mounts := ws.AgentMounts("T-001")
	want := map[string]container.Mount{
		"/work":      {Type: container.Volume, Source: "gateflow-work-sample", Subpath: "T-001/repo", Target: "/work"},
		"/work/.git": {Type: container.Volume, Source: "gateflow-work-sample", Subpath: "T-001/repo/.git", Target: "/work/.git", ReadOnly: true},
		"/cache":     {Type: container.Volume, Source: "gateflow-work-sample", Subpath: "T-001/cache", Target: "/cache", ReadOnly: true},
	}
	if len(mounts) != len(want) {
		t.Fatalf("mounts = %+v", mounts)
	}
	for _, m := range mounts {
		if m != want[m.Target] {
			t.Errorf("mount %s = %+v, want %+v", m.Target, m, want[m.Target])
		}
	}
}

func TestCommit_Spec(t *testing.T) {
	r := &fakeRunner{replies: []reply{{stdout: []byte("committed 5555555555555555555555555555555555555555\n")}}}
	ws := newWorkspace(t, newRepo(t), r)
	if err := ws.Commit(context.Background(), "T-001", "feat(T-001): 記事を公開する", workspace.Author{Name: "人", Email: "h@example.invalid"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	spec := r.specs[0]
	if spec.Network != "none" {
		t.Errorf("network = %q", spec.Network)
	}
	if m, ok := mountFor(spec, "/work"); !ok || m.ReadOnly || m.Subpath != "T-001/repo" {
		t.Errorf("/work = %+v", m)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
		"GIT_AUTHOR_NAME": "人", "GIT_AUTHOR_EMAIL": "h@example.invalid",
		"GIT_COMMITTER_NAME": "人", "GIT_COMMITTER_EMAIL": "h@example.invalid",
		"MESSAGE": "feat(T-001): 記事を公開する",
	} {
		if spec.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, spec.Env[k], v)
		}
	}
	script := strings.Join(spec.Cmd, " ")
	for _, w := range []string{"core.hooksPath=/dev/null", "* -filter", "info/attributes", "info/exclude", `-m "$MESSAGE"`} {
		if !strings.Contains(script, w) {
			t.Errorf("コミットのスクリプトに %q がない:\n%s", w, script)
		}
	}
}

func TestCommit_RequiresTicketInMessage(t *testing.T) {
	ws := newWorkspace(t, newRepo(t), &fakeRunner{})
	if err := ws.Commit(context.Background(), "T-001", "feat: 記事を公開する", workspace.Author{Name: "人", Email: "h@example.invalid"}); err == nil {
		t.Fatal("T-NNN のないメッセージでコミットした")
	}
}

// branchBundle は、project の main から branch を作って1コミット足し、その bundle を返す（コンテナの出力の代わり）。
func branchBundle(t *testing.T, project, branch string) []byte {
	t.Helper()
	work := t.TempDir()
	git(t, work, "clone", "-q", project, ".")
	git(t, work, "switch", "-q", "-c", branch)
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "feat(T-001): x")
	out := filepath.Join(t.TempDir(), "b.bundle")
	git(t, work, "bundle", "create", out, branch)
	data, _ := os.ReadFile(out)
	return data
}

func TestImport(t *testing.T) {
	project := newRepo(t)
	head := git(t, project, "rev-parse", "HEAD")
	r := &fakeRunner{replies: []reply{{stdout: branchBundle(t, project, "feature/t-001-x")}}}
	ws := newWorkspace(t, project, r)

	if err := ws.Import(context.Background(), "T-001", "feature/t-001-x"); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := git(t, project, "log", "-1", "--format=%s", "feature/t-001-x"); got != "feat(T-001): x" {
		t.Errorf("取り込んだブランチの最後のコミット = %q", got)
	}
	if git(t, project, "rev-parse", "HEAD") != head || git(t, project, "branch", "--show-current") != "main" {
		t.Error("プロジェクトの HEAD か今のブランチが変わった")
	}
	if spec := r.specs[0]; spec.Network != "none" || !strings.Contains(strings.Join(spec.Cmd, " "), "bundle create") {
		t.Errorf("取り込みのコンテナ = %+v", spec)
	}
}

// 取り込み先のブランチを早送りできないときは止める。
func TestImport_NotFastForward(t *testing.T) {
	project := newRepo(t)
	bundle := branchBundle(t, project, "feature/t-001-x")
	git(t, project, "branch", "feature/t-001-x")
	git(t, project, "switch", "-q", "feature/t-001-x")
	os.WriteFile(filepath.Join(project, "b.txt"), []byte("human\n"), 0o644)
	git(t, project, "add", "-A")
	git(t, project, "commit", "-q", "-m", "人の変更")
	git(t, project, "switch", "-q", "main")

	ws := newWorkspace(t, project, &fakeRunner{replies: []reply{{stdout: bundle}}})
	if err := ws.Import(context.Background(), "T-001", "feature/t-001-x"); err == nil {
		t.Fatal("早送りできないのに取り込んだ")
	}
	if got := git(t, project, "log", "-1", "--format=%s", "feature/t-001-x"); got != "人の変更" {
		t.Errorf("人のブランチが変わった: %q", got)
	}
}

// コミットは、作ったコミットのハッシュを返す。変更がなければ今の HEAD を返す（コミットしない）。
func TestCommit_ReturnsHash(t *testing.T) {
	r := &fakeRunner{replies: []reply{{stdout: []byte("committed 1111111111111111111111111111111111111111\n")}, {stdout: []byte("unchanged 2222222222222222222222222222222222222222\n")}}}
	ws := newWorkspace(t, newRepo(t), r)
	a := workspace.Author{Name: "人", Email: "h@example.invalid"}
	hash, changed, err := ws.CommitHash(context.Background(), "T-001", "feat(T-001): x", a)
	if err != nil || hash != "1111111111111111111111111111111111111111" || !changed {
		t.Errorf("hash, changed, err = %q, %v, %v", hash, changed, err)
	}
	hash, changed, err = ws.CommitHash(context.Background(), "T-001", "feat(T-001): x", a)
	if err != nil || hash != "2222222222222222222222222222222222222222" || changed {
		t.Errorf("変更なし: hash, changed, err = %q, %v, %v", hash, changed, err)
	}
	if !strings.Contains(strings.Join(r.specs[0].Cmd, " "), "unchanged") {
		t.Error("変更がないときの扱いがスクリプトにない")
	}
}

// 書き出しは、clone の HEAD を tar で受け取り、ホストのディレクトリに展開して、その版のハッシュを返す。
func TestExport(t *testing.T) {
	project := newRepo(t)
	head := git(t, project, "rev-parse", "HEAD")
	archive := filepath.Join(t.TempDir(), "a.tar")
	git(t, project, "archive", "--format=tar", "-o", archive, "HEAD")
	tarData, _ := os.ReadFile(archive)
	r := &fakeRunner{replies: []reply{{stdout: append([]byte(head+"\n"), tarData...)}}}
	ws := newWorkspace(t, project, r)
	dest := filepath.Join(t.TempDir(), "export")

	hash, err := ws.Export(context.Background(), "T-001", dest)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if hash != head {
		t.Errorf("hash = %q, want %q", hash, head)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(data) != "x\n" {
		t.Errorf("展開した README.md = %q, %v", data, err)
	}
	if spec := r.specs[0]; spec.Network != "none" || !strings.Contains(strings.Join(spec.Cmd, " "), "git archive") {
		t.Errorf("書き出しのコンテナ = %+v", spec)
	}
	if m, ok := mountFor(r.specs[0], "/work"); !ok || !m.ReadOnly {
		t.Error("書き出しは clone を読み取り専用で使う")
	}
}

// 展開は、書き出し先の外へのパスを拒む。
func TestExport_RejectsEscapingPaths(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("3333333333333333333333333333333333333333\n")
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	ws := newWorkspace(t, newRepo(t), &fakeRunner{replies: []reply{{stdout: buf.Bytes()}}})
	if _, err := ws.Export(context.Background(), "T-001", filepath.Join(t.TempDir(), "export")); err == nil {
		t.Fatal("外へのパスを展開した")
	}
}

func TestDiffAndTreeHash(t *testing.T) {
	r := &fakeRunner{replies: []reply{{stdout: []byte("diff --git a/x b/x\n")}, {stdout: []byte("4444444444444444444444444444444444444444\n")}}}
	ws := newWorkspace(t, newRepo(t), r)
	diff, err := ws.Diff(context.Background(), "T-001", "main")
	if err != nil || !strings.HasPrefix(diff, "diff --git") {
		t.Errorf("Diff = %q, %v", diff, err)
	}
	if r.specs[0].Env["BASE"] != "main" {
		t.Errorf("Diff の BASE = %q", r.specs[0].Env["BASE"])
	}
	tree, err := ws.TreeHash(context.Background(), "T-001")
	if err != nil || tree != "4444444444444444444444444444444444444444" {
		t.Errorf("TreeHash = %q, %v", tree, err)
	}
}

// BJ には clone を .git を含めてすべて読み取り専用で渡す。
func TestReviewerMounts(t *testing.T) {
	ws := newWorkspace(t, newRepo(t), &fakeRunner{})
	for _, m := range ws.ReviewerMounts("T-001") {
		if !m.ReadOnly {
			t.Errorf("BJ のマウント %s が書き込み可", m.Target)
		}
	}
}

// catalogBundle は、project の main から作業ブランチを切り、file に line を足したコミットのバンドルを返す。
func catalogBundle(t *testing.T, project, branch, file, line string) []byte {
	t.Helper()
	work := t.TempDir()
	git(t, work, "clone", "-q", project, ".")
	git(t, work, "switch", "-q", "-c", branch)
	p := filepath.Join(work, file)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, []byte(line)...), 0o644)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "feat(T-002): x")
	out := filepath.Join(t.TempDir(), "b.bundle")
	git(t, work, "bundle", "create", out, branch)
	data, _ := os.ReadFile(out)
	return data
}

// MergedTree は、その時点の main と作業ブランチを合わせた木を取り出す。ホストの ref と作業ツリーは変えない（M2.9）。
func TestMergedTree(t *testing.T) {
	project := newRepo(t)
	os.WriteFile(filepath.Join(project, "var.md"), []byte("| ID | x |\n| --- | --- |\n| VAR-01 | a |\n"), 0o644)
	git(t, project, "add", "-A")
	git(t, project, "commit", "-q", "-m", "catalog")
	bundle := catalogBundle(t, project, "feature/t-002-x", "var.md", "| VAR-02 | b |\n")
	// 作業ブランチを切った後に、main が先に進む（ほかのチケットが VAR-02 を入れた）
	os.WriteFile(filepath.Join(project, "README.md"), []byte("y\n"), 0o644)
	b, _ := os.ReadFile(filepath.Join(project, "var.md"))
	os.WriteFile(filepath.Join(project, "var.md"), append([]byte("# 変数\n\n"), b...), 0o644)
	git(t, project, "commit", "-qam", "other ticket")
	refs := git(t, project, "for-each-ref")
	head := git(t, project, "rev-parse", "HEAD")

	dest := t.TempDir()
	ws := newWorkspace(t, project, &fakeRunner{replies: []reply{{stdout: bundle}}})
	conflict, err := ws.MergedTree(context.Background(), "T-002", "feature/t-002-x", "main", dest)
	if err != nil || conflict {
		t.Fatalf("MergedTree: %v, 衝突 %v", err, conflict)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "var.md"))
	if !strings.Contains(string(got), "# 変数") || !strings.Contains(string(got), "| VAR-02 | b |") {
		t.Errorf("合わせた木の var.md:\n%s", got)
	}
	if git(t, project, "for-each-ref") != refs || git(t, project, "rev-parse", "HEAD") != head {
		t.Error("ホストの ref か HEAD が変わった")
	}
	if _, err := os.Stat(filepath.Join(project, ".git", "FETCH_HEAD")); err == nil {
		t.Error("FETCH_HEAD を書いた")
	}

	// 同じ行を両方で変えると衝突する。衝突なら何も取り出さない
	git(t, project, "commit", "-q", "--allow-empty", "-m", "x")
	os.WriteFile(filepath.Join(project, "var.md"), []byte("| ID | x |\n| --- | --- |\n| VAR-01 | main |\n"), 0o644)
	git(t, project, "commit", "-qam", "main changes VAR-01")
	conflictBundle := func() []byte {
		work := t.TempDir()
		git(t, work, "clone", "-q", project, ".")
		git(t, work, "switch", "-q", "-c", "feature/t-003-x", "HEAD~2")
		os.WriteFile(filepath.Join(work, "var.md"), []byte("| ID | x |\n| --- | --- |\n| VAR-01 | branch |\n"), 0o644)
		git(t, work, "commit", "-qam", "branch changes VAR-01")
		out := filepath.Join(t.TempDir(), "c.bundle")
		git(t, work, "bundle", "create", out, "feature/t-003-x")
		data, _ := os.ReadFile(out)
		return data
	}()
	dest2 := t.TempDir()
	ws = newWorkspace(t, project, &fakeRunner{replies: []reply{{stdout: conflictBundle}}})
	if conflict, err := ws.MergedTree(context.Background(), "T-003", "feature/t-003-x", "main", dest2); err != nil || !conflict {
		t.Fatalf("衝突を返さない: %v, %v", conflict, err)
	}
}
