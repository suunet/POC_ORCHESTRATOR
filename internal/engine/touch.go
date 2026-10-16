package engine

import (
	"context"
	"fmt"
	"path"
	"strings"

	"gateflow/internal/config"
)

// touchMapKnowledge は、P3 に渡す touchMap の説明（このコードを触る計画なら、この文書を §4.4 の候補にする。M3）。
func touchMapKnowledge(rules []config.TouchRule) string {
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("コードと確認すべき文書の対応（touchMap）。計画で次のコードを触るなら、対応する文書を §4.4 の更新する文書の候補にする（変える必要がなければ挙げなくてよい）：")
	for _, r := range rules {
		fmt.Fprintf(&b, "\n- `%s` → %s", r.Code, strings.Join(quoteAll(r.Docs), "、"))
		if r.Note != "" {
			fmt.Fprintf(&b, "（%s）", r.Note)
		}
	}
	return b.String()
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "`" + s + "`"
	}
	return out
}

// checkTouchDocs は、作業ブランチで変わったコードのファイルが touchMap の code に当たり、対応する文書が §4.4 にも差分にも
// なければ、推奨の指摘（touch-doc）にする（M3）。止めない。
func (e *Engine) checkTouchDocs(ctx context.Context) error {
	if len(e.Project.TouchMap) == 0 {
		return nil
	}
	diff, err := e.Workspace.Diff(ctx, e.Ticket.ID, e.Project.Branch.Main)
	if err != nil {
		return err
	}
	changed := diffFiles(diff)
	sheets := e.Ticket.SheetFiles()
	n := 0
	for _, r := range e.Project.TouchMap {
		var hit []string
		for _, f := range changed {
			if globMatch(r.Code, f) {
				hit = append(hit, f)
			}
		}
		if len(hit) == 0 {
			continue
		}
		for _, d := range r.Docs {
			if covers(d, changed) || covers(d, sheets) {
				continue
			}
			n++
			msg := fmt.Sprintf("touch-doc：%s を変えたが、対応する文書 %s が §4.4 にも差分にもない", strings.Join(hit, "、"), d)
			if r.Note != "" {
				msg += "（" + r.Note + "）"
			}
			e.recommended = append(e.recommended, msg)
		}
	}
	e.log("check-touch-docs", map[string]any{"changed": len(changed), "findings": n})
	return nil
}

// diffFiles は、git diff の出力から、変わったファイル（変更の後のパス）を返す。
func diffFiles(diff string) []string {
	var files []string
	for _, l := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(l, "diff --git a/") {
			continue
		}
		if i := strings.LastIndex(l, " b/"); i >= 0 {
			files = append(files, l[i+3:])
		}
	}
	return files
}

// covers は、文書 doc（ファイルか、末尾が / のディレクトリ）が files のどれかに当たるかを返す。
func covers(doc string, files []string) bool {
	dir := strings.TrimSuffix(doc, "/") + "/"
	for _, f := range files {
		if f == doc || strings.HasPrefix(f, dir) {
			return true
		}
	}
	return false
}

// globMatch は、/ 区切りのパス name がグロブ pattern に当たるかを返す。** は0個以上のディレクトリに当たる。
func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
