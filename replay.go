package main

import (
	"fmt"
	"io"
	"sort"
)

// 按履历进度回看设备状态（只读）。
//
// 回看只依据全库履历序号：纳入序号不大于截止序号的全部履历，在其上重放
// 报修、派工、关闭、取消、停用、恢复使用、位置变更与保养建立、完成、撤销、
// 调整，得到截止处的资产位置、状态、未关闭工单（含当时负责人）与当时的
// 保养方案段。绝不直接采用记录中保存的最终状态或最终负责人——截止之后的
// 关闭、转派、搬移、恢复、撤销或调整不得提前影响回看结果。
//
// 截止值不必对应现存事件：落在序号间隔中仍可查询；超过全库最大序号时按
// 全部履历处理（即当前状态）；序号 0 为初态——可用、无工单、无保养计划，
// 初始位置取最早位置变更履历的原位置，没有位置履历则取保存位置。
//
// 维修中搬移后资产位置随搬移改变，旧工单的报修地点保持为其报修履历序号
// 当时的位置。保养建立、完成、撤销与调整采用截止处的方案段：延期完成按
// 当段起点与间隔推进，跳过周期不补记录；撤销按目标完成序号恢复周期，同
// 到期日重新完成按新序号区分；调整后采用新方案。停用期间仍展示保存的
// 保养方案与到期日。
//
// 查询针对当前已登记资产的履历进度，不判断资产登记时间；编号与名称沿用
// 登记信息。查询为只读：不写文件、不初始化目录、不消耗编号，不改请求绑定
// 或当前业务状态。openStore 已先做整库一致性检查：即使矛盾位于截止之后或
// 其他资产，也整次拒绝并说明问题类别，不输出部分摘要。

// replaySnapshot 为某一截止序号处的资产回看结果，全部由序号不大于 cutoff
// 的履历重放得到，不直接读取资产/工单/计划上保存的最终状态字段。
type replaySnapshot struct {
	assetID    string
	name       string
	cutoff     int
	location   string
	status     string
	openTicket *replayTicket
	plan       *replayPlan
}

// replayTicket 为截止处仍未终结工单的回看信息；报修地点为报修履历序号当时
// 的资产位置，维修中搬移不改变它。status 为截止处的工单状态：未关闭、待验收
// 或验收通过待关闭中间态；后两者都表示工单仍占用资产。待验收时
// submitSeq/submitResult 为当时待验收提交的履历序号与维修结果；验收通过待关闭
// 中间态（验收履历已纳入、紧随的关闭履历尚未纳入）时当前待验收提交为无，
// 截止之后的验收（通过或退回）与关闭不提前生效。
type replayTicket struct {
	id             string
	assignee       string
	reportLocation string
	status         string
	submitSeq      int
	submitResult   string
}

// replayPlan 为截止处的方案段状态：内容/首次到期日/间隔为当时所在段的方案，
// nextDue 为由截止前履历链推出的下一到期日。
type replayPlan struct {
	content  string
	firstDue string
	interval int
	nextDue  string
}

// replayAsset 按全库履历序号重放资产在截止序号 cutoff 处的状态。调用前
// openStore 已完成整库一致性检查；assetID 由调用方确认存在。本方法只读，
// 不修改任何业务记录。
func (s *store) replayAsset(assetID string, cutoff int) *replaySnapshot {
	a := s.findAsset(assetID)
	snap := &replaySnapshot{
		assetID:  assetID,
		name:     a.Name,
		cutoff:   cutoff,
		location: s.locationOrigin(assetID),
		status:   statusAvailable,
	}

	// 只按全库序号取前缀，不按履历时间或数组位置截取；数组乱序、序号间隔、
	// 时间不递增均不影响重放。
	events := make([]Event, 0)
	for _, e := range s.data.Events {
		if e.AssetID == assetID && e.Seq <= cutoff {
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	var openID string
	assignee := ""
	deactivated := false
	core := newMaintCore()
	// 提交验收链状态（未关闭/待验收/验收通过待关闭中间态、当前待验收提交身份）
	// 由共享规则核心按截止前履历重放，与日常操作、加载与保存校验共用同一套
	// 判定，不直接采用保存的最终状态。
	chain := newRepairCore()
	for _, e := range events {
		// 提交、验收、直接关闭、取消与报修的接续全部由规则核心判定；其余
		// 履历在核心中忽略。
		_ = chain.apply(e)
		switch e.Kind {
		case eventReport:
			// 整库一致性已保证同一资产前一张工单终结后才会产生新报修。
			openID = e.TicketID
			assignee = ""
		case eventAssign:
			if e.TicketID == openID {
				assignee = e.To
			}
		case eventClose, eventCancel:
			if e.TicketID == openID {
				openID = ""
				assignee = ""
			}
		case eventRelocate:
			snap.location = e.To
		case eventDeactivate:
			deactivated = true
		case eventReactivate:
			deactivated = false
		}
		// 保养方案段推进与停用状态由共享规则核心完成；停用只影响是否允许
		// 新完成登记（数据一致性已保证），不影响方案与到期日的展示。
		_ = core.apply(e)
	}

	switch {
	case deactivated:
		snap.status = statusDeactivated
	case openID != "":
		snap.status = statusRepairing
	default:
		snap.status = statusAvailable
	}
	if openID != "" {
		openStatus := chain.state(openID)
		submitSeq, submitResult := 0, ""
		// 仅待验收状态有当前待验收提交；验收通过待关闭中间态（验收已纳入、
		// 关闭尚未纳入）当前待验收提交为无，不能把刚通过的提交再列为待验收。
		if seq, result, ok := chain.pending(openID); ok && openStatus == ticketPending {
			submitSeq, submitResult = seq, result
		}
		snap.openTicket = &replayTicket{
			id:             openID,
			assignee:       assignee,
			reportLocation: s.locationAtSeq(assetID, s.ticketReportSeq(assetID, openID, cutoff)),
			status:         openStatus,
			submitSeq:      submitSeq,
			submitResult:   submitResult,
		}
	}
	if content, firstDue, interval, nextDue, created := core.derived(assetID); created {
		snap.plan = &replayPlan{
			content:  content,
			firstDue: firstDue,
			interval: interval,
			nextDue:  nextDue,
		}
	}
	return snap
}

// ticketReportSeq 返回工单在截止序号之前的报修履历序号（每张工单恰有一条）。
func (s *store) ticketReportSeq(assetID, ticketID string, cutoff int) int {
	seq := 0
	for _, e := range s.data.Events {
		if e.AssetID == assetID && e.TicketID == ticketID && e.Kind == eventReport &&
			e.Seq <= cutoff && e.Seq > seq {
			seq = e.Seq
		}
	}
	return seq
}

// printReplay 按固定条目输出回看摘要。
func printReplay(w io.Writer, snap *replaySnapshot) {
	fmt.Fprintf(w, "资产编号: %s\n", snap.assetID)
	fmt.Fprintf(w, "名称: %s\n", snap.name)
	fmt.Fprintf(w, "截止序号: %d\n", snap.cutoff)
	fmt.Fprintf(w, "当时位置: %s\n", snap.location)
	fmt.Fprintf(w, "当时状态: %s\n", snap.status)
	if snap.openTicket == nil {
		fmt.Fprintln(w, "当时未关闭工单: 无")
	} else {
		fmt.Fprintf(w, "当时未关闭工单: %s\n", snap.openTicket.id)
		fmt.Fprintf(w, "工单状态: %s\n", snap.openTicket.status)
		if snap.openTicket.status == ticketPending {
			fmt.Fprintf(w, "待验收提交序号: %d\n", snap.openTicket.submitSeq)
			fmt.Fprintf(w, "待验收提交结果: %s\n", snap.openTicket.submitResult)
		}
		fmt.Fprintf(w, "工单负责人: %s\n", assigneeDisplay(snap.openTicket.assignee))
		fmt.Fprintf(w, "报修地点: %s\n", snap.openTicket.reportLocation)
	}
	if snap.plan == nil {
		fmt.Fprintln(w, "当时保养计划: 无")
		return
	}
	fmt.Fprintf(w, "保养内容: %s\n", snap.plan.content)
	fmt.Fprintf(w, "首次到期日: %s\n", snap.plan.firstDue)
	fmt.Fprintf(w, "保养间隔: 每 %d 天\n", snap.plan.interval)
	fmt.Fprintf(w, "下一到期日: %s\n", snap.plan.nextDue)
}
