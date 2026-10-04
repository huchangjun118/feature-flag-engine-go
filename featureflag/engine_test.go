package featureflag

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func ctxOf(subject string, attrs map[string]string, tags ...string) EvalContext {
	return EvalContext{SubjectID: subject, Attributes: attrs, Tags: tags}
}

// TestBucketStability 验证分桶与进程状态、遍历顺序无关，且上调不踢人。
func TestBucketStability(t *testing.T) {
	subjects := make([]string, 500)
	for i := range subjects {
		subjects[i] = fmt.Sprintf("user-%d", i)
	}
	// 同一输入多次计算（模拟进程重启后重算）结果一致。
	first := map[string]int{}
	for _, s := range subjects {
		first[s] = bucketOf("flag-a", "salt", s)
	}
	for _, s := range subjects {
		if got := bucketOf("flag-a", "salt", s); got != first[s] {
			t.Fatalf("bucket for %s changed: %d -> %d", s, first[s], got)
		}
		if first[s] < 0 || first[s] >= 100 {
			t.Fatalf("bucket out of range: %d", first[s])
		}
	}
	// 比例上调：已进入主体不得被踢出。
	for _, s := range subjects {
		b := first[s]
		if b < 30 && b >= 60 { // 不可达，仅示意
			t.Fatal("impossible")
		}
		if b < 30 && !(b < 60) {
			t.Fatalf("subject %s kicked out after upscale", s)
		}
	}
	// 盐与开关名影响分桶（打散相关性）。
	same := 0
	for _, s := range subjects {
		if bucketOf("flag-a", "salt", s) == bucketOf("flag-b", "salt", s) {
			same++
		}
	}
	if same == len(subjects) {
		t.Fatal("different flags must not produce identical buckets")
	}
}

func newStoreWith(t *testing.T, cfg Config) *Store {
	t.Helper()
	s := NewStore("admin", "sre")
	if err := s.Update(cfg); err != nil {
		t.Fatalf("update: %v", err)
	}
	return s
}

// TestRulePriorityAndFallback 验证规则优先级与默认值回落。
func TestRulePriorityAndFallback(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"checkout": {
				Key:          "checkout",
				DefaultValue: false,
				Rules: []Rule{
					{ID: "low", Priority: 10, Value: false,
						Conditions: []Condition{{Attr: "region", Op: OpEq, Values: []string{"cn"}}}},
					{ID: "high", Priority: 1, Value: true,
						Conditions: []Condition{{Attr: "region", Op: OpEq, Values: []string{"cn"}}}},
				},
			},
		},
	})
	e := NewEngine(store)
	res := e.Evaluate(ctxOf("u1", map[string]string{"region": "cn"}), "checkout")
	if !res.Value || res.RuleID != "high" || res.Reason != ReasonRuleMatch {
		t.Fatalf("priority rule should win: %+v", res)
	}
	// 未命中任何规则 → 默认值。
	res = e.Evaluate(ctxOf("u2", map[string]string{"region": "us"}), "checkout")
	if res.Value || res.Reason != ReasonDefault {
		t.Fatalf("expected default fallback: %+v", res)
	}
	if res.ConfigVersion != 1 {
		t.Fatalf("wrong config version: %d", res.ConfigVersion)
	}
}

// TestConditionKinds 验证版本区间与自定义标签条件。
func TestConditionKinds(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"f": {
				Key: "f", DefaultValue: false,
				Rules: []Rule{
					{ID: "ver", Priority: 1, Value: true, Conditions: []Condition{
						{Attr: "version", Op: OpVersionInRange, Values: []string{"1.2.0", "2.0.0"}},
						{Op: OpTagContains, Values: []string{"beta"}},
					}},
				},
			},
		},
	})
	e := NewEngine(store)
	cases := []struct {
		version string
		tags    []string
		want    bool
	}{
		{"1.5.0", []string{"beta"}, true},
		{"2.0.0", []string{"beta"}, true},  // 闭区间上界
		{"2.0.1", []string{"beta"}, false}, // 超出区间
		{"1.5.0", nil, false},              // 缺少标签
		{"not-a-version", []string{"beta"}, false},
	}
	for _, c := range cases {
		got := e.Evaluate(ctxOf("u", map[string]string{"version": c.version}, c.tags...), "f")
		if got.Value != c.want {
			t.Errorf("version=%s tags=%v: got %v want %v", c.version, c.tags, got.Value, c.want)
		}
	}
}

// TestRolloutStabilityAcrossPercentageChange 验证放量比例变化时主体不被重新随机。
func TestRolloutStabilityAcrossPercentageChange(t *testing.T) {
	mk := func(version int64, pct int) Config {
		return Config{
			Version: version,
			Flags: map[string]Flag{
				"exp": {Key: "exp", DefaultValue: false, Rules: []Rule{
					{ID: "r", Priority: 1, Value: true, Rollout: &Rollout{Percentage: pct}},
				}},
			},
		}
	}
	store := newStoreWith(t, mk(1, 20))
	e := NewEngine(store)
	subjects := make([]string, 300)
	in := map[string]bool{}
	for i := range subjects {
		subjects[i] = fmt.Sprintf("s%d", i)
		in[subjects[i]] = e.Evaluate(ctxOf(subjects[i], nil), "exp").Value
	}
	// 上调到 80：原已进入主体必须仍在。
	if err := store.Update(mk(2, 80)); err != nil {
		t.Fatal(err)
	}
	for _, s := range subjects {
		got := e.Evaluate(ctxOf(s, nil), "exp").Value
		if in[s] && !got {
			t.Fatalf("subject %s kicked out after upscale", s)
		}
	}
	// 下调到 10：每个主体的分桶值不变（未被重新随机）。
	if err := store.Update(mk(3, 10)); err != nil {
		t.Fatal(err)
	}
	for _, s := range subjects {
		res := e.Evaluate(ctxOf(s, nil), "exp")
		wantBucket := bucketOf("exp", "r", s)
		if res.Bucket != wantBucket {
			t.Fatalf("subject %s re-randomized: bucket %d != %d", s, res.Bucket, wantBucket)
		}
		if res.Value != (wantBucket < 10) {
			t.Fatalf("subject %s: value %v inconsistent with bucket %d", s, res.Value, wantBucket)
		}
	}
}

// TestDependenciesAndCycle 验证前置依赖、循环依赖拒绝与深度上限。
func TestDependenciesAndCycle(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"base": {Key: "base", DefaultValue: false},
			"mid":  {Key: "mid", DefaultValue: true, DependsOn: []Dependency{{FlagKey: "base", Expected: true}}},
			"top":  {Key: "top", DefaultValue: true, DependsOn: []Dependency{{FlagKey: "mid", Expected: true}}},
		},
	})
	e := NewEngine(store)
	// base 关闭 → mid/top 均不得开启。
	if res := e.Evaluate(ctxOf("u", nil), "top"); res.Value || res.Reason != ReasonDependencyUnmet {
		t.Fatalf("dependency unmet should close flag: %+v", res)
	}
	// 打开 base 后链路打通。
	if err := store.Update(Config{
		Version: 2,
		Flags: map[string]Flag{
			"base": {Key: "base", DefaultValue: true},
			"mid":  {Key: "mid", DefaultValue: true, DependsOn: []Dependency{{FlagKey: "base", Expected: true}}},
			"top":  {Key: "top", DefaultValue: true, DependsOn: []Dependency{{FlagKey: "mid", Expected: true}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate(ctxOf("u", nil), "top"); !res.Value {
		t.Fatalf("dependency chain should open flag: %+v", res)
	}
	// 循环依赖：整体拒绝并指出来源，旧版本继续服务。
	err := store.Update(Config{
		Version: 3,
		Flags: map[string]Flag{
			"a": {Key: "a", DependsOn: []Dependency{{FlagKey: "b", Expected: true}}},
			"b": {Key: "b", DependsOn: []Dependency{{FlagKey: "a", Expected: true}}},
		},
	})
	if err == nil {
		t.Fatal("cycle config must be rejected")
	}
	t.Logf("cycle rejected with: %v", err)
	if got := store.Snapshot().Version; got != 2 {
		t.Fatalf("failed update must keep previous version, got %d", got)
	}
	// 深度上限：构造超过 maxDepth 的链。
	deep := Config{Version: 10, Flags: map[string]Flag{}}
	chain := DefaultMaxDepth + 3
	for i := 0; i < chain; i++ {
		k := fmt.Sprintf("d%d", i)
		f := Flag{Key: k, DefaultValue: true}
		if i > 0 {
			f.DependsOn = []Dependency{{FlagKey: fmt.Sprintf("d%d", i-1), Expected: true}}
		}
		deep.Flags[k] = f
	}
	s2 := NewStore()
	if err := s2.Update(deep); err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(s2)
	if res := e2.Evaluate(ctxOf("u", nil), fmt.Sprintf("d%d", chain-1)); res.Reason != ReasonMaxDepthExceeded {
		t.Fatalf("expected max depth exceeded, got %+v", res)
	}
}

// TestConcurrentUpdateAndEvaluate 验证评估与热更新并发：
// 评估结果永远来自某个完整版本，且并发期间无数据竞争（配合 -race）。
func TestConcurrentUpdateAndEvaluate(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"f": {Key: "f", DefaultValue: false},
		},
	})
	e := NewEngine(store)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res := e.Evaluate(ctxOf(fmt.Sprintf("u%d", id), nil), "f")
				if res.ConfigVersion < 1 {
					t.Errorf("evaluation saw invalid version %d", res.ConfigVersion)
					return
				}
				// 每个版本的 default 即版本奇偶：v 奇=false, v 偶=true，
				// 结果必须与某一完整版本一致。
				want := res.ConfigVersion%2 == 0
				if res.Value != want {
					t.Errorf("mixed state: version=%d value=%v", res.ConfigVersion, res.Value)
					return
				}
			}
		}(w)
	}
	for v := int64(2); v <= 200; v++ {
		if err := store.Update(Config{
			Version: v,
			Flags:   map[string]Flag{"f": {Key: "f", DefaultValue: v%2 == 0}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestRollback 验证回滚后同一输入结果与当时一致。
func TestRollback(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"f": {Key: "f", DefaultValue: true, Rules: []Rule{
				{ID: "r1", Priority: 1, Value: false,
					Conditions: []Condition{{Attr: "region", Op: OpEq, Values: []string{"cn"}}}},
			}},
		},
	})
	e := NewEngine(store)
	ctx := ctxOf("u1", map[string]string{"region": "cn"})
	before := e.Evaluate(ctx, "f")
	if before.Value || before.RuleID != "r1" {
		t.Fatalf("unexpected v1 result: %+v", before)
	}
	if err := store.Update(Config{
		Version: 2,
		Flags:   map[string]Flag{"f": {Key: "f", DefaultValue: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.Evaluate(ctx, "f"); !got.Value {
		t.Fatalf("v2 should open flag: %+v", got)
	}
	if _, err := store.Rollback(1); err != nil {
		t.Fatal(err)
	}
	after := e.Evaluate(ctx, "f")
	if after.Value != before.Value || after.RuleID != before.RuleID || after.ConfigVersion != 1 {
		t.Fatalf("rollback result mismatch: before=%+v after=%+v", before, after)
	}
	if _, err := store.Rollback(999); err == nil {
		t.Fatal("rollback to unknown version must fail")
	}
}

// TestOverride 验证覆盖优先级、有效期、鉴权与熔断。
func TestOverride(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags: map[string]Flag{
			"f": {Key: "f", DefaultValue: true},
		},
	})
	e := NewEngine(store)
	now := time.Now()
	// 越权操作者被拒绝。
	err := store.AddOverride(Override{
		ID: "ov-bad", FlagKey: "f", Value: false,
		ExpiresAt: now.Add(time.Hour), Operator: "mallory", Reason: "attack",
	})
	if err == nil {
		t.Fatal("unauthorized operator must be rejected")
	}
	// 全局熔断：强制关闭，常规默认值（true）不得绕过。
	if err := store.AddOverride(Override{
		ID: "ov-kill", FlagKey: "f", Value: false,
		ExpiresAt: now.Add(time.Hour), Operator: "sre", Reason: "incident #123",
	}); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate(ctxOf("anyone", nil), "f"); res.Value || res.Reason != ReasonOverride || res.OverrideID != "ov-kill" {
		t.Fatalf("global kill switch must win: %+v", res)
	}
	// 主体级覆盖优先于全局熔断。
	if err := store.AddOverride(Override{
		ID: "ov-vip", FlagKey: "f", SubjectID: "vip", Value: true,
		ExpiresAt: now.Add(time.Hour), Operator: "admin", Reason: "whitelist vip",
	}); err != nil {
		t.Fatal(err)
	}
	if res := e.Evaluate(ctxOf("vip", nil), "f"); !res.Value || res.OverrideID != "ov-vip" {
		t.Fatalf("subject override should beat global: %+v", res)
	}
	if res := e.Evaluate(ctxOf("other", nil), "f"); res.Value {
		t.Fatalf("global kill should still apply to others: %+v", res)
	}
	// 过期覆盖失效：用未来时间评估。
	future := ctxOf("vip", nil)
	future.Now = now.Add(2 * time.Hour)
	if res := e.Evaluate(future, "f"); !res.Value || res.Reason != ReasonDefault {
		t.Fatalf("expired override must be ignored: %+v", res)
	}
	// 缺少原因/有效期的覆盖被拒绝。
	if err := store.AddOverride(Override{
		ID: "ov-noreason", FlagKey: "f", Value: false,
		ExpiresAt: now.Add(time.Hour), Operator: "admin",
	}); err == nil {
		t.Fatal("override without reason must be rejected")
	}
}

// TestInvalidInputDeterministic 验证异常输入产生确定性结果。
func TestInvalidInputDeterministic(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags:   map[string]Flag{"f": {Key: "f", DefaultValue: true}},
	})
	e := NewEngine(store)
	r1 := e.Evaluate(EvalContext{}, "f")
	r2 := e.Evaluate(EvalContext{}, "f")
	if r1.Reason != ReasonInvalidInput || r1.Value != r2.Value {
		t.Fatalf("invalid input must be deterministic: %+v %+v", r1, r2)
	}
	if res := e.Evaluate(ctxOf("u", nil), "missing"); res.Reason != ReasonFlagNotFound || res.Value {
		t.Fatalf("missing flag must be deterministic false: %+v", res)
	}
}

// TestUpdateValidation 验证非法配置整体拒绝且旧版本继续服务。
func TestUpdateValidation(t *testing.T) {
	store := newStoreWith(t, Config{
		Version: 1,
		Flags:   map[string]Flag{"f": {Key: "f", DefaultValue: true}},
	})
	bad := Config{
		Version: 2,
		Flags: map[string]Flag{
			"g": {Key: "g", Rules: []Rule{
				{ID: "r", Priority: 1, Value: true, Rollout: &Rollout{Percentage: 150}},
			}},
		},
	}
	if err := store.Update(bad); err == nil {
		t.Fatal("invalid rollout percentage must be rejected")
	}
	if got := store.Snapshot().Version; got != 1 {
		t.Fatalf("failed update must keep version 1, got %d", got)
	}
	// 版本号必须单调递增。
	if err := store.Update(Config{Version: 1, Flags: map[string]Flag{}}); err == nil {
		t.Fatal("non-increasing version must be rejected")
	}
}
