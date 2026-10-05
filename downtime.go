package main

import (
	"fmt"
	"sort"
	"time"
)

// DowntimeAssetLine 是单项资产的停机统计结果。
type DowntimeAssetLine struct {
	AssetID string
	Name    string
	Seconds int64
}

// DowntimeResult 是一次停机统计的结果：每项资产一行（已按编号字典序排序），
// TotalSeconds 为各资产秒数之和。
type DowntimeResult struct {
	Lines        []DowntimeAssetLine
	TotalSeconds int64
}

// downtimeInterval 是工单对资产的一段占用区间，按半开区间 [Start, End) 处理。
type downtimeInterval struct {
	ticketID string
	start    time.Time
	end      time.Time
}

// downtimeStats 在半开窗口 [windowStart, windowEnd) 内统计停机时长。
// assetID 为空时统计全部资产，否则只统计指定资产（未知资产报错）。
//
// 停机区间严格按“工单归属 + 履历序号”配对得到：起点是该工单的报修履历时间，
// 终点是该工单关闭或取消履历时间；未关闭工单暂算到窗口终点。绝不使用数组位置
// 或工单记录里的重复时间字段（created_at/closed_at/cancelled_at）替代履历时间。
// 派工/转派不另起区间。
//
// 同一资产不同工单的区间可能因时间逆序而重叠，只统计各工单区间与窗口交集的
// 并集，绝不直接相加；不同资产分别计算。每项资产合计后向下取整为秒。
//
// 该方法为纯读取：不写文件、不初始化目录、不追加履历、不改变任何状态或绑定。
func (s *store) downtimeStats(assetID string, windowStart, windowEnd time.Time) (*DowntimeResult, error) {
	// 选出参与统计的资产；保持资产在台账中的出现顺序，最终输出前再排序。
	var assets []*Asset
	if assetID != "" {
		a := s.findAsset(assetID)
		if a == nil {
			return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
		}
		assets = []*Asset{a}
	} else {
		assets = make([]*Asset, len(s.data.Assets))
		copy(assets, s.data.Assets)
	}

	// 按工单归属收集该工单的报修/关闭/取消履历时间。配对只认工单编号与
	// 履历序号，不依赖数组位置，也不读工单记录中的重复时间字段。
	type ticketTimes struct {
		reportAt    time.Time
		closedAt    time.Time
		cancelledAt time.Time
		hasReport   bool
	}
	timesByTicket := map[string]*ticketTimes{}
	getTimes := func(ticketID string) *ticketTimes {
		t := timesByTicket[ticketID]
		if t == nil {
			t = &ticketTimes{}
			timesByTicket[ticketID] = t
		}
		return t
	}
	for i := range s.data.Events {
		e := &s.data.Events[i]
		if assetID != "" && e.AssetID != assetID {
			continue
		}
		tt := getTimes(e.TicketID)
		switch e.Kind {
		case eventReport:
			tt.reportAt = e.Time
			tt.hasReport = true
		case eventClose:
			tt.closedAt = e.Time
		case eventCancel:
			tt.cancelledAt = e.Time
		}
	}

	result := &DowntimeResult{Lines: []DowntimeAssetLine{}}
	for _, a := range assets {
		// 直接遍历工单记录来决定每张工单的终态（关闭/取消/未关闭），
		// 时间一律取自上面按履历配对的结果。
		var intervals []downtimeInterval
		for _, t := range s.data.Tickets {
			if t.AssetID != a.ID {
				continue
			}
			tt := timesByTicket[t.ID]
			// 整库一致性检查已保证每张工单恰有一条报修履历；此处防御性处理。
			if tt == nil || !tt.hasReport {
				return nil, fmt.Errorf("数据内部错误：工单 %s 缺少报修履历，无法计算停机时长", t.ID)
			}
			start := tt.reportAt
			var end time.Time
			switch t.Status {
			case ticketClosed:
				end = tt.closedAt
			case ticketCancelled:
				end = tt.cancelledAt
			case ticketOpen:
				// 未关闭工单暂算到窗口终点，不取运行时刻，不改变状态。
				end = windowEnd
			default:
				return nil, fmt.Errorf("数据内部错误：工单 %s 状态无效 %q", t.ID, t.Status)
			}
			// 仅统计规则：任一终结工单结束早于开始，即使该单完全落在窗口外，
			// 整次统计也失败；指出资产与工单，不输出部分结果、不把负时长归零。
			// 结束等于开始合法（零长度区间，不贡献时长）。
			if t.Status != ticketOpen && end.Before(start) {
				return nil, fmt.Errorf(
					"资产 %s 的工单 %s 终结履历时间（%s）早于报修履历时间（%s），停机统计失败",
					a.ID, t.ID, end.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
			}
			intervals = append(intervals, downtimeInterval{ticketID: t.ID, start: start, end: end})
		}
		seconds := unionIntersectionSeconds(intervals, windowStart, windowEnd)
		result.Lines = append(result.Lines, DowntimeAssetLine{
			AssetID: a.ID, Name: a.Name, Seconds: seconds,
		})
	}

	sort.Slice(result.Lines, func(i, j int) bool { return result.Lines[i].AssetID < result.Lines[j].AssetID })
	for _, l := range result.Lines {
		result.TotalSeconds += l.Seconds
	}
	return result, nil
}

// unionIntersectionSeconds 计算各占用区间与半开窗口 [ws, we) 交集的并集时长，
// 向下取整为整秒。窗口外及零长度区间不贡献；重叠或相邻（含时间逆序造成的
// 重叠）的区间先合并再求和，绝不直接相加。
func unionIntersectionSeconds(intervals []downtimeInterval, ws, we time.Time) int64 {
	type seg struct{ s, e time.Time }
	clipped := make([]seg, 0, len(intervals))
	for _, iv := range intervals {
		s, e := iv.start, iv.end
		if s.Before(ws) {
			s = ws
		}
		if e.After(we) {
			e = we
		}
		// 半开区间：交集起点须早于终点，相等（零长度）不贡献。
		if !s.Before(e) {
			continue
		}
		clipped = append(clipped, seg{s, e})
	}
	if len(clipped) == 0 {
		return 0
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].s.Before(clipped[j].s) })

	merged := []seg{clipped[0]}
	for _, c := range clipped[1:] {
		last := &merged[len(merged)-1]
		// 前一段为 [s,e)：下一段起点不晚于上一段终点即重叠或相邻，需合并，
		// 避免相邻零间隔被重复计入（此处不会产生重复，但合并更稳健）。
		if !c.s.After(last.e) {
			if c.e.After(last.e) {
				last.e = c.e
			}
			continue
		}
		merged = append(merged, c)
	}
	var total time.Duration
	for _, m := range merged {
		total += m.e.Sub(m.s)
	}
	return int64(total / time.Second) // Duration 整除即向零截断，非负时长即向下取整
}
