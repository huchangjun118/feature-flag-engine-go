package flagengine

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cfgVersion1() *Config {
	return &Config{
		Version: 1,
		Flags: map[string]*Flag{
			"f": {Key: "f", DefaultOn: false,
				Rules: []Rule{{ID: "r1", Percentage: 0, On: true}}},
		},
	}
}

func cfgVersionN(n int64, pct int, salt string) *Config {
	return &Config{
		Version: n,
		Flags: map[string]*Flag{
			"f": {Key: "f", Salt: salt, DefaultOn: false,
				Rules: []Rule{{ID: "r1", Percentage: pct, On: true}}},
		},
	}
}

func TestCommitAtomicAndKeepsOldOnFailure(t *testing.T) {
	eng, err := New(Options{}, cfgVersion1())
	if err != nil {
		t.Fatal(err)
	}
	// 非法配置：百分比越界 -> 提交失败，版本保持 1
	bad := cfgVersionN(2, 500, "")
	if err := eng.Commit(bad); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig, got %v", err)
	}
	if v := eng.CurrentVersion(); v != 1 {
		t.Fatalf("current version changed after failed commit: %d", v)
	}
	// 循环依赖的新版本同样被整体拒绝
	cyclic := &Config{Version: 2, Flags: map[string]*Flag{
		"f": {Key: "f", Dependencies: []Dependency{{FlagKey: "f", RequireOn: true}}},
	}}
	if err := eng.Commit(cyclic); err == nil {
		t.Fatal("cyclic config must be rejected")
	}
	if v := eng.CurrentVersion(); v != 1 {
		t.Fatalf("current version changed after rejected cyclic commit: %d", v)
	}
	// 版本号必须递增
	if err := eng.Commit(cfgVersionN(1, 50, "")); err == nil {
		t.Fatal("stale version must be rejected")
	}
	// 合法新版本生效
	if err := eng.Commit(cfgVersionN(2, 100, "")); err != nil {
		t.Fatal(err)
	}
	if v := eng.CurrentVersion(); v != 2 {
		t.Fatalf("want version 2, got %d", v)
	}
}

func TestExternalConfigMutationIsolated(t *testing.T) {
	cfg := cfgVersion1()
	eng, _ := New(Options{}, cfg)
	// 提交后外部修改原对象不影响引擎
	cfg.Flags["f"].Rules[0].Percentage = 100
	cfg.Flags["f"].Rules[0].ID = "mutated"
	res, err := eng.Evaluate("f", Subject{ID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// v1 中 percentage=0，任何用户都不开启
	if res.On || res.Trace.MatchedRule != "r1" || res.Trace.Reason != ReasonRuleThrottled {
		t.Fatalf("snapshot leaked external mutation: %+v", res.Trace)
	}
}

func TestRollbackReproducesHistoricalResult(t *testing.T) {
	// v1: 100% on; v2: 0% off（salt 相同 => 桶不变）
	eng, _ := New(Options{}, cfgVersionN(1, 100, "s"))
	s := Subject{ID: "user-123"}
	r1, err := eng.Evaluate("f", s)
	if err != nil || !r1.On {
		t.Fatal("v1 should be on")
	}
	if err := eng.Commit(cfgVersionN(2, 0, "s")); err != nil {
		t.Fatal(err)
	}
	r2, _ := eng.Evaluate("f", s)
	if r2.On {
		t.Fatal("v2 should be off")
	}
	if err := eng.Rollback(1); err != nil {
		t.Fatal(err)
	}
	r3, v, err := eng.EvaluateOnConfig("f", s)
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 || !r3.On || r3.Trace.Bucket != r1.Trace.Bucket {
		t.Fatalf("rollback result differs from historical: r1=%+v r3=%+v v=%d",
			r1.Trace, r3.Trace, v)
	}
	if err := eng.Rollback(999); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("rollback missing version should fail, got %v", err)
	}
}

func TestRestartReproducesResult(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfg := cfgVersionN(1, 37, "stable-salt")
	if err := SaveConfigFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	resultsBefore := map[string]bool{}
	eng1, err := NewFromFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("subj-%d", i)
		r, err := eng1.Evaluate("f", Subject{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		resultsBefore[id] = r.On
	}
	// "重启"：从同一文件重建引擎
	eng2, err := NewFromFile(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range resultsBefore {
		r, err := eng2.Evaluate("f", Subject{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if r.On != want {
			t.Fatalf("after restart result changed for %s: %v -> %v (bucket trace v1=%d)",
				id, want, r.On, r.Trace.Bucket)
		}
	}
}

func TestConcurrentEvalAndCommit(t *testing.T) {
	eng, _ := New(Options{}, cfgVersionN(1, 50, "s"))

	stop := atomic.Bool{}
	var wg sync.WaitGroup
	var evalErrCount atomic.Int64

	// 并发评估：任何时刻都只能看到某个完整版本（1 或 2），且结果稳定。
	results := sync.Map{} // subjectID -> (version, on)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				id := fmt.Sprintf("g%d-u%d", g, i%500)
				r, v, err := eng.EvaluateOnConfig("f", Subject{ID: id})
				if err != nil {
					evalErrCount.Add(1)
					continue
				}
				if v != 1 && v != 2 {
					t.Errorf("saw torn version %d", v)
					return
				}
				type res struct {
					v  int64
					on bool
				}
				key := id + "|" + fmt.Sprint(v)
				if prev, ok := results.LoadOrStore(key, res{v, r.On}); ok {
					if prev.(res).on != r.On {
						t.Errorf("non-deterministic eval for %s", key)
						return
					}
				}
				// 同一主体在同一版本下结果必须始终一致；
				// 两次评估各自携带版本，仅在版本相同时比较（版本可能恰在期间切换）。
				if r2, v2, err := eng.EvaluateOnConfig("f", Subject{ID: id}); err == nil &&
					v2 == v && r2.On != r.On {
					t.Errorf("subject %s flipped within version %d", id, v)
					return
				}
			}
		}(g)
	}

	// 提交新版本（v2 全量），让评估与更新并发进行
	time.Sleep(20 * time.Millisecond)
	if err := eng.Commit(cfgVersionN(2, 100, "s")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if evalErrCount.Load() != 0 {
		t.Fatalf("evaluations errored during concurrent commit: %d", evalErrCount.Load())
	}
	if v := eng.CurrentVersion(); v != 2 {
		t.Fatalf("final version=%d", v)
	}
	// 更新后所有主体都应开启
	for i := 0; i < 100; i++ {
		r, err := eng.Evaluate("f", Subject{ID: fmt.Sprintf("final-%d", i)})
		if err != nil || !r.On {
			t.Fatalf("post-update eval on=%v err=%v", r.On, err)
		}
	}
}

func TestEvalBoundedByBudget(t *testing.T) {
	// 构建一条长依赖链，用极小预算强制触发预算错误
	flags := map[string]*Flag{}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("f%d", i)
		f := &Flag{Key: key, DefaultOn: true}
		if i > 0 {
			f.Dependencies = []Dependency{{FlagKey: fmt.Sprintf("f%d", i-1), RequireOn: true}}
		}
		flags[key] = f
	}
	cfg := &Config{Version: 1, Flags: flags}
	eng, err := New(Options{MaxEvalOps: 2}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Evaluate("f9", Subject{ID: "u"}); !errors.Is(err, ErrEvalBudget) {
		t.Fatalf("expected ErrEvalBudget, got %v", err)
	}
}

func TestCommitRejectsUnknownOperatorAndBadRefs(t *testing.T) {
	eng, _ := New(Options{}, cfgVersion1())
	bad := &Config{Version: 2, Flags: map[string]*Flag{
		"f": {Key: "f", Rules: []Rule{{ID: "r",
			Conditions: []Condition{{Attribute: "region", Operator: "bogus"}}}}},
	}}
	if err := eng.Commit(bad); err == nil {
		t.Fatal("unknown operator must be rejected")
	}
	missingDep := &Config{Version: 2, Flags: map[string]*Flag{
		"f": {Key: "f", Dependencies: []Dependency{{FlagKey: "ghost", RequireOn: true}}},
	}}
	if err := eng.Commit(missingDep); err == nil {
		t.Fatal("dependency on unknown flag must be rejected")
	}
	dupRule := &Config{Version: 2, Flags: map[string]*Flag{
		"f": {Key: "f", Rules: []Rule{
			{ID: "same"}, {ID: "same"},
		}},
	}}
	if err := eng.Commit(dupRule); err == nil {
		t.Fatal("duplicate rule ids must be rejected")
	}
	if v := eng.CurrentVersion(); v != 1 {
		t.Fatalf("version must remain 1 after rejections, got %d", v)
	}
}
