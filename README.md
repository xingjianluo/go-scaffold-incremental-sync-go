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
# 单目标
go run ./cmd/scaffold -targets /tmp/demo -name myapp -module example.com/myapp
go run ./cmd/scaffold -targets /tmp/demo -name myapp -module example.com/myapp  # 重复执行输出完全一致

# 多目标批量（逗号分隔），-jobs 限制同时处理的目标数
go run ./cmd/scaffold -targets /tmp/a,/tmp/b,/tmp/c -name myapp -module example.com/myapp -jobs 2

# 只预演不落盘：看每个文件会 create/update/delete/conflict/keep/untouched
go run ./cmd/scaffold -targets /tmp/a -name newname -module example.com/myapp -dry-run

# 整体超时；运行中也可 Ctrl-C 主动取消
go run ./cmd/scaffold -targets /tmp/a,/tmp/b -name myapp -module example.com/myapp -timeout 30s
```

退出码：`0` 成功；`2` 配置被拒绝；`3` 冲突（`-policy=fail` 时双方同时改动）；
`4` 阶段失败或 panic；`5` 批量被取消/超时；`1` 其它错误。

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

## 批量执行（`internal/batch`）

`-targets` 接受逗号分隔的多个目标目录；目录先去空白、去空项，再按字典序
调度（与输入顺序无关）。每个目标独立执行、互不影响：

- **并发上限 `-jobs n`**：同一时刻最多 n 个目标在执行（信号量控制），默认 4；
  `n < 1` 会以退出码 2 被拒绝。
- **结果汇总**：每个目标归入 `ok` / `failed` / `cancelled` 三类，普通错误与
  `context.Canceled`/`DeadlineExceeded`（用 `errors.Is`，支持被包裹）分开统计；
  最后打印 `batch summary: total=.. ok=.. failed=.. cancelled=..`。某个目标
  失败只影响它自己，退出码按 3（冲突）→ 4（阶段失败）→ 1（其它）的优先级取。
- **确定性**：目标按名称字典序启动、结果按名称字典序返回；阶段实现无全局
  可变状态，渲染结果不含时间戳。相同输入重复执行，每个文件结论与整批结果
  完全一致（测试重复执行 20 次断言一致）。

## 取消与超时语义

`-timeout 30s` 设置整体超时；运行中按 Ctrl-C（SIGINT/SIGTERM）主动取消。
两者取消的是同一个 `context.Context`，语义相同：

1. 取消/超时触发后，**绝不启动新目标**（批量调度在申领并发名额时即感知 ctx）；
2. 已经在执行的目标运行到**落盘检查点**干净收尾：每个文件写入/删除前、
   提交清单前都检查 ctx。文件写入本身是“临时文件 + 原子改名”，要么完整
   出现、要么不出现；取消时清单不会提交半成品，因此不会固化中间 base；
3. 在途目标若在取消前已自行正常完成，仍按真实结果记 `ok/failed`——它的工作
   已经完成，取消不篡改结论；在检查点停下的记 `cancelled`；
4. 没有写一半的文件、没有遗留临时文件：每次 Apply 开头会清理本工具已知
   位置（各渲染文件与清单旁的 `.tmp`）上次中断可能残留的临时文件；
5. **取消后重跑安全**：三方合并天然幂等——已原子写入的文件下次判为
   `keep`，未写入的继续 `create/update`，已提交的清单是完整旧版，
   不会出现重复写入、重复计数或计数错乱。

超时（`DeadlineExceeded`）与主动取消（`Canceled`）在结果中都归为
`cancelled`，对应退出码 5。

## 变更预演（`-dry-run`）

`Applier.Plan` 与 `Applier.Apply` 共享同一份纯只读判定函数 `evaluate`：
它只读项目目录与清单，产出「按路径字典序排列的结论 + 待执行变更序列 +
提交后的清单状态」，自身不做任何修改。

- `Plan` 只返回结论，**不创建、不修改、不删除任何文件，也不写清单**；
- `Apply` 拿到的是同一份评估结果，只是把其中的变更序列落盘。因此
  **预演结论与实际执行严格一致**；冲突时两者都返回 `*ConflictError`
  （dry-run 下同样以退出码 3 提示需要决策）。

预演/执行中每个文件给出 7 类动作之一，均带可解释的 `Reason`：

| 动作 | 含义 |
|---|---|
| `create` | 模板新文件，将新增 |
| `update` | 模板侧改动、项目未改（或 `policy=theirs`），将更新 |
| `keep` | 无变化，或用户改了而模板没改，原样保留 |
| `skip` | 用户已删除/模板移除且文件已不存在，尊重现状不复活 |
| `delete` | 模板移除且用户未改，将删除 |
| `conflict` | 双方同时改动且 `policy=fail`，需人工决策 |
| `untouched` | 本地存在但模板与清单都不接管的文件，永不触碰 |

一致性由测试强保证：在覆盖全部 7 类动作的目录上，先 Plan 后 Apply，断言
两者 `Decision` 逐字节一致，并对文件系统差异做双向核验——Plan 预告的
每个 create/update/delete 都真实发生，且文件系统的每一处变化都在 Plan
中出现；Apply 之后再次 Plan 不再预告任何写动作（进入稳态）。

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
  模板移除与用户删除、冲突时不提交清单；新增：
  - 预演零副作用（前后快照一致）、Plan/Apply 三种策略下结论逐字节一致、
    7 类动作全覆盖、Plan 与真实文件系统差异双向核验；
  - 写入中途确定性取消（测试钩子在第 101 个文件前取消）：无 `.tmp` 残留、
    清单不提交半成品、重跑幂等收敛（100 keep + 100 create → 全 keep 稳态）；
  - 开始前已取消不产生任何文件；超时触发后同样无残留且可安全重跑；
- `internal/batch`：成功/失败/取消分类与互不连累、`-jobs` 1/2/4 下峰值
  并发不超限（原子计数）、整批预取消零启动、中途取消不再启动排队目标而
  在途目标干净收尾、20 次重复执行结果顺序完全一致（`-race`）；
- `cmd/scaffold`：端到端重复生成一致性、退出码分类（2/3/0）；新增：
  多目标批量隔离（仅冲突目标返回 3 且本地内容保留）、批量字节级确定性、
  dry-run 不落盘且与正式执行对得上、非法 `-jobs` 拒绝（2）。

单测日志（`go test -v`）会打印输入组合（如 `policy=ours/dryFirst=true`、
`max=2`）、每个文件的动作与判定依据、并发峰值、取消时 started/finished
计数，方便核对判断依据。

## 本地验证

```sh
go build ./...            # 编译
go test ./...             # 单元测试
go test ./... -race       # 含数据竞争检测（批量并发场景必过）
go test ./... -v          # 查看输入组合、合并结果与判定依据日志
```

手工验证取消收尾（可自行准备大量文件后运行并在中途 Ctrl-C）：

```sh
go run ./cmd/scaffold -targets /tmp/a,/tmp/b -name app -module x/app -timeout 2s
# 观察退出码 5 后：
find /tmp/a /tmp/b -name '*.tmp'     # 期望：无输出（无临时文件残留）
go run ./cmd/scaffold -targets /tmp/a,/tmp/b -name app -module x/app
# 期望：batch summary 全 ok，再重复一次仍完全一致
```
