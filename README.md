# go-scaffold-incremental-sync-go

可复用的项目生成与增量更新工具：模板演进后，既有项目可以被安全、可复现地同步。
仅依赖 Go 标准库，全部能力可本地验证。

## 快速开始

```sh
go build -o bin/scaffold ./cmd/scaffold

# 生成新项目
./bin/scaffold init -target /tmp/demo -name demo -module example.com/demo

# 模板演进后同步既有项目
./bin/scaffold sync -target /tmp/demo -name demo -module example.com/demo -policy markers

# 并发处理多个目标
./bin/scaffold sync -targets /tmp/p1,/tmp/p2 -config scaffold.json
```

## 配置来源与优先级

生成参数从多层来源加载，优先级（低 → 高）：

1. 内置默认值（`config.Defaults()`）
2. 配置文件（`-config`，JSON）
3. 环境变量（`SCAFFOLD_PROJECT_NAME` / `_MODULE_PATH` / `_GO_VERSION` / `_LICENSE` / `_FEATURES` / `_LABELS`）
4. 命令行标志（`-name` / `-module` / `-go` / `-license` / `-features` / `-labels`）

合并规则：**高层只覆盖其显式设置的字段**（每层记录 `Set` 集合），
`features` 与 `labels` 为整值替换而非追加，保证结果确定、可复现。

### 严格拒绝（不静默忽略）

| 类别 | `config.Error.Kind` | 示例 |
|---|---|---|
| 未知字段 | `unknown_field` | 配置文件出现未定义键（JSON 解码 `DisallowUnknownFields`） |
| 非法取值 | `invalid_value` | 未知 license/feature、坏的项目名/Go 版本/标签键、重复 feature |
| 矛盾选项 | `conflict` | `modulePath` 末段 ≠ `projectName`；`license=none` 却声明 `labels.copyright` |
| 读取失败 | `io` | 配置文件不可读 |

### 确定性

相同输入重复执行产生字节级一致的输出：模板遍历先排序再渲染；
不使用时间戳/随机数；清单 JSON 键有序；报告动作按路径排序。

## 增量更新与冲突解决

目标目录下维护同步状态：

- `.scaffold/manifest.json`：每个被模板接管文件的基线哈希与权限位
- `.scaffold/base/`：基线内容副本（上次生成时模板侧的内容）

每次同步对每个文件做三方比较（base / 本地当前 / 模板新内容）：

| 情形 | 结论 |
|---|---|
| 本地未改、模板更新 | `update`：模板更新生效 |
| 模板未改、本地已改 | `keep`：本地修改保留 |
| 双方都未改 / 已收敛 | `keep` |
| 本地删除 | `deleted`：尊重删除（`theirs` 策略下恢复） |
| 模板移除的文件 | `stale`：本地文件保留，仅报告 |
| 双方都改且不一致 | 冲突，按 `-policy` 处理 |
| 本地已存在未接管的同名文件 | 冲突，绝不静默覆盖 |

冲突策略（`-policy`）：

- `fail`（默认）：**规划阶段**发现冲突即返回 `sync.ConflictError`（列出全部冲突路径），
  不写入任何文件；
- `theirs`：以模板侧为准覆盖；
- `ours`：保留本地内容，基线推进到模板新版本；
- `markers`：行级三方合并（LCS diff + 区域裁决），无法自动合并的区域写入
  `<<<<<<< local` / `=======` / `>>>>>>> template` 冲突标记。

所有写入均为「临时文件 + rename」原子写入；清单最后落盘。
每次同步返回 `sync.Report`，逐文件记录动作与原因（`Detail`），结论可解释。

## 扩展约定

### 自定义模板

`scaffold.Options{TemplateFS, TemplateRoot}` 可替换内嵌模板：

- 以 `.tmpl` 结尾的文件按 `text/template` 渲染（数据为 `config.Params`，
  `missingkey=error`，可用函数 `upper/lower/join`），输出去掉 `.tmpl` 后缀；
- 其余文件原样复制；`.sh` 文件自动加可执行位；
- 以 `.` 开头的目录被跳过。

### 自定义生成阶段

流水线由 `pipeline.Stage` 组成，按注册顺序严格执行（当前为
`config → render → sync`）。实现 `Stage` 接口即可插入新阶段：

```go
type Stage interface {
    Name() string
    Run(ctx context.Context, ec *pipeline.ExecContext) error
}
```

- 阶段内通过 `ec.Defer(func())` 注册资源清理，流水线结束（无论成败）按 LIFO 执行；
- 阶段返回错误或 panic 都会被归一化为 `*pipeline.StageError`
  （含阶段名、原因、`Panic` 标记），已完成阶段的清理不会遗漏；
- `pipeline.RunTargets` 并发处理多目标：结果与输入顺序对齐，
  目标间状态完全隔离，单目标失败/panic 不影响其他目标。

## 本地验证

```sh
go build ./...        # 编译
go vet ./...          # 静态检查
go test ./... -race   # 全部测试（含竞态检测）
go test ./internal/config/ -v   # 查看输入组合、合并结果与判定依据日志
```

测试覆盖：重复生成的确定性、四类冲突策略、未接管文件保护、
未知字段/非法取值/矛盾配置的拒绝分类、阶段失败与 panic 归一化、
并发多目标隔离。单测日志打印输入组合、合并结果与判定依据。
