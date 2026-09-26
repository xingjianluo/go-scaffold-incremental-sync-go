package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEndToEndRegenerationDeterministic(t *testing.T) {
	dir := t.TempDir()
	args := []string{"-targets", dir, "-name", "demo", "-module", "example.com/demo"}
	if code := run(args); code != 0 {
		t.Fatalf("first run exit = %d", code)
	}
	snap1 := snapshot(t, dir)
	t.Logf("first run files: %v", keys(snap1))
	for i := 0; i < 5; i++ {
		if code := run(args); code != 0 {
			t.Fatalf("run %d exit = %d", i, code)
		}
		if snap := snapshot(t, dir); !reflect.DeepEqual(snap, snap1) {
			t.Fatalf("run %d output differs", i)
		}
	}
	t.Logf("5 regenerations byte-identical")
}

func TestEndToEndExitCodes(t *testing.T) {
	t.Run("invalid config exits 2", func(t *testing.T) {
		dir := t.TempDir()
		code := run([]string{"-targets", dir, "-license", "WTFPL"})
		t.Logf("exit=%d for invalid license", code)
		if code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})

	t.Run("conflict exits 3 and keeps user content", func(t *testing.T) {
		dir := t.TempDir()
		if code := run([]string{"-targets", dir, "-name", "demo", "-module", "example.com/demo"}); code != 0 {
			t.Fatalf("setup run exit = %d", code)
		}
		readme := filepath.Join(dir, "README.md")
		if err := os.WriteFile(readme, []byte("user custom readme\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 用不同参数再生成：模板侧也变了 -> 冲突。
		code := run([]string{"-targets", dir, "-name", "renamed", "-module", "example.com/demo"})
		t.Logf("exit=%d for conflicting regeneration", code)
		if code != 3 {
			t.Fatalf("exit = %d, want 3", code)
		}
		b, _ := os.ReadFile(readme)
		if string(b) != "user custom readme\n" {
			t.Fatalf("conflict overwrote user content: %q", b)
		}
	})

	t.Run("conflict resolved by policy theirs", func(t *testing.T) {
		dir := t.TempDir()
		if code := run([]string{"-targets", dir, "-name", "demo", "-module", "example.com/demo"}); code != 0 {
			t.Fatalf("setup run exit = %d", code)
		}
		readme := filepath.Join(dir, "README.md")
		if err := os.WriteFile(readme, []byte("user custom readme\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code := run([]string{"-targets", dir, "-name", "renamed", "-module", "example.com/demo", "-policy", "theirs"})
		t.Logf("exit=%d with -policy=theirs", code)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
	})
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
