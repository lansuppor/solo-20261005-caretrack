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

// 调整方案：成功更换内容、周期起点与间隔，下一到期日设为新首次到期日；
// 原保养记录保留；追加含前后方案、原下一到期日、理由与时间的调整履历；
// detail、due 采用当前方案；维修中、停用时也允许。
func TestAdjustPlanBasic(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}

	// 未知资产、无计划、空内容、空理由、非正间隔、无效日期均拒绝。
	if _, _, err := s.adjustPlan("NOPE", "新内容", "2027-01-01", 60, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-2", "新内容", "2027-01-01", 60, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("无计划应失败，得到 %v", err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "", "2027-01-01", 60, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("空内容应失败，得到 %v", err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "新内容", "2027-01-01", 60, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应失败，得到 %v", err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "新内容", "2027-01-01", 0, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("零间隔应失败，得到 %v", err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "新内容", "2027-13-01", 60, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("无效日期应失败，得到 %v", err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 1 {
		t.Fatalf("失败的调整不应产生履历，履历数 = %d", got)
	}

	p, old, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2027-01-01", 60, "滤芯型号升级")
	if err != nil {
		t.Fatalf("adjustPlan: %v", err)
	}
	if old.Content != "更换滤芯" || old.FirstDue != "2026-11-01" || old.IntervalDays != 90 || old.NextDue != "2026-11-01" {
		t.Fatalf("原方案快照不对: %+v", old)
	}
	if p.Content != "更换高效滤芯" || p.FirstDue != "2027-01-01" || p.IntervalDays != 60 || p.NextDue != "2027-01-01" {
		t.Fatalf("调整后计划不对: %+v", p)
	}
	// 调整履历：前后方案、原下一到期日、理由与时间。
	events := s.eventsOf("EQ-1")
	if len(events) != 2 || events[1].Kind != eventPlanAdjust {
		t.Fatalf("履历不对: %+v", events)
	}
	adj := events[1]
	if adj.Content != "滤芯型号升级" || adj.NewContent != "更换高效滤芯" ||
		adj.Due != "2027-01-01" || adj.Interval != 60 ||
		adj.OldContent != "更换滤芯" || adj.OldDue != "2026-11-01" ||
		adj.OldInterval != 90 || adj.OldNextDue != "2026-11-01" || adj.Time.IsZero() {
		t.Fatalf("调整履历不对: %+v", adj)
	}

	// 重启后保持；detail、due 采用当前方案；history 展示调整履历。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.Content != "更换高效滤芯" || got.NextDue != "2027-01-01" || got.IntervalDays != 60 {
		t.Fatalf("重开后计划未保持: %+v", got)
	}
	var buf bytes.Buffer
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"保养内容: 更换高效滤芯", "保养间隔: 每 60 天", "下一到期日: 2027-01-01"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("详情缺少 %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-12-31"}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "EQ-1") {
		t.Fatalf("调整后下一到期日为 2027-01-01，不应在 2026 年内到期:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2027-01-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "EQ-1") || !strings.Contains(buf.String(), "更换高效滤芯") {
		t.Fatalf("due 应采用当前方案:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdHistory([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "保养调整") || !strings.Contains(out, "滤芯型号升级") ||
		!strings.Contains(out, "更换滤芯 -> 更换高效滤芯") {
		t.Fatalf("history 应展示调整履历:\n%s", out)
	}
}

// 新首次到期日须严格晚于所有未撤销完成的实际完成日；已撤销完成不再限制；
// 没有有效完成则无此限制。
func TestAdjustFirstDueAfterValidCompletions(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	_, _, doneSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-10", "已更换")
	if err != nil {
		t.Fatal(err)
	}
	// 等于或早于有效完成的实际完成日均拒绝。
	for _, bad := range []string{"2026-01-10", "2026-01-09", "2025-12-31"} {
		if _, _, err := s.adjustPlan("EQ-1", "新内容", bad, 60, "理由"); !errors.Is(err, errConflict) {
			t.Fatalf("新首次到期日 %s 应失败，得到 %v", bad, err)
		}
	}
	if _, _, err := s.adjustPlan("EQ-1", "新内容", "2026-01-11", 60, "理由"); err != nil {
		t.Fatalf("晚于完成日的调整应成功: %v", err)
	}
	// 旧段完成不能再撤销，其完成日仍参与限制（仍属未撤销完成）。
	if _, _, err := s.revokeCompletion("EQ-1", doneSeq, "误登记"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成应不能撤销，得到 %v", err)
	}
	// 在当前段完成再撤销：已撤销的完成日不再限制下一次调整。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-11", "2026-02-01", "已更换"); err != nil {
		t.Fatal(err)
	}
	_, _, doneSeq2, err := s.completePlan("EQ-1", "2026-03-12", "2026-03-15", "已更换")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", doneSeq2, "误登记"); err != nil {
		t.Fatalf("撤销当前段最新完成: %v", err)
	}
	// 未撤销完成的完成日为 2026-02-01（01-11 段）与 2026-01-10（旧段）：
	// 新首次日 2026-02-02 合法（已撤销的 03-15 不限制）。
	if _, _, err := s.adjustPlan("EQ-1", "再调整", "2026-02-02", 30, "理由"); err != nil {
		t.Fatalf("已撤销完成不应限制调整: %v", err)
	}
}

// 跨段撤销：调整开启新方案段后，旧段完成不能再撤销；当前段可连续撤销，
// 恢复目标完成的周期到期日；同周期重新完成生成新序号，旧序号不能误撤销新完成。
func TestAdjustSegmentRevoke(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	_, _, oldSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "旧段完成")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 60, "方案升级"); err != nil {
		t.Fatal(err)
	}
	// 旧段完成不能再撤销。
	if _, _, err := s.revokeCompletion("EQ-1", oldSeq, "想撤销旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成应不能撤销，得到 %v", err)
	}
	// 当前段完成两次，只能先撤销最新的一次。
	_, _, seq1, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-03", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	_, _, seq2, err := s.completePlan("EQ-1", "2026-04-02", "2026-04-05", "第二次")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "跳序撤销"); !errors.Is(err, errConflict) {
		t.Fatalf("存在更晚有效完成时应拒绝，得到 %v", err)
	}
	p, target, err := s.revokeCompletion("EQ-1", seq2, "误登记")
	if err != nil {
		t.Fatalf("撤销最新完成: %v", err)
	}
	if p.NextDue != "2026-04-02" || target.Due != "2026-04-02" {
		t.Fatalf("恢复下一到期日 = %q，想得到 2026-04-02", p.NextDue)
	}
	// 连续撤销本段：再撤销 seq1，下一到期日恢复为其周期到期日。
	p, _, err = s.revokeCompletion("EQ-1", seq1, "继续撤销")
	if err != nil {
		t.Fatalf("连续撤销本段: %v", err)
	}
	if p.NextDue != "2026-02-01" {
		t.Fatalf("下一到期日 = %q，想得到 2026-02-01", p.NextDue)
	}
	// 旧段完成依然不能撤销。
	if _, _, err := s.revokeCompletion("EQ-1", oldSeq, "再试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成仍应不能撤销，得到 %v", err)
	}
	// 同周期重新完成生成新序号；旧序号不能误撤销新完成。
	_, _, seq3, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-04", "重新完成")
	if err != nil {
		t.Fatal(err)
	}
	if seq3 == seq1 {
		t.Fatal("重新完成应生成新序号")
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "旧序号"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号的再次撤销应被拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-04-02" {
		t.Fatalf("旧序号撤销不应影响新完成，下一到期日 = %q", got.NextDue)
	}
	// 原完成的内容、日期、时间及撤销状态保留。
	events := s.eventsOf("EQ-1")
	var oldDone, redone *Event
	revoked := s.revokedDoneSeqs()
	for i := range events {
		if events[i].Seq == oldSeq {
			oldDone = &events[i]
		}
		if events[i].Seq == seq3 {
			redone = &events[i]
		}
	}
	if oldDone == nil || oldDone.Content != "旧段完成" || oldDone.Done != "2026-01-02" || oldDone.Time.IsZero() {
		t.Fatalf("旧段完成履历应保留: %+v", oldDone)
	}
	if !revoked[seq1] || !revoked[seq2] || revoked[seq3] || revoked[oldSeq] {
		t.Fatalf("撤销状态不对: %v", revoked)
	}
	if redone == nil || redone.Content != "重新完成" {
		t.Fatalf("重新完成的履历应保留: %+v", redone)
	}
	// 重启后段边界与撤销规则保持。
	s = saveAndReopen(t, s)
	if _, _, err := s.revokeCompletion("EQ-1", oldSeq, "重开后试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("重开后旧段完成仍应不能撤销，得到 %v", err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq3, "重开后撤销当前段"); err != nil {
		t.Fatalf("重开后应能撤销当前段最新完成: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-02-01" {
		t.Fatalf("重开后恢复下一到期日 = %q，想得到 2026-02-01", got.NextDue)
	}
}

// 调整后 maintain 只完成当前段下一周期，按本段首次日加整数倍间隔推进到严格
// 晚于完成日的最早日期；日期越界整次拒绝。
func TestAdjustSegmentCompleteDelayed(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 7, "方案升级"); err != nil {
		t.Fatal(err)
	}
	// 旧段的下一到期日不再可登记：所填到期日须等于当前下一到期日 2026-02-01。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-02-05", "旧周期"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段周期应不能登记，得到 %v", err)
	}
	// 延期到 2026-02-20 完成：按本段首次日 2026-02-01 加整数倍 7 天推进，
	// 严格晚于 02-20 的最早日期为 2026-02-22（02-01 + 3*7）。
	p, next, _, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-20", "已更换")
	if err != nil {
		t.Fatalf("completePlan: %v", err)
	}
	if next != "2026-02-22" || p.NextDue != "2026-02-22" {
		t.Fatalf("下一到期日 = %q，想得到 2026-02-22", next)
	}
	// 日期越界整次拒绝：完成日接近年末，下一到期日超出 9999-12-31。
	if _, _, err := s.adjustPlan("EQ-1", "年末方案", "9999-12-30", 10, "年末调整"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "9999-12-30", "9999-12-30", "越界"); !errors.Is(err, errConflict) {
		t.Fatalf("下一到期日越界应整次拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "9999-12-30" {
		t.Fatalf("越界拒绝不应推进下一到期日，得到 %q", got.NextDue)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("越界拒绝不应产生完成履历，履历数 = %d", got)
	}
}

// 维修中、停用时也允许调整；调整不改变资产状态、工单或请求绑定。
func TestAdjustWhileRepairingOrDeactivated(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	// 维修中允许调整，资产状态与工单不变。
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "维修中调整", "2026-12-01", 30, "理由"); err != nil {
		t.Fatalf("维修中调整应成功: %v", err)
	}
	if a := s.findAsset("EQ-1"); a.Status != statusRepairing {
		t.Fatalf("调整不应改变资产状态: %s", a.Status)
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != tk.ID {
		t.Fatal("调整不应改变工单")
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	// 停用时允许调整，但停用资产仍不参与 due、不能登记完成。
	if _, err := s.deactivateAsset("EQ-1", "设备调拨"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "停用中调整", "2027-01-01", 60, "理由"); err != nil {
		t.Fatalf("停用中调整应成功: %v", err)
	}
	if a := s.findAsset("EQ-1"); a.Status != statusDeactivated {
		t.Fatalf("调整不应改变资产状态: %s", a.Status)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2027-01-01", "2027-01-01", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间不能登记完成，得到 %v", err)
	}
	var buf bytes.Buffer
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2027-06-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "EQ-1") {
		t.Fatalf("停用资产不应参与 due:\n%s", buf.String())
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
}

// 导入接续：源台账含多次调整与跨段完成，导入后段边界、顺序与当前方案保持，
// 可继续调整、完成与撤销；源只读，目标原有记录不变。
func TestImportWithAdjustContinuation(t *testing.T) {
	src := newTestStore(t)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	_, _, oldSeq, err := src.completePlan("EQ-1", "2026-01-01", "2026-01-05", "旧段完成")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 60, "方案升级"); err != nil {
		t.Fatal(err)
	}
	_, _, curSeq, err := src.completePlan("EQ-1", "2026-02-01", "2026-02-03", "当前段完成")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.save(); err != nil {
		t.Fatal(err)
	}
	srcRaw, err := os.ReadFile(filepath.Join(src.dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 目标已有其他资产的记录。
	dst := newTestStore(t)
	if _, err := dst.registerAsset("EQ-9", "空调", "三楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.createPlan("EQ-9", "清洗滤网", "2026-03-01", 15); err != nil {
		t.Fatal(err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}

	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("importAssets: %v", err)
	}
	if outcome.plans != 1 || len(outcome.completions) != 2 {
		t.Fatalf("导入结果不对: %+v", outcome)
	}
	// 源文件字节不变。
	got, err := os.ReadFile(filepath.Join(src.dir, dataFileName))
	if err != nil || !bytes.Equal(got, srcRaw) {
		t.Fatal("源台账应保持只读")
	}
	d, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	// 目标原有记录不变。
	if p := d.findPlan("EQ-9"); p == nil || p.Content != "清洗滤网" || p.NextDue != "2026-03-01" {
		t.Fatalf("目标原有计划不应改变: %+v", p)
	}
	// 当前方案与下一到期日原样复制。
	p := d.findPlan("EQ-1")
	if p == nil || p.Content != "更换高效滤芯" || p.FirstDue != "2026-02-01" ||
		p.IntervalDays != 60 || p.NextDue != "2026-04-02" {
		t.Fatalf("导入后计划不对: %+v", p)
	}
	// 段边界保持：旧段完成（重编号后）不能撤销；当前段完成可撤销。
	var newOldSeq, newCurSeq int
	for _, m := range outcome.completions {
		if m.OldSeq == oldSeq {
			newOldSeq = m.NewSeq
		}
		if m.OldSeq == curSeq {
			newCurSeq = m.NewSeq
		}
	}
	if newOldSeq == 0 || newCurSeq == 0 || newOldSeq == newCurSeq {
		t.Fatalf("完成序号映射不对: %+v", outcome.completions)
	}
	if _, _, err := d.revokeCompletion("EQ-1", newOldSeq, "试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("导入后旧段完成应不能撤销，得到 %v", err)
	}
	if _, _, err := d.revokeCompletion("EQ-1", newCurSeq, "误登记"); err != nil {
		t.Fatalf("导入后应能撤销当前段完成: %v", err)
	}
	if got := d.findPlan("EQ-1"); got.NextDue != "2026-02-01" {
		t.Fatalf("撤销后下一到期日 = %q，想得到 2026-02-01", got.NextDue)
	}
	// 导入后可继续完成与调整。
	if _, next, _, err := d.completePlan("EQ-1", "2026-02-01", "2026-02-10", "导入后完成"); err != nil || next != "2026-04-02" {
		t.Fatalf("导入后完成: next=%q err=%v", next, err)
	}
	if _, _, err := d.adjustPlan("EQ-1", "再次升级", "2026-05-01", 45, "导入后调整"); err != nil {
		t.Fatalf("导入后调整: %v", err)
	}
	if err := d.save(); err != nil {
		t.Fatal(err)
	}
	// 重载后仍一致。
	if _, err := openStore(dst.dir); err != nil {
		t.Fatalf("导入后继续操作应通过一致性检查: %v", err)
	}
}

// 矛盾台账：调整履历与当时方案不符、新首次日未晚于有效完成日、保存方案与
// 履历推出的当前方案不符、撤销旧段完成等，所有读写命令拒绝并保留原文件。
func TestContradictoryAdjustLedger(t *testing.T) {
	// 合法基底：建立 + 完成 + 调整，履历序号有间隔、数组乱序仍合法。
	base := `{
	  "version": 1,
	  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"可用"}],
	  "tickets": [],
	  "events": [
	    {"seq":7,"asset_id":"EQ-1","kind":"保养调整","content":"方案升级","due":"2026-02-01","interval":60,"new_content":"更换高效滤芯","old_content":"更换滤芯","old_due":"2026-01-01","old_interval":30,"old_next_due":"2026-01-31","time":"2026-02-01T08:00:00Z"},
	    {"seq":2,"asset_id":"EQ-1","kind":"保养建立","content":"更换滤芯","due":"2026-01-01","interval":30,"time":"2026-01-01T08:00:00Z"},
	    {"seq":5,"asset_id":"EQ-1","kind":"保养完成","content":"已更换","due":"2026-01-01","done":"2026-01-10","time":"2026-01-10T08:00:00Z"}
	  ],
	  "requests": [],
	  "plans": [{"asset_id":"EQ-1","content":"更换高效滤芯","first_due":"2026-02-01","interval_days":60,"next_due":"2026-02-01"}],
	  "next_ticket_seq": 1
	}`
	write := func(t *testing.T, dir string, mutate func(m map[string]any)) []byte {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(base), &m); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(m)
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	events := func(m map[string]any) []any { return m["events"].([]any) }
	adjust := func(m map[string]any) map[string]any { return events(m)[0].(map[string]any) }

	// 合法基底可直接使用：数组乱序、序号间隔合法。
	dir := t.TempDir()
	write(t, dir, nil)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if p := s.findPlan("EQ-1"); p.NextDue != "2026-02-01" || p.IntervalDays != 60 {
		t.Fatalf("合法台账计划不对: %+v", p)
	}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"调整记录的原方案与当时方案不符", func(m map[string]any) {
			adjust(m)["old_interval"] = 31
		}},
		{"调整记录的原下一到期日不符", func(m map[string]any) {
			adjust(m)["old_next_due"] = "2026-01-30"
		}},
		{"新首次日未严格晚于有效完成日", func(m map[string]any) {
			adjust(m)["due"] = "2026-01-10"
			m["plans"].([]any)[0].(map[string]any)["first_due"] = "2026-01-10"
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-01-10"
		}},
		{"保存方案与履历推出的当前方案不符", func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["interval_days"] = 61
		}},
		{"保存的下一到期日与履历不符", func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-02-02"
		}},
		{"撤销旧段完成", func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 9, "asset_id": "EQ-1", "kind": "保养撤销",
				"content": "撤销旧段", "target_seq": 5, "time": "2026-02-02T08:00:00Z",
			})
		}},
		{"调整后完成履历不接续当前段", func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 9, "asset_id": "EQ-1", "kind": "保养完成",
				"content": "旧段周期", "due": "2026-01-31", "done": "2026-02-05",
				"time": "2026-02-05T08:00:00Z",
			})
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-04-02"
		}},
		{"调整履历缺少原方案字段", func(m map[string]any) {
			delete(adjust(m), "old_content")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := write(t, dir, tc.mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			}
			// 所有读写命令拒绝：查询与写入都以退出码 1 失败。
			var out, errBuf bytes.Buffer
			if code := run([]string{"detail", "--data-dir", dir, "--asset-id", "EQ-1"}, &out, &errBuf); code != 1 {
				t.Fatalf("detail 应以退出码 1 失败，得到 %d", code)
			}
			out.Reset()
			if code := run([]string{"adjust", "--data-dir", dir, "--asset-id", "EQ-1",
				"--content", "x", "--first-due", "2027-01-01", "--interval-days", "30", "--reason", "r"},
				&out, &errBuf); code != 1 {
				t.Fatalf("adjust 应以退出码 1 失败，得到 %d", code)
			}
			// 原文件字节保留，不自动修复。
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("矛盾台账的原文件应保持不变")
			}
		})
	}
}

// 失败重载：失败的调整不产生履历、不消耗序号、不改变计划；成功调整原子保存，
// 重载后方案与段边界保持。
func TestAdjustFailureAndReload(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-10", "已更换"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(s.dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(s.data.Events)

	// 失败的调整：新首次日未严格晚于有效完成日。
	if _, _, err := s.adjustPlan("EQ-1", "新内容", "2026-01-10", 60, "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("应拒绝，得到 %v", err)
	}
	if len(s.data.Events) != eventCount {
		t.Fatal("失败的调整不应产生履历")
	}
	if p := s.findPlan("EQ-1"); p.Content != "更换滤芯" || p.NextDue != "2026-01-31" {
		t.Fatalf("失败的调整不应改变计划: %+v", p)
	}
	// 失败不写文件：磁盘上的台账字节保持不变。
	gotRaw, err := os.ReadFile(filepath.Join(s.dir, dataFileName))
	if err != nil || !bytes.Equal(gotRaw, before) {
		t.Fatal("失败的调整不应改动台账文件")
	}
	// 不消耗序号：随后的成功操作使用紧接着的序号。
	p, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-01-11", 60, "型号升级")
	if err != nil {
		t.Fatalf("adjustPlan: %v", err)
	}
	events := s.eventsOf("EQ-1")
	last := events[len(events)-1]
	if last.Kind != eventPlanAdjust || last.Seq != events[len(events)-2].Seq+1 {
		t.Fatalf("失败不应消耗序号: %+v", last)
	}
	if p.NextDue != "2026-01-11" {
		t.Fatalf("下一到期日 = %q，想得到 2026-01-11", p.NextDue)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	// 失败本身未写文件：保存前文件字节与之前一致（由后续成功保存覆盖）。
	s = saveAndReopen(t, s)
	if p := s.findPlan("EQ-1"); p.Content != "更换高效滤芯" || p.FirstDue != "2026-01-11" ||
		p.IntervalDays != 60 || p.NextDue != "2026-01-11" {
		t.Fatalf("重载后方案未保持: %+v", p)
	}
	if got := len(s.eventsOf("EQ-1")); got != 3 {
		t.Fatalf("重载后履历数 = %d，想得到 3", got)
	}
}

// 命令行入口：参数错误退出码 2，业务失败退出码 1，成功输出新方案与日期。
func TestCmdAdjustExitCodes(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	if code := run([]string{"register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼"}, &out, &errBuf); code != 0 {
		t.Fatalf("register: %d %s", code, errBuf.String())
	}
	if code := run([]string{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "90"}, &out, &errBuf); code != 0 {
		t.Fatalf("plan: %d %s", code, errBuf.String())
	}
	// 参数错误：缺理由、无效日期、非正间隔、缺内容。
	for _, args := range [][]string{
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2027-01-01", "--interval-days", "30"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2027-13-01", "--interval-days", "30", "--reason", "r"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "x", "--first-due", "2027-01-01", "--interval-days", "0", "--reason", "r"},
		{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--first-due", "2027-01-01", "--interval-days", "30", "--reason", "r"},
	} {
		out.Reset()
		if code := run(args, &out, &errBuf); code != 2 {
			t.Fatalf("%v 应以退出码 2 失败，得到 %d", args, code)
		}
	}
	// 业务失败：未知资产、无计划。
	out.Reset()
	if code := run([]string{"adjust", "--data-dir", dir, "--asset-id", "NOPE", "--content", "x",
		"--first-due", "2027-01-01", "--interval-days", "30", "--reason", "r"}, &out, &errBuf); code != 1 {
		t.Fatalf("未知资产应以退出码 1 失败，得到 %d", code)
	}
	if code := run([]string{"register", "--data-dir", dir, "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼"}, &out, &errBuf); code != 0 {
		t.Fatal(code)
	}
	if code := run([]string{"adjust", "--data-dir", dir, "--asset-id", "EQ-2", "--content", "x",
		"--first-due", "2027-01-01", "--interval-days", "30", "--reason", "r"}, &out, &errBuf); code != 1 {
		t.Fatalf("无计划应以退出码 1 失败，得到 %d", code)
	}
	// 成功：输出新方案与日期。
	out.Reset()
	if code := run([]string{"adjust", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换高效滤芯",
		"--first-due", "2027-01-01", "--interval-days", "60", "--reason", "型号升级"}, &out, &errBuf); code != 0 {
		t.Fatalf("adjust 应成功，得到 %d: %s", code, errBuf.String())
	}
	for _, want := range []string{"保养内容: 更换高效滤芯", "保养间隔: 每 60 天", "首次到期日: 2027-01-01", "下一到期日: 2027-01-01"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("输出缺少 %q:\n%s", want, out.String())
		}
	}
	// plan 仍不能覆盖已有计划。
	if code := run([]string{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "覆盖",
		"--first-due", "2028-01-01", "--interval-days", "30"}, &out, &errBuf); code != 1 {
		t.Fatalf("已有计划时 plan 应拒绝，得到 %d", code)
	}
}
