# go-scaffold-incremental-sync-go

可复用的项目生成与增量更新工具。模板演进后，既有项目可以被安全、可复现地重新同步。

## 构建与本地验证

```sh
go build ./...          # 编译
go test ./...           # 单元测试
go test ./... -race     # 含数据竞争检测（并发目标场景）
go test ./... -v        # 查看输入组合、合并结果与判定依据日志
```

CLI 冒烟验证：

```sh
go run ./cmd/scaffold -targets /tmp/demo -name myapp -module example.com/myapp
go run ./cmd/scaffold -targets /tmp/demo -name myapp -module example.com/myapp  # 重复执行输出完全一致
```

退出码：`0` 成功；`2` 配置被拒绝；`3` 冲突（`-policy=fail` 时双方同时改动）；`4` 阶段失败或 panic；`1` 其它错误。

## 配置优先级

生成参数从三层来源加载，**低 → 高**依次为：

1. 内置默认值（`config.Defaults()`）
2. `-config <path>` 指定的 JSON 文件
3. 命令行显式给出的选项（只收集用户真正设置过的 flag）

高层来源只覆盖它显式设置的字段，其余字段保留低层取值。

### 严格拒绝（不会被静默忽略）

所有拒绝都以 `*config.Error` 返回，`Kind` 字段可区分原因：

| Kind | 含义 | 示例 |
|---|---|---|
| `unknown_field` | 出现了未定义的字段 | 配置文件中写了 `nonsense: 1` |
| `invalid_value` | 字段取值非法 | 空项目名、非法 module 字符、`go_version: latest`、未知 license、`-with-git maybe` |
| `conflict` | 选项互相矛盾 | `features` 含 `git` 但 `with_git=false` |
| `source_failure` | 来源读取失败 | 配置文件不存在或 JSON 损坏 |

### 确定性

相同输入必然产生完全相同的输出：字段按字典序校验、`features` 排序后存储、
渲染结果不含时间戳、文件按路径字典序写入、清单（`.scaffold-manifest.json`）
的 JSON 键有序。测试中对同一输入重复执行 10–20 次断言字节级一致。

## 增量更新与冲突解决

每次生成后，工具把各文件内容的 SHA-256 记录到项目内的 `.scaffold-manifest.json`，
作为下次同步的三方合并基准：

- **base**：清单记录的上次生成内容
- **ours**：项目目录当前内容（用户可能改过）
- **theirs**：本次模板渲染结果

判定规则（每个文件的结论都会以 `Decision{Path, Action, Reason}` 记录并可解释）：

| 用户侧 | 模板侧 | 结论 |
|---|---|---|
| 未改 | 改了 | `update`：安全应用模板更新 |
| 改了 | 未改 | `keep`：保留用户改动 |
| 都改了 | 都改了 | 按 `-policy` 处理（见下） |
| 未改 | 已移除该文件 | `delete`：跟随模板删除 |
| 改了 | 已移除该文件 | `keep`：保留用户改动 |
| 用户删除了文件 | — | `skip`：尊重删除，不会复活 |
| 未被模板接管的文件 | — | 永不触碰 |

冲突策略（`-policy`）：

- `fail`（默认）：报 `*sync.ConflictError`（含全部冲突路径），**不覆盖用户内容、
  不提交清单**，退出码 3；
- `ours`：保留项目侧版本；
- `theirs`：采用模板侧版本。

文件写入先落临时文件再原子改名，避免中途失败留下损坏内容。

## 扩展约定（生成阶段）

生成流程由 `pipeline.Runner` 按**注册顺序**串行执行若干 `Phase`：

```go
type Phase interface {
    Name() string
    Run(ctx context.Context, c *Context) error
}
```

约定：

- 阶段按 `NewRunner(p1, p2, ...)` 的顺序执行；任一阶段失败或 panic 即停止，
  后续阶段不再运行，已完成阶段的结果不受影响；
- panic 会被恢复并归一化为 `*pipeline.PhaseError{Phase, Reason, Panic: true}`；
  普通错误归一化为 `Panic: false` 的 `PhaseError`，均带阶段标识与原因；
- 每个目标目录拥有独立的 `pipeline.Context`，`Runner` 构造后不可变，
  `RunTargets` 可并发处理多个目标；阶段实现不得使用全局可变量，
  只读写自己 `Context` 的 `Vars`（竞态由 `-race` 测试保障）。

## 测试覆盖

- `internal/config`：合并优先级、确定性（重复加载）、全部拒绝类别；
- `internal/sync`：重复生成字节级一致、用户改动保留、三种冲突策略、
  模板移除与用户删除、冲突时不提交清单；
- `internal/pipeline`：阶段顺序、失败归一化、panic 恢复、并发目标隔离
  （`-race`）、部分目标失败不影响其它目标；
- `cmd/scaffold`：端到端重复生成一致性、退出码分类（2/3/0）。

单测日志（`go test -v`）会打印输入组合、合并结果与每个文件的判定依据。
