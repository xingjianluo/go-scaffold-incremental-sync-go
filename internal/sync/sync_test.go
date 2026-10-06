package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// snapshot 读取目录下所有文件的确定性快照（路径 -> 内容）。
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

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func logDecisions(t *testing.T, res *Result) {
	t.Helper()
	for _, d := range res.Decisions {
		t.Logf("decision: %s -> %s (%s)", d.Path, d.Action, d.Reason)
	}
}

func TestApplyRepeatedGenerationDeterministic(t *testing.T) {
	dir := t.TempDir()
	rendered := map[string]string{
		"go.mod":    "module example.com/x\n\ngo 1.22\n",
		"README.md": "# x\n",
	}
	a := &Applier{Policy: PolicyFail}
	res1, err := a.Apply(context.Background(), dir, rendered)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	logDecisions(t, res1)
	snap1 := snapshot(t, dir)

	// 重复生成 10 次，快照与结论必须完全一致。
	var steady []Decision
	for i := 0; i < 10; i++ {
		res, err := a.Apply(context.Background(), dir, rendered)
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if snap := snapshot(t, dir); !reflect.DeepEqual(snap, snap1) {
			t.Fatalf("run %d snapshot differs:\n%v\nvs\n%v", i, snap, snap1)
		}
		// 第二次起进入稳态（全部 keep），此后结论必须逐次一致。
		if i == 0 {
			steady = res.Decisions
			for _, d := range res.Decisions {
				if d.Action != "keep" {
					t.Fatalf("second run should keep everything, got %s for %s", d.Action, d.Path)
				}
			}
		} else if !reflect.DeepEqual(res.Decisions, steady) {
			t.Fatalf("run %d decisions unstable: %v vs %v", i, res.Decisions, steady)
		}
	}
	t.Logf("10 repeated runs byte-identical, files: %v", keys(snap1))
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestApplyPreservesUserChanges(t *testing.T) {
	dir := t.TempDir()
	v1 := map[string]string{"a.txt": "template-a-v1", "b.txt": "template-b-v1"}
	a := &Applier{Policy: PolicyFail}
	if _, err := a.Apply(context.Background(), dir, v1); err != nil {
		t.Fatal(err)
	}
	// 用户改 a.txt，新增自己的笔记；模板只演进 b.txt。
	write(t, dir, "a.txt", "user-edited-a")
	write(t, dir, "notes.txt", "my own notes")
	v2 := map[string]string{"a.txt": "template-a-v1", "b.txt": "template-b-v2"}
	res, err := a.Apply(context.Background(), dir, v2)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	logDecisions(t, res)
	snap := snapshot(t, dir)
	if snap["a.txt"] != "user-edited-a" {
		t.Fatalf("user change to a.txt lost: %q", snap["a.txt"])
	}
	if snap["b.txt"] != "template-b-v2" {
		t.Fatalf("template update to b.txt not applied: %q", snap["b.txt"])
	}
	if snap["notes.txt"] != "my own notes" {
		t.Fatalf("unmanaged file notes.txt touched")
	}
	t.Logf("basis: a.txt user-changed+template-unchanged -> keep; b.txt template-changed+user-untouched -> update; notes.txt unmanaged -> untouched")
}

func TestApplyConflictPolicies(t *testing.T) {
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		a := &Applier{Policy: PolicyFail}
		if _, err := a.Apply(context.Background(), dir, map[string]string{"f.txt": "v1"}); err != nil {
			t.Fatal(err)
		}
		write(t, dir, "f.txt", "user-v2") // 用户改
		return dir
	}
	rendered := map[string]string{"f.txt": "template-v2"} // 模板也改

	t.Run("fail reports distinguishable conflict", func(t *testing.T) {
		dir := setup(t)
		res, err := (&Applier{Policy: PolicyFail}).Apply(context.Background(), dir, rendered)
		logDecisions(t, res)
		var ce *ConflictError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v (%T), want *ConflictError", err, err)
		}
		t.Logf("conflict classified: %v", ce)
		if !reflect.DeepEqual(ce.Paths, []string{"f.txt"}) {
			t.Fatalf("conflict paths = %v", ce.Paths)
		}
		// 冲突时不得覆盖用户内容，也不得固化清单。
		if got := snapshot(t, dir)["f.txt"]; got != "user-v2" {
			t.Fatalf("conflict overwrote user content: %q", got)
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestPath)); err != nil {
			t.Fatalf("manifest should not be committed on conflict: %v", err)
		}
	})

	t.Run("ours keeps project version", func(t *testing.T) {
		dir := setup(t)
		res, err := (&Applier{Policy: PolicyOurs}).Apply(context.Background(), dir, rendered)
		if err != nil {
			t.Fatal(err)
		}
		logDecisions(t, res)
		if got := snapshot(t, dir)["f.txt"]; got != "user-v2" {
			t.Fatalf("policy=ours should keep user content, got %q", got)
		}
	})

	t.Run("theirs applies template version", func(t *testing.T) {
		dir := setup(t)
		res, err := (&Applier{Policy: PolicyTheirs}).Apply(context.Background(), dir, rendered)
		if err != nil {
			t.Fatal(err)
		}
		logDecisions(t, res)
		if got := snapshot(t, dir)["f.txt"]; got != "template-v2" {
			t.Fatalf("policy=theirs should apply template content, got %q", got)
		}
	})
}

func TestApplyTemplateRemovalAndUserDeletion(t *testing.T) {
	dir := t.TempDir()
	a := &Applier{Policy: PolicyFail}
	v1 := map[string]string{"keep.txt": "k", "drop.txt": "d", "gone.txt": "g"}
	if _, err := a.Apply(context.Background(), dir, v1); err != nil {
		t.Fatal(err)
	}
	// 用户删除 gone.txt；模板移除 drop.txt。
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(context.Background(), dir, map[string]string{"keep.txt": "k"})
	if err != nil {
		t.Fatal(err)
	}
	logDecisions(t, res)
	snap := snapshot(t, dir)
	if _, ok := snap["drop.txt"]; ok {
		t.Fatalf("drop.txt should be deleted (template removed, user untouched)")
	}
	if _, ok := snap["gone.txt"]; ok {
		t.Fatalf("gone.txt should stay deleted")
	}
	// 再次应用：gone.txt 不得复活。
	res2, err := a.Apply(context.Background(), dir, map[string]string{"keep.txt": "k"})
	if err != nil {
		t.Fatal(err)
	}
	logDecisions(t, res2)
	if _, ok := snapshot(t, dir)["gone.txt"]; ok {
		t.Fatalf("user-deleted file resurrected on re-apply")
	}
	t.Logf("basis: template-removed+user-untouched -> delete; user-deleted -> keep deletion")
}

// decisionMap 把结论按路径转成 map，便于逐文件比对。
func decisionMap(res *Result) map[string]Decision {
	out := make(map[string]Decision, len(res.Decisions))
	for _, d := range res.Decisions {
		out[d.Path] = d
	}
	return out
}

// setupPreviewTree 准备一个含新增/更新/冲突/删除/未接管五类情形的目录。
func setupPreviewTree(t *testing.T) (dir string, v2 map[string]string, policy Policy) {
	t.Helper()
	dir = t.TempDir()
	v1 := map[string]string{
		"new.txt":      "will-be-created", // 实际首次不会存在：下面删除模拟
		"update.txt":   "u1",
		"conflict.txt": "c1",
		"keep.txt":     "k1",
		"delete.txt":   "d1",
	}
	a := &Applier{Policy: PolicyFail}
	if _, err := a.Apply(context.Background(), dir, v1); err != nil {
		t.Fatal(err)
	}
	// 制造各类三方状态：
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil { // 用户删除 -> skip
		t.Fatal(err)
	}
	write(t, dir, "conflict.txt", "user-conflict")                      // 用户改
	if err := os.Remove(filepath.Join(dir, "delete.txt")); err != nil { // 用户删除模板文件
		t.Fatal(err)
	}
	write(t, dir, "notes.txt", "user-only file") // 未接管 -> untouched
	v2 = map[string]string{
		"update.txt":   "u2",          // 模板改、用户未改 -> update
		"conflict.txt": "template-c2", // 双方改 -> conflict
		"keep.txt":     "k1",          // 无变化 -> keep
		"fresh.txt":    "brand-new",   // 全新文件 -> create
		// delete.txt 从模板移除，但已被用户删除 -> skip
	}
	return dir, v2, PolicyFail
}

func TestPlanIsSideEffectFree(t *testing.T) {
	dir, v2, pol := setupPreviewTree(t)
	before := snapshot(t, dir)
	a := &Applier{Policy: pol}
	res, err := a.Plan(dir, v2)
	if err == nil {
		t.Fatalf("Plan with policy=fail should report conflict, got nil; decisions=%v", res.Decisions)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("Plan err = %v, want *ConflictError", err)
	}
	logDecisions(t, res)
	after := snapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Plan modified the filesystem:\nbefore=%v\nafter=%v", before, after)
	}
	t.Logf("Plan wrote nothing; conflict paths=%v", ce.Paths)
}

func TestPlanMatchesApplyAllPolicies(t *testing.T) {
	// 三种策略 + 是否预演 组成 3x2 组合；冲突解决后预演结论必须等于实际执行。
	for _, pol := range []Policy{PolicyFail, PolicyOurs, PolicyTheirs} {
		for _, dryFirst := range []bool{true, false} {
			name := fmt.Sprintf("policy=%s/dryFirst=%v", pol, dryFirst)
			t.Run(name, func(t *testing.T) {
				dir, v2, _ := setupPreviewTree(t)
				a := &Applier{Policy: pol}
				plan, planErr := a.Plan(dir, v2)
				logDecisions(t, plan)

				applied, applyErr := a.Apply(context.Background(), dir, v2)
				logDecisions(t, applied)

				// Plan 与 Apply 必须返回同一种错误类别（fail 时都冲突，其余成功）。
				var pc, ac *ConflictError
				planConflict := errors.As(planErr, &pc)
				applyConflict := errors.As(applyErr, &ac)
				if planConflict != applyConflict {
					t.Fatalf("conflict classification differs: planErr=%v applyErr=%v", planErr, applyErr)
				}
				if !planConflict && !reflect.DeepEqual(plan.Decisions, applied.Decisions) {
					t.Fatalf("decisions differ:\nplan =%v\napply=%v", plan.Decisions, applied.Decisions)
				}
				if planConflict {
					return // fail 策略下不落盘，结论逐文件比对即可
				}

				// 对非冲突策略：Apply 之后再 Plan，应得到全 keep/skip/untouched
				// 的稳态结论（没有任何待写入动作），证明 Apply 做的正是 Plan 预告的，
				// 执行结果与下一次预演自洽。
				steady, err := a.Plan(dir, v2)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range steady.Decisions {
					if d.Action == "create" || d.Action == "update" || d.Action == "delete" {
						t.Fatalf("post-apply Plan still predicts mutation %s -> %s: %v",
							d.Path, d.Action, steady.Decisions)
					}
				}
			})
		}
	}
}

func TestPlanActionCoverage(t *testing.T) {
	dir, v2, _ := setupPreviewTree(t)
	res, _ := (&Applier{Policy: PolicyTheirs}).Plan(dir, v2)
	wantActions := map[string]string{
		"fresh.txt":    "create",
		"update.txt":   "update",
		"conflict.txt": "update", // theirs 采用模板
		"keep.txt":     "keep",
		"new.txt":      "skip", // 用户删除，尊重删除
		"delete.txt":   "skip", // 模板移除且用户已删
		"notes.txt":    "untouched",
	}
	got := decisionMap(res)
	for p, want := range wantActions {
		d, ok := got[p]
		if !ok {
			t.Fatalf("missing decision for %s in %+v", p, got)
		}
		t.Logf("input file %-13s -> action=%-9s reason=%q", p, d.Action, d.Reason)
		if d.Action != want {
			t.Fatalf("%s action=%s want %s", p, d.Action, want)
		}
	}
}

func TestApplyCancelMidwayLeavesNoTempFilesAndIsResumable(t *testing.T) {
	dir := t.TempDir()
	// 200 个文件，保证取消点一定落在写入序列中间。
	v2 := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		v2[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("content-%d", i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Applier{Policy: PolicyFail, beforeWrite: func(rel string) {
		if rel == "f100.txt" {
			cancel() // 在第 101 次写入前确定性地取消
		}
	}}
	res, err := a.Apply(ctx, dir, v2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	logDecisions(t, res)

	// 不得留下任何 .tmp 临时文件。
	var tmps []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && strings.HasSuffix(p, ".tmp") {
			tmps = append(tmps, p)
		}
		return nil
	})
	if len(tmps) != 0 {
		t.Fatalf("temp files left after cancellation: %v", tmps)
	}
	// 清单不得在取消后提交半成品：此时应不存在。
	if _, statErr := os.Stat(filepath.Join(dir, ManifestPath)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("manifest committed despite cancellation: stat err=%v", statErr)
	}

	// 取消后重新执行必须安全：完整收敛、无重复写入痕迹、无临时文件。
	a2 := &Applier{Policy: PolicyFail}
	re1, err := a2.Apply(context.Background(), dir, v2)
	if err != nil {
		t.Fatalf("resume apply: %v", err)
	}
	snap := snapshot(t, dir)
	if len(snap) != 201 { // 200 个文件 + 清单
		t.Fatalf("after resume want 201 files, got %d", len(snap))
	}
	// 第一次 resume：取消前已写的 100 个为 keep，其余 100 个为 create。
	counts := map[string]int{}
	for _, d := range re1.Decisions {
		counts[d.Action]++
	}
	t.Logf("resume decision counts: %v", counts)
	if counts["keep"] != 100 || counts["create"] != 100 {
		t.Fatalf("resume should keep 100 and create 100, got %v", counts)
	}
	// 第二次起进入全 keep 稳态，且文件系统字节级不再变化、无重复计数。
	re2, err := a2.Apply(context.Background(), dir, v2)
	if err != nil {
		t.Fatalf("second resume apply: %v", err)
	}
	for _, d := range re2.Decisions {
		if d.Action != "keep" {
			t.Fatalf("second apply must be steady all-keep, got %s for %s", d.Action, d.Path)
		}
	}
	if snapshot2 := snapshot(t, dir); !reflect.DeepEqual(snapshot2, snap) {
		t.Fatalf("filesystem changed on repeated apply after resume")
	}
	t.Logf("resume converged with %d files, steady-state decisions=%d, no .tmp", len(snap), len(re2.Decisions))
}

func TestApplyCancelledBeforeStartDoesNothing(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Applier{Policy: PolicyFail}
	res, err := a.Apply(ctx, dir, map[string]string{"a.txt": "a"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res != nil {
		t.Fatalf("no evaluation result expected before start, got %+v", res)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("cancelled-before-start created files: %v", entries)
	}
}

func TestApplyTimeoutCleansUpAndResumes(t *testing.T) {
	dir := t.TempDir()
	v2 := make(map[string]string, 150)
	for i := 0; i < 150; i++ {
		v2[fmt.Sprintf("g%03d.txt", i)] = "x"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a := &Applier{Policy: PolicyFail, beforeWrite: func(rel string) {
		if rel == "g050.txt" {
			time.Sleep(1100 * time.Millisecond) // 越过 deadline 触发超时
		}
	}}
	_, err := a.Apply(ctx, dir, v2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	var tmps []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() && strings.HasSuffix(p, ".tmp") {
			tmps = append(tmps, p)
		}
		return nil
	})
	if len(tmps) != 0 {
		t.Fatalf("temp files after timeout: %v", tmps)
	}
	if _, err := (&Applier{Policy: PolicyFail}).Apply(context.Background(), dir, v2); err != nil {
		t.Fatalf("resume after timeout: %v", err)
	}
	t.Logf("timeout cleaned temp files and resume succeeded")
}

func TestPlanPredictsActualFilesystemChanges(t *testing.T) {
	// 以 Plan 的动作为“操作手册”，逐步核验 Apply 造成的真实差异。
	dir, v2, _ := setupPreviewTree(t)
	a := &Applier{Policy: PolicyTheirs}
	plan, err := a.Plan(dir, v2)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)

	applied, err := a.Apply(context.Background(), dir, v2)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)

	// 1) 结论逐字节一致。
	if !reflect.DeepEqual(plan.Decisions, applied.Decisions) {
		t.Fatalf("plan/apply decisions differ:\nplan =%v\napply=%v", plan.Decisions, applied.Decisions)
	}

	// 2) 按动作类别核验文件系统差异。
	for _, d := range plan.Decisions {
		_, hadBefore := before[d.Path]
		got, hasAfter := after[d.Path]
		switch d.Action {
		case "create", "update":
			if hasAfter && got == v2[d.Path] {
				t.Logf("predicted %-9s %s -> on disk matches template (changed=%v)", d.Action, d.Path, !hadBefore || before[d.Path] != got)
			} else {
				t.Fatalf("predicted %s for %s but on-disk content mismatches template", d.Action, d.Path)
			}
		case "delete":
			if hasAfter {
				t.Fatalf("predicted delete for %s but file still exists", d.Path)
			}
			t.Logf("predicted delete    %s -> absent on disk", d.Path)
		case "keep", "skip", "untouched":
			want, existed := before[d.Path]
			if existed && (!hasAfter || got != want) {
				t.Fatalf("predicted %s for %s but content changed", d.Action, d.Path)
			}
			if !existed && hasAfter {
				t.Fatalf("predicted %s for previously-absent %s but it appeared", d.Action, d.Path)
			}
			t.Logf("predicted %-9s %s -> unchanged (existed=%v)", d.Action, d.Path, existed)
		case "conflict":
			t.Fatalf("conflict should not occur with policy=theirs: %s", d.Path)
		}
	}

	// 3) 反向核验：文件系统里每个发生变化的路径都在 Plan 中出现过。
	pm := decisionMap(plan)
	for p := range after {
		if b, ok := before[p]; (!ok || b != after[p]) && p != ManifestPath {
			if _, known := pm[p]; !known {
				t.Fatalf("filesystem changed %s but Plan did not predict it", p)
			}
		}
	}
	for p := range before {
		if _, still := after[p]; !still {
			if _, known := pm[p]; !known {
				t.Fatalf("filesystem removed %s but Plan did not predict it", p)
			}
		}
	}
}
