package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// 维修提交验收流程（可选择启用：从未提交的工单仍可按原 close 直接关闭）。
//
// 提交：仅“未关闭”工单可提交，输入工单编号与非空维修结果；成功后工单变为
// “待验收”，追加一条提交履历，其全库履历序号即提交身份。待验收仍占用资产
// （资产保持“维修中”）：拒绝新报修、停用与重复提交；待验收也拒绝派工、转派
// 及备件领用、退回，退回后恢复。
//
// 验收：输入工单编号、目标提交序号、通过或退回决定与非空意见；仅当前待验收
// 提交可处理——未知工单或序号、非提交序号、跨单引用、已处理或非当前提交均
// 拒绝。每次验收追加一条含决定、意见与目标序号的验收履历：
//   - 通过：沿用一条关闭履历终结工单（内容为被通过提交的维修结果），工单
//     维修结果取该次提交内容，资产恢复“可用”；
//   - 退回：工单恢复“未关闭”，资产仍为“维修中”，可再次提交并产生新序号；
//     旧提交与意见全部保留。
//
// 首次提交后工单只能验收通过或取消终结，退回后也不能绕过验收直接关闭；
// 待验收可取消（保留提交，不填写维修结果）；终结后拒绝提交和验收。
// 提交、验收各为一次原子保存：校验、履历容量或读写失败时保留原文件字节，
// 不留下部分状态或履历、不消耗编号，恢复后可重试。

// hasSubmissions 报告工单是否已有提交履历（含已退回、已取消前的旧提交）。
func (s *store) hasSubmissions(ticketID string) bool {
	for _, e := range s.data.Events {
		if e.TicketID == ticketID && e.Kind == eventSubmit {
			return true
		}
	}
	return false
}

// pendingSubmissionOf 返回工单当前待验收的提交履历；没有时返回 nil。
// 只按全库履历序号重放该工单的提交与验收履历：提交使最近一次提交成为待验收，
// 验收（通过或退回）处理掉当前待验收提交。整库一致性已保证链的合法性。
func (s *store) pendingSubmissionOf(ticketID string) *Event {
	idxs := make([]int, 0)
	for i := range s.data.Events {
		if e := &s.data.Events[i]; e.TicketID == ticketID && (e.Kind == eventSubmit || e.Kind == eventReview) {
			idxs = append(idxs, i)
		}
	}
	sort.SliceStable(idxs, func(a, b int) bool { return s.data.Events[idxs[a]].Seq < s.data.Events[idxs[b]].Seq })
	pending := -1
	for _, i := range idxs {
		if s.data.Events[i].Kind == eventSubmit {
			pending = i
		} else {
			pending = -1
		}
	}
	if pending < 0 {
		return nil
	}
	return &s.data.Events[pending]
}

// appendReviewEvent 以给定序号追加验收履历。验收履历属于被验收工单，from/to、
// 保养、备件与附件字段恒为空；Content 为验收意见，Decision 为通过或退回，
// TargetSeq 为目标提交履历的全库序号。
func (s *store) appendReviewEvent(seq int, t *Ticket, targetSeq int, decision, comment string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:       seq,
		AssetID:   t.AssetID,
		TicketID:  t.ID,
		Kind:      eventReview,
		Content:   comment,
		Decision:  decision,
		TargetSeq: targetSeq,
		Time:      s.now(),
	})
}

// submitRepair 提交维修结果进入验收：仅未关闭工单可提交，维修结果非空。
// 成功后工单变为“待验收”，追加一条提交履历并返回其全库序号（提交身份）。
// 失败路径不修改任何业务数据，也不消耗履历序号。
func (s *store) submitRepair(ticketID, result string) (*Ticket, int, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, 0, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketClosed {
		return nil, 0, fmt.Errorf("%w: 工单 %s 已关闭，不能提交", errConflict, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, 0, fmt.Errorf("%w: 工单 %s 已取消，不能提交", errConflict, ticketID)
	}
	if t.Status == ticketPending {
		return nil, 0, fmt.Errorf("%w: 工单 %s 已有待验收提交，不能重复提交", errConflict, ticketID)
	}
	if result == "" {
		return nil, 0, fmt.Errorf("%w: 维修结果不能为空", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, 0, err
	}
	t.Status = ticketPending
	s.appendEvent(eventSeq, t.AssetID, t.ID, eventSubmit, result, "", "")
	return t, eventSeq, nil
}

// reviewSubmission 验收当前待验收提交：目标须为本工单当前待验收的提交履历
// （未知序号、非提交序号、跨单引用、已处理或非当前提交均拒绝），意见非空。
// 通过时沿用一条关闭履历终结工单（维修结果取该次提交内容），资产恢复“可用”；
// 退回时工单恢复“未关闭”，资产仍为“维修中”，可再次提交并产生新序号。
// 失败路径不修改任何业务数据，也不消耗履历序号。
func (s *store) reviewSubmission(ticketID string, targetSeq int, decision, comment string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketClosed {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能验收", errConflict, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已取消，不能验收", errConflict, ticketID)
	}
	if t.Status != ticketPending {
		return nil, nil, fmt.Errorf("%w: 工单 %s 当前没有待验收提交", errConflict, ticketID)
	}
	if comment == "" {
		return nil, nil, fmt.Errorf("%w: 验收意见不能为空", errConflict)
	}
	var target *Event
	for i := range s.data.Events {
		if s.data.Events[i].Seq == targetSeq {
			target = &s.data.Events[i]
			break
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("%w: 未知提交序号 %d", errNotFound, targetSeq)
	}
	if target.Kind != eventSubmit {
		return nil, nil, fmt.Errorf("%w: 履历序号 %d 不是提交履历，不能作为验收目标", errConflict, targetSeq)
	}
	if target.TicketID != ticketID {
		return nil, nil, fmt.Errorf("%w: 提交序号 %d 属于工单 %s，不能跨单验收工单 %s",
			errConflict, targetSeq, target.TicketID, ticketID)
	}
	pending := s.pendingSubmissionOf(ticketID)
	if pending == nil || pending.Seq != targetSeq {
		return nil, nil, fmt.Errorf("%w: 提交序号 %d 不是工单 %s 当前待验收提交（已处理或已被新提交取代）",
			errConflict, targetSeq, ticketID)
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	// 先确认履历计数器能推进（通过需追加验收与关闭两条履历），再修改任何业务
	// 数据：容量耗尽时拒绝验收，不消耗序号，也不产生履历。
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	if decision == decisionApprove && eventSeq == math.MaxInt {
		return nil, nil, fmt.Errorf("%w: 履历序号计数器已耗尽，无法追加验收与关闭履历", errConflict)
	}
	s.appendReviewEvent(eventSeq, t, targetSeq, decision, comment)
	if decision == decisionApprove {
		closeSeq, err := s.nextEventSeq()
		if err != nil {
			return nil, nil, err
		}
		t.Status = ticketClosed
		t.Result = target.Content
		t.ClosedAt = s.now().Format(time.RFC3339)
		asset.Status = statusAvailable
		s.appendEvent(closeSeq, t.AssetID, t.ID, eventClose, target.Content, "", "")
	} else {
		t.Status = ticketOpen
	}
	return t, asset, nil
}
