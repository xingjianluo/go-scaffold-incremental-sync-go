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

func TestEndToEndBatchMultipleTargets(t *testing.T) {
	root := t.TempDir()
	dirs := []string{"alpha", "beta", "gamma"}
	var paths []string
	for _, d := range dirs {
		paths = append(paths, filepath.Join(root, d))
	}
	args := []string{"-targets", strings.Join(paths, ","), "-jobs", "2",
		"-name", "demo", "-module", "example.com/demo"}
	if code := run(args); code != 0 {
		t.Fatalf("batch run exit = %d", code)
	}
	for _, p := range paths {
		if snap := snapshot(t, p); snap["go.mod"] == "" {
			t.Fatalf("target %s not generated", p)
		}
		t.Logf("target %s generated: %v", p, keys(snapshot(t, p)))
	}

	// 让 beta 与模板双方同时改动 -> 仅 beta 冲突，退出码 3，其它目标不受影响。
	if err := os.WriteFile(filepath.Join(paths[1], "README.md"), []byte("beta local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := run([]string{"-targets", strings.Join(paths, ","), "-jobs", "2",
		"-name", "demo2", "-module", "example.com/demo"})
	t.Logf("partial-conflict batch exit=%d", code)
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	// alpha/gamma 正常更新，beta 用户内容保留。
	if snap := snapshot(t, paths[0]); !strings.Contains(snap["README.md"], "demo2") {
		t.Fatalf("alpha should be updated, README=%q", snap["README.md"])
	}
	if got := snapshot(t, paths[1])["README.md"]; got != "beta local\n" {
		t.Fatalf("beta user content overwritten: %q", got)
	}
	if snap := snapshot(t, paths[2]); !strings.Contains(snap["README.md"], "demo2") {
		t.Fatalf("gamma should be updated, README=%q", snap["README.md"])
	}
	t.Logf("basis: one conflicting target isolated; others updated; its content kept")
}

func TestEndToEndBatchDeterministic(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "d2"), filepath.Join(root, "d0"), filepath.Join(root, "d1")}
	args := []string{"-targets", strings.Join(paths, ","), "-jobs", "3",
		"-name", "demo", "-module", "example.com/demo"}
	if code := run(args); code != 0 {
		t.Fatalf("first run exit=%d", code)
	}
	ref := map[string]map[string]string{}
	for _, p := range paths {
		ref[p] = snapshot(t, p)
	}
	for i := 0; i < 5; i++ {
		if code := run(args); code != 0 {
			t.Fatalf("repeat run %d exit=%d", i, code)
		}
		for _, p := range paths {
			if !reflect.DeepEqual(snapshot(t, p), ref[p]) {
				t.Fatalf("run %d target %s differs", i, p)
			}
		}
	}
	t.Logf("batch of %d targets regenerated 5x byte-identical", len(paths))
}

func TestEndToEndDryRunWritesNothingAndMatchesRealRun(t *testing.T) {
	dir := t.TempDir()
	// 先做一次正式生成建立 base。
	if code := run([]string{"-targets", dir, "-name", "demo", "-module", "example.com/demo"}); code != 0 {
		t.Fatalf("setup exit=%d", code)
	}
	// 本地改动一个文件并新增一个私有文件。
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("local readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LOCAL.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)

	// dry-run（模板侧也改 README -> 冲突，policy 默认 fail）：退出码 3，不落盘。
	code := run([]string{"-targets", dir, "-name", "newname", "-module", "example.com/demo", "-dry-run"})
	t.Logf("dry-run conflict exit=%d", code)
	if code != 3 {
		t.Fatalf("dry-run conflict exit = %d, want 3", code)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Fatalf("dry-run modified files")
	}

	// dry-run + theirs：退出码 0（仅预演），磁盘仍然不变。
	code = run([]string{"-targets", dir, "-name", "newname", "-module", "example.com/demo",
		"-dry-run", "-policy", "theirs"})
	t.Logf("dry-run theirs exit=%d", code)
	if code != 0 {
		t.Fatalf("dry-run theirs exit = %d, want 0", code)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Fatalf("dry-run theirs modified files")
	}

	// 正式执行 theirs 后，README 必须变成新模板内容，LOCAL.md 原样保留。
	if code := run([]string{"-targets", dir, "-name", "newname", "-module", "example.com/demo",
		"-policy", "theirs"}); code != 0 {
		t.Fatalf("real run exit=%d", code)
	}
	after := snapshot(t, dir)
	if !strings.Contains(after["README.md"], "newname") {
		t.Fatalf("real run did not apply predicted update: %q", after["README.md"])
	}
	if after["LOCAL.md"] != "mine\n" {
		t.Fatalf("local-only file changed: %q", after["LOCAL.md"])
	}
	t.Logf("dry-run wrote nothing; real run matched the preview; local file kept")
}

func TestEndToEndInvalidJobsRejected(t *testing.T) {
	dir := t.TempDir()
	code := run([]string{"-targets", dir, "-jobs", "0"})
	t.Logf("invalid -jobs exit=%d", code)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
