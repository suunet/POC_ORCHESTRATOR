package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// 記録の書き込みは2つに分けて扱う（M2.8。設計の原則「失敗を取り繕わない」の具体）。
//   - 状態と人が読む記録、次のエージェントの入力（state.json、レビュー報告、質疑、指示書、running.json、
//     feedback.md・diff.patch・verify.log）：書けなければ、その実行を止めて人の確認待ちにする（write-failed）
//   - 診断用の記録（prompt.md、エージェントの標準出力と標準エラー、コマンドごとの検証のログ、allowlist.txt、
//     指紋、proxy.log）：書けなくても続け、log.jsonl に write-error を残す

// stopWriteFailed は、止まる理由。再開の位置は、書けなかったときに行っていた step（M2.8 の Q-03）。
const stopWriteFailed = "write-failed"

// writeState は、状態と人が読む記録・次の入力を書く。書けなければ、実行を止める errStop を返す。
func (e *Engine) writeState(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return e.writeFailed(path, err)
	}
	return nil
}

// writeFailed は、状態の記録が書けなかったことを、実行を止める errStop にする。
func (e *Engine) writeFailed(path string, err error) error {
	return &errStop{reason: stopWriteFailed, detail: fmt.Sprintf("%s を書けない：%v", e.relPath(path), err)}
}

// writeDiag は、診断用の記録を書く。書けなくても続け、write-error を残す。
func (e *Engine) writeDiag(path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		e.writeError(path, err)
	}
}

// writeError は、診断用の記録が書けなかったことを log.jsonl に残す（書けなければ標準エラーへ）。
func (e *Engine) writeError(path string, err error) {
	e.logOrStderr("write-error", map[string]any{"file": e.relPath(path), "error": err.Error()})
}

// cleanupError は、後片付け（コンテナ・プロキシ・印の削除）の失敗を残す。実行は止めない。
func (e *Engine) cleanupError(what string, err error) {
	e.logOrStderr("cleanup-error", map[string]any{"target": what, "error": err.Error()})
}

// removeIfExists は、ファイルを消す。ないことだけを無視する。
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// logOrStderr は、log.jsonl に記録する。実行の外（試験など）か、log.jsonl に書けなければ標準エラーへ出す。
func (e *Engine) logOrStderr(event string, fields map[string]any) {
	if e.run != nil {
		if err := e.run.Log(e.Now(), event, fields); err == nil {
			return
		} else {
			fmt.Fprintf(e.stderr(), "gateflow: 実行の記録（log.jsonl）に書けない：%v\n", err)
		}
	}
	fmt.Fprintf(e.stderr(), "gateflow: %s %v\n", event, fields)
}

func (e *Engine) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return os.Stderr
}

// relPath は、記録に残すファイルの名前（チケットのディレクトリからの相対パス）。
func (e *Engine) relPath(path string) string {
	if e.Ticket != nil {
		if rel, err := filepath.Rel(filepath.Dir(e.Ticket.Path), path); err == nil {
			return rel
		}
	}
	return path
}
