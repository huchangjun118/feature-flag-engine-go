# feature-flag-engine-go

纯本地进程内的特性开关（Feature Flag）引擎：无需任何外部服务，评估过程是
纯内存计算。支持定向规则、按比例稳定分桶、开关依赖、配置版本化与原子热更新、
显式覆盖/紧急熔断，以及评估结果的可观测与追溯。

- 包路径：`github.com/huchangjun118/feature-flag-engine-go`
- 零第三方依赖，仅用 Go 标准库。

## 快速开始

```go
eng, err := flagengine.New(flagengine.Options{
    Authorizer: flagengine.NewRoleAuthorizer("sre-oncall"),
}, cfg)
if err != nil { /* 配置非法（含循环依赖），拒绝启动 */ }

res, version, err := eng.EvaluateOnConfig("new_checkout", flagengine.Subject{
    ID: "user-1001", Region: "cn", Device: "ios", Version: "2.4.1",
    Tags: []string{"beta"},
})
// res.On            最终开关结果
// res.Trace.Reason  结果来源（可解释）
// res.Trace.Bucket  主体的稳定分桶值 0..99
// version           本次评估基于的配置版本
```

完整可运行示例见 `example_test.go`（`ExampleEngine_basicUsage`）。

## 1. 评估语义（Evaluation Semantics）

对 `Evaluate(flagKey, subject)`，引擎在**当前已提交的不可变快照**上按下列
**固定优先级**决策（从高到低）：

| 优先级 | 判定                              | 结果                         | Trace.Reason                |
|--------|-----------------------------------|------------------------------|-----------------------------|
| 1      | 任一前置依赖开关结果不满足要求    | **关闭**（即使默认值为开）   | `prerequisite_failed`       |
| 2      | 全局覆盖强制关闭（紧急熔断，`*`） | 关闭，**任何规则无法绕过**   | `global_override`           |
| 3      | 主体级覆盖强制关闭                | 关闭                         | `subject_override`          |
| 4      | 主体级覆盖强制开启                | 开启                         | `subject_override`          |
| 5      | 全局覆盖强制开启                  | 开启                         | `global_override`           |
| 6      | 命中定向规则                      | 桶值 `< Percentage` → `Rule.On` | `rule` / `rule_percentage_miss` |
| 7      | 未命中任何规则                    | `Flag.DefaultOn`             | `default`                   |

设计要点：

- **安全优先**：全局熔断永远压过全局强制开启；主体级强制关闭压过全局开启。
  这样"强制开启"不可能绕过对特定主体或全局的止血。
- **未命中回落默认值**：命中了规则但桶未进入放量集合时，结果同样回落
  `DefaultOn`，但 `Reason = rule_percentage_miss` 且 Trace 中保留命中规则、
  桶值与百分比，便于事后解释"为什么他当时没开"。
- **异常输入确定化**：未知运算符、缺失属性、非法版本号一律视为"不命中"，
  绝不 panic、绝不产生随机结果；`Subject.ID` 为空直接返回 `ErrEmptySubject`；
  开关不存在返回 `ErrFlagNotFound`；没有任何已提交版本时返回明确错误。

### 规则与条件

- 规则按 **`Priority` 升序、`ID` 字典序** 排序，第一条全部条件命中的规则生效，
  优先级完全确定，不依赖 map 遍历顺序。
- 条件之间是 AND；空条件规则命中所有主体（可做兜底规则）。
- 内置属性：`region`、`device`、`version`、`tag`、`id`；
  其他键从 `Subject.Attrs` 自定义属性读取。
- 运算符：`eq / ne / in / not_in / prefix / suffix / contains /
  version_gte / version_lte / version_between / tag_any / tag_all / exists`。
- 版本为语义化版本（`1`、`1.2`、`v1.2.3` 均可，缺段补 0；非法输入安全判否）。

## 2. 分桶规则（Bucketing）

- 桶总数固定 `BucketCount = 100`，桶值 `0..99`。
- 桶值 = `FNV-1a-64(salt | "|" | flagKey | "|" | subjectID) mod 100`。
  FNV 是纯函数：**结果与并发度、进程重启、map 遍历顺序、随机源完全无关**。
- `salt` 缺省取 `Flag.Key`。
- 分桶输入**刻意不包含**配置版本号、提交时间和放量比例，因此：
  - 调大/调小 `Percentage` 只改变阈值，主体桶位不变；
  - 放量集合是桶的"前缀区间" `bucket < Percentage`，比例单调变化：
    已放量主体不会因上调而被踢出，也不会被重新随机；
  - 回滚到历史版本（同一定义）后桶值与当时一致；
  - 进程重启后从同一配置文件载入，结果逐位一致。
- 若确实需要"换一批人"（例如重新灰度），显式更换 `Flag.Salt` 即可——
  这是唯一会主动改变桶位的手段，且对审计可见。
- 同一个 (flag, subject) 在同一版本任意次评估结果恒等；
  不同 flag 使用不同 `flagKey` 命名空间，互不相关。

## 3. 依赖与循环检测

- `Flag.Dependencies` 声明前置开关及其要求的结果（`RequireOn`）。
  前置不满足时，依赖开关**一定**评估为关闭。
- 提交配置时做 DFS 三色环检测：发现环则**整体拒绝该版本**，
  返回 `*CycleError`（可用 `errors.Is(err, flagengine.ErrCycleDetected)`），
  错误信息给出完整环路径，例如 `a -> b -> c -> a`；旧版本继续服务。
  自依赖（`a` 依赖 `a`）同样拒绝。
- 评估深度有硬上限 `Options.MaxDepth`（默认 16），超限返回
  `ErrDepthLimit`；另有单次评估比较操作预算 `Options.MaxEvalOps`
  （默认 100000），耗尽返回 `ErrEvalBudget`。
- **单次评估耗时上界**：评估无 IO、无锁竞争热点、无随机数，
  工作量被依赖深度与操作预算封顶：
  `O(min(MaxDepth, 依赖数) × 每开关规则/条件数)`，且不超过 `MaxEvalOps`
  次简单比较；读取当前快照是一次 `atomic.Pointer.Load()`。

## 4. 版本化与原子热更新

- `Commit(*Config)`：深拷贝 → 校验（完整性/未知运算符/比例区间/依赖目标）
  → 环检测 → 构建不可变快照 → `atomic.Pointer` 一次性替换。
- 评估全程只读一个快照指针，**永远看不到字段级混合的半更新状态**；
  慢评估不阻塞更新（更新不等待在途评估），更新也不影响在途评估的输入。
- 任一环节失败：保留旧快照继续服务，返回错误；版本号必须严格递增。
- `Rollback(version)`：原子切回历史版本（默认保留最近 32 个，
  由 `Options.HistorySize` 调整；当前版本永不被裁剪）。
- 外部在 `Commit` 后继续修改传入的 `Config` 对象不影响引擎（深拷贝隔离）。
- 持久化：`SaveConfigFile`（临时文件 + rename 原子落盘）、
  `LoadConfigFile`、`NewFromFile`。进程重启载入同一文件，
  同一输入的结果与重启前完全一致。

## 5. 覆盖与紧急熔断

- `SetOverride(Override)` 设置主体级或全局覆盖；
  `SubjectID == "*" 或 ""` 表示全局（熔断）。
- 覆盖**必须**带 `Operator`（操作者）与 `Reason`（原因）；
  支持 `ExpiresAt` 有效期，过期后自动失效（评估时即时判定，无需后台清理）。
- 鉴权：`Options.Authorizer`。默认 `DenyAllAuthorizer` 拒绝一切操作；
  内置 `NewRoleAuthorizer("alice", ...)` 白名单。越权返回
  `ErrUnauthorized` 并写拒绝审计事件。
- 覆盖以**自动递增的新版本**原子提交（不是原地改字段）：
  可审计、可回滚（移除覆盖也是新版本）。覆盖优先级最高，
  常规规则永远无法绕过，见上文评估优先级表。

## 6. 可观测与追溯

- 每次评估返回 `Trace`：配置版本、命中规则、变体、桶值、百分比、
  结果原因、前置依赖链、评估时间。
- 内存环形缓冲（默认 4096 条，`Options.TraceSize`）：
  `QueryTrace(flagKey, subjectID)` 查最近一条，`RecentTraces(n)` 取最近 n 条。
- 审计事件（JSON Lines）：`commit / rollback / override /
  override_remove / reject / eval`。可配置 `Options.AuditSink`，
  内置 `FileAuditSink`（追加写、每条刷盘），进程重启后历史仍可追溯。
  sink 写入失败绝不影响评估主路径。

## 配置项（Options）

| 字段          | 默认值   | 含义                                                 |
|---------------|----------|------------------------------------------------------|
| `MaxDepth`    | 16       | 依赖评估最大深度，超过返回 `ErrDepthLimit`           |
| `MaxEvalOps`  | 100000   | 单次评估比较操作预算（硬性耗时上界）                 |
| `HistorySize` | 32       | 保留的可回滚历史版本数                               |
| `TraceSize`   | 4096     | 内存保留的最近评估记录条数                           |
| `Authorizer`  | 拒绝全部 | 覆盖/熔断操作者鉴权                                  |
| `Now`         | time.Now | 注入时钟（过期判定/审计时间），测试可固定            |
| `AuditSink`   | 无       | JSON Lines 审计事件输出（如 `NewFileAuditSink`）     |

## JSON 配置示例

```json
{
  "version": 1,
  "comment": "first release",
  "flags": {
    "new_checkout": {
      "key": "new_checkout",
      "default_on": false,
      "rules": [
        {
          "id": "cn_ios_10pct",
          "priority": 1,
          "conditions": [
            {"attribute": "region",  "operator": "eq", "values": ["cn"]},
            {"attribute": "device",  "operator": "in", "values": ["ios", "android"]},
            {"attribute": "version", "operator": "version_between",
             "values": ["2.0.0", "3.0.0"]},
            {"attribute": "tag", "operator": "tag_any", "values": ["beta"]}
          ],
          "percentage": 10,
          "on": true,
          "variation": "new_ui"
        }
      ],
      "dependencies": [{"flag_key": "checkout_available", "require_on": true}]
    },
    "checkout_available": {"key": "checkout_available", "default_on": true}
  },
  "overrides": {
    "new_checkout": {
      "*": {
        "flag_key": "new_checkout", "subject_id": "*", "on": false,
        "operator": "sre-oncall", "reason": "INCIDENT-42",
        "expires_at": "2025-06-01T18:00:00Z"
      }
    }
  }
}
```

## 错误一览

| 错误                  | 场景                                       |
|-----------------------|--------------------------------------------|
| `ErrInvalidConfig`    | 配置非法：版本号、比例区间、未知运算符等   |
| `ErrCycleDetected`    | 依赖图存在环（`*CycleError` 携带环路径）   |
| `ErrFlagNotFound`     | 评估/覆盖引用了不存在的开关                |
| `ErrDepthLimit`       | 依赖深度超过 `MaxDepth`                    |
| `ErrEvalBudget`       | 单次评估比较操作超过 `MaxEvalOps`          |
| `ErrUnauthorized`     | 操作者不在鉴权白名单                       |
| `ErrInvalidOverride`  | 覆盖缺原因、目标不存在等                   |
| `ErrVersionNotFound`  | 回滚目标版本不在历史中                     |
| `ErrEmptySubject`     | 主体 ID 为空                               |

## 测试

```bash
go test ./... -race -count=1
```

覆盖场景：分桶稳定性/单调性/分布、规则优先级与全部运算符、依赖满足与
前置失败、循环依赖与自依赖拒绝、深度与预算上限、并发热更新（race 检测下
验证无半更新视图与同版本确定性）、提交失败保持旧版本、回滚结果逐位复现、
重启复现、主体/全局覆盖、越权拒绝、覆盖过期、审计落盘。
