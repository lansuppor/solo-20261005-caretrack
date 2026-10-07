package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 连续操作、跨段限制、撤销后重新完成与停用交错：整个序列的每一一步结果都以
// 独立确定的日期、完成身份（履历序号）和方案为预期；中途与结尾重载后，
// 保存的方案与由履历链推出的状态一致，操作可继续。
func TestMaintCoreSequenceAndReload(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// 建立：下一到期日即首次到期日。
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 第一方案段：两次完成，按首次日加整数倍间隔推进。
	if _, next, seq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-05", "第一次"); err != nil || next != "2026-01-11" || seq != 2 {
		t.Fatalf("第一次完成: next=%q seq=%d err=%v", next, seq, err)
	}
	if _, next, seq, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-11", "第二次"); err != nil || next != "2026-01-21" || seq != 3 {
		t.Fatalf("第二次完成: next=%q seq=%d err=%v", next, seq, err)
	}
	// 调整开启新方案段：下一到期日设为新首次日。
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 7, "型号升级"); err != nil {
		t.Fatal(err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-02-01" || got.IntervalDays != 7 {
		t.Fatalf("调整后计划不对: %+v", got)
	}
	// 跨段限制：旧段完成（序号 2、3）不能再撤销。
	for _, old := range []int{2, 3} {
		if _, _, err := s.revokeCompletion("EQ-1", old, "试旧段"); !errors.Is(err, errConflict) {
			t.Fatalf("旧段完成序号 %d 应不能撤销，得到 %v", old, err)
		}
	}
	// 当前段延期完成：02-01 加整数倍 7 天，严格晚于 02-20 的最早日期为 02-22。
	if _, next, seq, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-20", "延期完成"); err != nil || next != "2026-02-22" || seq != 5 {
		t.Fatalf("延期完成: next=%q seq=%d err=%v", next, seq, err)
	}
	// 撤销该完成：下一到期日恢复为其周期到期日。
	if p, _, err := s.revokeCompletion("EQ-1", 5, "误登记"); err != nil || p.NextDue != "2026-02-01" {
		t.Fatalf("撤销: next=%q err=%v", p.NextDue, err)
	}
	// 同周期重新完成：生成新序号，旧序号再次撤销仍被拒绝。
	if _, next, seq, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-03", "重新完成"); err != nil || next != "2026-02-08" || seq != 7 {
		t.Fatalf("重新完成: next=%q seq=%d err=%v", next, seq, err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", 5, "旧序号"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号再次撤销应拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-02-08" {
		t.Fatalf("旧序号撤销不应影响新完成，下一到期日 = %q", got.NextDue)
	}
	// 停用交错：停用期间拒绝完成，允许调整；停用不重算周期。
	if _, err := s.deactivateAsset("EQ-1", "设备调拨"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-02-08", "2026-02-08", "停用中"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间完成应拒绝，得到 %v", err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "停用中调整", "2026-03-01", 15, "调拨后新方案"); err != nil {
		t.Fatalf("停用期间调整应成功: %v", err)
	}
	// 调整后序号 7 的完成也属于旧段，不能再撤销。
	if _, _, err := s.revokeCompletion("EQ-1", 7, "试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成序号 7 应不能撤销，得到 %v", err)
	}
	if _, err := s.reactivateAsset("EQ-1", "调拨完成"); err != nil {
		t.Fatal(err)
	}
	// 恢复后按保存的下一到期日继续完成。
	if _, next, seq, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-01", "恢复后完成"); err != nil || next != "2026-03-16" || seq != 11 {
		t.Fatalf("恢复后完成: next=%q seq=%d err=%v", next, seq, err)
	}
	// 失败的完成、撤销与停用期间的拒绝都不产生履历：恰 11 条资产履历。
	if got := len(s.eventsOf("EQ-1")); got != 11 {
		t.Fatalf("履历数 = %d，想得到 11", got)
	}

	// 重载后：保存的方案与履历链推出的状态一致，操作可继续。
	s = saveAndReopen(t, s)
	p := s.findPlan("EQ-1")
	if p.Content != "停用中调整" || p.FirstDue != "2026-03-01" || p.IntervalDays != 15 || p.NextDue != "2026-03-16" {
		t.Fatalf("重载后计划不对: %+v", p)
	}
	// 撤销最新完成并再次重新完成，结果与首次推进一致。
	if p, _, err := s.revokeCompletion("EQ-1", 11, "误登记"); err != nil || p.NextDue != "2026-03-01" {
		t.Fatalf("重载后撤销: next=%q err=%v", p.NextDue, err)
	}
	s = saveAndReopen(t, s)
	if _, next, seq, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-05", "再次完成"); err != nil || next != "2026-03-16" || seq != 13 {
		t.Fatalf("重载后重新完成: next=%q seq=%d err=%v", next, seq, err)
	}
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-03-16" {
		t.Fatalf("最终下一到期日 = %q，想得到 2026-03-16", got.NextDue)
	}
	// 撤销状态保持：序号 5、11 已撤销，其余完成有效。
	revoked := s.revokedDoneSeqs()
	for seq, want := range map[int]bool{2: false, 3: false, 5: true, 7: false, 11: true, 13: false} {
		if revoked[seq] != want {
			t.Fatalf("完成序号 %d 的撤销状态 = %v，想得到 %v", seq, revoked[seq], want)
		}
	}
	// 保养不改变资产状态、不创建工单。
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("资产状态 = %q，想得到 %q", got, statusAvailable)
	}
	if len(s.data.Tickets) != 0 {
		t.Fatalf("保养不应创建工单，工单数 = %d", len(s.data.Tickets))
	}
}

// 导入接续：源台账含调整、撤销与重新完成的历史，导入后方案段、撤销引用与
// 下一到期日保持，可用新序号继续撤销、完成与调整；源只读，目标原有记录不变。
func TestMaintCoreImportContinuation(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := src.completePlan("EQ-1", "2026-01-01", "2026-01-10", "旧段完成"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 60, "方案升级"); err != nil {
		t.Fatal(err)
	}
	_, _, curSeq, err := src.completePlan("EQ-1", "2026-02-01", "2026-02-03", "当前段完成")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.revokeCompletion("EQ-1", curSeq, "误登记"); err != nil {
		t.Fatal(err)
	}
	// 撤销后同周期重新完成：源台账带有效与已撤销两种完成状态。
	_, _, redoSeq, err := src.completePlan("EQ-1", "2026-02-01", "2026-02-05", "重新完成")
	if err != nil {
		t.Fatal(err)
	}
	if redoSeq == curSeq {
		t.Fatal("重新完成应生成新序号")
	}
	mustSave(t, src)
	srcBefore := readFileBytes(t, srcDir)

	// 目标已有自己的资产与计划。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-9", "空调", "三楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.createPlan("EQ-9", "清洗滤网", "2026-03-01", 15); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("importAssets: %v", err)
	}
	if outcome.plans != 1 || len(outcome.completions) != 3 {
		t.Fatalf("导入结果不对: %+v", outcome)
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读不变")
	}

	d, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// 目标原有记录保持。
	if p := d.findPlan("EQ-9"); p == nil || p.Content != "清洗滤网" || p.NextDue != "2026-03-01" {
		t.Fatalf("目标原有计划不应改变: %+v", p)
	}
	// 导入的计划与下一到期日原样保持（撤销后重新完成推进到 2026-04-02）。
	p := d.findPlan("EQ-1")
	if p == nil || p.Content != "更换高效滤芯" || p.FirstDue != "2026-02-01" ||
		p.IntervalDays != 60 || p.NextDue != "2026-04-02" {
		t.Fatalf("导入后计划不对: %+v", p)
	}
	// 撤销引用随履历重编号替换：被撤销的完成映射到新序号。
	remap := map[int]int{}
	for _, m := range outcome.completions {
		remap[m.OldSeq] = m.NewSeq
	}
	var revokes int
	for _, e := range d.eventsOf("EQ-1") {
		if e.Kind == eventPlanRevoke {
			revokes++
			if e.TargetSeq != remap[curSeq] {
				t.Fatalf("撤销引用应替换为新序号 %d，得到 %d", remap[curSeq], e.TargetSeq)
			}
		}
	}
	if revokes != 1 {
		t.Fatalf("应导入 1 条撤销履历，得到 %d", revokes)
	}
	// 撤销状态保持：被撤销的完成在新库仍为已撤销。
	if got := d.revokedDoneSeqs(); !got[remap[curSeq]] || got[remap[redoSeq]] {
		t.Fatalf("导入后撤销状态不对: %v", got)
	}
	// 跨段限制保持：旧段完成（源序号 2）导入后仍不能撤销。
	if _, _, err := d.revokeCompletion("EQ-1", remap[2], "试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("导入后旧段完成应不能撤销，得到 %v", err)
	}
	// 导入后可继续撤销当前段最新有效完成（重新完成的那条）。
	if p, _, err := d.revokeCompletion("EQ-1", remap[redoSeq], "又登错了"); err != nil || p.NextDue != "2026-02-01" {
		t.Fatalf("导入后撤销: next=%q err=%v", p.NextDue, err)
	}
	// 继续完成与调整：按当前段方案推进。
	if _, next, _, err := d.completePlan("EQ-1", "2026-02-01", "2026-02-06", "导入后完成"); err != nil || next != "2026-04-02" {
		t.Fatalf("导入后完成: next=%q err=%v", next, err)
	}
	if _, _, err := d.adjustPlan("EQ-1", "再次升级", "2026-05-01", 45, "导入后调整"); err != nil {
		t.Fatalf("导入后调整: %v", err)
	}
	mustSave(t, d)
	// 重载后一致。
	d2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.findPlan("EQ-1"); got.Content != "再次升级" || got.FirstDue != "2026-05-01" ||
		got.IntervalDays != 45 || got.NextDue != "2026-05-01" {
		t.Fatalf("重载后计划不对: %+v", got)
	}
}

// 矛盾历史：重复建立、完成在建立之前、完成日早于到期日、推算下一到期日越界，
// 加载即拒绝并指出问题类别，原文件保留、不自动修复。
func TestMaintCoreContradictoryHistory(t *testing.T) {
	plan := func(first string, interval int, next string) map[string]any {
		return map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": first, "interval_days": interval, "next_due": next,
		}
	}
	create := func(seq int, first string, interval int) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
			"due": first, "interval": interval, "time": "2026-10-01T12:00:00Z",
		}
	}
	done := func(seq int, due, doneDay string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "kind": "保养完成", "content": "已更换",
			"due": due, "done": doneDay, "time": "2026-10-01T13:00:00Z",
		}
	}
	cases := map[string]func(m map[string]any){
		"重复建立履历": func(m map[string]any) {
			m["plans"] = []any{plan("2026-11-01", 90, "2026-11-01")}
			appendTo(m, "events", create(4, "2026-11-01", 90))
			appendTo(m, "events", create(5, "2026-11-01", 90))
		},
		"完成履历在建立之前": func(m map[string]any) {
			m["plans"] = []any{plan("2026-01-01", 10, "2026-01-11")}
			appendTo(m, "events", done(4, "2026-01-01", "2026-01-01"))
			appendTo(m, "events", create(5, "2026-01-01", 10))
		},
		"完成日早于周期到期日": func(m map[string]any) {
			m["plans"] = []any{plan("2026-01-01", 10, "2026-01-11")}
			appendTo(m, "events", create(4, "2026-01-01", 10))
			appendTo(m, "events", done(5, "2026-01-01", "2025-12-31"))
		},
		"推算下一到期日越界": func(m map[string]any) {
			m["plans"] = []any{plan("9999-01-01", 364, "9999-01-01")}
			appendTo(m, "events", create(4, "9999-01-01", 364))
			appendTo(m, "events", done(5, "9999-01-01", "9999-12-31"))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			} else if !strings.Contains(err.Error(), "矛盾") {
				t.Fatalf("错误应指出问题类别（矛盾），得到 %v", err)
			}
			if got := readFileBytes(t, dir); !bytes.Equal(got, raw) {
				t.Fatal("矛盾台账的原文件应保持不变")
			}
		})
	}
}

// 保存失败后的重试：完成与调整在写入失败时不留下部分持久化变化、不消耗序号，
// 原文件字节不变，恢复后可按原输入重试成功。
func TestMaintCoreSaveFailureAndRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, s.dir)

	failSave := func() error {
		if err := os.Chmod(s.dir, 0o555); err != nil {
			t.Fatal(err)
		}
		err := s.save()
		if err2 := os.Chmod(s.dir, 0o755); err2 != nil {
			t.Fatal(err2)
		}
		return err
	}

	// 完成登记后保存失败。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "已更换"); err != nil {
		t.Fatal(err)
	}
	if err := failSave(); err == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：下一到期日未推进，完成未登记。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.now = s.now
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("写入失败不应留下部分变化，下一到期日 = %q", got.NextDue)
	}
	// 恢复后按原输入重试成功，履历序号未被失败消耗（仍为 2）。
	if _, next, seq, err := s2.completePlan("EQ-1", "2026-01-01", "2026-01-01", "已更换"); err != nil || next != "2026-01-11" || seq != 2 {
		t.Fatalf("重试完成: next=%q seq=%d err=%v", next, seq, err)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	before = readFileBytes(t, s.dir)

	// 调整后保存失败同样不留部分变化。
	if _, _, err := s2.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 7, "型号升级"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	adjErr := s2.save()
	if err := os.Chmod(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if adjErr == nil {
		t.Fatal("只读目录下保存应失败")
	}
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s3.now = s.now
	if got := s3.findPlan("EQ-1"); got.Content != "更换滤芯" || got.NextDue != "2026-01-11" {
		t.Fatalf("失败的调整不应持久化: %+v", got)
	}
	// 恢复后按原输入重试成功，调整履历序号为 3（未被消耗）。
	if _, _, err := s3.adjustPlan("EQ-1", "更换高效滤芯", "2026-02-01", 7, "型号升级"); err != nil {
		t.Fatalf("重试调整: %v", err)
	}
	events := s3.eventsOf("EQ-1")
	last := events[len(events)-1]
	if last.Kind != eventPlanAdjust || last.Seq != 3 {
		t.Fatalf("失败不应消耗履历序号，调整履历 = %+v", last)
	}
	if err := s3.save(); err != nil {
		t.Fatal(err)
	}
	s4, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s4.findPlan("EQ-1"); got.Content != "更换高效滤芯" || got.NextDue != "2026-02-01" {
		t.Fatalf("重载后计划不对: %+v", got)
	}
}

// 共用核心只读业务记录：台账校验与操作前的核心重放都不修改传入的履历、
// 计划或资产，不借校验补字段或修复数据。
func TestMaintCoreDoesNotMutateRecords(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-10", "第一次"); err != nil {
		t.Fatal(err)
	}
	_, _, seq, err := s.completePlan("EQ-1", "2026-01-31", "2026-02-01", "第二次")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq, "误登记"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-03-01", 60, "型号升级"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-02", "新段完成"); err != nil {
		t.Fatal(err)
	}
	s = saveAndReopen(t, s)

	// 快照全部业务记录。
	eventsBefore := append([]Event(nil), s.data.Events...)
	plansBefore := make([]Plan, len(s.data.Plans))
	for i, p := range s.data.Plans {
		plansBefore[i] = *p
	}
	assetsBefore := make([]Asset, len(s.data.Assets))
	for i, a := range s.data.Assets {
		assetsBefore[i] = *a
	}

	// 台账校验（加载、保存前与导入后共用的同一入口）。
	if err := validateData(s.data); err != nil {
		t.Fatalf("合法数据应通过校验: %v", err)
	}
	// 操作前的核心重放与派生查询。
	core, err := s.maintCore()
	if err != nil {
		t.Fatal(err)
	}
	content, firstDue, interval, nextDue, created := core.derived("EQ-1")
	if !created || content != "更换高效滤芯" || firstDue != "2026-03-01" || interval != 60 || nextDue != "2026-04-30" {
		t.Fatalf("派生状态不对: %q %q %d %q created=%v", content, firstDue, interval, nextDue, created)
	}

	// 业务记录原样未动。
	if !reflect.DeepEqual(s.data.Events, eventsBefore) {
		t.Fatal("校验与重放不应修改履历记录")
	}
	if len(s.data.Plans) != len(plansBefore) {
		t.Fatal("校验与重放不应修改计划记录")
	}
	for i, p := range s.data.Plans {
		if !reflect.DeepEqual(*p, plansBefore[i]) {
			t.Fatalf("计划记录被修改: %+v != %+v", *p, plansBefore[i])
		}
	}
	for i, a := range s.data.Assets {
		if !reflect.DeepEqual(*a, assetsBefore[i]) {
			t.Fatalf("资产记录被修改: %+v != %+v", *a, assetsBefore[i])
		}
	}
}
