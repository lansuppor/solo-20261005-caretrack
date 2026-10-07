package main

import (
	"fmt"
	"sort"
)

// 周期保养计划、方案调整与完成登记。
//
// 日期一律为 0001 至 9999 年的公历日历日（YYYY-MM-DD），按日历日运算：
// 不涉及时区与运行时刻，内部以“距 0001-01-01 的天数”做整数运算，避免
// time.Time 的时区、跨度（time.Duration 约 292 年上限）与年份范围问题。
//
// 每个计划恰有一条建立履历（含初始方案：内容、首次到期日、间隔天数），
// 每次方案调整追加一条调整履历（含前后方案、原下一到期日、理由），每次完成
// 登记追加一条完成履历（含周期到期日、实际完成日、结果），每次撤销误登记
// 追加一条撤销履历（含目标完成履历序号、理由）。以该资产建立履历与各次调整
// 履历的全库序号划分方案段：每段按当时方案推进，完成与撤销只作用于当前段。
// 计划保存当前方案与由履历链推出的下一到期日，加载与保存时核对二者一致。
// 保养不创建或终结工单、不消耗工单编号，不改变资产状态、请求绑定或停机统计。

// Plan 为资产的周期保养计划；每项资产最多一个，不可覆盖。
// Content/FirstDue/IntervalDays 为当前方案（随方案调整更换），
// NextDue 为由履历链推出的下一到期日，随完成登记推进、随方案调整重设。
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
// 不覆盖。成功时追加一条含初始计划的建立履历，下一到期日即首次到期日。
// 失败路径不修改任何业务数据。
func (s *store) createPlan(assetID, content, firstDue string, intervalDays int) (*Plan, error) {
	if s.findAsset(assetID) == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if s.findPlan(assetID) != nil {
		return nil, fmt.Errorf("%w: 资产 %s 已有保养计划，不能重复建立或覆盖", errConflict, assetID)
	}
	if content == "" {
		return nil, fmt.Errorf("%w: 保养内容不能为空", errConflict)
	}
	if intervalDays < 1 {
		return nil, fmt.Errorf("%w: 保养间隔天数须为正整数", errConflict)
	}
	if _, err := parseDate(firstDue); err != nil {
		return nil, fmt.Errorf("%w: 首次到期日无效：%s", errConflict, err)
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

// adjustPlan 调整资产的周期保养方案：仅已有计划可调整（plan 只建立计划，不能
// 覆盖；adjust 更换已有计划的内容、周期起点和间隔），维修中、停用时也允许。
// 新首次到期日须严格晚于该资产所有未撤销完成的实际完成日；没有有效完成则无
// 此限制。成功时计划采用新方案，下一到期日设为新首次到期日，并追加一条记录
// 前后方案、原下一到期日、理由与时间的资产级调整履历；不补任何完成记录，
// 原完成的内容、日期、时间及撤销状态保留。调整不改变资产状态、工单、请求
// 绑定或停机统计。失败路径不修改任何业务数据。
func (s *store) adjustPlan(assetID, content, firstDue string, intervalDays int, reason string) (*Plan, error) {
	if s.findAsset(assetID) == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	p := s.findPlan(assetID)
	if p == nil {
		return nil, fmt.Errorf("%w: 资产 %s 没有保养计划，不能调整方案", errNotFound, assetID)
	}
	if content == "" {
		return nil, fmt.Errorf("%w: 保养内容不能为空", errConflict)
	}
	if intervalDays < 1 {
		return nil, fmt.Errorf("%w: 保养间隔天数须为正整数", errConflict)
	}
	newFirst, err := parseDate(firstDue)
	if err != nil {
		return nil, fmt.Errorf("%w: 新首次到期日无效：%s", errConflict, err)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: 调整理由不能为空", errConflict)
	}
	// 新首次日须严格晚于所有未撤销完成的实际完成日；没有有效完成则无此限制。
	revoked := s.revokedDoneSeqs()
	var maxDone int64
	hasValid := false
	for _, e := range s.data.Events {
		if e.AssetID != assetID || e.Kind != eventPlanDone || revoked[e.Seq] {
			continue
		}
		done, _ := parseDate(e.Done) // 整库一致性检查已保证完成日有效
		if !hasValid || done > maxDone {
			maxDone = done
		}
		hasValid = true
	}
	if hasValid && newFirst <= maxDone {
		return nil, fmt.Errorf(
			"%w: 新首次到期日 %s 须严格晚于资产 %s 未撤销完成的最晚实际完成日 %s",
			errConflict, firstDue, assetID, formatDate(maxDone))
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	s.data.Events = append(s.data.Events, Event{
		Seq:         eventSeq,
		AssetID:     assetID,
		Kind:        eventPlanAdjust,
		Content:     content,
		Due:         firstDue,
		Interval:    intervalDays,
		Reason:      reason,
		OldContent:  p.Content,
		OldDue:      p.FirstDue,
		OldInterval: p.IntervalDays,
		OldNextDue:  p.NextDue,
		Time:        s.now(),
	})
	p.Content = content
	p.FirstDue = firstDue
	p.IntervalDays = intervalDays
	p.NextDue = firstDue
	return p, nil
}

// completePlan 登记当前方案段下一周期的保养完成：所填到期日必须等于当前下一
// 到期日（旧周期不能重复登记，也不能登记尚未到期的新周期），实际完成日不得
// 早于周期到期日。成功时追加一条完成履历，并把下一到期日推进到本段首次到期日
// 加整数倍间隔所得日期中严格晚于完成日的最早日期；延期跨过的周期不生成完成
// 记录。若下一到期日超出 9999-12-31，整次拒绝。返回完成履历的全库序号。
// 失败路径不修改任何业务数据。
func (s *store) completePlan(assetID, due, done, result string) (*Plan, string, int, error) {
	if s.findAsset(assetID) == nil {
		return nil, "", 0, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	p := s.findPlan(assetID)
	if p == nil {
		return nil, "", 0, fmt.Errorf("%w: 资产 %s 没有保养计划", errNotFound, assetID)
	}
	if a := s.findAsset(assetID); a != nil && a.Status == statusDeactivated {
		// 停用期间拒绝保养完成登记；停用不暂停或重算周期，保存的下一到期日不变。
		return nil, "", 0, fmt.Errorf("%w: 资产 %s 已停用，不能登记保养完成", errConflict, assetID)
	}
	if result == "" {
		return nil, "", 0, fmt.Errorf("%w: 保养结果不能为空", errConflict)
	}
	dueN, err := parseDate(due)
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: 周期到期日无效：%s", errConflict, err)
	}
	doneN, err := parseDate(done)
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: 实际完成日无效：%s", errConflict, err)
	}
	if due != p.NextDue {
		return nil, "", 0, fmt.Errorf(
			"%w: 周期到期日 %s 与资产 %s 当前下一到期日 %s 不符（旧周期不能重复登记，也不能登记新周期）",
			errConflict, due, assetID, p.NextDue)
	}
	if doneN < dueN {
		return nil, "", 0, fmt.Errorf("%w: 实际完成日 %s 早于周期到期日 %s", errConflict, done, due)
	}
	first, _ := parseDate(p.FirstDue)
	next, ok := nextDueAfter(first, int64(p.IntervalDays), doneN)
	if !ok {
		return nil, "", 0, fmt.Errorf(
			"%w: 按完成日 %s 推算的下一到期日超出 9999-12-31，无法登记本次完成", errConflict, done)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, "", 0, err
	}
	p.NextDue = formatDate(next)
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

// revokeCompletion 撤销误登记的保养完成：目标须为该资产当前方案段按序号最新的
// 未撤销完成（存在更晚有效完成时拒绝；撤销后可继续撤销本段此前最新有效完成；
// 方案段由该资产建立履历与各次调整履历的全库序号划分，旧段完成不能再撤销）。
// 成功时保留原完成的日期、结果与时间，追加一条含目标序号、理由与操作时间的
// 撤销履历，并把下一到期日恢复为该完成的周期到期日（延期跨过的周期不补记录）。
// 维修或其他资产事件不阻止撤销。失败路径不修改任何业务数据。
func (s *store) revokeCompletion(assetID string, targetSeq int, reason string) (*Plan, *Event, error) {
	if s.findAsset(assetID) == nil {
		return nil, nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if reason == "" {
		return nil, nil, fmt.Errorf("%w: 撤销理由不能为空", errConflict)
	}
	p := s.findPlan(assetID)
	if p == nil {
		return nil, nil, fmt.Errorf("%w: 资产 %s 没有保养计划", errNotFound, assetID)
	}
	var target *Event
	for i := range s.data.Events {
		if s.data.Events[i].Seq == targetSeq {
			target = &s.data.Events[i]
			break
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("%w: 未知履历序号 %d", errNotFound, targetSeq)
	}
	if target.Kind != eventPlanDone || target.AssetID != assetID {
		return nil, nil, fmt.Errorf("%w: 履历序号 %d 不是资产 %s 的保养完成履历", errConflict, targetSeq, assetID)
	}
	revoked := s.revokedDoneSeqs()
	if revoked[targetSeq] {
		return nil, nil, fmt.Errorf("%w: 完成履历序号 %d 已撤销，不能重复撤销", errConflict, targetSeq)
	}
	// 当前方案段起点：该资产建立履历或最近一次调整履历的序号。
	segStart := 0
	for _, e := range s.data.Events {
		if e.AssetID == assetID && (e.Kind == eventPlanCreate || e.Kind == eventPlanAdjust) && e.Seq > segStart {
			segStart = e.Seq
		}
	}
	if target.Seq <= segStart {
		return nil, nil, fmt.Errorf(
			"%w: 完成履历序号 %d 属于资产 %s 的旧方案段，方案调整后旧段完成不能再撤销",
			errConflict, targetSeq, assetID)
	}
	latest := 0
	for _, e := range s.data.Events {
		if e.AssetID == assetID && e.Kind == eventPlanDone && e.Seq > segStart && !revoked[e.Seq] && e.Seq > latest {
			latest = e.Seq
		}
	}
	if latest != targetSeq {
		return nil, nil, fmt.Errorf(
			"%w: 完成履历序号 %d 不是资产 %s 当前方案段最新的有效完成（当前为序号 %d），存在更晚有效完成时不能撤销",
			errConflict, targetSeq, assetID, latest)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
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
