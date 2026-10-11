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
if [ -d "$d/repo/.git" ]; then
  cd "$d/repo"
  st=clean
  [ -z "$(git status --porcelain)" ] || st=dirty
  git fetch -q /tmp/main.bundle "+refs/heads/$MAIN:refs/gateflow/main"
  m=$(git rev-parse refs/gateflow/main)
  h=$(git rev-parse HEAD)
  if git merge-base --is-ancestor "$m" "$h"; then
    mode=current
  elif [ "$st" = clean ] && git merge-base --is-ancestor "$h" "$m"; then
    git -c core.hooksPath=/dev/null merge -q --ff-only "$m"
    mode=forwarded
  else
    mode=behind
  fi
  echo "exists $(git rev-parse HEAD) $st $m $mode"
else
  git clone -q --branch "$MAIN" /tmp/main.bundle "$d/repo"
  cd "$d/repo"
  git remote remove origin
  printf '.env\n' >> .git/info/exclude
  git switch -q -c "$BRANCH"
  echo "created $(git rev-parse HEAD)"
fi
`

// Prepared は、clone の用意の結果。Main は使った main のハッシュ、Head は用意した後の作業ブランチの先頭。
// Forwarded は、作業ブランチを新しい main へ早送りしたこと。Behind は、作業ブランチに自分のコミットがあり、
// main の進みを取り込んでいないこと（Q-23：触らずに警告する）。
type Prepared struct {
	Main      string
	Head      string
	Forwarded bool
	Behind    bool
}

// Prepare は、チケットの clone を用意する。clone がなければ main から作り、あれば使い回す。
// 使い回すときに取り込まれていない変更があれば止める。作業ブランチに自分のコミットがなく、作業ツリーがきれいなら、
// 新しい main へ早送りする（Q-23）。deps があれば、main の写しに対して依存を取得する。
func (w *Workspace) Prepare(ctx context.Context, ticketID, branch, mainBranch string, deps *config.Deps) (Prepared, error) {
	bundle, err := w.bundleMain(ctx, mainBranch)
	if err != nil {
		return Prepared{}, err
	}
	var out bytes.Buffer
	spec := w.spec(ticketID, "clone", []container.Mount{{Type: container.Volume, Source: w.Volume(), Target: "/vol"}},
		map[string]string{"TICKET": ticketID, "BRANCH": branch, "MAIN": mainBranch}, cloneScript)
	spec.Stdin = bytes.NewReader(bundle)
	if err := w.run(ctx, spec, &out); err != nil {
		return Prepared{}, fmt.Errorf("clone を用意できない: %w", err)
	}
	p, err := w.checkClone(ctx, ticketID, strings.TrimSpace(out.String()))
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

func (w *Workspace) checkClone(ctx context.Context, ticketID, line string) (Prepared, error) {
	f := strings.Fields(line)
	switch {
	case len(f) == 2 && f[0] == "created":
		return Prepared{Main: f[1], Head: f[1]}, nil
	case len(f) == 5 && f[0] == "exists":
		if f[2] != "clean" {
			return Prepared{}, fmt.Errorf("%s の clone に、取り込まれていない変更（コミットしていない変更）がある。確かめてから clone を消すか取り込む", ticketID)
		}
		if _, err := w.git(ctx, "cat-file", "-e", f[1]+"^{commit}"); err != nil {
			return Prepared{}, fmt.Errorf("%s の clone に、プロジェクトに取り込まれていないコミット %s がある。確かめてから clone を消すか取り込む", ticketID, f[1])
		}
		switch f[4] {
		case "current", "forwarded", "behind":
		default:
			return Prepared{}, fmt.Errorf("clone の用意の出力を読めない: %q", line)
		}
		return Prepared{Main: f[3], Head: f[1], Forwarded: f[4] == "forwarded", Behind: f[4] == "behind"}, nil
	default:
		return Prepared{}, fmt.Errorf("clone の用意の出力を読めない: %q", line)
	}
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

func (w *Workspace) bundleMain(ctx context.Context, mainBranch string) ([]byte, error) {
	if _, err := w.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+mainBranch); err != nil {
		return nil, fmt.Errorf("プロジェクトに %s のブランチがない", mainBranch)
	}
	dir, err := os.MkdirTemp("", "gateflow-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "main.bundle")
	if _, err := w.git(ctx, "bundle", "create", "--quiet", path, "refs/heads/"+mainBranch); err != nil {
		return nil, fmt.Errorf("%s を束ねられない: %w", mainBranch, err)
	}
	return os.ReadFile(path)
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
