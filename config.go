package flagengine

import (
	"encoding/json"
	"fmt"
	"time"
)

// snapshot 是不可变的已提交版本：评估只读快照，更新只换快照指针。
type snapshot struct {
	config     *Config
	rulesOrder map[string][]Rule // 每个开关排好序的规则
}

// AuditEvent 是写往 AuditSink 的审计事件。
type AuditEvent struct {
	Type      string `json:"type"` // commit|rollback|override|override_remove|reject
	Version   int64  `json:"version"`
	FlagKey   string `json:"flag_key,omitempty"`
	SubjectID string `json:"subject_id,omitempty"`
	On        *bool  `json:"on,omitempty"`
	Operator  string `json:"operator,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
	At        string `json:"at"`
}

// audit 写入一条审计事件；sink 缺失或写入失败均不影响主流程。
func (e *Engine) audit(ev AuditEvent) {
	if e.opts.AuditSink == nil {
		return
	}
	if ev.At == "" {
		ev.At = e.now().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = e.opts.AuditSink.Write(append(b, '\n'))
}

// buildSnapshot 对配置做深拷贝、校验并构建不可变快照。
// 校验失败返回错误，调用方继续保留旧快照服务。
func buildSnapshot(cfg *Config) (*snapshot, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is nil", ErrInvalidConfig)
	}
	cloned := cloneConfig(cfg)
	if err := validateConfig(cloned); err != nil {
		return nil, err
	}
	if err := detectCycle(cloned); err != nil {
		return nil, err
	}
	snap := &snapshot{
		config:     cloned,
		rulesOrder: make(map[string][]Rule, len(cloned.Flags)),
	}
	// validateConfig 已保证 flag.Key == map key（缺失会被规范化补齐），
	// 这里以 flag.Key 为准建立规则顺序，保证与 computeBucket/eval 使用的键一致。
	for _, flag := range cloned.Flags {
		snap.rulesOrder[flag.Key] = sortRules(flag.Rules)
	}
	return snap, nil
}

// validateConfig 校验配置完整性。
func validateConfig(cfg *Config) error {
	if cfg.Version <= 0 {
		return fmt.Errorf("%w: version must be positive", ErrInvalidConfig)
	}
	if cfg.Flags == nil {
		return fmt.Errorf("%w: flags map is nil", ErrInvalidConfig)
	}
	ruleIDs := make(map[string]map[string]bool, len(cfg.Flags))
	for key, flag := range cfg.Flags {
		if flag == nil {
			return fmt.Errorf("%w: flag %q is nil", ErrInvalidConfig, key)
		}
		if flag.Key == "" {
			flag.Key = key // 容忍 key 仅在 map 上给出，规范化补齐
		} else if flag.Key != key {
			return fmt.Errorf("%w: flag map key %q does not match flag.Key %q",
				ErrInvalidConfig, key, flag.Key)
		}
		ids := make(map[string]bool, len(flag.Rules))
		for i := range flag.Rules {
			r := &flag.Rules[i]
			if r.ID == "" {
				return fmt.Errorf("%w: flag %q has a rule with empty id",
					ErrInvalidConfig, key)
			}
			if ids[r.ID] {
				return fmt.Errorf("%w: flag %q has duplicate rule id %q",
					ErrInvalidConfig, key, r.ID)
			}
			ids[r.ID] = true
			if r.Percentage < 0 || r.Percentage > 100 {
				return fmt.Errorf("%w: flag %q rule %q percentage %d out of range [0,100]",
					ErrInvalidConfig, key, r.ID, r.Percentage)
			}
			for _, c := range r.Conditions {
				if !validOperator(c.Operator) {
					return fmt.Errorf("%w: flag %q rule %q has unknown operator %q",
						ErrInvalidConfig, key, r.ID, c.Operator)
				}
				if c.Attribute == "" {
					return fmt.Errorf("%w: flag %q rule %q has condition with empty attribute",
						ErrInvalidConfig, key, r.ID)
				}
			}
		}
		ruleIDs[key] = ids

		for _, dep := range flag.Dependencies {
			if dep.FlagKey == "" {
				return fmt.Errorf("%w: flag %q has empty dependency key",
					ErrInvalidConfig, key)
			}
			if dep.FlagKey == key {
				return &CycleError{Cycle: []string{key, key}}
			}
			if _, ok := cfg.Flags[dep.FlagKey]; !ok {
				return fmt.Errorf("%w: flag %q depends on unknown flag %q",
					ErrInvalidConfig, key, dep.FlagKey)
			}
		}
	}
	// 覆盖项校验：flag 必须存在；主体/全局键不得异常。
	for fk, subs := range cfg.Overrides {
		if _, ok := cfg.Flags[fk]; !ok {
			return fmt.Errorf("%w: override references unknown flag %q",
				ErrInvalidConfig, fk)
		}
		for sid, ov := range subs {
			if ov == nil {
				return fmt.Errorf("%w: nil override for %q/%q",
					ErrInvalidConfig, fk, sid)
			}
			if ov.FlagKey != fk {
				return fmt.Errorf("%w: override flag key %q != map key %q",
					ErrInvalidConfig, ov.FlagKey, fk)
			}
			if ov.SubjectID != sid {
				return fmt.Errorf("%w: override subject %q != map key %q",
					ErrInvalidConfig, ov.SubjectID, sid)
			}
			if ov.Operator == "" {
				return fmt.Errorf("%w: override %q/%q missing operator",
					ErrInvalidConfig, fk, sid)
			}
		}
	}
	return nil
}

func validOperator(op Operator) bool {
	switch op {
	case OpEq, OpNe, OpIn, OpNotIn, OpPrefix, OpSuffix, OpContains,
		OpVersionGTE, OpVersionLTE, OpVersionBetween, OpTagAny, OpTagAll, OpExists:
		return true
	default:
		return false
	}
}

// cloneConfig 深拷贝配置，保证外部后续修改不影响已提交版本。
func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return nil
	}
	out := &Config{
		Version:   cfg.Version,
		Comment:   cfg.Comment,
		CreatedAt: cfg.CreatedAt,
		Flags:     make(map[string]*Flag, len(cfg.Flags)),
		Overrides: make(map[string]map[string]*Override, len(cfg.Overrides)),
	}
	for key, flag := range cfg.Flags {
		if flag == nil {
			out.Flags[key] = nil
			continue
		}
		nf := &Flag{
			Key:         flag.Key,
			Description: flag.Description,
			DefaultOn:   flag.DefaultOn,
			Salt:        flag.Salt,
		}
		nf.Rules = make([]Rule, len(flag.Rules))
		for i, r := range flag.Rules {
			nr := Rule{
				ID:         r.ID,
				Priority:   r.Priority,
				Percentage: r.Percentage,
				On:         r.On,
				Variation:  r.Variation,
			}
			nr.Conditions = make([]Condition, len(r.Conditions))
			for j, c := range r.Conditions {
				nc := Condition{
					Attribute: c.Attribute,
					Operator:  c.Operator,
				}
				if c.Values != nil {
					nc.Values = append([]string(nil), c.Values...)
				}
				nr.Conditions[j] = nc
			}
			nf.Rules[i] = nr
		}
		nf.Dependencies = make([]Dependency, len(flag.Dependencies))
		copy(nf.Dependencies, flag.Dependencies)
		out.Flags[key] = nf
	}
	for fk, subs := range cfg.Overrides {
		m := make(map[string]*Override, len(subs))
		for sid, ov := range subs {
			if ov == nil {
				m[sid] = nil
				continue
			}
			cp := *ov
			m[sid] = &cp
		}
		out.Overrides[fk] = m
	}
	return out
}
