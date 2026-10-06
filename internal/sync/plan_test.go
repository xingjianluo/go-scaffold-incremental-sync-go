package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// setupV1 在临时目录生成 v1，返回该目录。
func setupV1(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	v1 := map[string]string{"a.txt": "A-v1", "b.txt": "B-v1", "drop.txt": "D-v1"}
	if _, err := (&Applier{Policy: PolicyFail}).Apply(dir, v1); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cloneTree 把 src 完整复制到一个全新的临时目录（含清单），用于构造
// 两个完全一致的初始状态：一个给 Plan，一个给 Apply。
func cloneTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
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

// TestPlanIsReadOnly 预演不得创建/修改/删除任何文件，也不得写清单。
func TestPlanIsReadOnly(t *testing.T) {
	src := setupV1(t)
	write(t, src, "a.txt", "user-edited-a")
	write(t, src, "notes.txt", "my own notes")
	v2 := map[string]string{"a.txt": "A-v1", "b.txt": "B-v2"}

	before := snapshot(t, src)
	res, err := (&Applier{Policy: PolicyOurs}).Plan(context.Background(), src, v2)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	after := snapshot(t, src)
	logDecisions(t, res)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Plan mutated the filesystem:\nbefore=%v\nafter=%v", before, after)
	}
	t.Logf("input: user-edited a.txt + unmanaged notes.txt, template evolves b.txt; Plan left files untouched")

	// 在尚不存在的目录上预演也不得创建目录/文件/清单。
	empty := filepath.Join(t.TempDir(), "does-not-exist")
	res2, err := (&Applier{Policy: PolicyFail}).Plan(context.Background(), empty, map[string]string{"x.txt": "x"})
	if err != nil {
		t.Fatalf("plan on missing dir: %v", err)
	}
	logDecisions(t, res2)
	if _, err := os.Stat(empty); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Plan created the target directory: %v", err)
	}
}

// TestPlanConflictReported 双方同时改动时 Plan 返回 ConflictError，
// 但 Result 仍包含全部判定，且不动任何文件。
func TestPlanConflictReported(t *testing.T) {
	src := setupV1(t)
	write(t, src, "a.txt", "user-a")                          // 用户改 a
	v2 := map[string]string{"a.txt": "A-v2", "b.txt": "B-v2"} // 模板也改 a、只模板改 b
	res, err := (&Applier{Policy: PolicyFail}).Plan(context.Background(), src, v2)
	logDecisions(t, res)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("Plan err = %v, want ConflictError", err)
	}
	if !reflect.DeepEqual(ce.Paths, []string{"a.txt"}) {
		t.Fatalf("conflict paths = %v", ce.Paths)
	}
	if got := snapshot(t, src)["a.txt"]; got != "user-a" {
		t.Fatalf("Plan mutated conflicting file: %q", got)
	}
}

// TestPlanMatchesApply 对同一文件系统状态，预演结论必须与真正执行的
// 逐文件结论（含 Action/Reason）与错误完全一致。
func TestPlanMatchesApply(t *testing.T) {
	scenarios := []struct {
		name    string
		policy  Policy
		prepare func(t *testing.T, dir string) // 在 v1 基础上制造本地状态
		v2      map[string]string
	}{
		{
			name:   "create-update-keep-skip",
			policy: PolicyFail,
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "user-a")          // 用户改 a（模板未改 a）-> keep
				remove(t, filepath.Join(dir, "drop.txt")) // 模板移除 drop：用户先删 -> skip
			},
			v2: map[string]string{"a.txt": "A-v1", "b.txt": "B-v2", "new.txt": "N"},
		},
		{
			name:   "both-changed-ours",
			policy: PolicyOurs,
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "user-a")
			},
			v2: map[string]string{"a.txt": "A-v2", "b.txt": "B-v2"},
		},
		{
			name:   "both-changed-theirs",
			policy: PolicyTheirs,
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "user-a")
			},
			v2: map[string]string{"a.txt": "A-v2", "b.txt": "B-v2"},
		},
		{
			name:   "both-changed-fail",
			policy: PolicyFail,
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "a.txt", "user-a")
			},
			v2: map[string]string{"a.txt": "A-v2", "b.txt": "B-v2"},
		},
		{
			name:   "user-deleted-respected",
			policy: PolicyFail,
			prepare: func(t *testing.T, dir string) {
				remove(t, filepath.Join(dir, "a.txt"))
			},
			v2: map[string]string{"a.txt": "A-v1", "b.txt": "B-v1"},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			origin := setupV1(t)
			if sc.prepare != nil {
				sc.prepare(t, origin)
			}
			planDir := cloneTree(t, origin)
			applyDir := cloneTree(t, origin)

			planRes, planErr := (&Applier{Policy: sc.policy}).Plan(context.Background(), planDir, sc.v2)
			applyRes, applyErr := (&Applier{Policy: sc.policy}).ApplyContext(context.Background(), applyDir, sc.v2)
			t.Logf("input: policy=%s v2=%v", sc.policy, sc.v2)
			for _, d := range planRes.Decisions {
				t.Logf("plan  decision: %s -> %s (%s)", d.Path, d.Action, d.Reason)
			}
			for _, d := range applyRes.Decisions {
				t.Logf("apply decision: %s -> %s (%s)", d.Path, d.Action, d.Reason)
			}
			if !reflect.DeepEqual(planRes.Decisions, applyRes.Decisions) {
				t.Fatalf("plan/apply decisions differ:\nplan=%v\napply=%v",
					planRes.Decisions, applyRes.Decisions)
			}
			if (planErr == nil) != (applyErr == nil) ||
				(planErr != nil && planErr.Error() != applyErr.Error()) {
				t.Fatalf("plan/apply error differ: %v vs %v", planErr, applyErr)
			}
			// 预演目录必须保持初始状态（Plan 只读）。
			if got := snapshot(t, planDir); !reflect.DeepEqual(got, snapshot(t, origin)) {
				t.Fatalf("Plan directory diverged from origin:\n%v\n%v", got, snapshot(t, origin))
			}
		})
	}
}

// TestApplyContextCancellationCleanup 预先取消后不得写入任何文件；
// 重新执行幂等收敛到完整结果，不遗留 .tmp，不出现重复计数。
func TestApplyContextCancellationCleanup(t *testing.T) {
	dir := t.TempDir()
	rendered := map[string]string{}
	for i := 0; i < 20; i++ {
		rendered[fmt.Sprintf("file-%02d.txt", i)] = fmt.Sprintf("content-%d", i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消：不应执行任何写入。
	res, err := (&Applier{Policy: PolicyFail}).ApplyContext(ctx, dir, rendered)
	t.Logf("pre-cancelled apply: err=%v decisions=%d", err, len(res.Decisions))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("pre-cancelled apply created files: %v", entries)
	}

	// 重跑（不取消）必须幂等收敛：20 个文件全部 create 成功，无遗留 .tmp。
	res2, err := (&Applier{Policy: PolicyFail}).ApplyContext(context.Background(), dir, rendered)
	if err != nil {
		t.Fatalf("re-apply after cancel: %v", err)
	}
	if len(res2.Decisions) != 20 {
		t.Fatalf("decisions = %d, want 20", len(res2.Decisions))
	}
	assertNoTmpFiles(t, dir)
	finalSnap := snapshot(t, dir)
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("file-%02d.txt", i)
		if finalSnap[p] != fmt.Sprintf("content-%d", i) {
			t.Fatalf("file %s content wrong after re-apply: %q", p, finalSnap[p])
		}
	}

	// 再来一遍进入稳态：全部 keep，结论稳定。
	res3, err := (&Applier{Policy: PolicyFail}).ApplyContext(context.Background(), dir, rendered)
	if err != nil {
		t.Fatalf("third apply: %v", err)
	}
	for _, d := range res3.Decisions {
		if d.Action != "keep" {
			t.Fatalf("steady state decision %s = %s, want keep", d.Path, d.Action)
		}
	}
	assertNoTmpFiles(t, dir)
	t.Logf("cancellation cleanup + idempotent re-apply verified, 20 files")
}

func remove(t *testing.T, p string) {
	t.Helper()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func assertNoTmpFiles(t *testing.T, dir string) {
	t.Helper()
	var tmps []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".tmp" {
			tmps = append(tmps, p)
		}
		return nil
	})
	if len(tmps) != 0 {
		t.Fatalf("leftover temp files: %v", tmps)
	}
}
