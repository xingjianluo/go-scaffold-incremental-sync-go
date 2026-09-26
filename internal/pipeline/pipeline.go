// Package pipeline 按确定顺序执行生成阶段并归一化错误。
//
// 语义保证：
//   - 阶段严格按注册顺序执行，任一阶段失败即停止；
//   - 阶段 panic 被恢复并归一化为 StageError（Panic=true）；
//   - 通过 ExecContext.Defer 注册的清理函数在流水线结束时
//     按 LIFO 执行，失败不会遗留未释放资源；
//   - RunTargets 并发处理多个目标，各目标状态完全隔离。
package pipeline

import (
	"context"
	"fmt"
	"sync"
)

// Stage 是一个生成阶段。
type Stage interface {
	Name() string
	Run(ctx context.Context, ec *ExecContext) error
}

// ExecContext 在阶段间传递状态，并收集资源清理函数。
type ExecContext struct {
	Values map[string]any

	mu       sync.Mutex
	cleanups []func()
}

// NewExecContext 创建空的执行上下文。
func NewExecContext() *ExecContext {
	return &ExecContext{Values: map[string]any{}}
}

// Defer 注册清理函数，流水线结束时按 LIFO 执行（无论成败）。
func (ec *ExecContext) Defer(cleanup func()) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.cleanups = append(ec.cleanups, cleanup)
}

// runCleanups 按 LIFO 执行全部清理函数，单个清理 panic 不影响其余。
func (ec *ExecContext) runCleanups() {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for i := len(ec.cleanups) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			ec.cleanups[i]()
		}()
	}
	ec.cleanups = nil
}

// StageError 是带阶段标识的归一化错误。
type StageError struct {
	Stage  string
	Reason string
	Panic  bool
	Err    error
}

func (e *StageError) Error() string {
	if e.Panic {
		return fmt.Sprintf("pipeline: stage %q panicked: %s", e.Stage, e.Reason)
	}
	return fmt.Sprintf("pipeline: stage %q failed: %s: %v", e.Stage, e.Reason, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

// Runner 按序执行阶段。
type Runner struct {
	Stages []Stage
}

// Run 顺序执行所有阶段。失败或 panic 时已完成阶段注册的清理
// 函数仍会被执行，错误归一化为 *StageError。
func (r *Runner) Run(ctx context.Context) error {
	ec := NewExecContext()
	defer ec.runCleanups()
	for _, st := range r.Stages {
		if err := ctx.Err(); err != nil {
			return &StageError{Stage: st.Name(), Reason: "context done", Err: err}
		}
		if err := runStage(ctx, st, ec); err != nil {
			return err
		}
	}
	return nil
}

// runStage 执行单个阶段并恢复 panic。
func runStage(ctx context.Context, st Stage, ec *ExecContext) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &StageError{Stage: st.Name(), Reason: fmt.Sprintf("%v", r), Panic: true}
		}
	}()
	if err := st.Run(ctx, ec); err != nil {
		return &StageError{Stage: st.Name(), Reason: "run error", Err: err}
	}
	return nil
}

// RunTargets 并发处理多个目标，返回与 targets 对齐的错误切片。
// 每个目标在独立 goroutine 中执行 run，目标间不共享任何状态；
// 单个目标的 panic 被归一化为错误，不影响其他目标。
func RunTargets(ctx context.Context, targets []string, run func(ctx context.Context, target string) error) []error {
	errs := make([]error, len(targets)) // 每个下标仅由对应 goroutine 写入，无数据竞争
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = &StageError{Stage: tgt, Reason: fmt.Sprintf("%v", r), Panic: true}
				}
			}()
			errs[i] = run(ctx, tgt)
		}()
	}
	wg.Wait()
	return errs
}
