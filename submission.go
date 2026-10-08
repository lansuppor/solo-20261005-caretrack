package main

import (
	"fmt"
	"math"
	"time"
)

// 维修提交与验收（可选择启用的验收流程）。
//
// 提交：仅“未关闭”工单可提交非空维修结果，成功后工单转为“待验收”，追加一条
// 提交履历并以其全库序号作为该次提交的身份（提交序号）。待验收工单仍占用资产
// （资产保持“维修中”）：拒绝新报修、停用、派工、转派、备件领用退回与重复提交，
// 也不能直接 close；旧请求重放仍只读返回原单当前状态。
//
// 验收：输入工单编号、目标提交序号、决定（通过/退回）与非空意见，仅当前待验收
// 提交可处理；未知序号、非提交序号、跨单、已处理或非当前提交均拒绝。每次验收
// 追加一条含决定、意见与目标序号的验收履历。通过在追加验收履历的同时沿用一条
// 关闭履历（内容为该次提交的维修结果）终结工单，资产恢复可用；退回后工单恢复
// 未关闭，资产仍为维修中，可再次提交并产生新序号。旧提交与意见全部保留。
//
// 从未提交的工单仍可按原 close 直接关闭；首次提交后只能验收通过或 cancel 终结。
// 待验收工单可取消：保留提交履历且不填写维修结果；终结后拒绝提交与验收。
// 提交、验收各为一次原子保存：校验、履历容量或读写失败时保留原文件字节，
// 不留下部分状态或履历、不消耗序号，恢复后可重试。

// hasSubmission 报告工单是否曾提交过维修（含已退回的提交）。首次提交后工单
// 只能验收通过或取消终结，不能再直接关闭。
func (s *store) hasSubmission(ticketID string) bool {
	for _, e := range s.data.Events {
		if e.TicketID == ticketID && e.Kind == eventSubmit {
			return true
		}
	}
	return false
}

// currentSubmission 返回工单当前待验收提交的履历序号与维修结果；无待验收提交
// 时 ok=false。整库一致性已保证待验收工单恰有一个未被验收处理的提交：最新一次
// 提交若已有验收履历指向它，则已被处理（通过或退回）。
func (s *store) currentSubmission(ticketID string) (seq int, result string, ok bool) {
	processed := map[int]bool{}
	best := 0
	bestResult := ""
	for _, e := range s.data.Events {
		if e.TicketID != ticketID {
			continue
		}
		switch e.Kind {
		case eventAccept:
			processed[e.TargetSeq] = true
		case eventSubmit:
			if e.Seq > best {
				best = e.Seq
				bestResult = e.Content
			}
		}
	}
	if best == 0 || processed[best] {
		return 0, "", false
	}
	return best, bestResult, true
}

// appendSubmitEvent 以给定序号追加提交履历：内容即该次提交的维修结果，
// 履历的全库序号即提交身份。
func (s *store) appendSubmitEvent(seq int, t *Ticket, result string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:      seq,
		AssetID:  t.AssetID,
		TicketID: t.ID,
		Kind:     eventSubmit,
		Content:  result,
		Time:     s.now(),
	})
}

// appendAcceptEvent 以给定序号追加验收履历：内容为验收意见，Decision 为决定
// （通过/退回），TargetSeq 为目标提交的全库履历序号。
func (s *store) appendAcceptEvent(seq int, t *Ticket, decision, comment string, targetSeq int) {
	s.data.Events = append(s.data.Events, Event{
		Seq:       seq,
		AssetID:   t.AssetID,
		TicketID:  t.ID,
		Kind:      eventAccept,
		Content:   comment,
		Decision:  decision,
		TargetSeq: targetSeq,
		Time:      s.now(),
	})
}

// submitRepair 提交未关闭工单的维修结果：工单转为待验收，追加一条提交履历并
// 返回其全库序号作为提交身份。失败路径不修改任何业务数据，也不消耗履历序号。
func (s *store) submitRepair(ticketID, result string) (*Ticket, int, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, 0, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	switch t.Status {
	case ticketPending:
		return nil, 0, fmt.Errorf("%w: 工单 %s 已有待验收提交，不能重复提交", errConflict, ticketID)
	case ticketClosed:
		return nil, 0, fmt.Errorf("%w: 工单 %s 已关闭，不能提交", errConflict, ticketID)
	case ticketCancelled:
		return nil, 0, fmt.Errorf("%w: 工单 %s 已取消，不能提交", errConflict, ticketID)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, 0, err
	}
	t.Status = ticketPending
	s.appendSubmitEvent(eventSeq, t, result)
	return t, eventSeq, nil
}

// acceptSubmission 验收工单当前待验收提交：seq 须等于当前待验收提交的履历序号。
// 通过时追加验收履历并沿用一条关闭履历（内容为该次提交的维修结果）终结工单，
// 资产恢复可用；退回时追加验收履历，工单恢复未关闭，资产仍为维修中。
// 失败路径不修改任何业务数据，也不消耗履历序号。
func (s *store) acceptSubmission(ticketID string, seq int, decision, comment string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	switch t.Status {
	case ticketClosed:
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能验收", errConflict, ticketID)
	case ticketCancelled:
		return nil, nil, fmt.Errorf("%w: 工单 %s 已取消，不能验收", errConflict, ticketID)
	case ticketOpen:
		return nil, nil, fmt.Errorf("%w: 工单 %s 当前没有待验收提交", errConflict, ticketID)
	}
	curSeq, curResult, ok := s.currentSubmission(ticketID)
	if !ok {
		return nil, nil, fmt.Errorf("数据内部错误：待验收工单 %s 缺少当前提交履历", ticketID)
	}
	if seq != curSeq {
		return nil, nil, s.submissionSeqError(ticketID, seq)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	if decision == decisionApprove {
		// 通过需同时追加验收与关闭两条履历：先确认序号空间足够，再修改数据。
		if eventSeq == math.MaxInt {
			return nil, nil, fmt.Errorf("%w: 履历序号计数器空间不足，无法同时追加验收与关闭履历", errConflict)
		}
		s.appendAcceptEvent(eventSeq, t, decision, comment, seq)
		s.appendEvent(eventSeq+1, t.AssetID, t.ID, eventClose, curResult, "", "")
		t.Status = ticketClosed
		t.Result = curResult
		t.ClosedAt = s.now().Format(time.RFC3339)
		asset.Status = statusAvailable
		return t, asset, nil
	}
	s.appendAcceptEvent(eventSeq, t, decision, comment, seq)
	t.Status = ticketOpen
	return t, asset, nil
}

// submissionSeqError 为验收目标序号不匹配当前待验收提交时生成指明原因的错误：
// 未知序号、非提交序号、跨单提交、已处理或非当前提交分别说明。
func (s *store) submissionSeqError(ticketID string, seq int) error {
	for _, e := range s.data.Events {
		if e.Seq != seq {
			continue
		}
		if e.Kind != eventSubmit {
			return fmt.Errorf("%w: 序号 %d 不是维修提交序号（该序号为%s履历）", errConflict, seq, e.Kind)
		}
		if e.TicketID != ticketID {
			return fmt.Errorf("%w: 提交序号 %d 属于工单 %s，不能用于工单 %s 的验收",
				errConflict, seq, e.TicketID, ticketID)
		}
		return fmt.Errorf("%w: 提交序号 %d 已处理，不是工单 %s 当前待验收提交",
			errConflict, seq, ticketID)
	}
	return fmt.Errorf("%w: 未知提交序号 %d", errConflict, seq)
}
