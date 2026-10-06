package batch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestRunFailureIsolationAndConcurrency 输入组合：
// 5 个目标 [good-1, bad, good-2, good-3, good-4]，并发上限 2，
// 其中 bad 返回注入错误。判定依据：只有 bad=failed，其余成功，
// 且观测到的同时在跑数绝不超过上限。
func TestRunFailureIsolationAndConcurrency(t *testing.T) {
	const limit = 2
	var (
		mu      sync.Mutex
		running int
		peak    int
	)
	names := []string{"good-1", "bad", "good-2", "good-3", "good-4"}
	jobs := make([]Job, len(names))
	for i, n := range names {
		n := n
		jobs[i] = Job{
			Name: n,
			Run: func(_ context.Context) error {
				mu.Lock()
				running++
				if running > peak {
					peak = running
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				running--
				mu.Unlock()
				if n == "bad" {
					return errors.New("injected failure")
				}
				return nil
			},
		}
	}

	r := Run(context.Background(), jobs, Options{Concurrency: limit})
	t.Logf("input: targets=%v concurrency=%d", names, limit)
	for _, o := range r.Outcomes {
		t.Logf("outcome: %s -> %s err=%v", o.Name, o.Status, o.Err)
	}
	if peak > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", peak, limit)
	}
	t.Logf("observed peak concurrency = %d (limit %d)", peak, limit)
	if got := statusAt(r, "bad"); got != StatusFailed {
		t.Fatalf("bad target status = %s, want failed", got)
	}
	for _, n := range []string{"good-1", "good-2", "good-3", "good-4"} {
		if got := statusAt(r, n); got != StatusSucceeded {
			t.Fatalf("%s status = %s, want succeeded", n, got)
		}
	}
	if !reflect.DeepEqual(r.Succeeded, []string{"good-1", "good-2", "good-3", "good-4"}) {
		t.Fatalf("Succeeded not sorted/deterministic: %v", r.Succeeded)
	}
	if len(r.Failed) != 1 || r.Failed[0].Name != "bad" {
		t.Fatalf("Failed summary wrong: %+v", r.Failed)
	}
	if len(r.Cancelled) != 0 {
		t.Fatalf("unexpected cancellations: %v", r.Cancelled)
	}
}

// TestRunReportDeterministic 相同输入重复跑两遍，汇总必须完全一致。
func TestRunReportDeterministic(t *testing.T) {
	mk := func() []Job {
		return []Job{
			{Name: "c", Run: func(_ context.Context) error { return nil }},
			{Name: "a", Run: func(_ context.Context) error { return errors.New("x") }},
			{Name: "b", Run: func(_ context.Context) error { return nil }},
		}
	}
	r1 := Run(context.Background(), mk(), Options{Concurrency: 3})
	r2 := Run(context.Background(), mk(), Options{Concurrency: 3})
	t.Logf("run1: succeeded=%v failed=%v cancelled=%v", r1.Succeeded, namesOf(r1.Failed), r1.Cancelled)
	if !reflect.DeepEqual(r1.Succeeded, r2.Succeeded) ||
		!reflect.DeepEqual(r1.Cancelled, r2.Cancelled) ||
		!reflect.DeepEqual(namesOf(r1.Failed), namesOf(r2.Failed)) {
		t.Fatalf("report not deterministic:\n%+v\n%+v", r1, r2)
	}
	// 输出顺序保持输入顺序，汇总列表按字典序。
	if r1.Outcomes[0].Name != "c" {
		t.Fatalf("outcome order must follow input order, got %q", r1.Outcomes[0].Name)
	}
	if !reflect.DeepEqual(r1.Succeeded, []string{"b", "c"}) {
		t.Fatalf("succeeded summary = %v", r1.Succeeded)
	}
}

// TestRunCancellationStopsStartingNew 输入组合：4 个目标共享栅栏，
// 先放行 2 个（并发上限 2），随即取消 ctx；被栅栏挡住的目标不得启动，
// 已启动目标收尾后归类为 cancelled（其错误由 context.Canceled 引起）。
func TestRunCancellationStopsStartingNew(t *testing.T) {
	const total = 4
	started := make(chan string, total)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	jobs := make([]Job, total)
	for i := range jobs {
		n := fmt.Sprintf("target-%d", i)
		jobs[i] = Job{
			Name: n,
			Run: func(ctx context.Context) error {
				started <- n
				<-release // 模拟耗时工作，等待测试统一放行收尾
				return ctx.Err()
			},
		}
	}

	done := make(chan *Report, 1)
	go func() {
		done <- Run(ctx, jobs, Options{Concurrency: 2})
	}()

	got := []string{<-started, <-started} // 恰好 2 个启动
	t.Logf("started before cancel: %v", got)
	cancel()
	close(release) // 已启动的目标干净收尾

	r := <-done
	t.Logf("input: %d targets, concurrency=2, cancel after 2 started", total)
	for _, o := range r.Outcomes {
		t.Logf("outcome: %s -> %s err=%v", o.Name, o.Status, o.Err)
	}
	if len(r.Cancelled) != total {
		t.Fatalf("cancelled = %v, want all %d targets classified cancelled", r.Cancelled, total)
	}
	if !reflect.DeepEqual(r.Cancelled, []string{"target-0", "target-1", "target-2", "target-3"}) {
		t.Fatalf("cancelled summary not sorted: %v", r.Cancelled)
	}
	if len(r.Succeeded) != 0 || len(r.Failed) != 0 {
		t.Fatalf("no success/failure expected after cancel, got %+v / %+v", r.Succeeded, r.Failed)
	}
}

// TestRunTimeoutDeadlineExceeded 整体超时触发后，在跑的目标报
// context.DeadlineExceeded，归类为 cancelled 而不是 failed。
func TestRunTimeoutDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	jobs := []Job{{
		Name: "slow",
		Run: func(ctx context.Context) error {
			<-ctx.Done()
			return fmt.Errorf("apply stopped: %w", ctx.Err())
		},
	}}
	r := Run(ctx, jobs, Options{Concurrency: 1})
	t.Logf("outcome: %s -> %s err=%v", r.Outcomes[0].Name, r.Outcomes[0].Status, r.Outcomes[0].Err)
	if r.Outcomes[0].Status != StatusCancelled {
		t.Fatalf("status = %s, want cancelled", r.Outcomes[0].Status)
	}
}

// TestRunJobPanicIsolated 某个目标 panic 不连累其它目标，且被归一化为 failed。
func TestRunJobPanicIsolated(t *testing.T) {
	jobs := []Job{
		{Name: "ok", Run: func(_ context.Context) error { return nil }},
		{Name: "panicky", Run: func(_ context.Context) error { panic("kaboom") }},
		{Name: "ok2", Run: func(_ context.Context) error { return nil }},
	}
	r := Run(context.Background(), jobs, Options{Concurrency: 3})
	for _, o := range r.Outcomes {
		t.Logf("outcome: %s -> %s err=%v", o.Name, o.Status, o.Err)
	}
	if statusAt(r, "panicky") != StatusFailed ||
		statusAt(r, "ok") != StatusSucceeded ||
		statusAt(r, "ok2") != StatusSucceeded {
		t.Fatalf("panic isolation broken: %+v", r.Outcomes)
	}
	if r.Failed[0].Err == nil || r.Failed[0].Err.Error() == "" {
		t.Fatalf("panic must carry a descriptive error")
	}
}

func statusAt(r *Report, name string) Status {
	for _, o := range r.Outcomes {
		if o.Name == name {
			return o.Status
		}
	}
	return ""
}

func namesOf(fs []FailedTarget) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}
