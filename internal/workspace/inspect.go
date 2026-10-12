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
