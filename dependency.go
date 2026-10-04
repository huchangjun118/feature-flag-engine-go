package flagengine

import (
	"strconv"
	"strings"
)

// CycleError 描述检测到的循环依赖链。
type CycleError struct {
	Cycle []string // 构成环的开关 key 链，首尾相同
}

func (e *CycleError) Error() string {
	return "flagengine: dependency cycle detected: " + strings.Join(e.Cycle, " -> ")
}

func (e *CycleError) Is(target error) bool { return target == ErrCycleDetected }

// detectCycle 对配置中全部开关做依赖环检测（DFS 三色标记）。
// 依赖指向不存在的开关不算环（由 validateConfig 单独报错）。
// 返回 nil 表示无环；否则返回包含首个发现环的 CycleError。
func detectCycle(cfg *Config) error {
	const (
		white = 0 // 未访问
		gray  = 1 // 在当前 DFS 栈中
		black = 2 // 已完成
	)
	color := make(map[string]int, len(cfg.Flags))
	stack := make([]string, 0, 16)

	var dfs func(key string) error
	dfs = func(key string) error {
		color[key] = gray
		stack = append(stack, key)
		flag := cfg.Flags[key]
		for _, dep := range flag.Dependencies {
			depFlag, ok := cfg.Flags[dep.FlagKey]
			if !ok {
				continue // 缺失依赖交给 validateConfig
			}
			switch color[dep.FlagKey] {
			case gray:
				// 找到环：从栈中 dep.FlagKey 第一次出现处切出环路径
				start := 0
				for i, k := range stack {
					if k == dep.FlagKey {
						start = i
						break
					}
				}
				cycle := append(append([]string(nil), stack[start:]...), dep.FlagKey)
				return &CycleError{Cycle: cycle}
			case white:
				if err := dfs(depFlag.Key); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
		return nil
	}

	// 固定遍历顺序，保证错误报告可复现，不受 map 遍历影响。
	keys := make([]string, 0, len(cfg.Flags))
	for k := range cfg.Flags {
		keys = append(keys, k)
	}
	// 简单字典序排序
	sortStrings(keys)
	for _, k := range keys {
		if color[k] == white {
			if err := dfs(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortStrings 对字符串切片做升序排序（避免本文件引入 sort 包名歧义）。
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// SemVersion 是解析后的语义化版本。
type SemVersion struct {
	Major, Minor, Patch int
}

// ParseSemVer 解析形如 "1.2.3" 的版本；缺失段补 0（"1" => 1.0.0），
// 忽略预发布/构建元数据后缀前的非数字内容时直接判非法。
// 非法输入返回 false（绝不 panic），异常输入不会产生随机结果。
func ParseSemVer(s string) (SemVersion, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return SemVersion{}, false
	}
	// 去掉常见的构建号后缀，如 1.2.3-build5 / 1.2.3+sha1 / 1.2.3-rc1
	for _, sep := range []string{"+", "-", "_", " "} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	if strings.HasPrefix(s, "v") || strings.HasPrefix(s, "V") {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return SemVersion{}, false
	}
	nums := [3]int{}
	for i, p := range parts {
		if p == "" {
			return SemVersion{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return SemVersion{}, false
		}
		nums[i] = n
	}
	return SemVersion{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}

// compare 返回 -1/0/1 表示 v 小于/等于/大于 o。
func (v SemVersion) compare(o SemVersion) int {
	switch {
	case v.Major != o.Major:
		return intCmp(v.Major, o.Major)
	case v.Minor != o.Minor:
		return intCmp(v.Minor, o.Minor)
	default:
		return intCmp(v.Patch, o.Patch)
	}
}

func intCmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
