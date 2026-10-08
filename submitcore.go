package main

import "fmt"

// 维修提交验收链规则核心：提交、验收（通过/退回）、直接关闭与取消的业务判定、
// 提交身份与工单状态推导全部收敛于此。
//
// 日常操作（submit/accept/close/cancel）、台账加载校验、保存前校验、ticket
// 当前提交查询与 replay 回看共用本核心，不再各路径分别维护重复规则；命令与
// 持久化层只按场景把核心返回的规则错误包装成 errConflict 或“履历矛盾”。
//
// 核心按全库履历序号逐步采纳履历，每一步都以当时的工单状态做判定，绝不用
// 最终状态代替历史状态。核心只推进自己的派生状态，从不修改传入的履历、工单
// 或资产等业务记录，也不借校验补字段或修复数据；判定失败时派生状态保持
// 判定前原样。

// repairRuleCode 为提交验收链规则冲突的类别，供各场景包装成各自的错误信息。
type repairRuleCode int

const (
	chainDuplicateReport     repairRuleCode = iota // 工单被重复报修
	chainSubmitNotOpen                             // 提交时工单不在未关闭状态
	chainAcceptNotPending                          // 验收时工单不在待验收状态
	chainTargetNotSubmit                           // 验收目标序号不是提交履历
	chainTargetOtherTicket                         // 验收目标提交属于其他工单
	chainTargetProcessed                           // 目标提交已处理或不是当前提交
	chainTargetUnknown                             // 目标序号未知（含尚未出现的未来序号）
	chainCloseAfterSubmit                          // 首次提交后出现直接关闭
	chainCloseResultMismatch                       // 待关闭中间态的关闭内容与提交结果不符
	chainCloseBadState                             // 关闭时工单状态不允许关闭
	chainCancelBadState                            // 取消时工单状态不允许取消
)

// repairRuleError 为核心判定返回的规则冲突，携带各场景包装错误所需的上下文。
type repairRuleError struct {
	code        repairRuleCode
	ticketID    string
	seq         int    // 触发判定的履历序号
	target      int    // 验收目标提交序号
	targetKind  string // 目标序号实际履历类型（chainTargetNotSubmit）
	otherTicket string // 目标提交实际归属工单（chainTargetOtherTicket）
	state       string // 当时由履历推出的工单状态
}

func (e *repairRuleError) Error() string {
	return fmt.Sprintf("提交验收链规则冲突 %d（工单 %s，履历序号 %d）", int(e.code), e.ticketID, e.seq)
}

// ticketRepair 为单工单提交验收链的派生状态。
//
// state 取值：""（尚未报修）、ticketOpen（未关闭）、ticketPending（待验收）、
// derivedApproved（验收通过待关闭中间态）、ticketClosed（已关闭）、
// ticketCancelled（已取消）。derivedApproved 只存在于验收通过履历与紧随的
// 关闭履历之间（同一次保存），绝不会作为保存状态出现。
type ticketRepair struct {
	state          string
	submittedEver  bool   // 是否曾提交（含已退回的提交）
	pendingSeq     int    // 当前待验收提交的全库履历序号
	pendingResult  string // 当前待验收提交的维修结果
	approvedResult string // 待关闭中间态下该次通过验收的提交结果
}

// repairCore 为全部工单提交验收链的重放器。
type repairCore struct {
	tickets map[string]*ticketRepair
	// bySeq 记录已采纳履历序号对应的履历，供验收目标分类（未知序号、非提交
	// 序号、跨单、已处理或过时目标）；候选履历（Seq 为 0）不记入。
	bySeq map[int]Event
}

func newRepairCore() *repairCore {
	return &repairCore{tickets: map[string]*ticketRepair{}, bySeq: map[int]Event{}}
}

// state 返回工单由履历链推出的状态；从未报修的工单返回空串。
func (c *repairCore) state(ticketID string) string {
	if t := c.tickets[ticketID]; t != nil {
		return t.state
	}
	return ""
}

// pending 返回工单当前待验收提交的全库履历序号与维修结果；无待验收提交时
// ok=false。
func (c *repairCore) pending(ticketID string) (seq int, result string, ok bool) {
	t := c.tickets[ticketID]
	if t == nil || t.state != ticketPending || t.pendingSeq == 0 {
		return 0, "", false
	}
	return t.pendingSeq, t.pendingResult, true
}

// everSubmitted 报告工单是否曾提交过维修（含已退回的提交）。首次提交后工单
// 只能验收通过或取消终结，不能再直接关闭。
func (c *repairCore) everSubmitted(ticketID string) bool {
	t := c.tickets[ticketID]
	return t != nil && t.submittedEver
}

// apply 按全库履历序号顺序采纳一条履历：报修、提交、验收、关闭、取消按当时
// 状态判定并推进派生状态，其余履历忽略。判定失败返回 *repairRuleError，派生
// 状态保持判定前原样。传入的履历不被修改。
func (c *repairCore) apply(e Event) error {
	if e.Seq > 0 {
		c.bySeq[e.Seq] = e
	}
	switch e.Kind {
	case eventReport:
		return c.stateOf(e.TicketID).applyReport(e)
	case eventSubmit:
		return c.stateOf(e.TicketID).applySubmit(e)
	case eventAccept:
		if err := c.stateOf(e.TicketID).applyAccept(e, c.targetError(e)); err != nil {
			return err
		}
		// 退回/通过后该提交不再是待验收提交；目标分类映射不再变化。
		return nil
	case eventClose:
		return c.stateOf(e.TicketID).applyClose(e)
	case eventCancel:
		return c.stateOf(e.TicketID).applyCancel(e)
	}
	return nil
}

// stateOf 返回工单的派生状态对象，不存在时新建（报修前为空状态）。
func (c *repairCore) stateOf(ticketID string) *ticketRepair {
	t := c.tickets[ticketID]
	if t == nil {
		t = &ticketRepair{}
		c.tickets[ticketID] = t
	}
	return t
}

// targetError 按当前已采纳履历把验收目标序号分类为具体的规则冲突；目标恰为
// 当前待验收提交时调用方不会调用本函数。
func (c *repairCore) targetError(e Event) error {
	ev, seen := c.bySeq[e.TargetSeq]
	if !seen {
		return &repairRuleError{
			code: chainTargetUnknown, ticketID: e.TicketID, seq: e.Seq,
			target: e.TargetSeq,
		}
	}
	if ev.Kind != eventSubmit {
		return &repairRuleError{
			code: chainTargetNotSubmit, ticketID: e.TicketID, seq: e.Seq,
			target: e.TargetSeq, targetKind: ev.Kind,
		}
	}
	if ev.TicketID != e.TicketID {
		return &repairRuleError{
			code: chainTargetOtherTicket, ticketID: e.TicketID, seq: e.Seq,
			target: e.TargetSeq, otherTicket: ev.TicketID,
		}
	}
	return &repairRuleError{
		code: chainTargetProcessed, ticketID: e.TicketID, seq: e.Seq,
		target: e.TargetSeq,
	}
}

// applyReport 判定并采纳报修履历：每张工单恰有一条报修，报修后进入未关闭。
func (t *ticketRepair) applyReport(e Event) error {
	if t.state != "" {
		return &repairRuleError{code: chainDuplicateReport, ticketID: e.TicketID, seq: e.Seq, state: t.state}
	}
	t.state = ticketOpen
	return nil
}

// applySubmit 判定并采纳提交履历：仅未关闭工单可提交，成功后转为待验收，
// 提交履历的全库序号即该次提交身份。
func (t *ticketRepair) applySubmit(e Event) error {
	if t.state != ticketOpen {
		return &repairRuleError{code: chainSubmitNotOpen, ticketID: e.TicketID, seq: e.Seq, state: t.state}
	}
	t.state = ticketPending
	t.pendingSeq = e.Seq
	t.pendingResult = e.Content
	t.submittedEver = true
	return nil
}

// applyAccept 判定并采纳验收履历：仅当前待验收提交可处理，targetError 已按
// 未知、非提交、跨单、已处理或过时分类。通过后进入验收通过待关闭中间态，
// 由紧随的关闭履历终结；退回后恢复未关闭，可再次提交并产生新身份。
func (t *ticketRepair) applyAccept(e Event, targetErr error) error {
	if t.state != ticketPending {
		return &repairRuleError{code: chainAcceptNotPending, ticketID: e.TicketID, seq: e.Seq, state: t.state}
	}
	if e.TargetSeq != t.pendingSeq {
		// targetErr 已携带具体类别与上下文。
		return targetErr
	}
	if e.Decision == decisionApprove {
		t.state = derivedApproved
		t.approvedResult = t.pendingResult
	} else {
		t.state = ticketOpen
	}
	t.pendingSeq = 0
	t.pendingResult = ""
	return nil
}

// applyClose 判定并采纳关闭履历：从未提交的工单可直接关闭；首次提交后只能
// 验收通过或取消终结。待关闭中间态只接受内容与该次提交结果一致的关闭履历，
// 且它必须紧随验收通过（状态本身即保证紧邻，中间不允许其他提交验收事件）。
func (t *ticketRepair) applyClose(e Event) error {
	switch t.state {
	case ticketOpen:
		if t.submittedEver {
			return &repairRuleError{code: chainCloseAfterSubmit, ticketID: e.TicketID, seq: e.Seq, state: t.state}
		}
	case derivedApproved:
		if e.Content != t.approvedResult {
			return &repairRuleError{
				code: chainCloseResultMismatch, ticketID: e.TicketID, seq: e.Seq, state: t.state,
			}
		}
	default:
		return &repairRuleError{code: chainCloseBadState, ticketID: e.TicketID, seq: e.Seq, state: t.state}
	}
	t.state = ticketClosed
	t.approvedResult = ""
	return nil
}

// applyCancel 判定并采纳取消履历：未关闭与待验收工单均可取消；待验收取消
// 保留全部提交履历且不填写维修结果。终态工单不能再取消。
func (t *ticketRepair) applyCancel(e Event) error {
	if t.state != ticketOpen && t.state != ticketPending {
		return &repairRuleError{code: chainCancelBadState, ticketID: e.TicketID, seq: e.Seq, state: t.state}
	}
	t.state = ticketCancelled
	t.pendingSeq = 0
	t.pendingResult = ""
	return nil
}
