package batch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor 轮询条件成立，避免用固定 sleep 造成竞态式用例。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func TestRunIsolationAndClassification(t *testing.T) {
	// 输入组合：成功 / 普通失败 / 冲突式错误 / context 错误，验证互不连累与分类。
	jobs := []Job{
		{Name: "zeta", Run: func(context.Context) error { return nil }},
		{Name: "alpha", Run: func(context.Context) error { return errors.New("boom") }},
		{Name: "mid", Run: func(context.Context) error { return fmt.Errorf("wrap: %w", context.DeadlineExceeded) }},
		{Name: "beta", Run: func(context.Context) error { return nil }},
	}
	sum := Run(context.Background(), jobs, Options{MaxConcurrent: 3})
	want := []struct {
		name   string
		status Status
	}{
		{"alpha", StatusFailed},
		{"beta", StatusOK},
		{"mid", StatusCancelled}, // errors.Is(DeadlineExceeded)
		{"zeta", StatusOK},
	}
	if len(sum.Results) != len(want) {
		t.Fatalf("results len = %d, want %d", len(sum.Results), len(want))
	}
	for i, w := range want {
		got := sum.Results[i]
		t.Logf("input=%s -> status=%s err=%v", got.Name, got.Status, got.Err)
		if got.Name != w.name || got.Status != w.status {
			t.Fatalf("result[%d] = (%s,%s), want (%s,%s)", i, got.Name, got.Status, w.name, w.status)
		}
	}
	if sum.Success() != 2 || sum.Failed() != 1 || sum.Cancelled() != 1 || sum.AllOK() {
		t.Fatalf("counts wrong: ok=%d failed=%d cancelled=%d allOK=%v",
			sum.Success(), sum.Failed(), sum.Cancelled(), sum.AllOK())
	}
}

func TestRunConcurrencyLimit(t *testing.T) {
	for _, max := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			const total = 16
			var inFlight, peak, started int64
			jobs := make([]Job, total)
			for i := range jobs {
				jobs[i] = Job{
					Name: fmt.Sprintf("job-%02d", i),
					Run: func(context.Context) error {
						cur := atomic.AddInt64(&inFlight, 1)
						atomic.AddInt64(&started, 1)
						for {
							p := atomic.LoadInt64(&peak)
							if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
								break
							}
						}
						time.Sleep(2 * time.Millisecond)
						atomic.AddInt64(&inFlight, -1)
						return nil
					},
				}
			}
			sum := Run(context.Background(), jobs, Options{MaxConcurrent: max})
			t.Logf("max=%d total=%d started=%d peak=%d", max, total, started, atomic.LoadInt64(&peak))
			if !sum.AllOK() {
				t.Fatalf("all jobs should succeed: %+v", sum.Results)
			}
			if started != total {
				t.Fatalf("started = %d, want %d", started, total)
			}
			if peak := atomic.LoadInt64(&peak); peak > int64(max) {
				t.Fatalf("peak concurrency = %d exceeds limit %d", peak, max)
			}
		})
	}
}

func TestRunPreCancelledStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 整批开始前就已取消
	var started int64
	jobs := make([]Job, 6)
	for i := range jobs {
		jobs[i] = Job{Name: fmt.Sprintf("j%d", i), Run: func(context.Context) error {
			atomic.AddInt64(&started, 1)
			return nil
		}}
	}
	sum := Run(ctx, jobs, Options{MaxConcurrent: 3})
	t.Logf("started=%d statuses=%v", atomic.LoadInt64(&started), statuses(sum))
	if atomic.LoadInt64(&started) != 0 {
		t.Fatalf("no job should start after cancellation, started=%d", started)
	}
	if sum.Cancelled() != 6 || sum.Success() != 0 {
		t.Fatalf("all must be cancelled: %+v", sum.Results)
	}
	for _, r := range sum.Results {
		if !errors.Is(r.Err, context.Canceled) {
			t.Fatalf("%s err = %v, want context.Canceled", r.Name, r.Err)
		}
	}
}

func TestRunCancelMidwayStopsNewWork(t *testing.T) {
	const max = 3
	ctx, cancel := context.WithCancel(context.Background())
	var started, finished int64
	release := make(chan struct{})
	mkSlow := func(name string) Job {
		return Job{Name: name, Run: func(ctx context.Context) error {
			atomic.AddInt64(&started, 1)
			select {
			case <-ctx.Done():
				atomic.AddInt64(&finished, 1)
				return ctx.Err() // 在检查点干净收尾
			case <-release:
				atomic.AddInt64(&finished, 1)
				return nil
			}
		}}
	}
	// 2 个占名额、在取消点停下的慢任务 + 1 个占名额但无视取消、自行正常
	// 收尾的在途任务（经 release 放行，结论应为 ok）+ 8 个排队任务。
	// 名称字典序保证前 3 个启动的恰好是在途任务。
	jobs := []Job{mkSlow("aaa-slow-0"), mkSlow("aaa-slow-1")}
	jobs = append(jobs, Job{Name: "aaa-self-finish", Run: func(ctx context.Context) error {
		atomic.AddInt64(&started, 1)
		<-release // 即使 ctx 取消也把手里的活干净做完
		atomic.AddInt64(&finished, 1)
		return nil
	}})
	for i := 0; i < 8; i++ {
		jobs = append(jobs, mkSlow(fmt.Sprintf("zzz-queued-%d", i)))
	}

	done := make(chan *Summary, 1)
	go func() { done <- Run(ctx, jobs, Options{MaxConcurrent: max}) }()
	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&started) == int64(max) })
	cancel()
	// 取消后：排队任务永不启动。给一点时间确认，再放行在途任务收尾。
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt64(&started); got != int64(max) {
		t.Fatalf("queued jobs started after cancel: %d > %d", got, max)
	}
	close(release)
	sum := <-done
	t.Logf("started=%d finished=%d statuses=%s", atomic.LoadInt64(&started),
		atomic.LoadInt64(&finished), statuses(sum))
	if sum.Success() != 1 {
		t.Fatalf("the self-finishing in-flight job keeps its real result: %+v", sum.Results)
	}
	if sum.Cancelled() != len(jobs)-1 {
		t.Fatalf("want %d cancelled, got %d", len(jobs)-1, sum.Cancelled())
	}
	if got := atomic.LoadInt64(&finished); got != int64(max) {
		t.Fatalf("all %d in-flight jobs must finish cleanly, finished=%d", max, got)
	}
}

func TestRunDeterministic(t *testing.T) {
	// 输入故意打乱名称顺序，重复 20 次，结果顺序与内容必须完全一致。
	jobs := []Job{
		{Name: "c", Run: func(context.Context) error { return errors.New("err-c") }},
		{Name: "a", Run: func(context.Context) error { return nil }},
		{Name: "b", Run: func(context.Context) error { return nil }},
		{Name: "a2", Run: func(context.Context) error { return context.Canceled }},
	}
	var ref []string
	for i := 0; i < 20; i++ {
		sum := Run(context.Background(), jobs, Options{MaxConcurrent: 4})
		cur := make([]string, len(sum.Results))
		for j, r := range sum.Results {
			cur[j] = fmt.Sprintf("%s:%s:%v", r.Name, r.Status, r.Err)
		}
		if i == 0 {
			ref = cur
			t.Logf("reference ordering: %s", strings.Join(cur, " | "))
		} else if strings.Join(cur, "|") != strings.Join(ref, "|") {
			t.Fatalf("run %d nondeterministic:\n%v\nvs\n%v", i, cur, ref)
		}
	}
}

func statuses(s *Summary) []string {
	out := make([]string, len(s.Results))
	for i, r := range s.Results {
		out[i] = string(r.Status)
	}
	return out
}
