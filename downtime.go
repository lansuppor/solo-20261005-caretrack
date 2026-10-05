package main

import (
	"fmt"
	"sort"
	"time"
)

// 设备维修停机时长统计（只读）。
//
// 停机区间一律以履历时间为准，而不是工单记录里的重复时间字段：
//   - 起点为该工单报修履历的时间；
//   - 已关闭/已取消工单的终点为对应关闭/取消履历的时间（取消前的占用同样计入）；
//   - 未关闭工单暂算到输入窗口终点，不取运行时刻，也不改变其状态。
//
// 派工、转派不另起区间。统计只计算工单占用区间与窗口 [start,end) 的交集；
// 同一资产不同工单的交集先合并为并集再求时长，重叠部分不重复计算；不同资产
// 分别计算。统计成功或失败都不写文件、不初始化目录、不追加履历、不消耗编号，
// 也不改变报修请求绑定。

// timeWindow 是半开放时间区间 [start, end)。
type timeWindow struct {
	start time.Time
	end   time.Time
}

// downtimeResult 为单项资产的统计结果，秒数已向下取整。
type downtimeResult struct {
	AssetID string
	Name    string
	Seconds int64
}

// downtimeAnomaly 描述一张终结工单“结束履历时间早于报修履历时间”的异常。
// 该异常只用于统计：它使整次停机查询整体失败，既不使台账失去一致性（因此
// 不拒绝原有查询与工单操作），也不自动改写任何时间。
type downtimeAnomaly struct {
	AssetID  string
	TicketID string
	Start    time.Time
	End      time.Time
}

func (e *downtimeAnomaly) Error() string {
	return fmt.Sprintf("资产 %s 的工单 %s 终结履历时间（%s）早于报修履历时间（%s），统计失败",
		e.AssetID, e.TicketID, e.End.Format(time.RFC3339Nano), e.Start.Format(time.RFC3339Nano))
}

// downtimeForAssets 按给定资产顺序统计窗口 [start,end) 内各资产的停机秒数。
// 任一所选资产的任一张终结工单出现结束早于开始（即使该单完全在窗口外），
// 整次统计即失败，不返回任何部分结果。调用方负责资产编号的去重与排序。
func (s *store) downtimeForAssets(assetIDs []string, start, end time.Time) ([]downtimeResult, error) {
	results := make([]downtimeResult, 0, len(assetIDs))
	for _, id := range assetIDs {
		asset := s.findAsset(id)
		if asset == nil {
			return nil, fmt.Errorf("未知资产编号 %q", id)
		}
		intervals, err := s.occupancyIntervals(id, start, end)
		if err != nil {
			return nil, err
		}
		results = append(results, downtimeResult{
			AssetID: id,
			Name:    asset.Name,
			Seconds: unionDurationSeconds(intervals),
		})
	}
	return results, nil
}

// allAssetIDs 返回按编号字典序排序的全部资产编号。
func (s *store) allAssetIDs() []string {
	ids := make([]string, 0, len(s.data.Assets))
	for _, a := range s.data.Assets {
		ids = append(ids, a.ID)
	}
	sort.Strings(ids)
	return ids
}

// occupancyIntervals 按工单归属及履历序号配对，提取该资产每张工单与窗口
// [start,end) 的占用交集。允许编号与履历序号存在间隔、数组乱序、履历时间
// 不递增；窗口外与零长度交集不贡献区间。
func (s *store) occupancyIntervals(assetID string, start, end time.Time) ([]timeWindow, error) {
	intervals := make([]timeWindow, 0)
	for _, t := range s.data.Tickets {
		if t.AssetID != assetID {
			continue
		}
		// 只按“工单归属 + 履历”配对：扫描该工单的全部履历，取唯一的报修
		// 履历时间为起点、可能存在的一条关闭/取消履历时间为终点。不读取
		// 工单记录上的 created_at/closed_at/cancelled_at。
		var report, terminal *Event
		for i := range s.data.Events {
			e := &s.data.Events[i]
			if e.TicketID != t.ID || e.AssetID != assetID {
				continue
			}
			switch e.Kind {
			case eventReport:
				report = e
			case eventClose, eventCancel:
				terminal = e
			}
		}
		if report == nil {
			// 整库一致性检查已保证报修履历恰有一条；走到这里属于内部错误。
			return nil, fmt.Errorf("数据内部错误：工单 %s 缺少报修履历", t.ID)
		}
		occStart := report.Time
		var occEnd time.Time
		if terminal != nil {
			occEnd = terminal.Time
			// 结束早于开始即整体失败；结束等于开始合法（零长度，后续自然不计）。
			if occEnd.Before(occStart) {
				return nil, &downtimeAnomaly{
					AssetID:  assetID,
					TicketID: t.ID,
					Start:    occStart,
					End:      occEnd,
				}
			}
		} else {
			// 未关闭工单暂算到输入终点。在终点或之后才报修时，裁剪结果为空，记零。
			occEnd = end
		}
		if iv, ok := clipWindow(occStart, occEnd, start, end); ok {
			intervals = append(intervals, iv)
		}
	}
	return intervals, nil
}

// clipWindow 返回占用区间 [occStart,occEnd) 与窗口 [winStart,winEnd) 的交集。
// 窗口包含起点、不包含终点；零长度交集以 ok=false 返回。
func clipWindow(occStart, occEnd, winStart, winEnd time.Time) (timeWindow, bool) {
	lo := occStart
	if winStart.After(lo) {
		lo = winStart
	}
	hi := occEnd
	if winEnd.Before(hi) {
		hi = winEnd
	}
	if !hi.After(lo) {
		return timeWindow{}, false
	}
	return timeWindow{start: lo, end: hi}, true
}

// mergeWindows 把可能重叠或相邻的区间合并为并集，按起点排序。时间逆序导致的
// 跨工单重叠在此消除，而不是把各工单时长直接相加。
func mergeWindows(in []timeWindow) []timeWindow {
	sorted := make([]timeWindow, len(in))
	copy(sorted, in)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].start.Equal(sorted[j].start) {
			return sorted[i].start.Before(sorted[j].start)
		}
		return sorted[i].end.Before(sorted[j].end)
	})
	merged := make([]timeWindow, 0, len(sorted))
	for _, iv := range sorted {
		last := len(merged) - 1
		if last < 0 || iv.start.After(merged[last].end) {
			merged = append(merged, iv)
			continue
		}
		if iv.end.After(merged[last].end) {
			merged[last].end = iv.end
		}
	}
	return merged
}

// unionDurationSeconds 合并区间并集后，对每项资产的合计时长整体向下取整为
// 整秒：先合计全部区间的精确时长，最后才取整一次（而不是每段取整后相加，
// 否则两个 0.6 秒的分离区间会被错算成 0 秒而非合计 1.2 秒取整为 1 秒）。
//
// 时长以“整秒 + 秒内纳秒”两部分累加，避免大窗口下纳秒整数溢出
// （time.Duration 最多约 292 年，而 RFC3339 时刻跨度可能更大）。
func unionDurationSeconds(intervals []timeWindow) int64 {
	merged := mergeWindows(intervals)
	var totalSec int64
	var totalNano int64
	const nanoPerSec = 1_000_000_000
	for _, iv := range merged {
		// Unix() 是向下取整的秒刻度，Nanosecond() 为非负的秒内偏移，
		// 故 hi-lo 精确等于 (hi.Unix()-lo.Unix()) 秒
		// 加上 (hi.Nanosecond()-lo.Nanosecond()) 纳秒。
		dSec := iv.end.Unix() - iv.start.Unix()
		dNano := int64(iv.end.Nanosecond() - iv.start.Nanosecond())
		totalSec += dSec
		totalNano += dNano
		// 归一化，避免区间很多时秒内部分累积溢出。
		totalSec += totalNano / nanoPerSec
		totalNano %= nanoPerSec
	}
	// 合计后整体向下取整：秒内部分为负时再减一秒。
	if totalNano < 0 {
		totalSec--
	}
	return totalSec
}
