package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// 建立计划：成功、重复拒绝、未知资产拒绝、非法输入拒绝；详情与履历输出。
func TestPlanCreateAndDetail(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}

	// 未知资产拒绝。
	if _, err := s.createPlan("NOPE", "更换滤芯", "2026-11-01", 90); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	// 非法输入拒绝：空内容、非正间隔、无效日期。
	if _, err := s.createPlan("EQ-1", "", "2026-11-01", 90); !errors.Is(err, errConflict) {
		t.Fatalf("空内容应失败，得到 %v", err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 0); !errors.Is(err, errConflict) {
		t.Fatalf("零间隔应失败，得到 %v", err)
	}
	for _, bad := range []string{"2026-1-1", "2026-13-01", "2026-02-29", "0000-01-01", "20261101"} {
		if _, err := s.createPlan("EQ-1", "更换滤芯", bad, 90); !errors.Is(err, errConflict) {
			t.Fatalf("日期 %q 应失败，得到 %v", bad, err)
		}
	}
	if len(s.data.Plans) != 0 || len(s.data.Events) != 0 {
		t.Fatal("失败的建立不应产生计划或履历")
	}

	p, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90)
	if err != nil {
		t.Fatalf("createPlan: %v", err)
	}
	if p.NextDue != "2026-11-01" {
		t.Fatalf("下一到期日 = %q，想得到 2026-11-01", p.NextDue)
	}
	// 已有计划拒绝，不覆盖。
	if _, err := s.createPlan("EQ-1", "其他内容", "2027-01-01", 30); !errors.Is(err, errConflict) {
		t.Fatalf("重复建立应冲突，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.Content != "更换滤芯" || got.IntervalDays != 90 {
		t.Fatalf("计划不应被覆盖: %+v", got)
	}
	// 恰一条建立履历，含初始计划。
	events := s.eventsOf("EQ-1")
	if len(events) != 1 || events[0].Kind != eventPlanCreate ||
		events[0].Content != "更换滤芯" || events[0].Due != "2026-11-01" || events[0].Interval != 90 {
		t.Fatalf("建立履历不对: %+v", events)
	}

	// 重启后保持；详情显示计划。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got == nil || got.NextDue != "2026-11-01" {
		t.Fatalf("重开后计划未保持: %+v", got)
	}
	var buf bytes.Buffer
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"保养内容: 更换滤芯", "保养间隔: 每 90 天", "下一到期日: 2026-11-01"} {
		if !strings.Contains(out, want) {
			t.Fatalf("详情缺少 %q:\n%s", want, out)
		}
	}
	// 无计划资产明确提示。
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-2"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "保养计划: 无") {
		t.Fatalf("无计划应明确提示:\n%s", buf.String())
	}
}

// 延期完成：跨过的周期不生成记录，下一到期日为首次到期日加整数倍间隔中
// 严格晚于完成日的最早日期，而不是完成日加间隔。
func TestCompleteDelayedSkipsCycles(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 到期日 2026-01-01，延期到 2026-01-25 完成：跨过 01-11、01-21 两个周期，
	// 下一到期日为 01-31（首次+3*10），而非 01-25+10=02-04。
	p, next, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-25", "已更换")
	if err != nil {
		t.Fatalf("completePlan: %v", err)
	}
	if next != "2026-01-31" || p.NextDue != "2026-01-31" {
		t.Fatalf("下一到期日 = %q，想得到 2026-01-31", next)
	}
	// 只记录一次实际保养：建立 + 完成共两条履历。
	events := s.eventsOf("EQ-1")
	if len(events) != 2 || events[1].Kind != eventPlanDone ||
		events[1].Due != "2026-01-01" || events[1].Done != "2026-01-25" || events[1].Content != "已更换" {
		t.Fatalf("完成履历不对: %+v", events)
	}
	// 重启后保持。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-31" {
		t.Fatalf("重开后下一到期日 = %q", got.NextDue)
	}
}

// 旧周期重复登记与登记新周期均拒绝；完成日早于到期日拒绝；空结果拒绝。
func TestCompleteOldCycleRepeatRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 无计划资产拒绝。
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.completePlan("EQ-2", "2026-01-01", "2026-01-01", "结果"); !errors.Is(err, errNotFound) {
		t.Fatalf("无计划应失败，得到 %v", err)
	}
	// 完成日早于到期日拒绝。
	if _, _, err := s.completePlan("EQ-1", "2026-01-01", "2025-12-31", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("完成日早于到期日应失败，得到 %v", err)
	}
	// 空结果拒绝。
	if _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空结果应失败，得到 %v", err)
	}
	// 尚未到期的新周期不能登记。
	if _, _, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-11", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("登记新周期应失败，得到 %v", err)
	}

	// 正常完成两个周期：01-01 当天完成 → 下一 01-11；01-11 当天完成 → 下一 01-21。
	if _, next, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "第一次"); err != nil || next != "2026-01-11" {
		t.Fatalf("第一周期: next=%q err=%v", next, err)
	}
	if _, next, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-11", "第二次"); err != nil || next != "2026-01-21" {
		t.Fatalf("第二周期: next=%q err=%v", next, err)
	}
	// 旧周期重复登记拒绝，状态不推进。
	for _, due := range []string{"2026-01-01", "2026-01-11"} {
		if _, _, err := s.completePlan("EQ-1", due, "2026-01-21", "重复"); !errors.Is(err, errConflict) {
			t.Fatalf("旧周期 %s 重复应失败，得到 %v", due, err)
		}
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-21" {
		t.Fatalf("失败的重复登记不应推进日期，下一到期日 = %q", got.NextDue)
	}
	if got := len(s.eventsOf("EQ-1")); got != 3 {
		t.Fatalf("失败的重复登记不应新增履历，履历数 = %d", got)
	}
}

// 闰日按公历规则运算：2024 为闰年，2025、2100 不是。
func TestLeapDayArithmetic(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// 2024-02-29 是有效日期；间隔 365 天，下一到期为 2025-02-28
	// （2024-02-29 + 365 个日历日；2024-03-01 至 2025-02-28 共 365 天）。
	if _, err := s.createPlan("EQ-1", "年检", "2024-02-29", 365); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.completePlan("EQ-1", "2024-02-29", "2024-02-29", "完成"); err != nil || next != "2025-02-28" {
		t.Fatalf("闰日完成: next=%q err=%v", next, err)
	}
	// 2025-02-29 无效（平年）。
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "年检", "2025-02-29", 365); !errors.Is(err, errConflict) {
		t.Fatalf("平年 2 月 29 日应拒绝，得到 %v", err)
	}
	// 间隔 1 天跨闰日：2024-02-28 完成 → 下一 2024-02-29。
	if _, err := s.createPlan("EQ-2", "日检", "2024-02-28", 1); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.completePlan("EQ-2", "2024-02-28", "2024-02-28", "完成"); err != nil || next != "2024-02-29" {
		t.Fatalf("跨闰日: next=%q err=%v", next, err)
	}
	// 2100 年不是闰年：2100-02-28 + 1 = 2100-03-01。
	if _, err := s.registerAsset("EQ-3", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-3", "日检", "2100-02-28", 1); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.completePlan("EQ-3", "2100-02-28", "2100-02-28", "完成"); err != nil || next != "2100-03-01" {
		t.Fatalf("2100 非闰年: next=%q err=%v", next, err)
	}
	// 延期跨过闰日：2024-02-28 到期、间隔 1、2024-03-01 完成 → 下一 2024-03-02。
	if _, err := s.registerAsset("EQ-4", "饮水机", "四楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-4", "日检", "2024-02-27", 1); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.completePlan("EQ-4", "2024-02-27", "2024-03-01", "完成"); err != nil || next != "2024-03-02" {
		t.Fatalf("延期跨闰日: next=%q err=%v", next, err)
	}
}

// 无保养字段的有效旧库直接加载，可建立计划并完成，重启后保持。
func TestLegacyStoreWithoutPlansField(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // baseLedger 无 plans 字段
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无保养字段的旧库应能加载: %v", err)
	}
	if len(s.data.Plans) != 0 {
		t.Fatal("旧库不应有保养计划")
	}
	s.now = newTestStore(t).now
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatalf("旧库建立计划: %v", err)
	}
	if _, next, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-02", "已更换"); err != nil {
		t.Fatalf("旧库完成登记: %v", err)
	} else if next != "2027-01-30" {
		t.Fatalf("下一到期日 = %q，想得到 2027-01-30", next)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got == nil || got.NextDue != "2027-01-30" {
		t.Fatalf("重开后计划未保持: %+v", got)
	}
	// 旧工单行为保持：未关闭工单仍在，资产仍为维修中。
	if got := s2.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatalf("旧库工单应保持，得到 %v", got)
	}
	if got := s2.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("资产状态 = %q", got)
	}
}

// 维修中资产可建立计划并登记完成：不创建或终结工单、不消耗工单编号，
// 不改变资产状态、请求绑定或停机统计；履历按序号交错展示。
func TestMaintenanceInterleavedWithRepair(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	// 维修中建立计划、登记完成。
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatalf("维修中建立计划: %v", err)
	}
	if _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-02", "已更换"); err != nil {
		t.Fatalf("维修中登记完成: %v", err)
	}
	// 资产状态、工单、编号计数器、请求绑定均不变。
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("保养不应改变资产状态，得到 %q", got)
	}
	if len(s.data.Tickets) != 1 || s.data.Tickets[0].Status != ticketOpen {
		t.Fatalf("保养不应创建或终结工单: %+v", s.data.Tickets)
	}
	if s.data.NextTicketSeq != 2 {
		t.Fatalf("保养不应消耗工单编号，NextTicketSeq = %d", s.data.NextTicketSeq)
	}
	if len(s.data.Requests) != 1 {
		t.Fatalf("保养不应改变请求绑定，绑定数 = %d", len(s.data.Requests))
	}
	// 履历交错：报修、保养建立、保养完成，按序号排列。
	events := s.eventsOf("EQ-1")
	if len(events) != 3 || events[0].Kind != eventReport ||
		events[1].Kind != eventPlanCreate || events[2].Kind != eventPlanDone {
		t.Fatalf("履历顺序不对: %+v", events)
	}
	// 关闭工单后保养行为不变。
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, next, err := s.completePlan("EQ-1", "2027-01-30", "2027-01-30", "第二次"); err != nil || next != "2027-04-30" {
		t.Fatalf("关闭后登记完成: next=%q err=%v", next, err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("资产状态 = %q", got)
	}
	// 停机统计不受保养履历影响（履历时间由测试时钟产生，窗口覆盖即可）。
	s = saveAndReopen(t, s)
	start := mustParseTime(t, "2020-01-01T00:00:00Z")
	end := mustParseTime(t, "2030-12-31T00:00:00Z")
	res, err := s.downtimeForAssets([]string{"EQ-1"}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	events = s.eventsOf("EQ-1")
	var reportTime, closeTime int64
	for _, e := range events {
		switch e.Kind {
		case eventReport:
			reportTime = e.Time.Unix()
		case eventClose:
			closeTime = e.Time.Unix()
		}
	}
	if res[0].Seconds != closeTime-reportTime {
		t.Fatalf("停机秒数 = %d，想得到 %d（保养履历不应计入）", res[0].Seconds, closeTime-reportTime)
	}
	// history 输出包含保养履历。
	var buf bytes.Buffer
	if err := cmdHistory([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"保养建立", "保养完成", "周期到期日 2026-11-01，实际完成日 2026-11-02"} {
		if !strings.Contains(out, want) {
			t.Fatalf("履历输出缺少 %q:\n%s", want, out)
		}
	}
}

// 到期查询：按到期日再按资产编号排序，无匹配明确提示，只读不初始化目录。
func TestDueQuery(t *testing.T) {
	s := newTestStore(t)
	for _, a := range [][3]string{
		{"EQ-B", "空调", "二楼"}, {"EQ-A", "打印机", "一楼"}, {"EQ-C", "叉车", "仓库"},
	} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	// EQ-A 与 EQ-B 同一到期日（检验按编号次序），EQ-C 更晚。
	if _, err := s.createPlan("EQ-B", "清洗", "2026-11-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-A", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-C", "润滑", "2026-12-01", 60); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-11-15"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 { // 表头 + 两行
		t.Fatalf("输出应为 3 行:\n%s", out)
	}
	if !strings.Contains(lines[1], "EQ-A\t打印机\t更换滤芯\t2026-11-01") ||
		!strings.Contains(lines[2], "EQ-B\t空调\t清洗\t2026-11-01") {
		t.Fatalf("同到期日应按资产编号排序:\n%s", out)
	}
	// 更晚的截止日包含全部，EQ-C 排在最后。
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-12-31"}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	if !strings.Contains(out, "共 3 项") || strings.Index(out, "EQ-C") < strings.Index(out, "EQ-B") {
		t.Fatalf("排序或数量不对:\n%s", out)
	}
	// 无匹配明确提示。
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-10-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "没有到期的保养计划") {
		t.Fatalf("无匹配应明确提示:\n%s", buf.String())
	}
	// 只读：目录不存在时不初始化目录、不写文件。
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", missing, "--date", "2026-12-31"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "没有到期的保养计划") {
		t.Fatalf("空库应明确提示:\n%s", buf.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("查询不应初始化目录: %v", err)
	}
	// 查询不推进计划。
	if got := s.findPlan("EQ-A"); got.NextDue != "2026-11-01" {
		t.Fatalf("查询不应推进计划，下一到期日 = %q", got.NextDue)
	}
}

// 导入复制保养计划与保养履历，完成链接续；目标已有计划不变；源只读。
func TestImportCopiesPlans(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	for _, a := range [][3]string{{"EQ-A", "打印机", "一楼"}, {"EQ-B", "空调", "二楼"}} {
		if _, err := src.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.createPlan("EQ-A", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.completePlan("EQ-A", "2026-01-01", "2026-01-25", "已更换"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-B", "清洗", "2026-06-01", 30); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标已有自己的计划，导入后不变。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.createPlan("EQ-X", "润滑", "2026-05-01", 60); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if outcome.plans != 1 {
		t.Fatalf("应导入 1 项计划，得到 %d", outcome.plans)
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读不变")
	}

	// 目标：EQ-A 计划原样复制（下一到期日 2026-01-31），EQ-X 计划不变。
	re, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	pa := re.findPlan("EQ-A")
	if pa == nil || pa.Content != "更换滤芯" || pa.FirstDue != "2026-01-01" ||
		pa.IntervalDays != 10 || pa.NextDue != "2026-01-31" {
		t.Fatalf("导入的计划不对: %+v", pa)
	}
	if px := re.findPlan("EQ-X"); px == nil || px.NextDue != "2026-05-01" {
		t.Fatalf("目标已有计划不应改变: %+v", px)
	}
	if re.findPlan("EQ-B") != nil {
		t.Fatal("未选择的资产不应导入计划")
	}
	// 完成链接续：可在目标继续登记下一周期。
	if _, next, err := re.completePlan("EQ-A", "2026-01-31", "2026-01-31", "目标完成"); err != nil || next != "2026-02-10" {
		t.Fatalf("导入后完成链接续: next=%q err=%v", next, err)
	}
	mustSave(t, re)
	// 重启后保持。
	re2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := re2.findPlan("EQ-A"); got.NextDue != "2026-02-10" {
		t.Fatalf("重启后下一到期日 = %q", got.NextDue)
	}
	// 保养履历随资产导入：EQ-A 的建立与完成履历齐全。
	var creates, dones int
	for _, e := range re2.eventsOf("EQ-A") {
		switch e.Kind {
		case eventPlanCreate:
			creates++
		case eventPlanDone:
			dones++
		}
	}
	if creates != 1 || dones != 2 {
		t.Fatalf("保养履历条数不对: 建立 %d 完成 %d", creates, dones)
	}
}

// 下一到期日超出日期范围时整次拒绝：不推进日期、不新增履历，原文件不变，
// 恢复（改用更早完成日）后可重试。
func TestCompleteOverflowRejectedAndRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// 9999-01-01 + 364 天 = 9999-12-31（范围内）；再推进即超出 9999-12-31。
	if _, err := s.createPlan("EQ-1", "年检", "9999-01-01", 364); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, s.dir)

	// 完成日 9999-12-31：下一到期日须严格晚于它，即 9999-01-01+728 天，越界。
	if _, _, err := s.completePlan("EQ-1", "9999-01-01", "9999-12-31", "完成"); !errors.Is(err, errConflict) {
		t.Fatalf("下一到期日越界应整次拒绝，得到 %v", err)
	}
	// 不推进日期、不新增履历。
	if got := s.findPlan("EQ-1"); got.NextDue != "9999-01-01" {
		t.Fatalf("拒绝后不应推进日期，下一到期日 = %q", got.NextDue)
	}
	if got := len(s.eventsOf("EQ-1")); got != 1 {
		t.Fatalf("拒绝后不应新增履历，履历数 = %d", got)
	}
	// 原文件字节不变。
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("拒绝后原文件不应改变")
	}
	// 重载后状态保持，可用更早完成日重试成功。
	s = saveAndReopen(t, s)
	if _, next, err := s.completePlan("EQ-1", "9999-01-01", "9999-01-01", "完成"); err != nil || next != "9999-12-31" {
		t.Fatalf("重试应成功: next=%q err=%v", next, err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "9999-12-31" {
		t.Fatalf("重载后下一到期日 = %q", got.NextDue)
	}
	// 到达范围尽头后，再次完成必然越界，整次拒绝。
	if _, _, err := s.completePlan("EQ-1", "9999-12-31", "9999-12-31", "完成"); !errors.Is(err, errConflict) {
		t.Fatalf("范围尽头完成应拒绝，得到 %v", err)
	}
}

// 矛盾保养数据（计划归属、唯一性、日期、建立及完成链、推出日期）加载即报错，
// 不自动修复，原文件保留。
func TestPlanConsistencyRejected(t *testing.T) {
	// 在 baseLedger 基础上附加合法的保养数据，再分别破坏。
	basePlan := func(m map[string]any) {
		m["plans"] = []any{map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": "2026-11-01", "interval_days": 90, "next_due": "2026-11-01",
		}}
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
			"due": "2026-11-01", "interval": 90, "time": "2026-10-01T12:00:00Z",
		})
	}
	cases := map[string]func(m map[string]any){
		"计划引用不存在的资产": func(m map[string]any) {
			basePlan(m)
			m["plans"].([]any)[0].(map[string]any)["asset_id"] = "EQ-X"
		},
		"同一资产两个计划": func(m map[string]any) {
			basePlan(m)
			m["plans"] = append(m["plans"].([]any), map[string]any{
				"asset_id": "EQ-1", "content": "另一个",
				"first_due": "2026-11-01", "interval_days": 90, "next_due": "2026-11-01",
			})
		},
		"首次到期日无效": func(m map[string]any) {
			basePlan(m)
			m["plans"].([]any)[0].(map[string]any)["first_due"] = "2026-02-29"
		},
		"间隔非正整数": func(m map[string]any) {
			basePlan(m)
			m["plans"].([]any)[0].(map[string]any)["interval_days"] = 0
		},
		"缺少建立履历": func(m map[string]any) {
			m["plans"] = []any{map[string]any{
				"asset_id": "EQ-1", "content": "更换滤芯",
				"first_due": "2026-11-01", "interval_days": 90, "next_due": "2026-11-01",
			}}
		},
		"建立履历与计划不符": func(m map[string]any) {
			basePlan(m)
			m["plans"].([]any)[0].(map[string]any)["content"] = "其他内容"
		},
		"完成链接续断裂": func(m map[string]any) {
			basePlan(m)
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "保养完成", "content": "已更换",
				"due": "2026-11-02", "done": "2026-11-02", "time": "2026-10-01T13:00:00Z",
			})
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2027-01-31"
		},
		"推出日期与保存不符": func(m map[string]any) {
			basePlan(m)
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "保养完成", "content": "已更换",
				"due": "2026-11-01", "done": "2026-11-02", "time": "2026-10-01T13:00:00Z",
			})
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-11-01"
		},
		"无计划却有保养履历": func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
				"due": "2026-11-01", "interval": 90, "time": "2026-10-01T12:00:00Z",
			})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾数据应报错")
			} else if !strings.Contains(err.Error(), "矛盾") {
				t.Fatalf("错误应指出矛盾，得到 %v", err)
			}
			// 原文件保留，不自动修复。
			if got := readFileBytes(t, dir); !bytes.Equal(got, raw) {
				t.Fatal("原文件不应被修改")
			}
		})
	}
	// 合法的保养数据可以加载。
	dir := t.TempDir()
	writeLedger(t, dir, basePlan)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法保养数据应能加载: %v", err)
	}
	if got := s.findPlan("EQ-1"); got == nil || got.NextDue != "2026-11-01" {
		t.Fatalf("计划未加载: %+v", got)
	}
}

// 参数错误退出码 2，业务失败退出码 1。
func TestMaintenanceExitCodes(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	// 缺少必填参数、非法日期、非正间隔 → 2。
	for _, args := range [][]string{
		{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2026-11-01"},
		{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2026-13-01", "--interval-days", "90"},
		{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2026-11-01", "--interval-days", "-3"},
		{"maintain", "--data-dir", dir, "--asset-id", "EQ-1", "--due", "bad", "--done", "2026-11-01", "--result", "x"},
		{"due", "--data-dir", dir, "--date", "2026-1-1"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("参数错误应退出 2: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 未知资产 → 1。
	out.Reset()
	errOut.Reset()
	code := run([]string{"plan", "--data-dir", dir, "--asset-id", "NOPE", "--content", "x",
		"--first-due", "2026-11-01", "--interval-days", "90"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("业务失败应退出 1，得到 %d", code)
	}
	// 建立后重复建立 → 1；成功 → 0。
	out.Reset()
	errOut.Reset()
	code = run([]string{"register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("register: %d %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = run([]string{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "90"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "首次到期日: 2026-11-01") {
		t.Fatalf("plan 应成功并显示首次到期日: %d %s %s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code = run([]string{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "覆盖",
		"--first-due", "2027-01-01", "--interval-days", "30"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("重复建立应退出 1，得到 %d", code)
	}
	// 完成登记成功输出完成周期与下一到期日。
	out.Reset()
	errOut.Reset()
	code = run([]string{"maintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--due", "2026-11-01", "--done", "2026-11-03", "--result", "已更换"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "已完成周期 2026-11-01") ||
		!strings.Contains(out.String(), "下一到期日: 2027-01-30") {
		t.Fatalf("maintain 输出不对: %d %s %s", code, out.String(), errOut.String())
	}
}
