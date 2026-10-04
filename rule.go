package flagengine

import "sort"

// 内置属性名常量。
const (
	AttrRegion  = "region"
	AttrDevice  = "device"
	AttrVersion = "version"
	AttrTag     = "tag"
	AttrID      = "id"
)

// conditionAttribute 解析条件属性值。
// 返回值：单值字符串、多值（tag）、是否存在。
func conditionAttribute(c Condition, s Subject) (string, []string, bool) {
	switch c.Attribute {
	case AttrRegion:
		return s.Region, nil, s.Region != ""
	case AttrDevice:
		return s.Device, nil, s.Device != ""
	case AttrVersion:
		return s.Version, nil, s.Version != ""
	case AttrID:
		return s.ID, nil, s.ID != ""
	case AttrTag:
		return "", s.Tags, len(s.Tags) > 0
	default:
		v, ok := s.Attrs[c.Attribute]
		return v, nil, ok && v != ""
	}
}

// matchCondition 判断单个条件是否命中主体。
// 未知运算符、缺失属性或非法值一律判定为不命中（AND 语义下即规则不命中），
// 绝不产生随机或未定义结果。
func matchCondition(c Condition, s Subject) bool {
	val, multi, exists := conditionAttribute(c, s)
	switch c.Operator {
	case OpExists:
		return exists
	case OpEq:
		return exists && len(c.Values) > 0 && val == c.Values[0]
	case OpNe:
		// 属性存在时按值不等判断；属性不存在不满足"不等"，
		// 以避免缺属性主体意外命中定向规则。
		return exists && len(c.Values) > 0 && val != c.Values[0]
	case OpIn:
		if !exists {
			return false
		}
		for _, v := range c.Values {
			if val == v {
				return true
			}
		}
		return false
	case OpNotIn:
		if !exists {
			return false
		}
		for _, v := range c.Values {
			if val == v {
				return false
			}
		}
		return true
	case OpPrefix:
		return exists && len(c.Values) > 0 && hasPrefix(val, c.Values[0])
	case OpSuffix:
		return exists && len(c.Values) > 0 && hasSuffix(val, c.Values[0])
	case OpContains:
		return exists && len(c.Values) > 0 && contains(val, c.Values[0])
	case OpVersionGTE, OpVersionLTE, OpVersionBetween:
		if !exists {
			return false
		}
		sub, ok := ParseSemVer(val)
		if !ok || len(c.Values) == 0 {
			return false
		}
		lo, okLo := ParseSemVer(c.Values[0])
		if !okLo {
			return false
		}
		switch c.Operator {
		case OpVersionGTE:
			return sub.compare(lo) >= 0
		case OpVersionLTE:
			return sub.compare(lo) <= 0
		case OpVersionBetween:
			if len(c.Values) < 2 {
				return false
			}
			hi, okHi := ParseSemVer(c.Values[1])
			if !okHi {
				return false
			}
			return sub.compare(lo) >= 0 && sub.compare(hi) <= 0
		}
		return false
	case OpTagAny:
		if !exists {
			return false
		}
		want := stringSet(c.Values)
		for _, t := range multi {
			if _, ok := want[t]; ok {
				return true
			}
		}
		return false
	case OpTagAll:
		if !exists {
			return false
		}
		have := stringSet(multi)
		for _, v := range c.Values {
			if _, ok := have[v]; !ok {
				return false
			}
		}
		return true
	default:
		// 未知运算符：保守地不命中
		return false
	}
}

// matchRule 判断规则的全部条件是否命中（AND 语义）。空条件规则命中所有主体。
func matchRule(r Rule, s Subject) bool {
	for _, c := range r.Conditions {
		if !matchCondition(c, s) {
			return false
		}
	}
	return true
}

// sortRules 返回按 (Priority 升序, ID 字典序) 排好序的规则副本。
// 二级排序键保证任何环境下规则优先级完全确定。
func sortRules(rules []Rule) []Rule {
	out := append([]Rule(nil), rules...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// firstMatch 在已排序规则中返回第一条命中规则及其下标；未命中返回 -1,nil。
func firstMatch(rules []Rule, s Subject) (int, *Rule) {
	for i := range rules {
		if matchRule(rules[i], s) {
			return i, &rules[i]
		}
	}
	return -1, nil
}

func stringSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}

// 以下小工具避免为简单操作引入 strings 包名混杂，保持匹配逻辑直观。

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func hasSuffix(s, sfx string) bool {
	return len(s) >= len(sfx) && s[len(s)-len(sfx):] == sfx
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
