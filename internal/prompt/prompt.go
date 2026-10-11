// Package prompt は、エージェントへのプロンプトを5要素（Persona・Knowledge・Instruction・Output Contract・Policy）から組み立てる。
//
// Persona はシステムプロンプトに、ほかはユーザーメッセージに入れ、Policy を最後に置く。層の解決（組み込み → プロジェクトの上書き）は M3。
package prompt

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gateflow/internal/agent"
)

// PolicyFile は、エージェントに渡す規約のファイル（コンテナの中の場所と用途）。
type PolicyFile struct {
	Path    string
	Purpose string
}

// Input は、プロンプトの5要素。
type Input struct {
	Persona        string
	Knowledge      []string
	Instruction    string
	OutputContract string
	Policies       []PolicyFile
	BuiltinPolicy  []string // 組み込みの規約の本文
	Model          string
}

// Build は、5要素からエージェントへの依頼を作る。
func Build(in Input) agent.Request {
	var b strings.Builder
	if len(in.Knowledge) > 0 {
		b.WriteString("# 知識\n\n")
		for _, k := range in.Knowledge {
			fmt.Fprintf(&b, "- %s\n", k)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "# 指示\n\n%s\n\n", strings.TrimSpace(in.Instruction))
	fmt.Fprintf(&b, "# 出力\n\n%s\n\n", strings.TrimSpace(in.OutputContract))
	if len(in.Policies) > 0 || len(in.BuiltinPolicy) > 0 {
		b.WriteString("# 規約\n\n")
		if len(in.Policies) > 0 {
			b.WriteString("次のプロジェクトの規約を読み、従う。\n\n")
			for _, p := range in.Policies {
				fmt.Fprintf(&b, "- `%s`：%s\n", p.Path, p.Purpose)
			}
			b.WriteString("\n")
		}
		for _, p := range in.BuiltinPolicy {
			fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(p))
		}
	}
	return agent.Request{System: strings.TrimSpace(in.Persona), Prompt: strings.TrimSpace(b.String()) + "\n", Model: in.Model}
}

// LoadBuiltin は、組み込みの枠を読み、同じディレクトリの supplement/ に同じ名前の補足があれば後ろに足す。
func LoadBuiltin(fsys fs.FS, name string) (string, error) {
	frame, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("組み込みの %s を読めない: %w", name, err)
	}
	text := string(frame)
	if sup, err := fs.ReadFile(fsys, path.Join(path.Dir(name), "supplement", path.Base(name))); err == nil {
		text = strings.TrimRight(text, "\n") + "\n\n" + string(sup)
	}
	return text, nil
}
