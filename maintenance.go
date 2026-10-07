package main

import (
	"fmt"
	"sort"
)

// 周期保养计划与完成登记。
//
// 日期一律为 0001 至 9999 年的公历日历日（YYYY-MM-DD），按日历日运算：
// 不涉及时区与运行时刻，内部以“距 0001-01-01 的天数”做整数运算，避免
// time.Time 的时区、跨度（time.Duration 约 292 年上限）与年份范围问题。
//
// 每个计划恰有一条建立履历（含初始计划：内容、首次到期日、间隔天数），
// 每次完成登记追加一条完成履历（含周期到期日、实际完成日、结果），每次撤销
// 误登记追加一条撤销履历（含目标完成履历序号、理由），每次方案调整追加一条
// 调整履历（含前后方案、原下一到期日与理由）。建立与调整履历的全库序号把
// 保养历史划分为方案段：完成与撤销只作用于当时所在段，下一到期日按本段
// 首次日加整数倍间隔推进；计划保存的是当前段方案与由履历链推出的下一到期日，
// 加载与保存时核对二者一致。
// 保养不创建或终结工单、不消耗工单编号，不改变资产状态、请求绑定或停机统计。

// Plan 为资产的周期保养计划；每项资产最多一个，不可覆盖，但可经调整履历
// 更换内容、周期起点与间隔。Content/FirstDue/IntervalDays 为当前方案段的
// 方案，NextDue 为由履历链推出的下一到期日，随每次完成登记推进、随撤销
// 回退、随调整重置为新首次到期日。
type Plan struct {
	AssetID      string `json:"asset_id"`
	Content      string `json:"content"`
	FirstDue     string `json:"first_due"`
	IntervalDays int    `json:"interval_days"`
	NextDue      string `json:"next_due"`
}

// maxDayNum 为 9999-12-31 的日序号（0001-01-01 为 0）。
const maxDayNum = 3652058

// daysFromCivil 返回公历日期距 0001-01-01 的天数（该日为 0）。
// 输入须为 0001 至 9999 年间的有效日期（由 parseDate 保证）。
func daysFromCivil(y, m, d int) int64 {
	yy := int64(y)
	if m <= 2 {
		yy--
	}
	era := yy / 400
	yoe := yy - era*400
	mp := int64((m + 9) % 12)
	doy := (153*mp+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 306
}

// civilFromDays 为 daysFromCivil 的逆运算。
func civilFromDays(n int64) (int, int, int) {
	z := n + 306
	era := z / 146097
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	if mp < 10 {
		return int(y), int(mp + 3), int(d)
	}
	return int(y + 1), int(mp - 9), int(d)
}

func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	}
	if isLeapYear(y) {
		return 29
	}
	return 28
}

// atoiN 解析定长十进制数字串；含非数字字符时 ok=false。
func atoiN(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// parseDate 解析 YYYY-MM-DD 形式的公历日期（0001 至 9999 年，须为零填充的
// 完整形式），返回日序号。闰日等按公历规则校验。
func parseDate(s string) (int64, error) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return 0, fmt.Errorf("不是 YYYY-MM-DD 形式: %q", s)
	}
	y, ok1 := atoiN(s[0:4])
	m, ok2 := atoiN(s[5:7])
	d, ok3 := atoiN(s[8:10])
	if !ok1 || !ok2 || !ok3 {
		return 0, fmt.Errorf("不是 YYYY-MM-DD 形式: %q", s)
	}
	if y < 1 || m < 1 || m > 12 || d < 1 || d > daysInMonth(y, m) {
		return 0, fmt.Errorf("不是 0001 至 9999 年间的有效公历日期: %q", s)
	}
	return daysFromCivil(y, m, d), nil
}

// formatDate 把日序号格式化为 YYYY-MM-DD。
func formatDate(n int64) string {
	y, m, d := civilFromDays(n)
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

// nextDueAfter 返回首次到期日加整数倍间隔所得日期中，严格晚于 after 的最早
// 日期。结果超出 9999-12-31 时 ok=false。间隔必须为正整数。
func nextDueAfter(first, interval, after int64) (int64, bool) {
	var k int64
	if after >= first {
		k = (after-first)/interval + 1
	}
	// k ≥ 1 且间隔本身已超出剩余日期范围时，结果必然越界；先判断避免溢出。
	if k > 0 && interval > maxDayNum-first {
		return 0, false
	}
	n := first + k*interval
	if n > maxDayNum {
		return 0, false
	}
	return n, true
}

// findPlan 返回资产的保养计划，无计划时返回 nil。
func (s *store) findPlan(assetID string) *Plan {
	for _, p := range s.data.Plans {
		if p.AssetID == assetID {
			return p
		}
	}
	return nil
}

// appendMaintEvent 以给定序号追加保养履历（建立或完成）。保养履历不属于任何
// 工单，from/to 恒为空；due/done/interval 按履历类型使用。
func (s *store) appendMaintEvent(seq int, assetID, kind, content, due, done string, interval int) {
	s.data.Events = append(s.data.Events, Event{
		Seq:      seq,
		AssetID:  assetID,
		Kind:     kind,
		Content:  content,
		Due:      due,
		Done:     done,
		Interval: interval,
		Time:     s.now(),
	})
}

// createPlan 为资产建立唯一的周期保养计划：每项资产最多一个，已有计划时拒绝，
// 不覆盖。业务判定全部经共用核心（checkCreatePlan）；通过后才取序号并落库，
// 因此任何失败路径都不修改业务数据、不消耗序号。
func (s *store) createPlan(assetID, content, firstDue string, intervalDays int) (*Plan, error) {
	if err := checkCreatePlan(s.data, assetID, content, firstDue, intervalDays); err != nil {
		return nil, err
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	p := &Plan{
		AssetID:      assetID,
		Content:      content,
		FirstDue:     firstDue,
		IntervalDays: intervalDays,
		NextDue:      firstDue,
	}
	s.data.Plans = append(s.data.Plans, p)
	s.appendMaintEvent(eventSeq, assetID, eventPlanCreate, content, firstDue, "", intervalDays)
	return p, nil
}

// adjustPlan 调整已有计划的保养方案：更换内容、周期起点（新首次到期日）与
// 间隔，理由非空；仅已有计划的资产可调整，维修中、停用时也允许。业务判定
// （含“新首次到期日严格晚于所有方案段内未撤销完成实际完成日”）全部经共用
// 核心，核心逐步重放得到当时状态而非取最终状态；通过后才取序号并落库。
// 成功以调整履历的全库序号开启新方案段，下一到期日设为新首次日，保留全部
// 历史；调整不改变资产状态、工单、请求绑定或停机统计。
func (s *store) adjustPlan(assetID, content, firstDue string, intervalDays int, reason string) (*Plan, *Plan, error) {
	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		return nil, nil, fmt.Errorf("保养履历无法核对：%w", err)
	}
	oldSpec, err := checkAdjustPlan(s.data, rr, assetID, content, firstDue, intervalDays, reason)
	if err != nil {
		return nil, nil, err
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	p := s.findPlan(assetID)
	old := &Plan{
		AssetID:      assetID,
		Content:      oldSpec.content,
		FirstDue:     oldSpec.firstDue,
		IntervalDays: oldSpec.interval,
		NextDue:      p.NextDue,
	}
	s.data.Events = append(s.data.Events, Event{
		Seq:         eventSeq,
		AssetID:     assetID,
		Kind:        eventPlanAdjust,
		Content:     reason,
		Due:         firstDue,
		Interval:    intervalDays,
		NewContent:  content,
		OldContent:  oldSpec.content,
		OldDue:      oldSpec.firstDue,
		OldInterval: oldSpec.interval,
		OldNextDue:  p.NextDue,
		Time:        s.now(),
	})
	p.Content = content
	p.FirstDue = firstDue
	p.IntervalDays = intervalDays
	p.NextDue = firstDue
	return p, old, nil
}

// completePlan 登记当前方案段下一周期的保养完成。业务判定（接续当时下一
// 到期日、完成日不早于周期到期日、按当段方案推进、越界整次拒绝、停用拒绝）
// 全部经共用核心，核心逐步重放得到当时方案段与下一到期日；通过后才取序号
// 并落库，因此失败路径不推进日期、不新增履历、不消耗序号。返回完成履历的
// 全库序号。
func (s *store) completePlan(assetID, due, done, result string) (*Plan, string, int, error) {
	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		return nil, "", 0, fmt.Errorf("保养履历无法核对：%w", err)
	}
	next, err := checkCompletePlan(s.data, rr, assetID, due, done, result)
	if err != nil {
		return nil, "", 0, err
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, "", 0, err
	}
	p := s.findPlan(assetID)
	p.NextDue = next
	s.appendMaintEvent(eventSeq, assetID, eventPlanDone, result, due, done, 0)
	return p, p.NextDue, eventSeq, nil
}

// revokedDoneSeqs 返回已被撤销的保养完成履历序号集合。
func (s *store) revokedDoneSeqs() map[int]bool {
	revoked := map[int]bool{}
	for _, e := range s.data.Events {
		if e.Kind == eventPlanRevoke {
			revoked[e.TargetSeq] = true
		}
	}
	return revoked
}

// revokeCompletion 撤销误登记的保养完成。撤销定位、归属与“当前段最新未撤销
// 完成”的判定全部经共用核心（checkRevokeCompletion，基于逐步重放的当时方案
// 段）；通过后才取序号并落库。成功保留原完成的日期、结果与时间，追加撤销
// 履历，并把下一到期日恢复为该完成的周期到期日。维修中、停用时允许撤销。
func (s *store) revokeCompletion(assetID string, targetSeq int, reason string) (*Plan, *Event, error) {
	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		return nil, nil, fmt.Errorf("保养履历无法核对：%w", err)
	}
	target, err := checkRevokeCompletion(s.data, rr, assetID, targetSeq, reason)
	if err != nil {
		return nil, nil, err
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	p := s.findPlan(assetID)
	p.NextDue = target.Due
	s.data.Events = append(s.data.Events, Event{
		Seq:       eventSeq,
		AssetID:   assetID,
		Kind:      eventPlanRevoke,
		Content:   reason,
		TargetSeq: targetSeq,
		Time:      s.now(),
	})
	return p, target, nil
}

// duePlanRow 为到期查询的一行结果。
type duePlanRow struct {
	AssetID string
	Name    string
	Content string
	Due     string
}

// duePlans 列出下一到期日不晚于 date 的保养计划，按到期日再按资产编号排序。
// 停用资产不参与到期查询：停用不暂停或重算周期，保存的下一到期日保持不变，
// 恢复使用后仍按该日期参与查询，逾期计划显示原到期日。
// 只读：不写文件、不初始化目录、不推进计划。date 须为有效日期（调用方校验）。
func (s *store) duePlans(date string) []duePlanRow {
	rows := make([]duePlanRow, 0)
	for _, p := range s.data.Plans {
		// YYYY-MM-DD 零填充形式的字典序即时间顺序。
		if p.NextDue > date {
			continue
		}
		a := s.findAsset(p.AssetID)
		if a == nil {
			continue // 整库一致性检查已保证归属；防御性跳过
		}
		if a.Status == statusDeactivated {
			continue
		}
		rows = append(rows, duePlanRow{AssetID: p.AssetID, Name: a.Name, Content: p.Content, Due: p.NextDue})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Due != rows[j].Due {
			return rows[i].Due < rows[j].Due
		}
		return rows[i].AssetID < rows[j].AssetID
	})
	return rows
}
