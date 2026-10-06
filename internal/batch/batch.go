// Package batch 提供带并发上限、可取消、可复现的多目标批量执行编排。
//
// 每个目标独立执行，单个目标出错不连累其它目标；结果按目标名（目标目录）
// 字典序返回，相同输入下批量结果与每个目标的结论完全确定。
package batch

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Status 是单个目标的批量执行结论分类。
type Status string

const (
	StatusOK        Status = "ok"        // 目标成功
	StatusFailed    Status = "failed"    // 目标执行失败（非取消类错误）
	StatusCancelled Status = "cancelled" // 整批取消/超时，目标未能正常完成
)

// Job 是一个待处理目标。
type Job struct {
	Name string                      // 目标名（通常为目录），用于排序与结果展示
	Run  func(context.Context) error // 目标自身的执行函数
}

// Result 是单个目标的执行结果，按 Job.Name 字典序出现在 Summary.Results 中。
type Result struct {
	Name   string
	Status Status
	Err    error
}

// Options 控制批量执行行为。
type Options struct {
	// MaxConcurrent 限制同时执行的目标数；<=0 时视为 1。
	MaxConcurrent int
}

// Summary 是整批执行的汇总。
type Summary struct {
	Results []Result
}

// Success 返回成功目标数。
func (s *Summary) Success() int {
	n := 0
	for _, r := range s.Results {
		if r.Status == StatusOK {
			n++
		}
	}
	return n
}

// Failed 返回失败目标数。
func (s *Summary) Failed() int {
	n := 0
	for _, r := range s.Results {
		if r.Status == StatusFailed {
			n++
		}
	}
	return n
}

// Cancelled 返回被取消/超时的目标数。
func (s *Summary) Cancelled() int {
	n := 0
	for _, r := range s.Results {
		if r.Status == StatusCancelled {
			n++
		}
	}
	return n
}

// AllOK 报告整批是否全部成功。
func (s *Summary) AllOK() bool {
	return s.Success() == len(s.Results)
}

// classify 把目标错误归一化为取消或失败两类。
func classify(err error) Status {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return StatusCancelled
	default:
		return StatusFailed
	}
}

// Run 按名称字典序调度 jobs，最多 opts.MaxConcurrent 个目标同时执行。
// ctx 被取消或超时后不再启动新目标；每个目标的成败互不影响。
// 返回的 Summary.Results 按名称字典序排列，保证相同输入输出完全一致。
//
// 取消语义：ctx 取消后，尚未开始的目标与在取消检查点停下的目标标记为
// cancelled；已经在执行且自行正常收尾的目标仍记为 ok/failed——它的工作
// 已经完成，取消不应篡改真实结论。调用方（如 sync.Applier）负责在 ctx
// 取消时不提交半成品，因此已完成的目标本身就是干净的。
func Run(ctx context.Context, jobs []Job, opts Options) *Summary {
	max := opts.MaxConcurrent
	if max <= 0 {
		max = 1
	}

	// 拷贝并按名称字典序排序，调度顺序确定；重复名称稳定保序。
	ordered := make([]Job, len(jobs))
	copy(ordered, jobs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Name < ordered[j].Name
	})

	results := make([]Result, len(ordered))
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup

	for i, job := range ordered {
		// 已取消：剩余目标一律不再启动。
		if err := ctx.Err(); err != nil {
			for j := i; j < len(ordered); j++ {
				results[j] = Result{Name: ordered[j].Name, Status: StatusCancelled, Err: err}
			}
			break
		}
		// 申领名额时也监听取消：取消/超时后绝不启动新工作。
		select {
		case <-ctx.Done():
			for j := i; j < len(ordered); j++ {
				results[j] = Result{Name: ordered[j].Name, Status: StatusCancelled, Err: ctx.Err()}
			}
			wg.Wait()
			return &Summary{Results: results}
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(i int, job Job) {
			defer wg.Done()
			defer func() { <-sem }()
			err := job.Run(ctx)
			results[i] = Result{Name: job.Name, Status: classify(err), Err: err}
		}(i, job)
	}
	wg.Wait()

	return &Summary{Results: results}
}
