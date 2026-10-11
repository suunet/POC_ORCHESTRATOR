package container

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tool は、ツール本体（gateflow のリポジトリ）。読み取りだけで使う。
type Tool struct {
	Root string
}

// FindTool は、実行ファイル exe の場所（<ツール本体>/bin/）からツール本体を求める。home（GATEFLOW_HOME）があれば優先する。
func FindTool(exe, home string) (Tool, error) {
	if home != "" {
		return Tool{Root: home}, nil
	}
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		return Tool{Root: filepath.Dir(dir)}, nil
	}
	return Tool{}, fmt.Errorf("ツール本体の場所が分からない（実行ファイル %s が bin/ にない）。GATEFLOW_HOME にツール本体の場所を指定する", exe)
}

// ProxyBinary は、プロキシのイメージに入れる Linux 用の実行ファイルの場所。
func (t Tool) ProxyBinary(arch string) string {
	return filepath.Join(t.Root, "bin", "gateflow-linux-"+arch)
}

// Dockerfile は、ツール本体の docker/<name>.Dockerfile の場所。
func (t Tool) Dockerfile(name string) string {
	return filepath.Join(t.Root, "docker", name+".Dockerfile")
}

// CheckProxyBinary は、Linux 用の実行ファイルがあり、ビルド時に刻んだ版が want と同じことを確かめて、場所を返す。
// 古い実行ファイルで古いプロキシが黙って動くことを防ぐ。版は実行せずにビルドの情報から読む。
func (t Tool) CheckProxyBinary(arch, want string) (string, error) {
	path := t.ProxyBinary(arch)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("プロキシ用の実行ファイル %s がない。ツール本体で scripts/build.sh を実行する", path)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("プロキシ用の実行ファイル %s の版を読めない（scripts/build.sh で作り直す）: %w", path, err)
	}
	var got string
	for _, s := range info.Settings {
		if s.Key == "-ldflags" {
			got = VersionFromLdflags(s.Value)
		}
	}
	if got != want {
		return "", fmt.Errorf("プロキシ用の実行ファイルの版 %q が、この gateflow の版 %q と違う。ツール本体で scripts/build.sh を実行する", got, want)
	}
	return path, nil
}

// VersionFromLdflags は、-ldflags の値から -X main.version=<版> の版を取り出す。なければ空。
func VersionFromLdflags(flags string) string {
	fields := splitQuoted(flags)
	for i, f := range fields {
		var v string
		switch {
		case f == "-X" && i+1 < len(fields):
			v = fields[i+1]
		case strings.HasPrefix(f, "-X="):
			v = strings.TrimPrefix(f, "-X=")
		default:
			continue
		}
		if ver, ok := strings.CutPrefix(v, "main.version="); ok {
			return ver
		}
	}
	return ""
}

// splitQuoted は、空白で区切る。二重引用符の中の空白では区切らず、引用符は取り除く。
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, has := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, has = !quoted, true
		case r == ' ' && !quoted:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}
