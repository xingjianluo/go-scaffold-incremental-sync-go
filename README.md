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

# 批量：一次传多个目标（逗号分隔），-concurrency 限制同时处理个数
go run ./cmd/scaffold \
  -targets /tmp/a,/tmp/b,/tmp/c -concurrency 2 \
  -name myapp -module example.com/myapp

# 预演：只打印每个文件的结论，不创建/修改/删除任何东西
go run ./cmd/scaffold -targets /tmp/demo -name myapp -module example.com/myapp -plan

# 整体超时；运行中按 Ctrl-C（或收到 SIGTERM）也会触发同样的取消流程
go run ./cmd/scaffold -targets /tmp/a,/tmp/b -concurrency 2 -timeout 30s
```

退出码：`0` 成功；`2` 配置被拒绝；`3` 冲突（`-policy=fail` 时双方同时改动）；
`4` 阶段失败或 panic；`5` 整批被取消或超时；`1` 其它错误。
一个批次里同时存在多种失败时取最严重的退出码（`5 > 4 > 3 > 1`）。

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

## 批量执行

`-targets` 接受逗号分隔的多个目标目录，一次性批量处理：

- **并发上限**：`-concurrency N`（默认 4，必须 ≥ 1，否则退出码 2）。
  内部是固定大小的 worker 池，池规模即同时处理目标的上限，不会把机器一次打满。
- **目标互不影响**：每个目标拥有独立的 `pipeline.Context`、独立日志缓冲和
  独立错误。某个目标失败、panic 或冲突都不会连累其它目标（见 `internal/batch`）。
- **结果一目了然**：跑完后按输入顺序输出每个目标的逐文件判定与最终状态，
  末尾给出汇总行，例如 `batch summary: 2 succeeded, 1 failed, 1 cancelled`，
  并逐条列出失败目标名与原因。
- **确定性**：目标目录会先修剪空白、去重并按字典序排序；模板只渲染一次，
  结果对所有目标只读共享；成功/失败/取消汇总列表都按名字字典序排列。
  同样的输入重跑，结论与文件字节完全一致。

## 取消与超时

批量过程支持两种中途停止方式，二者共用同一个 `context.Context`，语义一致：

- `-timeout 30s`：整批的整体截止时间，超时即取消（`context.DeadlineExceeded`）；
  `0`（默认）表示不设超时。
- `Ctrl-C` / `SIGTERM`：主动取消（`context.Canceled`）。

取消后的保证：

1. **不再启动新目标**：调度器在派发每个目标前检查 ctx，取消后剩余目标直接标记为
   `cancelled`，不会动手；已经在跑的目标可以干净收尾，收完即止。
2. **不留下半成品**：单个目标内部先做完全部只读三方分析，之后每个写/删动作之前
   再检查 ctx。取消时不会再发起新的变更；已经开始的单个文件写入走
   “临时文件 + 原子改名”，所以该文件要么是完整的新内容、要么保持原状，
   绝不会写一半。清单最后才一次性原子提交，取消时绝不提交。
3. **不遗留临时文件**：每次写文件/清单前都会先清掉同名 `.tmp`，
   因此即使上次进程被杀留下临时文件，重跑也是干净的。
4. **取消后重跑安全**：取消不提交清单、不产生部分计数，重跑从同一个三方基准
   重新分析，幂等收敛；不会出现重复写入或计数错乱。

被取消的目标（无论取消时还没启动、还是在执行中因 ctx 退出）统一归类为
`cancelled`，与真正的业务失败 `failed` 区分开；整批存在取消时退出码为 5。

## 变更预演（dry run）

`-plan` 在正式写盘之前先给出每个目标本次会发生什么，逐文件打印结论：

| 结论 | 含义 |
|---|---|
| `create` | 模板新文件，将新增 |
| `update` | 模板侧演进且本地未改，将更新（含 `policy=theirs` 的冲突） |
| `keep` | 本地内容原样保留（已是最新 / 仅本地改过 / `policy=ours` 的冲突） |
| `delete` | 模板已移除且本地未改，将删除 |
| `skip` | 本地已删除，尊重删除不复活 |
| `conflict` | 双方同时改动且 `policy=fail`，需要人工决定 |

预演是**严格只读**的：不创建目录/文件、不修改、不删除、不写清单；
`policy=fail` 且存在冲突时，预演同样返回 `ConflictError`（退出码 3）并打印
全部冲突文件，但仍然不动磁盘。

### 为什么预演结论一定和实际执行对得上

预演（`Applier.Plan`）和真正执行（`Applier.ApplyContext`）**共用同一个纯只读
分析函数 `analyze`**：它只读取清单与当前文件、比较三方哈希，产出按路径字典序
排列的逐文件判定和“成功后应落盘的清单”，但不做任何写操作。区别只在最后一步：

- `Plan` 拿到分析结果后直接返回；
- `ApplyContext` 在相同的分析结果上，按同样的顺序执行写/删并原子提交清单。

两条路径对同一文件系统状态的输入完全相同，`analyze` 又不含任何副作用或随机、
时间因素，因此预演的逐文件 `Decision`（动作与理由）以及返回错误，与真正执行的
结果逐条一致。测试 `TestPlanMatchesApply` 用两棵初始状态完全相同的目录树，分别
跑 Plan 与 Apply，断言 `Decision` 与错误完全相等，同时断言预演目录保持原样。

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
  模板移除与用户删除、冲突时不提交清单；**预演严格只读、预演/执行逐文件一致
  （含三种策略与冲突）、取消后无半成品与无遗留 `.tmp`、取消后重跑幂等收敛**；
- `internal/batch`：并发上限不被突破、部分目标失败/panic 互不连累、
  汇总按字典序且重复执行一致、取消后不启动新目标、超时归类为 cancelled；
- `internal/pipeline`：阶段顺序、失败归一化、panic 恢复、并发目标隔离
  （`-race`）、部分目标失败不影响其它目标；
- `cmd/scaffold`：端到端重复生成一致性、退出码分类（2/3/0），
  **批量多目标与重复批量一致、批量部分失败（冲突目标不连累其它目标）、
  `-plan` 不落盘（含冲突场景）、`-timeout` 超时退出码 5、非法 `-concurrency` 拒绝**。

单测日志（`go test -v`）会打印输入组合、合并结果与每个文件的判定依据，
例如 `plan  decision: a.txt -> keep (...)` 与 `apply decision: ...` 成对出现，
方便核对预演与执行是否逐条吻合。

## 本地如何验证

```sh
go build ./...          # 编译
go vet ./...            # 静态检查
go test ./...           # 单元测试
go test ./... -race     # 含数据竞争检测（批量并发目标场景）
go test ./... -v        # 查看输入组合、合并结果与逐文件判定依据日志
```

建议的手工冒烟：

```sh
# 1) 预演不落盘：目录在执行前后内容不变
go run ./cmd/scaffold -targets /tmp/demo -name demo -module example.com/demo -plan
ls -a /tmp/demo         # 应为空（目录不存在时连目录都不会被创建）

# 2) 真正执行后，再预演一次：结论应全部为 keep / skip，且不改动文件
go run ./cmd/scaffold -targets /tmp/demo -name demo -module example.com/demo
go run ./cmd/scaffold -targets /tmp/demo -name demo -module example.com/demo -plan

# 3) 批量 + 并发上限，观察末尾 summary
go run ./cmd/scaffold -targets /tmp/a,/tmp/b,/tmp/c -concurrency 2 \
  -name demo -module example.com/demo

# 4) 取消：批量跑到一半按 Ctrl-C（或给一个极短 -timeout），
#    已开始的目标收尾，其它目标标记 cancelled；再次执行应幂等收敛
go run ./cmd/scaffold -targets /tmp/a,/tmp/b,/tmp/c -concurrency 1 -timeout 1ns
find /tmp/a /tmp/b /tmp/c -name '*.tmp'   # 不应有遗留临时文件
```

