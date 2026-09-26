// Package scaffold 把配置加载、模板渲染、增量同步组装为
// 按确定顺序执行的生成流水线，并内嵌默认模板。
package scaffold

import (
	"context"
	"embed"
	"io/fs"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/pipeline"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/render"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/sync"
)

//go:embed all:templates/default
var defaultTemplates embed.FS

// Options 是一次生成/同步的全部输入。
type Options struct {
	Layers []config.Layer // 配置层（低→高）；Generate 会自动在最底层补默认值
	Policy sync.Policy    // 冲突处理策略
	// TemplateFS 与 TemplateRoot 指定自定义模板；为零值时使用内嵌默认模板。
	TemplateFS   fs.FS
	TemplateRoot string
}

// Result 是一次生成的结论。
type Result struct {
	Params config.Params
	Report *sync.Report
}

// runState 在阶段间共享（单流水线内串行访问，无需加锁）。
type runState struct {
	opts   Options
	target string
	params config.Params
	files  []render.File
	report *sync.Report
}

// Generate 对 target 目录执行完整生成/同步流水线。
// 阶段顺序固定：config → render → sync。
// 任一阶段失败返回 *pipeline.StageError，可区分阶段与原因。
func Generate(ctx context.Context, target string, opts Options) (*Result, error) {
	st := &runState{opts: opts, target: target}
	runner := &pipeline.Runner{Stages: []pipeline.Stage{
		configStage{st},
		renderStage{st},
		syncStage{st},
	}}
	if err := runner.Run(ctx); err != nil {
		return nil, err
	}
	return &Result{Params: st.params, Report: st.report}, nil
}

// GenerateMany 并发处理多个目标目录，返回与 targets 对齐的错误切片。
// 各目标的配置与状态完全隔离。
func GenerateMany(ctx context.Context, targets []string, opts Options) []error {
	return pipeline.RunTargets(ctx, targets, func(ctx context.Context, target string) error {
		_, err := Generate(ctx, target, opts)
		return err
	})
}

// ---- 阶段实现 ----

type configStage struct{ st *runState }

func (configStage) Name() string { return "config" }

func (s configStage) Run(_ context.Context, ec *pipeline.ExecContext) error {
	layers := append([]config.Layer{config.Defaults()}, s.st.opts.Layers...)
	p, err := config.Load(layers...)
	if err != nil {
		return err
	}
	s.st.params = p
	return nil
}

type renderStage struct{ st *runState }

func (renderStage) Name() string { return "render" }

func (s renderStage) Run(_ context.Context, ec *pipeline.ExecContext) error {
	fsys, root := s.st.opts.TemplateFS, s.st.opts.TemplateRoot
	if fsys == nil {
		fsys, root = defaultTemplates, "templates/default"
	}
	files, err := (&render.Renderer{FS: fsys, Root: root}).Render(s.st.params)
	if err != nil {
		return err
	}
	s.st.files = files
	return nil
}

type syncStage struct{ st *runState }

func (syncStage) Name() string { return "sync" }

func (s syncStage) Run(_ context.Context, ec *pipeline.ExecContext) error {
	rep, err := (&sync.Syncer{Dir: s.st.target, Policy: s.st.opts.Policy}).Apply(s.st.files)
	if err != nil {
		return err
	}
	s.st.report = rep
	return nil
}
