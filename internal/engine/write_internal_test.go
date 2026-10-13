package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// os.Remove は、ファイルがないことだけを無視する（M2.8）。
func TestRemoveIfExists(t *testing.T) {
	dir := t.TempDir()
	if err := removeIfExists(filepath.Join(dir, "none")); err != nil {
		t.Errorf("ないファイル: %v", err)
	}
	f := filepath.Join(dir, "f")
	os.WriteFile(f, []byte("x"), 0o644)
	if err := removeIfExists(f); err != nil {
		t.Errorf("あるファイル: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("消えていない")
	}
	d := filepath.Join(dir, "d")
	os.MkdirAll(filepath.Join(d, "x"), 0o755)
	if err := removeIfExists(d); err == nil {
		t.Error("中身のあるディレクトリを消せないのに、失敗を返さない")
	}
}
