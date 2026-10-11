package builtins_test

import (
	"io/fs"
	"testing"

	"gateflow/builtins"
)

// 枠は補足の有無にかかわらず埋め込まれる。
func TestFS_Frames(t *testing.T) {
	for _, path := range []string{
		"templates/catalog/buc.md",
		"templates/catalog/supplement/README.md",
		"templates/ticket/ticket.md",
		"prompts/policies/test-first.md",
		"prompts/personas/dev.md",
		"prompts/instructions/p5.md",
		"prompts/personas/bj.md",
		"prompts/instructions/p2-review.md",
		"prompts/instructions/p3-prepare.md",
		"prompts/instructions/p5-verify.md",
		"workflows/aidd-impl.json",
		"prompts/policies/supplement/README.md",
	} {
		if _, err := fs.Stat(builtins.FS(), path); err != nil {
			t.Errorf("%s が埋め込まれていない: %v", path, err)
		}
	}
}

func TestSupplements_ExcludesReadme(t *testing.T) {
	for _, p := range builtins.Supplements() {
		if p == "templates/catalog/supplement/README.md" {
			t.Errorf("案内の README.md が補足に含まれている")
		}
	}
}
