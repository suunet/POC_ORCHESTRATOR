// Package builtins は、実行ファイルに埋め込む組み込みの層（ひな形とプロンプト）を持つ。
//
// 各ディレクトリは、Git管理する枠と、Git管理外の補足（supplement/）に分かれる。
// 補足はビルド時にあれば埋め込まれ、なくてもビルドは失敗しない（supplement/README.md は常にある）。
package builtins

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed templates prompts
var files embed.FS

// FS は、組み込みのファイルを返す。
func FS() fs.FS {
	return files
}

// Supplements は、埋め込まれた補足のファイル（案内の README.md を除く）のパスを返す。
func Supplements() []string {
	var out []string
	_ = fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(path, "/supplement/") && !strings.HasSuffix(path, "/supplement/README.md") {
			out = append(out, path)
		}
		return nil
	})
	return out
}
