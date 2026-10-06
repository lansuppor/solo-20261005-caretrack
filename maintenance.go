package main

import (
	"fmt"
	"sort"
	"time"
)

// 周期保养计划：plan 建立计划，complete 登记一次实际保养，due 按日期查询。
//
// 日期一律为 0001 至 9999 年的有效公历 YYYY-MM-DD，按日历日运算（使用
// proleptic Gregorian 的年内日序数），不涉及时区与运行时刻。计划不可覆盖：
// 每项资产最多一个。完成登记只接受当前下一到期日的周期，完成日不早于该
// 到期日；下一到期日取“首次到期日 + k×间隔”中严格晚于完成日的最早日期
// ——延期跨过的周期不生成完成记录，也不能改用完成日加间隔。

const (
	eventPlanCreate = "保养计划"
	eventMaintDone  = "保养完成"

	minCalendarYear = 1
	maxCalendarYear = 9999
	dateFormat      = "2006-01-02"
)

// maxDateOrdinal 为 9999-12-31 的公历年内日序数（0001-01-01 为 0）。
var maxDateOrdinal = daysFromCivil(maxCalendarYear, 12, 31)

// Plan 为一项资产的不可覆盖周期保养计划；首次到期日与间隔天数确定整条
// 周期链，当前下一到期日不另行存储，而由建立与完成履历重放推出。
type Plan struct {
	AssetID      string `json:"asset_id"`
	Content      string `json:"content"`
	FirstDue     string `json:"first_due"`
	IntervalDays int    `json:"interval_days"`
}

// dueRow 为到期查询的一行结果。
type dueRow struct {
	AssetID string
	Name    string
	Content string
	Due     string
}

// parseCalendarDate 严格解析 YYYY-MM-DD：必须是 0001..9999 年的有效公历
// 日期且为规范写法（月、日补零），与运行时刻和时区无关。
func parseCalendarDate(s string) (time.Time, error) {
	t, err := time.Parse(dateFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期 %q 不是有效的公历 YYYY-MM-DD: %w", s, err)
	}
	if t.Year() < minCalendarYear || t.Year() > maxCalendarYear || t.Format(dateFormat) != s {
		return time.Time{}, fmt.Errorf("日期 %q 不是 %04d..%04d 年间的有效公历 YYYY-MM-DD 日期",
			s, minCalendarYear, maxCalendarYear)
	}
	return t, nil
}

// daysFromCivil 返回 proleptic Gregorian 日历日距 1970-01-01 的天数
// （Howard Hinnant 的公历序数算法），仅按年月日做整数运算。
func daysFromCivil(y, m, d int) int64 {
	y2 := int64(y)
	if m <= 2 {
		y2--
	}
	var era int64
	if y2 >= 0 {
		era = y2 / 400
	} else {
		era = (y2 - 399) / 400
	}
	yoe := y2 - era*400 // 纪元内年份 [0, 399]
	doy := int64((153*(monthOrdToMarch(m))+2)/5 + d - 1)
	doe := yoe*365 + yoe/4 - yoe/100 + doy // 纪元内天数 [0, 146096]
	return era*146097 + doe - 719468
}

// civilFromDays 是 daysFromCivil 的逆运算。
func civilFromDays(z int64) (y, m, d int) {
	z += 719468
	var era int64
	if z >= 0 {
		era = z / 146097
	} else {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097 // 纪元内天数 [0, 146096]
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y64 := yoe + era*400
	mp := (5*(doe-(365*yoe+yoe/4-yoe/100)) + 2) / 153
	d = int(doe-(365*yoe+yoe/4-yoe/100)-(153*mp+2)/5) + 1
	if mp < 10 {
		m = int(mp) + 3
	} else {
		m = int(mp) - 9
	}
	if m <= 2 {
		y64++
	}
	return int(y64), m, d
}

// monthOrdToMarch 把 1..12 月换算成以 3 月为岁首的月序 0..11。
func monthOrdToMarch(m int) int {
	if m > 2 {
		return m - 3
	}
	return m + 9
}

// dateOrdinal 解析日期并返回公历日序数；调用方也可传入已校验的日期。
func dateOrdinal(s string) (int64, error) {
	t, err := parseCalendarDate(s)
	if err != nil {
		return 0, err
	}
	return daysFromCivil(t.Year(), int(t.Month()), t.Day()), nil
}

// formatOrdinal 把公历日序数格式化为 YYYY-MM-DD。
func formatOrdinal(n int64) string {
	y, m, d := civilFromDays(n)
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

// nextDueOrdinal 返回 firstOrdinal + k×intervalDays（k ≥ 0 的整数）中
// 严格晚于完成日 doneOrdinal 的最早日期序数；该日期超出 9999-12-31 时
// ok=false。逐日按日历日累加，不做“完成日加间隔”。
func nextDueOrdinal(firstOrdinal, doneOrdinal int64, intervalDays int) (int64, bool) {
	cand := firstOrdinal
	for cand <= doneOrdinal {
		step := int64(intervalDays)
		if cand > maxDateOrdinal-step {
			return 0, false
		}
		cand += step
	}
	return cand, true
}

// findPlan 返回资产的计划，没有时为 nil。
func (s *store) findPlan(assetID string) *Plan {
	for _, p := range s.data.Plans {
		if p.AssetID == assetID {
			return p
		}
	}
	return nil
}

// planDoneEvents 返回资产按履历序号排列的保养完成履历。
func (s *store) planDoneEvents(assetID string) []Event {
	out := make([]Event, 0)
	for _, e := range s.data.Events {
		if e.AssetID == assetID && e.Kind == eventMaintDone {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// replayPlan 按完成履历重放计划链，返回当前（下一）到期日序数。
// 调用前履历已经过整库一致性校验；此处的矛盾检查属于防御性断言。
func replayPlan(p *Plan, doneEvents []Event) (int64, error) {
	current, err := dateOrdinal(p.FirstDue)
	if err != nil {
		return 0, err
	}
	for _, e := range doneEvents {
		periodOrd, err := dateOrdinal(e.Due)
		if err != nil {
			return 0, err
		}
		doneOrd, err := dateOrdinal(e.Done)
		if err != nil {
			return 0, err
		}
		if periodOrd != current {
			return 0, fmt.Errorf("计划完成链与周期到期日不接续")
		}
		if doneOrd < periodOrd {
			return 0, fmt.Errorf("实际完成日早于周期到期日")
		}
		next, ok := nextDueOrdinal(current, doneOrd, p.IntervalDays)
		if !ok {
			return 0, fmt.Errorf("下一到期日超出 %04d-12-31", maxCalendarYear)
		}
		current = next
	}
	return current, nil
}

// planNextDue 返回计划当前下一到期日字符串；履历链矛盾或日期超界时返回错误。
func (s *store) planNextDue(p *Plan) (string, error) {
	ord, err := replayPlan(p, s.planDoneEvents(p.AssetID))
	if err != nil {
		return "", err
	}
	return formatOrdinal(ord), nil
}

// createPlan 为资产建立不可覆盖的周期保养计划。所有失败路径都不修改数据。
func (s *store) createPlan(assetID, content, firstDue string, intervalDays int) (*Plan, error) {
	if s.findAsset(assetID) == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if s.findPlan(assetID) != nil {
		return nil, fmt.Errorf("%w: 资产 %s 已存在保养计划，计划不可覆盖", errConflict, assetID)
	}
	if _, err := dateOrdinal(firstDue); err != nil {
		return nil, err
	}
	if intervalDays < 1 {
		return nil, fmt.Errorf("间隔天数必须为正整数，得到 %d", intervalDays)
	}
	// 先确认履历序号可分配，再修改任何业务数据。
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	p := &Plan{
		AssetID:      assetID,
		Content:      content,
		FirstDue:     firstDue,
		IntervalDays: intervalDays,
	}
	s.data.Plans = append(s.data.Plans, p)
	s.appendMaintenanceEvent(eventSeq, assetID, eventPlanCreate, content, firstDue, "")
	return p, nil
}

// completeMaintenance 登记一次实际保养。periodDue 必须等于当前下一到期日，
// doneDate 不得早于 periodDue；不满足、无计划或下一到期日超出日期范围时
// 整次拒绝，不修改任何数据，也不把旧周期或延期周期转为完成。
func (s *store) completeMaintenance(assetID, periodDue, doneDate, result string) (*Plan, string, string, error) {
	p := s.findPlan(assetID)
	if p == nil {
		if s.findAsset(assetID) == nil {
			return nil, "", "", fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
		}
		return nil, "", "", fmt.Errorf("%w: 资产 %s 尚未安排保养计划", errConflict, assetID)
	}
	periodOrd, err := dateOrdinal(periodDue)
	if err != nil {
		return nil, "", "", err
	}
	doneOrd, err := dateOrdinal(doneDate)
	if err != nil {
		return nil, "", "", err
	}
	doneEvents := s.planDoneEvents(assetID)
	currentOrd, err := replayPlan(p, doneEvents)
	if err != nil {
		return nil, "", "", err
	}
	if periodOrd != currentOrd {
		return nil, "", "", fmt.Errorf(
			"%w: 所填周期到期日 %s 不是资产 %s 的当前下一到期日 %s：旧周期不能重复登记，也不能提前完成新周期",
			errConflict, periodDue, assetID, formatOrdinal(currentOrd))
	}
	if doneOrd < periodOrd {
		return nil, "", "", fmt.Errorf("%w: 实际完成日 %s 不得早于所完成周期的到期日 %s",
			errConflict, doneDate, periodDue)
	}
	nextOrd, ok := nextDueOrdinal(currentOrd, doneOrd, p.IntervalDays)
	if !ok {
		return nil, "", "", fmt.Errorf(
			"%w: 完成后无法在 %04d-12-31 之前得到下一到期日，整次登记拒绝",
			errConflict, maxCalendarYear)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, "", "", err
	}
	s.appendMaintenanceEvent(eventSeq, assetID, eventMaintDone, result, periodDue, doneDate)
	return p, periodDue, formatOrdinal(nextOrd), nil
}

// duePlans 为只读到期查询：返回下一到期日不晚于 asOf 的计划，先按到期日、
// 再按资产编号排序。不写文件、不初始化目录、不推进任何计划。
func (s *store) duePlans(asOf string) ([]dueRow, error) {
	asOfOrd, err := dateOrdinal(asOf)
	if err != nil {
		return nil, err
	}
	rows := make([]dueRow, 0)
	for _, p := range s.data.Plans {
		nextOrd, err := replayPlan(p, s.planDoneEvents(p.AssetID))
		if err != nil {
			return nil, err
		}
		if nextOrd > asOfOrd {
			continue
		}
		asset := s.findAsset(p.AssetID)
		if asset == nil {
			return nil, fmt.Errorf("数据内部错误：计划引用的资产 %s 不存在", p.AssetID)
		}
		rows = append(rows, dueRow{
			AssetID: p.AssetID,
			Name:    asset.Name,
			Content: p.Content,
			Due:     formatOrdinal(nextOrd),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Due != rows[j].Due {
			return rows[i].Due < rows[j].Due
		}
		return rows[i].AssetID < rows[j].AssetID
	})
	return rows, nil
}
