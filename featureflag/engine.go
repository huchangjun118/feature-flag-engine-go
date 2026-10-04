package featureflag

import (
	"fmt"
	"time"
)

// DefaultMaxDepth 是依赖链评估的默认深度上限，防止失控的深层递归。
const DefaultMaxDepth = 8

// Engine 是开关评估引擎。评估完全在进程内完成，不依赖外部服务。
type Engine struct {
	store    *Store
	maxDepth int
}

// NewEngine 基于给定 Store 创建引擎；maxDepth 可选，默认 DefaultMaxDepth。
func NewEngine(store *Store, maxDepth ...int) *Engine {
	d := DefaultMaxDepth
	if len(maxDepth) > 0 && maxDepth[0] > 0 {
		d = maxDepth[0]
	}
	return &Engine{store: store, maxDepth: d}
}

// Evaluate 评估单个开关，返回完整可追溯结果。
//
// 单次评估只读取加载时的那一个配置快照（见 Store），
// 耗时上界为 O(maxDepth × 单开关规则数 × 单规则条件数)，
// 与并发更新、其他评估无关；评估路径不加锁、不分配随机源。
func (e *Engine) Evaluate(ctx EvalContext, flagKey string) EvalResult {
	cfg := e.store.Snapshot()
	res := EvalResult{FlagKey: flagKey, Bucket: -1}
	if cfg == nil {
		res.Reason = ReasonFlagNotFound
		res.Trace = append(res.Trace, "no active config version")
		return res
	}
	res.ConfigVersion = cfg.Version
	if ctx.SubjectID == "" {
		// 异常输入：返回确定性的兜底值，绝不随机。
		res.Reason = ReasonInvalidInput
		res.Trace = append(res.Trace, "empty subject id")
		return res
	}
	now := ctx.Now
	if now.IsZero() {
		now = time.Now()
	}
	return e.eval(cfg, ctx, now, flagKey, 0, map[string]bool{}, &res)
}

// eval 是递归求值主体。visited 防止运行期依赖环（配置提交时已做环检测，
// 此处为防御性兜底）。结果写入 out，同时返回给上层依赖判断使用。
func (e *Engine) eval(cfg *Config, ctx EvalContext, now time.Time, flagKey string, depth int, visited map[string]bool, out *EvalResult) EvalResult {
	trace := func(format string, args ...any) {
		out.Trace = append(out.Trace, fmt.Sprintf(format, args...))
	}

	if depth > e.maxDepth {
		out.Value = false
		out.Reason = ReasonMaxDepthExceeded
		trace("flag=%s: max dependency depth %d exceeded", flagKey, e.maxDepth)
		return *out
	}
	if visited[flagKey] {
		out.Value = false
		out.Reason = ReasonDependencyUnmet
		trace("flag=%s: runtime dependency cycle guard", flagKey)
		return *out
	}

	// 1. 显式覆盖优先级最高，常规规则无法绕过。
	if ov := activeOverride(cfg, flagKey, ctx.SubjectID, now); ov != nil {
		out.Value = ov.Value
		out.Reason = ReasonOverride
		out.OverrideID = ov.ID
		trace("flag=%s: override %s by %s (%s) -> %v", flagKey, ov.ID, ov.Operator, ov.Reason, ov.Value)
		return *out
	}

	flag, ok := cfg.Flags[flagKey]
	if !ok {
		out.Value = false
		out.Reason = ReasonFlagNotFound
		trace("flag=%s: not found in config version %d", flagKey, cfg.Version)
		return *out
	}

	// 2. 前置依赖：任一未满足则直接关闭，不再评估规则。
	if len(flag.DependsOn) > 0 {
		visited[flagKey] = true
		defer delete(visited, flagKey)
	}
	for _, dep := range flag.DependsOn {
		var depOut EvalResult
		depRes := e.eval(cfg, ctx, now, dep.FlagKey, depth+1, visited, &depOut)
		out.Trace = append(out.Trace, depOut.Trace...)
		if depRes.Value != dep.Expected {
			out.Value = false
			out.Reason = ReasonDependencyUnmet
			if depRes.Reason == ReasonMaxDepthExceeded {
				// 深度超限的原因向上传播，避免被误读为普通的前置未满足。
				out.Reason = ReasonMaxDepthExceeded
			}
			trace("flag=%s: dependency %s expected %v got %v", flagKey, dep.FlagKey, dep.Expected, depRes.Value)
			return *out
		}
		trace("flag=%s: dependency %s satisfied (%v)", flagKey, dep.FlagKey, dep.Expected)
	}

	// 3. 定向规则：快照中的规则已按 (Priority, ID) 排序，顺序确定。
	rolloutMissRule := ""
	rolloutMissBucket := -1
	for _, rule := range flag.Rules {
		if !matchRule(ctx, rule) {
			continue
		}
		if rule.Rollout == nil {
			out.Value = rule.Value
			out.Reason = ReasonRuleMatch
			out.RuleID = rule.ID
			trace("flag=%s: rule %s matched -> %v", flagKey, rule.ID, rule.Value)
			return *out
		}
		salt := rule.Rollout.Salt
		if salt == "" {
			salt = rule.ID
		}
		bucket := bucketOf(flagKey, salt, ctx.SubjectID)
		if bucket < rule.Rollout.Percentage {
			out.Value = rule.Value
			out.Reason = ReasonRuleMatch
			out.RuleID = rule.ID
			out.Bucket = bucket
			trace("flag=%s: rule %s matched, bucket=%d < %d -> %v",
				flagKey, rule.ID, bucket, rule.Rollout.Percentage, rule.Value)
			return *out
		}
		trace("flag=%s: rule %s matched but bucket=%d >= %d, rollout miss",
			flagKey, rule.ID, bucket, rule.Rollout.Percentage)
		if rolloutMissRule == "" {
			rolloutMissRule = rule.ID
			rolloutMissBucket = bucket
		}
		// 放量未中：继续尝试后续规则。
	}

	// 4. 回落默认值。
	out.Value = flag.DefaultValue
	out.Bucket = rolloutMissBucket
	if rolloutMissRule != "" {
		out.Reason = ReasonRuleRolloutMiss
		out.RuleID = rolloutMissRule
		trace("flag=%s: rollout missed, fall back to default %v", flagKey, flag.DefaultValue)
	} else {
		out.Reason = ReasonDefault
		trace("flag=%s: no rule matched, default %v", flagKey, flag.DefaultValue)
	}
	return *out
}
