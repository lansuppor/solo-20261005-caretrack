package main

import (
	"fmt"
	"sort"
)

// 按履历进度回看设备状态（只读）。
//
// 回看输入资产编号与必填的非负整数截止序号，只采纳全库序号不大于截止值的
// 履历，重放资产当时的状态、位置、未关闭工单（负责人、报修地点）与保养方案
// 段（内容、首次到期日、间隔、下一到期日）。截止值不必对应现存事件：落在
// 序号间隔中仍可查询；超过全库最大序号按全部履历处理；序号 0 表示初态。
//
// 回看针对当前已登记资产的履历进度，不判断资产登记时间：编号和名称沿用登记
// 信息。序号 0 及无履历资产的初态为可用、无工单、无保养计划；初始位置取
// 最早位置变更履历的原位置，没有位置履历则取保存位置。
//
// 报修、派工、关闭、取消、停用、恢复使用与位置变更全部按全库序号重放还原，
// 不能直接采用记录中保存的最终状态或负责人：维修中搬移后位置随搬移改变，
// 旧单报修地点保持为报修当时的位置。保养建立、完成、撤销与调整采用截止处
// 的方案段：延期完成按当段起点和间隔推进，跳过周期不补记录；撤销按目标
// 完成序号恢复周期，同到期日重新完成按新序号区分；调整后采用新方案。截止
// 之后的关闭、转派、搬移、恢复、撤销或调整不得提前影响回看结果。
//
// 查询前由 openStore 完成整库一致性检查：即使矛盾位于截止之后或属于其他
// 资产，也整次拒绝并说明问题类别，不输出部分摘要。回看成功或失败都不写
// 文件、不初始化目录、不消耗编号、不改请求绑定或当前业务状态。

// replayState 为一项资产在某一截止序号当时的回看状态。全部字段都由全库
// 履历序号不大于截止值的履历重放得到，不直接采用资产或工单记录中保存的
// 最终状态、位置或负责人。
type replayState struct {
	AssetID    string
	Name       string
	Cutoff     int
	Status     string // 当时资产状态：可用 / 维修中 / 停用
	Location   string // 当时位置
	OpenTicket *replayTicket
	Plan       *replayPlan
}

// replayTicket 为截止序号当时仍未关闭的工单摘要；报修地点为报修履历序号
// 当时的资产位置，维修中搬移不改变它。
type replayTicket struct {
	ID             string
	Assignee       string // 当时最后负责人，空表示未派工
	ReportLocation string
}

// replayPlan 为截止序号当时所在方案段的保养方案与下一到期日。停用期间仍
// 展示保存的保养方案与到期日：回看不排除停用资产。
type replayPlan struct {
	Content      string
	FirstDue     string
	IntervalDays int
	NextDue      string
}

// replayAsset 按全库履历序号重放资产在 cutoff 当时的状态。cutoff 为非负
// 整数：只采纳序号不大于它的履历（序号 0 时没有任何履历，得到初态）。
// 未知资产返回 errNotFound。调用前 openStore 已完成整库一致性检查，因此
// 重放只按既有规则推进；防御性的规则冲突按内部错误返回。本方法只读。
func (s *store) replayAsset(assetID string, cutoff int) (*replayState, error) {
	asset := s.findAsset(assetID)
	if asset == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	// 初始位置：最早一条位置变更履历的原位置；没有位置履历则取保存位置。
	// 初始状态恒为可用、无工单、无计划，即使 cutoff 为 0 也一样。
	st := &replayState{
		AssetID:  asset.ID,
		Name:     asset.Name,
		Cutoff:   cutoff,
		Status:   statusAvailable,
		Location: s.locationOrigin(assetID),
	}

	// 收集序号不大于截止值的全部履历并按全库序号升序重放。不按履历时间
	// 或数组位置截取：数组乱序、序号间隔、时间不递增仍合法。
	events := make([]Event, 0)
	for _, e := range s.data.Events {
		if e.Seq <= cutoff {
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	// 维修工单状态与当时负责人由报修/派工/关闭/取消履历推出，绝不读取
	// 工单记录保存的最终状态与负责人。
	ticketOpenNow := map[string]bool{}
	ticketAssignee := map[string]string{}
	// 保养方案段与下一到期日由保养规则核心按同一套规则重放；停用状态也由
	// 核心依据停用/恢复使用履历维护（停用期间仍展示方案与到期日）。
	core := newMaintCore()

	for _, e := range events {
		// 先更新停用状态与保养派生（核心忽略无关履历），再处理本资产的
		// 工单与位置；保养完成是否发生在停用期间由核心按当时停用状态判定，
		// 合法台账不会在停用期间出现完成履历。
		if err := core.apply(e); err != nil {
			return nil, fmt.Errorf("数据内部错误：回看重放保养履历失败: %w", err)
		}
		if e.AssetID != assetID {
			// 其他资产的履历仍参与核心停用状态，但不影响本资产的工单、
			// 位置与状态。
			continue
		}
		switch e.Kind {
		case eventReport:
			ticketOpenNow[e.TicketID] = true
			ticketAssignee[e.TicketID] = ""
			st.Status = statusRepairing
		case eventAssign:
			// 合法台账已保证派工链接续且只作用于未关闭工单；回看只取当时
			// 最后负责人，不读取工单保存值。
			ticketAssignee[e.TicketID] = e.To
		case eventClose, eventCancel:
			delete(ticketOpenNow, e.TicketID)
			// 关闭/取消后资产恢复可用；停用与否以停用/恢复履历链为准，
			// 而合法台账在工单未关闭时不允许停用，故终结后当时必为可用。
			st.Status = statusAvailable
		case eventDeactivate:
			st.Status = statusDeactivated
		case eventReactivate:
			st.Status = statusAvailable
		case eventRelocate:
			// 位置随搬移改变；可用、维修中、停用资产均可变更。旧单报修
			// 地点在报修时另行记录，不随搬移改写。
			st.Location = e.To
		}
	}

	// 截止处的未关闭工单：每项资产至多一张（整库一致性已保证），按全库
	// 序号找到其报修履历，报修地点取报修序号当时的资产位置。
	var openID string
	for id := range ticketOpenNow {
		openID = id
	}
	if openID != "" {
		t := s.findTicket(openID)
		if t == nil {
			return nil, fmt.Errorf("数据内部错误：回看推出的未关闭工单 %s 不存在", openID)
		}
		reportSeq := 0
		for _, e := range s.data.Events {
			if e.TicketID == openID && e.Kind == eventReport && e.Seq <= cutoff && e.Seq > reportSeq {
				reportSeq = e.Seq
			}
		}
		st.OpenTicket = &replayTicket{
			ID:             openID,
			Assignee:       ticketAssignee[openID],
			ReportLocation: s.locationAtSeq(assetID, reportSeq),
		}
	}

	// 截止处的保养方案段：计划在建立履历之后即存在（计划记录在整库一致的
	// 台账中必然存在），撤销与调整都按履历还原。
	content, firstDue, interval, nextDue, created := core.derived(assetID)
	if created {
		st.Plan = &replayPlan{
			Content:      content,
			FirstDue:     firstDue,
			IntervalDays: interval,
			NextDue:      nextDue,
		}
	}
	return st, nil
}
