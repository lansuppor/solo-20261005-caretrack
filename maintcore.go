package main

import "fmt"

// 周期保养规则核心：建立、完成、撤销与方案调整的业务判定及状态推进。
//
// 日常操作（plan/maintain/unmaintain/adjust）、台账加载校验、保存前校验与
// 导入后校验共用本核心，不再各路径分别维护重复规则；命令与持久化层只按
// 场景包装核心返回的规则错误（errConflict 或“履历矛盾”）。
//
// 核心按全库履历序号逐步采纳履历：每一步都以当时的方案段方案、有效完成
// 集合与停用状态做判定，绝不用最终状态代替历史状态。核心只推进自己的
// 派生状态，从不修改传入的履历、计划或资产等业务记录，也不借校验补字段
// 或修复数据；判定失败时派生状态保持判定前原样。

// maintRuleCode 为保养规则冲突的类别，供各场景包装成各自的错误信息。
type maintRuleCode int

const (
	ruleDuplicateCreate  maintRuleCode = iota // 同一资产多条建立履历
	ruleBeforeCreate                          // 保养履历出现在建立履历之前
	ruleAdjustScheme                          // 调整记录的原方案与当时方案不符
	ruleAdjustNextDue                         // 调整记录的原下一到期日与当时不符
	ruleAdjustFirstDue                        // 新首次到期日未严格晚于未撤销完成的完成日
	ruleRevokeOldSegment                      // 撤销目标属于调整前的旧方案段
	ruleRevokeUnknown                         // 撤销目标不是该资产先前的完成履历
	ruleRevokeDuplicate                       // 完成被重复撤销
	ruleRevokeNotLatest                       // 撤销目标不是当时最新的有效完成
	ruleDoneDeactivated                       // 停用期间登记保养完成
	ruleDueMismatch                           // 完成周期到期日与当时下一到期日不接续
	ruleDoneBeforeDue                         // 实际完成日早于周期到期日
	ruleNextOverflow                          // 推算的下一到期日超出 9999-12-31
)

// maintRuleError 为核心判定返回的规则冲突，携带各场景包装错误所需的上下文。
type maintRuleError struct {
	code    maintRuleCode
	assetID string
	seq     int    // 触发判定的履历序号
	kind    string // 触发履历类型（ruleBeforeCreate）
	target  int    // 撤销目标完成履历序号
	latest  int    // 当时最新有效完成履历序号（ruleRevokeNotLatest）
	doneSeq int    // 相关完成履历序号（ruleAdjustFirstDue）
	dateA   string // 相关日期：填写到期日/完成日/新首次到期日/记录的原下一到期日
	dateB   string // 对照日期：当时下一到期日/周期到期日/完成日
}

func (e *maintRuleError) Error() string {
	return fmt.Sprintf("保养规则冲突 %d（资产 %s，履历序号 %d）", int(e.code), e.assetID, e.seq)
}

// isPlanEvent 报告履历类型是否为保养履历（建立/完成/撤销/调整）。
func isPlanEvent(kind string) bool {
	switch kind {
	case eventPlanCreate, eventPlanDone, eventPlanRevoke, eventPlanAdjust:
		return true
	}
	return false
}

// assetMaint 为单资产保养履历链的派生状态：当前方案段方案、由履历推出的
// 下一到期日、当前段完成链与撤销集合。段边界按建立与调整履历的全库序号
// 划分，不按日期或数组位置。
type assetMaint struct {
	created  bool           // 是否已有建立履历
	content  string         // 当前方案段的保养内容
	firstDue string         // 当前方案段的首次到期日
	interval int            // 当前方案段的间隔天数
	nextDue  string         // 由履历链推出的下一到期日
	segDones []int          // 当前段完成履历序号（按序号递增追加）
	doneDue  map[int]string // 当前段完成序号 -> 周期到期日
	allDone  map[int]bool   // 全部完成序号（含旧段，用于识别旧段撤销）
	validDay map[int]int64  // 未撤销完成序号 -> 实际完成日（跨全部方案段）
	revoked  map[int]bool   // 已撤销完成序号
}

func newAssetMaint() *assetMaint {
	return &assetMaint{
		doneDue:  map[int]string{},
		allDone:  map[int]bool{},
		validDay: map[int]int64{},
		revoked:  map[int]bool{},
	}
}

// maintCore 为全部资产的保养派生状态与停用状态的重放器。
type maintCore struct {
	assets      map[string]*assetMaint
	deactivated map[string]bool
}

func newMaintCore() *maintCore {
	return &maintCore{assets: map[string]*assetMaint{}, deactivated: map[string]bool{}}
}

// state 返回资产的派生状态；尚无保养履历的资产返回空状态。
func (c *maintCore) state(assetID string) *assetMaint {
	st := c.assets[assetID]
	if st == nil {
		st = newAssetMaint()
		c.assets[assetID] = st
	}
	return st
}

// deactivated 报告资产当前是否处于停用状态（由停用/恢复使用履历链推出；
// 与保存的资产状态一致，由整库一致性校验保证）。
func (c *maintCore) deactivatedNow(assetID string) bool {
	return c.deactivated[assetID]
}

// derived 返回资产由履历链推出的当前段方案与下一到期日；
// created 报告是否已有建立履历。
func (c *maintCore) derived(assetID string) (content, firstDue string, interval int, nextDue string, created bool) {
	st := c.state(assetID)
	return st.content, st.firstDue, st.interval, st.nextDue, st.created
}

// apply 按全库履历序号顺序采纳一条履历：保养履历按当时状态判定并推进派生
// 状态，停用/恢复使用履历更新停用状态，其余履历忽略。判定失败返回
// *maintRuleError，派生状态保持判定前原样。传入的履历不被修改。
func (c *maintCore) apply(e Event) error {
	switch e.Kind {
	case eventDeactivate:
		c.deactivated[e.AssetID] = true
		return nil
	case eventReactivate:
		delete(c.deactivated, e.AssetID)
		return nil
	}
	if !isPlanEvent(e.Kind) {
		return nil
	}
	st := c.state(e.AssetID)
	switch e.Kind {
	case eventPlanCreate:
		return st.applyCreate(e)
	case eventPlanDone:
		return st.applyDone(e, c.deactivated[e.AssetID])
	case eventPlanRevoke:
		return st.applyRevoke(e)
	default: // eventPlanAdjust
		return st.applyAdjust(e)
	}
}

// applyCreate 判定并采纳建立履历：每项资产恰一条，开启首个方案段，
// 下一到期日即首次到期日。
func (m *assetMaint) applyCreate(e Event) error {
	if m.created {
		return &maintRuleError{code: ruleDuplicateCreate, assetID: e.AssetID, seq: e.Seq}
	}
	m.created = true
	m.content, m.firstDue, m.interval = e.Content, e.Due, e.Interval
	m.nextDue = e.Due
	return nil
}

// applyDone 判定并采纳完成履历：须在建立之后、非停用期间，周期到期日接续
// 当时下一到期日，实际完成日不早于周期到期日；按本段首次日加整数倍间隔
// 推进到严格晚于完成日的最早日期，延期跨过的周期不补记录，越界整次拒绝。
func (m *assetMaint) applyDone(e Event, deactivated bool) error {
	if !m.created {
		return &maintRuleError{code: ruleBeforeCreate, assetID: e.AssetID, seq: e.Seq, kind: e.Kind}
	}
	if deactivated {
		return &maintRuleError{code: ruleDoneDeactivated, assetID: e.AssetID, seq: e.Seq}
	}
	if e.Due != m.nextDue {
		return &maintRuleError{code: ruleDueMismatch, assetID: e.AssetID, seq: e.Seq, dateA: e.Due, dateB: m.nextDue}
	}
	dueN, _ := parseDate(e.Due)
	doneN, _ := parseDate(e.Done)
	if doneN < dueN {
		return &maintRuleError{code: ruleDoneBeforeDue, assetID: e.AssetID, seq: e.Seq, dateA: e.Done, dateB: e.Due}
	}
	first, _ := parseDate(m.firstDue)
	next, ok := nextDueAfter(first, int64(m.interval), doneN)
	if !ok {
		return &maintRuleError{code: ruleNextOverflow, assetID: e.AssetID, seq: e.Seq, dateA: e.Done}
	}
	m.nextDue = formatDate(next)
	m.segDones = append(m.segDones, e.Seq)
	m.doneDue[e.Seq] = e.Due
	m.allDone[e.Seq] = true
	m.validDay[e.Seq] = doneN
	return nil
}

// applyRevoke 判定并采纳撤销履历：目标须为当前方案段内先前的完成履历、
// 未撤销且为当时最新有效；撤销后下一到期日恢复为该完成的周期到期日，
// 其完成日不再限制后续调整。旧段完成不能再撤销。
func (m *assetMaint) applyRevoke(e Event) error {
	if !m.created {
		return &maintRuleError{code: ruleBeforeCreate, assetID: e.AssetID, seq: e.Seq, kind: e.Kind}
	}
	due, ok := m.doneDue[e.TargetSeq]
	if !ok {
		if m.allDone[e.TargetSeq] {
			return &maintRuleError{code: ruleRevokeOldSegment, assetID: e.AssetID, seq: e.Seq, target: e.TargetSeq}
		}
		return &maintRuleError{code: ruleRevokeUnknown, assetID: e.AssetID, seq: e.Seq, target: e.TargetSeq}
	}
	if m.revoked[e.TargetSeq] {
		return &maintRuleError{code: ruleRevokeDuplicate, assetID: e.AssetID, seq: e.Seq, target: e.TargetSeq}
	}
	latest := 0
	for _, seq := range m.segDones {
		if !m.revoked[seq] {
			latest = seq
		}
	}
	if latest != e.TargetSeq {
		return &maintRuleError{code: ruleRevokeNotLatest, assetID: e.AssetID, seq: e.Seq, target: e.TargetSeq, latest: latest}
	}
	m.revoked[e.TargetSeq] = true
	delete(m.validDay, e.TargetSeq)
	m.nextDue = due
	return nil
}

// applyAdjust 判定并采纳调整履历：记录的原方案与原下一到期日须与当时状态
// 一致，新首次到期日须严格晚于当时所有未撤销完成（跨全部方案段）的实际
// 完成日。采纳后开启新方案段：采用新方案，下一到期日设为新首次到期日，
// 不补任何完成记录，旧段完成不能再撤销。
func (m *assetMaint) applyAdjust(e Event) error {
	if !m.created {
		return &maintRuleError{code: ruleBeforeCreate, assetID: e.AssetID, seq: e.Seq, kind: e.Kind}
	}
	if e.OldContent != m.content || e.OldDue != m.firstDue || e.OldInterval != m.interval {
		return &maintRuleError{code: ruleAdjustScheme, assetID: e.AssetID, seq: e.Seq}
	}
	if e.OldNextDue != m.nextDue {
		return &maintRuleError{code: ruleAdjustNextDue, assetID: e.AssetID, seq: e.Seq, dateA: e.OldNextDue, dateB: m.nextDue}
	}
	newFirst, _ := parseDate(e.Due)
	// 未撤销完成的完成日沿段内严格递增（各完成日不早于其周期到期日，而到期日
	// 严格推进；跨段的新首次日又须晚于当时全部有效完成日），取最大者即判定
	// 边界，结果与逐一比较等价且确定。
	maxSeq, maxDay := 0, int64(-1)
	for seq, day := range m.validDay {
		if day > maxDay {
			maxSeq, maxDay = seq, day
		}
	}
	if maxDay >= 0 && newFirst <= maxDay {
		return &maintRuleError{code: ruleAdjustFirstDue, assetID: e.AssetID, seq: e.Seq, dateA: e.Due, doneSeq: maxSeq, dateB: formatDate(maxDay)}
	}
	m.content, m.firstDue, m.interval = e.NewContent, e.Due, e.Interval
	m.nextDue = e.Due
	m.segDones = nil
	m.doneDue = map[int]string{}
	return nil
}
