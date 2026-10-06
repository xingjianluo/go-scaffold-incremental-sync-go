// scaffold 从模板生成项目，并支持对既有项目做增量同步。
//
// 配置优先级（低 → 高）：内置默认值 < -config 指定的 JSON 文件 < 命令行显式选项。
// 支持一次传入多个 -targets 批量处理（-concurrency 限制并发），
// -plan 只预演不落盘，-timeout 设整体超时，Ctrl-C / SIGTERM 可中途取消。
// 退出码：0 成功；2 配置被拒绝；3 冲突；4 阶段失败；5 取消或超时；1 其它错误。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/batch"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/pipeline"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/sync"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	var (
		configPath  = fs.String("config", "", "path to JSON config file")
		name        = fs.String("name", "", "project name")
		module      = fs.String("module", "", "Go module path")
		goVersion   = fs.String("go-version", "", "Go version, e.g. 1.22")
		license     = fs.String("license", "", "license: MIT, Apache-2.0, BSD-3-Clause")
		withGit     = fs.String("with-git", "", "initialize git layout: true/false")
		features    = fs.String("features", "", "comma separated feature list")
		policy      = fs.String("policy", string(sync.PolicyFail), "conflict policy: fail, ours, theirs")
		targetsFlag = fs.String("targets", "", "comma separated target directories")
		concurrency = fs.Int("concurrency", 4, "max number of targets processed at once (>= 1)")
		timeout     = fs.Duration("timeout", 0, "overall timeout, e.g. 30s; 0 disables it")
		plan        = fs.Bool("plan", false, "dry run: report per-file decisions without touching the filesystem")
	)
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	// -with-git 只接受 true/false，其它取值必须显式拒绝而非静默当作 false。
	if *withGit != "" && *withGit != "true" && *withGit != "false" {
		fmt.Fprintf(os.Stderr, "rejected: invalid -with-git %q (want true or false)\n", *withGit)
		return 2
	}
	if *concurrency < 1 {
		fmt.Fprintf(os.Stderr, "rejected: -concurrency must be >= 1, got %d\n", *concurrency)
		return 2
	}
	if *timeout < 0 {
		fmt.Fprintf(os.Stderr, "rejected: -timeout must be non-negative, got %s\n", *timeout)
		return 2
	}

	// 组装分层来源：defaults < file < flags。
	sources := []config.Source{config.Defaults()}
	if *configPath != "" {
		sources = append(sources, &config.FileSource{Path: *configPath})
	}
	sources = append(sources, config.FlagSource(collectFlags(fs, map[string]any{
		"project_name": *name,
		"module":       *module,
		"go_version":   *goVersion,
		"license":      *license,
		"with_git":     *withGit,
		"features":     *features,
	})))

	params, err := config.Load(sources...)
	if err != nil {
		var ce *config.Error
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "rejected: %s\n", ce)
			return 2
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	pol := sync.Policy(*policy)
	if !pol.Valid() {
		fmt.Fprintf(os.Stderr, "rejected: invalid -policy %q (want fail, ours or theirs)\n", *policy)
		return 2
	}

	dirs := parseTargets(*targetsFlag)
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "error: -targets is required")
		return 1
	}

	// 渲染只做一次，结果对所有目标只读共享（渲染不含时间戳，天然确定）。
	rendered := render(params)

	// 取消来源：Ctrl-C / SIGTERM；可选再叠加整体超时。
	ctx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	runner := pipeline.NewRunner(applyPhase{})
	jobs := make([]batch.Job, len(dirs))
	logs := make([]*strings.Builder, len(dirs))
	for i, d := range dirs {
		buf := &strings.Builder{}
		logs[i] = buf
		c := &pipeline.Context{
			TargetDir: d,
			Vars: map[string]any{
				"rendered": rendered,
				"policy":   pol,
				"plan":     *plan,
				"log":      buf, // 每个目标独占一份缓冲，避免并发输出交错
				"logLabel": logLabel(*plan),
			},
		}
		jobs[i] = batch.Job{
			Name: d,
			Run:  func(jobCtx context.Context) error { return runner.Run(jobCtx, c) },
		}
	}

	mode := "apply"
	if *plan {
		mode = "plan"
	}
	fmt.Fprintf(os.Stderr, "batch: %d target(s), concurrency=%d, mode=%s\n", len(dirs), *concurrency, mode)
	report := batch.Run(ctx, jobs, batch.Options{Concurrency: *concurrency})

	// 所有目标结束后，按输入顺序整块输出每目标的判定日志与最终状态。
	for i, d := range dirs {
		if s := logs[i].String(); s != "" {
			fmt.Printf("%s:\n%s", d, s)
		}
		o := report.Outcomes[i]
		switch o.Status {
		case batch.StatusSucceeded:
			if *plan {
				fmt.Printf("%s: plan ok (no files changed)\n", d)
			} else {
				fmt.Printf("%s: ok\n", d)
			}
		case batch.StatusCancelled:
			fmt.Fprintf(os.Stderr, "%s: cancelled (%v)\n", d, o.Err)
		default:
			fmt.Fprintf(os.Stderr, "%s: %v\n", d, o.Err)
		}
	}
	fmt.Fprintf(os.Stderr, "batch summary: %d succeeded, %d failed, %d cancelled\n",
		len(report.Succeeded), len(report.Failed), len(report.Cancelled))
	for _, f := range report.Failed {
		fmt.Fprintf(os.Stderr, "  failed: %s: %v\n", f.Name, f.Err)
	}

	return exitCode(ctx, report)
}

// exitCode 按全批结果给出最严重的退出码：取消(5) > 冲突(3) / 阶段失败(4) / 其它(1)。
func exitCode(ctx context.Context, r *batch.Report) int {
	if len(r.Cancelled) > 0 || ctx.Err() != nil {
		return 5
	}
	code := 0
	for _, f := range r.Failed {
		var ce *sync.ConflictError
		var pe *pipeline.PhaseError
		switch {
		case errors.Is(f.Err, context.Canceled) || errors.Is(f.Err, context.DeadlineExceeded):
			code = maxCode(code, 5)
		case errors.As(f.Err, &ce):
			code = maxCode(code, 3)
		case errors.As(f.Err, &pe):
			code = maxCode(code, 4)
		default:
			code = maxCode(code, 1)
		}
	}
	return code
}

func maxCode(a, b int) int {
	// 4（阶段失败）比 3（冲突）更能代表整批存在非冲突性故障。
	if a == 5 || b == 5 {
		return 5
	}
	if a >= b {
		return a
	}
	return b
}

// parseTargets 拆分、修剪、去重并排序目标目录，保证批量输入确定。
func parseTargets(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := map[string]bool{}
	var dirs []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		dirs = append(dirs, p)
	}
	sort.Strings(dirs)
	return dirs
}

func logLabel(plan bool) string {
	if plan {
		return "plan"
	}
	return "apply"
}

// collectFlags 只收集用户显式设置过的选项，并把 flag 名映射为配置键，
// 保证分层合并语义正确。
func collectFlags(fs *flag.FlagSet, values map[string]any) map[string]any {
	// flag 名 -> 配置键
	keyOf := map[string]string{
		"name":       "project_name",
		"module":     "module",
		"go-version": "go_version",
		"license":    "license",
		"with-git":   "with_git",
		"features":   "features",
	}
	set := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		key, ok := keyOf[f.Name]
		if !ok {
			return
		}
		v := values[key]
		switch key {
		case "with_git":
			set[key] = v.(string) == "true"
		case "features":
			parts := []any{}
			if s := v.(string); s != "" {
				for _, p := range strings.Split(s, ",") {
					parts = append(parts, strings.TrimSpace(p))
				}
			}
			set[key] = parts
		default:
			set[key] = v
		}
	})
	return set
}

// applyPhase 把渲染结果增量同步（或在 plan 模式下只预演）到目标目录。
type applyPhase struct{}

func (applyPhase) Name() string { return "apply" }

func (applyPhase) Run(ctx context.Context, c *pipeline.Context) error {
	rendered := c.Vars["rendered"].(map[string]string)
	pol := c.Vars["policy"].(sync.Policy)
	log := c.Vars["log"].(*strings.Builder)
	label := c.Vars["logLabel"].(string)
	applier := &sync.Applier{Policy: pol}

	var (
		res *sync.Result
		err error
	)
	if c.Vars["plan"].(bool) {
		res, err = applier.Plan(ctx, c.TargetDir, rendered)
	} else {
		res, err = applier.ApplyContext(ctx, c.TargetDir, rendered)
	}
	if res != nil {
		for _, d := range res.Decisions {
			fmt.Fprintf(log, "  [%s] %s: %s (%s)\n", label, d.Path, d.Action, d.Reason)
		}
	}
	return err
}

// render 由参数确定性地渲染模板文件，不含时间戳等不稳定因素。
func render(p *config.Params) map[string]string {
	files := map[string]string{
		"go.mod": fmt.Sprintf("module %s\n\ngo %s\n", p.Module, p.GoVersion),
		"README.md": fmt.Sprintf("# %s\n\nModule: `%s`\n\nLicense: %s\n\nFeatures: %s\n",
			p.ProjectName, p.Module, p.License, strings.Join(p.Features, ", ")),
		"cmd/" + p.ProjectName + "/main.go": fmt.Sprintf(`package main

import "fmt"

func main() {
	fmt.Println(%q)
}
`, p.ProjectName),
	}
	if p.WithGit {
		files[".gitignore"] = "*.tmp\n/dist/\n"
	}
	if p.License != "" {
		files["LICENSE"] = p.License + " License\n"
	}
	return files
}
