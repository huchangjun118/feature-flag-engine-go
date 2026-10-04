package flagengine

// RoleAuthorizer 是基于操作者名单的简单鉴权器。
type RoleAuthorizer struct {
	allowed map[string]bool
}

// NewRoleAuthorizer 创建鉴权器，operators 为允许执行覆盖/熔断操作的名单。
func NewRoleAuthorizer(operators ...string) *RoleAuthorizer {
	a := &RoleAuthorizer{allowed: make(map[string]bool, len(operators))}
	for _, op := range operators {
		if op != "" {
			a.allowed[op] = true
		}
	}
	return a
}

// CanOverride 判断操作者是否有权执行覆盖/熔断。
// 空操作者永远无权，避免匿名越权。
func (a *RoleAuthorizer) CanOverride(operator string) bool {
	if operator == "" {
		return false
	}
	return a.allowed[operator]
}

// DenyAllAuthorizer 拒绝所有操作者（引擎默认）。
type DenyAllAuthorizer struct{}

func (DenyAllAuthorizer) CanOverride(string) bool { return false }
