package main

import (
	"fmt"
	"math"
	"math/big"
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

// withdrawSeqs 返回每笔领用编号对应的领用履历全库序号。领用编号只用于定位
// 记录：各笔领用的先后由该笔唯一领用履历的序号决定，与记录数组位置、编号
// 大小及履历时间无关（有效台账允许数组乱序、序号间隔、时间不递增以及编号
// 大小与领用先后不一致）。
func (s *store) withdrawSeqs() map[string]int {
	seqs := make(map[string]int, len(s.data.Parts))
	for _, e := range s.data.Events {
		if e.Kind == eventPartWithdraw {
			seqs[e.WithdrawalID] = e.Seq
		}
	}
	return seqs
}

// partsOf 按领用先后返回工单的全部领用记录：顺序由每笔唯一领用履历的全库
// 序号决定，不使用数组位置或编号大小。
func (s *store) partsOf(ticketID string) []*PartWithdrawal {
	seqs := s.withdrawSeqs()
	out := make([]*PartWithdrawal, 0)
	for _, p := range s.data.Parts {
		if p.TicketID == ticketID {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return seqs[out[i].ID] < seqs[out[j].ID] })
	return out
}

// partNetRow 为按备件编号汇总的一行净量。Net 为精确十进制整数：同备件多笔
// 净量之和可能超出单笔数量范围，汇总不得回绕或截断，也不因此拒绝合法台账。
type partNetRow struct {
	PartID string
	Net    *big.Int
}

// partNetSummary 按备件编号字典序汇总工单各笔领用的净量；全部退回的备件
// 净量为零仍显示。
func (s *store) partNetSummary(ticketID string) []partNetRow {
	nets := map[string]*big.Int{}
	for _, p := range s.partsOf(ticketID) {
		acc := nets[p.PartID]
		if acc == nil {
			acc = new(big.Int)
			nets[p.PartID] = acc
		}
		acc.Add(acc, big.NewInt(int64(p.Quantity-p.Returned)))
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
	if t.Status == ticketPending {
		return nil, fmt.Errorf("%w: 工单 %s 处于待验收，不能领用备件", errConflict, ticketID)
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
	if t.Status == ticketPending {
		return nil, nil, fmt.Errorf("%w: 工单 %s 处于待验收，不能退回备件", errConflict, t.ID)
	}
	if reason == "" {
		return nil, nil, fmt.Errorf("%w: 退回理由不能为空", errConflict)
	}
	if quantity < 1 {
		return nil, nil, fmt.Errorf("%w: 退回数量须为正整数", errConflict)
	}
	// 剩余可退数量 = 原数量 - 累计退回（两者均在 [0, 原数量] 内，减法不会溢出）；
	// 退回数量超过剩余即拒绝。不做 Returned+quantity 的加法比较：大数量相加
	// 可能整数溢出回绕成负数，从而错误接受超额退回。
	if quantity > p.Quantity-p.Returned {
		return nil, nil, fmt.Errorf(
			"%w: 备件数量问题：领用记录 %s 原数量 %d，已退回 %d，再退 %d 将超过原数量",
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
