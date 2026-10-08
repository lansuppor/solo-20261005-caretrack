package main

import "fmt"

// 维修提交验收链规则核心：提交、验收、直接关闭与取消的业务判定、提交身份与
// 工单状态推导全部收敛于此。
//
// 日常操作（submit/accept/close/cancel）以候选履历先走一遍核心判定，通过后
// 才分配序号、修改业务记录；台账加载与保存前校验按全库履历序号重放同一核心；
// ticket 的当前待验收提交查询与 replay 截止回看同样只依据核心推出的状态。
// 各路径不再分别维护“是否提交过/当前提交是谁/工单处于何态”的重复判定。
//
// 核心按全库履历序号逐步采纳履历，每一步都以当时的工单状态做判定：
//   - 报修使工单进入未关闭；
//   - 提交仅在未关闭时接受非空维修结果，工单转为待验收，提交履历的全库序号
//     即该次提交身份；
//   - 验收仅在待验收时接受，且目标序号必须恰为当前待验收提交，未知序号、非
//     提交序号、跨单提交、已处理或过时目标一律拒绝；通过进入“验收通过待关闭”
//     中间态（内容锁定为该次提交结果），紧随的同单关闭履历内容匹配才终结，
//     退回则恢复未关闭，可再次提交并产生新身份；
//   - 关闭在未关闭且从未提交时直接接受，在待关闭中间态只接受内容匹配的关闭，
//     其余状态拒绝；
//   - 取消在未关闭或待验收时接受，清除当前待验收提交，保留全部提交历史。
//
// “验收通过待关闭”只存在于同次保存的验收履历与关闭履历之间，绝不会作为
// 保存状态出现：截止回看纳入验收、尚未纳入关闭时，工单仍未终结、资产仍维修
// 中，但当前待验收提交为无。
//
// 核心只推进自己的派生状态，从不修改传入的履历、工单或资产等业务记录，也不
// 借校验补字段或修复数据；判定失败时派生状态保持判定前原样。

// repairRuleCode 为维修链规则冲突的类别，供各场景包装成各自的错误信息。
type repairRuleCode int

const (
	ruleRepairDuplicateReport   repairRuleCode = iota // 同一工单多条报修履历
	ruleRepairNoReport                                // 终结/提交类履历出现在报修履历之前
	ruleSubmitNotOpen                                 // 提交时工单不是未关闭
	ruleSubmitEmptyResult                             // 提交维修结果为空
	ruleCloseNoReport                                 // 关闭履历出现在报修履历之前
	ruleCloseAlreadyClosed                            // 已关闭工单重复关闭
	ruleCloseAlreadyCancelled                         // 已取消工单被关闭
	ruleClosePending                                  // 待验收工单被直接关闭
	ruleCloseAfterSubmit                              // 首次提交后直接关闭
	ruleCloseApprovedMismatch                         // 待关闭中间态的关闭内容与验收提交结果不一致
	ruleCloseEmptyResult                              // 关闭维修结果为空
	ruleCancelNoReport                                // 取消履历出现在报修履历之前
	ruleCancelAlreadyClosed                           // 已关闭工单被取消
	ruleCancelAlreadyCancelled                        // 已取消工单重复取消
	ruleCancelApprovedPending                         // 验收通过待关闭中间态出现取消履历
	ruleAcceptNotPending                              // 验收时工单不是待验收
	ruleAcceptEmptyComment                            // 验收意见为空
	ruleAcceptBadDecision                             // 验收决定既不是通过也不是退回
	ruleAcceptTargetUnknown                           // 验收目标序号不存在
	ruleAcceptTargetNotSubmit                         // 验收目标序号不是提交履历
	ruleAcceptTargetCrossTicket                       // 验收目标属于其他工单
	ruleAcceptTargetProcessed                         // 验收目标已处理或不是当前提交
)

// repairRuleError 为核心判定返回的规则冲突，携带各场景包装错误所需的上下文。
type repairRuleError struct {
	code     repairRuleCode
	ticketID string
	seq      int    // 触发判定的履历序号
	target   int    // 验收目标提交序号
	from     string // 判定前的工单状态
	kind     string // 目标序号实际的履历类型（ruleAcceptTargetNotSubmit）
	owner    string // 目标序号实际归属的工单（ruleAcceptTargetCrossTicket）
	contentA string // 关闭内容 / 验收锁定的提交结果
	contentB string // 对照的提交结果
}

func (e *repairRuleError) Error() string {
	return fmt.Sprintf("维修链规则冲突 %d（工单 %s，履历序号 %d）", int(e.code), e.ticketID, e.seq)
}

// ticketRepair 为单张工单的提交验收链派生状态。
type ticketRepair struct {
	status         string // 空串：尚未报修；否则为未关闭/待验收/验收通过待关闭/已关闭/已取消
	submittedEver  bool   // 是否曾提交（含已退回的提交）
	pendingSeq     int    // 当前待验收提交的全库履历序号
	pendingResult  string // 当前待验收提交的维修结果
	approvedResult string // 验收通过待关闭时锁定的提交维修结果
}

// repairCore 为全部工单的维修链派生状态重放器。eventBySeq 收录已采纳履历中
// 每个全库序号对应的履历，用于验收目标的身份分类。
type repairCore struct {
	tickets    map[string]*ticketRepair
	eventBySeq map[int]Event
}

func newRepairCore() *repairCore {
	return &repairCore{tickets: map[string]*ticketRepair{}, eventBySeq: map[int]Event{}}
}

// state 返回工单的派生状态；尚无报修履历的工单返回空状态（不代表业务工单存在）。
func (c *repairCore) state(ticketID string) *ticketRepair {
	st := c.tickets[ticketID]
	if st == nil {
		st = &ticketRepair{}
		c.tickets[ticketID] = st
	}
	return st
}

// status 返回工单由履历链推出的状态；从未报修时为空串。注意“验收通过待关闭”
// 中间态也由此返回，调用方不得把它当作可保存状态。
func (c *repairCore) status(ticketID string) string {
	st := c.tickets[ticketID]
	if st == nil {
		return ""
	}
	return st.status
}

// submittedEver 报告工单是否曾提交过维修（含已退回的提交）。
func (c *repairCore) submittedEver(ticketID string) bool {
	st := c.tickets[ticketID]
	return st != nil && st.submittedEver
}

// pending 返回工单当前待验收提交的全库履历序号与维修结果；无待验收提交（未
// 提交、已退回、待关闭中间态、已终结）时 ok=false。
func (c *repairCore) pending(ticketID string) (seq int, result string, ok bool) {
	st := c.tickets[ticketID]
	if st == nil || st.status != ticketPending {
		return 0, "", false
	}
	return st.pendingSeq, st.pendingResult, true
}

// apply 按全库履历序号顺序采纳一条履历：维修链履历按当时状态判定并推进派生
// 状态，其余履历忽略。判定失败返回 *repairRuleError，派生状态（含序号索引）
// 保持判定前原样。传入的履历不被修改。候选履历（日常操作预判）可使用零序号；
// 此时它不会进入 eventBySeq 索引，也不影响其他工单的判定。
func (c *repairCore) apply(e Event) error {
	var err error
	switch e.Kind {
	case eventReport:
		err = c.applyReport(e)
	case eventSubmit:
		err = c.applySubmit(e)
	case eventAccept:
		err = c.applyAccept(e)
	case eventClose:
		err = c.applyClose(e)
	case eventCancel:
		err = c.applyCancel(e)
	}
	// 仅在判定通过后才收录序号索引，保证失败不留下任何派生痕迹。
	if err == nil && e.Seq > 0 {
		c.eventBySeq[e.Seq] = e
	}
	return err
}

// applyReport 判定并采纳报修履历：每张工单恰有一条，报修后工单未关闭。
func (c *repairCore) applyReport(e Event) error {
	st := c.state(e.TicketID)
	if st.status != "" {
		return &repairRuleError{code: ruleRepairDuplicateReport, ticketID: e.TicketID, seq: e.Seq}
	}
	st.status = ticketOpen
	return nil
}

// applySubmit 判定并采纳提交履历：仅未关闭工单可提交非空维修结果，提交后工单
// 转为待验收，履历全库序号即该次提交身份。
func (c *repairCore) applySubmit(e Event) error {
	st := c.state(e.TicketID)
	if e.Content == "" {
		return &repairRuleError{code: ruleSubmitEmptyResult, ticketID: e.TicketID, seq: e.Seq}
	}
	if st.status != ticketOpen {
		return &repairRuleError{code: ruleSubmitNotOpen, ticketID: e.TicketID, seq: e.Seq, from: st.status}
	}
	st.status = ticketPending
	st.pendingSeq = e.Seq
	st.pendingResult = e.Content
	st.submittedEver = true
	return nil
}

// classifyTarget 核对验收目标序号是否恰为工单当前待验收提交，返回不匹配的
// 规则类别与上下文：未知序号、非提交序号、跨单提交、已处理或非当前提交。
func (c *repairCore) classifyTarget(e Event, st *ticketRepair) repairRuleCode {
	target, ok := c.eventBySeq[e.TargetSeq]
	if !ok {
		return ruleAcceptTargetUnknown
	}
	if target.Kind != eventSubmit {
		return ruleAcceptTargetNotSubmit
	}
	if target.TicketID != e.TicketID {
		return ruleAcceptTargetCrossTicket
	}
	if target.Seq != st.pendingSeq {
		return ruleAcceptTargetProcessed
	}
	return -1
}

// applyAccept 判定并采纳验收履历：仅待验收工单可验收，意见非空、决定合法，
// 目标序号须恰为当前待验收提交。通过进入待关闭中间态并锁定提交结果；退回
// 恢复未关闭并清除当前提交，可再次提交产生新身份。
func (c *repairCore) applyAccept(e Event) error {
	st := c.state(e.TicketID)
	if e.Decision != decisionApprove && e.Decision != decisionReject {
		return &repairRuleError{code: ruleAcceptBadDecision, ticketID: e.TicketID, seq: e.Seq}
	}
	if e.Content == "" {
		return &repairRuleError{code: ruleAcceptEmptyComment, ticketID: e.TicketID, seq: e.Seq}
	}
	if st.status != ticketPending {
		return &repairRuleError{code: ruleAcceptNotPending, ticketID: e.TicketID, seq: e.Seq, from: st.status}
	}
	if code := c.classifyTarget(e, st); code >= 0 {
		re := &repairRuleError{code: code, ticketID: e.TicketID, seq: e.Seq, target: e.TargetSeq}
		if ev, ok := c.eventBySeq[e.TargetSeq]; ok {
			re.kind = ev.Kind
			re.owner = ev.TicketID
		}
		return re
	}
	if e.Decision == decisionApprove {
		st.status = derivedApproved
		st.approvedResult = st.pendingResult
	} else {
		st.status = ticketOpen
	}
	st.pendingSeq = 0
	st.pendingResult = ""
	return nil
}

// applyClose 判定并采纳关闭履历：未关闭且从未提交的工单可直接关闭；待关闭
// 中间态只接受内容与验收提交结果一致的关闭；其他状态一律拒绝。
func (c *repairCore) applyClose(e Event) error {
	st := c.state(e.TicketID)
	if e.Content == "" {
		return &repairRuleError{code: ruleCloseEmptyResult, ticketID: e.TicketID, seq: e.Seq}
	}
	switch st.status {
	case ticketOpen:
		if st.submittedEver {
			return &repairRuleError{code: ruleCloseAfterSubmit, ticketID: e.TicketID, seq: e.Seq}
		}
	case derivedApproved:
		if e.Content != st.approvedResult {
			return &repairRuleError{code: ruleCloseApprovedMismatch, ticketID: e.TicketID, seq: e.Seq,
				contentA: e.Content, contentB: st.approvedResult}
		}
	case ticketClosed:
		return &repairRuleError{code: ruleCloseAlreadyClosed, ticketID: e.TicketID, seq: e.Seq}
	case ticketCancelled:
		return &repairRuleError{code: ruleCloseAlreadyCancelled, ticketID: e.TicketID, seq: e.Seq}
	case ticketPending:
		return &repairRuleError{code: ruleClosePending, ticketID: e.TicketID, seq: e.Seq}
	default:
		return &repairRuleError{code: ruleCloseNoReport, ticketID: e.TicketID, seq: e.Seq}
	}
	st.status = ticketClosed
	st.pendingSeq = 0
	st.pendingResult = ""
	st.approvedResult = ""
	return nil
}

// applyCancel 判定并采纳取消履历：未关闭与待验收工单均可取消，取消清除当前
// 待验收提交但保留全部提交历史；已终结或处于待关闭中间态时拒绝。
func (c *repairCore) applyCancel(e Event) error {
	st := c.state(e.TicketID)
	switch st.status {
	case ticketOpen, ticketPending:
	case ticketClosed:
		return &repairRuleError{code: ruleCancelAlreadyClosed, ticketID: e.TicketID, seq: e.Seq}
	case ticketCancelled:
		return &repairRuleError{code: ruleCancelAlreadyCancelled, ticketID: e.TicketID, seq: e.Seq}
	case derivedApproved:
		return &repairRuleError{code: ruleCancelApprovedPending, ticketID: e.TicketID, seq: e.Seq}
	default:
		return &repairRuleError{code: ruleCancelNoReport, ticketID: e.TicketID, seq: e.Seq}
	}
	st.status = ticketCancelled
	st.pendingSeq = 0
	st.pendingResult = ""
	st.approvedResult = ""
	return nil
}
