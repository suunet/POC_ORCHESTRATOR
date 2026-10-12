package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gateflow/internal/output"
)

// normalizers は、進展なしの判定で出力の指紋を作る前に潰す、実行ごとに変わる部分（Q-08）。
// 言語の出力は解析しない。誤判定のときに人が根拠を確かめられるよう、正規化の前と後の両方を記録に残す。
var normalizers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// タイムスタンプ（例：2026-10-12T09:30:05.123Z、2026-10-12 09:30:05）
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`), "<time>"},
	// 一時ディレクトリのパス（/tmp/…、/var/folders/…、go-build…）
	{regexp.MustCompile(`(/private)?/var/folders/[^\s:]+|/tmp/[^\s:]+`), "<tmp>"},
	// 16進のアドレス（例：0xc000123abc）
	{regexp.MustCompile(`0x[0-9a-fA-F]+`), "<addr>"},
	// 所要時間（例：0.45s、12ms、1m2s）
	{regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|us|ms|s|m|h)\b`), "<dur>"},
}

// Normalize は、出力から実行ごとに変わる部分を潰す。
func Normalize(s string) string {
	for _, n := range normalizers {
		s = n.re.ReplaceAllString(s, n.repl)
	}
	return s
}

// Fingerprint は、指紋と、その元になった正規化の前と後の文字列。
type Fingerprint struct {
	Hash       string
	Raw        string
	Normalized string
}

// VerifyFingerprint は、不合格の検証コマンド（名前 → 出力）の指紋を作る。
func VerifyFingerprint(failed map[string]string) Fingerprint {
	names := make([]string, 0, len(failed))
	for n := range failed {
		names = append(names, n)
	}
	sort.Strings(names)
	var raw, norm strings.Builder
	for _, n := range names {
		fmt.Fprintf(&raw, "## %s\n%s\n", n, failed[n])
		fmt.Fprintf(&norm, "## %s\n%s\n", n, Normalize(failed[n]))
	}
	return Fingerprint{Hash: hash(norm.String()), Raw: raw.String(), Normalized: norm.String()}
}

// FindingKeys は、遷移を止める指摘（MUST・Q）の指紋の一覧（ラベル＋根拠＋空白を除いた本文）を返す。
func FindingKeys(fs []output.Finding) []string {
	var keys []string
	for _, f := range fs {
		if f.Label != "MUST" && f.Label != "Q" {
			continue
		}
		keys = append(keys, findingKey(f))
	}
	sort.Strings(keys)
	return keys
}

// findingKey は、指摘1件の指紋（ラベル、根拠、本文）。
func findingKey(f output.Finding) string {
	var ev string
	if e := f.Evidence; e != nil {
		ev = fmt.Sprintf("%s:%d:%s", e.File, e.Line, e.ID)
	}
	return f.Label + "|" + ev + "|" + hash(strings.Join(strings.Fields(f.Finding), ""))
}

// Overlaps は、前回の指摘の指紋のどれかが今回も残っているかを返す。
func Overlaps(prev, cur []string) bool {
	for _, c := range cur {
		for _, p := range prev {
			if c == p {
				return true
			}
		}
	}
	return false
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
