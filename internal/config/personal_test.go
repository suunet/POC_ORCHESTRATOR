package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gateflow/internal/config"
)

func writePersonal(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".gateflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadPersonal(t *testing.T) {
	home := writePersonal(t, `{"agent": "claude", "model": "opus"}`)
	p, err := config.LoadPersonal(home)
	if err != nil {
		t.Fatalf("LoadPersonal: %v", err)
	}
	if p.Agent != config.AgentClaude || p.Model != "opus" {
		t.Errorf("personal = %+v", p)
	}
}

func TestLoadPersonal_Invalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		// 使う AI サービスに既定値はない（許可のない人が意図せず使うことを防ぐ）。
		{"使う AI サービスがない", `{}`, []string{"agent"}},
		{"知らない AI サービス", `{"agent": "other"}`, []string{"other"}},
		{"知らない項目", `{"agent": "claude", "token": "x"}`, []string{"token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadPersonal(writePersonal(t, tc.body))
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestLoadPersonal_Missing(t *testing.T) {
	_, err := config.LoadPersonal(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("err = %v, want 個人の設定がないことの誤り", err)
	}
}
