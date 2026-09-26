// Package render 将模板确定性地渲染为目标文件集。
//
// 确定性保证：文件遍历结果先排序再渲染；不使用时间戳、随机数或
// 任何环境相关数据；相同 (模板, 参数) 输入必然产生字节级一致的输出。
package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
)

// File 表示一个待写入的文件。
type File struct {
	Path    string // 相对目标根目录的斜杠路径
	Content []byte
	Mode    uint32
}

// Renderer 按参数渲染模板。FS 为模板所在的文件系统，Root 为模板根目录。
type Renderer struct {
	FS   fs.FS
	Root string
}

// FuncMap 是模板可用的纯函数集（全部无副作用、结果确定）。
var FuncMap = template.FuncMap{
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"join":  strings.Join,
}

// Render 渲染全部模板文件，输出按路径排序，顺序确定。
// 以 .tmpl 结尾的文件作为 text/template 渲染并去掉该后缀，
// 其余文件原样复制。
func (r *Renderer) Render(params config.Params) ([]File, error) {
	var paths []string
	err := fs.WalkDir(r.FS, r.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != r.Root {
				return fs.SkipDir
			}
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("render: walk %s: %w", r.Root, err)
	}
	sort.Strings(paths) // 关键：排序保证遍历顺序不影响输出

	files := make([]File, 0, len(paths))
	for _, fp := range paths {
		rel := strings.TrimPrefix(strings.TrimPrefix(fp, r.Root), "/")
		data, err := fs.ReadFile(r.FS, fp)
		if err != nil {
			return nil, fmt.Errorf("render: read %s: %w", fp, err)
		}
		mode := uint32(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if strings.HasSuffix(rel, ".tmpl") {
			out, err := renderOne(rel, data, params)
			if err != nil {
				return nil, err
			}
			files = append(files, File{Path: strings.TrimSuffix(rel, ".tmpl"), Content: out, Mode: mode})
			continue
		}
		files = append(files, File{Path: rel, Content: data, Mode: mode})
	}
	return files, nil
}

func renderOne(name string, text []byte, p config.Params) ([]byte, error) {
	tpl, err := template.New(name).Funcs(FuncMap).Option("missingkey=error").Parse(string(text))
	if err != nil {
		return nil, fmt.Errorf("render: parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render: execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
