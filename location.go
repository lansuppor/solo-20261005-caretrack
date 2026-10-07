package main

import (
	"fmt"
	"sort"
)

// 设备位置变更与工单报修地点追溯。
//
// relocate 输入资产编号、新位置与非空理由；可用、维修中、停用资产均可变更。
// 未知资产、空位置或理由、新位置与当前位置相同均拒绝。成功后更新当前位置，
// 追加一条含原位置、新位置、理由、时间及全库履历序号的资产级“位置变更”履历。
// 位置变更不改变资产状态、工单状态、负责人、请求绑定、保养周期、备件、附件
// 及停机统计。
//
// 每张工单保存“报修时位置”：取该工单报修履历序号当时的资产位置——即该资产
// 序号不大于报修序号的最后一条位置变更的新位置，没有位置变更履历则为位置
// 起点（资产保存位置的初始值）。不能用当前地点、履历时间或数组位置代替。
// 维修中搬移不改旧单的报修地点；搬移后的新报修采用新地点；旧请求重放仍返回
// 原工单，不改地点、不追加履历。
//
// 首次变更前的位置须在重载后仍可还原：没有位置履历的有效旧库以保存位置作为
// 起点，无需转换；查询不补写数据、不初始化目录。位置履历允许数组乱序、序号
// 间隔、时间不递增；加载与保存按履历序号核对位置链（归属资产存在、原位置
// 接续当时位置、新位置非空且与原位置不同、最终位置与资产保存值一致），并核对
// 每张工单保存的报修地点与报修履历序号当时的位置一致。位置矛盾使所有读写
// 命令退出 1 并说明类别，原文件不变，不自动修复。
//
// 变更与整批导入各为一次原子保存：校验、履历容量不足或读写失败不留部分位置
// 或履历、不消耗序号，原文件字节保持，恢复后可按原输入重试。

// appendLocationEvent 以给定序号追加位置变更履历。该履历为资产级履历，
// 不属于任何工单；from/to 为原位置与新位置，content 为变更理由。
func (s *store) appendLocationEvent(seq int, assetID, from, to, reason string) {
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

// relocateAsset 变更资产位置：可用、维修中、停用资产均可变更。未知资产、
// 空位置或理由、新位置与当前位置相同均拒绝。成功返回更新后的资产与原位置。
// 失败路径不修改任何业务数据，也不产生履历或消耗序号。
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
		return nil, "", fmt.Errorf("%w: 资产 %s 当前位置已是 %q，不能变更为相同位置",
			errConflict, assetID, newLocation)
	}
	// 先确认履历计数器能推进，再修改任何业务数据：容量耗尽时拒绝变更，
	// 不更新位置，也不产生履历。
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, "", err
	}
	from := a.Location
	a.Location = newLocation
	s.appendLocationEvent(eventSeq, assetID, from, newLocation, reason)
	return a, from, nil
}

// sortedLocationEvents 返回全部位置变更履历按全库序号升序排列的副本。
// 有效台账允许位置履历数组乱序、序号间隔、时间不递增，位置链一律以全库
// 序号为准，不使用数组位置或履历时间。
func sortedLocationEvents(events []Event) []Event {
	out := make([]Event, 0)
	for _, e := range events {
		if e.Kind == eventRelocate {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// locationAt 返回资产在给定全库履历序号当时的位置：取该资产序号不大于 seq
// 的最后一条位置变更履历的新位置；此前没有位置变更履历时返回起点位置。
// 调用方须保证起点非空、loc 已按序号升序排列且该资产的位置链接续。
func locationAt(start string, loc []Event, assetID string, seq int) string {
	locAt := start
	for _, e := range loc {
		if e.AssetID != assetID || e.Seq > seq {
			continue
		}
		locAt = e.To
	}
	return locAt
}

// reportEventSeq 返回一张工单唯一报修履历的全库序号。整库一致性校验已保证
// 每张工单恰有一条报修履历；调用前已完成校验时第二个返回值恒为 true。
func (s *store) reportEventSeq(ticketID string) (int, bool) {
	for _, e := range s.data.Events {
		if e.Kind == eventReport && e.TicketID == ticketID {
			return e.Seq, true
		}
	}
	return 0, false
}

// reportLocationOf 返回工单展示用的报修地点：新台账直接取保存值；没有位置
// 履历的有效旧库旧工单没有该字段时，按报修履历序号当时的位置（即位置起点）
// 推导展示，不补写任何数据。
func (s *store) reportLocationOf(t *Ticket) string {
	if t.ReportLocation != "" {
		return t.ReportLocation
	}
	loc := sortedLocationEvents(s.data.Events)
	byAsset := make([]Event, 0)
	for _, e := range loc {
		if e.AssetID == t.AssetID {
			byAsset = append(byAsset, e)
		}
	}
	start := ""
	if a := s.findAsset(t.AssetID); a != nil {
		start = a.Location
	}
	if len(byAsset) > 0 {
		start = byAsset[0].From
	}
	seq, _ := s.reportEventSeq(t.ID)
	return locationAt(start, byAsset, t.AssetID, seq)
}
