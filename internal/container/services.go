package container

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Services は、プロジェクトで共有する DB などのサービス（compose のサービス。M3）。run の始めに起動して、
// エージェントの内部ネットワークと、サービスにだけ届く専用のネットワークの両方へ、サービスの名前を別名としてつなぐ。
// 検証のコンテナは専用のネットワークにだけ置く（内部ネットワークに置かない。人の決定）。共有なので止めない。
type Services struct {
	Docker  Docker
	Project string
	Root    string   // プロジェクトのルート（ホスト）
	Compose string   // compose のファイル（ルートからの相対パス）
	Names   []string // 起動するサービスの名前
}

// Network は、サービスと検証のコンテナだけをつなぐ専用のネットワークの名前（外への経路なし）。
func (s Services) Network() string { return "gateflow-" + s.Project + "-services" }

// composeProject は、compose のプロジェクトの名前（プロジェクトの compose の既定の名前と分ける）。
func (s Services) composeProject() string { return "gateflow-" + s.Project + "-services" }

func (s Services) compose(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"compose", "--project-directory", s.Root, "-f", filepath.Join(s.Root, filepath.FromSlash(s.Compose)), "-p", s.composeProject()}
	return s.Docker.Exec(ctx, append(base, args...)...)
}

// Up は、サービスを起動し（立っていれば何もしない）、2つのネットワークへつないで、検証のコンテナを置くネットワークの名前を返す。
func (s Services) Up(ctx context.Context) (string, error) {
	internal := Egress{Project: s.Project}.Network()
	for _, n := range []string{internal, s.Network()} {
		if err := ensureInternalNetwork(ctx, s.Docker, n); err != nil {
			return "", err
		}
	}
	if _, err := s.compose(ctx, append([]string{"up", "-d", "--wait"}, s.Names...)...); err != nil {
		return "", fmt.Errorf("共有のサービス（%s）を起動できない: %w", strings.Join(s.Names, "・"), err)
	}
	for _, name := range s.Names {
		out, err := s.compose(ctx, "ps", "-q", name)
		if err != nil {
			return "", fmt.Errorf("共有のサービス %s のコンテナが分からない: %w", name, err)
		}
		id := strings.TrimSpace(string(out))
		if id == "" {
			return "", fmt.Errorf("共有のサービス %s のコンテナがない（compose のファイル %s のサービスの名前を確かめる）", name, s.Compose)
		}
		for _, n := range []string{internal, s.Network()} {
			if err := s.connect(ctx, n, name, id); err != nil {
				return "", err
			}
		}
	}
	return s.Network(), nil
}

// connect は、コンテナ id をネットワーク n へ別名 alias でつなぐ。もうつながっていれば何もしない。
func (s Services) connect(ctx context.Context, n, alias, id string) error {
	out, err := s.Docker.Exec(ctx, "inspect", "--format", "{{json .NetworkSettings.Networks}}", id)
	if err == nil && strings.Contains(string(out), `"`+n+`"`) {
		return nil
	}
	if _, err := s.Docker.Exec(ctx, "network", "connect", "--alias", alias, n, id); err != nil {
		return fmt.Errorf("共有のサービス %s をネットワーク %s へつなげない: %w", alias, n, err)
	}
	return nil
}

// Down は、共有のサービスを止めて消し、専用のネットワークを消す（gateflow services down。人が行う）。
func (s Services) Down(ctx context.Context) error {
	if _, err := s.compose(ctx, "down"); err != nil {
		return fmt.Errorf("共有のサービスを止められない: %w", err)
	}
	if _, err := s.Docker.Exec(ctx, "network", "inspect", s.Network()); err != nil {
		return nil // ない
	}
	if _, err := s.Docker.Exec(ctx, "network", "rm", s.Network()); err != nil {
		return fmt.Errorf("専用のネットワーク %s を消せない（使っているコンテナが残っている）: %w", s.Network(), err)
	}
	return nil
}

// ensureInternalNetwork は、外への経路のない（--internal）ネットワーク name を用意する。同時に別の実行が作ったなら、それを使う。
func ensureInternalNetwork(ctx context.Context, d Docker, name string) error {
	if _, err := d.Exec(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	if _, err := d.Exec(ctx, "network", "create", "--internal", name); err != nil {
		if _, ierr := d.Exec(ctx, "network", "inspect", name); ierr != nil {
			return fmt.Errorf("ネットワーク %s を作れない: %w", name, err)
		}
	}
	return nil
}
