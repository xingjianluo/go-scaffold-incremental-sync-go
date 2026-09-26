// Package pipeline 按确定顺序执行生成阶段，并把失败归一化为 PhaseError。
//
// Runner 构造后不可变，可安全地并发用于多个目标；
// 每个目标使用独立的 Context，阶段之间不得共享可变量。
package pipeline

import (
	"context"
	"fmt"
	"sync"
)

// Phase 是一个生成阶段或扩展。实现必须保证 Run 可重入且不使用全局可变量，
// 以便同一 Runner 并发处理多个目标时不发生数据竞争。
type Phase interface {
	Name() string
	Run(ctx context.Context, c *Context) error
}

// Context 在阶段之间传递共享状态，每个目标独占一份。
type Context struct {
	TargetDir string
	Vars      map[string]any
}

// PhaseError 是带阶段标识与原因的归一化错误。
type PhaseError struct {
	Phase  string
	Reason string
	Panic  bool // 为 true 表示阶段发生了 panic
	Err    error
}

func (e *PhaseError) Error() string {
	if e.Panic {
		return fmt.Sprintf("phase %q panicked: %s", e.Phase, e.Reason)
	}
	return fmt.Sprintf("phase %q failed: %s", e.Phase, e.Reason)
}

func (e *PhaseError) Unwrap() error { return e.Err }

// Runner 按注册顺序执行阶段。
type Runner struct {
	phases []Phase
}

// NewRunner 按给定顺序注册阶段，执行顺序与注册顺序一致。
func NewRunner(phases ...Phase) *Runner {
	cp := make([]Phase, len(phases))
	copy(cp, phases)
	return &Runner{phases: cp}
}

// PhaseNames 按执行顺序返回阶段名，用于日志与测试断言。
func (r *Runner) PhaseNames() []string {
	names := make([]string, len(r.phases))
	for i, p := range r.phases {
		names[i] = p.Name()
	}
	return names
}

// Run 顺序执行所有阶段；任一阶段失败或 panic 即停止并返回 *PhaseError。
// panic 会被恢复并转换为 Panic=true 的 PhaseError，已完成阶段的结果不受影响。
func (r *Runner) Run(ctx context.Context, c *Context) (err error) {
	for _, p := range r.phases {
		if e := ctx.Err(); e != nil {
			return &PhaseError{Phase: p.Name(), Reason: "context cancelled: " + e.Error(), Err: e}
		}
		if e := runOne(ctx, p, c); e != nil {
			return e
		}
	}
	return nil
}

// runOne 执行单个阶段并恢复 panic。
func runOne(ctx context.Context, p Phase, c *Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PhaseError{
				Phase:  p.Name(),
				Reason: fmt.Sprintf("%v", r),
				Panic:  true,
			}
		}
	}()
	if e := p.Run(ctx, c); e != nil {
		return &PhaseError{Phase: p.Name(), Reason: e.Error(), Err: e}
	}
	return nil
}

// RunTargets 并发处理多个目标，每个目标一个 goroutine、一份独立 Context。
// 返回与 targets 等长、按序对应的错误切片；目标之间互不影响。
func (r *Runner) RunTargets(ctx context.Context, targets []*Context) []error {
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, c := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = r.Run(ctx, c) // 每个 goroutine 只写自己的下标，无数据竞争
		}()
	}
	wg.Wait()
	return errs
}
