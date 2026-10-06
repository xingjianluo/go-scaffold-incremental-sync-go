// Package batch 以可配置的并发上限批量执行多个目标，目标之间互不影响。
//
// 每个目标一个 Job；Run 返回与输入等长、按序对应的 Outcome，并额外给出
// 成功/失败/取消的确定性汇总。取消（ctx.Done）后不再启动新的目标，
// 已经开始的目标自行收尾后结束。
package batch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Status 是单个目标的最终状态。
type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled" // 未启动或执行中收到取消
)

// Job 是一个待处理目标：Name 用于汇总展示，Run 执行该目标的全部工作。
// Run 必须自行保证取消时干净收尾；panic 会被 Run 恢复为失败，不影响其它目标。
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Outcome 是单个目标的执行结果，顺序与传入的 Job 一致。
type Outcome struct {
	Name   string
	Status Status
	Err    error
}

// FailedTarget 记录一个失败目标及其原因。
type FailedTarget struct {
	Name string
	Err  error
}

// Report 是整批执行的确定性汇总。
type Report struct {
	Outcomes  []Outcome // 与输入 Job 等长、按序对应
	Succeeded []string  // 成功目标名，按字典序
	Failed    []FailedTarget
	Cancelled []string // 被取消（未启动或执行中取消）的目标名，按字典序
}

// Options 控制批量执行行为。
type Options struct {
	// Concurrency 是同时处理目标数的上限，必须 >= 1。
	Concurrency int
}

type indexedJob struct {
	idx int
	job Job
}

// Run 以 opts.Concurrency 为上限并发执行全部 jobs，返回确定性汇总。
// 返回值永不为 nil；某个 Job 的错误或 panic 只影响它自己的 Outcome。
//
// 取消语义：ctx 取消后调度器不再派发新目标（这些目标记为 cancelled）；
// 已经在执行的目标继续跑到自行收尾，其返回的错误若由取消引起也记为
// cancelled，否则按实际错误记为 failed。
func Run(ctx context.Context, jobs []Job, opts Options) *Report {
	if opts.Concurrency < 1 {
		panic(fmt.Sprintf("batch: Concurrency must be >= 1, got %d", opts.Concurrency))
	}
	outcomes := make([]Outcome, len(jobs))

	// 固定大小的 worker 池：池规模即并发上限。调度器是 work 的唯一发送方，
	// 取消时停止发送并关闭 work，worker 排空后退出；避免了“执行完归还令牌时
	// 调度器已退出”而永久阻塞的问题。
	work := make(chan indexedJob)
	var wg sync.WaitGroup
	for w := 0; w < opts.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				outcomes[j.idx] = execute(ctx, j.job)
			}
		}()
	}

dispatch:
	for i, job := range jobs {
		select {
		case <-ctx.Done():
			// 取消后不再启动新目标：当前及其后目标全部记为 cancelled。
			for j := i; j < len(jobs); j++ {
				outcomes[j] = Outcome{Name: jobs[j].Name, Status: StatusCancelled, Err: ctx.Err()}
			}
			break dispatch
		case work <- indexedJob{idx: i, job: job}:
		}
	}
	close(work)
	wg.Wait()
	return summarize(jobs, outcomes)
}

// execute 执行单个目标，恢复 panic 并按错误起因归类状态。
func execute(ctx context.Context, j Job) (out Outcome) {
	out.Name = j.Name
	defer func() {
		if r := recover(); r != nil {
			out = Outcome{
				Name:   j.Name,
				Status: StatusFailed,
				Err:    fmt.Errorf("job %q panicked: %v", j.Name, r),
			}
		}
	}()
	err := j.Run(ctx)
	switch {
	case err == nil:
		out.Status = StatusSucceeded
	case isCausedByCancellation(ctx, err):
		out.Status = StatusCancelled
		out.Err = err
	default:
		out.Status = StatusFailed
		out.Err = err
	}
	return out
}

// isCausedByCancellation 判断错误是否由 ctx 取消/超时引起。
// 兼容被 fmt.Errorf("...: %w", ctx.Err()) 或 PhaseError 包过一层的情况。
func isCausedByCancellation(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ce := ctx.Err(); ce != nil && errors.Is(err, ce) {
		return true
	}
	return false
}

// summarize 从按序的 outcomes 汇总出字典序确定的成功/失败/取消列表。
func summarize(jobs []Job, outcomes []Outcome) *Report {
	r := &Report{
		Outcomes:  outcomes,
		Succeeded: []string{},
		Failed:    []FailedTarget{},
		Cancelled: []string{},
	}
	for i, o := range outcomes {
		name := o.Name
		if name == "" && i < len(jobs) {
			name = jobs[i].Name
		}
		switch o.Status {
		case StatusSucceeded:
			r.Succeeded = append(r.Succeeded, name)
		case StatusFailed:
			r.Failed = append(r.Failed, FailedTarget{Name: name, Err: o.Err})
		case StatusCancelled:
			r.Cancelled = append(r.Cancelled, name)
		}
	}
	sort.Strings(r.Succeeded)
	sort.Strings(r.Cancelled)
	sort.Slice(r.Failed, func(i, j int) bool { return r.Failed[i].Name < r.Failed[j].Name })
	return r
}
