package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustPlan / mustComplete 是测试里的便捷封装。
func mustPlan(t *testing.T, s *store, assetID, content, firstDue string, interval int) {
	t.Helper()
	if _, err := s.createPlan(assetID, content, firstDue, interval); err != nil {
		t.Fatalf("createPlan(%s): %v", assetID, err)
	}
}

func mustComplete(t *testing.T, s *store, assetID, periodDue, doneDate, result string) (string, string) {
	t.Helper()
	_, due, next, err := s.completeMaintenance(assetID, periodDue, doneDate, result)
	if err != nil {
		t.Fatalf("completeMaintenance(%s %s): %v", assetID, periodDue, err)
	}
	return due, next
}

// 基本建立与推进：首次到期、下一到期日按日历日推算，严格晚于完成日。
func TestPlanCreateAndComplete(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}

	// 未知资产拒绝。
	if _, err := s.createPlan("NOPE", "更换滤芯", "2026-11-01", 30); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	// 间隔非正、日期非法拒绝。
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 0); err == nil {
		t.Fatal("间隔 0 应拒绝")
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-02-30", 30); err == nil {
		t.Fatal("非法日期应拒绝")
	}
	if len(s.data.Plans) != 0 || len(s.data.Events) != 0 {
		t.Fatal("失败的建立不应留下计划或履历")
	}

	mustPlan(t, s, "EQ-1", "更换滤芯", "2026-11-01", 30)
	if got := len(s.data.Plans); got != 1 {
		t.Fatalf("计划数 = %d, 想得到 1", got)
	}
	// 计划不可覆盖。
	if _, err := s.createPlan("EQ-1", "别的保养", "2026-12-01", 10); !errors.Is(err, errConflict) {
		t.Fatalf("已有计划应冲突，得到 %v", err)
	}
	// 建立履历恰一条。
	ev := s.eventsOf("EQ-1")
	if len(ev) != 1 || ev[0].Kind != eventPlanCreate || ev[0].Due != "2026-11-01" ||
		ev[0].Content != "更换滤芯" || ev[0].TicketID != "" || ev[0].Done != "" {
		t.Fatalf("建立履历不正确: %+v", ev)
	}

	p := s.findPlan("EQ-1")
	if next, err := s.planNextDue(p); err != nil || next != "2026-11-01" {
		t.Fatalf("初始下一到期日 = %q, %v", next, err)
	}

	// 无计划资产完成登记拒绝。
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completeMaintenance("EQ-2", "2026-11-01", "2026-11-01", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("无计划完成应冲突，得到 %v", err)
	}
	if _, _, _, err := s.completeMaintenance("NOPE", "2026-11-01", "2026-11-01", "结果"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产完成应未找到，得到 %v", err)
	}

	// 到期日不匹配（提前完成下一周期 / 填错日期）拒绝。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-12-01", "2026-12-01", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("非当前到期日应冲突，得到 %v", err)
	}
	// 完成日早于到期日拒绝。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-11-01", "2026-10-31", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("完成日早于到期日应冲突，得到 %v", err)
	}
	if len(s.data.Events) != 1 {
		t.Fatal("失败的完成登记不应产生履历")
	}

	// 按期完成：下一到期日严格晚于完成日。11-01 完成，下一周期 12-01。
	due, next := mustComplete(t, s, "EQ-1", "2026-11-01", "2026-11-01", "已更换")
	if due != "2026-11-01" || next != "2026-12-01" {
		t.Fatalf("完成输出 = %s -> %s，想得到 2026-11-01 -> 2026-12-01", due, next)
	}
	done := s.planDoneEvents("EQ-1")
	if len(done) != 1 || done[0].Due != "2026-11-01" || done[0].Done != "2026-11-01" ||
		done[0].Content != "已更换" || done[0].TicketID != "" {
		t.Fatalf("完成履历不正确: %+v", done)
	}
	// 旧周期重复登记拒绝。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-11-01", "2026-11-02", "再来一次"); !errors.Is(err, errConflict) {
		t.Fatalf("旧周期重复应冲突，得到 %v", err)
	}
	if len(s.data.Events) != 2 {
		t.Fatal("旧周期重复不应产生履历")
	}
}

// 延期：跨过的周期不生成完成记录，下一到期日锚定首次到期日的整数倍间隔，
// 而不是完成日加间隔。
func TestPlanDeferredCompletionSkipsPeriods(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "水泵", "机房")
	mustPlan(t, s, "EQ-1", "润滑", "2026-01-10", 30)

	// 周期：01-10, 02-09, 03-11, 04-10 ... 完成日 03-20（晚于 03-11，早于 04-10）。
	// 必须用当前下一到期日 01-10 登记（跨两个周期只产生一条完成记录）。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-02-09", "2026-03-20", "延期完成"); !errors.Is(err, errConflict) {
		t.Fatalf("非当前到期日（02-09）应拒绝，得到 %v", err)
	}
	_, next := mustComplete(t, s, "EQ-1", "2026-01-10", "2026-03-20", "延期完成")
	if next != "2026-04-10" {
		t.Fatalf("延期后下一到期日 = %s，想得到 2026-04-10（锚定首次到期日，非完成日+间隔）", next)
	}
	if got := len(s.planDoneEvents("EQ-1")); got != 1 {
		t.Fatalf("跨过的周期不应生成完成记录，完成履历数 = %d", got)
	}
	// 下一周期按新到期日继续；04-10 + 30 天 = 05-10。
	_, next = mustComplete(t, s, "EQ-1", "2026-04-10", "2026-04-10", "按期")
	if next != "2026-05-10" {
		t.Fatalf("再下一周期 = %s，想得到 2026-05-10", next)
	}
}

// 闰日与不同月份长度：按日历日推进；落在闰日的周期在平年归一到 2-28 或 3-1。
func TestPlanLeapDayCalendarArithmetic(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("A", "设备A", "一楼")
	_, _ = s.registerAsset("B", "设备B", "一楼")
	// 首次到期 2024-02-29（闰日），间隔 365 天：
	// 2025-02-28（365 天后），2026-02-28。
	mustPlan(t, s, "A", "年检", "2024-02-29", 365)
	_, next := mustComplete(t, s, "A", "2024-02-29", "2024-02-29", "完成")
	if next != "2025-02-28" {
		t.Fatalf("闰日 +365 = %s，想得到 2025-02-28", next)
	}
	_, next = mustComplete(t, s, "A", "2025-02-28", "2025-03-01", "完成")
	if next != "2026-02-28" {
		t.Fatalf("2025-02-28 +365 = %s，想得到 2026-02-28", next)
	}

	// 按月末附近、间隔一个月（28~31 天）检查按日历日而非固定 30 天。
	mustPlan(t, s, "B", "保养", "2024-01-31", 29)
	p := s.findPlan("B")
	if got, _ := s.planNextDue(p); got != "2024-01-31" {
		t.Fatalf("初始 = %s", got)
	}
	_, next = mustComplete(t, s, "B", "2024-01-31", "2024-01-31", "x")
	if next != "2024-02-29" {
		t.Fatalf("01-31 +29 = %s，想得到 2024-02-29（闰日）", next)
	}
}

// 到期查询：排序（先到期日再资产编号）、边界“不晚于”含当日、无匹配提示、只读。
func TestDueQuerySortingAndReadOnly(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-3", "泵", "机房")
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	_, _ = s.registerAsset("EQ-2", "空调", "二楼")
	mustPlan(t, s, "EQ-1", "更换滤芯", "2026-11-01", 30)
	mustPlan(t, s, "EQ-2", "清洗", "2026-10-01", 10)
	mustPlan(t, s, "EQ-3", "润滑", "2026-11-01", 90)

	rows, err := s.duePlans("2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	want := []dueRow{
		{AssetID: "EQ-2", Name: "空调", Content: "清洗", Due: "2026-10-01"},
		{AssetID: "EQ-1", Name: "打印机", Content: "更换滤芯", Due: "2026-11-01"},
		{AssetID: "EQ-3", Name: "泵", Content: "润滑", Due: "2026-11-01"},
	}
	if len(rows) != len(want) {
		t.Fatalf("行数 = %d, 想得到 %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("rows[%d] = %+v, 想得到 %+v", i, rows[i], want[i])
		}
	}

	// EQ-2 完成后推进到 10-11；查询 10-10 不含它。
	mustComplete(t, s, "EQ-2", "2026-10-01", "2026-10-05", "已清洗")
	rows, _ = s.duePlans("2026-10-10")
	if len(rows) != 0 {
		t.Fatalf("10-10 应无匹配，得到 %+v", rows)
	}
	rows, _ = s.duePlans("2026-10-11")
	if len(rows) != 1 || rows[0].AssetID != "EQ-2" || rows[0].Due != "2026-10-11" {
		t.Fatalf("10-11 应仅含 EQ-2 的 10-11，得到 %+v", rows)
	}
}

// 维修与保养交错：维修中的资产可安排/完成保养，工单、资产状态、请求绑定、
// 工单编号与停机统计均不改变；履历序号全库唯一并按序号共同展示。
func TestMaintenanceInterleavedWithRepair(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	// 先安排保养，再报修进入维修中。
	mustPlan(t, s, "EQ-1", "更换滤芯", "2026-11-01", 30)
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != "T0001" || s.findAsset("EQ-1").Status != statusRepairing {
		t.Fatalf("报修后状态/编号异常: %+v", tk)
	}
	beforeTickets := len(s.data.Tickets)
	beforeNextSeq := s.data.NextTicketSeq
	beforeBindings := len(s.data.Requests)

	// 维修中完成保养：成功，但不动工单体系。
	_, next := mustComplete(t, s, "EQ-1", "2026-11-01", "2026-11-02", "维修期间顺带保养")
	if next != "2026-12-01" {
		t.Fatalf("下一到期日 = %s", next)
	}
	if s.findAsset("EQ-1").Status != statusRepairing {
		t.Fatal("保养完成不应改变资产状态")
	}
	if cur := s.findTicket("T0001"); cur.Status != ticketOpen {
		t.Fatalf("保养不应改变工单状态: %s", cur.Status)
	}
	if len(s.data.Tickets) != beforeTickets || s.data.NextTicketSeq != beforeNextSeq ||
		len(s.data.Requests) != beforeBindings {
		t.Fatal("保养不应创建工单、消耗编号或改变请求绑定")
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatal("未关闭工单应仍在")
	}

	// 维修中再安排计划到另一台资产，互不影响；关闭工单后资产恢复可用，计划保留。
	_, _ = s.registerAsset("EQ-2", "空调", "二楼")
	mustPlan(t, s, "EQ-2", "清洗", "2026-12-01", 14)
	closed, asset, err := s.closeTicket("T0001", "已更换搓纸轮")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != ticketClosed || asset.Status != statusAvailable {
		t.Fatal("工单关闭行为异常")
	}
	p := s.findPlan("EQ-1")
	if p == nil || p.Content != "更换滤芯" {
		t.Fatalf("工单关闭不应影响保养计划: %+v", p)
	}
	if got, _ := s.planNextDue(p); got != "2026-12-01" {
		t.Fatalf("下一到期日 = %s，仍应为 2026-12-01", got)
	}

	// 履历序号全库唯一且连续，按序号混合展示。
	ev := s.eventsOf("EQ-1")
	wantKinds := []string{eventPlanCreate, eventReport, eventMaintDone, eventClose}
	if len(ev) != len(wantKinds) {
		t.Fatalf("EQ-1 履历数 = %d, 想得到 %d", len(ev), len(wantKinds))
	}
	for i, k := range wantKinds {
		if ev[i].Kind != k {
			t.Fatalf("履历[%d] = %s, 想得到 %s: %+v", i, ev[i].Kind, k, ev)
		}
		if i > 0 && ev[i].Seq <= ev[i-1].Seq {
			t.Fatalf("履历序号应严格递增: %+v", ev)
		}
	}

	// 持久化后仍自洽。
	s = saveAndReopen(t, s)
	if got, _ := s.planNextDue(s.findPlan("EQ-1")); got != "2026-12-01" {
		t.Fatalf("重载后下一到期日 = %s", got)
	}
}

// 下一到期日超出 9999-12-31：整次拒绝，不推进、不留履历，恢复条件后可重试。
func TestPlanNextDueOutOfRangeRejects(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "设备", "一楼")
	mustPlan(t, s, "EQ-1", "保养", "9999-12-30", 2)
	// 9999-12-30 完成：严格晚于完成日的下一是 10000-01-01（越界），整次拒绝。
	_, _, _, err := s.completeMaintenance("EQ-1", "9999-12-30", "9999-12-30", "结果")
	if err == nil || !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "9999-12-31") {
		t.Fatalf("越界应冲突拒绝并提示日期范围，得到 %v", err)
	}
	if got := len(s.data.Events); got != 1 {
		t.Fatalf("拒绝后不应新增完成履历，履历数 = %d", got)
	}
	p := s.findPlan("EQ-1")
	if got, _ := s.planNextDue(p); got != "9999-12-30" {
		t.Fatalf("拒绝不应推进下一到期日，得到 %s", got)
	}
	// 完成日早于到期日同样拒绝。
	if _, _, _, err := s.completeMaintenance("EQ-1", "9999-12-30", "9999-12-29", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("完成日早于到期日应冲突，得到 %v", err)
	}
	// 间隔 1 天时在边界仍可登记：9999-12-30 完成，下一是 9999-12-31，合法。
	_, _ = s.registerAsset("EQ-2", "设备2", "二楼")
	mustPlan(t, s, "EQ-2", "保养", "9999-12-30", 1)
	_, next := mustComplete(t, s, "EQ-2", "9999-12-30", "9999-12-30", "结果")
	if next != "9999-12-31" {
		t.Fatalf("边界内下一到期日 = %s，想得到 9999-12-31", next)
	}
}

// 无保养字段的有效旧库直接使用；首次保存后补上空段且旧库字节语义不变。
func TestLegacyStoreWithoutPlans(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // 既有 baseLedger：无 plans 段
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无保养字段旧库应能加载: %v", err)
	}
	if len(s.data.Plans) != 0 {
		t.Fatalf("旧库应无计划，得到 %d", len(s.data.Plans))
	}
	// 可直接安排与完成保养。
	mustPlan(t, s, "EQ-1", "更换滤芯", "2026-11-01", 30)
	mustComplete(t, s, "EQ-1", "2026-11-01", "2026-11-02", "已更换")
	if err := s.save(); err != nil {
		t.Fatalf("保存: %v", err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if got, _ := s2.planNextDue(s2.findPlan("EQ-1")); got != "2026-12-01" {
		t.Fatalf("重载后下一到期日 = %s", got)
	}
	// 旧库的工单/去重/编号延续不受影响。
	old, replay, err := s2.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" {
		t.Fatalf("旧库去重应保持: %v replay=%v err=%v", old, replay, err)
	}
}

// 重启后：计划、完成链与下一到期日保持，旧周期仍不能重复。
func TestPlanPersistenceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "水泵", "机房")
	mustPlan(t, s, "EQ-1", "润滑", "2026-01-10", 30)
	mustComplete(t, s, "EQ-1", "2026-01-10", "2026-03-20", "延期完成") // 01-10 +90 = 04-10
	s = saveAndReopen(t, s)
	p := s.findPlan("EQ-1")
	if p == nil || p.FirstDue != "2026-01-10" || p.IntervalDays != 30 || p.Content != "润滑" {
		t.Fatalf("重载后计划字段不对: %+v", p)
	}
	if got, _ := s.planNextDue(p); got != "2026-04-10" {
		t.Fatalf("重载后下一到期日 = %s，想得到 2026-04-10", got)
	}
	// 旧周期 01-10 仍拒绝；当前 04-10 可完成。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-01-10", "2026-04-10", "重复"); !errors.Is(err, errConflict) {
		t.Fatalf("重载后旧周期仍应拒绝，得到 %v", err)
	}
	_, next := mustComplete(t, s, "EQ-1", "2026-04-10", "2026-04-10", "按期")
	if next != "2026-05-10" {
		t.Fatalf("下一周期 = %s，想得到 2026-05-10", next)
	}
}

// 导入：复制所选资产的计划与保养履历，内容、日期与完成链续接，履历序号重排；
// 目标已有资产/计划不变，冲突整批拒绝，重启后保持。
func TestImportCopiesPlansAndHistory(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	_, _ = src.registerAsset("EQ-A", "打印机", "一楼")
	_, _ = src.registerAsset("EQ-B", "空调", "二楼")
	mustPlan(t, src, "EQ-A", "更换滤芯", "2026-01-10", 30)
	mustComplete(t, src, "EQ-A", "2026-01-10", "2026-03-20", "延期完成") // 01-10 +90 = 04-10
	mustComplete(t, src, "EQ-A", "2026-04-10", "2026-04-10", "按期")   // +30 = 05-10
	mustPlan(t, src, "EQ-B", "清洗", "2026-02-01", 14)
	mustSave(t, src)

	// 目标已有资产 EQ-X 带一张工单（占用履历序号 1），且也有一个保养计划。
	dst := newStoreAt(t, dstDir)
	_, _ = dst.registerAsset("EQ-X", "叉车", "仓库")
	_, _, _ = dst.report("EQ-X", "抖动", "req-x1")
	mustPlan(t, dst, "EQ-X", "点检", "2026-03-01", 7)
	mustSave(t, dst)

	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A", "EQ-B"}); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	// 计划复制且完成链续接：EQ-A 下一到期日 2026-05-10。
	pa := s.findPlan("EQ-A")
	if pa == nil || pa.Content != "更换滤芯" || pa.FirstDue != "2026-01-10" || pa.IntervalDays != 30 {
		t.Fatalf("EQ-A 计划应原样复制，得到 %+v", pa)
	}
	if got, _ := s.planNextDue(pa); got != "2026-05-10" {
		t.Fatalf("EQ-A 完成链应续接到 2026-05-10，得到 %s", got)
	}
	pb := s.findPlan("EQ-B")
	if pb == nil || pb.FirstDue != "2026-02-01" {
		t.Fatalf("EQ-B 计划应复制，得到 %+v", pb)
	}
	// 目标既有计划不变。
	px := s.findPlan("EQ-X")
	if px == nil || px.Content != "点检" {
		t.Fatalf("目标 EQ-X 计划不应改变，得到 %+v", px)
	}
	// EQ-A 可继续完成导入链上的当前周期；旧周期仍拒绝。
	if _, _, _, err := s.completeMaintenance("EQ-A", "2026-04-10", "2026-05-10", "重复"); !errors.Is(err, errConflict) {
		t.Fatalf("导入后旧周期应拒绝，得到 %v", err)
	}
	_, next := mustComplete(t, s, "EQ-A", "2026-05-10", "2026-05-10", "继续")
	if next != "2026-06-09" {
		t.Fatalf("导入链再推进 = %s，想得到 2026-06-09", next)
	}
	mustSave(t, s)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("再次保存后应仍自洽: %v", err)
	}

	// 履历序号全库唯一：导入履历在目标最大序号之后重排，且保养履历一并迁入。
	seqs := map[int]bool{}
	for _, e := range s.data.Events {
		if seqs[e.Seq] {
			t.Fatalf("履历序号 %d 在导入后重复", e.Seq)
		}
		seqs[e.Seq] = true
	}
	if got := len(s.planDoneEvents("EQ-A")); got != 3 {
		t.Fatalf("EQ-A 应迁入 2 条完成履历并在目标续登 1 条，得到 %d", got)
	}

	// 源台账只读不变。
	src2, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := src2.planNextDue(src2.findPlan("EQ-A")); got != "2026-05-10" {
		t.Fatalf("源台账不应被修改，EQ-A 下一日 = %s", got)
	}

	// 再次导入已存在资产：整批冲突拒绝。
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A"}); !errors.Is(err, errConflict) {
		t.Fatalf("再次导入应冲突，得到 %v", err)
	}
}

// 矛盾的计划/完成台账在加载时报错（指出类别），原文件保留，不自动修复。
func TestContradictoryPlanLedgersRejected(t *testing.T) {
	planEvent := func(seq int, firstDue string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": "",
			"kind": eventPlanCreate, "content": "更换滤芯", "due": firstDue,
			"time": "2026-10-01T10:00:00Z",
		}
	}
	doneEvent := func(seq int, due, done string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": "",
			"kind": eventMaintDone, "content": "已更换", "due": due, "done": done,
			"time": "2026-10-02T10:00:00Z",
		}
	}
	planRecord := func() map[string]any {
		return map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": "2026-11-01", "interval_days": 30,
		}
	}
	withPlan := func(fn func(m map[string]any)) func(m map[string]any) {
		return func(m map[string]any) {
			// baseLedger 中 EQ-1 有一张未关闭工单；保养履历与工单履历并存合法。
			m["plans"] = []any{planRecord()}
			fn(m)
		}
	}

	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"计划缺少建立履历", withPlan(func(m map[string]any) {}), "保养计划矛盾"},
		{"有建立履历但缺计划记录", func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
		}, "保养计划矛盾"},
		{"同一资产两个计划", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
			appendTo(m, "plans", planRecord())
		}), "保养计划矛盾"},
		{"间隔为零", withPlan(func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["interval_days"] = 0
			appendTo(m, "events", planEvent(4, "2026-11-01"))
		}), "保养计划矛盾"},
		{"首次到期日非法", withPlan(func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["first_due"] = "2026-02-30"
			appendTo(m, "events", planEvent(4, "2026-02-30"))
		}), "履历矛盾"},
		{"计划内容为空", withPlan(func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["content"] = ""
			appendTo(m, "events", planEvent(4, "2026-11-01"))
		}), "保养计划矛盾"},
		{"建立履历首次到期日与计划不一致", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-12-01"))
		}), "保养计划矛盾"},
		{"建立履历内容与计划不一致", withPlan(func(m map[string]any) {
			ev := planEvent(4, "2026-11-01")
			ev["content"] = "别的保养"
			appendTo(m, "events", ev)
		}), "保养计划矛盾"},
		{"完成履历日期非法", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
			appendTo(m, "events", doneEvent(5, "2026-11-01", "2026-11-31"))
		}), "履历矛盾"},
		{"完成日早于周期到期日", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
			appendTo(m, "events", doneEvent(5, "2026-11-01", "2026-10-31"))
		}), "保养计划矛盾"},
		{"旧周期重复完成（链不接续）", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
			appendTo(m, "events", doneEvent(5, "2026-11-01", "2026-11-01")) // 下一 12-01
			appendTo(m, "events", doneEvent(6, "2026-11-01", "2026-12-02")) // 重复旧周期
		}), "保养计划矛盾"},
		{"完成履历出现在建立之前", withPlan(func(m map[string]any) {
			// 完成履历（序号 4）先于建立履历（序号 5）；按数组顺序重放时
			// 完成出现在建立之前，拒绝。
			appendTo(m, "events", doneEvent(4, "2026-11-01", "2026-11-01"))
			appendTo(m, "events", planEvent(5, "2026-11-01"))
		}), "保养计划矛盾"},
		{"无计划却有完成履历", func(m map[string]any) {
			// 不添加 plans 段与建立履历，仅有一条完成履历。
			appendTo(m, "events", doneEvent(4, "2026-11-01", "2026-11-01"))
		}, "保养计划矛盾"},
		{"完成履历带工单编号", withPlan(func(m map[string]any) {
			appendTo(m, "events", planEvent(4, "2026-11-01"))
			ev := doneEvent(5, "2026-11-01", "2026-11-01")
			ev["ticket_id"] = "T0001"
			appendTo(m, "events", ev)
		}), "履历矛盾"},
		{"计划引用不存在的资产", func(m map[string]any) {
			// 计划记录指向不存在的 EQ-9；建立履历挂在既有资产 EQ-1 上（自身合法），
			// 使计划段校验先报告归属错误，而不是履历缺资产。
			pr := planRecord()
			pr["asset_id"] = "EQ-9"
			m["plans"] = []any{pr}
			appendTo(m, "events", planEvent(4, "2026-11-01"))
		}, "保养计划矛盾"},
		{"完成后下一到期日越界", withPlan(func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["first_due"] = "9999-12-30"
			m["plans"].([]any)[0].(map[string]any)["interval_days"] = 2
			appendTo(m, "events", planEvent(4, "9999-12-30"))
			appendTo(m, "events", doneEvent(5, "9999-12-30", "9999-12-30"))
		}), "保养计划矛盾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, tc.mutate)
			_, err := openStore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应报 %q，得到 %v", tc.wantErr, err)
			}
			got, rerr := os.ReadFile(filepath.Join(dir, dataFileName))
			if rerr != nil || !bytes.Equal(got, raw) {
				t.Fatal("矛盾台账原文件应保留不变")
			}
			// 查询命令在矛盾台账上失败（退出码 1）。
			var out, errBuf bytes.Buffer
			if code := run([]string{"due", "--data-dir", dir, "--date", "9999-12-31"}, &out, &errBuf); code != 1 {
				t.Fatalf("矛盾台账上 due 退出码 = %d，应为 1（%s）", code, errBuf.String())
			}
		})
	}
}

// 失败保护：完成登记因下一到期日越界被拒后，文件不变，重启后可重试；
// 写入失败时不留下部分变化。
func TestPlanFailureThenReload(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "设备", "一楼")
	mustPlan(t, s, "EQ-1", "保养", "2026-11-01", 30)
	mustComplete(t, s, "EQ-1", "2026-11-01", "2026-11-01", "已完成") // 下一 12-01
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, dataFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 旧周期重复：业务拒绝，保存后文件字节不变。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-11-01", "2026-12-01", "重复"); !errors.Is(err, errConflict) {
		t.Fatalf("旧周期重复应冲突，得到 %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Fatal("业务失败后保存不应改变文件")
	}

	// 目录不可写时保存失败：内存中的完成不落地，恢复后可重试。
	if _, _, _, err := s.completeMaintenance("EQ-1", "2026-12-01", "2026-12-02", "再次完成"); err != nil {
		t.Fatalf("合法完成应在内存成功: %v", err)
	}
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s.save()
	_ = os.Chmod(s.dir, 0o755)
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，跳过写入失败分支")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应留下部分变化")
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.planNextDue(s2.findPlan("EQ-1")); got != "2026-12-01" {
		t.Fatalf("写入失败后下一到期日应仍是 2026-12-01，得到 %s", got)
	}
	// 恢复后重试成功。
	_, next := mustComplete(t, s2, "EQ-1", "2026-12-01", "2026-12-02", "再次完成")
	if next != "2026-12-31" {
		t.Fatalf("重试后下一到期日 = %s，想得到 2026-12-31", next)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
}

// 命令行：参数错误退出 2，业务失败退出 1，成功输出关键字段；due 只读。
func TestMaintenanceCommandLine(t *testing.T) {
	dir := t.TempDir()
	runOK := func(args ...string) string {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code != 0 {
			t.Fatalf("%v 应成功 code=%d: %s", args[:1], code, errBuf.String())
		}
		return out.String()
	}
	runCode := func(wantCode int, args ...string) string {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code != wantCode {
			t.Fatalf("%v 退出码 = %d, 想得到 %d: %s", args[:1], code, wantCode, errBuf.String())
		}
		return errBuf.String()
	}

	// 登记资产。
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")

	// 参数错误（缺参数 / 坏日期 / 间隔非正）退出 2。
	runCode(2, "plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01")
	runCode(2, "plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-31", "--interval-days", "30")
	runCode(2, "plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "0")
	runCode(2, "due", "--data-dir", dir, "--date", "not-a-date")

	// 成功建立。
	out := runOK("plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "30")
	if !strings.Contains(out, "首次到期日: 2026-11-01") {
		t.Fatalf("plan 输出应含首次到期日:\n%s", out)
	}
	// 已有计划：业务冲突退出 1。
	runCode(1, "plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "别的",
		"--first-due", "2026-12-01", "--interval-days", "10")
	// 未知资产退出 1。
	runCode(1, "plan", "--data-dir", dir, "--asset-id", "NOPE", "--content", "x",
		"--first-due", "2026-12-01", "--interval-days", "10")

	// 详情显示计划信息；无计划资产明确提示。
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼")
	out = runOK("detail", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(out, "保养计划: 有") || !strings.Contains(out, "下一到期日: 2026-11-01") ||
		!strings.Contains(out, "间隔天数: 30") {
		t.Fatalf("detail 应显示计划:\n%s", out)
	}
	out = runOK("detail", "--data-dir", dir, "--asset-id", "EQ-2")
	if !strings.Contains(out, "保养计划: 无") {
		t.Fatalf("无计划应明确提示:\n%s", out)
	}

	// 完成：日期不合条件退出 1；成功输出完成周期与下一到期日。
	runCode(1, "complete", "--data-dir", dir, "--asset-id", "EQ-1",
		"--period-due", "2026-12-01", "--done-date", "2026-12-01", "--result", "提前完成下一周期")
	runCode(1, "complete", "--data-dir", dir, "--asset-id", "EQ-1",
		"--period-due", "2026-11-01", "--done-date", "2026-10-31", "--result", "完成日过早")
	runCode(2, "complete", "--data-dir", dir, "--asset-id", "EQ-1",
		"--period-due", "11/01", "--done-date", "2026-11-01", "--result", "坏日期")
	out = runOK("complete", "--data-dir", dir, "--asset-id", "EQ-1",
		"--period-due", "2026-11-01", "--done-date", "2026-11-05", "--result", "已更换原厂滤芯")
	if !strings.Contains(out, "完成周期到期日: 2026-11-01") || !strings.Contains(out, "下一到期日: 2026-12-01") {
		t.Fatalf("complete 输出应含完成周期与下一到期日:\n%s", out)
	}

	// due 查询与排序、空提示。
	mustPlan2(t, dir, "EQ-2", "2026-10-01")
	out = runOK("due", "--data-dir", dir, "--date", "2026-11-01")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// 标题行 + EQ-2(10-01) + EQ-1(12-01? 不，EQ-1 已推进到 12-01，11-01 不含)
	if !strings.Contains(out, "EQ-2") || strings.Contains(out, "EQ-1") {
		t.Fatalf("截至 11-01 应只含 EQ-2:\n%s", out)
	}
	if lines[0] == "" {
		t.Fatal("应有标题行")
	}
	out = runOK("due", "--data-dir", dir, "--date", "2026-09-30")
	if !strings.Contains(out, "没有已到期的保养计划") {
		t.Fatalf("无匹配应明确提示:\n%s", out)
	}

	// 履历混合展示。
	out = runOK("history", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(out, eventPlanCreate) || !strings.Contains(out, eventMaintDone) ||
		!strings.Contains(out, "周期到期日 2026-11-01") || !strings.Contains(out, "实际完成日 2026-11-05") {
		t.Fatalf("history 应展示保养履历:\n%s", out)
	}

	// due 为只读：查询前后文件字节一致。
	raw1, _ := os.ReadFile(filepath.Join(dir, dataFileName))
	runOK("due", "--data-dir", dir, "--date", "9999-12-31")
	raw2, _ := os.ReadFile(filepath.Join(dir, dataFileName))
	if !bytes.Equal(raw1, raw2) {
		t.Fatal("due 查询不应写文件")
	}
}

// mustPlan2 通过命令行为另一资产建立简单计划（间隔 10 天）。
func mustPlan2(t *testing.T, dir, assetID, firstDue string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run([]string{"plan", "--data-dir", dir, "--asset-id", assetID,
		"--content", "清洗", "--first-due", firstDue, "--interval-days", "10"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("plan 失败: %s", errBuf.String())
	}
}
