package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 延期完成的撤销回退：跨过的周期不补记录，下一到期日恢复为该完成的周期到期日；
// 原完成的日期、结果与时间保留，追加含目标序号、理由与操作时间的撤销履历；
// detail、due 反映回退；maintain 输出完成履历序号，history 显示序号与状态。
func TestUndoDelayedCompletion(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 延期到 2026-01-25 完成：跨过 01-11、01-21 两个周期，下一到期日 01-31。
	_, next, doneSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-25", "已更换")
	if err != nil || next != "2026-01-31" {
		t.Fatalf("completePlan: next=%q err=%v", next, err)
	}
	if doneSeq != 2 {
		t.Fatalf("完成履历序号 = %d，想得到 2", doneSeq)
	}
	mustSave(t, s)

	// maintain 命令输出完成履历序号。
	var buf bytes.Buffer
	if err := cmdMaintain([]string{"--data-dir", s.dir, "--asset-id", "EQ-1",
		"--due", "2026-01-31", "--done", "2026-01-31", "--result", "第二次"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "完成履历序号: 3") {
		t.Fatalf("maintain 应输出完成履历序号:\n%s", buf.String())
	}

	// 撤销第二次完成（序号 3）：下一到期日恢复为该完成的周期到期日 2026-01-31。
	// cmdMaintain 自己保存了数据，这里重新打开而不是保存旧的内存状态。
	reopened, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = s.now
	s = reopened
	p, restored, err := s.undoCompletion("EQ-1", 3, "误登记")
	if err != nil {
		t.Fatalf("undoCompletion: %v", err)
	}
	if restored != "2026-01-31" || p.NextDue != "2026-01-31" {
		t.Fatalf("恢复下一到期日 = %q，想得到 2026-01-31", restored)
	}
	// 再撤销第一次完成（序号 2，延期完成）：恢复为周期到期日 2026-01-01，
	// 跨过的 01-11、01-21 周期不补记录。
	if _, restored, err := s.undoCompletion("EQ-1", 2, "继续撤销"); err != nil || restored != "2026-01-01" {
		t.Fatalf("撤销延期完成: restored=%q err=%v", restored, err)
	}
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重启后下一到期日 = %q，想得到 2026-01-01", got.NextDue)
	}

	// 原完成保留日期、结果与时间；撤销履历含目标序号、理由与操作时间。
	events := s.eventsOf("EQ-1")
	if len(events) != 5 {
		t.Fatalf("撤销后履历条数 = %d，想得到 5（建立 + 两次完成 + 两次撤销）", len(events))
	}
	first := events[1]
	if first.Kind != eventPlanDone || first.Seq != 2 || first.Due != "2026-01-01" ||
		first.Done != "2026-01-25" || first.Content != "已更换" || first.Time.IsZero() {
		t.Fatalf("原完成履历不应被修改: %+v", first)
	}
	undo1 := events[3]
	if undo1.Kind != eventPlanUndo || undo1.UndoSeq != 3 || undo1.Content != "误登记" || undo1.Time.IsZero() {
		t.Fatalf("撤销履历不对: %+v", undo1)
	}
	undo2 := events[4]
	if undo2.Kind != eventPlanUndo || undo2.UndoSeq != 2 || undo2.Content != "继续撤销" {
		t.Fatalf("第二条撤销履历不对: %+v", undo2)
	}

	// detail、due 反映回退后的下一到期日。
	buf.Reset()
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "下一到期日: 2026-01-01") {
		t.Fatalf("detail 应反映回退:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-01-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "EQ-1\t打印机\t更换滤芯\t2026-01-01") {
		t.Fatalf("due 应反映回退:\n%s", buf.String())
	}

	// history 显示各次完成的序号及有效或已撤销状态，以及撤销履历。
	buf.Reset()
	if err := cmdHistory([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"保养完成（履历序号 2，已撤销）",
		"保养完成（履历序号 3，已撤销）",
		"保养撤销（履历序号 4）: 撤销保养完成履历序号 3（误登记）",
		"保养撤销（履历序号 5）: 撤销保养完成履历序号 2（继续撤销）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("history 输出缺少 %q:\n%s", want, out)
		}
	}
}

// 连续撤销：仅可撤销按序号最新的未撤销完成，存在更晚有效完成时拒绝；
// 撤销后可继续撤销此前最新有效完成；维修与其他资产的事件不阻止撤销。
func TestUndoChain(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 维修事件穿插其间：报修、完成第一周期、关闭工单、完成第二周期。
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, seq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "第一次"); err != nil || seq != 3 {
		t.Fatalf("第一周期: seq=%d err=%v", seq, err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, seq, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-11", "第二次"); err != nil || seq != 5 {
		t.Fatalf("第二周期: seq=%d err=%v", seq, err)
	}
	// 其他资产的保养事件不阻止撤销。
	if _, err := s.createPlan("EQ-2", "清洗", "2026-06-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-2", "2026-06-01", "2026-06-01", "已清洗"); err != nil {
		t.Fatal(err)
	}

	// 存在更晚有效完成（序号 5）时，撤销较早的完成（序号 3）拒绝。
	if _, _, err := s.undoCompletion("EQ-1", 3, "跳过最新"); !errors.Is(err, errConflict) {
		t.Fatalf("存在更晚有效完成时应拒绝，得到 %v", err)
	}
	// 撤销最新完成（序号 5）：恢复为 2026-01-11。
	if _, restored, err := s.undoCompletion("EQ-1", 5, "误登记"); err != nil || restored != "2026-01-11" {
		t.Fatalf("撤销最新完成: restored=%q err=%v", restored, err)
	}
	// 继续撤销此前最新有效完成（序号 3）：恢复为 2026-01-01。
	if _, restored, err := s.undoCompletion("EQ-1", 3, "继续撤销"); err != nil || restored != "2026-01-01" {
		t.Fatalf("连续撤销: restored=%q err=%v", restored, err)
	}
	// 重复撤销拒绝。
	for _, seq := range []int{3, 5} {
		if _, _, err := s.undoCompletion("EQ-1", seq, "再次"); !errors.Is(err, errConflict) {
			t.Fatalf("重复撤销序号 %d 应拒绝，得到 %v", seq, err)
		}
	}
	// 其他资产的完成不受影响，可独立撤销。
	if _, restored, err := s.undoCompletion("EQ-2", 7, "其他资产"); err != nil || restored != "2026-06-01" {
		t.Fatalf("其他资产撤销: restored=%q err=%v", restored, err)
	}
	// 重启后保持。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重启后 EQ-1 下一到期日 = %q", got.NextDue)
	}
	if got := s.findPlan("EQ-2"); got.NextDue != "2026-06-01" {
		t.Fatalf("重启后 EQ-2 下一到期日 = %q", got.NextDue)
	}
}

// 同周期重新完成：撤销后按原 maintain 规则重新完成生成新序号；
// 旧序号的再次撤销仍拒绝，不能误撤销新登记。
func TestUndoRecomplete(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, next, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-15", "误登记的一次"); err != nil || next != "2026-01-21" {
		t.Fatalf("completePlan: next=%q err=%v", next, err)
	}
	if _, restored, err := s.undoCompletion("EQ-1", 2, "误登记"); err != nil || restored != "2026-01-01" {
		t.Fatalf("撤销: restored=%q err=%v", restored, err)
	}
	// 恢复周期按原规则重新完成：同一周期到期日，生成新序号。
	_, next, seq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "实际完成")
	if err != nil || next != "2026-01-11" || seq != 4 {
		t.Fatalf("重新完成: next=%q seq=%d err=%v", next, seq, err)
	}
	// 旧序号（2）的再次撤销仍拒绝，且不误撤销新登记（序号 4）。
	if _, _, err := s.undoCompletion("EQ-1", 2, "再次撤销旧序号"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号再次撤销应拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("新登记不应被误撤销，下一到期日 = %q", got.NextDue)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("失败的撤销不应新增履历，履历数 = %d", got)
	}
	// 新登记本身可正常撤销。
	if _, restored, err := s.undoCompletion("EQ-1", 4, "还是不对"); err != nil || restored != "2026-01-01" {
		t.Fatalf("撤销新登记: restored=%q err=%v", restored, err)
	}
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重启后下一到期日 = %q", got.NextDue)
	}
}

// 撤销的拒绝情形：未知资产、无计划、空理由、未知序号、目标不是该资产的完成
// （建立履历、维修履历、其他资产的完成）。失败路径不修改任何业务数据。
func TestUndoRejectedCases(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "已更换"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-06-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-2", "2026-06-01", "2026-06-01", "已清洗"); err != nil {
		t.Fatal(err)
	}
	// 履历：1 建立(EQ-1) 2 完成(EQ-1) 3 报修(EQ-1) 4 建立(EQ-2) 5 完成(EQ-2)。

	if _, _, err := s.undoCompletion("NOPE", 2, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	if _, _, err := s.undoCompletion("EQ-1", 2, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应失败，得到 %v", err)
	}
	if _, _, err := s.undoCompletion("EQ-1", 99, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知序号应失败，得到 %v", err)
	}
	for _, seq := range []int{1, 3, 4, 5} {
		if _, _, err := s.undoCompletion("EQ-1", seq, "理由"); !errors.Is(err, errConflict) {
			t.Fatalf("序号 %d 不是该资产的完成，应失败，得到 %v", seq, err)
		}
	}
	// 无计划资产：先撤销 EQ-2 的完成使计划回到初始，再注册新资产检查。
	if _, _, err := s.undoCompletion("EQ-2", 5, "误登记"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-3", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.undoCompletion("EQ-3", 2, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("无计划资产应失败，得到 %v", err)
	}
	// 失败路径不修改业务数据：EQ-1 下一到期日与履历不变。
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("失败的撤销不应改变下一到期日，得到 %q", got.NextDue)
	}
	if got := len(s.eventsOf("EQ-1")); got != 3 {
		t.Fatalf("失败的撤销不应新增履历，履历数 = %d", got)
	}
	// 有效目标仍可撤销。
	if _, restored, err := s.undoCompletion("EQ-1", 2, "误登记"); err != nil || restored != "2026-01-01" {
		t.Fatalf("有效撤销: restored=%q err=%v", restored, err)
	}
}

// unmaintain 命令：输出目标序号与恢复日期；参数错误退出 2，业务失败退出 1。
func TestUnmaintainCommandAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runOK := func(args []string) {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v 应成功: %d %s", args, code, errOut.String())
		}
	}
	runOK([]string{"register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼"})
	runOK([]string{"plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-11-01", "--interval-days", "90"})
	runOK([]string{"maintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--due", "2026-11-01", "--done", "2026-11-03", "--result", "已更换"})

	// 参数错误 → 2：缺参数、非正序号、非数字序号。
	for _, args := range [][]string{
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "2"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "0", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "-2", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "abc", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("参数错误应退出 2: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 业务失败 → 1：未知资产、未知序号。
	for _, args := range [][]string{
		{"unmaintain", "--data-dir", dir, "--asset-id", "NOPE", "--seq", "2", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "99", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 1 {
			t.Fatalf("业务失败应退出 1: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 成功：输出目标序号与恢复日期。
	out.Reset()
	errOut.Reset()
	code := run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "2",
		"--reason", "误登记，实际未保养"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "已撤销资产 EQ-1 的保养完成履历（序号 2）") ||
		!strings.Contains(out.String(), "恢复下一到期日: 2026-11-01") {
		t.Fatalf("unmaintain 输出不对: %d %s %s", code, out.String(), errOut.String())
	}
	// 重复撤销 → 1。
	out.Reset()
	errOut.Reset()
	code = run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "2", "--reason", "再次"},
		&out, &errOut)
	if code != 1 {
		t.Fatalf("重复撤销应退出 1，得到 %d", code)
	}
	// detail 反映回退。
	out.Reset()
	errOut.Reset()
	runOK([]string{"detail", "--data-dir", dir, "--asset-id", "EQ-1"})
	if !strings.Contains(out.String(), "下一到期日: 2026-11-01") {
		t.Fatalf("detail 应反映回退:\n%s", out.String())
	}
}

// 导入复制完成与撤销履历：撤销引用随履历重编号同步替换，输出完成序号的原、新
// 映射；保留顺序、时间精度、撤销状态和下一到期日；导入后可用新序号继续撤销；
// 源只读，目标原有记录不变。
func TestImportCopiesUndo(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	for _, a := range [][3]string{{"EQ-A", "打印机打印机", "一楼"}, {"EQ-B", "空调", "二楼"}} {
		if _, err := src.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.createPlan("EQ-A", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-A", "2026-01-01", "2026-01-05", "第一次"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-A", "2026-01-11", "2026-01-11", "第二次"); err != nil {
		t.Fatal(err)
	}
	// 撤销第二次完成（源序号 3）：下一到期日回到 2026-01-11。
	if _, restored, err := src.undoCompletion("EQ-A", 3, "误登记"); err != nil || restored != "2026-01-11" {
		t.Fatalf("源撤销: restored=%q err=%v", restored, err)
	}
	mustSave(t, src)

	// 目标已有自己的资产与计划，导入后不变。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.createPlan("EQ-X", "润滑", "2026-05-01", 60); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	var buf bytes.Buffer
	if err := cmdImport([]string{"--data-dir", dstDir, "--source-dir", srcDir, "--asset-id", "EQ-A"}, &buf); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 输出完成序号的原、新映射：源完成序号 2、3 → 目标 3、4（目标已有履历序号 1）。
	out := buf.String()
	if !strings.Contains(out, "保养完成履历序号映射（原序号 -> 新序号）:") ||
		!strings.Contains(out, "2 -> 3") || !strings.Contains(out, "3 -> 4") {
		t.Fatalf("导入应输出完成序号映射:\n%s", out)
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读不变")
	}

	re, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// 下一到期日保持（撤销后的 2026-01-11）；目标已有计划不变。
	if got := re.findPlan("EQ-A"); got == nil || got.NextDue != "2026-01-11" {
		t.Fatalf("导入后下一到期日 = %+v", got)
	}
	if got := re.findPlan("EQ-X"); got == nil || got.NextDue != "2026-05-01" {
		t.Fatalf("目标已有计划不应改变: %+v", got)
	}
	// 撤销引用随重编号替换：目标撤销履历（序号 5）引用新完成序号 4。
	events := re.eventsOf("EQ-A")
	if len(events) != 4 {
		t.Fatalf("导入后履历条数 = %d，想得到 4", len(events))
	}
	var undoEv *Event
	for i := range events {
		if events[i].Kind == eventPlanUndo {
			undoEv = &events[i]
		}
	}
	if undoEv == nil || undoEv.Seq != 5 || undoEv.UndoSeq != 4 || undoEv.Content != "误登记" {
		t.Fatalf("撤销引用未同步替换: %+v", undoEv)
	}
	// 时间精度保持：与源撤销履历时间一致。
	srcRe, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	var srcUndo *Event
	for i, e := range srcRe.eventsOf("EQ-A") {
		if e.Kind == eventPlanUndo {
			srcUndo = &srcRe.eventsOf("EQ-A")[i]
		}
	}
	if srcUndo == nil || !undoEv.Time.Equal(srcUndo.Time) {
		t.Fatalf("撤销履历时间未保持: 源 %v 目标 %v", srcUndo, undoEv.Time)
	}
	// 撤销状态保持：history 中新序号 4 已撤销、3 有效。
	buf.Reset()
	if err := cmdHistory([]string{"--data-dir", dstDir, "--asset-id", "EQ-A"}, &buf); err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	if !strings.Contains(out, "保养完成（履历序号 4，已撤销）") ||
		!strings.Contains(out, "保养完成（履历序号 3，有效）") {
		t.Fatalf("导入后 history 状态不对:\n%s", out)
	}
	// 导入后可用新序号继续撤销：新序号 4 已撤销（拒绝），新序号 3 可撤销。
	if _, _, err := re.undoCompletion("EQ-A", 4, "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("已撤销完成应拒绝，得到 %v", err)
	}
	if _, restored, err := re.undoCompletion("EQ-A", 3, "继续撤销"); err != nil || restored != "2026-01-01" {
		t.Fatalf("导入后继续撤销: restored=%q err=%v", restored, err)
	}
	mustSave(t, re)
	re2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := re2.findPlan("EQ-A"); got.NextDue != "2026-01-01" {
		t.Fatalf("重启后下一到期日 = %q", got.NextDue)
	}
}

// 矛盾撤销数据加载即报错，不自动修复，原文件保留；合法撤销链（含撤销后重新
// 完成）可以加载。
func TestUndoConsistencyRejected(t *testing.T) {
	// 合法台账：建立(4) → 完成(5) → 撤销(6，目标 5)，下一到期日回到首次到期日。
	undoLedger := func(m map[string]any) {
		m["plans"] = []any{map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": "2026-11-01", "interval_days": 90, "next_due": "2026-11-01",
		}}
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
			"due": "2026-11-01", "interval": 90, "time": "2026-10-01T12:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "kind": "保养完成", "content": "已更换",
			"due": "2026-11-01", "done": "2026-11-02", "time": "2026-10-01T13:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 6, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
			"undo_seq": 5, "time": "2026-10-01T14:00:00Z",
		})
	}
	lastEvent := func(m map[string]any) map[string]any {
		ev := m["events"].([]any)
		return ev[len(ev)-1].(map[string]any)
	}
	cases := map[string]func(m map[string]any){
		"撤销引用未知序号": func(m map[string]any) {
			lastEvent(m)["undo_seq"] = 99
		},
		"撤销引用维修履历": func(m map[string]any) {
			lastEvent(m)["undo_seq"] = 3
		},
		"撤销引用建立履历": func(m map[string]any) {
			lastEvent(m)["undo_seq"] = 4
		},
		"重复撤销同一完成": func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "再次",
				"undo_seq": 5, "time": "2026-10-01T15:00:00Z",
			})
		},
		"撤销目标不是最新有效完成": func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养完成", "content": "第二次",
				"due": "2027-01-30", "done": "2027-01-30", "time": "2026-10-01T13:30:00Z",
			})
			lastEvent(m)["seq"] = 8 // 撤销履历移到第二次完成之后，目标仍为 5
		},
		"撤销引用其他资产完成": func(m map[string]any) {
			m["assets"] = append(m["assets"].([]any), map[string]any{
				"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
			})
			m["plans"] = append(m["plans"].([]any), map[string]any{
				"asset_id": "EQ-2", "content": "清洗",
				"first_due": "2026-11-01", "interval_days": 90, "next_due": "2027-01-30",
			})
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-2", "kind": "保养建立", "content": "清洗",
				"due": "2026-11-01", "interval": 90, "time": "2026-10-01T12:30:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 8, "asset_id": "EQ-2", "kind": "保养完成", "content": "已清洗",
				"due": "2026-11-01", "done": "2026-11-02", "time": "2026-10-01T13:30:00Z",
			})
			m["events"].([]any)[3].(map[string]any)["undo_seq"] = 8
		},
		"撤销后推出日期与保存不符": func(m map[string]any) {
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2027-01-30"
		},
		"撤销履历带有工单编号": func(m map[string]any) {
			lastEvent(m)["ticket_id"] = "T0001"
		},
		"撤销履历带有到期日": func(m map[string]any) {
			lastEvent(m)["due"] = "2026-11-01"
		},
		"撤销履历缺少目标序号": func(m map[string]any) {
			delete(lastEvent(m), "undo_seq")
		},
		"撤销引用之后的完成": func(m map[string]any) {
			m["events"].([]any)[2].(map[string]any)["seq"] = 7 // 完成履历移到撤销之后
			lastEvent(m)["undo_seq"] = 7
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, func(m map[string]any) {
				undoLedger(m)
				mutate(m)
			})
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
	// 合法撤销链可以加载：撤销后下一到期日恢复，撤销状态可查。
	dir := t.TempDir()
	writeLedger(t, dir, undoLedger)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法撤销数据应能加载: %v", err)
	}
	if got := s.findPlan("EQ-1"); got == nil || got.NextDue != "2026-11-01" {
		t.Fatalf("计划未加载: %+v", got)
	}
	if !s.undoneMaintSeqs()[5] {
		t.Fatal("完成序号 5 应处于已撤销状态")
	}
	// 撤销后重新完成的链也合法：完成(5) → 撤销(6) → 同周期重新完成(7)。
	dir2 := t.TempDir()
	writeLedger(t, dir2, func(m map[string]any) {
		undoLedger(m)
		appendTo(m, "events", map[string]any{
			"seq": 7, "asset_id": "EQ-1", "kind": "保养完成", "content": "实际完成",
			"due": "2026-11-01", "done": "2026-11-01", "time": "2026-10-01T15:00:00Z",
		})
		m["plans"].([]any)[0].(map[string]any)["next_due"] = "2027-01-30"
	})
	s2, err := openStore(dir2)
	if err != nil {
		t.Fatalf("撤销后重新完成的链应能加载: %v", err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2027-01-30" {
		t.Fatalf("重新完成后下一到期日 = %q", got.NextDue)
	}
}

// 失败重载：撤销的写入失败保留原文件字节、不留下部分变化、不消耗序号，
// 恢复后可重试，重启结果保持。
func TestUndoSaveFailureLeavesFileUntouched(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-02", "已更换"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, s.dir)

	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.undoCompletion("EQ-1", 2, "误登记"); err != nil {
		t.Fatal(err)
	}
	saveErr := s2.save()
	if cerr := os.Chmod(s.dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	// 重载后无部分变化、序号未消耗，恢复后可重试。
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if got := s3.findPlan("EQ-1"); got.NextDue != "2027-01-30" {
		t.Fatalf("写入失败不应回退下一到期日，得到 %q", got.NextDue)
	}
	if got := len(s3.eventsOf("EQ-1")); got != 2 {
		t.Fatalf("写入失败不应留下部分履历，履历数 = %d", got)
	}
	if _, restored, err := s3.undoCompletion("EQ-1", 2, "误登记"); err != nil || restored != "2026-11-01" {
		t.Fatalf("恢复后重试应成功: restored=%q err=%v", restored, err)
	}
	if got := s3.eventsOf("EQ-1"); len(got) != 3 || got[2].Seq != 3 || got[2].Kind != eventPlanUndo {
		t.Fatalf("重试撤销履历不对: %+v", got)
	}
	mustSave(t, s3)
	s4, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s4.findPlan("EQ-1"); got.NextDue != "2026-11-01" {
		t.Fatalf("重启后下一到期日 = %q", got.NextDue)
	}
}
