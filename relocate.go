package main

import (
	"fmt"
)

// 设备位置变更与工单报修地点追溯。
//
// 变更入口输入资产编号、新位置与非空理由；可用、维修中、停用资产均可变更。
// 未知资产、空位置或理由、新位置与当前位置相同均拒绝。成功后更新资产当前
// 位置，并追加一条资产级履历，含原位置、新位置、理由、时间与全库履历序号。
// 位置变更不改变资产状态、工单状态、负责人、请求绑定、保养周期、备件、附件
// 及停机统计：维修中搬移不改旧工单的报修地点，搬移后新报修采用新地点。
//
// 每张工单的报修地点在报修时按当时资产位置确定（Ticket.ReportLocation），
// 之后只能由履历链按全库序号追溯：不能用当前地点、履历时间或数组位置代替。
// 旧请求重放仍返回原工单，不改地点也不追加履历。
//
// 首次变更前的位置就是最早一条位置变更履历记录的原位置，重载后仍可还原；
// 没有位置履历的有效旧库以资产保存位置作为起点，无需转换，查询不补写数据。
// 加载与保存按履历序号重放核对位置链：归属资产存在、原位置接续当时位置、
// 新位置非空且与原位置不同，最终位置与资产保存值一致。数组乱序、序号间隔、
// 时间不递增仍合法；位置矛盾时所有读写命令退出 1 并指出“位置矛盾”类别，
// 原文件不变，不自动修复。
//
// 变更为一次原子保存：校验、履历容量不足或读写失败时不留部分位置或履历、
// 不消耗序号，原文件字节保持，恢复后可按原输入重试。

// appendRelocateEvent 以给定序号追加位置变更履历。该履历为资产级履历，
// 不属于任何工单；from/to 为原位置与新位置，content 为变更理由。
func (s *store) appendRelocateEvent(seq int, assetID, from, to, reason string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:     seq,
		AssetID: assetID,
		Kind:    eventRelocate,
		Content: reason,
		From:    from,
		To:      to,
		Time:    s.now(),
	})
}

// relocateAsset 变更资产当前位置：可用、维修中、停用资产均可变更。
// 未知资产、空位置或理由、新位置与当前位置相同均拒绝；位置变更不改变资产
// 状态、工单、负责人、请求绑定、保养周期、备件、附件及停机统计。
// 返回更新后的资产与原位置。失败路径不修改任何业务数据。
func (s *store) relocateAsset(assetID, newLocation, reason string) (*Asset, string, error) {
	a := s.findAsset(assetID)
	if a == nil {
		return nil, "", fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if newLocation == "" {
		return nil, "", fmt.Errorf("%w: 新位置不能为空", errConflict)
	}
	if reason == "" {
		return nil, "", fmt.Errorf("%w: 位置变更理由不能为空", errConflict)
	}
	if a.Location == newLocation {
		return nil, "", fmt.Errorf("%w: 资产 %s 当前位置已是 %s，不能变更为相同位置",
			errConflict, assetID, newLocation)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, "", err
	}
	from := a.Location
	a.Location = newLocation
	s.appendRelocateEvent(eventSeq, assetID, from, newLocation, reason)
	return a, from, nil
}

// locationAtSeq 按全库履历序号追溯资产在序号 seq 当时的位置：取序号不大于
// seq 的最后一条位置变更履历的新位置；此前（含没有任何位置变更履历时）为
// 位置链起点。位置链起点为最早一条位置变更履历的原位置；没有位置履历时为
// 资产保存位置。判定只按履历序号，不用履历时间或数组位置。
func (s *store) locationAtSeq(assetID string, seq int) string {
	current := s.locationOrigin(assetID)
	best := 0
	for _, e := range s.data.Events {
		if e.AssetID != assetID || e.Kind != eventRelocate {
			continue
		}
		if e.Seq <= seq && e.Seq > best {
			best = e.Seq
			current = e.To
		}
	}
	return current
}

// locationOrigin 返回资产位置链起点：有位置变更履历时为最早一条（按全库
// 序号）履历的原位置；没有位置履历时为资产保存位置。
func (s *store) locationOrigin(assetID string) string {
	origin := ""
	minSeq := 0
	found := false
	for _, e := range s.data.Events {
		if e.AssetID != assetID || e.Kind != eventRelocate {
			continue
		}
		if !found || e.Seq < minSeq {
			origin = e.From
			minSeq = e.Seq
			found = true
		}
	}
	if found {
		return origin
	}
	if a := s.findAsset(assetID); a != nil {
		return a.Location
	}
	return ""
}

// reportLocationOf 返回工单报修履历序号当时的资产位置。工单保存了报修地点时
// 以保存值为准（加载校验已保证它与履历链一致）；缺少该字段的有效旧库工单
// 按履历序号现场追溯，查询不补写数据。
func (s *store) reportLocationOf(t *Ticket) string {
	if t.ReportLocation != "" {
		return t.ReportLocation
	}
	reportSeq := 0
	for _, e := range s.data.Events {
		if e.TicketID == t.ID && e.Kind == eventReport && e.Seq > reportSeq {
			reportSeq = e.Seq
		}
	}
	return s.locationAtSeq(t.AssetID, reportSeq)
}
