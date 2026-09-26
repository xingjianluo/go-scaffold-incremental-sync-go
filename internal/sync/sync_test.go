package sync

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
	res1, err := a.Apply(dir, rendered)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	logDecisions(t, res1)
	snap1 := snapshot(t, dir)

	// 重复生成 10 次，快照与结论必须完全一致。
	var steady []Decision
	for i := 0; i < 10; i++ {
		res, err := a.Apply(dir, rendered)
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
	if _, err := a.Apply(dir, v1); err != nil {
		t.Fatal(err)
	}
	// 用户改 a.txt，新增自己的笔记；模板只演进 b.txt。
	write(t, dir, "a.txt", "user-edited-a")
	write(t, dir, "notes.txt", "my own notes")
	v2 := map[string]string{"a.txt": "template-a-v1", "b.txt": "template-b-v2"}
	res, err := a.Apply(dir, v2)
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
		if _, err := a.Apply(dir, map[string]string{"f.txt": "v1"}); err != nil {
			t.Fatal(err)
		}
		write(t, dir, "f.txt", "user-v2") // 用户改
		return dir
	}
	rendered := map[string]string{"f.txt": "template-v2"} // 模板也改

	t.Run("fail reports distinguishable conflict", func(t *testing.T) {
		dir := setup(t)
		res, err := (&Applier{Policy: PolicyFail}).Apply(dir, rendered)
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
		res, err := (&Applier{Policy: PolicyOurs}).Apply(dir, rendered)
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
		res, err := (&Applier{Policy: PolicyTheirs}).Apply(dir, rendered)
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
	if _, err := a.Apply(dir, v1); err != nil {
		t.Fatal(err)
	}
	// 用户删除 gone.txt；模板移除 drop.txt。
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(dir, map[string]string{"keep.txt": "k"})
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
	res2, err := a.Apply(dir, map[string]string{"keep.txt": "k"})
	if err != nil {
		t.Fatal(err)
	}
	logDecisions(t, res2)
	if _, ok := snapshot(t, dir)["gone.txt"]; ok {
		t.Fatalf("user-deleted file resurrected on re-apply")
	}
	t.Logf("basis: template-removed+user-untouched -> delete; user-deleted -> keep deletion")
}
