package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
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
//
// 提交、验收（含通过时紧随的关闭）、直接关闭与取消的业务判定、提交身份与工单
// 状态推导全部由提交验收链规则核心（submitcore.go）统一完成，日常操作、台账
// 加载、保存前校验、ticket 当前提交查询与 replay 回看共用；本文件只做记录级
// 前置检查（存在性、参数形式）并按操作场景包装核心的规则错误。
//
// 提交、验收各为一次原子保存：校验、履历容量或读写失败时保留原文件字节，
// 不留下部分状态或履历、不消耗序号，恢复后可重试。

// repairCore 用当前履历重放提交验收链规则核心，供日常操作做业务判定、供
// ticket 查询当前待验收提交。数据在加载与每次保存前均通过整库一致性检查，
// 重放必然成功；失败说明内存数据损坏。核心只推进自己的派生状态，不修改任何
// 业务记录。
func (s *store) repairCore() (*repairCore, error) {
	core := newRepairCore()
	sorted := make([]Event, len(s.data.Events))
	copy(sorted, s.data.Events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	for _, e := range sorted {
		if err := core.apply(e); err != nil {
			return nil, fmt.Errorf("数据内部错误：提交验收链履历重放失败: %w", err)
		}
	}
	return core, nil
}

// repairOpError 把核心返回的规则冲突包装为日常操作场景的错误（errConflict
// 类别，面向命令行的说明）。合法操作不可能触发的类别视为数据内部错误。
func repairOpError(err error) error {
	var re *repairRuleError
	if !errors.As(err, &re) {
		return err
	}
	switch re.code {
	case chainSubmitNotOpen:
		switch re.state {
		case ticketPending:
			return fmt.Errorf("%w: 工单 %s 已有待验收提交，不能重复提交", errConflict, re.ticketID)
		case ticketClosed:
			return fmt.Errorf("%w: 工单 %s 已关闭，不能提交", errConflict, re.ticketID)
		case ticketCancelled:
			return fmt.Errorf("%w: 工单 %s 已取消，不能提交", errConflict, re.ticketID)
		}
	case chainAcceptNotPending:
		switch re.state {
		case ticketClosed:
			return fmt.Errorf("%w: 工单 %s 已关闭，不能验收", errConflict, re.ticketID)
		case ticketCancelled:
			return fmt.Errorf("%w: 工单 %s 已取消，不能验收", errConflict, re.ticketID)
		case ticketOpen:
			return fmt.Errorf("%w: 工单 %s 当前没有待验收提交", errConflict, re.ticketID)
		}
	case chainTargetUnknown:
		return fmt.Errorf("%w: 未知提交序号 %d", errConflict, re.target)
	case chainTargetNotSubmit:
		return fmt.Errorf("%w: 序号 %d 不是维修提交序号（该序号为%s履历）",
			errConflict, re.target, re.targetKind)
	case chainTargetOtherTicket:
		return fmt.Errorf("%w: 提交序号 %d 属于工单 %s，不能用于工单 %s 的验收",
			errConflict, re.target, re.otherTicket, re.ticketID)
	case chainTargetProcessed:
		return fmt.Errorf("%w: 提交序号 %d 已处理，不是工单 %s 当前待验收提交",
			errConflict, re.target, re.ticketID)
	case chainCloseAfterSubmit:
		return fmt.Errorf("%w: 工单 %s 已提交过维修，不能直接关闭，须重新提交并验收通过或取消",
			errConflict, re.ticketID)
	case chainCloseBadState:
		switch re.state {
		case ticketPending:
			return fmt.Errorf("%w: 工单 %s 处于待验收，须验收通过或取消终结，不能直接关闭",
				errConflict, re.ticketID)
		case ticketCancelled:
			return fmt.Errorf("%w: 工单 %s 已取消，不能关闭", errConflict, re.ticketID)
		case ticketClosed:
			return fmt.Errorf("%w: 工单 %s 已关闭，不能重复关闭", errConflict, re.ticketID)
		}
	case chainCancelBadState:
		switch re.state {
		case ticketCancelled:
			return fmt.Errorf("%w: 工单 %s 已取消，不能再次取消", errConflict, re.ticketID)
		case ticketClosed:
			return fmt.Errorf("%w: 工单 %s 已关闭，不能取消", errConflict, re.ticketID)
		}
	}
	return fmt.Errorf("数据内部错误：%v", err)
}

// hasSubmission 报告工单是否曾提交过维修（含已退回的提交）。首次提交后工单
// 只能验收通过或取消终结，不能再直接关闭。由规则核心按履历链推导，不直接
// 扫描或修改任何记录。
func (s *store) hasSubmission(ticketID string) bool {
	core, err := s.repairCore()
	if err != nil {
		return false
	}
	return core.everSubmitted(ticketID)
}

// currentSubmission 返回工单当前待验收提交的履历序号与维修结果；无待验收提交
// 时 ok=false。由规则核心按履历链推导：最新一次提交若已有验收履历指向它，
// 则已处理（通过或退回），不再是当前待验收提交。只读，不写文件、不初始化目录。
func (s *store) currentSubmission(ticketID string) (seq int, result string, ok bool) {
	core, err := s.repairCore()
	if err != nil {
		return 0, "", false
	}
	return core.pending(ticketID)
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
// 返回其全库序号作为提交身份。业务判定由规则核心完成；失败路径不修改任何
// 业务数据，也不消耗履历序号。
func (s *store) submitRepair(ticketID, result string) (*Ticket, int, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, 0, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if result == "" {
		return nil, 0, fmt.Errorf("%w: 维修结果不能为空", errConflict)
	}
	core, err := s.repairCore()
	if err != nil {
		return nil, 0, err
	}
	// 候选履历不含序号：核心判定不依赖序号，真正的序号在判定通过后分配，
	// 失败路径不消耗序号。
	if err := core.apply(Event{TicketID: ticketID, Kind: eventSubmit, Content: result}); err != nil {
		return nil, 0, repairOpError(err)
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
// 资产恢复可用；退回时追加验收履历，工单恢复未关闭，资产仍为维修中。业务判定
// （含目标序号的未知、非提交、跨单、已处理或过时分类）由规则核心完成；失败
// 路径不修改任何业务数据，也不消耗履历序号。
func (s *store) acceptSubmission(ticketID string, seq int, decision, comment string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	core, err := s.repairCore()
	if err != nil {
		return nil, nil, err
	}
	curSeq, curResult, ok := core.pending(ticketID)
	if !ok {
		// 不在待验收状态：由核心按当时状态给出关闭/取消/无待验收提交等说明。
		return nil, nil, repairOpError(&repairRuleError{
			code: chainAcceptNotPending, ticketID: ticketID, state: core.state(ticketID),
		})
	}
	if seq != curSeq {
		// 目标分类（未知、非提交、跨单、已处理或过时）由共享核心完成。
		cerr := core.apply(Event{
			TicketID: ticketID, Kind: eventAccept,
			Decision: decision, TargetSeq: seq, Content: comment,
		})
		if cerr == nil {
			return nil, nil, fmt.Errorf("数据内部错误：工单 %s 的验收目标 %d 分类失败", ticketID, seq)
		}
		return nil, nil, repairOpError(cerr)
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
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
