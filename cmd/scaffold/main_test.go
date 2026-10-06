package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// --- 批量 / 取消 / 预演 的端到端用例 ---

// TestCLIBatchMultipleTargets 一次传 3 个目标：每个目录都应生成，
// 重复执行结果字节级一致（相同输入两遍结果完全一致）。
func TestCLIBatchMultipleTargets(t *testing.T) {
	d1, d2, d3 := t.TempDir(), t.TempDir(), t.TempDir()
	// 故意打乱顺序，验证内部会排序去重。
	targets := strings.Join([]string{d3, d1, d2}, ",")
	args := []string{"-targets", targets, "-name", "demo", "-module", "example.com/demo", "-concurrency", "2"}
	if code := run(args); code != 0 {
		t.Fatalf("first batch run exit = %d", code)
	}
	s1, s2, s3 := snapshot(t, d1), snapshot(t, d2), snapshot(t, d3)
	for i, s := range []map[string]string{s1, s2, s3} {
		if _, ok := s["go.mod"]; !ok {
			t.Fatalf("target %d missing go.mod: %v", i, keys(s))
		}
	}
	t.Logf("batch generated %d targets, files per target=%v", 3, keys(s1))
	for k := 0; k < 3; k++ {
		if code := run(args); code != 0 {
			t.Fatalf("repeat %d exit = %d", k, code)
		}
	}
	for i, want := range []map[string]string{s1, s2, s3} {
		got := snapshot(t, []string{d1, d2, d3}[i])
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("target %d not stable across repeats", i)
		}
	}
	t.Logf("3 repeated batch runs byte-identical")
}

// TestCLIBatchPartialFailure 一个目标冲突，其它目标不受影响仍成功，退出码 3。
func TestCLIBatchPartialFailure(t *testing.T) {
	bad := t.TempDir()
	good1 := t.TempDir()
	good2 := t.TempDir()
	// bad 先按默认参数生成，再改掉 README，随后用不同 name 再生成 -> 双方都改冲突。
	if code := run([]string{"-targets", bad, "-name", "demo", "-module", "example.com/demo"}); code != 0 {
		t.Fatalf("setup bad target exit = %d", code)
	}
	if err := os.WriteFile(filepath.Join(bad, "README.md"), []byte("user readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-targets", strings.Join([]string{good1, bad, good2}, ","),
		"-name", "renamed", "-module", "example.com/demo",
		"-concurrency", "1",
	}
	code := run(args)
	t.Logf("partial-failure batch exit = %d", code)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (conflict)", code)
	}
	// 冲突目标保留用户内容。
	if b, _ := os.ReadFile(filepath.Join(bad, "README.md")); string(b) != "user readme\n" {
		t.Fatalf("conflict target user content lost: %q", b)
	}
	// 其余目标正常生成。
	for i, g := range []string{good1, good2} {
		if _, ok := snapshot(t, g)["go.mod"]; !ok {
			t.Fatalf("good target %d not generated", i)
		}
	}
}

// TestCLIPlanIsReadOnly -plan 不得落盘任何文件；随后真正执行应生成文件。
func TestCLIPlanIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	planArgs := []string{"-targets", dir, "-name", "demo", "-module", "example.com/demo", "-plan"}
	if code := run(planArgs); code != 0 {
		t.Fatalf("plan exit = %d", code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("-plan wrote files: %v", entries)
	}
	t.Logf("plan left target directory empty")

	// 对已有项目预演（含冲突）同样不落盘。
	if code := run([]string{"-targets", dir, "-name", "demo", "-module", "example.com/demo"}); code != 0 {
		t.Fatalf("seed run exit = %d", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	code := run([]string{"-targets", dir, "-name", "renamed", "-module", "example.com/demo", "-plan"})
	t.Logf("plan with conflict exit = %d", code)
	if code != 3 {
		t.Fatalf("conflicting plan exit = %d, want 3", code)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Fatalf("conflicting plan mutated files")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "local\n" {
		t.Fatalf("plan overwrote local content: %q", b)
	}
}

// TestCLITimeoutExitCode 整体超时：整批应归类取消，退出码 5。
func TestCLITimeoutExitCode(t *testing.T) {
	dirs := make([]string, 8)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	args := []string{
		"-targets", strings.Join(dirs, ","),
		"-name", "demo", "-module", "example.com/demo",
		"-concurrency", "1", "-timeout", "1ns",
	}
	code := run(args)
	t.Logf("timeout batch exit = %d", code)
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (cancelled/timeout)", code)
	}
}

// TestCLIRejectBadConcurrency 非法并发上限必须被拒绝（退出码 2）。
func TestCLIRejectBadConcurrency(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{"-targets", dir, "-concurrency", "0"})
	t.Logf("concurrency=0 exit = %d", code)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
