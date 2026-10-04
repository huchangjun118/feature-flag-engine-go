package flagengine_test

import (
	"fmt"
	"time"

	flagengine "github.com/huchangjun118/feature-flag-engine-go"
)

func ExampleEngine_basicUsage() {
	cfg := &flagengine.Config{
		Version: 1,
		Comment: "first release",
		Flags: map[string]*flagengine.Flag{
			"new_checkout": {
				Key:       "new_checkout",
				DefaultOn: false,
				Rules: []flagengine.Rule{
					{
						ID:       "cn_ios_10pct",
						Priority: 1,
						Conditions: []flagengine.Condition{
							{Attribute: "region", Operator: flagengine.OpEq, Values: []string{"cn"}},
							{Attribute: "device", Operator: flagengine.OpEq, Values: []string{"ios"}},
							{Attribute: "version", Operator: flagengine.OpVersionBetween,
								Values: []string{"2.0.0", "3.0.0"}},
							{Attribute: "tag", Operator: flagengine.OpTagAny, Values: []string{"beta"}},
						},
						Percentage: 10,
						On:         true,
					},
				},
				Dependencies: []flagengine.Dependency{
					{FlagKey: "checkout_available", RequireOn: true},
				},
			},
			"checkout_available": {Key: "checkout_available", DefaultOn: true},
		},
	}

	eng, err := flagengine.New(flagengine.Options{
		Authorizer: flagengine.NewRoleAuthorizer("sre-oncall"),
		Now:        func() time.Time { return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC) },
	}, cfg)
	if err != nil {
		panic(err)
	}

	subject := flagengine.Subject{
		ID: "user-1001", Region: "cn", Device: "ios", Version: "2.4.1",
		Tags: []string{"beta"},
	}
	res, version, err := eng.EvaluateOnConfig("new_checkout", subject)
	if err != nil {
		panic(err)
	}
	fmt.Printf("v%d on=%v reason=%s rule=%s bucket=%d/%d\n",
		version, res.On, res.Trace.Reason, res.Trace.MatchedRule,
		res.Trace.Bucket, flagengine.BucketCount)

	// 紧急熔断：只有授权操作者可以执行
	err = eng.SetOverride(flagengine.Override{
		FlagKey: "new_checkout",
		// SubjectID 留空或填 "*" 表示全局熔断
		On: false, Operator: "sre-oncall", Reason: "INCIDENT-2025-0601",
	})
	if err != nil {
		panic(err)
	}
	res2, _ := eng.Evaluate("new_checkout", subject)
	fmt.Printf("after kill-switch: on=%v reason=%s\n", res2.On, res2.Trace.Reason)
	// 循环依赖的配置会被整体拒绝，旧版本继续服务
	// 覆盖操作已自动生成 v2，因此新配置必须使用更大的版本号
	cyclic := &flagengine.Config{Version: 3, Flags: map[string]*flagengine.Flag{
		"a": {Key: "a", Dependencies: []flagengine.Dependency{{FlagKey: "b", RequireOn: true}}},
		"b": {Key: "b", Dependencies: []flagengine.Dependency{{FlagKey: "a", RequireOn: true}}},
	}}
	if err := eng.Commit(cyclic); err != nil {
		fmt.Println("cyclic config rejected, serving version", eng.CurrentVersion())
	}
}

// Output:
// v1 on=false reason=rule_percentage_miss rule=cn_ios_10pct bucket=69/100
// after kill-switch: on=false reason=global_override
// cyclic config rejected, serving version 2
