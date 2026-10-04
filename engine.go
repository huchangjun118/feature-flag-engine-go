package flagengine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GlobalOverrideSubject 是全局覆盖（紧急熔断）使用的主体键。
const GlobalOverrideSubject = "*"

// 默认参数。
const (
	defaultMaxDepth   = 16
	defaultMaxEvalOps = 100_000
	defaultHistory    = 32
	defaultTraceSize  = 4096
)

// Engine 是特性开关引擎。所有方法可被多 goroutine 并发调用。
//
// 并发模型：
//   - current 以 atomic.Pointer 指向当前已提交快照；评估全程只读同一快照，
//     永远不会看到字段级混合的半更新状态，且不会被更新阻塞。
//   - 更新在 mu 写锁内完成 校验->建快照->原子切换->入历史，失败保留旧版本。
//   - 评估使用比较操作预算（MaxEvalOps）作为硬性上界，无 IO、无随机数，
//     单次评估耗时 O(MaxEvalOps) 次简单比较，可给出确定上界。
type Engine struct {
	opts Options

	current atomic.Pointer[snapshot]

	mu      sync.Mutex // 保护 history 与版本提交/覆盖操作的串行化
	history []*snapshot
	maxVer  int64 // 曾出现过的最大版本号，防止回滚后提交旧版本

	tracesMu sync.Mutex
	traces   []Trace // 环形缓冲
	tracePos int

	now func() time.Time
}

// New 创建引擎；initial 可为 nil，表示尚无已提交版本（此时评估返回明确错误）。
func New(opts Options, initial *Config) (*Engine, error) {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}
	if opts.MaxEvalOps <= 0 {
		opts.MaxEvalOps = defaultMaxEvalOps
	}
	if opts.HistorySize <= 0 {
		opts.HistorySize = defaultHistory
	}
	if opts.TraceSize <= 0 {
		opts.TraceSize = defaultTraceSize
	}
	if opts.Authorizer == nil {
		opts.Authorizer = DenyAllAuthorizer{}
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	e := &Engine{
		opts:   opts,
		traces: make([]Trace, 0, opts.TraceSize),
		now:    nowFn,
	}
	if initial != nil {
		snap, err := buildSnapshot(initial)
		if err != nil {
			return nil, err
		}
		e.current.Store(snap)
		e.history = append(e.history, snap)
		e.maxVer = snap.config.Version
	}
	return e, nil
}

// Evaluate 在当前已提交版本上评估开关。
func (e *Engine) Evaluate(flagKey string, subject Subject) (Result, error) {
	res, _, err := e.EvaluateOnConfig(flagKey, subject)
	return res, err
}

// EvaluateOnConfig 与 Evaluate 相同，但额外返回所基于的配置版本。
func (e *Engine) EvaluateOnConfig(flagKey string, subject Subject) (Result, int64, error) {
	if subject.ID == "" {
		return Result{}, 0, ErrEmptySubject
	}
	snap := e.current.Load()
	if snap == nil {
		return Result{}, 0, fmt.Errorf("%w: no committed config yet", ErrInvalidConfig)
	}
	budget := e.opts.MaxEvalOps
	chain := make([]string, 0, 4)
	res, err := e.eval(snap, flagKey, subject, 0, chain, &budget)
	if err != nil {
		return Result{}, snap.config.Version, err
	}
	res.Trace.Version = snap.config.Version
	res.Trace.EvaluatedAt = parseNow(e.opts.Now)
	e.recordTrace(res.Trace)
	e.audit(AuditEvent{
		Type:      "eval",
		Version:   snap.config.Version,
		FlagKey:   flagKey,
		SubjectID: subject.ID,
		Detail:    string(res.Trace.Reason),
	})
	return res, snap.config.Version, nil
}

// eval 在给定快照上执行评估（含前置依赖），depth 为当前依赖深度。
//
// 决策优先级（高到低）：
//  1. 依赖前置不满足 -> 关闭
//  2. 全局强制关闭（熔断）-> 关闭（任何常规规则无法绕过）
//  3. 主体级强制关闭 -> 关闭
//  4. 主体级强制开启 -> 开启
//  5. 全局强制开启 -> 开启
//  6. 命中定向规则 -> 按桶与百分比决定
//  7. 回落默认值
func (e *Engine) eval(snap *snapshot, flagKey string, s Subject, depth int, chain []string, budget *int) (Result, error) {
	*budget--
	if *budget <= 0 {
		return Result{}, ErrEvalBudget
	}
	if depth > e.opts.MaxDepth {
		return Result{}, fmt.Errorf("%w: %s depth %d", ErrDepthLimit, flagKey, depth)
	}
	// 同一评估链内重复出现同一开关，等价于环（提交时已拦截，这里是双保险）。
	for _, k := range chain {
		if k == flagKey {
			return Result{}, &CycleError{Cycle: append(append([]string(nil), chain...), flagKey)}
		}
	}

	flag, ok := snap.config.Flags[flagKey]
	if !ok {
		return Result{}, fmt.Errorf("%w: %q", ErrFlagNotFound, flagKey)
	}

	trace := Trace{FlagKey: flagKey, SubjectID: s.ID, Bucket: -1}

	// 1. 前置依赖
	for _, dep := range flag.Dependencies {
		depRes, err := e.eval(snap, dep.FlagKey, s, depth+1, append(chain, flagKey), budget)
		if err != nil {
			return Result{}, err
		}
		if depRes.On != dep.RequireOn {
			trace.On = false
			trace.Reason = ReasonPrerequisiteFailed
			trace.PrereqChain = append(append(append([]string(nil), chain...), flagKey), dep.FlagKey)
			return Result{On: false, Trace: trace}, nil
		}
	}

	nowTime := parseNow(e.opts.Now)

	// 2/5. 覆盖
	global := lookupActiveOverride(snap, flagKey, GlobalOverrideSubject, nowTime)
	subject := lookupActiveOverride(snap, flagKey, s.ID, nowTime)

	pick := func(ov *Override, reason Reason) (Result, error) {
		trace.Reason = reason
		trace.On = ov.On
		return Result{On: ov.On, Trace: trace}, nil
	}
	if global != nil && !global.On {
		return pick(global, ReasonGlobalOverride)
	}
	if subject != nil && !subject.On {
		return pick(subject, ReasonSubjectOverride)
	}
	if subject != nil && subject.On {
		return pick(subject, ReasonSubjectOverride)
	}
	if global != nil && global.On {
		return pick(global, ReasonGlobalOverride)
	}

	// 3. 定向规则（使用快照内固定顺序）
	ordered := snap.rulesOrder[flagKey]
	if idx, rule := firstMatch(ordered, s); rule != nil {
		*budget -= len(ordered) - idx // 粗略记账：每条规则比较也消耗预算
		bucket := computeBucket(flag, s.ID)
		trace.MatchedRule = rule.ID
		trace.Variation = rule.Variation
		trace.Bucket = bucket
		trace.Percentage = rule.Percentage
		if inBucket(bucket, rule.Percentage) {
			trace.Reason = ReasonRule
			trace.On = rule.On
			return Result{On: rule.On, Trace: trace}, nil
		}
		// 命中规则但未进放量集合：结果回到该开关的默认值，
		// 原因标记为 percentage_miss，便于事后解释"为什么没开"。
		trace.Reason = ReasonRuleThrottled
		trace.On = flag.DefaultOn
		return Result{On: flag.DefaultOn, Trace: trace}, nil
	}
	*budget -= len(ordered)

	// 4. 默认回落
	trace.Reason = ReasonDefault
	trace.On = flag.DefaultOn
	return Result{On: flag.DefaultOn, Trace: trace}, nil
}

// lookupActiveOverride 查找未过期的覆盖；过期覆盖视为不存在。
func lookupActiveOverride(snap *snapshot, flagKey, subjectID string, now time.Time) *Override {
	subs := snap.config.Overrides[flagKey]
	if subs == nil {
		return nil
	}
	ov := subs[subjectID]
	if ov == nil {
		return nil
	}
	if !ov.ExpiresAt.IsZero() && !now.Before(ov.ExpiresAt) {
		return nil
	}
	return ov
}

// Commit 原子提交一个完整新版本：
// 校验/建快照成功后一次性原子替换当前指针；失败则旧版本继续服务。
// 新版本号必须严格大于历史最大版本号。
func (e *Engine) Commit(cfg *Config) error {
	snap, err := buildSnapshot(cfg)
	if err != nil {
		e.audit(AuditEvent{Type: "reject", Version: versionOf(cfg), Detail: err.Error()})
		return err
	}
	e.mu.Lock()
	if snap.config.Version <= e.maxVer {
		e.mu.Unlock()
		err := fmt.Errorf("%w: version %d must be greater than %d",
			ErrInvalidConfig, snap.config.Version, e.maxVer)
		e.audit(AuditEvent{Type: "reject", Version: snap.config.Version, Detail: err.Error()})
		return err
	}
	e.history = append(e.history, snap)
	if len(e.history) > e.opts.HistorySize {
		// 保留最近 HistorySize 个版本，但当前版本永不被裁剪（它总在末尾）。
		e.history = e.history[len(e.history)-e.opts.HistorySize:]
	}
	e.maxVer = snap.config.Version
	e.current.Store(snap)
	e.mu.Unlock()

	e.audit(AuditEvent{Type: "commit", Version: snap.config.Version, Detail: snap.config.Comment})
	return nil
}

// Rollback 原子切换到历史中的指定版本。回滚不改变 flag 定义本身，
// 因此同一主体的分桶与评估结果与该版本当时完全一致。
func (e *Engine) Rollback(version int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var target *snapshot
	for _, snap := range e.history {
		if snap.config.Version == version {
			target = snap
			break
		}
	}
	if target == nil {
		err := fmt.Errorf("%w: %d", ErrVersionNotFound, version)
		e.audit(AuditEvent{Type: "reject", Version: version, Detail: err.Error()})
		return err
	}
	e.current.Store(target)
	e.audit(AuditEvent{Type: "rollback", Version: version})
	return nil
}

// CurrentVersion 返回当前服务中的配置版本；无版本时返回 0。
func (e *Engine) CurrentVersion() int64 {
	if snap := e.current.Load(); snap != nil {
		return snap.config.Version
	}
	return 0
}

// Versions 返回历史中可回滚的版本号（从旧到新）。
func (e *Engine) Versions() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]int64, 0, len(e.history))
	for _, snap := range e.history {
		out = append(out, snap.config.Version)
	}
	return out
}

// SetOverride 新增/更新一条覆盖（全局时 SubjectID 传 "*" 或 ""）。
// 覆盖以"新版本"形式原子提交：旧版本留在历史中，可审计、可回滚。
func (e *Engine) SetOverride(ov Override) error {
	if e.opts.Authorizer == nil || !e.opts.Authorizer.CanOverride(ov.Operator) {
		e.audit(AuditEvent{Type: "reject", FlagKey: ov.FlagKey,
			SubjectID: ov.SubjectID, Operator: ov.Operator, Reason: ov.Reason,
			Detail: "unauthorized override attempt"})
		return fmt.Errorf("%w: operator %q", ErrUnauthorized, ov.Operator)
	}
	if ov.FlagKey == "" {
		return fmt.Errorf("%w: empty flag key", ErrInvalidOverride)
	}
	if ov.SubjectID == "" {
		ov.SubjectID = GlobalOverrideSubject
	}
	if ov.Reason == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalidOverride)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.current.Load()
	if cur == nil {
		return fmt.Errorf("%w: no committed config yet", ErrInvalidConfig)
	}
	if _, ok := cur.config.Flags[ov.FlagKey]; !ok {
		return fmt.Errorf("%w: flag %q", ErrFlagNotFound, ov.FlagKey)
	}
	next := cloneConfig(cur.config)
	next.Version = e.maxVer + 1
	next.Comment = "override:" + ov.Operator
	if next.Overrides[ov.FlagKey] == nil {
		next.Overrides[ov.FlagKey] = map[string]*Override{}
	}
	cp := ov
	next.Overrides[ov.FlagKey][ov.SubjectID] = &cp
	snap, err := buildSnapshot(next)
	if err != nil {
		return err
	}
	e.history = append(e.history, snap)
	e.maxVer = snap.config.Version
	e.current.Store(snap)

	on := ov.On
	e.audit(AuditEvent{
		Type: "override", Version: snap.config.Version, FlagKey: ov.FlagKey,
		SubjectID: ov.SubjectID, On: &on, Operator: ov.Operator, Reason: ov.Reason,
	})
	return nil
}

// RemoveOverride 移除一条主体级/全局覆盖。同样生成新版本并原子提交。
func (e *Engine) RemoveOverride(flagKey, subjectID, operator string) error {
	if e.opts.Authorizer == nil || !e.opts.Authorizer.CanOverride(operator) {
		e.audit(AuditEvent{Type: "reject", FlagKey: flagKey, SubjectID: subjectID,
			Operator: operator, Detail: "unauthorized override removal"})
		return fmt.Errorf("%w: operator %q", ErrUnauthorized, operator)
	}
	if subjectID == "" {
		subjectID = GlobalOverrideSubject
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.current.Load()
	if cur == nil {
		return fmt.Errorf("%w: no committed config yet", ErrInvalidConfig)
	}
	subs, ok := cur.config.Overrides[flagKey]
	if !ok {
		return fmt.Errorf("%w: %q/%q", ErrInvalidOverride, flagKey, subjectID)
	}
	if _, ok := subs[subjectID]; !ok {
		return fmt.Errorf("%w: %q/%q", ErrInvalidOverride, flagKey, subjectID)
	}
	next := cloneConfig(cur.config)
	next.Version = e.maxVer + 1
	next.Comment = "override_remove:" + operator
	delete(next.Overrides[flagKey], subjectID)
	if len(next.Overrides[flagKey]) == 0 {
		delete(next.Overrides, flagKey)
	}
	snap, err := buildSnapshot(next)
	if err != nil {
		return err
	}
	e.history = append(e.history, snap)
	e.maxVer = snap.config.Version
	e.current.Store(snap)

	e.audit(AuditEvent{
		Type: "override_remove", Version: snap.config.Version, FlagKey: flagKey,
		SubjectID: subjectID, Operator: operator,
	})
	return nil
}

// QueryTrace 按 flagKey+subjectID 查询最近一条匹配的评估记录。
func (e *Engine) QueryTrace(flagKey, subjectID string) (Trace, bool) {
	e.tracesMu.Lock()
	defer e.tracesMu.Unlock()
	total := len(e.traces)
	c := cap(e.traces)
	// 从最新向最旧扫描；环形缓冲不变量：tracePos 指向下一个写入位置。
	for i := 0; i < total; i++ {
		idx := (e.tracePos - 1 - i + c) % c
		if idx >= total {
			continue
		}
		t := e.traces[idx]
		if t.FlagKey == flagKey && t.SubjectID == subjectID {
			return t, true
		}
	}
	return Trace{}, false
}

// RecentTraces 返回最近 n 条评估记录（按时间倒序）。
func (e *Engine) RecentTraces(n int) []Trace {
	e.tracesMu.Lock()
	defer e.tracesMu.Unlock()
	total := len(e.traces)
	c := cap(e.traces)
	if n <= 0 || n > total {
		n = total
	}
	out := make([]Trace, 0, n)
	for i := 0; i < total && len(out) < n; i++ {
		idx := (e.tracePos - 1 - i + c) % c
		if idx >= total {
			continue
		}
		out = append(out, e.traces[idx])
	}
	return out
}

// parseNow 返回引擎注入时钟的当前时间；未注入时用 time.Now。
func parseNow(fn func() time.Time) time.Time {
	if fn != nil {
		return fn()
	}
	return time.Now()
}

func versionOf(cfg *Config) int64 {
	if cfg == nil {
		return 0
	}
	return cfg.Version
}
