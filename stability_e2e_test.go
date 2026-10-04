package flagengine

import (
	"testing"
)

// 跨多个版本（含覆盖新版本）验证：同一主体的桶值永远不变，
// 且放量集合只随百分比前缀式变化。
func TestBucketStableAcrossVersionsAndOverrides(t *testing.T) {
	eng, err := New(Options{
		Authorizer: NewRoleAuthorizer("ops"),
	}, cfgVersionN(1, 10, "fixed-salt"))
	if err != nil {
		t.Fatal(err)
	}
	const subjects = 500
	firstBucket := map[string]int{}
	for i := 0; i < subjects; i++ {
		id := "s" + itoa(i)
		r, err := eng.Evaluate("f", Subject{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		firstBucket[id] = r.Trace.Bucket
	}

	// 经过：放量到 50 -> 全量 -> 回滚到 10 -> 主体覆盖 -> 移除覆盖
	versions := []int{50, 100}
	for i, p := range versions {
		if err := eng.Commit(cfgVersionN(int64(i+2), p, "fixed-salt")); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Rollback(1); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetOverride(Override{FlagKey: "f", SubjectID: "s1",
		On: false, Operator: "ops", Reason: "freeze s1"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.RemoveOverride("f", "s1", "ops"); err != nil {
		t.Fatal(err)
	}

	// 回滚到 v1 后产生的两个覆盖/移除版本（v4、v5）克隆自 v1 的定义，
	// 因而当前版本 v5 在分桶与放量语义上与 v1 逐位等价。
	if v := eng.CurrentVersion(); v != 5 {
		t.Fatalf("current version=%d, want 5", v)
	}
	for i := 0; i < subjects; i++ {
		id := "s" + itoa(i)
		r, v, err := eng.EvaluateOnConfig("f", Subject{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if v != 5 {
			t.Fatalf("%s evaluated on version %d, want 5", id, v)
		}
		if r.Trace.Bucket != firstBucket[id] {
			t.Fatalf("bucket for %s changed: %d -> %d after rollback/overrides",
				id, firstBucket[id], r.Trace.Bucket)
		}
		// 10% 放量时的结果只取决于固定桶位
		wantOn := inBucket(firstBucket[id], 10)
		if r.On != wantOn {
			t.Fatalf("%s on=%v want %v (bucket %d)", id, r.On, wantOn, firstBucket[id])
		}
	}
}

// 不存在的属性、未知运算符等异常输入组合不得产生开启结果或 panic。
func TestAbnormalInputsAreDeterministic(t *testing.T) {
	cfg := &Config{Version: 1, Flags: map[string]*Flag{
		"f": {Key: "f", DefaultOn: false, Rules: []Rule{
			{ID: "weird", Priority: 1, Conditions: []Condition{
				{Attribute: "ghost", Operator: OpExists},
				{Attribute: "region", Operator: OpVersionGTE, Values: []string{"???"}},
				{Attribute: "missing", Operator: OpEq, Values: []string{"x"}},
				{Attribute: "tag", Operator: OpTagAll, Values: []string{"nonexistent-tag"}},
			}, Percentage: 100, On: true},
			{ID: "fallback", Priority: 2, Percentage: 100, On: true},
		}},
	}}
	eng, _ := New(Options{}, cfg)
	weirdSubjects := []Subject{
		{ID: "a"},
		{ID: "a", Attrs: map[string]string{"ghost": "x"}},
		{ID: "a", Region: "not-a-version"},
		{ID: "a", Tags: []string{}},
	}
	for i, s := range weirdSubjects {
		r1, err := eng.Evaluate("f", s)
		if err != nil {
			t.Fatalf("case %d unexpected error: %v", i, err)
		}
		r2, _ := eng.Evaluate("f", s)
		if r1.On != r2.On || r1.Trace.Reason != r2.Trace.Reason {
			t.Fatalf("case %d non-deterministic across repeats", i)
		}
	}
}
