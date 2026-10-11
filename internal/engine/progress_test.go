package engine_test

import (
	"strings"
	"testing"

	"gateflow/internal/engine"
	"gateflow/internal/output"
)

// 正規化は、タイムスタンプ・所要時間・16進のアドレス・一時ディレクトリのパスを潰す。
func TestNormalize(t *testing.T) {
	a := "2026-10-12T09:30:05.123Z --- FAIL: TestX (0.45s)\npanic at 0xc000123abc\nok  pkg 1.234s\n/tmp/go-build123456/b001/x.go:3\n/var/folders/p7/abc/T/TestX12/001/y\n12ms elapsed"
	b := "2026-10-13T11:00:00Z --- FAIL: TestX (1.02s)\npanic at 0xc000999fff\nok  pkg 0.5s\n/tmp/go-build987/b001/x.go:3\n/var/folders/zz/qq/T/TestX99/002/y\n3ms elapsed"
	if engine.Normalize(a) != engine.Normalize(b) {
		t.Errorf("正規化の結果が違う:\n%s\n---\n%s", engine.Normalize(a), engine.Normalize(b))
	}
	if engine.Normalize("--- FAIL: TestX") == engine.Normalize("--- FAIL: TestY") {
		t.Error("テストの名前の違いまで潰した")
	}
}

func TestVerifyFingerprint(t *testing.T) {
	fp1 := engine.VerifyFingerprint(map[string]string{"test": "--- FAIL: TestX (0.1s)\n"})
	fp2 := engine.VerifyFingerprint(map[string]string{"test": "--- FAIL: TestX (0.9s)\n"})
	fp3 := engine.VerifyFingerprint(map[string]string{"test": "--- FAIL: TestX (0.9s)\n", "lint": "x"})
	if fp1.Hash != fp2.Hash {
		t.Error("所要時間だけが違う失敗を、別の失敗とみなした")
	}
	if fp1.Hash == fp3.Hash {
		t.Error("不合格のコマンドが増えたのに、同じ失敗とみなした")
	}
	if !strings.Contains(fp1.Raw, "(0.1s)") || strings.Contains(fp1.Normalized, "(0.1s)") {
		t.Errorf("正規化の前と後の両方を持っていない: %+v", fp1)
	}
}

// 前回と同じ指摘（ラベル・根拠・本文）が残っていれば、進展なし。
func TestSameFindings(t *testing.T) {
	f := func(label, file string, line int, text string) output.Finding {
		return output.Finding{Label: label, Finding: text, Evidence: &output.Evidence{File: file, Line: line}}
	}
	prev := engine.FindingKeys([]output.Finding{f("MUST", "a.go", 3, "エラーを無視している"), f("IMO", "b.go", 1, "名前")})
	same := engine.FindingKeys([]output.Finding{f("MUST", "a.go", 3, "エラーを  無視している")})
	other := engine.FindingKeys([]output.Finding{f("MUST", "a.go", 9, "別の指摘")})
	if !engine.Overlaps(prev, same) {
		t.Error("同じ指摘が残っているのに進展ありとみなした")
	}
	if engine.Overlaps(prev, other) {
		t.Error("別の指摘なのに進展なしとみなした")
	}
	if len(prev) != 1 {
		t.Errorf("止めないラベル（IMO）まで数えた: %v", prev)
	}
}
