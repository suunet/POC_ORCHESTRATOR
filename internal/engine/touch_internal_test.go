package engine

import "testing"

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"backend/**/adapters/db/**", "backend/internal/adapters/db/user.go", true},
		{"backend/**/adapters/db/**", "backend/adapters/db/x/y.go", true},
		{"backend/**/adapters/db/**", "backend/internal/adapters/http/user.go", false},
		{"backend/*.go", "backend/main.go", true},
		{"backend/*.go", "backend/x/main.go", false},
		{"**/*.sql", "db/migrations/001.sql", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}

func TestDiffFilesAndCovers(t *testing.T) {
	files := diffFiles("diff --git a/x/a.go b/x/a.go\n+1\ndiff --git a/.docs/design/erd/columns/users.md b/.docs/design/erd/columns/users.md\n")
	if len(files) != 2 || files[1] != ".docs/design/erd/columns/users.md" {
		t.Fatalf("files = %q", files)
	}
	if !covers(".docs/design/erd/columns/", files) || !covers(".docs/design/erd/columns", files) || covers(".docs/design/uc/", files) {
		t.Error("covers が誤っている")
	}
}
