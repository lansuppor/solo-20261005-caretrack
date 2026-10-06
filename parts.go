package main

import (
	"fmt"
	"math"
	"sort"
)

// 维修工单备件领用与退回台账。
//
// 领用记录（PartIssue）是定位单位：每笔领用有同目录唯一且不复用的领用编号
// （P 加补零正整数序号），同一备件多次领用各自独立；退回按领用编号定位，
// 只作用于该笔及其所属工单，不能冲减另一笔。退回不另存记录，而是追加备件
// 退回履历，累计退回量由履历推出；净量 = 原数量 - 累计退回。工单关闭或取消
// 后领用记录与净量保留、不清零，但终结后拒绝再领用、再退回；旧单的领用
// 记录不影响同一资产之后的新工单。

// PartIssue 为一笔备件领用记录。数量为该工单对该备件的一次用量（正整数）。
type PartIssue struct {
	ID       string `json:"id"`
	TicketID string `json:"ticket_id"`
	AssetID  string `json:"asset_id"`
	PartNo   string `json:"part_no"`
	Qty      int    `json:"qty"`
	Note     string `json:"note"`
}

func (s *store) findPartIssue(id string) *PartIssue {
	for _, pi := range s.data.PartIssues {
		if pi.ID == id {
			return pi
		}
	}
	return nil
}

// returnedQty 返回一笔领用的累计退回数量（由备件退回履历求和）。
func (s *store) returnedQty(issueID string) int {
	total := 0
	for _, e := range s.data.Events {
		if e.Kind == eventPartReturn && e.IssueID == issueID {
			total += e.Qty
		}
	}
	return total
}

// appendPartEvent 以给定序号追加备件履历（领用或退回）。调用方须先通过
// nextEventSeq 取得序号，确保任何失败路径都不会在部分变更后才报错。
func (s *store) appendPartEvent(seq int, assetID, ticketID, issueID, kind, partNo string, qty int, content string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:      seq,
		AssetID:  assetID,
		TicketID: ticketID,
		Kind:     kind,
		Content:  content,
		IssueID:  issueID,
		PartNo:   partNo,
		Qty:      qty,
		Time:     s.now(),
	})
}

// issuePart 为未关闭工单领用备件：生成同目录唯一且不复用的领用编号，
// 追加一条领用履历。失败路径不修改任何业务数据，也不消耗领用编号。
func (s *store) issuePart(ticketID, partNo string, qty int, note string) (*PartIssue, error) {
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
	if partNo == "" {
		return nil, fmt.Errorf("%w: 备件编号不能为空", errConflict)
	}
	if qty < 1 {
		return nil, fmt.Errorf("%w: 领用数量须为正整数", errConflict)
	}
	if note == "" {
		return nil, fmt.Errorf("%w: 领用说明不能为空", errConflict)
	}
	// 先确认编号与履历计数器都能推进，再修改任何业务数据。
	seq := s.data.NextIssueSeq
	if seq < 1 || seq == math.MaxInt {
		return nil, fmt.Errorf("%w: 领用编号计数器已耗尽，无法分配新领用编号", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	pi := &PartIssue{
		ID:       fmt.Sprintf("P%04d", seq),
		TicketID: t.ID,
		AssetID:  t.AssetID,
		PartNo:   partNo,
		Qty:      qty,
		Note:     note,
	}
	s.data.NextIssueSeq = seq + 1
	s.data.PartIssues = append(s.data.PartIssues, pi)
	s.appendPartEvent(eventSeq, t.AssetID, t.ID, pi.ID, eventPartIssue, partNo, qty, note)
	return pi, nil
}

// returnPart 按领用编号退回备件：作用于该笔领用所属工单，允许多次部分退回，
// 累计不得超过该笔原数量。返回该笔记录与新的累计退回量。
// 失败路径不修改任何业务数据。
func (s *store) returnPart(issueID string, qty int, reason string) (*PartIssue, int, error) {
	pi := s.findPartIssue(issueID)
	if pi == nil {
		return nil, 0, fmt.Errorf("%w: 未知领用编号 %q", errNotFound, issueID)
	}
	t := s.findTicket(pi.TicketID)
	if t == nil {
		return nil, 0, fmt.Errorf("数据内部错误：领用记录 %s 引用的工单不存在", issueID)
	}
	if t.Status == ticketClosed {
		return nil, 0, fmt.Errorf("%w: 工单 %s 已关闭，不能退回备件", errConflict, t.ID)
	}
	if t.Status == ticketCancelled {
		return nil, 0, fmt.Errorf("%w: 工单 %s 已取消，不能退回备件", errConflict, t.ID)
	}
	if qty < 1 {
		return nil, 0, fmt.Errorf("%w: 退回数量须为正整数", errConflict)
	}
	if reason == "" {
		return nil, 0, fmt.Errorf("%w: 退回理由不能为空", errConflict)
	}
	returned := s.returnedQty(issueID)
	if qty > pi.Qty-returned {
		return nil, 0, fmt.Errorf(
			"%w: 领用编号 %s 原数量 %d，已退回 %d，再退 %d 将超过原数量",
			errConflict, issueID, pi.Qty, returned, qty)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, 0, err
	}
	s.appendPartEvent(eventSeq, pi.AssetID, pi.TicketID, pi.ID, eventPartReturn, pi.PartNo, qty, reason)
	return pi, returned + qty, nil
}

// partIssueRow 为 ticket 查询的一行领用记录：编号、备件、原数量、累计退回与净量。
type partIssueRow struct {
	ID       string
	PartNo   string
	Qty      int
	Returned int
	Net      int
}

// partIssuesOf 按领用顺序（领用编号序号升序）返回工单的全部领用记录行。
func (s *store) partIssuesOf(ticketID string) []partIssueRow {
	issues := make([]*PartIssue, 0)
	for _, pi := range s.data.PartIssues {
		if pi.TicketID == ticketID {
			issues = append(issues, pi)
		}
	}
	sort.SliceStable(issues, func(i, j int) bool {
		ni, _ := parseIssueSeq(issues[i].ID)
		nj, _ := parseIssueSeq(issues[j].ID)
		return ni < nj
	})
	rows := make([]partIssueRow, 0, len(issues))
	for _, pi := range issues {
		r := s.returnedQty(pi.ID)
		rows = append(rows, partIssueRow{
			ID: pi.ID, PartNo: pi.PartNo, Qty: pi.Qty, Returned: r, Net: pi.Qty - r,
		})
	}
	return rows
}

// partNetRow 为按备件编号汇总的一行净量。
type partNetRow struct {
	PartNo string
	Net    int
}

// summarizePartNet 按备件编号字典序汇总净量；净量为零的备件仍列出。
func summarizePartNet(rows []partIssueRow) []partNetRow {
	net := map[string]int{}
	for _, r := range rows {
		net[r.PartNo] += r.Net
	}
	parts := make([]string, 0, len(net))
	for p := range net {
		parts = append(parts, p)
	}
	sort.Strings(parts)
	out := make([]partNetRow, 0, len(parts))
	for _, p := range parts {
		out = append(out, partNetRow{PartNo: p, Net: net[p]})
	}
	return out
}
