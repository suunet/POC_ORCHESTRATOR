package prompt_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"gateflow/internal/prompt"
)

func TestBuild(t *testing.T) {
	req := prompt.Build(prompt.Input{
		Persona:        "PERSONA",
		Knowledge:      []string{"指示書は /in/ticket.md", "作業場所は /work"},
		Instruction:    "INSTRUCTION",
		OutputContract: "OUTPUT",
		Policies:       []prompt.PolicyFile{{Path: "/in/policies/coding.md", Purpose: "コーディング規約"}},
		BuiltinPolicy:  []string{"BUILTIN-POLICY"},
		Model:          "opus",
	})
	if req.System != "PERSONA" || req.Model != "opus" {
		t.Errorf("System, Model = %q, %q", req.System, req.Model)
	}
	if strings.Contains(req.Prompt, "PERSONA") {
		t.Error("Persona がユーザーメッセージに入っている")
	}
	// 順序：Knowledge → Instruction → Output Contract → Policy（最後）
	order := []string{"/in/ticket.md", "INSTRUCTION", "OUTPUT", "/in/policies/coding.md", "BUILTIN-POLICY"}
	last := -1
	for _, w := range order {
		i := strings.Index(req.Prompt, w)
		if i < 0 {
			t.Fatalf("プロンプトに %q がない:\n%s", w, req.Prompt)
		}
		if i < last {
			t.Errorf("%q の位置が順序どおりでない:\n%s", w, req.Prompt)
		}
		last = i
	}
	if !strings.Contains(req.Prompt, "コーディング規約") {
		t.Error("規約の用途がない")
	}
}

func TestBuild_NoPolicies(t *testing.T) {
	req := prompt.Build(prompt.Input{Persona: "P", Instruction: "I", OutputContract: "O"})
	if !strings.Contains(req.Prompt, "I") || !strings.Contains(req.Prompt, "O") {
		t.Errorf("Prompt = %q", req.Prompt)
	}
}

// 組み込みの枠に、補足（supplement/ の同じ名前のファイル）があれば後ろに足す。
func TestLoadBuiltin(t *testing.T) {
	fsys := fstest.MapFS{
		"prompts/policies/test-first.md":            {Data: []byte("FRAME\n")},
		"prompts/policies/supplement/test-first.md": {Data: []byte("SUPPLEMENT\n")},
		"prompts/personas/dev.md":                   {Data: []byte("DEV\n")},
	}
	got, err := prompt.LoadBuiltin(fsys, "prompts/policies/test-first.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "FRAME") || !strings.Contains(got, "SUPPLEMENT") || strings.Index(got, "FRAME") > strings.Index(got, "SUPPLEMENT") {
		t.Errorf("got = %q", got)
	}
	got, err = prompt.LoadBuiltin(fsys, "prompts/personas/dev.md")
	if err != nil || strings.TrimSpace(got) != "DEV" {
		t.Errorf("補足のない枠 = %q, %v", got, err)
	}
	if _, err := prompt.LoadBuiltin(fsys, "prompts/none.md"); err == nil {
		t.Error("ない枠を読めた")
	}
}
