package flagengine

import "testing"

func TestMatchConditions(t *testing.T) {
	s := Subject{
		ID:      "u1",
		Region:  "cn",
		Device:  "ios",
		Version: "2.3.10",
		Tags:    []string{"beta", "vip"},
		Attrs:   map[string]string{"plan": "pro", "city": "hangzhou"},
	}
	cases := []struct {
		name string
		c    Condition
		want bool
	}{
		{"region eq", Condition{AttrRegion, OpEq, []string{"cn"}}, true},
		{"region ne mismatch", Condition{AttrRegion, OpNe, []string{"cn"}}, false},
		{"device in", Condition{AttrDevice, OpIn, []string{"android", "ios"}}, true},
		{"device not_in", Condition{AttrDevice, OpNotIn, []string{"web"}}, true},
		{"version gte", Condition{AttrVersion, OpVersionGTE, []string{"2.3.10"}}, true},
		{"version lt boundary", Condition{AttrVersion, OpVersionLTE, []string{"2.3.9"}}, false},
		{"version between", Condition{AttrVersion, OpVersionBetween, []string{"2.0.0", "3.0.0"}}, true},
		{"version between out", Condition{AttrVersion, OpVersionBetween, []string{"1.0.0", "2.0.0"}}, false},
		{"version short form", Condition{AttrVersion, OpVersionGTE, []string{"2"}}, true},
		{"tag any", Condition{AttrTag, OpTagAny, []string{"alpha", "beta"}}, true},
		{"tag all", Condition{AttrTag, OpTagAll, []string{"beta", "vip"}}, true},
		{"tag all miss", Condition{AttrTag, OpTagAll, []string{"beta", "staff"}}, false},
		{"custom eq", Condition{"plan", OpEq, []string{"pro"}}, true},
		{"custom prefix", Condition{"city", OpPrefix, []string{"hang"}}, true},
		{"custom contains", Condition{"city", OpContains, []string{"gzh"}}, true},
		{"custom suffix", Condition{"city", OpSuffix, []string{"zhou"}}, true},
		{"missing attr exists", Condition{"nope", OpExists, nil}, false},
		{"region exists", Condition{AttrRegion, OpExists, nil}, true},
		{"unknown operator safe", Condition{AttrRegion, "weird", nil}, false},
		{"bad version operand safe", Condition{AttrVersion, OpVersionGTE, []string{"xx"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchCondition(tc.c, s); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRulePriorityOrder(t *testing.T) {
	rules := []Rule{
		{ID: "low", Priority: 100, Percentage: 0, On: false},
		{ID: "high-b", Priority: 1, Percentage: 0, On: true},
		{ID: "high-a", Priority: 1, Percentage: 0, On: true},
	}
	ordered := sortRules(rules)
	if ordered[0].ID != "high-a" || ordered[1].ID != "high-b" || ordered[2].ID != "low" {
		t.Fatalf("unexpected order: %s %s %s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
	// 原切片不被修改
	if rules[0].ID != "low" {
		t.Fatal("sortRules must not mutate input")
	}
}

func TestFirstMatchAndSemantics(t *testing.T) {
	s := Subject{ID: "u", Region: "cn"}
	rules := []Rule{
		{ID: "r1", Priority: 1, Conditions: []Condition{
			{AttrRegion, OpEq, []string{"cn"}},
			{AttrDevice, OpEq, []string{"web"}}, // 不满足
		}},
		{ID: "r2", Priority: 2, Conditions: []Condition{
			{AttrRegion, OpEq, []string{"cn"}},
		}},
	}
	idx, r := firstMatch(sortRules(rules), s)
	if idx != 1 || r == nil || r.ID != "r2" {
		t.Fatalf("expected r2 at idx 1, got idx=%d rule=%v", idx, r)
	}

	// 无条件规则兜底命中
	idx, r = firstMatch([]Rule{{ID: "catchall"}}, s)
	if idx != 0 || r.ID != "catchall" {
		t.Fatal("empty-condition rule should match everyone")
	}
}

func TestParseSemVer(t *testing.T) {
	good := map[string]SemVersion{
		"1.2.3":  {1, 2, 3},
		"1":      {1, 0, 0},
		"2.5":    {2, 5, 0},
		"v3.1.4": {3, 1, 4},
	}
	for in, want := range good {
		v, ok := ParseSemVer(in)
		if !ok || v != want {
			t.Fatalf("ParseSemVer(%q) = %+v,%v want %+v", in, v, ok, want)
		}
	}
	bad := []string{"", "x", "1.2.x", "1.2.3.4", "-1.0.0"}
	for _, in := range bad {
		if _, ok := ParseSemVer(in); ok {
			t.Fatalf("ParseSemVer(%q) unexpectedly succeeded", in)
		}
	}
}
