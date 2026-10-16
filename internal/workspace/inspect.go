package workspace

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gateflow/internal/container"
)

// ReviewerMounts は、BJ のコンテナのマウントを返す。clone は .git を含めてすべて読み取り専用。
func (w *Workspace) ReviewerMounts(ticketID string) []container.Mount {
	return []container.Mount{
		{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work", ReadOnly: true},
		{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/cache", Target: "/cache", ReadOnly: true},
	}
}

func (w *Workspace) readOnlyRepo(ticketID string) []container.Mount {
	return []container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work", ReadOnly: true}}
}

const exportScript = `set -eu
cd /work
git rev-parse HEAD
git archive --format=tar HEAD
`

// NOTE: Git 管理のファイルと、無視されていない新しいファイルだけを書き出す（.env など無視したものは含めない）
const snapshotScript = `set -eu
cd /work
git ls-files -co --exclude-standard -z > /tmp/files
tr '\0' '\n' < /tmp/files | while IFS= read -r f; do [ -f "$f" ] && [ ! -L "$f" ] && printf '%s\0' "$f"; done | tar --null -T - -cf -
`

// Snapshot は、clone の作業ツリー（コミットしていない変更を含む。無視したファイルは含めない）をホストの dest に展開する。
// ARB が作業ツリーで直したカタログの変更を、コミットの前に確かめるのに使う（M2.6）。
func (w *Workspace) Snapshot(ctx context.Context, ticketID, dest string) error {
	var out bytes.Buffer
	if err := w.run(ctx, w.spec(ticketID, "snapshot", w.readOnlyRepo(ticketID), nil, snapshotScript), &out); err != nil {
		return fmt.Errorf("作業ツリーを書き出せない: %w", err)
	}
	return untar(&out, dest)
}

const resetScript = `set -eu
cd /work
git -c core.hooksPath=/dev/null reset -q --hard HEAD
git clean -q -fd
`

// Reset は、clone の作業ツリーを HEAD に戻す（コミットしていない変更と、無視されていない新しいファイルを消す）。
// ARB をやり直す前に、前の試みの書きかけを消すのに使う（M2.6）。
func (w *Workspace) Reset(ctx context.Context, ticketID string) error {
	mounts := []container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work"}}
	if err := w.run(ctx, w.spec(ticketID, "reset", mounts, nil, resetScript), io.Discard); err != nil {
		return fmt.Errorf("作業ツリーを戻せない: %w", err)
	}
	return nil
}

// Tracked は、path がプロジェクトの main で Git 管理されているかを返す（規約を clone の版から渡すかの判定。Q-23）。
func (w *Workspace) Tracked(ctx context.Context, mainBranch, path string) bool {
	_, err := w.git(ctx, "cat-file", "-e", mainBranch+":"+path)
	return err == nil
}

// Export は、clone の HEAD（コミット済みの版）をホストの dest に展開して、その版のハッシュを返す。
// clone の中身はエージェントが書いたものなので、dest の外を指すパスとリンクは展開しない。
func (w *Workspace) Export(ctx context.Context, ticketID, dest string) (string, error) {
	var out bytes.Buffer
	if err := w.run(ctx, w.spec(ticketID, "export", w.readOnlyRepo(ticketID), nil, exportScript), &out); err != nil {
		return "", fmt.Errorf("clone を書き出せない: %w", err)
	}
	br := bufio.NewReader(&out)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("書き出しの出力を読めない: %w", err)
	}
	hash := strings.TrimSpace(line)
	if err := untar(br, dest); err != nil {
		return "", err
	}
	return hash, nil
}

func untar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("書き出しの tar を読めない: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("書き出しに、書き出し先の外を指すパス %q がある", h.Name)
		}
		target := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// NOTE: シンボリックリンクなどは展開しない（ホストの外を指させないため）。検査はファイルの中身だけを使う
		}
	}
}

const diffScript = `set -eu
git -C /work diff "$BASE"..HEAD
`

// Diff は、clone の base から HEAD（コミット済み）までの差分を返す。BJ に渡す。
func (w *Workspace) Diff(ctx context.Context, ticketID, base string) (string, error) {
	var out bytes.Buffer
	if err := w.run(ctx, w.spec(ticketID, "diff", w.readOnlyRepo(ticketID), map[string]string{"BASE": base}, diffScript), &out); err != nil {
		return "", fmt.Errorf("差分を作れない: %w", err)
	}
	return out.String(), nil
}

const treeScript = `set -eu
cd /work
mkdir -p .git/info
printf '* -filter\n' > .git/info/attributes
git -c core.hooksPath=/dev/null -c core.fsmonitor=false add -A
git write-tree
`

// TreeHash は、clone の作業ツリー（コミットしていない変更を含む）の指紋を返す。進展なしの判定に使う。
func (w *Workspace) TreeHash(ctx context.Context, ticketID string) (string, error) {
	var out bytes.Buffer
	mounts := []container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work"}}
	if err := w.run(ctx, w.spec(ticketID, "tree", mounts, nil, treeScript), &out); err != nil {
		return "", fmt.Errorf("作業ツリーの指紋を作れない: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

const testsChangedScript = `set -eu
cd /work
g() { git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.quotepath=false "$@"; }
g diff-tree --no-commit-id --name-only -r --diff-filter=AM "$COMMIT" | while IFS= read -r f; do
  if ! g diff --quiet "$COMMIT" -- "$f"; then printf '%s\n' "$f"; fi
done
`

// TestsChanged は、commit で足したか変えたファイルのうち、clone の作業ツリー（コミットしていない変更を含む）で
// commit の時点から変わった（消された）ものを返す。p5-test のテストを p5-implement が書き換えていないかの検査に使う（M3）。
func (w *Workspace) TestsChanged(ctx context.Context, ticketID, commit string) ([]string, error) {
	var out bytes.Buffer
	mounts := []container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work"}}
	if err := w.run(ctx, w.spec(ticketID, "tests", mounts, map[string]string{"COMMIT": commit}, testsChangedScript), &out); err != nil {
		return nil, fmt.Errorf("テストのファイルの変更を確かめられない: %w", err)
	}
	var files []string
	for _, l := range strings.Split(out.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// ArchiveTree は、プロジェクトのリポジトリの木（ブランチや木の ID）を dest に取り出す（git archive。ホストの作業ツリーと
// ref は変えない）。main の ID 整合チェックに使う（M2.9）。
func ArchiveTree(ctx context.Context, project, treeish, dest string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", project, "archive", "--format=tar", treeish)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := untar(out, dest); err != nil {
		cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s を取り出せない: %v: %s", treeish, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// MergedTree は、clone の作業ブランチとプロジェクトの main を合わせた木（マージせずに git merge-tree で作る）を dest に
// 取り出す（M2.9。並行で先に main へ入った ID との重複の検査に使う）。ホストには作業ブランチのオブジェクトだけを入れ、
// 作業ツリーと ref は変えない。合わせると衝突するときは conflict を返し、何も取り出さない。
func (w *Workspace) MergedTree(ctx context.Context, ticketID, branch, mainBranch, dest string) (conflict bool, err error) {
	var out bytes.Buffer
	spec := w.spec(ticketID, "merged",
		[]container.Mount{{Type: container.Volume, Source: w.Volume(), Subpath: ticketID + "/repo", Target: "/work", ReadOnly: true}},
		map[string]string{"BRANCH": branch}, importScript)
	if err := w.run(ctx, spec, &out); err != nil {
		return false, fmt.Errorf("作業ブランチを取り出せない: %w", err)
	}
	f, err := os.CreateTemp("", "gateflow-*.bundle")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(out.Bytes()); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	ref := "refs/heads/" + branch
	heads, err := w.git(ctx, "bundle", "list-heads", f.Name(), ref)
	fields := strings.Fields(string(heads))
	if err != nil || len(fields) == 0 {
		return false, fmt.Errorf("作業ブランチ %s のバンドルを読めない: %v", branch, err)
	}
	head := fields[0]
	// オブジェクトだけを入れる（ref も FETCH_HEAD も書かない）
	if _, err := w.git(ctx, "-c", "core.hooksPath=/dev/null", "fetch", "--quiet", "--no-write-fetch-head", f.Name(), ref); err != nil {
		return false, fmt.Errorf("作業ブランチ %s のオブジェクトを入れられない: %w", branch, err)
	}
	tree, err := w.git(ctx, "merge-tree", "--write-tree", "--no-messages", "refs/heads/"+mainBranch, head)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return true, nil
		}
		return false, fmt.Errorf("main と作業ブランチを合わせられない: %w", err)
	}
	return false, ArchiveTree(ctx, w.project, strings.TrimSpace(firstLine(string(tree))), dest)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
