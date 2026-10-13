// Package workspace は、タスクごとの clone の作成、依存の用意、コミット、作業ブランチの取り込みを扱う。
//
// clone は作業用のボリューム（gateflow-work-<プロジェクト名>）の <チケット>/ に置く。
//
//	<チケット>/repo   エージェントが作業する clone（リモートは持たない）
//	<チケット>/main   人が承認した main の写し。依存の取得はここに対してだけ行う
//	<チケット>/cache  依存のキャッシュ（エージェントには読み取り専用で渡す）
//
// コンテナの中の操作は gateflow のスクリプトで行い、値（ブランチ名、メッセージ）は環境変数で渡す。
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gateflow/internal/config"
	"gateflow/internal/container"
)

// User は、エージェントのイメージの root でない利用者の名前。
const User = "agent"

// Runner は、コンテナを起動する。
type Runner interface {
	Run(ctx context.Context, spec container.Spec, stdout, stderr io.Writer) (int, error)
}

// Workspace は、1つのプロジェクトの作業用ボリュームを扱う。
type Workspace struct {
	project string // プロジェクトのルート（ホスト）
	name    string // プロジェクト名
	image   string // エージェントのイメージ
	runner  Runner

	// GitEnv は、ホストで git を動かすときに足す環境変数（テストでホストの設定に頼らないために使う）。
	GitEnv []string
}

// New は、project（ホストのルート）の Workspace を返す。
func New(project, name, image string, runner Runner) *Workspace {
	return &Workspace{project: project, name: name, image: image, runner: runner}
}

// Volume は、作業用のボリュームの名前。
func (w *Workspace) Volume() string { return "gateflow-work-" + w.name }

// gitEnv は、gateflow のコンテナの中の git がシステムとグローバルの設定を読まないようにする環境変数。
var gitEnv = map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "HOME": "/tmp"}

func withGitEnv(extra map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range gitEnv {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// NOTE: 新しい名前付きボリュームを初めてマウントすると、Docker はイメージのマウント先（/vol）の
// 持ち主を写す。エージェントのイメージは /vol を agent の持ち主で作っておくこと（特権を落としたコンテナは chown できない）
const cloneScript = `set -eu
d="/vol/$TICKET"
mkdir -p "$d/cache"
cat > /tmp/main.bundle
rm -rf "$d/main"
git clone -q --branch "$MAIN" /tmp/main.bundle "$d/main"
g() { git -c core.hooksPath=/dev/null "$@"; }
created=0
if [ ! -d "$d/repo/.git" ]; then
  git clone -q --branch "$MAIN" /tmp/main.bundle "$d/repo"
  cd "$d/repo"
  git remote remove origin
  printf '.env\n' >> .git/info/exclude
  if [ "$HOSTBRANCH" = 1 ]; then
    git fetch -q /tmp/main.bundle "refs/heads/$BRANCH:refs/heads/$BRANCH"
    git switch -q "$BRANCH"
  else
    git switch -q -c "$BRANCH"
  fi
  created=1
fi
cd "$d/repo"
st=clean
[ -z "$(git status --porcelain)" ] || st=dirty
h0=$(git rev-parse HEAD)
git fetch -q /tmp/main.bundle "+refs/heads/$MAIN:refs/gateflow/main"
synced=0
if [ "$HOSTBRANCH" = 1 ] && [ "$created" = 0 ]; then
  git fetch -q /tmp/main.bundle "+refs/heads/$BRANCH:refs/gateflow/branch"
  b=$(git rev-parse refs/gateflow/branch)
  if [ "$st" = clean ] && [ "$h0" != "$b" ] && git merge-base --is-ancestor "$h0" "$b"; then
    g merge -q --ff-only "$b"
    synced=1
  fi
fi
m=$(git rev-parse refs/gateflow/main)
h=$(git rev-parse HEAD)
if git merge-base --is-ancestor "$m" "$h"; then
  mode=current
elif [ "$st" = clean ] && git merge-base --is-ancestor "$h" "$m"; then
  g merge -q --ff-only "$m"
  mode=forwarded
elif [ "$st" = clean ] && [ "$MERGEMAIN" = 1 ]; then
  if g merge -q --no-ff --no-edit -m "merge: $MAIN を $BRANCH へマージする（gateflow）" "$m" >/dev/null 2>&1; then
    mode=merged
  else
    g merge --abort >/dev/null 2>&1 || true
    mode=conflict
  fi
else
  mode=behind
fi
# 取り込まれていないコミットの並び（プロジェクトの作業ブランチの先頭、なければ main から h0 まで）。続いていなければ x
base=$m
[ "$HOSTBRANCH" = 1 ] && [ "$created" = 0 ] && base=$b
if git merge-base --is-ancestor "$base" "$h0"; then
  pend=$(git rev-list --reverse "$base..$h0" | tr '\n' ',')
  pend=${pend%,}
  [ -n "$pend" ] || pend=-
else
  pend=x
fi
kind=exists
[ "$created" = 1 ] && kind=created
echo "$kind $h0 $(git rev-parse HEAD) $st $m $mode $synced $pend"
`

// Prepared は、clone の用意の結果。Main は使った main のハッシュ、Head は用意した後の作業ブランチの先頭。
// Forwarded は、作業ブランチを新しい main へ早送りしたこと。Behind は、作業ブランチに自分のコミットがあり、
// main の進みを取り込んでいないこと（Q-23：触らずに警告する）。Merged は、main を作業ブランチへマージしたこと
// （M2.6 の Q-01。新しいコミットができるので、呼び出し側がプロジェクトへ取り込む）。Synced は、プロジェクトの
// 作業ブランチ（人の決定のコミット）へ早送りしたこと。
type Prepared struct {
	Main      string
	Head      string
	Forwarded bool
	Behind    bool
	Merged    bool
	Synced    bool
}

// PrepareOptions は、clone の用意の選び方。MergeMain は、作業ブランチに自分のコミットがあっても、main の進みを
// マージする（P2 の段階。M2.6 の Q-01）。Author は、マージのコミットの名義。
// Own は、resume のとき、止まった実行で gateflow がコミットした順（最後が先頭のはず）。取り込まれていない
// コミットが、これと一致するときだけ止めずに続ける（M2.7）。取り込みは今までどおり呼び出し側が承認の後に行う。
type PrepareOptions struct {
	MergeMain bool
	Author    Author
	Own       []string
}

// Prepare は、チケットの clone を用意する。clone がなければ main から作り、あれば使い回す。
// 使い回すときに取り込まれていない変更があれば止める。作業ブランチに自分のコミットがなく、作業ツリーがきれいなら、
// 新しい main へ早送りする（Q-23）。deps があれば、main の写しに対して依存を取得する。
func (w *Workspace) Prepare(ctx context.Context, ticketID, branch, mainBranch string, deps *config.Deps, opt PrepareOptions) (Prepared, error) {
	bundle, hostBranch, err := w.bundleMain(ctx, mainBranch, branch)
	if err != nil {
		return Prepared{}, err
	}
	if opt.MergeMain && (opt.Author.Name == "" || opt.Author.Email == "") {
		return Prepared{}, errors.New("マージのコミットの名義（人の git の設定の user.name と user.email）がない")
	}
	env := map[string]string{"TICKET": ticketID, "BRANCH": branch, "MAIN": mainBranch, "HOSTBRANCH": "0", "MERGEMAIN": "0",
		"GIT_AUTHOR_NAME": opt.Author.Name, "GIT_AUTHOR_EMAIL": opt.Author.Email,
		"GIT_COMMITTER_NAME": opt.Author.Name, "GIT_COMMITTER_EMAIL": opt.Author.Email}
	if hostBranch {
		env["HOSTBRANCH"] = "1"
	}
	if opt.MergeMain {
		env["MERGEMAIN"] = "1"
	}
	var out bytes.Buffer
	spec := w.spec(ticketID, "clone", []container.Mount{{Type: container.Volume, Source: w.Volume(), Target: "/vol"}}, env, cloneScript)
	spec.Stdin = bytes.NewReader(bundle)
	if err := w.run(ctx, spec, &out); err != nil {
		return Prepared{}, fmt.Errorf("clone を用意できない: %w", err)
	}
	p, err := w.checkClone(ctx, ticketID, strings.TrimSpace(out.String()), opt.Own)
	if err != nil {
		return Prepared{}, err
	}
	if deps == nil {
		return p, nil
	}
	dep := container.Spec{
		Name:    w.containerName(ticketID, "deps"),
		Image:   w.image,
		User:    User,
		Workdir: "/main",
		Env:     withGitEnv(map[string]string{deps.CacheEnv: "/cache"}),
		Mounts: []container.Mount{
			{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/main", Target: "/main"},
			{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/cache", Target: "/cache"},
		},
		// NOTE: 人が管理する project.json の値を、人が承認した main の写しに対してだけ実行する
		Cmd:     []string{"sh", "-c", deps.Command},
		Timeout: 10 * time.Minute,
	}
	if err := w.run(ctx, dep, io.Discard); err != nil {
		return Prepared{}, fmt.Errorf("依存を取得できない: %w", err)
	}
	return p, nil
}

func (w *Workspace) checkClone(ctx context.Context, ticketID, line string, own []string) (Prepared, error) {
	f := strings.Fields(line)
	if len(f) != 8 || (f[0] != "created" && f[0] != "exists") {
		return Prepared{}, fmt.Errorf("clone の用意の出力を読めない: %q", line)
	}
	h0, head, state, main, mode, synced, pend := f[1], f[2], f[3], f[4], f[5], f[6], f[7]
	if state != "clean" {
		return Prepared{}, fmt.Errorf("%s の clone に、取り込まれていない変更（コミットしていない変更）がある。確かめてから clone を消すか取り込む", ticketID)
	}
	if _, err := w.git(ctx, "cat-file", "-e", h0+"^{commit}"); err != nil {
		if err := OwnPending(h0, pend, own); err != nil {
			return Prepared{}, fmt.Errorf("%s の clone に、プロジェクトに取り込まれていないコミット %s がある（%v）。確かめてから clone を消すか取り込む", ticketID, h0, err)
		}
	}
	switch mode {
	case "current", "forwarded", "behind", "merged":
	case "conflict":
		return Prepared{}, fmt.Errorf("%s の作業ブランチへ main（%.7s）をマージできない（衝突）。人が作業ブランチで解消してから run する", ticketID, main)
	default:
		return Prepared{}, fmt.Errorf("clone の用意の出力を読めない: %q", line)
	}
	return Prepared{Main: main, Head: head, Forwarded: mode == "forwarded", Behind: mode == "behind", Merged: mode == "merged", Synced: synced == "1"}, nil
}

// OwnPending は、clone の取り込まれていないコミットを、gateflow 自身のコミットとして通してよいかを確かめる（M2.7）。
// 通すのは、clone の先頭 h0 が、止まった実行で gateflow が最後にコミットしたと記録したもの（own の最後）と一致し、
// 作業ブランチの先頭から h0 までのコミット（pend。カンマ区切り、続いていなければ x）がすべて own にあるときだけ。
func OwnPending(h0, pend string, own []string) error {
	switch {
	case len(own) == 0:
		return errors.New("gateflow がコミットした記録がない")
	case h0 != own[len(own)-1]:
		return fmt.Errorf("先頭が、gateflow が最後にコミットしたと記録したもの（%.7s）と違う", own[len(own)-1])
	case pend == "x" || pend == "-":
		return errors.New("作業ブランチの先頭から続いていない")
	}
	for _, c := range strings.Split(pend, ",") {
		if !slices.Contains(own, c) {
			return fmt.Errorf("途中に、gateflow の記録にないコミット %.7s が挟まっている", c)
		}
	}
	return nil
}

// AgentMounts は、エージェントのコンテナのマウントを返す。.git と依存のキャッシュは読み取り専用。
func (w *Workspace) AgentMounts(ticketID string) []container.Mount {
	return []container.Mount{
		{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work"},
		{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo/.git", Target: "/work/.git", ReadOnly: true},
		{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/cache", Target: "/cache", ReadOnly: true},
	}
}

// Author は、コミットの名義（人の git の設定）。
type Author struct {
	Name  string
	Email string
}

// NOTE: エージェントが作業ツリーに置いた .gitattributes の filter や、フックがここで動かないようにする。
// .git/info/attributes は作業ツリーの .gitattributes より優先される
const commitScript = `set -eu
cd /work
mkdir -p .git/info
printf '* -filter\n' > .git/info/attributes
grep -qx '.env' .git/info/exclude 2>/dev/null || printf '.env\n' >> .git/info/exclude
g() { git -c core.hooksPath=/dev/null -c core.fsmonitor=false "$@"; }
g add -A
if g diff --cached --quiet; then echo "unchanged $(git rev-parse HEAD)"; exit 0; fi
g commit -q -m "$MESSAGE"
echo "committed $(git rev-parse HEAD)"
`

var ticketInMessage = regexp.MustCompile(`T-[0-9]{3,}`)

// Commit は、clone の変更をすべて1つのコミットにする。message にはチケットの ID を含める。
func (w *Workspace) Commit(ctx context.Context, ticketID, message string, author Author) error {
	_, _, err := w.CommitHash(ctx, ticketID, message, author)
	return err
}

// CommitHash は、clone の変更をすべて1つのコミットにして、そのハッシュを返す。
// 変更がなければコミットせず、今の HEAD のハッシュと changed=false を返す。
func (w *Workspace) CommitHash(ctx context.Context, ticketID, message string, author Author) (hash string, changed bool, err error) {
	if !strings.Contains(message, ticketID) || !ticketInMessage.MatchString(message) {
		return "", false, fmt.Errorf("コミットメッセージ %q に %s がない", message, ticketID)
	}
	if author.Name == "" || author.Email == "" {
		return "", false, errors.New("コミットの名義（人の git の設定の user.name と user.email）がない")
	}
	spec := w.spec(ticketID, "commit",
		[]container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work"}},
		map[string]string{
			"MESSAGE":         message,
			"GIT_AUTHOR_NAME": author.Name, "GIT_AUTHOR_EMAIL": author.Email,
			"GIT_COMMITTER_NAME": author.Name, "GIT_COMMITTER_EMAIL": author.Email,
		}, commitScript)
	var out bytes.Buffer
	if err := w.run(ctx, spec, &out); err != nil {
		return "", false, fmt.Errorf("コミットできない: %w", err)
	}
	f := strings.Fields(out.String())
	if len(f) != 2 || (f[0] != "committed" && f[0] != "unchanged") {
		return "", false, fmt.Errorf("コミットの出力を読めない: %q", out.String())
	}
	return f[1], f[0] == "committed", nil
}

const importScript = `set -eu
git -C /work bundle create - "refs/heads/$BRANCH"
`

// Import は、clone の作業ブランチをプロジェクトの同じ名前のブランチに取り込む。
// プロジェクトの作業ツリーと HEAD は変えない。早送りできないとき、そのブランチを人が開いているときは止める。
func (w *Workspace) Import(ctx context.Context, ticketID, branch string) error {
	var out bytes.Buffer
	spec := w.spec(ticketID, "import",
		[]container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work", ReadOnly: true}},
		map[string]string{"BRANCH": branch}, importScript)
	if err := w.run(ctx, spec, &out); err != nil {
		return fmt.Errorf("作業ブランチを取り出せない: %w", err)
	}
	f, err := os.CreateTemp("", "gateflow-*.bundle")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(out.Bytes()); err != nil {
		f.Close()
		return err
	}
	f.Close()
	ref := "refs/heads/" + branch
	if _, err := w.git(ctx, "-c", "core.hooksPath=/dev/null", "fetch", "--quiet", f.Name(), ref+":"+ref); err != nil {
		return fmt.Errorf("作業ブランチ %s を取り込めない（早送りできない、またはそのブランチを開いている）: %w", branch, err)
	}
	return nil
}

// bundleMain は、main と、プロジェクトに作業ブランチがあればそれも束ねる（人の決定のコミットを clone へ渡す。M2.6 の Q-01）。
func (w *Workspace) bundleMain(ctx context.Context, mainBranch, branch string) ([]byte, bool, error) {
	if _, err := w.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+mainBranch); err != nil {
		return nil, false, fmt.Errorf("プロジェクトに %s のブランチがない", mainBranch)
	}
	refs := []string{"refs/heads/" + mainBranch}
	_, err := w.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	hostBranch := err == nil && branch != mainBranch
	if hostBranch {
		refs = append(refs, "refs/heads/"+branch)
	}
	dir, err := os.MkdirTemp("", "gateflow-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "main.bundle")
	if _, err := w.git(ctx, append([]string{"bundle", "create", "--quiet", path}, refs...)...); err != nil {
		return nil, false, fmt.Errorf("%s を束ねられない: %w", mainBranch, err)
	}
	data, err := os.ReadFile(path)
	return data, hostBranch, err
}

func (w *Workspace) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.project}, args...)...)
	cmd.Env = append(os.Environ(), w.GitEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return out, nil
}

// spec は、gateflow の操作（外への経路なし）のコンテナの仕様を返す。
func (w *Workspace) spec(ticketID, op string, mounts []container.Mount, env map[string]string, script string) container.Spec {
	return container.Spec{
		Name:    w.containerName(ticketID, op),
		Image:   w.image,
		Network: "none",
		User:    User,
		Env:     withGitEnv(env),
		Mounts:  mounts,
		Cmd:     []string{"sh", "-c", script},
		Timeout: 5 * time.Minute,
	}
}

func (w *Workspace) containerName(ticketID, op string) string {
	return fmt.Sprintf("gateflow-%s-%s-%s", w.name, strings.ToLower(ticketID), op)
}

func (w *Workspace) run(ctx context.Context, spec container.Spec, stdout io.Writer) error {
	var stderr bytes.Buffer
	code, err := w.runner.Run(ctx, spec, stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("終了コード %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
