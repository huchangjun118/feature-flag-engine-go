package flagengine

import (
	"errors"
	"testing"
	"time"
)

func overrideCfg() *Config {
	return &Config{
		Version: 1,
		Flags: map[string]*Flag{
			"f": {Key: "f", DefaultOn: false,
				Rules: []Rule{{ID: "r", Percentage: 100, On: true}}},
		},
	}
}

func TestDenyAllByDefault(t *testing.T) {
	eng, _ := New(Options{}, overrideCfg())
	err := eng.SetOverride(Override{
		FlagKey: "f", SubjectID: "u1", On: false,
		Operator: "ops", Reason: "incident",
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestSubjectAndGlobalOverride(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	eng, err := New(Options{
		Authorizer: NewRoleAuthorizer("ops-admin"),
		Now:        func() time.Time { return now },
	}, overrideCfg())
	if err != nil {
		t.Fatal(err)
	}
	// 常规规则下所有用户开启
	r, _ := eng.Evaluate("f", Subject{ID: "u1"})
	if !r.On {
		t.Fatal("baseline should be on")
	}

	// 1) 主体级强制关闭：最高优先，常规规则无法绕过
	if err := eng.SetOverride(Override{
		FlagKey: "f", SubjectID: "u1", On: false,
		Operator: "ops-admin", Reason: "risk control",
	}); err != nil {
		t.Fatal(err)
	}
	r, _ = eng.Evaluate("f", Subject{ID: "u1"})
	if r.On || r.Trace.Reason != ReasonSubjectOverride {
		t.Fatalf("subject override off failed: %+v", r.Trace)
	}
	// 其他主体不受影响
	r2, _ := eng.Evaluate("f", Subject{ID: "u2"})
	if !r2.On {
		t.Fatal("other subjects must remain on")
	}

	// 2) 全局紧急熔断：所有主体关闭，即使主体级强制开启也不能越过全局熔断
	if err := eng.SetOverride(Override{
		FlagKey: "f", SubjectID: GlobalOverrideSubject, On: false,
		Operator: "ops-admin", Reason: "global incident",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u2", "u3", "u4"} {
		rr, _ := eng.Evaluate("f", Subject{ID: id})
		if rr.On || rr.Trace.Reason != ReasonGlobalOverride {
			t.Fatalf("global kill-switch failed for %s: %+v", id, rr.Trace)
		}
	}

	// 3) 越权操作者仍被拒绝
	err = eng.SetOverride(Override{
		FlagKey: "f", SubjectID: "u9", On: true,
		Operator: "intruder", Reason: "x",
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("intruder must be rejected, got %v", err)
	}

	// 4) 缺原因的操作被拒绝
	err = eng.SetOverride(Override{
		FlagKey: "f", SubjectID: "u9", On: true,
		Operator: "ops-admin",
	})
	if !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("missing reason must be rejected, got %v", err)
	}

	// 5) 移除全局熔断后恢复常规规则（主体级覆盖仍在）
	if err := eng.RemoveOverride("f", GlobalOverrideSubject, "ops-admin"); err != nil {
		t.Fatal(err)
	}
	rr, _ := eng.Evaluate("f", Subject{ID: "u2"})
	if !rr.On {
		t.Fatal("after removing global override, normal rules should resume")
	}
	rr, _ = eng.Evaluate("f", Subject{ID: "u1"})
	if rr.On {
		t.Fatal("subject-level override must still apply")
	}
	// 覆盖产生新版本，历史可回滚
	vs := eng.Versions()
	if len(vs) != 4 { // v1 + subject override + global override + removal
		t.Fatalf("versions=%v", vs)
	}
}

func TestOverrideExpiry(t *testing.T) {
	current := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }
	eng, _ := New(Options{
		Authorizer: NewRoleAuthorizer("admin"),
		Now:        clock,
	}, overrideCfg())

	// 有效期 1 小时的强制关闭
	if err := eng.SetOverride(Override{
		FlagKey: "f", SubjectID: "u1", On: false,
		Operator: "admin", Reason: "temp",
		ExpiresAt: current.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := eng.Evaluate("f", Subject{ID: "u1"})
	if r.On {
		t.Fatal("override should be active before expiry")
	}
	// 时间推进到过期后：覆盖失效，回到常规规则（开启）
	current = current.Add(2 * time.Hour)
	r, _ = eng.Evaluate("f", Subject{ID: "u1"})
	if !r.On || r.Trace.Reason != ReasonRule {
		t.Fatalf("expired override should be ignored: %+v", r.Trace)
	}
}

func TestSubjectOverrideBeatsGlobalOn(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	eng, _ := New(Options{
		Authorizer: NewRoleAuthorizer("admin"),
		Now:        func() time.Time { return now },
	}, &Config{Version: 1, Flags: map[string]*Flag{
		"f": {Key: "f", DefaultOn: false}, // 常规下全部关闭
	}})
	// 全局强制开启
	if err := eng.SetOverride(Override{FlagKey: "f",
		SubjectID: GlobalOverrideSubject, On: true,
		Operator: "admin", Reason: "rollout"}); err != nil {
		t.Fatal(err)
	}
	// 特定主体仍可被强制关闭（安全兜底优先）
	if err := eng.SetOverride(Override{FlagKey: "f",
		SubjectID: "vip-1", On: false,
		Operator: "admin", Reason: "vip opt-out"}); err != nil {
		t.Fatal(err)
	}
	r, _ := eng.Evaluate("f", Subject{ID: "normal"})
	if !r.On || r.Trace.Reason != ReasonGlobalOverride {
		t.Fatalf("global on should apply: %+v", r.Trace)
	}
	r, _ = eng.Evaluate("f", Subject{ID: "vip-1"})
	if r.On || r.Trace.Reason != ReasonSubjectOverride {
		t.Fatalf("subject off must beat global on: %+v", r.Trace)
	}
}

func TestAuditFileRecordsOverrides(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl"
	sink, err := NewFileAuditSink(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	eng, _ := New(Options{
		Authorizer: NewRoleAuthorizer("admin"),
		AuditSink:  sink,
	}, overrideCfg())
	_ = eng.SetOverride(Override{FlagKey: "f", SubjectID: "u1",
		On: false, Operator: "admin", Reason: "incident-42"})
	_, _ = eng.Evaluate("f", Subject{ID: "u1"})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data := readFileForTest(t, path)
	if !contains(string(data), "incident-42") || !contains(string(data), `"type":"override"`) {
		t.Fatalf("audit file missing override event:\n%s", data)
	}
	if !contains(string(data), `"type":"eval"`) {
		t.Fatalf("audit file missing eval event:\n%s", data)
	}
}
