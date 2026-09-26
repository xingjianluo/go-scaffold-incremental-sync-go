package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

type stubStage struct {
	name    string
	run     func(ec *ExecContext) error
	onPanic bool
}

func (s stubStage) Name() string { return s.name }
func (s stubStage) Run(ctx context.Context, ec *ExecContext) error {
	if s.onPanic {
		panic("boom in " + s.name)
	}
	if s.run != nil {
		return s.run(ec)
	}
	return nil
}

func TestOrderAndCleanupOnFailure(t *testing.T) {
	var order []string
	var cleaned []string
	mk := func(name string, fail bool) Stage {
		return stubStage{name: name, run: func(ec *ExecContext) error {
			order = append(order, name)
			ec.Defer(func() { cleaned = append(cleaned, "cleanup-"+name) })
			if fail {
				return fmt.Errorf("%s exploded", name)
			}
			return nil
		}}
	}
	r := &Runner{Stages: []Stage{mk("a", false), mk("b", true), mk("c", false)}}
	err := r.Run(context.Background())
	var se *StageError
	if !errors.As(err, &se) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	t.Logf("输入: 三阶段 a/b(失败)/c；执行顺序=%v 清理顺序=%v", order, cleaned)
	t.Logf("归一化错误: stage=%s reason=%s panic=%v", se.Stage, se.Reason, se.Panic)
	if strings.Join(order, ",") != "a,b" {
		t.Fatalf("阶段 c 不应执行, order=%v", order)
	}
	if strings.Join(cleaned, ",") != "cleanup-b,cleanup-a" {
		t.Fatalf("清理应按 LIFO 执行, cleaned=%v", cleaned)
	}
	if se.Stage != "b" || se.Panic {
		t.Fatalf("StageError 不符: %+v", se)
	}
}

func TestPanicNormalized(t *testing.T) {
	var cleaned bool
	r := &Runner{Stages: []Stage{
		stubStage{name: "ok", run: func(ec *ExecContext) error {
			ec.Defer(func() { cleaned = true })
			return nil
		}},
		stubStage{name: "bad", onPanic: true},
	}}
	err := r.Run(context.Background())
	var se *StageError
	if !errors.As(err, &se) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	t.Logf("输入: 阶段 bad panic；归一化: stage=%s panic=%v reason=%s cleaned=%v",
		se.Stage, se.Panic, se.Reason, cleaned)
	if !se.Panic || se.Stage != "bad" {
		t.Fatalf("StageError 不符: %+v", se)
	}
	if !cleaned {
		t.Fatal("panic 后已完成阶段的清理函数未执行")
	}
}

func TestRunTargetsIsolation(t *testing.T) {
	targets := []string{"t1", "t2", "t3", "t4"}
	var counter atomic.Int64
	errs := RunTargets(context.Background(), targets, func(ctx context.Context, tgt string) error {
		counter.Add(1)
		switch tgt {
		case "t2":
			return fmt.Errorf("t2 failed")
		case "t4":
			panic("t4 panicked")
		}
		return nil
	})
	for i, e := range errs {
		t.Logf("目标 %s -> %v", targets[i], e)
	}
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("t1/t3 应成功: %v", errs)
	}
	if errs[1] == nil || !strings.Contains(errs[1].Error(), "t2 failed") {
		t.Fatalf("t2 应返回原错误: %v", errs[1])
	}
	var se *StageError
	if !errors.As(errs[3], &se) || !se.Panic {
		t.Fatalf("t4 panic 应归一化为 StageError: %T %v", errs[3], errs[3])
	}
	if counter.Load() != 4 {
		t.Fatalf("所有目标都应被执行, got %d", counter.Load())
	}
	t.Log("判定依据: 失败与 panic 目标不影响其他目标，结果与输入顺序对齐")
}
