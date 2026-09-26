package scaffold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/pipeline"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/sync"
)

func baseLayer(t *testing.T) config.Layer {
	t.Helper()
	l, err := config.Decode("test", []byte(
		`{"projectName":"demo","modulePath":"example.com/demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	return l
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
		b, _ := os.ReadFile(p)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGenerateTwiceIdentical(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Layers: []config.Layer{baseLayer(t)}, Policy: sync.PolicyFail}
	r1, err := Generate(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	snap1 := snapshot(t, dir)
	r2, err := Generate(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	snap2 := snapshot(t, dir)
	t.Logf("输入: 相同参数连续生成两次；第一次动作数=%d 第二次动作数=%d",
		len(r1.Report.Actions), len(r2.Report.Actions))
	for _, a := range r2.Report.Actions {
		t.Logf("第二次动作: %s %s", a.Path, a.Kind)
		if a.Kind != "keep" {
			t.Fatalf("重复生成应全部 keep, got %s (%s)", a.Kind, a.Path)
		}
	}
	if len(snap1) != len(snap2) {
		t.Fatal("两次生成文件数不同")
	}
	for k, v := range snap1 {
		if snap2[k] != v {
			t.Fatalf("文件 %s 在重复生成后变化", k)
		}
	}
	t.Logf("判定依据: 两次生成后 %d 个文件字节级一致", len(snap1))
}

// 模板演进：v2 修改模板内容，既有项目应被增量更新。
func tplV2() fstest.MapFS {
	return fstest.MapFS{
		"tpl/go.mod.tmpl":    &fstest.MapFile{Data: []byte("module {{.ModulePath}}\n\ngo {{.GoVersion}}\n")},
		"tpl/README.md.tmpl": &fstest.MapFile{Data: []byte("# {{.ProjectName}} v2\n")},
	}
}

func TestTemplateEvolutionSync(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Layers: []config.Layer{baseLayer(t)}, Policy: sync.PolicyFail}
	if _, err := Generate(context.Background(), dir, opts); err != nil {
		t.Fatal(err)
	}
	// 本地新增文件 + 本地修改 cmd/main.go（模板 v2 未碰它）
	if err := os.WriteFile(filepath.Join(dir, "local.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "cmd", "main.go")
	orig, _ := os.ReadFile(mainPath)
	if err := os.WriteFile(mainPath, append(orig, []byte("// local\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.TemplateFS, opts.TemplateRoot = tplV2(), "tpl"
	res, err := Generate(context.Background(), dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range res.Report.Actions {
		t.Logf("演进同步动作: %s -> %s (%s)", a.Path, a.Kind, a.Detail)
	}
	if got := readFile(t, dir, "README.md"); got != "# demo v2\n" {
		t.Fatalf("模板更新未生效: %q", got)
	}
	if got := readFile(t, dir, "local.txt"); got != "mine\n" {
		t.Fatalf("本地文件被覆盖: %q", got)
	}
	if got := readFile(t, dir, "cmd/main.go"); got != string(orig)+"// local\n" {
		t.Fatalf("本地修改未保留: %q", got)
	}
	t.Log("判定依据: 模板侧更新生效、本地新增与修改均保留")
}

func TestStageErrorClassification(t *testing.T) {
	dir := t.TempDir()
	// 非法配置：应在 config 阶段失败，错误可区分为 StageError 包 config.Error
	bad, err := config.Decode("test", []byte(
		`{"projectName":"demo","modulePath":"example.com/other"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Generate(context.Background(), dir, Options{
		Layers: []config.Layer{bad}, Policy: sync.PolicyFail})
	var se *pipeline.StageError
	var ce *config.Error
	t.Logf("输入: 矛盾配置 modulePath 末段≠projectName；错误: %v", err)
	if !errors.As(err, &se) || se.Stage != "config" {
		t.Fatalf("应为 config 阶段错误: %v", err)
	}
	if !errors.As(err, &ce) || ce.Kind != config.KindConflict {
		t.Fatalf("应归类为 conflict 配置错误: %v", err)
	}
	t.Logf("判定依据: stage=%s kind=%s", se.Stage, ce.Kind)

	// 冲突：sync 阶段失败，错误可区分为 ConflictError
	good := baseLayer(t)
	opts := Options{Layers: []config.Layer{good}, Policy: sync.PolicyFail}
	if _, err := Generate(context.Background(), dir, opts); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(dir, "README.md")
	b, _ := os.ReadFile(readme)
	if err := os.WriteFile(readme, append(b, []byte("local\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.TemplateFS, opts.TemplateRoot = tplV2(), "tpl"
	_, err = Generate(context.Background(), dir, opts)
	var cf *sync.ConflictError
	t.Logf("输入: 本地与模板同改 README.md, policy=fail；错误: %v", err)
	if !errors.As(err, &se) || se.Stage != "sync" {
		t.Fatalf("应为 sync 阶段错误: %v", err)
	}
	if !errors.As(err, &cf) {
		t.Fatalf("应归类为 ConflictError: %v", err)
	}
	t.Logf("判定依据: stage=%s 冲突路径=%v", se.Stage, cf.Paths)
}

func TestGenerateManyConcurrent(t *testing.T) {
	var targets []string
	for i := 0; i < 8; i++ {
		targets = append(targets, filepath.Join(t.TempDir(), fmt.Sprintf("p%d", i)))
	}
	opts := Options{Layers: []config.Layer{baseLayer(t)}, Policy: sync.PolicyFail}
	errs := GenerateMany(context.Background(), targets, opts)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("目标 %s 失败: %v", targets[i], err)
		}
	}
	// 并发生成的所有目标内容必须一致
	snaps := make([]map[string]string, len(targets))
	for i, tgt := range targets {
		snaps[i] = snapshot(t, tgt)
	}
	for i := 1; i < len(snaps); i++ {
		for k, v := range snaps[0] {
			if snaps[i][k] != v {
				t.Fatalf("目标间内容不一致: %s", k)
			}
		}
	}
	t.Logf("判定依据: %d 个并发目标生成结果完全一致", len(targets))
}

func readFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
