package featureflag

import (
	"strconv"
	"strings"
)

// matchCondition 判断单个条件是否命中。
// 属性缺失、候选值为空、版本无法解析等异常输入一律返回 false（确定性的不命中），
// 绝不产生随机或未定义结果。
func matchCondition(ctx EvalContext, c Condition) bool {
	switch c.Op {
	case OpEq, OpIn:
		v, ok := ctx.Attributes[c.Attr]
		if !ok {
			return false
		}
		for _, cand := range c.Values {
			if v == cand {
				return true
			}
		}
		return false
	case OpNeq:
		v, ok := ctx.Attributes[c.Attr]
		if !ok {
			return false
		}
		for _, cand := range c.Values {
			if v == cand {
				return false
			}
		}
		return true
	case OpVersionInRange:
		if len(c.Values) != 2 {
			return false
		}
		v, ok := ctx.Attributes[c.Attr]
		if !ok {
			return false
		}
		return versionInRange(v, c.Values[0], c.Values[1])
	case OpTagContains:
		for _, cand := range c.Values {
			for _, t := range ctx.Tags {
				if t == cand {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

// matchRule 判断规则的全部条件（逻辑与）是否命中。
// 空条件列表视为恒真（规则退化为纯放量规则）。
func matchRule(ctx EvalContext, r Rule) bool {
	for _, c := range r.Conditions {
		if !matchCondition(ctx, c) {
			return false
		}
	}
	return true
}

// versionInRange 判断 v 是否落在语义化版本闭区间 [min, max]。
// 任一版本无法解析时返回 false。
func versionInRange(v, min, max string) bool {
	vv, err := parseSemver(v)
	if err != nil {
		return false
	}
	lo, err := parseSemver(min)
	if err != nil {
		return false
	}
	hi, err := parseSemver(max)
	if err != nil {
		return false
	}
	return compareSemver(vv, lo) >= 0 && compareSemver(vv, hi) <= 0
}

// semver 是语义化版本的三元组；预发布与构建元数据不参与比较。
type semver struct{ major, minor, patch int }

// parseSemver 解析 "1.2.3"，允许省略 minor/patch（按 0 补齐），
// 允许 "v" 前缀与 "-预发布"/"+构建" 后缀（后缀被忽略）。
func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return semver{}, strconv.ErrSyntax
	}
	var out [3]int
	for i, p := range parts {
		if p == "" {
			return semver{}, strconv.ErrSyntax
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, strconv.ErrSyntax
		}
		out[i] = n
	}
	return semver{out[0], out[1], out[2]}, nil
}

func compareSemver(a, b semver) int {
	if a.major != b.major {
		return cmpInt(a.major, b.major)
	}
	if a.minor != b.minor {
		return cmpInt(a.minor, b.minor)
	}
	return cmpInt(a.patch, b.patch)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
