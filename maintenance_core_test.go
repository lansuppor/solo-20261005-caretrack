package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// 本文件针对收敛后的保养共用核心补充回归测试：以独立确定的日期、完成身份
// （全库履历序号）与方案为预期，核对连续操作与重载一致、跨段限制、撤销后
// 重新完成、停用交错、矛盾历史分类，以及校验不改数据与保存失败后的重试。
// 核心同时服务于日常操作、台账加载、保存前校验与导入后校验。

// replayMaint 重放并返回指定资产的核心状态，便于独立于保存值核对。
func replayMaint(t *testing.T, s *store, assetID string) *maintState {
	t.Helper()
	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		t.Fatalf("replayAllMaintenance: %v", err)
	}
	st := rr.states[assetID]
	if st == nil {
		t.Fatalf("资产 %s 没有重放出保养状态", assetID)
	}
	return st
}

// 连续操作（延期完成、连续完成、撤销、调整、跨段完成）在每步保存重载后，
// 核心重放出的方案段方案与下一到期日都与独立确定的预期一致；履历序号有
// 间隔（穿插其他资产事件）不影响。
func TestCoreContinuousOpsAndReload(t *testing.T) {
	s := newTestStore(t)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 穿插其他资产的报修与另一个计划，制造全库序号间隔。
	if _, _, err := s.report("EQ-2", "不制冷", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-03-01", 30); err != nil {
		t.Fatal(err)
	}

	// 延期到 01-25：跨过 01-11、01-21，下一到期日为首次日网格上严格晚于
	// 01-25 的最早日期 2026-01-31（独立用 nextDueAfter 计算预期）。
	first, _ := parseDate("2026-01-01")
	done1, _ := parseDate("2026-01-25")
	wantNext1, ok := nextDueAfter(first, 10, done1)
	if !ok || formatDate(wantNext1) != "2026-01-31" {
		t.Fatalf("独立预期 2026-01-31，得到 %s/%v", formatDate(wantNext1), ok)
	}
	_, next1, seqDone1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-25", "已更换")
	if err != nil || next1 != "2026-01-31" {
		t.Fatalf("延期完成: next=%q err=%v", next1, err)
	}
	s = saveAndReopen(t, s)
	if st := replayMaint(t, s, "EQ-1"); st.nextDue != "2026-01-31" ||
		st.spec.content != "更换滤芯" || st.spec.interval != 10 ||
		st.latestValidDoneSeq() != seqDone1 {
		t.Fatalf("重载后核心状态不对: %+v", st)
	}

	// 按期完成 01-31 → 02-10。
	_, next2, seqDone2, err := s.completePlan("EQ-1", "2026-01-31", "2026-01-31", "第二次")
	if err != nil || next2 != "2026-02-10" {
		t.Fatalf("按期完成: next=%q err=%v", next2, err)
	}
	// 连续撤销：先撤销最新（seqDone2）恢复 01-31，再撤销 seqDone1 恢复 01-01。
	if _, _, err := s.revokeCompletion("EQ-1", seqDone2, "误登记"); err != nil {
		t.Fatal(err)
	}
	if p, _, err := s.revokeCompletion("EQ-1", seqDone1, "也登错"); err != nil || p.NextDue != "2026-01-01" {
		t.Fatalf("连续撤销: p=%v err=%v", p, err)
	}
	s = saveAndReopen(t, s)
	st := replayMaint(t, s, "EQ-1")
	if st.nextDue != "2026-01-01" || !st.revoked[seqDone1] || !st.revoked[seqDone2] {
		t.Fatalf("重载后撤销状态不对: next=%s revoked=%v", st.nextDue, st.revoked)
	}

	// 调整开启新段：新首次 2026-03-01、间隔 7；没有未撤销完成，无完成日限制。
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-03-01", 7, "型号升级"); err != nil {
		t.Fatal(err)
	}
	s = saveAndReopen(t, s)
	st = replayMaint(t, s, "EQ-1")
	if st.spec.content != "更换高效滤芯" || st.spec.firstDue != "2026-03-01" ||
		st.spec.interval != 7 || st.nextDue != "2026-03-01" || st.segStart <= seqDone2 {
		t.Fatalf("调整后核心状态不对: %+v", st)
	}

	// 跨段：旧段两个完成均不能撤销；当前段延期完成按新网格推进到 03-22。
	for _, old := range []int{seqDone1, seqDone2} {
		if _, _, err := s.revokeCompletion("EQ-1", old, "旧段"); !errors.Is(err, errConflict) {
			t.Fatalf("旧段完成 %d 应不能撤销，得到 %v", old, err)
		}
	}
	newFirst, _ := parseDate("2026-03-01")
	newDone, _ := parseDate("2026-03-20")
	wantNext2, ok := nextDueAfter(newFirst, 7, newDone)
	if !ok || formatDate(wantNext2) != "2026-03-22" {
		t.Fatalf("独立预期 2026-03-22，得到 %s/%v", formatDate(wantNext2), ok)
	}
	_, next3, seqDone3, err := s.completePlan("EQ-1", "2026-03-01", "2026-03-20", "新段完成")
	if err != nil || next3 != "2026-03-22" {
		t.Fatalf("新段延期完成: next=%q err=%v", next3, err)
	}
	s = saveAndReopen(t, s)
	if st := replayMaint(t, s, "EQ-1"); st.nextDue != "2026-03-22" ||
		st.latestValidDoneSeq() != seqDone3 || len(st.allDone) != 3 {
		t.Fatalf("新段完成后核心状态不对: %+v", st)
	}
}

// 撤销后同周期重新完成产生新序号；旧序号不影响新记录，旧序号不能撤销新完成。
func TestCoreRevokeThenRecompleteIdentity(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-05", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "误登记"); err != nil {
		t.Fatal(err)
	}
	_, next, seq2, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "重新登记")
	if err != nil || next != "2026-01-11" {
		t.Fatalf("重新完成: next=%q err=%v", next, err)
	}
	if seq2 <= seq1 {
		t.Fatalf("重新完成应使用更大的新序号: %d -> %d", seq1, seq2)
	}
	// 旧序号已撤销，再次撤销被拒，且绝不会误指向新完成。
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "旧序号再来"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号撤销应拒绝，得到 %v", err)
	}
	st := replayMaint(t, s, "EQ-1")
	if st.nextDue != "2026-01-11" || !st.revoked[seq1] || st.revoked[seq2] {
		t.Fatalf("旧序号不应影响新完成: %+v", st)
	}
	if st.latestValidDoneSeq() != seq2 {
		t.Fatalf("当前最新有效完成应为新序号 %d", seq2)
	}
	// 新序号可正常撤销并恢复周期到期日。
	if p, _, err := s.revokeCompletion("EQ-1", seq2, "又错了"); err != nil || p.NextDue != "2026-01-01" {
		t.Fatalf("新序号撤销: p=%v err=%v", p, err)
	}
}

// 停用交错：停用期间允许建立、调整与撤销，拒绝完成；核心重放以“当时停用
// 状态”判定，不能用最终可用状态代替历史。
func TestCoreDeactivationInterleaving(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "已更换")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	// 停用期间完成拒绝，且不推进、不新增履历。
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-11", "停用中完成"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间完成应拒绝，得到 %v", err)
	}
	// 停用期间允许撤销（与调整）：先撤销当前段完成，恢复周期到期日。
	if p, _, err := s.revokeCompletion("EQ-1", seq1, "停用中撤销"); err != nil || p.NextDue != "2026-01-01" {
		t.Fatalf("停用期间撤销: p=%v err=%v", p, err)
	}
	// 停用期间允许调整，调整开启新方案段。
	if _, _, err := s.adjustPlan("EQ-1", "停用调整", "2026-02-01", 7, "停用中调整"); err != nil {
		t.Fatalf("停用期间调整应成功: %v", err)
	}
	// 调整后旧段完成不能撤销（即使已恢复使用也不行）。
	if _, err := s.reactivateAsset("EQ-1", "恢复使用"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "恢复后试旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("旧段完成恢复后仍不能撤销，得到 %v", err)
	}
	// 恢复使用后按当前段方案继续完成。
	if _, next, _, err := s.completePlan("EQ-1", "2026-02-01", "2026-02-01", "恢复后完成"); err != nil || next != "2026-02-08" {
		t.Fatalf("恢复后完成: next=%q err=%v", next, err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(s.dir); err != nil {
		t.Fatalf("停用交错台账重载应一致: %v", err)
	}

	// 历史中停用期间的完成即使最终状态为可用，也必须被核心拒绝。
	bad := newTestStore(t)
	if _, err := bad.registerAsset("EQ-9", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.createPlan("EQ-9", "润滑", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.deactivateAsset("EQ-9", "停用"); err != nil {
		t.Fatal(err)
	}
	// 直接追加一条“停用期间完成”并把下一到期日改成推进后的值，绕过日常判定。
	badSeq, err := bad.nextEventSeq()
	if err != nil {
		t.Fatal(err)
	}
	bad.appendMaintEvent(badSeq, "EQ-9", eventPlanDone, "停用中完成", "2026-01-01", "2026-01-01", 0)
	bad.findPlan("EQ-9").NextDue = "2026-01-11"
	if err := bad.save(); err == nil || !strings.Contains(err.Error(), "停用期间") {
		t.Fatalf("保存前校验应指出停用期间完成，得到 %v", err)
	}
}

// 核心逐步重放的矛盾分类：每类历史矛盾都返回指明类别的错误。
func TestCoreApplyContradictionCategories(t *testing.T) {
	create := func(seq int) Event {
		return Event{Seq: seq, AssetID: "EQ-1", Kind: eventPlanCreate, Content: "C",
			Due: "2026-01-01", Interval: 10}
	}
	done := func(seq int, due, d string) Event {
		return Event{Seq: seq, AssetID: "EQ-1", Kind: eventPlanDone, Content: "r",
			Due: due, Done: d}
	}
	cases := []struct {
		name    string
		events  []Event
		wantSub string
	}{
		{"重复建立", []Event{create(1), create(2)}, "多条保养建立履历"},
		{"调整先于建立", []Event{{Seq: 1, AssetID: "EQ-1", Kind: eventPlanAdjust, Content: "r",
			Due: "2026-02-01", Interval: 5, NewContent: "N", OldContent: "C", OldDue: "2026-01-01",
			OldInterval: 10, OldNextDue: "2026-01-01"}}, "建立履历之前"},
		{"撤销先于建立", []Event{{Seq: 1, AssetID: "EQ-1", Kind: eventPlanRevoke,
			Content: "r", TargetSeq: 5}}, "建立履历之前"},
		{"完成先于建立", []Event{done(1, "2026-01-01", "2026-01-01")}, "建立履历之前"},
		{"调整原方案不符", []Event{create(1), {Seq: 2, AssetID: "EQ-1", Kind: eventPlanAdjust,
			Content: "r", Due: "2026-03-01", Interval: 5, NewContent: "N", OldContent: "X",
			OldDue: "2026-01-01", OldInterval: 10, OldNextDue: "2026-01-01"}}, "原方案"},
		{"调整原下一到期日不符", []Event{create(1), {Seq: 2, AssetID: "EQ-1", Kind: eventPlanAdjust,
			Content: "r", Due: "2026-03-01", Interval: 5, NewContent: "N", OldContent: "C",
			OldDue: "2026-01-01", OldInterval: 10, OldNextDue: "2026-02-01"}}, "原下一到期日"},
		{"调整新首次日不晚于完成日", []Event{
			create(1), done(2, "2026-01-01", "2026-01-20"),
			{Seq: 3, AssetID: "EQ-1", Kind: eventPlanAdjust, Content: "r", Due: "2026-01-20",
				Interval: 5, NewContent: "N", OldContent: "C", OldDue: "2026-01-01",
				OldInterval: 10, OldNextDue: "2026-01-21"},
		}, "未严格晚于"},
		{"完成到期日不接续", []Event{create(1), done(2, "2026-01-11", "2026-01-11")}, "不接续"},
		{"完成日早于到期日", []Event{create(1), done(2, "2026-01-01", "2025-12-31")}, "早于周期到期日"},
		{"撤销未知目标", []Event{create(1), {Seq: 3, AssetID: "EQ-1", Kind: eventPlanRevoke,
			Content: "r", TargetSeq: 99}}, "不是资产"},
		{"撤销非最新", []Event{
			create(1), done(2, "2026-01-01", "2026-01-01"), done(3, "2026-01-11", "2026-01-11"),
			{Seq: 4, AssetID: "EQ-1", Kind: eventPlanRevoke, Content: "r", TargetSeq: 2},
		}, "不是当时最新"},
		{"重复撤销", []Event{
			create(1), done(2, "2026-01-01", "2026-01-01"),
			{Seq: 3, AssetID: "EQ-1", Kind: eventPlanRevoke, Content: "r1", TargetSeq: 2},
			{Seq: 4, AssetID: "EQ-1", Kind: eventPlanRevoke, Content: "r2", TargetSeq: 2},
		}, "重复撤销"},
		{"撤销旧段完成", []Event{
			create(1), done(2, "2026-01-01", "2026-01-01"),
			{Seq: 3, AssetID: "EQ-1", Kind: eventPlanAdjust, Content: "r", Due: "2026-02-01",
				Interval: 5, NewContent: "N", OldContent: "C", OldDue: "2026-01-01",
				OldInterval: 10, OldNextDue: "2026-01-11"},
			{Seq: 4, AssetID: "EQ-1", Kind: eventPlanRevoke, Content: "r", TargetSeq: 2},
		}, "旧方案段"},
		{"完成推出日期越界", []Event{
			{Seq: 1, AssetID: "EQ-1", Kind: eventPlanCreate, Content: "C", Due: "9999-01-01", Interval: 364},
			done(2, "9999-01-01", "9999-12-31"),
		}, "日期范围"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMaintState()
			var err error
			for _, e := range tc.events {
				err = st.apply(e)
				if err != nil {
					break
				}
			}
			if err == nil {
				t.Fatalf("应报矛盾（%s）", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误类别应含 %q，得到 %v", tc.wantSub, err)
			}
		})
	}
}

// 纯判定函数绝不修改传入业务记录：成功与失败路径后，计划与履历序列化字节
// 完全一致，也不借校验补字段。
func TestCoreChecksDoNotMutate(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, _, seq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "已更换"); err != nil {
		t.Fatal(err)
	} else {
		_ = seq
	}
	snapshot := func() []byte {
		b, err := json.Marshal(struct {
			Plans  []*Plan
			Events []Event
		}{s.data.Plans, s.data.Events})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	assertUnchanged := func(before []byte, where string) {
		t.Helper()
		if after := snapshot(); !bytes.Equal(after, before) {
			t.Fatalf("%s 修改了业务记录:\nbefore=%s\n after=%s", where, before, after)
		}
	}

	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		t.Fatal(err)
	}
	// 成功与失败的判定混合调用，均不得改动数据。
	before := snapshot()
	if err := checkCreatePlan(s.data, "NOPE", "x", "2026-01-01", 1); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产建立应失败，得到 %v", err)
	}
	if err := checkCreatePlan(s.data, "EQ-1", "x", "2026-01-01", 1); !errors.Is(err, errConflict) {
		t.Fatalf("重复建立应失败，得到 %v", err)
	}
	if _, err := checkAdjustPlan(s.data, rr, "EQ-1", "新", "2026-01-03", 5, "r"); err != nil {
		t.Fatalf("合法调整判定应通过，得到 %v", err)
	}
	if _, err := checkAdjustPlan(s.data, rr, "EQ-1", "新", "2026-01-03", 0, "r"); !errors.Is(err, errConflict) {
		t.Fatalf("零间隔应失败，得到 %v", err)
	}
	if _, err := checkCompletePlan(s.data, rr, "EQ-1", "2026-01-11", "2026-01-11", "r"); err != nil {
		t.Fatalf("合法完成判定应通过，得到 %v", err)
	}
	if _, err := checkCompletePlan(s.data, rr, "EQ-1", "2026-01-01", "2026-01-01", "r"); !errors.Is(err, errConflict) {
		t.Fatalf("旧周期完成应失败，得到 %v", err)
	}
	latest := rr.states["EQ-1"].latestValidDoneSeq()
	if _, err := checkRevokeCompletion(s.data, rr, "EQ-1", latest, "r"); err != nil {
		t.Fatalf("合法撤销判定应通过，得到 %v", err)
	}
	if _, err := checkRevokeCompletion(s.data, rr, "EQ-1", latest, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应失败，得到 %v", err)
	}
	// 重放本身也不得改变数组顺序或内容。
	if _, err := replayAllMaintenance(s.data); err != nil {
		t.Fatal(err)
	}
	assertUnchanged(before, "纯判定")
}

// 重放顺序只取决于全库履历序号：履历数组乱序、序号有间隔得到同样的状态，
// 且乱序数据可原样保存重载；之后的日常操作接续正确。
func TestCoreReplayIndependentOfArrayOrder(t *testing.T) {
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
	if _, _, err := s.report("EQ-2", "故障", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, next, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-25", "已更换"); err != nil || next != "2026-01-31" {
		t.Fatalf("完成: next=%q err=%v", next, err)
	}
	want := replayMaint(t, s, "EQ-1")

	// 反转履历数组（乱序），重放结果必须一致。
	evs := s.data.Events
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	rr, err := replayAllMaintenance(s.data)
	if err != nil {
		t.Fatalf("乱序重放不应失败: %v", err)
	}
	got := rr.states["EQ-1"]
	if got.nextDue != want.nextDue || got.spec.content != want.spec.content ||
		got.spec.firstDue != want.spec.firstDue || got.spec.interval != want.spec.interval ||
		got.latestValidDoneSeq() != want.latestValidDoneSeq() {
		t.Fatalf("乱序重放结果不一致: got=%+v want=%+v", got, want)
	}
	// 乱序数据可保存重载，且后续完成仍按接续规则工作。
	if err := s.save(); err != nil {
		t.Fatalf("乱序数据应能保存: %v", err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatalf("乱序数据应能加载: %v", err)
	}
	if _, next, _, err := s2.completePlan("EQ-1", "2026-01-31", "2026-02-01", "续做"); err != nil || next != "2026-02-10" {
		t.Fatalf("乱序重载后续完成: next=%q err=%v", next, err)
	}
}

// 完成登记为一次原子保存：写入失败保留原文件字节，不留下部分推进、不消耗
// 序号，恢复写权限后可按原输入重试成功。
func TestCoreCompleteSaveFailureAndRetry(t *testing.T) {
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
	eventsBefore := len(s.data.Events)

	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "已更换"); err != nil {
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
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败后原文件字节应不变")
	}
	// 重新打开的台账看不到部分推进。
	fresh, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := fresh.findPlan("EQ-1"); p.NextDue != "2026-01-01" {
		t.Fatalf("写入失败不应留下部分推进，得到 %q", p.NextDue)
	}
	// 按原输入重试成功，且履历序号未被失败消耗（建立为 1，完成为 2）。
	_, next, seq, err := fresh.completePlan("EQ-1", "2026-01-01", "2026-01-02", "已更换")
	if err != nil || next != "2026-01-11" {
		t.Fatalf("恢复后重试: next=%q err=%v", next, err)
	}
	if seq != eventsBefore+1 {
		t.Fatalf("失败不应消耗序号，完成序号 = %d，想得到 %d", seq, eventsBefore+1)
	}
	if err := fresh.save(); err != nil {
		t.Fatal(err)
	}
	if got := readFileBytes(t, s.dir); bytes.Equal(got, before) {
		t.Fatal("重试保存后文件应更新")
	}
}

// 导入接续由同一核心保证：导入乱序源台账并重编号后，可继续调整、完成与
// 撤销，合并数据通过保存前的核心校验；源只读。
func TestCoreImportShuffledContinuation(t *testing.T) {
	src := newTestStore(t)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	oldSeqD := 0
	if _, _, seq, err := src.completePlan("EQ-1", "2026-01-01", "2026-01-05", "旧段"); err != nil {
		t.Fatal(err)
	} else {
		oldSeqD = seq
	}
	if _, _, err := src.adjustPlan("EQ-1", "高效滤芯", "2026-02-01", 60, "升级"); err != nil {
		t.Fatal(err)
	}
	if _, _, curSeq, err := src.completePlan("EQ-1", "2026-02-01", "2026-02-03", "当前段"); err != nil {
		t.Fatal(err)
	} else {
		_ = curSeq
	}
	// 源履历数组乱序后保存（合法，顺序只看序号）。
	evs := src.data.Events
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	if err := src.save(); err != nil {
		t.Fatal(err)
	}
	srcBefore := readFileBytes(t, src.dir)

	dst := newTestStore(t)
	if _, err := dst.registerAsset("EQ-9", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-9", "异响", "req-9"); err != nil {
		t.Fatal(err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}

	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("导入乱序源应成功: %v", err)
	}
	if got := readFileBytes(t, src.dir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应只读不变")
	}
	re, err := openStore(dst.dir)
	if err != nil {
		t.Fatalf("导入合并数据应通过核心校验: %v", err)
	}
	p := re.findPlan("EQ-1")
	if p == nil || p.Content != "高效滤芯" || p.NextDue != "2026-04-02" {
		t.Fatalf("导入后方案不对: %+v", p)
	}
	// 旧段完成重编号后仍不能撤销；当前段完成可撤销并恢复。
	var newOld, newCur int
	for _, m := range outcome.completions {
		switch m.OldSeq {
		case oldSeqD:
			newOld = m.NewSeq
		default:
			newCur = m.NewSeq
		}
	}
	if _, _, err := re.revokeCompletion("EQ-1", newOld, "旧段"); !errors.Is(err, errConflict) {
		t.Fatalf("导入后旧段完成应不能撤销，得到 %v", err)
	}
	if _, _, err := re.revokeCompletion("EQ-1", newCur, "撤销当前段"); err != nil {
		t.Fatalf("导入后当前段完成应能撤销: %v", err)
	}
	if got := re.findPlan("EQ-1"); got.NextDue != "2026-02-01" {
		t.Fatalf("撤销后下一到期日 = %q", got.NextDue)
	}
	if _, next, _, err := re.completePlan("EQ-1", "2026-02-01", "2026-02-10", "导入后重做"); err != nil || next != "2026-04-02" {
		t.Fatalf("导入后重新完成: next=%q err=%v", next, err)
	}
	if _, _, err := re.adjustPlan("EQ-1", "再升级", "2026-05-01", 45, "导入后调整"); err != nil {
		t.Fatalf("导入后调整: %v", err)
	}
	if err := re.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dst.dir); err != nil {
		t.Fatalf("导入后连续操作应通过重载校验: %v", err)
	}
}
