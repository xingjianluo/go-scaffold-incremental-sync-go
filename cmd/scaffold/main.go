// scaffold 是项目生成与增量同步的命令行入口。
//
// 用法：
//
//	scaffold init -target DIR [选项]   生成新项目
//	scaffold sync -target DIR [选项]   同步已存在项目（模板演进后增量更新）
//
// 配置优先级（低→高）：内置默认值 < -config 文件 < SCAFFOLD_* 环境变量 < 命令行标志。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/config"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/pipeline"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/scaffold"
	"github.com/xingjianluo/go-scaffold-incremental-sync-go/internal/sync"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		var se *pipeline.StageError
		var ce *config.Error
		var cf *sync.ConflictError
		switch {
		case errors.As(err, &se):
			fmt.Fprintf(os.Stderr, "stage error: %v\n", se)
		case errors.As(err, &ce):
			fmt.Fprintf(os.Stderr, "config error (%s): %v\n", ce.Kind, ce)
		case errors.As(err, &cf):
			fmt.Fprintf(os.Stderr, "conflict: %v\n", cf)
		default:
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "init" && args[0] != "sync") {
		return fmt.Errorf("usage: scaffold <init|sync> -target DIR [-config FILE] [-policy POLICY] [param flags]")
	}
	fs := config.NewFlagSet(args[0])
	configPath := fs.String("config", "", "path to JSON config file")
	target := fs.String("target", "", "target directory")
	targets := fs.String("targets", "", "comma separated target directories (concurrent)")
	policy := fs.String("policy", string(sync.PolicyFail), "conflict policy: "+policyList())
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	var layers []config.Layer
	if *configPath != "" {
		l, err := config.DecodeFile(*configPath)
		if err != nil {
			return err
		}
		layers = append(layers, l)
	}
	layers = append(layers, config.FromEnv("SCAFFOLD", nil), config.FromFlags(fs))

	opts := scaffold.Options{Layers: layers, Policy: sync.Policy(*policy)}
	ctx := context.Background()

	var dirs []string
	if *targets != "" {
		dirs = strings.Split(*targets, ",")
	} else if *target != "" {
		dirs = []string{*target}
	} else {
		return fmt.Errorf("-target or -targets is required")
	}

	errs := scaffold.GenerateMany(ctx, dirs, opts)
	failed := false
	for i, err := range errs {
		if err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "[%s] %v\n", dirs[i], err)
		} else {
			fmt.Printf("[%s] ok\n", dirs[i])
		}
	}
	if failed {
		return fmt.Errorf("some targets failed")
	}
	return nil
}

func policyList() string {
	ss := make([]string, len(sync.Policies))
	for i, p := range sync.Policies {
		ss[i] = string(p)
	}
	return strings.Join(ss, "|")
}
