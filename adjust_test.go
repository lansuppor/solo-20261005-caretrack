package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 方案调整：成功采用新方案并重设下一到期日，追加记录前后方案、原下一到期日、
// 理由与时间的调整履历；未知资产、无计划、不合法输入、新首次日未严格晚于有效
// 完成日均拒绝；detail、due、history 采用当前方案。
func TestAdjustPlanBasic(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 一次有效完成（完成日 2026-01-05），下一到期日 2026-01-11。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-05", "已完成"); err != nil {
		t.Fatal(err)
	}

	// 未知资产、无计划拒绝。
	if _, err := s.adjustPlan("NOPE", "方案B", "2026-02-01", 30, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	if _, err := s.adjustPlan("EQ-2", "方案B", "2026-02-01", 30, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("无计划应失败，得到 %v", err)
	}
	// 不合法输入拒绝：空内容、非正间隔、无效日期、空理由。
	if _, err := s.adjustPlan("EQ-1", "", "2026-02-01", 30, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("空内容应失败，得到 %v", err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 0, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("零间隔应失败，得到 %v", err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-29", 30, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("无效日期应失败，得到 %v", err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应失败，得到 %v", err)
	}
	// 新首次日须严格晚于所有未撤销完成的实际完成日（2026-01-05）。
	for _, d := range []string{"2026-01-04", "2026-01-05"} {
		if _, err := s.adjustPlan("EQ-1", "方案B", d, 30, "理由"); !errors.Is(err, errConflict) {
			t.Fatalf("新首次日 %s 未严格晚于有效完成日应失败，得到 %v", d, err)
		}
	}
	// 失败路径不产生任何变化。
	if got := s.findPlan("EQ-1"); got.Content != "方案A" || got.NextDue != "2026-01-11" {
		t.Fatalf("失败的调整不应改动计划: %+v", got)
	}
	if got := len(s.eventsOf("EQ-1")); got != 2 {
		t.Fatalf("失败的调整不应新增履历，履历数 = %d", got)
	}

	// 成功调整：计划采用新方案，下一到期日设为新首次日。
	p, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, "规范更新")
	if err != nil {
		t.Fatalf("adjustPlan: %v", err)
	}
	if p.Content != "方案B" || p.FirstDue != "2026-02-01" || p.IntervalDays != 30 || p.NextDue != "2026-02-01" {
		t.Fatalf("调整后的计划不对: %+v", p)
	}
	// 调整履历：前后方案、原下一到期日、理由与时间齐全；不补任何完成记录。
	events := s.eventsOf("EQ-1")
	if len(events) != 3 || events[2].Kind != eventPlanAdjust {
		t.Fatalf("调整履历不对: %+v", events)
	}
	adj := events[2]
	if adj.Content != "方案B" || adj.Due != "2026-02-01" || adj.Interval != 30 ||
		adj.Reason != "规范更新" || adj.OldContent != "方案A" || adj.OldDue != "2026-01-01" ||
		adj.OldInterval != 10 || adj.OldNextDue != "2026-01-11" || adj.Time.IsZero() ||
		adj.TicketID != "" || adj.Done != "" || adj.TargetSeq != 0 {
		t.Fatalf("调整履历字段不对: %+v", adj)
	}
	// 原完成履历保留内容、日期与时间。
	done := events[1]
	if done.Kind != eventPlanDone || done.Due != "2026-01-01" || done.Done != "2026-01-05" ||
		done.Content != "已完成" || done.Time.IsZero() {
		t.Fatalf("原完成履历应保留: %+v", done)
	}

	// 重启后保持；detail、due、history 采用当前方案。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.Content != "方案B" || got.NextDue != "2026-02-01" {
		t.Fatalf("重开后计划未保持: %+v", got)
	}
	var buf bytes.Buffer
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"保养内容: 方案B", "保养间隔: 每 30 天", "下一到期日: 2026-02-01"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail 缺少 %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-02-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "EQ-1\t打印机\t方案B\t2026-02-01") {
		t.Fatalf("due 应采用当前方案:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdHistory([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	for _, want := range []string{"保养调整", "方案B（首次到期日 2026-02-01，每 30 天）",
		"原方案 方案A（首次到期日 2026-01-01，每 10 天）", "原下一到期日 2026-01-11", "规范更新"} {
		if !strings.Contains(out, want) {
			t.Fatalf("history 缺少 %q:\n%s", want, out)
		}
	}
}

// 维修中、停用时也允许调整方案：调整不改变资产状态、工单、请求绑定或编号计数器。
func TestAdjustAllowedWhileRepairingAndDeactivated(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 维修中调整。
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, "维修中调整"); err != nil {
		t.Fatalf("维修中调整: %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("调整不应改变资产状态，得到 %q", got)
	}
	if got := s.findTicket(tk.ID); got.Status != ticketOpen {
		t.Fatalf("调整不应终结工单，得到 %q", got.Status)
	}
	if s.data.NextTicketSeq != 2 || len(s.data.Requests) != 1 {
		t.Fatal("调整不应消耗工单编号或改变请求绑定")
	}
	// 关闭工单后停用：停用期间也允许调整（但停用期间仍不能登记完成）。
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "设备调拨"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案C", "2026-03-01", 60, "停用中调整"); err != nil {
		t.Fatalf("停用中调整: %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusDeactivated {
		t.Fatalf("调整不应改变停用状态，得到 %q", got)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-01", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间登记完成应拒绝，得到 %v", err)
	}
	// 恢复使用后按当前方案登记完成。
	if _, err := s.reactivateAsset("EQ-1", "调拨回库"); err != nil {
		t.Fatal(err)
	}
	if _, next, _, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-01", "结果"); err != nil || next != "2026-04-30" {
		t.Fatalf("恢复后完成: next=%q err=%v", next, err)
	}
}

// 已撤销的完成不限制新首次日；没有有效完成时无此限制（新首次日可任意）。
func TestAdjustFirstDueConstraint(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 完成（完成日 2026-01-20）后在同一段内撤销：不再限制新首次日。
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-20", "已完成")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "误登记"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-01-01", 10, "已撤销完成不限制"); err != nil {
		t.Fatalf("已撤销完成不应限制新首次日: %v", err)
	}
	// 没有任何完成的资产：新首次日可任意（早于原首次日也可以）。
	if _, err := s.createPlan("EQ-2", "方案A", "2026-06-01", 30); err != nil {
		t.Fatal(err)
	}
	p, err := s.adjustPlan("EQ-2", "方案B", "2026-01-01", 10, "无完成无限制")
	if err != nil {
		t.Fatalf("无有效完成应无日期限制: %v", err)
	}
	if p.NextDue != "2026-01-01" {
		t.Fatalf("下一到期日 = %q，想得到 2026-01-01", p.NextDue)
	}
}

// 方案段：maintain 仅完成当前段下一周期并按本段方案推进（含延期）；unmaintain
// 只能撤销当前段最新有效完成，可连续撤销本段；旧段完成不能再撤销且原样保留；
// 同周期重新完成生成新序号，旧序号不能误撤销新完成。
func TestSegmentMaintainRevokeAndDelay(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 旧段两次完成：下一到期日 2026-01-21。
	_, _, c1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	_, _, c2, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-12", "第二次")
	if err != nil {
		t.Fatal(err)
	}
	// 调整进入新段：新首次日 2026-02-01（晚于最晚完成日 2026-01-12），间隔 30。
	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, "规范更新"); err != nil {
		t.Fatal(err)
	}
	// 旧段完成不能再撤销。
	for _, seq := range []int{c1, c2} {
		if _, _, err := s.revokeCompletion("EQ-1", seq, "旧段"); !errors.Is(err, errConflict) {
			t.Fatalf("旧段完成序号 %d 应不能撤销，得到 %v", seq, err)
		}
	}
	// maintain 仅完成当前段下一周期：旧段到期日不能登记。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-21", "2026-01-21", "旧周期"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段周期应不能登记，得到 %v", err)
	}
	// 当前段完成：2026-02-01 当天完成 → 下一 2026-03-03（02-01 + 30 天）。
	_, next, c3, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-01", "新段第一次")
	if err != nil || next != "2026-03-03" {
		t.Fatalf("新段完成: next=%q err=%v", next, err)
	}
	// 延期完成：2026-03-03 到期、2026-03-15 完成，按本段首次日 2026-02-01 加整数倍
	// 30 天推进到严格晚于完成日的最早日期 2026-04-02（而非完成日加间隔）。
	_, next, c4, err := s.completePlan("EQ-1", "2026-03-03", "2026-03-15", "延期完成")
	if err != nil || next != "2026-04-02" {
		t.Fatalf("延期完成: next=%q err=%v", next, err)
	}
	// 存在更晚有效完成时撤销较早的拒绝；可连续撤销本段。
	if _, _, err := s.revokeCompletion("EQ-1", c3, "较早"); !errors.Is(err, errConflict) {
		t.Fatalf("存在更晚有效完成时应拒绝，得到 %v", err)
	}
	p, _, err := s.revokeCompletion("EQ-1", c4, "撤销延期")
	if err != nil || p.NextDue != "2026-03-03" {
		t.Fatalf("撤销 c4: next=%q err=%v", p.NextDue, err)
	}
	p, _, err = s.revokeCompletion("EQ-1", c3, "继续撤销")
	if err != nil || p.NextDue != "2026-02-01" {
		t.Fatalf("连续撤销 c3: next=%q err=%v", p.NextDue, err)
	}
	// 本段已无有效完成；旧段完成仍不能撤销。
	if _, _, err := s.revokeCompletion("EQ-1", c2, "旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成仍应不能撤销，得到 %v", err)
	}
	// 旧段完成原样保留：内容、日期、时间及有效状态不变。
	revoked := s.revokedDoneSeqs()
	for _, e := range s.eventsOf("EQ-1") {
		if e.Seq == c1 && (e.Content != "第一次" || e.Due != "2026-01-01" || e.Done != "2026-01-01" || revoked[c1]) {
			t.Fatalf("旧段完成 c1 应原样保留: %+v", e)
		}
		if e.Seq == c2 && (e.Content != "第二次" || e.Due != "2026-01-11" || e.Done != "2026-01-12" || revoked[c2]) {
			t.Fatalf("旧段完成 c2 应原样保留: %+v", e)
		}
	}
	// 同周期重新完成生成新序号；旧序号（已撤销的 c3）不能误撤销新完成。
	_, next, c5, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-05", "重新登记")
	if err != nil || next != "2026-03-03" {
		t.Fatalf("重新完成: next=%q err=%v", next, err)
	}
	if c5 == c3 {
		t.Fatal("重新完成应生成新序号")
	}
	if _, _, err := s.revokeCompletion("EQ-1", c3, "旧序号"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号再次撤销应拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-03-03" {
		t.Fatalf("旧序号撤销不应影响新登记，下一到期日 = %q", got.NextDue)
	}
	// 重启后保持：新段完成仍可撤销，旧段仍不可。
	s = saveAndReopen(t, s)
	if _, _, err := s.revokeCompletion("EQ-1", c2, "旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("重开后旧段完成仍应不能撤销，得到 %v", err)
	}
	p, _, err = s.revokeCompletion("EQ-1", c5, "撤销新登记")
	if err != nil || p.NextDue != "2026-02-01" {
		t.Fatalf("重开后撤销 c5: next=%q err=%v", p.NextDue, err)
	}
}

// 导入复制全部保养历史与当前方案：保留段边界、顺序与时间精度，撤销引用随履历
// 重编号替换；导入后可继续调整、完成与撤销；源只读、目标原有记录不变。
func TestImportWithAdjustments(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-A", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-A", "2026-01-01", "2026-01-01", "第一次"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-A", "2026-01-11", "2026-01-12", "第二次"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.adjustPlan("EQ-A", "方案B", "2026-02-01", 30, "规范更新"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-A", "2026-02-01", "2026-02-01", "新段第一次"); err != nil {
		t.Fatal(err)
	}
	_, _, last, err := src.completePlan("EQ-A", "2026-03-03", "2026-03-03", "新段第二次")
	if err != nil {
		t.Fatal(err)
	}
	// 撤销新段最新完成：下一到期日回到 2026-03-03。
	if _, _, err := src.revokeCompletion("EQ-A", last, "误登记"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标已有自己的资产、计划与履历。
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
	if outcome.plans != 1 || len(outcome.completions) != 4 {
		t.Fatalf("导入结果不对: plans=%d completions=%d", outcome.plans, len(outcome.completions))
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读不变")
	}

	re, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// 当前方案与下一到期日保持；目标已有计划不变。
	pa := re.findPlan("EQ-A")
	if pa == nil || pa.Content != "方案B" || pa.FirstDue != "2026-02-01" ||
		pa.IntervalDays != 30 || pa.NextDue != "2026-03-03" {
		t.Fatalf("导入的计划不对: %+v", pa)
	}
	if px := re.findPlan("EQ-X"); px == nil || px.Content != "润滑" || px.NextDue != "2026-05-01" {
		t.Fatalf("目标已有计划不应改变: %+v", px)
	}
	// 段边界保留：恰一条建立、一条调整、四条完成、一条撤销；撤销引用已替换。
	var creates, adjusts, dones, revokes int
	var adjustEvent, revokeEvent *Event
	for i, e := range re.eventsOf("EQ-A") {
		switch e.Kind {
		case eventPlanCreate:
			creates++
		case eventPlanAdjust:
			adjusts++
			adjustEvent = &re.eventsOf("EQ-A")[i]
		case eventPlanDone:
			dones++
		case eventPlanRevoke:
			revokes++
			revokeEvent = &re.eventsOf("EQ-A")[i]
		}
	}
	if creates != 1 || adjusts != 1 || dones != 4 || revokes != 1 {
		t.Fatalf("保养履历条数不对: 建立 %d 调整 %d 完成 %d 撤销 %d", creates, adjusts, dones, revokes)
	}
	if adjustEvent.OldContent != "方案A" || adjustEvent.OldDue != "2026-01-01" ||
		adjustEvent.OldInterval != 10 || adjustEvent.OldNextDue != "2026-01-21" ||
		adjustEvent.Reason != "规范更新" {
		t.Fatalf("调整履历未原样复制: %+v", adjustEvent)
	}
	newLast := outcome.completions[3].NewSeq
	if revokeEvent.TargetSeq != newLast {
		t.Fatalf("撤销引用应替换为新序号 %d，得到 %d", newLast, revokeEvent.TargetSeq)
	}
	// 导入后继续调整：新首次日须晚于有效完成的最晚完成日（2026-02-01）。
	if _, err := re.adjustPlan("EQ-A", "方案C", "2026-02-01", 30, "未严格更晚"); !errors.Is(err, errConflict) {
		t.Fatalf("新首次日未严格更晚应拒绝，得到 %v", err)
	}
	if _, err := re.adjustPlan("EQ-A", "方案C", "2026-03-01", 15, "导入后继续调整"); err != nil {
		t.Fatalf("导入后继续调整: %v", err)
	}
	// 继续完成与撤销：当前段完成可撤销，旧段完成不能撤销。
	_, next, cNew, err := re.completePlan("EQ-A", "2026-03-01", "2026-03-01", "目标完成")
	if err != nil || next != "2026-03-16" {
		t.Fatalf("导入后完成: next=%q err=%v", next, err)
	}
	if _, _, err := re.revokeCompletion("EQ-A", outcome.completions[2].NewSeq, "旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成应不能撤销，得到 %v", err)
	}
	p, _, err := re.revokeCompletion("EQ-A", cNew, "撤销目标完成")
	if err != nil || p.NextDue != "2026-03-01" {
		t.Fatalf("导入后撤销: next=%q err=%v", p.NextDue, err)
	}
	mustSave(t, re)
	// 重启后保持。
	re2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := re2.findPlan("EQ-A"); got.Content != "方案C" || got.NextDue != "2026-03-01" {
		t.Fatalf("重启后计划未保持: %+v", got)
	}
}

// 矛盾调整数据（原方案或原下一到期日不符、新首次日未严格更晚、撤销旧段完成、
// 推出方案或日期与保存不符、调整在建立之前、缺少理由、调整后按旧方案完成）
// 加载即报错，不自动修复，原文件保留。
func TestAdjustConsistencyRejected(t *testing.T) {
	// 合法基础：计划（当前方案B）+ 建立（seq 4，方案A）+ 完成（seq 5）+ 调整（seq 6，方案B）。
	base := func(m map[string]any) {
		m["plans"] = []any{map[string]any{
			"asset_id": "EQ-1", "content": "方案B",
			"first_due": "2026-02-01", "interval_days": 30, "next_due": "2026-02-01",
		}}
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "方案A",
			"due": "2026-01-01", "interval": 10, "time": "2026-10-01T12:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "kind": "保养完成", "content": "已完成",
			"due": "2026-01-01", "done": "2026-01-05", "time": "2026-10-01T13:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 6, "asset_id": "EQ-1", "kind": "保养调整", "content": "方案B",
			"due": "2026-02-01", "interval": 30, "reason": "规范更新",
			"old_content": "方案A", "old_due": "2026-01-01", "old_interval": 10,
			"old_next_due": "2026-01-11", "time": "2026-10-01T14:00:00Z",
		})
	}
	adjustEvent := func(m map[string]any) map[string]any {
		for _, e := range m["events"].([]any) {
			if e.(map[string]any)["kind"] == "保养调整" {
				return e.(map[string]any)
			}
		}
		return nil
	}
	cases := map[string]func(m map[string]any){
		"调整记录的原方案与当时不符": func(m map[string]any) {
			base(m)
			adjustEvent(m)["old_content"] = "方案X"
		},
		"调整记录的原下一到期日不符": func(m map[string]any) {
			base(m)
			adjustEvent(m)["old_next_due"] = "2026-01-01"
		},
		"新首次日未严格晚于有效完成日": func(m map[string]any) {
			base(m)
			// 完成日为 2026-01-05，新首次日须严格更晚；等于完成日不合法。
			adjustEvent(m)["due"] = "2026-01-05"
			m["plans"].([]any)[0].(map[string]any)["first_due"] = "2026-01-05"
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-01-05"
		},
		"撤销旧段完成": func(m map[string]any) {
			base(m)
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 5, "time": "2026-10-01T15:00:00Z",
			})
		},
		"推出方案与保存不符": func(m map[string]any) {
			base(m)
			m["plans"].([]any)[0].(map[string]any)["content"] = "方案C"
		},
		"推出下一到期日与保存不符": func(m map[string]any) {
			base(m)
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-03-01"
		},
		"调整在建立之前": func(m map[string]any) {
			base(m)
			// 删除建立履历（seq 4），调整出现在建立之前。
			events := m["events"].([]any)
			m["events"] = append(events[:1], events[2:]...)
		},
		"调整缺少理由": func(m map[string]any) {
			base(m)
			delete(adjustEvent(m), "reason")
		},
		"调整缺少原方案": func(m map[string]any) {
			base(m)
			delete(adjustEvent(m), "old_content")
		},
		"调整携带来路不明的完成日": func(m map[string]any) {
			base(m)
			adjustEvent(m)["done"] = "2026-02-01"
		},
		"调整后按旧方案完成": func(m map[string]any) {
			base(m)
			// 调整后下一到期日为 2026-02-01，却登记旧方案的 2026-01-11 周期。
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-02-11"
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养完成", "content": "旧周期",
				"due": "2026-01-11", "done": "2026-01-11", "time": "2026-10-01T15:00:00Z",
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
			if got := readFileBytes(t, dir); !bytes.Equal(got, raw) {
				t.Fatal("原文件不应被修改")
			}
		})
	}
	// 合法调整链可以加载并继续操作：完成当前段周期、再次调整；旧段完成不能撤销；
	// 数组乱序、序号间隔、时间不递增仍合法。
	dir := t.TempDir()
	writeLedger(t, dir, base)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法调整数据应能加载: %v", err)
	}
	s.now = newTestStore(t).now
	if got := s.findPlan("EQ-1"); got.Content != "方案B" || got.NextDue != "2026-02-01" {
		t.Fatalf("计划未加载: %+v", got)
	}
	if _, _, err := s.revokeCompletion("EQ-1", 5, "旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成应不能撤销，得到 %v", err)
	}
	if _, next, _, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-01", "新段完成"); err != nil || next != "2026-03-03" {
		t.Fatalf("加载后完成: next=%q err=%v", next, err)
	}
	if _, err := s.adjustPlan("EQ-1", "方案C", "2026-04-01", 15, "再次调整"); err != nil {
		t.Fatalf("再次调整: %v", err)
	}
	// 乱序数组仍合法。
	events := s.data.Events
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	if err := s.save(); err != nil {
		t.Fatalf("乱序数组应能保存: %v", err)
	}
	if _, err := openStore(dir); err != nil {
		t.Fatalf("乱序数组应能加载: %v", err)
	}
}

// 调整为一次原子保存：写入失败保留原文件字节，不留下部分变化、不消耗序号，
// 恢复后可重试。
func TestAdjustSaveFailureAndRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "方案A", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, s.dir)

	if _, err := s.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, "规范更新"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s.save()
	if err := os.Chmod(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	// 原文件字节不变。
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：计划仍为旧方案。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.Content != "方案A" || got.NextDue != "2026-01-01" {
		t.Fatalf("写入失败不应留下部分变化: %+v", got)
	}
	// 恢复后可重试：调整履历序号未消耗。
	p, err := s2.adjustPlan("EQ-1", "方案B", "2026-02-01", 30, "规范更新")
	if err != nil {
		t.Fatalf("恢复后重试: %v", err)
	}
	if p.NextDue != "2026-02-01" {
		t.Fatalf("重试后下一到期日 = %q", p.NextDue)
	}
	events := s2.eventsOf("EQ-1")
	if got := events[len(events)-1].Seq; got != 2 {
		t.Fatalf("失败不应消耗履历序号，调整序号 = %d，想得到 2", got)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findPlan("EQ-1"); got.Content != "方案B" || got.NextDue != "2026-02-01" {
		t.Fatalf("重载后计划未保持: %+v", got)
	}
}

// adjust 命令：成功输出新方案与日期；参数错误退出码 2，业务失败退出码 1。
func TestAdjustCommandExitCodes(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runOK := func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v 应成功: %d %s", args, code, errOut.String())
		}
		return out.String()
	}
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼")
	runOK("plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案A",
		"--first-due", "2026-01-01", "--interval-days", "10")
	runOK("maintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--due", "2026-01-01", "--done", "2026-01-05", "--result", "已完成")

	// 参数错误 → 2：缺参数、非法日期、非正间隔。
	for _, args := range [][]string{
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案B", "--first-due", "2026-02-01", "--interval-days", "30"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案B", "--first-due", "2026-13-01", "--interval-days", "30", "--reason", "x"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案B", "--first-due", "2026-02-01", "--interval-days", "-1", "--reason", "x"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--first-due", "2026-02-01", "--interval-days", "30", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("参数错误应退出 2: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 业务失败 → 1：未知资产、无计划、新首次日未严格晚于有效完成日（2026-01-05）。
	for _, args := range [][]string{
		{"adjust", "--data-dir", dir, "--asset-id", "NOPE", "--content", "方案B", "--first-due", "2026-02-01", "--interval-days", "30", "--reason", "x"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-2", "--content", "方案B", "--first-due", "2026-02-01", "--interval-days", "30", "--reason", "x"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案B", "--first-due", "2026-01-05", "--interval-days", "30", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 1 {
			t.Fatalf("业务失败应退出 1: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 成功：输出新方案与日期。
	o := runOK("adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "方案B",
		"--first-due", "2026-02-01", "--interval-days", "30", "--reason", "规范更新")
	for _, want := range []string{"已调整资产 EQ-1 的保养方案", "保养内容: 方案B",
		"保养间隔: 每 30 天", "首次到期日: 2026-02-01", "下一到期日: 2026-02-01"} {
		if !strings.Contains(o, want) {
			t.Fatalf("adjust 输出缺少 %q:\n%s", want, o)
		}
	}
	// 重启后 detail 采用当前方案。
	o = runOK("detail", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(o, "保养内容: 方案B") || !strings.Contains(o, "下一到期日: 2026-02-01") {
		t.Fatalf("重启后 detail 应采用当前方案:\n%s", o)
	}
}
