// Package testutil gives tests a private copy of the fixture registry.
package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// FixtureDir returns testdata/registry at the module root.
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "registry")
}

// Registry copies the fixture registry into a temp dir and returns its path.
func Registry(t testing.TB) string {
	t.Helper()
	dst := t.TempDir()
	src := FixtureDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// Write writes content to root/rel, creating directories.
func Write(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
