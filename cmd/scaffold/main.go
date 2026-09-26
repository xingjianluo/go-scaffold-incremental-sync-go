// scaffold 从模板生成项目，并支持对既有项目做增量同步。
//
// 配置优先级（低 → 高）：内置默认值 < -config 指定的 JSON 文件 < 命令行显式选项。
// 退出码：0 成功；2 配置被拒绝；3 冲突；4 阶段失败；1 其它错误。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

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
	)
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	// -with-git 只接受 true/false，其它取值必须显式拒绝而非静默当作 false。
	if *withGit != "" && *withGit != "true" && *withGit != "false" {
		fmt.Fprintf(os.Stderr, "rejected: invalid -with-git %q (want true or false)\n", *withGit)
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

	rendered := render(params)
	dirs := strings.Split(*targets, ",")
	sort.Strings(dirs) // 固定处理顺序，日志确定
	ctxs := make([]*pipeline.Context, len(dirs))
	for i, d := range dirs {
		ctxs[i] = &pipeline.Context{
			TargetDir: d,
			Vars:      map[string]any{"rendered": rendered, "policy": pol},
		}
	}

	runner := pipeline.NewRunner(applyPhase{})
	errs := runner.RunTargets(context.Background(), ctxs)
	code := 0
	for i, e := range errs {
		if e == nil {
			fmt.Printf("%s: ok\n", dirs[i])
			continue
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", dirs[i], e)
		var ce *sync.ConflictError
		var pe *pipeline.PhaseError
		switch {
		case errors.As(e, &ce):
			code = 3
		case errors.As(e, &pe):
			code = 4
		default:
			code = 1
		}
	}
	return code
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

// applyPhase 把渲染结果增量同步到目标目录。
type applyPhase struct{}

func (applyPhase) Name() string { return "apply" }

func (applyPhase) Run(_ context.Context, c *pipeline.Context) error {
	rendered := c.Vars["rendered"].(map[string]string)
	pol := c.Vars["policy"].(sync.Policy)
	res, err := (&sync.Applier{Policy: pol}).Apply(c.TargetDir, rendered)
	if res != nil {
		for _, d := range res.Decisions {
			fmt.Printf("  %s: %s (%s)\n", d.Path, d.Action, d.Reason)
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
