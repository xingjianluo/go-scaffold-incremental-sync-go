package render

import (
	"bytes"
	"testing"
	"testing/fstest"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"tpl/go.mod.tmpl":       &fstest.MapFile{Data: []byte("module {{.ModulePath}}\n\ngo {{.GoVersion}}\n")},
		"tpl/cmd/main.go.tmpl":  &fstest.MapFile{Data: []byte("package main\n// {{.ProjectName}} license={{.License}}\n")},
		"tpl/README.md.tmpl":    &fstest.MapFile{Data: []byte("# {{.ProjectName}}\nfeatures: {{join .Features \",\"}}\n")},
		"tpl/scripts/build.sh":  &fstest.MapFile{Data: []byte("#!/bin/sh\necho build\n")},
		"tpl/.hidden/skip.tmpl": &fstest.MapFile{Data: []byte("should be skipped")},
	}
}

func testParams() config.Params {
	return config.Params{
		ProjectName: "demo",
		ModulePath:  "example.com/demo",
		GoVersion:   "1.25.0",
		License:     "MIT",
		Features:    []string{"http", "metrics"},
		Labels:      map[string]string{},
	}
}

func TestRenderDeterministic(t *testing.T) {
	r := &Renderer{FS: testFS(), Root: "tpl"}
	p := testParams()
	first, err := r.Render(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("输入参数: %+v", p)
	for i := 0; i < 20; i++ {
		got, err := r.Render(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(first) {
			t.Fatalf("第 %d 次渲染文件数不同", i)
		}
		for j := range got {
			if got[j].Path != first[j].Path || !bytes.Equal(got[j].Content, first[j].Content) || got[j].Mode != first[j].Mode {
				t.Fatalf("第 %d 次渲染第 %d 个文件不一致", i, j)
			}
		}
	}
	t.Logf("判定依据: 20 次渲染输出字节级一致，文件顺序: %v", paths(first))
	byPath := map[string]File{}
	for _, f := range first {
		byPath[f.Path] = f
	}
	if got := string(byPath["go.mod"].Content); got != "module example.com/demo\n\ngo 1.25.0\n" {
		t.Fatalf("go.mod 渲染结果不符: %q", got)
	}
	// 隐藏目录被跳过
	for _, f := range first {
		if f.Path == ".hidden/skip" {
			t.Fatalf("隐藏目录不应被渲染")
		}
	}
	// .sh 文件带可执行位
	for _, f := range first {
		if f.Path == "scripts/build.sh" && f.Mode != 0o755 {
			t.Fatalf("build.sh mode=%o, want 755", f.Mode)
		}
	}
}

func TestRenderMissingKey(t *testing.T) {
	fsys := fstest.MapFS{"tpl/a.tmpl": &fstest.MapFile{Data: []byte("{{.NoSuchField}}")}}
	r := &Renderer{FS: fsys, Root: "tpl"}
	_, err := r.Render(testParams())
	t.Logf("输入: 模板引用不存在字段；判定依据: missingkey=error 必须报错: %v", err)
	if err == nil {
		t.Fatal("应报错的模板未报错")
	}
}

func paths(fs []File) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}
