// ffdemo 是特性开关引擎的最小命令行演示：
// 生成示例配置 -> 启动引擎 -> 评估若干主体 -> 全局熔断 -> 输出审计。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	flagengine "github.com/huchangjun118/feature-flag-engine-go"
)

func main() {
	dir, err := os.MkdirTemp("", "ffdemo-")
	must(err)
	defer os.RemoveAll(dir)

	cfgPath := filepath.Join(dir, "config.json")
	auditPath := filepath.Join(dir, "audit.jsonl")

	cfg := &flagengine.Config{
		Version: 1,
		Comment: "demo config",
		Flags: map[string]*flagengine.Flag{
			"new_checkout": {
				Key:       "new_checkout",
				DefaultOn: false,
				Rules: []flagengine.Rule{{
					ID:       "cn_30pct",
					Priority: 1,
					Conditions: []flagengine.Condition{
						{Attribute: "region", Operator: flagengine.OpEq, Values: []string{"cn"}},
					},
					Percentage: 30,
					On:         true,
				}},
			},
		},
	}
	must(flagengine.SaveConfigFile(cfgPath, cfg))

	sink, err := flagengine.NewFileAuditSink(auditPath)
	must(err)
	defer sink.Close()

	eng, err := flagengine.NewFromFile(cfgPath, flagengine.Options{
		Authorizer: flagengine.NewRoleAuthorizer("sre-oncall"),
		AuditSink:  sink,
	})
	must(err)

	subjects := []flagengine.Subject{
		{ID: "user-1", Region: "cn"},
		{ID: "user-2", Region: "cn"},
		{ID: "user-3", Region: "us"},
	}
	for _, s := range subjects {
		r, v, err := eng.EvaluateOnConfig("new_checkout", s)
		must(err)
		fmt.Printf("[v%d] %-6s on=%-5v reason=%-23s bucket=%2d\n",
			v, s.ID, r.On, r.Trace.Reason, r.Trace.Bucket)
	}

	must(eng.SetOverride(flagengine.Override{
		FlagKey:  "new_checkout",
		On:       false, // 全局紧急熔断
		Operator: "sre-oncall",
		Reason:   "INCIDENT-DEMO",
	}))
	r, err := eng.Evaluate("new_checkout", subjects[0])
	must(err)
	fmt.Printf("after kill-switch: user-1 on=%v reason=%s version=%d\n",
		r.On, r.Trace.Reason, eng.CurrentVersion())

	data, err := os.ReadFile(auditPath)
	must(err)
	fmt.Printf("audit events written: %d bytes -> %s\n", len(data), auditPath)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
