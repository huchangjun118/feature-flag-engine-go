package featureflag

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Store 保存配置的版本历史，并以原子方式发布当前版本。
//
// 并发模型：评估方通过 Snapshot 原子加载当前版本指针，此后整个评估
// 只访问该不可变快照；更新方先完整构造并校验新版本，再一次原子替换。
// 因此慢评估不阻塞更新，更新也不会让进行中的评估看到半更新状态。
type Store struct {
	current atomic.Pointer[Config]

	mu      sync.Mutex // 仅保护历史与元信息，不在评估路径上
	history map[int64]*Config
	// allowedOperators 为允许执行覆盖操作的操作者集合，空表示拒绝一切覆盖。
	allowedOperators map[string]bool
}

// NewStore 创建 Store。allowedOperators 为允许设置覆盖的操作者名单。
func NewStore(allowedOperators ...string) *Store {
	s := &Store{
		history:          map[int64]*Config{},
		allowedOperators: map[string]bool{},
	}
	for _, op := range allowedOperators {
		s.allowedOperators[op] = true
	}
	return s
}

// Update 校验并原子发布一个新版本；失败时保持旧版本继续服务。
// 新版本号必须严格大于当前版本号，保证版本单调递增。
func (s *Store) Update(cfg Config) error {
	if err := validateConfig(&cfg); err != nil {
		return fmt.Errorf("update rejected: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.current.Load(); cur != nil && cfg.Version <= cur.Version {
		return fmt.Errorf("update rejected: version %d not greater than current %d",
			cfg.Version, cur.Version)
	}
	cp := deepCopyConfig(&cfg)
	s.history[cp.Version] = cp
	s.current.Store(cp)
	return nil
}

// Rollback 回滚到某个历史版本，返回回滚后生效的版本号。
// 回滚后同一输入的评估结果与该版本首次生效时一致（快照不可变）。
func (s *Store) Rollback(version int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.history[version]
	if !ok {
		return 0, fmt.Errorf("rollback rejected: version %d not found", version)
	}
	s.current.Store(cp)
	return version, nil
}

// AddOverride 在当前版本基础上追加一条覆盖并作为新版本原子发布。
// 覆盖优先级最高，且必须经过鉴权：操作者不在白名单内将被拒绝。
func (s *Store) AddOverride(ov Override) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowedOperators[ov.Operator] {
		return fmt.Errorf("override rejected: operator %q is not authorized", ov.Operator)
	}
	cur := s.current.Load()
	if cur == nil {
		return fmt.Errorf("override rejected: no active config")
	}
	next := deepCopyConfig(cur)
	next.Version = cur.Version + 1
	next.Overrides = append(next.Overrides, ov)
	if err := validateConfig(next); err != nil {
		return fmt.Errorf("override rejected: %w", err)
	}
	s.history[next.Version] = next
	s.current.Store(next)
	return nil
}

// Snapshot 返回当前生效版本的只读快照；无配置时返回 nil。
// 调用方不得修改返回的 Config。
func (s *Store) Snapshot() *Config {
	return s.current.Load()
}

// Versions 返回所有已提交版本号（升序）。
func (s *Store) Versions() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.history))
	for v := range s.history {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// deepCopyConfig 深拷贝配置，保证已发布快照不可变：
// 调用方后续对入参的修改不会影响已生效或历史版本。
func deepCopyConfig(cfg *Config) *Config {
	out := &Config{
		Version: cfg.Version,
		Flags:   make(map[string]Flag, len(cfg.Flags)),
	}
	for k, f := range cfg.Flags {
		nf := Flag{
			Key:          f.Key,
			Description:  f.Description,
			DefaultValue: f.DefaultValue,
		}
		if f.Rules != nil {
			nf.Rules = make([]Rule, len(f.Rules))
			for i, r := range f.Rules {
				nr := r
				nr.Conditions = append([]Condition(nil), r.Conditions...)
				for j, c := range nr.Conditions {
					nr.Conditions[j].Values = append([]string(nil), c.Values...)
				}
				if r.Rollout != nil {
					ro := *r.Rollout
					nr.Rollout = &ro
				}
				nf.Rules[i] = nr
			}
		}
		nf.DependsOn = append([]Dependency(nil), f.DependsOn...)
		// 提交时预排序：评估顺序按 (Priority, ID) 确定，与 map 遍历顺序无关。
		sort.Slice(nf.Rules, func(i, j int) bool {
			if nf.Rules[i].Priority != nf.Rules[j].Priority {
				return nf.Rules[i].Priority < nf.Rules[j].Priority
			}
			return nf.Rules[i].ID < nf.Rules[j].ID
		})
		out.Flags[k] = nf
	}
	out.Overrides = append([]Override(nil), cfg.Overrides...)
	return out
}

// activeOverride 在快照中查找对 (flagKey, subjectID) 生效的覆盖：
// 先查主体级覆盖，再查全局熔断；过期覆盖视为不存在。
// 多条同时命中时取最新（切片靠后）的一条。
func activeOverride(cfg *Config, flagKey, subjectID string, now time.Time) *Override {
	// 倒序遍历：同一次评估中后追加的覆盖优先。
	var global *Override
	for i := len(cfg.Overrides) - 1; i >= 0; i-- {
		ov := &cfg.Overrides[i]
		if ov.FlagKey != flagKey || !now.Before(ov.ExpiresAt) {
			continue
		}
		if ov.SubjectID == subjectID {
			return ov // 主体级覆盖优先于全局熔断
		}
		if ov.SubjectID == "" && global == nil {
			global = ov
		}
	}
	return global
}
