// Package flagengine 实现了一个纯本地进程内的特性开关引擎，
// 支持定向规则、按比例稳定分桶、开关依赖、配置版本化与原子热更新、
// 显式覆盖/紧急熔断以及评估结果的可观测与追溯。
package flagengine

import (
	"context"
	"errors"
	"time"
)

// Subject 是被评估的主体，携带用于规则匹配的属性。
type Subject struct {
	ID      string            `json:"id"`
	Region  string            `json:"region,omitempty"`
	Device  string            `json:"device,omitempty"`
	Version string            `json:"version,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Operator 是条件匹配运算符。
type Operator string

const (
	OpEq             Operator = "eq"              // 字符串相等
	OpNe             Operator = "ne"              // 字符串不等
	OpIn             Operator = "in"              // 属于 Values 集合
	OpNotIn          Operator = "not_in"          // 不属于 Values 集合
	OpPrefix         Operator = "prefix"          // 字符串前缀
	OpSuffix         Operator = "suffix"          // 字符串后缀
	OpContains       Operator = "contains"        // 字符串包含
	OpVersionGTE     Operator = "version_gte"     // 语义化版本 >= Values[0]
	OpVersionLTE     Operator = "version_lte"     // 语义化版本 <= Values[0]
	OpVersionBetween Operator = "version_between" // 语义化版本闭区间 [Values[0], Values[1]]
	OpTagAny         Operator = "tag_any"         // 主体标签与 Values 有任意交集
	OpTagAll         Operator = "tag_all"         // Values 全部包含在主体标签中
	OpExists         Operator = "exists"          // 属性存在且非空
)

// Condition 描述单条属性匹配条件。Attribute 可取内置属性名
// region/device/version/tag，也可取 Subject.Attrs 中的自定义键。
type Condition struct {
	Attribute string   `json:"attribute"`
	Operator  Operator `json:"operator"`
	Values    []string `json:"values,omitempty"`
}

// Rule 是开关的一条定向规则。规则按 (Priority 升序, ID 字典序) 排序，
// 第一条全部条件命中的规则生效；命中后再按 Percentage 做分桶放量。
type Rule struct {
	ID         string      `json:"id"`
	Priority   int         `json:"priority"`
	Conditions []Condition `json:"conditions,omitempty"`
	Percentage int         `json:"percentage"` // 0..100，桶值 < Percentage 时放量
	On         bool        `json:"on"`         // 放量命中时的开关结果
	Variation  string      `json:"variation,omitempty"`
}

// Dependency 声明开关的前置依赖：FlagKey 的评估结果必须等于 RequireOn。
type Dependency struct {
	FlagKey   string `json:"flag_key"`
	RequireOn bool   `json:"require_on"`
}

// Flag 是一个开关的完整定义。
type Flag struct {
	Key          string       `json:"key"`
	Description  string       `json:"description,omitempty"`
	DefaultOn    bool         `json:"default_on"` // 未命中任何规则时的回落值
	Rules        []Rule       `json:"rules,omitempty"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
	// Salt 为分桶盐值，缺省使用 Key。除非显式更换 Salt，
	// 否则调整 Percentage 不会改变主体所在的桶。
	Salt string `json:"salt,omitempty"`
}

// Override 是显式覆盖（含全局紧急熔断）。
// SubjectID 为 "*" 或空字符串时表示全局覆盖；否则为主体级覆盖。
// 覆盖优先级高于一切常规规则，且带有效期、操作者与原因。
type Override struct {
	FlagKey   string    `json:"flag_key"`
	SubjectID string    `json:"subject_id"`
	On        bool      `json:"on"`
	Operator  string    `json:"operator"`             // 操作者标识
	Reason    string    `json:"reason"`               // 操作原因
	ExpiresAt time.Time `json:"expires_at,omitempty"` // 零值表示长期有效
}

// Config 是某个已提交版本的完整配置，提交后不可变。
type Config struct {
	Version   int64            `json:"version"`
	Comment   string           `json:"comment,omitempty"`
	CreatedAt time.Time        `json:"created_at,omitempty"`
	Flags     map[string]*Flag `json:"flags"`
	// Overrides: flagKey -> subjectID("*" 表示全局) -> Override
	Overrides map[string]map[string]*Override `json:"overrides,omitempty"`
}

// Reason 说明评估结果的来源。
type Reason string

const (
	ReasonGlobalOverride     Reason = "global_override"
	ReasonSubjectOverride    Reason = "subject_override"
	ReasonRule               Reason = "rule"
	ReasonRuleThrottled      Reason = "rule_percentage_miss" // 命中规则但桶未落入放量比例
	ReasonDefault            Reason = "default"
	ReasonPrerequisiteFailed Reason = "prerequisite_failed"
)

// Trace 是一次评估的可追溯快照。
type Trace struct {
	FlagKey     string
	SubjectID   string
	Version     int64    // 评估所基于的配置版本
	On          bool     // 最终结果
	Reason      Reason   // 结果来源
	MatchedRule string   // 命中的规则 ID（未命中为 ""）
	Variation   string   // 命中规则的变体
	Bucket      int      // 分桶值 0..99，未参与分桶时为 -1
	Percentage  int      // 命中规则的放量比例
	PrereqChain []string // 前置依赖评估链
	EvaluatedAt time.Time
}

// Result 是一次评估的返回值。
type Result struct {
	On    bool
	Trace Trace
}

// Options 控制引擎行为。
type Options struct {
	// MaxDepth 是依赖评估的最大深度，默认 16。
	MaxDepth int
	// MaxEvalOps 是单次评估的比较操作预算（硬性上界），默认 100000。
	MaxEvalOps int
	// HistorySize 是保留的历史版本数，默认 32。
	HistorySize int
	// TraceSize 是内存中保留的最近评估记录条数，默认 4096。
	TraceSize int
	// Authorizer 判定操作者是否有权执行覆盖/熔断，默认拒绝所有操作者。
	Authorizer Authorizer
	// Now 注入时钟，便于测试；默认 time.Now。
	Now func() time.Time
	// AuditSink 可选：接收 JSON Lines 审计事件（提交/回滚/覆盖/评估）。
	AuditSink AuditSink
}

// Authorizer 负责覆盖类操作的鉴权。
type Authorizer interface {
	CanOverride(operator string) bool
}

// AuditSink 接收审计事件。
type AuditSink interface {
	Write(event []byte) error
}

// 引擎返回的哨兵错误。
var (
	ErrFlagNotFound    = errors.New("flagengine: flag not found")
	ErrInvalidConfig   = errors.New("flagengine: invalid config")
	ErrCycleDetected   = errors.New("flagengine: dependency cycle detected")
	ErrUnauthorized    = errors.New("flagengine: operator not authorized")
	ErrInvalidOverride = errors.New("flagengine: invalid override")
	ErrDepthLimit      = errors.New("flagengine: dependency depth limit exceeded")
	ErrEvalBudget      = errors.New("flagengine: evaluation operation budget exceeded")
	ErrVersionNotFound = errors.New("flagengine: config version not found")
	ErrEmptySubject    = errors.New("flagengine: subject id is empty")
)

// 确保 context 包在骨架阶段被引用，避免未使用导入。
var _ = context.Background
