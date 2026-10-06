// scaffold 从模板生成项目，并支持对既有项目做增量同步。
//
// 配置优先级（低 → 高）：内置默认值 < -config 指定的 JSON 文件 < 命令行显式选项。
// 支持一次传入多个目标（逗号分隔）批量执行，可用 -jobs 限制并发，
// -timeout 设整体超时，Ctrl-C（SIGINT）主动取消，-dry-run 只预演不落盘。
//
// 退出码：0 全部成功；1 普通错误；2 配置被拒绝；3 冲突；4 阶段失败；
// 5 批量被取消/超时。
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
	stdsync "sync"
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
		configPath = fs.String("config", "", "path to JSON config file")
		name       = fs.String("name", "", "project name")
		module     = fs.String("module", "", "Go module path")
		goVersion  = fs.String("go-version", "", "Go version, e.g. 1.22")
		license    = fs.String("license", "", "license: MIT, Apache-2.0, BSD-3-Clause")
		withGit    = fs.String("with-git", "", "initialize git layout: true/false")
		features   = fs.String("features", "", "comma separated feature list")
		policy     = fs.String("policy", string(sync.PolicyFail), "conflict policy: fail, ours, theirs")
		targets    = fs.String("targets", "", "comma separated target directories")
		jobs       = fs.Int("jobs", 4, "max number of targets processed concurrently")
		timeout    = fs.Duration("timeout", 0, "overall timeout, e.g. 30s; 0 means no timeout")
		dryRun     = fs.Bool("dry-run", false, "preview changes without writing anything")
	)
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	// -with-git 只接受 true/false，其它取值必须显式拒绝而非静默当作 false。
	if *withGit != "" && *withGit != "true" && *withGit != "false" {
		fmt.Fprintf(os.Stderr, "rejected: invalid -with-git %q (want true or false)\n", *withGit)
		return 2
	}
	if *jobs < 1 {
		fmt.Fprintf(os.Stderr, "rejected: invalid -jobs %d (must be >= 1)\n", *jobs)
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
	if *targets == "" {
		fmt.Fprintln(os.Stderr, "error: -targets is required")
		return 1
	}

	// 超时与主动取消共用同一个 ctx：触发后不启动新目标，在执行中的目标
	// 于落盘检查点干净收尾。
	ctx, cancel := context.WithCancel(context.Background())
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), *timeout)
	}
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			cancel()
		}
	}()

	rendered := render(params)
	dirs := splitTargets(*targets)

	// 输出加锁，保证多个目标并发时每个目标的行不会交错。
	var outMu stdsync.Mutex
	emit := func(format string, args ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Printf(format, args...)
	}

	runner := pipeline.NewRunner(applyPhase{dryRun: *dryRun, emit: emit})
	jobList := make([]batch.Job, len(dirs))
	for i, d := range dirs {
		d := d
		jobList[i] = batch.Job{
			Name: d,
			Run: func(ctx context.Context) error {
				c := &pipeline.Context{
					TargetDir: d,
					Vars:      map[string]any{"rendered": rendered, "policy": pol},
				}
				return runner.Run(ctx, c)
			},
		}
	}

	summary := batch.Run(ctx, jobList, batch.Options{MaxConcurrent: *jobs})

	code := 0
	for _, r := range summary.Results {
		switch r.Status {
		case batch.StatusOK:
			emit("%s: %s\n", r.Name, dryRunLabel(*dryRun))
		case batch.StatusCancelled:
			emit("%s: cancelled (%v)\n", r.Name, r.Err)
			code = 5
		case batch.StatusFailed:
			emit("%s: %v\n", r.Name, r.Err)
			var ce *sync.ConflictError
			var pe *pipeline.PhaseError
			switch {
			case errors.As(r.Err, &ce):
				if code == 0 || code == 1 {
					code = 3
				}
			case errors.As(r.Err, &pe):
				if code == 0 || code == 1 {
					code = 4
				}
			default:
				if code == 0 {
					code = 1
				}
			}
		}
	}
	emit("batch summary: total=%d ok=%d failed=%d cancelled=%d\n",
		len(summary.Results), summary.Success(), summary.Failed(), summary.Cancelled())
	return code
}

func dryRunLabel(dry bool) string {
	if dry {
		return "dry-run ok"
	}
	return "ok"
}

// splitTargets 拆分逗号分隔的目标并去空白、去空项，按字典序返回。
func splitTargets(raw string) []string {
	parts := strings.Split(raw, ",")
	dirs := make([]string, 0, len(parts))
	for _, p := range parts {
		if d := strings.TrimSpace(p); d != "" {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	return dirs
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

// applyPhase 把渲染结果增量同步（或预演）到目标目录。
type applyPhase struct {
	dryRun bool
	emit   func(format string, args ...any)
}

func (applyPhase) Name() string { return "apply" }

func (p applyPhase) Run(ctx context.Context, c *pipeline.Context) error {
	rendered := c.Vars["rendered"].(map[string]string)
	pol := c.Vars["policy"].(sync.Policy)
	a := &sync.Applier{Policy: pol}
	var res *sync.Result
	var err error
	if p.dryRun {
		res, err = a.Plan(c.TargetDir, rendered)
	} else {
		res, err = a.Apply(ctx, c.TargetDir, rendered)
	}
	if res != nil {
		for _, d := range res.Decisions {
			p.emit("  [%s] %s: %s (%s)\n", c.TargetDir, d.Path, d.Action, d.Reason)
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
