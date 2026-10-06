package main

import (
	"fmt"
	"math"
	"sort"
)

// 维修工单备件领用与退回台账。
//
// 领用：仅未关闭工单可领用，输入工单编号、非空备件编号、正整数数量（工单用量）
// 与非空说明；每次成功生成同目录唯一且不复用的领用编号（P 加补零序号），并追加
// 一条领用履历。同备件多次领用各自独立，领用编号用于定位记录，不用报修请求标识
// 代替。
//
// 退回：输入领用编号、正整数数量与非空理由，作用于该笔领用所属的工单；允许多次
// 部分退回，累计不得超过该笔原数量，也不能冲减另一笔。成功时更新累计退回并追加
// 一条退回履历，净量 = 原数量 - 累计退回。
//
// 工单关闭或取消后保留领用记录与净量，不自动清零；终结后拒绝领用与退回，
// 旧单的记录不影响新工单。领用、退回各为一次原子保存：校验、数量或编号容量
// 不足、读写失败时保留原文件字节，不留下部分记录、不消耗编号，恢复后可重试。

// findPart 按领用编号查找领用记录，不存在时返回 nil。
func (s *store) findPart(id string) *PartWithdrawal {
	for _, p := range s.data.Parts {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// partsOf 按领用顺序返回工单的全部领用记录（追加顺序即领用顺序）。
func (s *store) partsOf(ticketID string) []*PartWithdrawal {
	out := make([]*PartWithdrawal, 0)
	for _, p := range s.data.Parts {
		if p.TicketID == ticketID {
			out = append(out, p)
		}
	}
	return out
}

// partNetRow 为按备件编号汇总的一行净量。
type partNetRow struct {
	PartID string
	Net    int
}

// partNetSummary 按备件编号字典序汇总工单各笔领用的净量；全部退回的备件
// 净量为零仍显示。
func (s *store) partNetSummary(ticketID string) []partNetRow {
	nets := map[string]int{}
	for _, p := range s.partsOf(ticketID) {
		nets[p.PartID] += p.Quantity - p.Returned
	}
	rows := make([]partNetRow, 0, len(nets))
	for partID, net := range nets {
		rows = append(rows, partNetRow{PartID: partID, Net: net})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PartID < rows[j].PartID })
	return rows
}

// appendPartEvent 以给定序号追加备件履历（领用或退回）。备件履历属于领用记录
// 所在的工单，from/to 与保养字段恒为空；content 为领用说明或退回理由。
func (s *store) appendPartEvent(seq int, p *PartWithdrawal, kind string, quantity int, content string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:          seq,
		AssetID:      p.AssetID,
		TicketID:     p.TicketID,
		Kind:         kind,
		Content:      content,
		PartID:       p.PartID,
		Quantity:     quantity,
		WithdrawalID: p.ID,
		Time:         s.now(),
	})
}

// withdrawPart 为未关闭工单领用备件：生成同目录唯一且不复用的领用编号，
// 追加一条领用履历。失败路径不修改任何业务数据，也不消耗领用编号。
func (s *store) withdrawPart(ticketID, partID string, quantity int, note string) (*PartWithdrawal, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketClosed {
		return nil, fmt.Errorf("%w: 工单 %s 已关闭，不能领用备件", errConflict, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, fmt.Errorf("%w: 工单 %s 已取消，不能领用备件", errConflict, ticketID)
	}
	if partID == "" {
		return nil, fmt.Errorf("%w: 备件编号不能为空", errConflict)
	}
	if note == "" {
		return nil, fmt.Errorf("%w: 领用说明不能为空", errConflict)
	}
	if quantity < 1 {
		return nil, fmt.Errorf("%w: 领用数量须为正整数", errConflict)
	}
	// 先确认领用编号与履历计数器都能推进，再修改任何业务数据：
	// 编号耗尽时拒绝领用，不消耗编号，也不产生履历。
	seq := s.data.NextPartSeq
	if seq < 1 || seq == math.MaxInt {
		return nil, fmt.Errorf("%w: 领用编号计数器已耗尽，无法分配新领用编号", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	p := &PartWithdrawal{
		ID:       fmt.Sprintf("P%04d", seq),
		TicketID: t.ID,
		AssetID:  t.AssetID,
		PartID:   partID,
		Quantity: quantity,
		Note:     note,
	}
	s.data.NextPartSeq = seq + 1
	s.data.Parts = append(s.data.Parts, p)
	s.appendPartEvent(eventSeq, p, eventPartWithdraw, quantity, note)
	return p, nil
}

// returnPart 按领用编号退回备件：作用于该笔领用所属的工单，允许多次部分退回，
// 累计不得超过该笔原数量，也不能冲减另一笔。成功时更新累计退回并追加一条退回
// 履历。失败路径不修改任何业务数据。
func (s *store) returnPart(withdrawalID string, quantity int, reason string) (*PartWithdrawal, *Ticket, error) {
	p := s.findPart(withdrawalID)
	if p == nil {
		return nil, nil, fmt.Errorf("%w: 未知领用编号 %q", errNotFound, withdrawalID)
	}
	t := s.findTicket(p.TicketID)
	if t == nil {
		return nil, nil, fmt.Errorf("数据内部错误：领用记录 %s 引用的工单不存在", withdrawalID)
	}
	if t.Status == ticketClosed {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能退回备件", errConflict, t.ID)
	}
	if t.Status == ticketCancelled {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已取消，不能退回备件", errConflict, t.ID)
	}
	if reason == "" {
		return nil, nil, fmt.Errorf("%w: 退回理由不能为空", errConflict)
	}
	if quantity < 1 {
		return nil, nil, fmt.Errorf("%w: 退回数量须为正整数", errConflict)
	}
	if p.Returned+quantity > p.Quantity {
		return nil, nil, fmt.Errorf(
			"%w: 领用记录 %s 原数量 %d，已退回 %d，再退 %d 将超过原数量",
			errConflict, p.ID, p.Quantity, p.Returned, quantity)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	p.Returned += quantity
	s.appendPartEvent(eventSeq, p, eventPartReturn, quantity, reason)
	return p, t, nil
}
