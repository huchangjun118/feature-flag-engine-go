package featureflag

import (
	"fmt"
	"sort"
	"strings"
)

// validateConfig 校验一份配置：结构合法性 + 依赖图无环。
// 任何错误都会导致整份配置被拒绝（原子更新，不允许部分生效），
// 错误信息中携带问题来源（开关 key / 规则 ID / 依赖路径）。
func validateConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	keys := make([]string, 0, len(cfg.Flags))
	for k := range cfg.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if k == "" {
			return fmt.Errorf("flag with empty key")
		}
		f := cfg.Flags[k]
		if f.Key != "" && f.Key != k {
			return fmt.Errorf("flag %q: key mismatch with map entry %q", f.Key, k)
		}
		for _, r := range f.Rules {
			if r.ID == "" {
				return fmt.Errorf("flag %q: rule with empty id", k)
			}
			if r.Rollout != nil {
				if r.Rollout.Percentage < 0 || r.Rollout.Percentage > 100 {
					return fmt.Errorf("flag %q rule %q: rollout percentage %d out of [0,100]",
						k, r.ID, r.Rollout.Percentage)
				}
			}
			for _, c := range r.Conditions {
				switch c.Op {
				case OpEq, OpIn, OpNeq, OpTagContains:
					if len(c.Values) == 0 {
						return fmt.Errorf("flag %q rule %q: condition %s requires values", k, r.ID, c.Op)
					}
				case OpVersionInRange:
					if len(c.Values) != 2 {
						return fmt.Errorf("flag %q rule %q: version_in_range requires exactly 2 values", k, r.ID)
					}
					if _, err := parseSemver(c.Values[0]); err != nil {
						return fmt.Errorf("flag %q rule %q: invalid min version %q", k, r.ID, c.Values[0])
					}
					if _, err := parseSemver(c.Values[1]); err != nil {
						return fmt.Errorf("flag %q rule %q: invalid max version %q", k, r.ID, c.Values[1])
					}
				default:
					return fmt.Errorf("flag %q rule %q: unknown condition op %q", k, r.ID, c.Op)
				}
			}
		}
		for _, d := range f.DependsOn {
			if d.FlagKey == "" {
				return fmt.Errorf("flag %q: dependency with empty flag key", k)
			}
			if d.FlagKey == k {
				return fmt.Errorf("flag %q: self dependency", k)
			}
			if _, ok := cfg.Flags[d.FlagKey]; !ok {
				return fmt.Errorf("flag %q: dependency %q not found", k, d.FlagKey)
			}
		}
	}

	for _, ov := range cfg.Overrides {
		if ov.ID == "" {
			return fmt.Errorf("override with empty id")
		}
		if _, ok := cfg.Flags[ov.FlagKey]; !ok {
			return fmt.Errorf("override %q: flag %q not found", ov.ID, ov.FlagKey)
		}
		if ov.Operator == "" {
			return fmt.Errorf("override %q: operator is required", ov.ID)
		}
		if ov.Reason == "" {
			return fmt.Errorf("override %q: reason is required", ov.ID)
		}
		if ov.ExpiresAt.IsZero() {
			return fmt.Errorf("override %q: expires_at is required", ov.ID)
		}
	}

	if cycle := findDependencyCycle(cfg); cycle != nil {
		return fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// findDependencyCycle 在依赖图中寻找环，返回构成环的开关 key 路径；
// 无环返回 nil。遍历顺序确定（按 key 排序），保证报错可复现。
func findDependencyCycle(cfg *Config) []string {
	const (
		white = 0 // 未访问
		gray  = 1 // 在当前 DFS 栈上
		black = 2 // 已完成
	)
	color := map[string]int{}
	var stack []string

	keys := make([]string, 0, len(cfg.Flags))
	for k := range cfg.Flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var visit func(k string) []string
	visit = func(k string) []string {
		color[k] = gray
		stack = append(stack, k)
		deps := append([]Dependency(nil), cfg.Flags[k].DependsOn...)
		sort.Slice(deps, func(i, j int) bool { return deps[i].FlagKey < deps[j].FlagKey })
		for _, d := range deps {
			switch color[d.FlagKey] {
			case gray:
				// 找到环：截取栈中从 d.FlagKey 开始的部分。
				for i, s := range stack {
					if s == d.FlagKey {
						return append(append([]string(nil), stack[i:]...), d.FlagKey)
					}
				}
			case white:
				if cyc := visit(d.FlagKey); cyc != nil {
					return cyc
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = black
		return nil
	}

	for _, k := range keys {
		if color[k] == white {
			if cyc := visit(k); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}
