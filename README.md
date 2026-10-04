# feature-flag-engine-go

进程内的特性开关引擎：定向发布、渐进放量、开关依赖、版本化热更新、
显式覆盖 / 紧急熔断与评估追溯。评估完全在本地进程内完成，不依赖外部服务。

```go
store := featureflag.NewStore("sre", "admin") // 允许执行覆盖的操作者
_ = store.Update(featureflag.Config{Version: 1, Flags: ...})
engine := featureflag.NewEngine(store)        // 可选第二参数设置依赖深度上限
res := engine.Evaluate(featureflag.EvalContext{
    SubjectID:  "user-42",
    Attributes: map[string]string{"region": "cn", "version": "1.4.0"},
    Tags:       []string{"beta"},
}, "new-checkout")
// res.Value / res.Reason / res.RuleID / res.Bucket / res.ConfigVersion / res.Trace
```

## 评估语义

单次评估按以下顺序短路返回，命中情况可通过 `EvalResult.Reason` 与
`EvalResult.Trace` 完整解释：

1. **显式覆盖（`override`）**：先查主体级覆盖，再查全局熔断；
   过期覆盖视为不存在。优先级最高，常规规则无法绕过。
2. **开关存在性**：不存在返回 `flag_not_found`，值为 `false`。
3. **前置依赖（`dependency_unmet`）**：所有 `depends_on` 逐一递归评估，
   任一前置开关结果不等于期望值即返回 `false`，不再评估本开关规则。
   依赖链深度超过上限（默认 8）返回 `max_depth_exceeded`，
   该原因会沿依赖链向上传播。
4. **定向规则（`rule_match`）**：规则按 `(priority, id)` 升序排列
   （数值越小越优先），逐条匹配；条件之间为逻辑与。
   规则命中且无放量配置时直接返回规则值。
5. **渐进放量**：规则命中且配置了 `rollout` 时，主体分桶值
   `< percentage` 才返回规则值；否则记录 `rule_rollout_miss` 并继续
   尝试后续规则。
6. **默认值回落（`default`）**：未命中任何规则时返回开关的
   `default_value`。

异常输入（空主体 ID、无法解析的版本号、缺失属性等）一律产生确定性的
兜底结果（不命中 / `invalid_input`），绝不产生随机或未定义行为。

## 分桶规则

- 分桶值为 `FNV-1a(flagKey \0 salt \0 subjectID)` 高 32 位对 100 取模，
  值域 `[0,100)`；`salt` 为空时取规则 ID。
- 分桶是纯函数：与并发度、进程重启、map 遍历顺序、随机源均无关，
  同一主体在同一开关同一配置版本下结果恒定。
- 比例**上调**时，已在 `[0, oldP)` 内的主体必然仍在 `[0, newP)` 内，
  不会被踢出；比例**下调**时按分桶序确定性收缩（高分桶先退出），
  任何主体的分桶值本身不变，即不会被“重新随机”。

## 配置项

| 字段 | 说明 |
| --- | --- |
| `Config.Version` | 版本号，更新时必须严格大于当前版本 |
| `Flag.DefaultValue` | 未命中任何规则时的回落值 |
| `Rule.Priority` | 规则优先级，数值小者先评估；相同按 `Rule.ID` 字典序 |
| `Condition{Attr, Op, Values}` | 支持 `eq` / `neq` / `in` / `version_in_range`（语义化版本闭区间，`Values` 为 `[min, max]`）/ `tag_contains`（匹配 `EvalContext.Tags`） |
| `Rollout{Percentage, Salt}` | 放量百分比 `[0,100]` 与打散盐 |
| `Dependency{FlagKey, Expected}` | 前置开关及其期望结果 |
| `Override{SubjectID, Value, ExpiresAt, Operator, Reason}` | 覆盖 / 熔断；`SubjectID` 为空表示全局；必须携带有效期、操作者与原因 |

## 版本化与热更新

- `Store.Update` 先整体校验（结构、放量区间、依赖图无环——发现环时
  整份拒绝并在错误中指出构成环的开关路径），再一次原子发布；
  校验失败时上一版本继续服务。
- 已发布版本不可变（提交时深拷贝），评估方通过原子指针加载快照，
  因此慢评估不阻塞更新，更新也不会让进行中的评估看到半更新状态。
  单次评估耗时上界为 `O(maxDepth × 单开关规则数 × 单规则条件数)`，
  评估路径不加锁。
- `Store.Rollback(version)` 回滚到历史版本；由于快照不可变且分桶为
  纯函数，回滚后同一输入的评估结果与该版本首次生效时完全一致。
- `Store.AddOverride` 在当前版本上追加覆盖并作为新版本原子发布；
  操作者必须在 `NewStore` 的白名单内，否则拒绝。

## 测试

```sh
go test -race ./...
```

覆盖：分桶稳定性、规则优先级、依赖与循环、并发热更新、回滚、
强制覆盖与熔断、异常输入确定性。
