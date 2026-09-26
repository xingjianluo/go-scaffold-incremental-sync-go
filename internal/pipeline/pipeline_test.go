package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recordPhase 记录执行轨迹到共享日志（调用方保证串行或自行加锁）。
type recordPhase struct {
	name string
	log  *[]string
}

func (p recordPhase) Name() string { return p.name }
func (p recordPhase) Run(_ context.Context, c *Context) error {
	*p.log = append(*p.log, p.name)
	return nil
}

type failPhase struct{ name string }

func (p failPhase) Name() string { return p.name }
func (p failPhase) Run(_ context.Context, _ *Context) error {
	return fmt.Errorf("boom in %s", p.name)
}

type panicPhase struct{ name string }

func (p panicPhase) Name() string { return p.name }
func (p panicPhase) Run(_ context.Context, _ *Context) error {
	panic("kaboom")
}

func TestRunOrderAndFailureNormalization(t *testing.T) {
	var log []string
	r := NewRunner(
		recordPhase{"first", &log},
		failPhase{"second"},
		recordPhase{"third", &log},
	)
	err := r.Run(context.Background(), &Context{})
	t.Logf("executed phases: %v", log)
	if strings.Join(log, ",") != "first" {
		t.Fatalf("phase after failure must not run, log=%v", log)
	}
	var pe *PhaseError
	if !errors.As(err, &pe) {
		t.Fatalf("err type = %T, want *PhaseError", err)
	}
	t.Logf("normalized error: phase=%q reason=%q panic=%v", pe.Phase, pe.Reason, pe.Panic)
	if pe.Phase != "second" || pe.Panic {
		t.Fatalf("unexpected PhaseError: %+v", pe)
	}
	if !strings.Contains(pe.Reason, "boom in second") {
		t.Fatalf("reason lost original cause: %q", pe.Reason)
	}
}

func TestRunPanicRecovery(t *testing.T) {
	var log []string
	r := NewRunner(
		recordPhase{"ok", &log},
		panicPhase{"bad"},
		recordPhase{"never", &log},
	)
	err := r.Run(context.Background(), &Context{})
	var pe *PhaseError
	if !errors.As(err, &pe) {
		t.Fatalf("err type = %T, want *PhaseError", err)
	}
	t.Logf("panic normalized: phase=%q reason=%q panic=%v, executed=%v", pe.Phase, pe.Reason, pe.Panic, log)
	if !pe.Panic || pe.Phase != "bad" {
		t.Fatalf("panic not normalized correctly: %+v", pe)
	}
	if strings.Join(log, ",") != "ok" {
		t.Fatalf("completed phases should be exactly [ok], got %v", log)
	}
}

// touchPhase 只写自己 Context 的 Vars，验证并发目标互不污染。
type touchPhase struct{ name string }

func (p touchPhase) Name() string { return p.name }
func (p touchPhase) Run(_ context.Context, c *Context) error {
	for i := 0; i < 100; i++ {
		c.Vars[p.name] = i // 只写目标自己的 Context
	}
	return nil
}

func TestRunTargetsConcurrentIsolation(t *testing.T) {
	r := NewRunner(touchPhase{"a"}, touchPhase{"b"})
	const n = 32
	targets := make([]*Context, n)
	for i := range targets {
		targets[i] = &Context{TargetDir: fmt.Sprintf("dir-%d", i), Vars: map[string]any{}}
	}
	errs := r.RunTargets(context.Background(), targets)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("target %d: %v", i, err)
		}
	}
	// 每个目标的 Vars 只应包含本阶段写入的键，且互不影响。
	for i, c := range targets {
		if c.Vars["a"] != 99 || c.Vars["b"] != 99 || len(c.Vars) != 2 {
			t.Fatalf("target %d polluted: %v", i, c.Vars)
		}
	}
	t.Logf("%d targets completed, each with isolated Vars", n)
}

func TestRunTargetsPartialFailure(t *testing.T) {
	r := NewRunner(&condFailPhase{})
	targets := []*Context{
		{TargetDir: "good-1", Vars: map[string]any{"fail": false}},
		{TargetDir: "bad", Vars: map[string]any{"fail": true}},
		{TargetDir: "good-2", Vars: map[string]any{"fail": false}},
	}
	errs := r.RunTargets(context.Background(), targets)
	for i, err := range errs {
		t.Logf("target %s err=%v", targets[i].TargetDir, err)
	}
	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("good targets affected by bad target: %v", errs)
	}
	var pe *PhaseError
	if !errors.As(errs[1], &pe) || pe.Phase != "cond" {
		t.Fatalf("bad target error not normalized: %v", errs[1])
	}
}

type condFailPhase struct{ mu sync.Mutex }

func (p *condFailPhase) Name() string { return "cond" }
func (p *condFailPhase) Run(_ context.Context, c *Context) error {
	if c.Vars["fail"] == true {
		return errors.New("injected failure")
	}
	return nil
}
