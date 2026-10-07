package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 初态：序号 0 与无履历资产为可用、无工单、无计划；位置取保存位置；
// 有位置履历时初始位置取最早位置变更履历的原位置。
func TestReplayInitialState(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼大厅"); err != nil {
		t.Fatal(err)
	}
	st, err := s.replayAsset("EQ-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != statusAvailable || st.Location != "一楼大厅" || st.OpenTicket != nil || st.Plan != nil {
		t.Fatalf("cutoff 0 初态不对: %+v", st)
	}
	// 无任何履历的资产，超大截止值仍按全部履历（即初态）处理。
	st, err = s.replayAsset("EQ-1", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != statusAvailable || st.Location != "一楼大厅" || st.OpenTicket != nil || st.Plan != nil {
		t.Fatalf("超过最大序号应按全部履历处理，仍为初态: %+v", st)
	}

	// 最早位置变更履历的原位置即初始位置：手工构造保存位置为二楼、
	// 唯一一条位置履历为 一楼 -> 二楼 的合法台账，cutoff 0 应还原为一楼。
	dir := t.TempDir()
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "二楼", "status": "可用"},
		},
		"tickets": []any{},
		"events": []any{
			map[string]any{"seq": 7, "asset_id": "EQ-1", "kind": "位置变更",
				"content": "搬楼层", "from": "一楼", "to": "二楼", "time": "2026-10-01T08:00:00Z"},
		},
		"requests":        []any{},
		"next_ticket_seq": 1,
	}
	writeRawLedger(t, dir, ledger)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	st, err = s2.replayAsset("EQ-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Location != "一楼" || st.Status != statusAvailable {
		t.Fatalf("初始位置应为最早位置履历的原位置 一楼: %+v", st)
	}
	// 截止值落在序号间隔中（1..6）仍为初态位置。
	st, _ = s2.replayAsset("EQ-1", 6)
	if st.Location != "一楼" {
		t.Fatalf("间隔中的回看应仍取初始位置 一楼，得到 %q", st.Location)
	}
	// 到达序号 7 后位置随搬移改变。
	st, _ = s2.replayAsset("EQ-1", 7)
	if st.Location != "二楼" {
		t.Fatalf("序号 7 后位置应为二楼，得到 %q", st.Location)
	}
}

// 报修、派工、转派、关闭、取消与维修中搬移全部按全库序号重放，不采用
// 记录保存的最终状态或负责人；旧单报修地点在维修中搬移后保持。
func TestReplayTicketLifecycleAndRelocate(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	reportSeq := lastEventSeq(t, s, "EQ-1")
	// 报修之前：无工单、可用，即使当前真实状态已是维修中。
	st, err := s.replayAsset("EQ-1", reportSeq-1)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != statusAvailable || st.OpenTicket != nil {
		t.Fatalf("报修前回看应为可用无工单: %+v", st)
	}
	// 报修当时：维修中、工单未派工、报修地点为一楼。
	st, _ = s.replayAsset("EQ-1", reportSeq)
	if st.Status != statusRepairing || st.OpenTicket == nil {
		t.Fatalf("报修当时应维修中且有未关闭工单: %+v", st)
	}
	if st.OpenTicket.ID != tk.ID || st.OpenTicket.Assignee != "" || st.OpenTicket.ReportLocation != "一楼" {
		t.Fatalf("报修当时工单摘要不对: %+v", st.OpenTicket)
	}

	// 派工、转派后按截止序号取当时最后负责人。
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	assignSeq := lastEventSeq(t, s, "EQ-1")
	if _, err := s.assignTicket(tk.ID, "李四", "转派"); err != nil {
		t.Fatal(err)
	}
	transferSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", assignSeq)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" {
		t.Fatalf("首次派工后负责人应为张三: %+v", st.OpenTicket)
	}
	// 截止夹在两次派工之间：转派不得提前影响回看。
	st, _ = s.replayAsset("EQ-1", transferSeq-1)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" {
		t.Fatalf("转派前的截止回看负责人应为张三: %+v", st.OpenTicket)
	}
	st, _ = s.replayAsset("EQ-1", transferSeq)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "李四" {
		t.Fatalf("转派后负责人应为李四: %+v", st.OpenTicket)
	}

	// 维修中搬移：当时位置变为二楼，旧单报修地点仍为一楼。
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "维修工位调整"); err != nil {
		t.Fatal(err)
	}
	moveSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", moveSeq)
	if st.Location != "二楼" || st.Status != statusRepairing {
		t.Fatalf("维修中搬移后应为二楼/维修中: %+v", st)
	}
	if st.OpenTicket == nil || st.OpenTicket.ReportLocation != "一楼" {
		t.Fatalf("维修中搬移不改旧单报修地点: %+v", st.OpenTicket)
	}

	// 关闭后：无未关闭工单、可用、位置保持搬移后的二楼。
	if _, _, err := s.closeTicket(tk.ID, "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	closeSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", closeSeq)
	if st.Status != statusAvailable || st.OpenTicket != nil || st.Location != "二楼" {
		t.Fatalf("关闭后应为可用/无工单/二楼: %+v", st)
	}
	// 截止在关闭之前仍看到未关闭工单。
	st, _ = s.replayAsset("EQ-1", closeSeq-1)
	if st.OpenTicket == nil || st.Status != statusRepairing {
		t.Fatalf("关闭前回看应仍有未关闭工单: %+v", st)
	}

	// 取消同样使资产恢复可用。
	tk2, _, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket(tk2.ID, "误报"); err != nil {
		t.Fatal(err)
	}
	cancelSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", cancelSeq)
	if st.Status != statusAvailable || st.OpenTicket != nil {
		t.Fatalf("取消后应为可用无工单: %+v", st)
	}
}

// 停用、恢复使用与保养计划：停用期间仍显示保存的保养方案与到期日；
// 停用/恢复只按截止序号生效。
func TestReplayDeactivationWithPlan(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	createSeq := lastEventSeq(t, s, "EQ-1")
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	deactSeq := lastEventSeq(t, s, "EQ-1")
	if _, err := s.reactivateAsset("EQ-1", "调拨回库"); err != nil {
		t.Fatal(err)
	}
	reactSeq := lastEventSeq(t, s, "EQ-1")

	// 建立之前无计划。
	st, _ := s.replayAsset("EQ-1", createSeq-1)
	if st.Plan != nil {
		t.Fatalf("建立履历之前应无计划: %+v", st.Plan)
	}
	// 停用期间仍显示保养方案与到期日，状态为停用。
	st, _ = s.replayAsset("EQ-1", deactSeq)
	if st.Status != statusDeactivated {
		t.Fatalf("停用当时状态应为停用: %+v", st)
	}
	if st.Plan == nil || st.Plan.Content != "更换滤芯" || st.Plan.NextDue != "2026-11-01" ||
		st.Plan.FirstDue != "2026-11-01" || st.Plan.IntervalDays != 90 {
		t.Fatalf("停用期间仍应显示保存的保养方案与到期日: %+v", st.Plan)
	}
	// 截止在恢复之前仍为停用，恢复后为可用。
	st, _ = s.replayAsset("EQ-1", reactSeq-1)
	if st.Status != statusDeactivated {
		t.Fatalf("恢复前回看应仍为停用: %+v", st)
	}
	st, _ = s.replayAsset("EQ-1", reactSeq)
	if st.Status != statusAvailable || st.Plan == nil {
		t.Fatalf("恢复后应为可用且计划保留: %+v", st)
	}
}

// 保养完成、延期跨周期、撤销与同到期日重新完成、调整方案段均按截止处的
// 方案段还原；截止之后的撤销、调整、重新完成不得提前影响。
func TestReplayMaintenanceSegments(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	// 延期完成：到期日 2026-01-01，实际 2026-02-10；下一到期日按当段起点
	// 与间隔推进为严格晚于完成日的最早日期（2026-03-02），跳过周期不补记录。
	_, next, doneSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-02-10", "已更换")
	if err != nil {
		t.Fatal(err)
	}
	if next != "2026-03-02" {
		t.Fatalf("延期跨周期后的下一到期日 = %q，想得到 2026-03-02", next)
	}
	st, _ := s.replayAsset("EQ-1", doneSeq)
	if st.Plan == nil || st.Plan.NextDue != "2026-03-02" || st.Plan.FirstDue != "2026-01-01" ||
		st.Plan.IntervalDays != 30 || st.Plan.Content != "更换滤芯" {
		t.Fatalf("完成后应采用推进后的下一到期日: %+v", st.Plan)
	}
	// 截止在完成之前：下一到期日仍是首次到期日。
	st, _ = s.replayAsset("EQ-1", doneSeq-1)
	if st.Plan == nil || st.Plan.NextDue != "2026-01-01" {
		t.Fatalf("完成前回看应保持首次到期日: %+v", st.Plan)
	}

	// 撤销：下一到期日恢复为目标完成的周期到期日。
	if _, target, err := s.revokeCompletion("EQ-1", doneSeq, "误登记"); err != nil {
		t.Fatal(err)
	} else if target.Due != "2026-01-01" {
		t.Fatalf("测试前置：目标周期到期日应为 2026-01-01")
	}
	revokeSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", revokeSeq)
	if st.Plan == nil || st.Plan.NextDue != "2026-01-01" {
		t.Fatalf("撤销后应恢复周期到期日 2026-01-01: %+v", st.Plan)
	}
	// 截止在撤销之前：完成仍有效，下一到期日为推进值。
	st, _ = s.replayAsset("EQ-1", revokeSeq-1)
	if st.Plan.NextDue != "2026-03-02" {
		t.Fatalf("撤销前回看完成应仍有效: %+v", st.Plan)
	}

	// 同到期日重新完成按新序号区分。
	_, next2, redoSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-02-11", "已更换")
	if err != nil {
		t.Fatal(err)
	}
	if redoSeq == doneSeq || next2 != "2026-03-02" {
		t.Fatalf("重新完成应生成新序号 %d（旧 %d），下一到期日 %q", redoSeq, doneSeq, next2)
	}

	// 调整后采用新方案；调整前的截止仍看到旧方案。
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2027-01-01", 60, "型号升级"); err != nil {
		t.Fatal(err)
	}
	adjustSeq := lastEventSeq(t, s, "EQ-1")
	st, _ = s.replayAsset("EQ-1", adjustSeq-1)
	if st.Plan.Content != "更换滤芯" || st.Plan.IntervalDays != 30 || st.Plan.NextDue != "2026-03-02" {
		t.Fatalf("调整前回看应采用旧方案段: %+v", st.Plan)
	}
	st, _ = s.replayAsset("EQ-1", adjustSeq)
	if st.Plan.Content != "更换高效滤芯" || st.Plan.FirstDue != "2027-01-01" ||
		st.Plan.IntervalDays != 60 || st.Plan.NextDue != "2027-01-01" {
		t.Fatalf("调整后应采用新方案段: %+v", st.Plan)
	}
}

// 其他资产的交错履历不影响回看：本资产只看到自己的工单与状态，全库序号
// 间隔合法。
func TestReplayInterleavedAssets(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	tk1, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	seq1 := lastEventSeq(t, s, "EQ-1")
	tk2, _, err := s.report("EQ-2", "不制冷", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	seq2 := lastEventSeq(t, s, "EQ-2")
	if _, err := s.assignTicket(tk2.ID, "王五", "派工"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk1.ID, "张三", "派工"); err != nil {
		t.Fatal(err)
	}
	maxSeq := lastEventSeq(t, s, "EQ-1")

	// EQ-1 在 seq1 之前无工单；在 seq2 时仍只有自己的未关闭工单，且尚未派工。
	st, _ := s.replayAsset("EQ-1", seq1-1)
	if st.OpenTicket != nil || st.Status != statusAvailable {
		t.Fatalf("EQ-1 报修前应为初态: %+v", st)
	}
	st, _ = s.replayAsset("EQ-1", seq2)
	if st.OpenTicket == nil || st.OpenTicket.ID != tk1.ID || st.OpenTicket.Assignee != "" {
		t.Fatalf("EQ-1 只应看到自己的未派工工单: %+v", st.OpenTicket)
	}
	st, _ = s.replayAsset("EQ-2", seq2)
	if st.OpenTicket == nil || st.OpenTicket.ID != tk2.ID {
		t.Fatalf("EQ-2 只应看到自己的工单: %+v", st.OpenTicket)
	}
	st, _ = s.replayAsset("EQ-1", maxSeq)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" {
		t.Fatalf("EQ-1 截止处负责人应为张三: %+v", st.OpenTicket)
	}
}

// 乱序存放的履历数组仍按全库序号重放，不按数组位置或时间截取。
func TestReplayUnorderedEvents(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "维修中"},
		},
		"tickets": []any{
			map[string]any{"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-1",
				"status": "未关闭", "created_at": "2026-10-01T08:00:00Z",
				"assignee": "张三", "assigned_at": "2026-10-01T07:00:00Z", "assign_note": "首次派工"},
		},
		"events": []any{
			map[string]any{"seq": 9, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "派工", "content": "首次派工", "from": "", "to": "张三", "time": "2026-10-01T07:00:00Z"},
			map[string]any{"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "报修", "content": "卡纸", "time": "2026-10-01T10:00:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
		},
		"next_ticket_seq": 2,
	}
	writeRawLedger(t, dir, ledger)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序/间隔/时间不递增的有效台账应能加载: %v", err)
	}
	// 截止 5：工单存在但派工（序号 9）尚未发生，必须显示未派工。
	st, err := s.replayAsset("EQ-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "" || st.Status != statusRepairing {
		t.Fatalf("截止 5 应为维修中且工单未派工: %+v", st)
	}
	st, _ = s.replayAsset("EQ-1", 9)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" {
		t.Fatalf("截止 9 负责人应为张三: %+v", st.OpenTicket)
	}
}

// 有效旧库直接使用：baseLedger 的工单为未关闭、资产保存为维修中、报修履历
// 序号 3；cutoff 0 必须按履历还原为可用（不采用保存的最终状态），cutoff 3
// 为维修中、T0001 未派工、报修地点为一楼。
func TestReplayLegacyLedger(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil)
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.replayAsset("EQ-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != statusAvailable || st.OpenTicket != nil || st.Location != "一楼" {
		t.Fatalf("旧库 cutoff 0 应按履历还原为初态，不采用保存状态: %+v", st)
	}
	st, _ = s.replayAsset("EQ-1", 3)
	if st.Status != statusRepairing || st.OpenTicket == nil || st.OpenTicket.ID != "T0001" ||
		st.OpenTicket.Assignee != "" || st.OpenTicket.ReportLocation != "一楼" {
		t.Fatalf("旧库 cutoff 3 应为维修中/T0001/未派工/一楼: %+v", st)
	}
}

// 矛盾位于截止之后：即使截止序号只覆盖合法前缀，整次回看仍被拒绝。
func TestReplayRejectsContradictionAfterCutoff(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "维修中"},
		},
		"tickets": []any{
			// 工单保存为未关闭，却存在一条关闭履历（序号 10）：整库矛盾。
			map[string]any{"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-1",
				"status": "未关闭", "created_at": "2026-10-01T08:00:00Z"},
		},
		"events": []any{
			map[string]any{"seq": 3, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
			map[string]any{"seq": 10, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
		},
		"next_ticket_seq": 2,
	}
	writeRawLedger(t, dir, ledger)
	var out, errBuf bytes.Buffer
	// 截止 3 只覆盖报修这一合法前缀，但整库一致性检查失败，必须整次拒绝。
	if code := run([]string{"replay", "--data-dir", dir, "--asset-id", "EQ-1", "--cutoff", "3"},
		&out, &errBuf); code != 1 {
		t.Fatalf("截止之后存在矛盾也应退出 1，得到 %d；stderr=%s", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Fatalf("矛盾时不得输出部分摘要: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "矛盾") {
		t.Fatalf("错误信息应说明问题类别: %s", errBuf.String())
	}
}

// 矛盾位于其他资产：即使回看 EQ-1，也整次拒绝。
func TestReplayRejectsContradictionOnOtherAsset(t *testing.T) {
	dir := t.TempDir()
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "可用"},
			map[string]any{"id": "EQ-2", "name": "空调", "location": "二楼", "status": "维修中"},
		},
		"tickets": []any{
			map[string]any{"id": "T0009", "asset_id": "EQ-2", "description": "不制冷", "request_id": "req-2",
				"status": "未关闭", "created_at": "2026-10-01T08:00:00Z"},
		},
		"events": []any{
			// EQ-2 一张未关闭工单却有两条报修履历：履历矛盾。
			map[string]any{"seq": 1, "asset_id": "EQ-2", "ticket_id": "T0009",
				"kind": "报修", "content": "不制冷", "time": "2026-10-01T08:00:00Z"},
			map[string]any{"seq": 2, "asset_id": "EQ-2", "ticket_id": "T0009",
				"kind": "报修", "content": "不制冷", "time": "2026-10-01T08:01:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-2", "asset_id": "EQ-2", "description": "不制冷", "ticket_id": "T0009"},
		},
		"next_ticket_seq": 10,
	}
	writeRawLedger(t, dir, ledger)
	var out, errBuf bytes.Buffer
	if code := run([]string{"replay", "--data-dir", dir, "--asset-id", "EQ-1", "--cutoff", "0"},
		&out, &errBuf); code != 1 {
		t.Fatalf("其他资产矛盾也应整次拒绝，得到 %d；stderr=%s", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Fatalf("矛盾时不得输出部分摘要: %q", out.String())
	}
}

// CLI：参数错误退出 2；未知资产退出 1；成功输出各字段；只读不初始化目录。
func TestReplayCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	runCode := func(args ...string) int {
		out.Reset()
		errBuf.Reset()
		return run(args, &out, &errBuf)
	}

	// 目录尚不存在时的参数错误退出 2，且不初始化目录。
	missing := filepath.Join(t.TempDir(), "not-created")
	if code := runCode("replay", "--data-dir", missing, "--asset-id", "EQ-1"); code != 2 {
		t.Fatalf("缺少 --cutoff 应退出 2，得到 %d", code)
	}
	if code := runCode("replay", "--data-dir", missing, "--cutoff", "0"); code != 2 {
		t.Fatalf("缺少 --asset-id 应退出 2，得到 %d", code)
	}
	if code := runCode("replay", "--data-dir", missing, "--asset-id", "EQ-1", "--cutoff", "-1"); code != 2 {
		t.Fatalf("负截止序号应退出 2，得到 %d", code)
	}
	if code := runCode("replay", "--data-dir", missing, "--asset-id", "EQ-1", "--cutoff", "x"); code != 2 {
		t.Fatalf("非整数截止序号应退出 2，得到 %d", code)
	}
	if code := runCode("replay", "--data-dir", missing, "--asset-id", "EQ-1", "--cutoff", "0", "extra"); code != 2 {
		t.Fatalf("位置参数应退出 2，得到 %d", code)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("参数错误不应初始化数据目录: %v", err)
	}
	// 未知资产退出 1，仍不初始化目录。
	if code := runCode("replay", "--data-dir", missing, "--asset-id", "NOPE", "--cutoff", "0"); code != 1 {
		t.Fatalf("未知资产应退出 1，得到 %d；stderr=%s", code, errBuf.String())
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("失败查询不应初始化数据目录: %v", err)
	}

	// 建一份真实台账：报修、派工、建立计划、搬移。
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-001", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-001", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-001", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-001", "二楼", "维修中搬移"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	if code := runCode("replay", "--data-dir", dir, "--asset-id", "EQ-001", "--cutoff", "0"); code != 0 {
		t.Fatalf("cutoff 0 查询应成功，得到 %d；stderr=%s", code, errBuf.String())
	}
	o := out.String()
	for _, want := range []string{"资产编号: EQ-001", "名称: 打印机", "截止序号: 0",
		"当时位置: 一楼", "当时状态: 可用", "当时未关闭工单: 无", "当时保养计划: 无"} {
		if !strings.Contains(o, want) {
			t.Fatalf("cutoff 0 输出缺少 %q:\n%s", want, o)
		}
	}

	// 截止到全部履历：维修中、二楼、工单负责人张三、报修地点一楼、计划展示。
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if code := runCode("replay", "--data-dir", dir, "--asset-id", "EQ-001", "--cutoff", "999999"); code != 0 {
		t.Fatalf("超过最大序号查询应成功，得到 %d；stderr=%s", code, errBuf.String())
	}
	o = out.String()
	for _, want := range []string{"截止序号: 999999", "当时位置: 二楼", "当时状态: 维修中",
		"当时未关闭工单: " + tk.ID, "工单负责人: 张三", "报修地点: 一楼",
		"保养内容: 更换滤芯", "首次到期日: 2026-11-01", "保养间隔: 每 90 天",
		"下一到期日: 2026-11-01"} {
		if !strings.Contains(o, want) {
			t.Fatalf("全履历输出缺少 %q:\n%s", want, o)
		}
	}
	// 无未派工字样（已派工）；未派工场景单独验证。
	if strings.Contains(o, "未派工") {
		t.Fatalf("已派工工单不应显示未派工:\n%s", o)
	}

	// 成功查询不写文件、不改业务状态。
	if got := readFileBytes(t, dir); !bytes.Equal(got, before) {
		t.Fatal("只读回看不应改动台账字节")
	}

	// 未派工显示“未派工”：截止在报修与派工之间。
	reportSeq := 0
	for _, e := range s.data.Events {
		if e.Kind == eventReport && e.AssetID == "EQ-001" {
			reportSeq = e.Seq
		}
	}
	if code := runCode("replay", "--data-dir", dir, "--asset-id", "EQ-001",
		"--cutoff", itoa(reportSeq)); code != 0 {
		t.Fatalf("截止报修序号查询应成功，得到 %d", code)
	}
	if !strings.Contains(out.String(), "工单负责人: 未派工") {
		t.Fatalf("未派工应显示“未派工”:\n%s", out.String())
	}
}

// history 每条事件补充全库序号，保留原有内容与排序。
func TestHistoryShowsGlobalSeq(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	var buf bytes.Buffer
	if err := cmdHistory([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	o := buf.String()
	events := s.eventsOf("EQ-1")
	for _, e := range events {
		if !strings.Contains(o, "#"+itoa(e.Seq)) {
			t.Fatalf("history 应标注全库序号 #%d:\n%s", e.Seq, o)
		}
	}
	// 原有内容与排序保持。
	if !strings.Contains(o, "报修 工单 "+tk.ID) || !strings.Contains(o, "卡纸") ||
		!strings.Contains(o, "未派工 -> 张三") {
		t.Fatalf("history 原有内容应保留:\n%s", o)
	}
	if strings.Index(o, "#"+itoa(events[0].Seq)) > strings.Index(o, "#"+itoa(events[1].Seq)) {
		t.Fatalf("history 排序应保持序号升序:\n%s", o)
	}
}

// import 后按目标履历序号与映射后的工单编号回看。
func TestReplayAfterImport(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	// 目标库先放一张工单，占用 T0001 与履历序号 1..3，使导入发生重编号。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	xt, _, err := dst.report("EQ-X", "漏油", "req-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.closeTicket(xt.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	st1, _, err := src.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	srcReportSeq := lastEventSeq(t, src, "EQ-1")
	if _, err := src.assignTicket(st1.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	mapped := outcome.tickets[0].NewID
	if mapped == st1.ID {
		t.Fatalf("工单应被重编号，却仍为 %s", mapped)
	}

	s2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// 找到导入后报修履历的新序号。
	newReportSeq := 0
	for _, e := range s2.data.Events {
		if e.AssetID == "EQ-1" && e.Kind == eventReport {
			newReportSeq = e.Seq
		}
	}
	// 目标原有 2 条履历（EQ-X 报修、关闭），导入履历从序号 3 起重编号。
	if newReportSeq <= 2 || newReportSeq != srcReportSeq+2 {
		t.Fatalf("导入履历应在目标最大序号之后重编号，得到 %d", newReportSeq)
	}
	// 截止映射后的报修序号：维修中，工单为映射编号且未派工（派工履历更晚）。
	st, err := s2.replayAsset("EQ-1", newReportSeq)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != statusRepairing || st.OpenTicket == nil || st.OpenTicket.ID != mapped ||
		st.OpenTicket.Assignee != "" || st.OpenTicket.ReportLocation != "一楼" {
		t.Fatalf("导入后按目标序号回看应得映射工单: %+v", st.OpenTicket)
	}
	// 截止 0：初态，不受导入履历影响。
	st, _ = s2.replayAsset("EQ-1", 0)
	if st.Status != statusAvailable || st.OpenTicket != nil || st.Plan != nil {
		t.Fatalf("导入资产 cutoff 0 应为初态: %+v", st)
	}
	// 截止到全部：负责人张三、计划存在。
	st, _ = s2.replayAsset("EQ-1", 1_000_000)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" || st.Plan == nil {
		t.Fatalf("导入资产全履历回看应见负责人与计划: %+v", st)
	}
}

// restore 后按保留的全库序号回看，无需新增业务履历。
func TestReplayAfterRestore(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	s := newStoreAt(t, srcDir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	reportSeq, assignSeq := 0, 0
	for _, e := range s.data.Events {
		switch e.Kind {
		case eventReport:
			reportSeq = e.Seq
		case eventAssign:
			assignSeq = e.Seq
		}
	}

	pkgPath := filepath.Join(t.TempDir(), "eq1.zip")
	if _, err := exportPackage(srcDir, pkgPath, []string{"EQ-1"}); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := restorePackage(pkgPath, target); err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	s2, err := openStore(target)
	if err != nil {
		t.Fatal(err)
	}
	// 序号原样保留：报修截止看到未派工，派工截止看到张三。
	st, err := s2.replayAsset("EQ-1", reportSeq)
	if err != nil {
		t.Fatal(err)
	}
	if st.OpenTicket == nil || st.OpenTicket.ID != tk.ID || st.OpenTicket.Assignee != "" {
		t.Fatalf("还原后按保留序号回看报修点: %+v", st.OpenTicket)
	}
	st, _ = s2.replayAsset("EQ-1", assignSeq)
	if st.OpenTicket == nil || st.OpenTicket.Assignee != "张三" {
		t.Fatalf("还原后按保留序号回看派工点应为张三: %+v", st.OpenTicket)
	}
}

// lastEventSeq 返回资产最近一条（按全库序号）履历的序号。
func lastEventSeq(t *testing.T, s *store, assetID string) int {
	t.Helper()
	events := s.eventsOf(assetID)
	if len(events) == 0 {
		t.Fatalf("资产 %s 没有履历", assetID)
	}
	return events[len(events)-1].Seq
}

// writeRawLedger 把任意台账映射写入数据目录。
func writeRawLedger(t *testing.T, dir string, m map[string]any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// itoa 返回非负整数的十进制字符串。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
