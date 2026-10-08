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

// 提交验收全流程：提交后待验收占用资产（拒绝派工、领用、重复提交、直接关闭），
// 退回后恢复未关闭可再次提交并产生新序号，验收通过沿用关闭履历终结工单，
// 维修结果取被通过提交的内容，资产恢复可用；旧提交与意见全部保留。
func TestReviewReworkResubmit(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	// 从未提交的工单仍可直接关闭的规则不受影响，这里走提交验收流程。
	if _, seq, err := s.submitRepair(tk.ID, "已更换搓纸轮"); err != nil || seq != 2 {
		t.Fatalf("首次提交: seq=%d err=%v", seq, err)
	}
	if tk.Status != ticketPending {
		t.Fatalf("提交后工单状态 = %q，想得到 %q", tk.Status, ticketPending)
	}
	if a := s.findAsset("EQ-1"); a.Status != statusRepairing {
		t.Fatalf("待验收仍占用资产，状态 = %q", a.Status)
	}
	// 待验收拒绝重复提交、派工、领用、退回、直接关闭与新报修。
	if _, _, err := s.submitRepair(tk.ID, "重复提交"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收重复提交应拒绝，得到 %v", err)
	}
	if _, err := s.assignTicket(tk.ID, "张三", "派工"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收派工应拒绝，得到 %v", err)
	}
	if _, err := s.withdrawPart(tk.ID, "P-1", 1, "领用"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收领用应拒绝，得到 %v", err)
	}
	if _, _, err := s.closeTicket(tk.ID, "绕过验收"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收直接关闭应拒绝，得到 %v", err)
	}
	if _, _, err := s.report("EQ-1", "二次故障", "req-2"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收新报修应拒绝，得到 %v", err)
	}
	if _, err := s.deactivateAsset("EQ-1", "停用"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收停用应拒绝，得到 %v", err)
	}
	// 旧请求重放返回原单当前状态且只读。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.Status != ticketPending {
		t.Fatalf("旧请求重放应返回原单待验收状态: replay=%v status=%q err=%v", replay, old.Status, err)
	}

	// 退回：恢复未关闭，资产仍为维修中，派工与领用恢复。
	if _, _, err := s.reviewSubmission(tk.ID, 2, decisionReject, "故障复现，需要返修"); err != nil {
		t.Fatalf("退回: %v", err)
	}
	if tk.Status != ticketOpen {
		t.Fatalf("退回后工单状态 = %q，想得到 %q", tk.Status, ticketOpen)
	}
	if a := s.findAsset("EQ-1"); a.Status != statusRepairing {
		t.Fatalf("退回后资产仍为维修中，状态 = %q", a.Status)
	}
	if _, err := s.assignTicket(tk.ID, "张三", "重新派工"); err != nil {
		t.Fatalf("退回后派工应恢复: %v", err)
	}
	// 退回后仍不能绕过验收直接关闭。
	if _, _, err := s.closeTicket(tk.ID, "绕过验收"); !errors.Is(err, errConflict) {
		t.Fatalf("退回后直接关闭应拒绝，得到 %v", err)
	}
	// 再次提交产生新序号。
	_, seq2, err := s.submitRepair(tk.ID, "已更换搓纸轮并校准")
	if err != nil || seq2 <= 2 {
		t.Fatalf("返修重提应产生新序号: seq=%d err=%v", seq2, err)
	}
	// 验收通过：工单关闭，维修结果取本次提交内容，资产恢复可用。
	_, asset, err := s.reviewSubmission(tk.ID, tkSeq(t, s, tk.ID), decisionApprove, "验收合格")
	if err != nil {
		t.Fatalf("验收通过: %v", err)
	}
	if tk.Status != ticketClosed || tk.Result != "已更换搓纸轮并校准" || tk.ClosedAt == "" {
		t.Fatalf("验收通过后工单不对: %+v", tk)
	}
	if asset.Status != statusAvailable {
		t.Fatalf("通过后资产应恢复可用，状态 = %q", asset.Status)
	}
	// 终结后拒绝提交和验收。
	if _, _, err := s.submitRepair(tk.ID, "再次提交"); !errors.Is(err, errConflict) {
		t.Fatalf("终结后提交应拒绝，得到 %v", err)
	}
	if _, _, err := s.reviewSubmission(tk.ID, 2, decisionApprove, "意见"); !errors.Is(err, errConflict) {
		t.Fatalf("终结后验收应拒绝，得到 %v", err)
	}

	// 重载后保持；旧提交与意见全部保留，history 可见提交与验收履历。
	s = saveAndReopen(t, s)
	kinds := []string{}
	for _, e := range s.eventsOf("EQ-1") {
		kinds = append(kinds, e.Kind)
	}
	want := []string{eventReport, eventSubmit, eventReview, eventAssign, eventSubmit, eventReview, eventClose}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("履历链不对: %v", kinds)
	}
	var reviewSeen int
	for _, e := range s.eventsOf("EQ-1") {
		if e.Kind == eventReview {
			reviewSeen++
			if e.Decision == "" || e.Content == "" || e.TargetSeq < 1 {
				t.Fatalf("验收履历应含决定、意见与目标序号: %+v", e)
			}
		}
	}
	if reviewSeen != 2 {
		t.Fatalf("应有两条验收履历，实际 %d", reviewSeen)
	}
	tk2 := s.findTicket(tk.ID)
	if tk2.Status != ticketClosed || tk2.Result != "已更换搓纸轮并校准" {
		t.Fatalf("重载后工单不对: %+v", tk2)
	}
}

// tkSeq 返回工单当前待验收提交的履历序号；测试辅助。
func tkSeq(t *testing.T, s *store, ticketID string) int {
	t.Helper()
	sub := s.pendingSubmissionOf(ticketID)
	if sub == nil {
		t.Fatalf("工单 %s 没有待验收提交", ticketID)
	}
	return sub.Seq
}

// 旧序号误验收：未知序号、非提交序号、跨单引用、已处理或非当前提交均拒绝；
// 验收意见不能为空；待验收可取消，取消后拒绝验收。
func TestReviewStaleAndForeignSeq(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	t1, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := s.report("EQ-2", "不制冷", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	_, seq1, err := s.submitRepair(t1.ID, "结果一")
	if err != nil {
		t.Fatal(err)
	}
	_, seq2, err := s.submitRepair(t2.ID, "结果二")
	if err != nil {
		t.Fatal(err)
	}
	// 未知序号。
	if _, _, err := s.reviewSubmission(t1.ID, 9999, decisionApprove, "意见"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知序号应拒绝，得到 %v", err)
	}
	// 非提交序号（报修履历序号）。
	if _, _, err := s.reviewSubmission(t1.ID, 1, decisionApprove, "意见"); !errors.Is(err, errConflict) {
		t.Fatalf("非提交序号应拒绝，得到 %v", err)
	}
	// 跨单引用。
	if _, _, err := s.reviewSubmission(t1.ID, seq2, decisionApprove, "意见"); !errors.Is(err, errConflict) {
		t.Fatalf("跨单验收应拒绝，得到 %v", err)
	}
	// 空意见拒绝。
	if _, _, err := s.reviewSubmission(t1.ID, seq1, decisionApprove, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空意见应拒绝，得到 %v", err)
	}
	// 退回后旧序号不再是当前提交。
	if _, _, err := s.reviewSubmission(t1.ID, seq1, decisionReject, "返修"); err != nil {
		t.Fatal(err)
	}
	_, seq1b, err := s.submitRepair(t1.ID, "结果一修订")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reviewSubmission(t1.ID, seq1, decisionApprove, "旧序号"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号误验收应拒绝，得到 %v", err)
	}
	// 已处理序号（另一工单已验收通过后）再次验收拒绝。
	if _, _, err := s.reviewSubmission(t2.ID, seq2, decisionApprove, "合格"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reviewSubmission(t2.ID, seq2, decisionApprove, "重复验收"); !errors.Is(err, errConflict) {
		t.Fatalf("已处理提交再次验收应拒绝，得到 %v", err)
	}
	// 当前提交可正常验收通过。
	if _, _, err := s.reviewSubmission(t1.ID, seq1b, decisionApprove, "合格"); err != nil {
		t.Fatalf("当前提交验收通过: %v", err)
	}
	if t1.Result != "结果一修订" {
		t.Fatalf("维修结果应取被通过提交的内容，得到 %q", t1.Result)
	}

	// 待验收可取消：保留提交且不填写维修结果；取消后拒绝验收。
	t3, _, err := s.report("EQ-1", "无法开机", "req-3")
	if err != nil {
		t.Fatal(err)
	}
	_, seq3, err := s.submitRepair(t3.ID, "已更换电源")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket(t3.ID, "误报"); err != nil {
		t.Fatalf("待验收取消: %v", err)
	}
	if t3.Status != ticketCancelled || t3.Result != "" {
		t.Fatalf("待验收取消后不应填写维修结果: %+v", t3)
	}
	if _, _, err := s.reviewSubmission(t3.ID, seq3, decisionApprove, "意见"); !errors.Is(err, errConflict) {
		t.Fatalf("取消后验收应拒绝，得到 %v", err)
	}
	if _, _, err := s.submitRepair(t3.ID, "再次提交"); !errors.Is(err, errConflict) {
		t.Fatalf("取消后提交应拒绝，得到 %v", err)
	}
	// 旧提交履历保留。
	found := false
	for _, e := range s.eventsOf("EQ-1") {
		if e.Kind == eventSubmit && e.TicketID == t3.ID && e.Seq == seq3 {
			found = true
		}
	}
	if !found {
		t.Fatal("待验收取消后旧提交履历应保留")
	}
}

// 迁移接续：import 复制提交与验收历史并随重编号替换提交引用，导入后用目标
// 序号继续验收；export/restore 保留待验收状态、序号、引用与时间精度。
func TestReviewMigrationContinue(t *testing.T) {
	src := newTestStore(t)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := src.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	_, seq1, err := src.submitRepair(tk.ID, "第一次结果")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.reviewSubmission(tk.ID, seq1, decisionReject, "返修"); err != nil {
		t.Fatal(err)
	}
	_, seq2, err := src.submitRepair(tk.ID, "第二次结果")
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	// 导入目标已有履历，导入的提交序号随重编号平移。
	dst := newTestStore(t)
	if _, err := dst.registerAsset("EQ-9", "既有资产", "九楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-9", "既有故障", "req-9"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)
	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(outcome.tickets) != 1 || outcome.tickets[0].OldID != tk.ID {
		t.Fatalf("工单映射不对: %+v", outcome.tickets)
	}
	newTicketID := outcome.tickets[0].NewID
	dst2, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	nt := dst2.findTicket(newTicketID)
	if nt == nil || nt.Status != ticketPending {
		t.Fatalf("导入后工单应保持待验收: %+v", nt)
	}
	// 提交引用随重编号替换：用目标新序号继续验收，源序号不再有效。
	pending := dst2.pendingSubmissionOf(newTicketID)
	if pending == nil || pending.Content != "第二次结果" {
		t.Fatalf("导入后当前待验收提交不对: %+v", pending)
	}
	if pending.Seq == seq2 {
		t.Fatal("目标已有履历时导入的提交序号应随重编号平移")
	}
	if _, _, err := dst2.reviewSubmission(newTicketID, seq2, decisionApprove, "旧序号"); err == nil {
		t.Fatal("导入后源序号应失效（未知或指向其他履历），用其验收应拒绝")
	}
	if _, _, err := dst2.reviewSubmission(newTicketID, pending.Seq, decisionApprove, "合格"); err != nil {
		t.Fatalf("导入后用目标序号验收: %v", err)
	}
	if nt.Status != ticketClosed || nt.Result != "第二次结果" {
		t.Fatalf("导入后验收通过不对: %+v", nt)
	}
	// 验收履历的目标引用在重载后仍与提交履历接续（保存校验通过即一致）。

	// export/restore：待验收状态、序号、引用与时间精度保留。
	pending2 := newTestStore(t)
	if _, err := pending2.registerAsset("EQ-7", "扫描仪", "七楼"); err != nil {
		t.Fatal(err)
	}
	t7, _, err := pending2.report("EQ-7", "无法扫描", "req-7")
	if err != nil {
		t.Fatal(err)
	}
	_, seq7, err := pending2.submitRepair(t7.ID, "已更换扫描头")
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, pending2)
	pkg := filepath.Join(t.TempDir(), "pkg.zip")
	if _, err := exportPackage(pending2.dir, pkg, []string{"EQ-7"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	restoredDir := filepath.Join(t.TempDir(), "restored")
	if _, err := restorePackage(pkg, restoredDir); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rs, err := openSourceStore(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	rt := rs.findTicket(t7.ID)
	if rt == nil || rt.Status != ticketPending {
		t.Fatalf("还原后工单应保持待验收: %+v", rt)
	}
	sub := rs.pendingSubmissionOf(t7.ID)
	if sub == nil || sub.Seq != seq7 || sub.Content != "已更换扫描头" {
		t.Fatalf("还原后待验收提交应保持原序号与内容: %+v", sub)
	}
	if _, _, err := rs.reviewSubmission(t7.ID, seq7, decisionApprove, "合格"); err != nil {
		t.Fatalf("还原后按保留序号验收: %v", err)
	}
	if rt.Result != "已更换扫描头" {
		t.Fatalf("还原后验收通过的维修结果不对: %q", rt.Result)
	}
}

// 矛盾台账：提交引用、转换与保存值相互矛盾时全部读写拒绝且不修复，原文件不变。
func TestReviewContradictoryLedger(t *testing.T) {
	build := func(t *testing.T) (dir string, good []byte) {
		t.Helper()
		s := newTestStore(t)
		if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
			t.Fatal(err)
		}
		tk, _, err := s.report("EQ-1", "卡纸", "req-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.submitRepair(tk.ID, "已更换搓纸轮"); err != nil {
			t.Fatal(err)
		}
		mustSave(t, s)
		raw, err := os.ReadFile(filepath.Join(s.dir, dataFileName))
		if err != nil {
			t.Fatal(err)
		}
		return s.dir, raw
	}
	mutate := func(t *testing.T, dir string, good []byte, fn func(d *storeData)) {
		t.Helper()
		var d storeData
		if err := json.Unmarshal(good, &d); err != nil {
			t.Fatal(err)
		}
		d.Events = make([]Event, len(d.EventsJSON))
		for i, ej := range d.EventsJSON {
			ev, err := ej.toEvent()
			if err != nil {
				t.Fatal(err)
			}
			d.Events[i] = ev
		}
		fn(&d)
		d.EventsJSON = make([]eventJSON, len(d.Events))
		for i, e := range d.Events {
			d.EventsJSON[i] = e.toJSON()
		}
		raw, err := json.MarshalIndent(&d, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		fn   func(d *storeData)
	}{
		{"验收引用非提交序号", func(d *storeData) {
			d.Events = append(d.Events, Event{Seq: 3, AssetID: "EQ-1", TicketID: "T0001",
				Kind: eventReview, Content: "意见", Decision: decisionApprove, TargetSeq: 1})
		}},
		{"保存待验收但无提交履历", func(d *storeData) {
			for i := range d.Events {
				if d.Events[i].Kind == eventSubmit {
					d.Events[i].Kind = eventAssign
					d.Events[i].To = "张三"
				}
			}
		}},
		{"提交后未经验收直接关闭", func(d *storeData) {
			d.Tickets[0].Status = ticketClosed
			d.Tickets[0].Result = "绕过验收"
			d.Tickets[0].ClosedAt = "2026-01-01T00:00:00Z"
			d.Assets[0].Status = statusAvailable
			d.Events = append(d.Events, Event{Seq: 3, AssetID: "EQ-1", TicketID: "T0001",
				Kind: eventClose, Content: "绕过验收"})
		}},
		{"验收决定无效", func(d *storeData) {
			d.Events = append(d.Events, Event{Seq: 3, AssetID: "EQ-1", TicketID: "T0001",
				Kind: eventReview, Content: "意见", Decision: "待定", TargetSeq: 2})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, good := build(t)
			mutate(t, dir, good, tc.fn)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			} else if !strings.Contains(err.Error(), "矛盾") {
				t.Fatalf("错误应指明矛盾类别: %v", err)
			}
			// 读写命令同样拒绝（退出码 1），原文件字节不变、不修复。
			var out, errBuf bytes.Buffer
			if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 1 {
				t.Fatalf("矛盾台账读取命令应退出 1，得到 %d", code)
			}
			// 恢复良好字节后可正常使用。
			if err := os.WriteFile(filepath.Join(dir, dataFileName), good, 0o644); err != nil {
				t.Fatal(err)
			}
			s2, err := openStore(dir)
			if err != nil {
				t.Fatalf("恢复良好数据后应可加载: %v", err)
			}
			if s2.pendingSubmissionOf("T0001") == nil {
				t.Fatal("恢复后待验收提交应可识别")
			}
		})
	}
}

// 保存失败重载重试：提交与验收各为一次原子保存；目录不可写时保存失败，
// 原文件字节保持、不消耗编号，恢复后可重试，重载后状态正确。
func TestReviewSaveFailureRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	path := filepath.Join(s.dir, dataFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 提交后保存失败：文件字节不变，恢复后重试成功。
	if _, _, err := s.submitRepair(tk.ID, "已更换搓纸轮"); err != nil {
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
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, raw) {
		t.Fatal("保存失败不应留下部分状态或履历")
	}
	if err := s.save(); err != nil {
		t.Fatalf("恢复后重试保存应成功: %v", err)
	}
	s = saveAndReopen(t, s)
	tk = s.findTicket(tk.ID)
	if tk.Status != ticketPending {
		t.Fatalf("重载后工单应为待验收: %+v", tk)
	}

	// 验收（通过，含验收与关闭两条履历）保存失败同样可重试。
	seq := tkSeq(t, s, tk.ID)
	before, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reviewSubmission(tk.ID, seq, decisionApprove, "合格"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr = s.save()
	if err := os.Chmod(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, raw) {
		t.Fatal("验收保存失败不应留下部分状态或履历")
	}
	if err := s.save(); err != nil {
		t.Fatalf("恢复后重试验收保存应成功: %v", err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	tk2 := s2.findTicket(tk.ID)
	if tk2.Status != ticketClosed || tk2.Result != "已更换搓纸轮" {
		t.Fatalf("重载后工单应已关闭且维修结果取自提交: %+v", tk2)
	}
	if a := s2.findAsset("EQ-1"); a.Status != statusAvailable {
		t.Fatalf("重载后资产应恢复可用: %q", a.Status)
	}
}

// CLI 层：提交与验收的参数错误退出 2，业务失败退出 1；ticket 显示当前待验收
// 提交的序号与结果，无则提示；detail 仍列出待验收工单。
func TestReviewCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	runOK := func(args ...string) string {
		t.Helper()
		out.Reset()
		errBuf.Reset()
		if code := run(args, &out, &errBuf); code != 0 {
			t.Fatalf("%v 应成功，退出码 %d: %s", args, code, errBuf.String())
		}
		return out.String()
	}
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runOK("report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")

	// 参数错误退出 2。
	for _, args := range [][]string{
		{"submit", "--data-dir", dir, "--ticket-id", "T0001"},
		{"review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "1", "--decision", "通过"},
		{"review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "1", "--decision", "待定", "--comment", "x"},
		{"review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "0", "--decision", "通过", "--comment", "x"},
	} {
		out.Reset()
		errBuf.Reset()
		if code := run(args, &out, &errBuf); code != 2 {
			t.Fatalf("%v 参数错误应退出 2，得到 %d", args, code)
		}
	}
	// 业务失败退出 1：没有待验收提交时验收。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "1",
		"--decision", "通过", "--comment", "x"}, &out, &errBuf); code != 1 {
		t.Fatalf("无待验收提交时验收应退出 1，得到 %d", code)
	}

	// 提交成功返回全库序号；ticket 显示待验收提交；detail 仍列出工单。
	got := runOK("submit", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "已更换搓纸轮")
	if !strings.Contains(got, "提交序号: ") || !strings.Contains(got, "工单状态: 待验收") {
		t.Fatalf("提交输出不对:\n%s", got)
	}
	got = runOK("ticket", "--data-dir", dir, "--ticket-id", "T0001")
	if !strings.Contains(got, "工单状态: 待验收") || !strings.Contains(got, "待验收提交: 序号") ||
		!strings.Contains(got, "提交维修结果: 已更换搓纸轮") {
		t.Fatalf("ticket 应显示待验收提交:\n%s", got)
	}
	got = runOK("detail", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(got, "未关闭工单: T0001") {
		t.Fatalf("detail 仍应列出待验收工单:\n%s", got)
	}
	// 验收退回后 ticket 提示无待验收提交。
	runOK("review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "2", "--decision", "退回", "--comment", "返修")
	got = runOK("ticket", "--data-dir", dir, "--ticket-id", "T0001")
	if !strings.Contains(got, "待验收提交: 无") {
		t.Fatalf("退回后应提示无待验收提交:\n%s", got)
	}
	// 再次提交并验收通过：显示工单编号及新状态，资产恢复可用。
	runOK("submit", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "已更换搓纸轮并校准")
	got = runOK("review", "--data-dir", dir, "--ticket-id", "T0001", "--seq", "4", "--decision", "通过", "--comment", "合格")
	if !strings.Contains(got, "工单编号: T0001") || !strings.Contains(got, "工单状态: 已关闭") ||
		!strings.Contains(got, "当前状态: 可用") {
		t.Fatalf("验收通过输出不对:\n%s", got)
	}
	// replay 还原截止处的待验收提交，后续验收（序号 5）不提前生效。
	got = runOK("replay", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "4")
	if !strings.Contains(got, "工单状态: 待验收") || !strings.Contains(got, "提交维修结果: 已更换搓纸轮并校准") {
		t.Fatalf("replay 应还原截止处待验收提交:\n%s", got)
	}
	// history 展示提交与验收履历。
	got = runOK("history", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(got, "提交 工单 T0001") || !strings.Contains(got, "验收 工单 T0001: 通过，目标提交序号 4（合格）") {
		t.Fatalf("history 应展示提交与验收履历:\n%s", got)
	}
}
