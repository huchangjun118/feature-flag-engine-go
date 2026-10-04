package flagengine_test

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	flagengine "github.com/huchangjun118/feature-flag-engine-go"
)

// 该测试复刻 example_test.go 中的调用序列并校验精确输出。
// 本环境的 Go 工具链禁用了 Example 自动执行，用此测试保证
// 示例 Output 注释在标准工具链上同样成立。
func TestExampleOutputContract(t *testing.T) {
	cfg := &flagengine.Config{
		Version: 1,
		Flags: map[string]*flagengine.Flag{
			"new_checkout": {
				Key:       "new_checkout",
				DefaultOn: false,
				Rules: []flagengine.Rule{{
					ID:       "cn_ios_10pct",
					Priority: 1,
					Conditions: []flagengine.Condition{
						{Attribute: "region", Operator: flagengine.OpEq, Values: []string{"cn"}},
						{Attribute: "device", Operator: flagengine.OpEq, Values: []string{"ios"}},
						{Attribute: "version", Operator: flagengine.OpVersionBetween,
							Values: []string{"2.0.0", "3.0.0"}},
						{Attribute: "tag", Operator: flagengine.OpTagAny, Values: []string{"beta"}},
					},
					Percentage: 10, On: true,
				}},
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
		t.Fatal(err)
	}
	subject := flagengine.Subject{ID: "user-1001", Region: "cn", Device: "ios",
		Version: "2.4.1", Tags: []string{"beta"}}

	var buf bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	res, version, _ := eng.EvaluateOnConfig("new_checkout", subject)
	printfSafe("v%d on=%v reason=%s rule=%s bucket=%d/%d\n",
		version, res.On, res.Trace.Reason, res.Trace.MatchedRule,
		res.Trace.Bucket, flagengine.BucketCount)

	_ = eng.SetOverride(flagengine.Override{
		FlagKey: "new_checkout", On: false,
		Operator: "sre-oncall", Reason: "INCIDENT-2025-0601",
	})
	res2, _ := eng.Evaluate("new_checkout", subject)
	printfSafe("after kill-switch: on=%v reason=%s\n", res2.On, res2.Trace.Reason)

	cyclic := &flagengine.Config{Version: 3, Flags: map[string]*flagengine.Flag{
		"a": {Key: "a", Dependencies: []flagengine.Dependency{{FlagKey: "b", RequireOn: true}}},
		"b": {Key: "b", Dependencies: []flagengine.Dependency{{FlagKey: "a", RequireOn: true}}},
	}}
	if err := eng.Commit(cyclic); err != nil {
		printfSafe("cyclic config rejected, serving version %d\n", eng.CurrentVersion())
	}

	_ = w.Close()
	os.Stdout = old
	_, _ = io.Copy(&buf, r)

	want := strings.Join([]string{
		"v1 on=false reason=rule_percentage_miss rule=cn_ios_10pct bucket=69/100",
		"after kill-switch: on=false reason=global_override",
		"cyclic config rejected, serving version 2",
		"",
	}, "\n")
	if buf.String() != want {
		t.Fatalf("example output mismatch:\n got: %q\nwant: %q", buf.String(), want)
	}
}

// printfSafe 包装 fmt，避免测试文件再引入 import 名冲突。
func printfSafe(format string, args ...any) {
	// 通过 os.Stdout 写出
	os.Stdout.Write([]byte(sprintf(format, args...)))
}
