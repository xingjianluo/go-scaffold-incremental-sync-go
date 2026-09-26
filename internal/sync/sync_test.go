package sync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/render"
)

func filesV1() []render.File {
	return []render.File{
		{Path: "go.mod", Content: []byte("module example.com/demo\n\ngo 1.25.0\n"), Mode: 0o644},
		{Path: "main.go", Content: []byte("package main\n\n// v1\n"), Mode: 0o644},
	}
}

func filesV2() []render.File {
	return []render.File{
		{Path: "go.mod", Content: []byte("module example.com/demo\n\ngo 1.26.0\n"), Mode: 0o644},
		{Path: "main.go", Content: []byte("package main\n\n// v2\n"), Mode: 0o644},
	}
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRegenerateDeterministic(t *testing.T) {
	dir := t.TempDir()
	s := &Syncer{Dir: dir, Policy: PolicyFail}
	r1, err := s.Apply(filesV1())
	if err != nil {
		t.Fatal(err)
	}
	snap1 := snapshot(t, dir)
	// 相同输入重复生成：第二次应全部 keep，目录字节级一致
	r2, err := s.Apply(filesV1())
	if err != nil {
		t.Fatal(err)
	}
	snap2 := snapshot(t, dir)
	for _, a := range r2.Actions {
		t.Logf("二次生成动作: %s %s %s", a.Path, a.Kind, a.Detail)
		if a.Kind != "keep" {
			t.Fatalf("重复生成应全部 keep, got %s for %s", a.Kind, a.Path)
		}
	}
	t.Logf("判定依据: 两次生成后目录快照一致=%v", equalSnap(snap1, snap2))
	if !equalSnap(snap1, snap2) {
		t.Fatal("重复生成后目录内容发生变化")
	}
	_ = r1
}

func TestTemplateUpdateAppliesAndLocalKept(t *testing.T) {
	dir := t.TempDir()
	s := &Syncer{Dir: dir, Policy: PolicyFail}
	if _, err := s.Apply(filesV1()); err != nil {
		t.Fatal(err)
	}
	// 本地新建非模板文件 + 修改 main.go（与模板 v2 改到同一处）
	if err := os.WriteFile(filepath.Join(dir, "custom.go"), []byte("package main // mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\n// local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模板升级到 v2：go.mod 本地未动应更新；main.go 双方都改 → fail 策略报错
	_, err := s.Apply(filesV2())
	var ce *ConflictError
	t.Logf("输入: 模板v2与本地同时改 main.go；策略=fail；错误: %v", err)
	if !errors.As(err, &ce) {
		t.Fatalf("应返回 ConflictError, got %T %v", err, err)
	}
	if len(ce.Paths) != 1 || ce.Paths[0] != "main.go" {
		t.Fatalf("冲突路径不符: %v", ce.Paths)
	}
	// fail 策略下不得写入任何文件：go.mod 应仍是 v1
	if got := readFile(t, dir, "go.mod"); got != "module example.com/demo\n\ngo 1.25.0\n" {
		t.Fatalf("fail 策略下 go.mod 被改动: %q", got)
	}
	t.Log("判定依据: fail 策略冲突时不写入任何文件，go.mod 保持 v1")
	// 非模板文件必须保留
	if got := readFile(t, dir, "custom.go"); got != "package main // mine\n" {
		t.Fatalf("非模板文件被破坏: %q", got)
	}
}

func TestConflictPolicies(t *testing.T) {
	localMain := "package main\n\n// local change\n"
	cases := []struct {
		policy   Policy
		wantMain string
		wantKind string
	}{
		{PolicyTheirs, "package main\n\n// v2\n", "conflict-theirs"},
		{PolicyOurs, localMain, "conflict-ours"},
		{PolicyMarkers, "package main\n\n<<<<<<< local\n// local change\n=======\n// v2\n>>>>>>> template\n", "conflict-markers"},
	}
	for _, tc := range cases {
		t.Run(string(tc.policy), func(t *testing.T) {
			dir := t.TempDir()
			s := &Syncer{Dir: dir, Policy: tc.policy}
			if _, err := s.Apply(filesV1()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(localMain), 0o644); err != nil {
				t.Fatal(err)
			}
			rep, err := s.Apply(filesV2())
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range rep.Actions {
				t.Logf("动作: %s -> %s (%s)", a.Path, a.Kind, a.Detail)
			}
			if got := readFile(t, dir, "main.go"); got != tc.wantMain {
				t.Fatalf("main.go=%q, want %q", got, tc.wantMain)
			}
			// go.mod 本地未动，模板更新必须生效
			if got := readFile(t, dir, "go.mod"); got != "module example.com/demo\n\ngo 1.26.0\n" {
				t.Fatalf("go.mod 未更新: %q", got)
			}
			// 再次同步（模板未变）：冲突已按策略落盘，不应再报冲突
			rep2, err := s.Apply(filesV2())
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("判定依据: 策略=%s 二次同步无冲突=%v", tc.policy, !rep2.HasConflicts())
			if rep2.HasConflicts() {
				t.Fatal("冲突解决后再次同步不应再有冲突")
			}
		})
	}
}

func TestUntrackedLocalFileNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	// 目录中已有未接管的 main.go，模板首次也想生成 main.go
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // preexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Syncer{Dir: dir, Policy: PolicyFail}
	_, err := s.Apply(filesV1())
	var ce *ConflictError
	t.Logf("输入: 本地已存在未接管文件；策略=fail；错误: %v", err)
	if !errors.As(err, &ce) {
		t.Fatalf("应返回 ConflictError, got %v", err)
	}
	if got := readFile(t, dir, "main.go"); got != "package main // preexisting\n" {
		t.Fatalf("本地文件被静默覆盖: %q", got)
	}
}

func TestUnknownPolicyRejected(t *testing.T) {
	s := &Syncer{Dir: t.TempDir(), Policy: "bogus"}
	_, err := s.Apply(filesV1())
	t.Logf("输入: 非法策略 bogus；错误: %v", err)
	if err == nil {
		t.Fatal("非法策略应报错")
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalSnap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
