package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// eventSeqs 返回资产指定类型履历的全库序号（按序号升序）。
func eventSeqs(s *store, assetID, kind string) []int {
	var seqs []int
	for _, e := range s.data.Events {
		if e.AssetID == assetID && e.Kind == kind {
			seqs = append(seqs, e.Seq)
		}
	}
	return seqs
}

func lastSeq(seqs []int) int {
	return seqs[len(seqs)-1]
}

// 初态（序号 0）：可用、无工单、无计划；初始位置取最早位置变更履历的原位
// 置，没有位置履历则取保存位置。
func TestReplayInitialState(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "三楼仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "一楼大厅", "入库安装"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	// 有位置履历：初态位置为最早一条位置变更履历的原位置。
	snap := s.replayAsset("EQ-1", 0)
	if snap.status != statusAvailable || snap.openTicket != nil || snap.plan != nil {
		t.Fatalf("初态应为可用/无工单/无计划，得到 %+v", snap)
	}
	if snap.location != "三楼仓库" {
		t.Fatalf("初态位置应为最早搬移的原位置 三楼仓库，得到 %q", snap.location)
	}

	// 无位置履历的资产：初态位置取保存位置。
	s2 := newStoreAt(t, filepath.Join(t.TempDir(), "d2"))
	if _, err := s2.registerAsset("EQ-2", "空调", "二楼机房"); err != nil {
		t.Fatal(err)
	}
	snap2 := s2.replayAsset("EQ-2", 0)
	if snap2.location != "二楼机房" || snap2.status != statusAvailable {
		t.Fatalf("无履历资产初态应为保存位置/可用，得到 %+v", snap2)
	}
}

// 工单全链路回看：报修、派工、搬移、转派、关闭、再报修，截止之后的事件
// 不得提前影响；维修中搬移改变资产位置但不改旧单报修地点。
func TestReplayTicketLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil { // T0001 seq1
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil { // seq2
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "工位调整"); err != nil { // seq3
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "李四", "转派"); err != nil { // seq4
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil { // seq5
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil { // T0002 seq6
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0002", "王五", "派工新单"); err != nil { // seq7
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0002", "误报，设备正常"); err != nil { // seq8
		t.Fatal(err)
	}
	mustSave(t, s)

	check := func(cutoff int, wantStatus, wantLocation, wantTicket, wantAssignee, wantReport string) {
		t.Helper()
		snap := s.replayAsset("EQ-1", cutoff)
		if snap.status != wantStatus || snap.location != wantLocation {
			t.Fatalf("截止 %d 状态/位置 = %s/%s，想得到 %s/%s", cutoff, snap.status, snap.location, wantStatus, wantLocation)
		}
		if wantTicket == "" {
			if snap.openTicket != nil {
				t.Fatalf("截止 %d 应无未关闭工单，得到 %+v", cutoff, snap.openTicket)
			}
			return
		}
		if snap.openTicket == nil {
			t.Fatalf("截止 %d 应有未关闭工单 %s", cutoff, wantTicket)
		}
		if snap.openTicket.id != wantTicket || snap.openTicket.assignee != wantAssignee ||
			snap.openTicket.reportLocation != wantReport {
			t.Fatalf("截止 %d 工单回看 = %+v，想得到 %s/%s/%s",
				cutoff, snap.openTicket, wantTicket, wantAssignee, wantReport)
		}
	}

	check(0, statusAvailable, "一楼", "", "", "")
	check(1, statusRepairing, "一楼", "T0001", "", "一楼")
	check(2, statusRepairing, "一楼", "T0001", "张三", "一楼")
	// 维修中搬移：资产位置随搬移改变，旧单报修地点保持。
	check(3, statusRepairing, "二楼", "T0001", "张三", "一楼")
	// 转派后负责人为李四；截止落在序号间隔中（3 与 4 之间）仍取张三。
	check(4, statusRepairing, "二楼", "T0001", "李四", "一楼")
	// 关闭后可用、无工单。
	check(5, statusAvailable, "二楼", "", "", "")
	// 新报修采用新地点；派工后负责人为王五。
	check(6, statusRepairing, "二楼", "T0002", "", "二楼")
	check(7, statusRepairing, "二楼", "T0002", "王五", "二楼")
	// 取消后恢复可用、无工单，旧负责人随单终结清空。
	check(8, statusAvailable, "二楼", "", "", "")
	// 截止值超过全库最大序号按全部履历处理。
	check(1_000_000, statusAvailable, "二楼", "", "", "")

	// 即使当前负责人/状态已是最终值，回看不能直接采用保存值：截止 2 时
	// T0001 仍未关闭、负责人为张三。
	if t1 := s.findTicket("T0001"); t1.Status != ticketClosed || t1.Assignee != "李四" {
		t.Fatalf("测试前提：T0001 最终应为已关闭/李四，得到 %+v", t1)
	}
}

// 保养方案段回看：建立、延期完成（跳过周期）、撤销恢复周期、同到期日以新
// 序号重新完成、调整后采用新方案；截止之后的撤销或调整不提前影响。
func TestReplayPlanSegments(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 30); err != nil {
		t.Fatal(err)
	}
	createSeq := lastSeq(eventSeqs(s, "EQ-1", eventPlanCreate))
	if _, _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-01", "ok"); err != nil {
		t.Fatal(err)
	}
	d1 := lastSeq(eventSeqs(s, "EQ-1", eventPlanDone))
	// 延期完成：跨过 2027-01-01，下一到期日为严格晚于完成日的最早日期 2027-01-30。
	if _, _, _, err := s.completePlan("EQ-1", "2026-12-01", "2027-01-10", "ok2"); err != nil {
		t.Fatal(err)
	}
	d2 := eventSeqs(s, "EQ-1", eventPlanDone)[1]
	if _, _, err := s.revokeCompletion("EQ-1", d2, "误登记"); err != nil {
		t.Fatal(err)
	}
	rSeq := lastSeq(eventSeqs(s, "EQ-1", eventPlanRevoke))
	// 同一到期日重新完成，按新序号区分；下一到期日推进到 2026-12-31。
	if _, _, _, err := s.completePlan("EQ-1", "2026-12-01", "2026-12-05", "ok3"); err != nil {
		t.Fatal(err)
	}
	d3 := eventSeqs(s, "EQ-1", eventPlanDone)[2]
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2027-03-01", 60, "型号升级"); err != nil {
		t.Fatal(err)
	}
	adjSeq := lastSeq(eventSeqs(s, "EQ-1", eventPlanAdjust))
	mustSave(t, s)

	// 建立之前无计划。
	if snap := s.replayAsset("EQ-1", createSeq-1); snap.plan != nil {
		t.Fatalf("建立前应无计划，得到 %+v", snap.plan)
	}
	snap := s.replayAsset("EQ-1", createSeq)
	if snap.plan == nil || snap.plan.content != "更换滤芯" || snap.plan.firstDue != "2026-11-01" ||
		snap.plan.interval != 30 || snap.plan.nextDue != "2026-11-01" {
		t.Fatalf("建立后方案段不对: %+v", snap.plan)
	}
	if p := s.replayAsset("EQ-1", d1).plan; p.nextDue != "2026-12-01" {
		t.Fatalf("首次完成后下一到期日应为 2026-12-01，得到 %s", p.nextDue)
	}
	if p := s.replayAsset("EQ-1", d2).plan; p.nextDue != "2027-01-30" {
		t.Fatalf("延期完成应跳过周期到 2027-01-30，得到 %s", p.nextDue)
	}
	// 撤销按目标完成序号恢复周期。
	if p := s.replayAsset("EQ-1", rSeq).plan; p.nextDue != "2026-12-01" {
		t.Fatalf("撤销后应恢复周期到 2026-12-01，得到 %s", p.nextDue)
	}
	// 同到期日重新完成按新序号区分并正常推进。
	if p := s.replayAsset("EQ-1", d3).plan; p.nextDue != "2026-12-31" {
		t.Fatalf("重新完成后下一到期日应为 2026-12-31，得到 %s", p.nextDue)
	}
	// 调整后采用新方案，下一到期日即新首次到期日。
	p := s.replayAsset("EQ-1", adjSeq).plan
	if p.content != "更换高效滤芯" || p.firstDue != "2027-03-01" || p.interval != 60 || p.nextDue != "2027-03-01" {
		t.Fatalf("调整后方案段不对: %+v", p)
	}
	// 调整前一序号仍是旧方案。
	if p := s.replayAsset("EQ-1", adjSeq-1).plan; p.content != "更换滤芯" || p.nextDue != "2026-12-31" {
		t.Fatalf("调整不应提前影响回看: %+v", p)
	}
}

// 停用期间仍显示保存的保养方案与到期日；截止之后的恢复不影响。
func TestReplayDeactivatedKeepsPlan(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 30); err != nil {
		t.Fatal(err)
	}
	if a, err := s.deactivateAsset("EQ-1", "设备调拨"); err != nil {
		t.Fatal(err)
	} else if a.Status != statusDeactivated {
		t.Fatalf("停用前提不对: %s", a.Status)
	}
	offSeq := lastSeq(eventSeqs(s, "EQ-1", eventDeactivate))
	if _, err := s.reactivateAsset("EQ-1", "调拨回库"); err != nil {
		t.Fatal(err)
	}
	onSeq := lastSeq(eventSeqs(s, "EQ-1", eventReactivate))
	mustSave(t, s)

	snap := s.replayAsset("EQ-1", offSeq)
	if snap.status != statusDeactivated {
		t.Fatalf("截止停用处应为停用，得到 %s", snap.status)
	}
	if snap.plan == nil || snap.plan.nextDue != "2026-11-01" || snap.plan.content != "更换滤芯" {
		t.Fatalf("停用期间仍应显示保养方案与到期日，得到 %+v", snap.plan)
	}
	if got := s.replayAsset("EQ-1", onSeq).status; got != statusAvailable {
		t.Fatalf("恢复后应为可用，得到 %s", got)
	}
}

// 履历数组乱序、序号间隔、时间不递增仍按全库序号回看。
func TestReplayUnorderedEvents(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "派工"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	want := s.replayAsset("EQ-1", 2)

	// 逆序存放履历数组（不重新保存，仅在内存中乱序），回看结果须一致。
	for i, j := 0, len(s.data.Events)-1; i < j; i, j = i+1, j-1 {
		s.data.Events[i], s.data.Events[j] = s.data.Events[j], s.data.Events[i]
	}
	got := s.replayAsset("EQ-1", 2)
	if got.status != want.status || got.location != want.location ||
		got.openTicket == nil || got.openTicket.id != "T0001" || got.openTicket.assignee != "张三" {
		t.Fatalf("乱序履历回看结果不对: %+v", got)
	}
}

// 整库一致性检查：矛盾位于截止之后，或位于其他资产，都整次拒绝，不输出
// 部分摘要。
func TestReplayRejectsInconsistentStore(t *testing.T) {
	// 矛盾位于截止之后：seq10 有关闭履历，工单却保存为未关闭。
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		appendTo(m, "events", map[string]any{
			"seq": 10, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "关闭", "content": "已修复", "time": "2026-10-02T10:00:00Z",
		})
	})
	var out, errBuf bytes.Buffer
	if code := run([]string{"replay", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "3"}, &out, &errBuf); code != 1 {
		t.Fatalf("截止之后的矛盾也应整次拒绝，code=%d err=%s", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Fatalf("整次拒绝不应输出部分摘要: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "矛盾") {
		t.Fatalf("错误应说明问题类别: %s", errBuf.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, dataFileName)); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("被拒绝的回看不应改动原文件")
	}

	// 矛盾位于其他资产：回看 EQ-1 也必须拒绝。
	dir2 := t.TempDir()
	writeLedger(t, dir2, func(m map[string]any) {
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "维修中",
		})
	})
	var out2, errBuf2 bytes.Buffer
	if code := run([]string{"replay", "--data-dir", dir2, "--asset-id", "EQ-1", "--seq", "0"}, &out2, &errBuf2); code != 1 {
		t.Fatalf("其他资产的矛盾也应整次拒绝，code=%d err=%s", code, errBuf2.String())
	}
	if out2.Len() != 0 {
		t.Fatalf("整次拒绝不应输出部分摘要: %q", out2.String())
	}
}

// 参数错误退出 2；未知资产业务失败退出 1。
func TestReplayCLIErrors(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	mkRun := func(args ...string) int {
		out.Reset()
		errBuf.Reset()
		full := append([]string{args[0], "--data-dir", dir}, args[1:]...)
		return run(full, &out, &errBuf)
	}
	if code := mkRun("replay", "--asset-id", "EQ-1"); code != 2 {
		t.Fatalf("缺少 --seq 应退出 2，得到 %d", code)
	}
	if code := mkRun("replay", "--asset-id", "EQ-1", "--seq", "-1"); code != 2 {
		t.Fatalf("--seq 为负数应退出 2，得到 %d", code)
	}
	if code := mkRun("replay", "--asset-id", "EQ-1", "--seq", "x"); code != 2 {
		t.Fatalf("--seq 非整数应退出 2，得到 %d", code)
	}
	if code := mkRun("replay", "--seq", "0"); code != 2 {
		t.Fatalf("缺少 --asset-id 应退出 2，得到 %d", code)
	}
	if code := mkRun("replay", "--asset-id", "NOPE", "--seq", "0"); code != 1 {
		t.Fatalf("未知资产应退出 1，得到 %d (%s)", code, errBuf.String())
	}
}

// 成功输出包含全部规定条目；无工单、无计划明确提示，未派工显示“未派工”。
func TestReplayCLISuccessOutput(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	runOrFail := func(codeWant int, args ...string) string {
		t.Helper()
		out.Reset()
		errBuf.Reset()
		full := append([]string{args[0], "--data-dir", dir}, args[1:]...)
		if code := run(full, &out, &errBuf); code != codeWant {
			t.Fatalf("run %v code=%d 想得到 %d: %s", full, code, codeWant, errBuf.String())
		}
		return out.String()
	}

	runOrFail(0, "register", "--asset-id", "EQ-001", "--name", "打印机", "--location", "一楼")

	// 序号 0：初态摘要，无工单与无计划分别提示。
	o := runOrFail(0, "replay", "--asset-id", "EQ-001", "--seq", "0")
	for _, want := range []string{
		"资产编号: EQ-001", "名称: 打印机", "截止序号: 0",
		"当时位置: 一楼", "当时状态: 可用",
		"当时未关闭工单: 无", "当时保养计划: 无",
	} {
		if !strings.Contains(o, want) {
			t.Fatalf("初态回看缺少 %q:\n%s", want, o)
		}
	}

	// 报修后：未派工显示“未派工”，显示报修地点。
	runOrFail(0, "report", "--asset-id", "EQ-001", "--description", "卡纸", "--request-id", "req-1")
	runOrFail(0, "move", "--asset-id", "EQ-001", "--location", "二楼", "--reason", "工位调整")
	o = runOrFail(0, "replay", "--asset-id", "EQ-001", "--seq", "100")
	for _, want := range []string{
		"当时位置: 二楼", "当时状态: 维修中",
		"当时未关闭工单: T0001", "工单负责人: 未派工", "报修地点: 一楼",
	} {
		if !strings.Contains(o, want) {
			t.Fatalf("维修中回看缺少 %q:\n%s", want, o)
		}
	}

	// 停用 + 计划：停用期间仍显示方案与到期日。
	runOrFail(0, "close", "--ticket-id", "T0001", "--repair-result", "已修复")
	runOrFail(0, "plan", "--asset-id", "EQ-001", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "30")
	runOrFail(0, "deactivate", "--asset-id", "EQ-001", "--reason", "设备调拨")
	o = runOrFail(0, "replay", "--asset-id", "EQ-001", "--seq", "100")
	for _, want := range []string{
		"当时状态: 停用", "保养内容: 更换滤芯", "首次到期日: 2026-11-01",
		"保养间隔: 每 30 天", "下一到期日: 2026-11-01", "当时未关闭工单: 无",
	} {
		if !strings.Contains(o, want) {
			t.Fatalf("停用回看缺少 %q:\n%s", want, o)
		}
	}
}

// 只读：目录不存在时不初始化、不写文件；成功或失败都不改原文件字节，
// 后续日常操作仍从真实当前状态继续。
func TestReplayReadOnly(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "not-created")
	var out, errBuf bytes.Buffer
	// 未知资产在不存在的目录上失败退出 1，但不得初始化目录。
	if code := run([]string{"replay", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "0"}, &out, &errBuf); code != 1 {
		t.Fatalf("空目录未知资产应退出 1，得到 %d: %s", code, errBuf.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("回看不应初始化数据目录，Stat err=%v", err)
	}

	// 成功回看不改文件字节。
	okDir := t.TempDir()
	s := newStoreAt(t, okDir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, okDir)
	if code := run([]string{"replay", "--data-dir", okDir, "--asset-id", "EQ-1", "--seq", "0"}, &out, &errBuf); code != 0 {
		t.Fatalf("回看失败: %s", errBuf.String())
	}
	if after := readFileBytes(t, okDir); !bytes.Equal(after, before) {
		t.Fatal("回看不应改动数据文件")
	}
	// 后续日常操作仍从真实当前状态继续：T0001 仍未关闭，新报修冲突。
	s2, err := openStore(okDir)
	if err != nil {
		t.Fatal(err)
	}
	if t1 := s2.findTicket("T0001"); t1 == nil || t1.Status != ticketOpen {
		t.Fatalf("回看后当前业务状态应保持，得到 %+v", t1)
	}
}

// history 为每条事件补充全库序号，保留原有内容与排序。
func TestHistoryShowsGlobalSeq(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	must := func(args ...string) {
		t.Helper()
		out.Reset()
		errBuf.Reset()
		if code := run(append([]string{args[0], "--data-dir", dir}, args[1:]...), &out, &errBuf); code != 0 {
			t.Fatalf("run %v: %s", args, errBuf.String())
		}
	}
	must("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	must("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1") // seq1
	must("assign", "--ticket-id", "T0001", "--assignee", "张三", "--note", "首次派工")         // seq2
	must("history", "--asset-id", "EQ-1")
	o := out.String()
	if !strings.Contains(o, "序号 1: 报修 工单 T0001") {
		t.Fatalf("history 应为报修事件标注全库序号 1:\n%s", o)
	}
	if !strings.Contains(o, "序号 2: 派工 工单 T0001") {
		t.Fatalf("history 应为派工事件标注全库序号 2:\n%s", o)
	}
	if strings.Index(o, "序号 1") > strings.Index(o, "序号 2") {
		t.Fatalf("history 应保持序号升序:\n%s", o)
	}
}

// import 后按目标履历序号和映射后的工单编号回看。
func TestReplayAfterImport(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-I", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-I", "卡纸", "req-i1"); err != nil { // T0001 seq1
		t.Fatal(err)
	}
	if _, err := src.assignTicket("T0001", "张三", "首次派工"); err != nil { // seq2
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标库预置一项资产与一张工单，使导入编号映射为 T0001 -> T0002、
	// 履历序号 1/2 -> 2/3。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-Z", "饮水机", "四楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-Z", "漏水", "req-z1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-I"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	var mapped string
	for _, m := range outcome.tickets {
		if m.OldID == "T0001" {
			mapped = m.NewID
		}
	}
	if mapped != "T0002" {
		t.Fatalf("工单编号应映射为 T0002，得到 %q", mapped)
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	reportSeqs := eventSeqs(s, "EQ-I", eventReport)
	if len(reportSeqs) != 1 || reportSeqs[0] != 2 {
		t.Fatalf("导入后报修履历序号应为 2，得到 %v", reportSeqs)
	}
	// 按目标序号回看：seq2 时新映射工单未派工。
	snap := s.replayAsset("EQ-I", 2)
	if snap.openTicket == nil || snap.openTicket.id != "T0002" || snap.openTicket.assignee != "" {
		t.Fatalf("按目标序号回看应为未派工的 T0002，得到 %+v", snap.openTicket)
	}
	if snap.openTicket.reportLocation != "一楼" {
		t.Fatalf("导入后报修地点应保持 一楼，得到 %q", snap.openTicket.reportLocation)
	}
	// seq3 时负责人为映射后工单的张三。
	snap = s.replayAsset("EQ-I", 3)
	if snap.openTicket == nil || snap.openTicket.id != "T0002" || snap.openTicket.assignee != "张三" {
		t.Fatalf("派工履历导入后回看不对: %+v", snap.openTicket)
	}
}

// restore 后按保留序号回看，不新增业务履历。
func TestReplayAfterRestore(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	pkgPath := filepath.Join(t.TempDir(), "eq.zip")
	targetDir := filepath.Join(t.TempDir(), "restored")
	s := newStoreAt(t, srcDir)
	if _, err := s.registerAsset("EQ-R", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-R", "卡纸", "req-r1"); err != nil { // seq1
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-R", "二楼", "工位调整"); err != nil { // seq2
		t.Fatal(err)
	}
	mustSave(t, s)
	if _, err := exportPackage(srcDir, pkgPath, []string{"EQ-R"}); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if _, err := restorePackage(pkgPath, targetDir); err != nil {
		t.Fatalf("还原失败: %v", err)
	}

	r, err := openStore(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	// 序号原样保留：seq1 为报修、seq2 为搬移，没有新增业务履历。
	if seqs := eventSeqs(r, "EQ-R", eventReport); !reflect.DeepEqual(seqs, []int{1}) {
		t.Fatalf("还原后报修序号应保留为 1，得到 %v", seqs)
	}
	if seqs := eventSeqs(r, "EQ-R", eventRelocate); !reflect.DeepEqual(seqs, []int{2}) {
		t.Fatalf("还原后搬移序号应保留为 2，得到 %v", seqs)
	}
	snap := r.replayAsset("EQ-R", 1)
	if snap.status != statusRepairing || snap.location != "一楼" ||
		snap.openTicket == nil || snap.openTicket.reportLocation != "一楼" {
		t.Fatalf("还原后按保留序号回看（seq1）不对: %+v", snap)
	}
	snap = r.replayAsset("EQ-R", 2)
	if snap.location != "二楼" || snap.openTicket == nil || snap.openTicket.reportLocation != "一楼" {
		t.Fatalf("还原后按保留序号回看（seq2）不对: %+v", snap)
	}
}
