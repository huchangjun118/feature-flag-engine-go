// Package featureflag 提供进程内的特性开关评估能力：
// 定向规则、渐进放量、开关依赖、版本化热更新、显式覆盖与评估追溯。
package featureflag

import "time"

// ConditionOp 是单个条件比较操作符。
type ConditionOp string

const (
	// OpEq 属性值等于任一候选值。
	OpEq ConditionOp = "eq"
	// OpNeq 属性值不等于所有候选值。
	OpNeq ConditionOp = "neq"
	// OpIn 属性值在候选集合内（与 OpEq 等价，语义上强调集合）。
	OpIn ConditionOp = "in"
	// OpVersionInRange 属性值按语义化版本落在 [min, max] 闭区间，
	// Values 必须恰好为两个元素：Values[0]=min, Values[1]=max。
	OpVersionInRange ConditionOp = "version_in_range"
	// OpTagContains 主体自定义标签集合包含任一候选标签。
	// 该操作符读取上下文中的 Tags 字段而非 Attributes。
	OpTagContains ConditionOp = "tag_contains"
)

// Condition 描述对主体单个属性的一次匹配。
type Condition struct {
	// Attr 为属性名，如 region / device / version；OpTagContains 时忽略。
	Attr string      `json:"attr"`
	Op   ConditionOp `json:"op"`
	// Values 为候选值集合；OpVersionInRange 时长度为 2。
	Values []string `json:"values"`
}

// Rollout 描述规则命中后的按比例放量。
type Rollout struct {
	// Percentage 取值 [0,100]，表示放量的分桶百分比。
	Percentage int `json:"percentage"`
	// Salt 用于打散不同规则之间的分桶相关性，为空时使用规则 ID。
	Salt string `json:"salt,omitempty"`
}

// Rule 是一条定向规则。规则之间按 Priority 升序评估（数值越小越优先），
// Priority 相同按 ID 字典序，保证评估顺序确定。
type Rule struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
	// Conditions 之间为逻辑与。
	Conditions []Condition `json:"conditions"`
	// Value 为规则命中且放量通过时的返回值。
	Value bool `json:"value"`
	// Rollout 为空表示命中即返回 Value，不做放量。
	Rollout *Rollout `json:"rollout,omitempty"`
}

// Dependency 描述开关之间的前置条件：
// 仅当 FlagKey 评估结果等于 Expected 时，本开关才允许继续评估。
type Dependency struct {
	FlagKey  string `json:"flag_key"`
	Expected bool   `json:"expected"`
}

// Flag 是一个特性开关的完整定义。
type Flag struct {
	Key          string       `json:"key"`
	Description  string       `json:"description,omitempty"`
	DefaultValue bool         `json:"default_value"`
	Rules        []Rule       `json:"rules,omitempty"`
	DependsOn    []Dependency `json:"depends_on,omitempty"`
}

// Override 是显式覆盖 / 紧急熔断记录，优先级高于一切常规规则。
type Override struct {
	ID      string `json:"id"`
	FlagKey string `json:"flag_key"`
	// SubjectID 为空表示全局覆盖（熔断），否则仅作用于该主体。
	SubjectID string    `json:"subject_id,omitempty"`
	Value     bool      `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
	Operator  string    `json:"operator"`
	Reason    string    `json:"reason"`
}

// Config 是一份完整提交的配置版本。评估永远基于某个 Config 快照，
// 不会出现字段级混合。
type Config struct {
	Version   int64           `json:"version"`
	Flags     map[string]Flag `json:"flags"`
	Overrides []Override      `json:"overrides,omitempty"`
}

// EvalContext 是单次评估的输入。
type EvalContext struct {
	SubjectID  string            `json:"subject_id"`
	Attributes map[string]string `json:"attributes,omitempty"`
	// Tags 为主体自定义标签集合。
	Tags []string `json:"tags,omitempty"`
	// Now 用于覆盖当前时间（测试用），零值表示使用真实时间。
	Now time.Time `json:"-"`
}

// Reason 解释评估结果的来源。
type Reason string

const (
	ReasonOverride         Reason = "override"           // 命中显式覆盖
	ReasonDependencyUnmet  Reason = "dependency_unmet"   // 前置开关未满足
	ReasonRuleMatch        Reason = "rule_match"         // 命中定向规则
	ReasonRuleRolloutMiss  Reason = "rule_rollout_miss"  // 规则匹配但未落入放量分桶
	ReasonDefault          Reason = "default"            // 未命中任何规则，回落默认值
	ReasonFlagNotFound     Reason = "flag_not_found"     // 开关不存在
	ReasonMaxDepthExceeded Reason = "max_depth_exceeded" // 依赖评估深度超限
	ReasonInvalidInput     Reason = "invalid_input"      // 输入非法，返回确定性兜底值
)

// EvalResult 是单次评估的完整可追溯结果。
type EvalResult struct {
	FlagKey       string `json:"flag_key"`
	Value         bool   `json:"value"`
	Reason        Reason `json:"reason"`
	RuleID        string `json:"rule_id,omitempty"`
	OverrideID    string `json:"override_id,omitempty"`
	Bucket        int    `json:"bucket"` // 未参与放量时为 -1
	ConfigVersion int64  `json:"config_version"`
	// Trace 记录评估路径（依赖链、规则判定），用于事后解释。
	Trace []string `json:"trace,omitempty"`
}
