package main

import "fmt"

// 周期保养的可复用业务核心。
//
// 保养的全部业务判定——建立、完成、撤销与方案调整的接续规则及状态推进——都
// 收敛在本文件：
//   - 日常操作（maintenance.go 的 createPlan/adjustPlan/completePlan/
//     revokeCompletion）先经本核心做纯业务判定，再把判定结果落到记录上；
//   - 台账加载、保存前校验与导入后校验共用同一条“按履历序号逐步重放”的
//     校验路径（replayAllMaintenance），不存在各路径分别维护的重复规则；
//   - 到期查询（due）与 history 只读取重放得到的当前状态。
//
// 核心的判定函数只读取传入的业务记录，绝不修改它们，也不借校验补字段或
// 修复数据；命令与持久化层可在各场景按既有措辞与退出码包装错误。
//
// 方案段以保养建立与每次调整履历的全库履历序号划分（不按日期或数组位置）：
// 完成只接续当时下一到期日，按当段首次日加整数倍间隔推进；撤销只作用于
// 当前段最新的未撤销完成；调整开启新段并把下一到期日重置为新首次日。
//
// 重放逐步采用“当时”的方案、有效完成与停用状态，绝不用最终状态代替历史
// 状态：数组乱序、序号有间隔、履历时间不递增都不影响（顺序只由序号决定）。

// maintenanceError 标记保养核心自身发现的业务矛盾（区别于格式与字段问题）。
// 各调用场景按需要包装为既有错误措辞，错误信息仍须指出问题类别。
type maintenanceError struct{ msg string }

func (e *maintenanceError) Error() string { return e.msg }

func maintConflictf(format string, args ...any) error {
	return &maintenanceError{msg: fmt.Sprintf(format, args...)}
}

// planSpec 为某一时刻一个方案段的方案（内容、首次到期日、间隔天数）。
type planSpec struct {
	content  string
	firstDue string
	interval int
}

// maintState 为单个资产保养履历链按全库履历序号逐步重放得到的状态。
// 逐步重放时它表示“当时”的状态；重放结束后即当前状态。
type maintState struct {
	created   bool     // 是否已出现建立履历
	createSeq int      // 建立履历序号（0 表示尚未建立）
	spec      planSpec // 当前方案段方案
	nextDue   string   // 当时/当前下一到期日
	segStart  int      // 当前方案段起点履历序号（建立或调整）
	// 全部未撤销完成（跨所有方案段）的实际完成日序号，用于调整时的
	// “严格晚于所有未撤销完成实际完成日”限制，撤销即从中删除。
	validDoneDay map[int]int64
	// 当前段完成履历序号（按序号升序追加）；撤销不删除，用 revoked 判定。
	segDones []int
	// 当前段各完成的周期到期日，用于撤销时恢复下一到期日。
	segDoneDue map[int]string
	// 全部完成（含已撤销、含旧段）序号集合，用于区分“旧段完成”。
	allDone map[int]bool
	// 已撤销完成序号集合（全库）。
	revoked map[int]bool
}

func newMaintState() *maintState {
	return &maintState{
		validDoneDay: map[int]int64{},
		segDoneDue:   map[int]string{},
		allDone:      map[int]bool{},
		revoked:      map[int]bool{},
	}
}

// apply 按全库履历序号逐步应用一条保养履历（调用方须先按序号升序，并按
// 资产分别重放；停用/恢复履历不经过这里，停用状态由 replayAllMaintenance
// 在完成时另行核对）。任一步与当时状态矛盾即返回错误，且不保证状态仍可
// 继续使用——校验场景应直接拒绝整份台账。
func (st *maintState) apply(e Event) error {
	switch e.Kind {
	case eventPlanCreate:
		if st.created {
			return maintConflictf("资产 %s 有多条保养建立履历", e.AssetID)
		}
		st.created = true
		st.createSeq = e.Seq
		st.spec = planSpec{content: e.Content, firstDue: e.Due, interval: e.Interval}
		st.segStart = e.Seq
		st.nextDue = e.Due
		return nil

	case eventPlanAdjust:
		if !st.created {
			return maintConflictf("资产 %s 的调整履历（序号 %d）出现在建立履历之前", e.AssetID, e.Seq)
		}
		// 调整履历记录的原方案与原下一到期日须与当时状态一致。
		if e.OldContent != st.spec.content || e.OldDue != st.spec.firstDue ||
			e.OldInterval != st.spec.interval {
			return maintConflictf(
				"保养调整履历（序号 %d）记录的原方案与资产 %s 当时的方案不符",
				e.Seq, e.AssetID)
		}
		if e.OldNextDue != st.nextDue {
			return maintConflictf(
				"保养调整履历（序号 %d）记录的原下一到期日 %s 与当时的下一到期日 %s 不符",
				e.Seq, e.OldNextDue, st.nextDue)
		}
		// 新首次到期日须严格晚于当时所有方案段内未撤销完成的实际完成日。
		newFirst, _ := parseDate(e.Due)
		for doneSeq, doneDay := range st.validDoneDay {
			if newFirst <= doneDay {
				return maintConflictf(
					"保养调整履历（序号 %d）的新首次到期日 %s 未严格晚于未撤销完成（序号 %d）的实际完成日 %s",
					e.Seq, e.Due, doneSeq, formatDate(doneDay))
			}
		}
		// 开启新方案段：保留全部历史，下一到期日设为新首次日；旧段完成
		// 不能再撤销。
		st.spec = planSpec{content: e.NewContent, firstDue: e.Due, interval: e.Interval}
		st.segStart = e.Seq
		st.nextDue = e.Due
		st.segDones = nil
		st.segDoneDue = map[int]string{}
		return nil

	case eventPlanRevoke:
		if !st.created {
			return maintConflictf("资产 %s 的撤销履历（序号 %d）出现在建立履历之前", e.AssetID, e.Seq)
		}
		due, ok := st.segDoneDue[e.TargetSeq]
		if !ok {
			if st.allDone[e.TargetSeq] {
				return maintConflictf(
					"保养撤销履历（序号 %d）的目标序号 %d 属于资产 %s 调整前的旧方案段，旧段完成不能再撤销",
					e.Seq, e.TargetSeq, e.AssetID)
			}
			return maintConflictf(
				"保养撤销履历（序号 %d）的目标序号 %d 不是资产 %s 先前的完成履历",
				e.Seq, e.TargetSeq, e.AssetID)
		}
		if st.revoked[e.TargetSeq] {
			return maintConflictf("完成履历序号 %d 被重复撤销（撤销履历序号 %d）",
				e.TargetSeq, e.Seq)
		}
		if latest := st.latestValidDoneSeq(); latest != e.TargetSeq {
			return maintConflictf(
				"保养撤销履历（序号 %d）的目标序号 %d 不是当时最新的有效完成（序号 %d）",
				e.Seq, e.TargetSeq, latest)
		}
		st.revoked[e.TargetSeq] = true
		// 撤销目标完成的实际完成日不再参与调整限制；下一到期日恢复为该
		// 完成的周期到期日（延期跨过的周期不补记录）。
		delete(st.validDoneDay, e.TargetSeq)
		st.nextDue = due
		return nil

	case eventPlanDone:
		if !st.created {
			return maintConflictf("资产 %s 的完成履历（序号 %d）出现在建立履历之前", e.AssetID, e.Seq)
		}
		// 完成须接续当时下一到期日：旧周期不能重复登记，也不能登记尚未
		// 到期的新周期；实际完成日不得早于周期到期日。
		if e.Due != st.nextDue {
			return maintConflictf(
				"资产 %s 的保养完成履历（序号 %d）周期到期日 %s 与当前下一到期日 %s 不接续",
				e.AssetID, e.Seq, e.Due, st.nextDue)
		}
		dueN, _ := parseDate(e.Due)
		doneN, _ := parseDate(e.Done)
		if doneN < dueN {
			return maintConflictf("资产 %s 的保养完成履历（序号 %d）实际完成日早于周期到期日",
				e.AssetID, e.Seq)
		}
		// 以当段首次日加整数倍间隔，推进到严格晚于完成日的最早日期；
		// 超出 9999-12-31 的完成整次拒绝。
		first, _ := parseDate(st.spec.firstDue)
		next, ok := nextDueAfter(first, int64(st.spec.interval), doneN)
		if !ok {
			return maintConflictf(
				"资产 %s 的保养完成履历（序号 %d）无法推出日期范围内的下一到期日",
				e.AssetID, e.Seq)
		}
		st.nextDue = formatDate(next)
		st.segDones = append(st.segDones, e.Seq)
		st.segDoneDue[e.Seq] = e.Due
		st.allDone[e.Seq] = true
		st.validDoneDay[e.Seq] = doneN
		return nil
	}
	return maintConflictf("履历序号 %d 不是保养履历", e.Seq)
}

// latestValidDoneSeq 返回当前段内按序号最新的未撤销完成；没有时返回 0。
func (st *maintState) latestValidDoneSeq() int {
	latest := 0
	for _, seq := range st.segDones {
		if !st.revoked[seq] {
			latest = seq
		}
	}
	return latest
}

// replayResult 为整库保养履历按序号逐步重放的结果。
type replayResult struct {
	states map[string]*maintState
}

// replayAllMaintenance 按全库履历序号逐步重放全部保养履历，逐步采用当时的
// 方案、有效完成与停用状态（停用期间出现完成履历即矛盾；停用不暂停或重算
// 周期，其余接续规则不受影响）。纯函数：不修改 d 中的任何记录，也不补字段。
// 履历字段的格式与归属问题已由 validateData 的结构性检查先行拒绝，这里只
// 负责建立、完成、撤销与调整链的接续判定。
func replayAllMaintenance(d *storeData) (*replayResult, error) {
	// 顺序只由全库履历序号决定；复制一份排序，绝不改动原数组。
	sorted := make([]Event, len(d.Events))
	copy(sorted, d.Events)
	sortEventsBySeq(sorted)

	states := map[string]*maintState{}
	deactivated := map[string]bool{}
	for _, e := range sorted {
		switch e.Kind {
		case eventDeactivate:
			deactivated[e.AssetID] = true
			continue
		case eventReactivate:
			delete(deactivated, e.AssetID)
			continue
		case eventPlanCreate, eventPlanDone, eventPlanRevoke, eventPlanAdjust:
		default:
			continue
		}
		st := states[e.AssetID]
		if st == nil {
			st = newMaintState()
			states[e.AssetID] = st
		}
		if e.Kind == eventPlanDone && deactivated[e.AssetID] {
			return nil, maintConflictf("资产 %s 在停用期间出现保养完成履历（序号 %d）",
				e.AssetID, e.Seq)
		}
		if err := st.apply(e); err != nil {
			return nil, err
		}
	}
	return &replayResult{states: states}, nil
}

// sortEventsBySeq 按履历序号稳定升序排序（不改动元素本身）。
func sortEventsBySeq(events []Event) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j-1].Seq > events[j].Seq; j-- {
			events[j-1], events[j] = events[j], events[j-1]
		}
	}
}

// ---- 日常操作的纯业务判定 ----
//
// 下列函数只读取当前业务记录与调用方提供的参数，返回业务判定结论，绝不
// 修改记录；调用方（maintenance.go）在全部判定通过后再一次性落库，保证
// 失败路径不留下部分变化、不消耗序号。

// checkCreatePlan 判定能否为资产建立计划：资产存在、尚无计划、内容非空、
// 间隔为正整数、首次到期日有效。
func checkCreatePlan(d *storeData, assetID, content, firstDue string, interval int) error {
	if findAssetIn(d, assetID) == nil {
		return fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if findPlanIn(d, assetID) != nil {
		return fmt.Errorf("%w: 资产 %s 已有保养计划，不能重复建立或覆盖", errConflict, assetID)
	}
	if content == "" {
		return fmt.Errorf("%w: 保养内容不能为空", errConflict)
	}
	if interval < 1 {
		return fmt.Errorf("%w: 保养间隔天数须为正整数", errConflict)
	}
	if _, err := parseDate(firstDue); err != nil {
		return fmt.Errorf("%w: 首次到期日无效：%s", errConflict, err)
	}
	return nil
}

// checkAdjustPlan 判定能否调整方案并返回原方案快照。新首次到期日须严格晚于
// 所有方案段内未撤销完成的实际完成日；维修中、停用时也允许。
func checkAdjustPlan(d *storeData, rr *replayResult, assetID, content, firstDue string, interval int, reason string) (planSpec, error) {
	if findAssetIn(d, assetID) == nil {
		return planSpec{}, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	p := findPlanIn(d, assetID)
	if p == nil {
		return planSpec{}, fmt.Errorf("%w: 资产 %s 没有保养计划，不能调整", errNotFound, assetID)
	}
	if content == "" {
		return planSpec{}, fmt.Errorf("%w: 保养内容不能为空", errConflict)
	}
	if reason == "" {
		return planSpec{}, fmt.Errorf("%w: 调整理由不能为空", errConflict)
	}
	if interval < 1 {
		return planSpec{}, fmt.Errorf("%w: 保养间隔天数须为正整数", errConflict)
	}
	firstN, err := parseDate(firstDue)
	if err != nil {
		return planSpec{}, fmt.Errorf("%w: 新首次到期日无效：%s", errConflict, err)
	}
	if st := rr.states[assetID]; st != nil {
		latestDone := int64(-1)
		for _, doneDay := range st.validDoneDay {
			if doneDay > latestDone {
				latestDone = doneDay
			}
		}
		if latestDone >= 0 && firstN <= latestDone {
			return planSpec{}, fmt.Errorf(
				"%w: 新首次到期日 %s 须严格晚于资产 %s 未撤销完成的实际完成日 %s",
				errConflict, firstDue, assetID, formatDate(latestDone))
		}
	}
	return planSpec{content: p.Content, firstDue: p.FirstDue, interval: p.IntervalDays}, nil
}

// checkCompletePlan 判定能否登记完成：须有计划、资产未停用、结果非空、日期
// 有效、所填周期到期日等于当前下一到期日、完成日不早于到期日，且推进后的
// 下一到期日不超出 9999-12-31。返回推进后的下一到期日字符串。
func checkCompletePlan(d *storeData, rr *replayResult, assetID, due, done, result string) (string, error) {
	if findAssetIn(d, assetID) == nil {
		return "", fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	p := findPlanIn(d, assetID)
	if p == nil {
		return "", fmt.Errorf("%w: 资产 %s 没有保养计划", errNotFound, assetID)
	}
	if a := findAssetIn(d, assetID); a.Status == statusDeactivated {
		return "", fmt.Errorf("%w: 资产 %s 已停用，不能登记保养完成", errConflict, assetID)
	}
	if result == "" {
		return "", fmt.Errorf("%w: 保养结果不能为空", errConflict)
	}
	dueN, err := parseDate(due)
	if err != nil {
		return "", fmt.Errorf("%w: 周期到期日无效：%s", errConflict, err)
	}
	doneN, err := parseDate(done)
	if err != nil {
		return "", fmt.Errorf("%w: 实际完成日无效：%s", errConflict, err)
	}
	st := rr.states[assetID]
	if st == nil || !st.created {
		// 已有计划却重放不出建立履历属于台账矛盾，加载/保存校验应已拒绝。
		return "", fmt.Errorf("%w: 资产 %s 的保养履历缺少建立记录", errConflict, assetID)
	}
	if due != st.nextDue {
		return "", fmt.Errorf(
			"%w: 周期到期日 %s 与资产 %s 当前下一到期日 %s 不符（旧周期不能重复登记，也不能登记新周期）",
			errConflict, due, assetID, st.nextDue)
	}
	if doneN < dueN {
		return "", fmt.Errorf("%w: 实际完成日 %s 早于周期到期日 %s", errConflict, done, due)
	}
	first, _ := parseDate(st.spec.firstDue)
	next, ok := nextDueAfter(first, int64(st.spec.interval), doneN)
	if !ok {
		return "", fmt.Errorf(
			"%w: 按完成日 %s 推算的下一到期日超出 9999-12-31，无法登记本次完成", errConflict, done)
	}
	return formatDate(next), nil
}

// checkRevokeCompletion 判定能否撤销目标完成履历，返回目标完成事件。目标须
// 为该资产当前方案段内按序号最新的未撤销完成；未知资产、无计划、未知序号、
// 目标不是该资产完成、旧段完成、重复撤销、存在更晚有效完成均拒绝。维修中、
// 停用时允许撤销。
func checkRevokeCompletion(d *storeData, rr *replayResult, assetID string, targetSeq int, reason string) (*Event, error) {
	if findAssetIn(d, assetID) == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: 撤销理由不能为空", errConflict)
	}
	if findPlanIn(d, assetID) == nil {
		return nil, fmt.Errorf("%w: 资产 %s 没有保养计划", errNotFound, assetID)
	}
	var target *Event
	for i := range d.Events {
		if d.Events[i].Seq == targetSeq {
			target = &d.Events[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("%w: 未知履历序号 %d", errNotFound, targetSeq)
	}
	if target.Kind != eventPlanDone || target.AssetID != assetID {
		return nil, fmt.Errorf("%w: 履历序号 %d 不是资产 %s 的保养完成履历",
			errConflict, targetSeq, assetID)
	}
	st := rr.states[assetID]
	if st == nil {
		return nil, fmt.Errorf("%w: 资产 %s 没有可撤销的保养完成", errConflict, assetID)
	}
	if target.Seq < st.segStart {
		return nil, fmt.Errorf(
			"%w: 完成履历序号 %d 属于资产 %s 调整前的旧方案段，旧段完成不能再撤销",
			errConflict, targetSeq, assetID)
	}
	if st.revoked[targetSeq] {
		return nil, fmt.Errorf("%w: 完成履历序号 %d 已撤销，不能重复撤销", errConflict, targetSeq)
	}
	if latest := st.latestValidDoneSeq(); latest != targetSeq {
		return nil, fmt.Errorf(
			"%w: 完成履历序号 %d 不是资产 %s 当前方案段最新的有效完成（当前为序号 %d），存在更晚有效完成时不能撤销",
			errConflict, targetSeq, assetID, latest)
	}
	return target, nil
}

// findAssetIn 在给定数据中查找资产，不修改数据。
func findAssetIn(d *storeData, id string) *Asset {
	for _, a := range d.Assets {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// findPlanIn 在给定数据中查找保养计划，不修改数据。
func findPlanIn(d *storeData, assetID string) *Plan {
	for _, p := range d.Plans {
		if p.AssetID == assetID {
			return p
		}
	}
	return nil
}
