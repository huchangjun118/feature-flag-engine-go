package flagengine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func baseConfig() *Config {
	return &Config{
		Version:   1,
		Comment:   "base",
		CreatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Flags: map[string]*Flag{
			"checkout": {
				Key:       "checkout",
				DefaultOn: false,
				Rules: []Rule{
					{
						ID:       "cn_ios",
						Priority: 1,
						Conditions: []Condition{
							{AttrRegion, OpEq, []string{"cn"}},
							{AttrDevice, OpEq, []string{"ios"}},
						},
						Percentage: 100,
						On:         true,
						Variation:  "new",
					},
					{
						ID:         "cn_10pct",
						Priority:   2,
						Conditions: []Condition{{AttrRegion, OpEq, []string{"cn"}}},
						Percentage: 10,
						On:         true,
					},
				},
			},
		},
	}
}

func TestDefaultFallback(t *testing.T) {
	eng, err := New(Options{}, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	// us 用户不命中任何规则 -> 默认值 false
	res, err := eng.Evaluate("checkout", Subject{ID: "u1", Region: "us", Device: "web"})
	if err != nil || res.On {
		t.Fatalf("got on=%v err=%v, want false/nil", res.On, err)
	}
	if res.Trace.Reason != ReasonDefault {
		t.Fatalf("reason=%s want default", res.Trace.Reason)
	}
	if _, err := eng.Evaluate("missing", Subject{ID: "u1"}); !errors.Is(err, ErrFlagNotFound) {
		t.Fatalf("expected ErrFlagNotFound, got %v", err)
	}
	if _, err := eng.Evaluate("checkout", Subject{}); !errors.Is(err, ErrEmptySubject) {
		t.Fatalf("expected ErrEmptySubject, got %v", err)
	}
}

func TestRuleMatchAndTrace(t *testing.T) {
	eng, _ := New(Options{}, baseConfig())
	res, ver, err := eng.EvaluateOnConfig("checkout", Subject{ID: "u1", Region: "cn", Device: "ios"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.On || res.Trace.Reason != ReasonRule || res.Trace.MatchedRule != "cn_ios" ||
		res.Trace.Variation != "new" || ver != 1 {
		t.Fatalf("unexpected result: %+v version=%d", res.Trace, ver)
	}
	// 可追溯：查询最近记录
	tr, ok := eng.QueryTrace("checkout", "u1")
	if !ok || tr.MatchedRule != "cn_ios" || tr.Version != 1 {
		t.Fatalf("query trace failed: %+v ok=%v", tr, ok)
	}
}

func TestPercentageRolloutStableAndExplainable(t *testing.T) {
	eng, _ := New(Options{}, baseConfig())
	flag := baseConfig().Flags["checkout"]
	// 找出桶值 < 10 的一个 cn 用户与桶值 >= 10 的一个 cn 用户
	var inUser, outUser string
	for i := 0; ; i++ {
		id := "user-" + strings.Repeat("x", 0) + itoa(i)
		b := computeBucket(flag, id)
		if inUser == "" && inBucket(b, 10) {
			inUser = id
		}
		if outUser == "" && !inBucket(b, 10) {
			outUser = id
		}
		if inUser != "" && outUser != "" {
			break
		}
		if i > 1_000_000 {
			t.Fatal("could not find sample users")
		}
	}
	r1, err := eng.Evaluate("checkout", Subject{ID: inUser, Region: "cn", Device: "android"})
	if err != nil || !r1.On || r1.Trace.Reason != ReasonRule {
		t.Fatalf("in-bucket user: %+v err=%v", r1.Trace, err)
	}
	r2, err := eng.Evaluate("checkout", Subject{ID: outUser, Region: "cn", Device: "android"})
	if err != nil || r2.On || r2.Trace.Reason != ReasonRuleThrottled {
		t.Fatalf("out-bucket user: on=%v reason=%s err=%v", r2.On, r2.Trace.Reason, err)
	}
	if r2.Trace.Bucket < 10 || r2.Trace.Percentage != 10 {
		t.Fatalf("trace should carry bucket/percentage: %+v", r2.Trace)
	}
}

func TestDependencies(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Flags: map[string]*Flag{
			"base": {Key: "base", DefaultOn: false, Rules: []Rule{{
				ID: "on", Percentage: 100, On: true,
			}}},
			"child": {Key: "child", DefaultOn: false,
				Dependencies: []Dependency{{FlagKey: "base", RequireOn: true}},
				Rules:        []Rule{{ID: "r", Percentage: 100, On: true}}},
		},
	}
	eng, err := New(Options{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Evaluate("child", Subject{ID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.On {
		t.Fatal("child should be on when prerequisite base is on")
	}

	// 前置不满足：把 base 规则置为 On=false
	cfg2 := &Config{
		Version: 2,
		Flags: map[string]*Flag{
			"base": {Key: "base", DefaultOn: false, Rules: []Rule{{
				ID: "on", Percentage: 100, On: false,
			}}},
			"child": {Key: "child", DefaultOn: true,
				Dependencies: []Dependency{{FlagKey: "base", RequireOn: true}},
				Rules:        []Rule{{ID: "r", Percentage: 100, On: true}}},
		},
	}
	eng2, _ := New(Options{}, cfg2)
	res2, err := eng2.Evaluate("child", Subject{ID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.On || res2.Trace.Reason != ReasonPrerequisiteFailed {
		t.Fatalf("child must be off when prereq fails: %+v", res2.Trace)
	}
}

func TestCycleRejected(t *testing.T) {
	cfg := &Config{Version: 1, Flags: map[string]*Flag{
		"a": {Key: "a", Dependencies: []Dependency{{FlagKey: "b", RequireOn: true}}},
		"b": {Key: "b", Dependencies: []Dependency{{FlagKey: "c", RequireOn: true}}},
		"c": {Key: "c", Dependencies: []Dependency{{FlagKey: "a", RequireOn: true}}},
	}}
	_, err := New(Options{}, cfg)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CycleError, got %v", err)
	}
	if len(ce.Cycle) < 3 || ce.Cycle[0] != ce.Cycle[len(ce.Cycle)-1] {
		t.Fatalf("cycle chain malformed: %v", ce.Cycle)
	}
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatal("CycleError must satisfy errors.Is ErrCycleDetected")
	}
}

func TestSelfDependencyRejected(t *testing.T) {
	cfg := &Config{Version: 1, Flags: map[string]*Flag{
		"a": {Key: "a", Dependencies: []Dependency{{FlagKey: "a", RequireOn: true}}},
	}}
	if _, err := New(Options{}, cfg); !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("self dependency must be rejected, got %v", err)
	}
}

func TestDepthLimit(t *testing.T) {
	// a->b->c 链，MaxDepth=1 时评估 a 超限
	cfg := &Config{Version: 1, Flags: map[string]*Flag{
		"a": {Key: "a", Dependencies: []Dependency{{FlagKey: "b", RequireOn: true}}},
		"b": {Key: "b", Dependencies: []Dependency{{FlagKey: "c", RequireOn: true}}},
		"c": {Key: "c", DefaultOn: true},
	}}
	eng, err := New(Options{MaxDepth: 1}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Evaluate("a", Subject{ID: "u"}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("expected ErrDepthLimit, got %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
