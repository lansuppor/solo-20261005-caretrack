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

// 提交-验收通过全流程：提交后工单待验收、资产仍维修中；验收通过后工单按该次
// 提交的维修结果关闭，资产恢复可用；履历含提交、验收与关闭三条记录。
func TestSubmitApproveLifecycle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}

	// 提交维修结果：工单转为待验收，返回提交履历的全库序号。
	tk, submitSeq, err := s.submitRepair(tk.ID, "已更换搓纸轮")
	if err != nil {
		t.Fatalf("submitRepair: %v", err)
	}
	if tk.Status != ticketPending {
		t.Fatalf("提交后工单状态 = %q，想得到 %q", tk.Status, ticketPending)
	}
	if submitSeq != 2 {
		t.Fatalf("提交序号 = %d，想得到 2", submitSeq)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("待验收时资产应仍为维修中，得到 %q", got)
	}
	seq, result, ok := s.currentSubmission(tk.ID)
	if !ok || seq != submitSeq || result != "已更换搓纸轮" {
		t.Fatalf("当前待验收提交 = (%d, %q, %v)", seq, result, ok)
	}

	// 验收通过：工单按提交的维修结果关闭，资产恢复可用。
	tk, asset, err := s.acceptSubmission(tk.ID, submitSeq, decisionApprove, "复核通过")
	if err != nil {
		t.Fatalf("acceptSubmission: %v", err)
	}
	if tk.Status != ticketClosed || tk.Result != "已更换搓纸轮" || tk.ClosedAt == "" {
		t.Fatalf("验收通过后工单异常: %+v", tk)
	}
	if asset.Status != statusAvailable {
		t.Fatalf("验收通过后资产状态 = %q", asset.Status)
	}

	// 履历：提交、验收（含决定、意见与目标序号）、关闭（内容为提交结果）。
	events := s.eventsOf("EQ-1")
	if len(events) != 4 {
		t.Fatalf("履历条数 = %d，想得到 4", len(events))
	}
	if events[1].Kind != eventSubmit || events[1].Content != "已更换搓纸轮" || events[1].Seq != submitSeq {
		t.Fatalf("提交履历不对: %+v", events[1])
	}
	if events[2].Kind != eventAccept || events[2].Decision != decisionApprove ||
		events[2].Content != "复核通过" || events[2].TargetSeq != submitSeq {
		t.Fatalf("验收履历不对: %+v", events[2])
	}
	if events[3].Kind != eventClose || events[3].Content != "已更换搓纸轮" {
		t.Fatalf("关闭履历不对: %+v", events[3])
	}

	// 终结后拒绝提交与验收。
	if _, _, err := s.submitRepair(tk.ID, "再次提交"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单提交应失败，得到 %v", err)
	}
	if _, _, err := s.acceptSubmission(tk.ID, submitSeq, decisionApprove, "再次验收"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单验收应失败，得到 %v", err)
	}

	// 重载后状态、结果与履历保持；旧请求重放返回已关闭工单。
	s = saveAndReopen(t, s)
	tk = s.findTicket("T0001")
	if tk.Status != ticketClosed || tk.Result != "已更换搓纸轮" {
		t.Fatalf("重载后工单状态未保持: %+v", tk)
	}
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.Status != ticketClosed {
		t.Fatalf("重载后旧请求重放应返回已关闭工单: %v replay=%v err=%v", old, replay, err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("重载后履历条数 = %d", got)
	}

	// 停机区间：等待验收的时间计入原报修至关闭的停机区间。
	var reportTime, closeTime int64
	for _, e := range s.data.Events {
		switch e.Kind {
		case eventReport:
			reportTime = e.Time.Unix()
		case eventClose:
			closeTime = e.Time.Unix()
		}
	}
	res, err := s.downtimeForAssets([]string{"EQ-1"},
		time.Unix(reportTime-10, 0), time.Unix(closeTime+10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Seconds != closeTime-reportTime {
		t.Fatalf("停机秒数 = %d，想得到 %d", res[0].Seconds, closeTime-reportTime)
	}
}

// 返修重提：验收退回后工单恢复未关闭、资产仍维修中，不能绕过验收直接关闭；
// 再次提交产生新序号，验收通过后按新提交结果关闭；旧提交与意见全部保留。
func TestSubmitRejectAndResubmit(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")

	tk, seq1, err := s.submitRepair(tk.ID, "已清理纸路")
	if err != nil {
		t.Fatal(err)
	}
	// 重复提交被拒绝。
	if _, _, err := s.submitRepair(tk.ID, "重复提交"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时重复提交应失败，得到 %v", err)
	}
	// 验收退回：恢复未关闭，资产仍维修中，不填写维修结果。
	tk, asset, err := s.acceptSubmission(tk.ID, seq1, decisionReject, "仍有卡纸，需返修")
	if err != nil {
		t.Fatalf("验收退回: %v", err)
	}
	if tk.Status != ticketOpen || tk.Result != "" {
		t.Fatalf("退回后工单异常: %+v", tk)
	}
	if asset.Status != statusRepairing {
		t.Fatalf("退回后资产应仍为维修中，得到 %q", asset.Status)
	}
	// 退回后不能绕过验收直接关闭。
	if _, _, err := s.closeTicket(tk.ID, "直接关闭"); !errors.Is(err, errConflict) {
		t.Fatalf("首次提交后直接关闭应失败，得到 %v", err)
	}
	// 再次提交产生新序号。
	tk, seq2, err := s.submitRepair(tk.ID, "已更换搓纸轮")
	if err != nil {
		t.Fatal(err)
	}
	if seq2 <= seq1 {
		t.Fatalf("再次提交序号 %d 应大于首次 %d", seq2, seq1)
	}
	// 第二次验收通过：按第二次提交内容关闭。
	tk, _, err = s.acceptSubmission(tk.ID, seq2, decisionApprove, "复核通过")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Result != "已更换搓纸轮" {
		t.Fatalf("维修结果应取第二次提交内容，得到 %q", tk.Result)
	}

	// 旧提交与验收意见全部保留：提交×2、验收×2、关闭×1。
	var submits, accepts, closes int
	for _, e := range s.eventsOf("EQ-1") {
		switch e.Kind {
		case eventSubmit:
			submits++
		case eventAccept:
			accepts++
			if e.TargetSeq == seq1 && (e.Decision != decisionReject || e.Content != "仍有卡纸，需返修") {
				t.Fatalf("首次验收履历未保留: %+v", e)
			}
		case eventClose:
			closes++
		}
	}
	if submits != 2 || accepts != 2 || closes != 1 {
		t.Fatalf("履历统计 = 提交%d/验收%d/关闭%d，想得到 2/2/1", submits, accepts, closes)
	}
	s = saveAndReopen(t, s)
	if got := s.findTicket("T0001"); got.Status != ticketClosed || got.Result != "已更换搓纸轮" {
		t.Fatalf("重载后工单异常: %+v", got)
	}
}

// 旧序号误验收与其他非法验收目标：已处理的旧提交序号、非提交序号、未知序号、
// 跨单提交序号均拒绝；非待验收工单的验收也拒绝。
func TestAcceptWrongSeqRejected(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	_, _ = s.registerAsset("EQ-2", "空调", "二楼")
	tk1, _, _ := s.report("EQ-1", "卡纸", "req-1")
	tk2, _, _ := s.report("EQ-2", "不制冷", "req-2")

	_, seq1, _ := s.submitRepair(tk1.ID, "第一次结果")
	if _, _, err := s.acceptSubmission(tk1.ID, seq1, decisionReject, "返修"); err != nil {
		t.Fatal(err)
	}
	_, seq2, _ := s.submitRepair(tk1.ID, "第二次结果")
	_, otherSeq, _ := s.submitRepair(tk2.ID, "他单结果")

	// 已处理的旧提交序号。
	if _, _, err := s.acceptSubmission(tk1.ID, seq1, decisionApprove, "误验收"); !errors.Is(err, errConflict) {
		t.Fatalf("旧提交序号验收应失败，得到 %v", err)
	}
	// 非提交序号（报修履历序号）。
	if _, _, err := s.acceptSubmission(tk1.ID, 1, decisionApprove, "误验收"); !errors.Is(err, errConflict) {
		t.Fatalf("非提交序号验收应失败，得到 %v", err)
	}
	// 未知序号。
	if _, _, err := s.acceptSubmission(tk1.ID, 9999, decisionApprove, "误验收"); !errors.Is(err, errConflict) {
		t.Fatalf("未知序号验收应失败，得到 %v", err)
	}
	// 跨单提交序号。
	if _, _, err := s.acceptSubmission(tk1.ID, otherSeq, decisionApprove, "误验收"); !errors.Is(err, errConflict) {
		t.Fatalf("跨单提交序号验收应失败，得到 %v", err)
	}
	// 全部拒绝后当前待验收提交不受影响。
	seq, result, ok := s.currentSubmission(tk1.ID)
	if !ok || seq != seq2 || result != "第二次结果" {
		t.Fatalf("非法验收不应改变当前提交: (%d, %q, %v)", seq, result, ok)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("非法验收不应产生履历，履历数 = %d", got)
	}
	// 未知工单与未提交工单的验收。
	if _, _, err := s.acceptSubmission("T9999", 1, decisionApprove, "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单验收应失败，得到 %v", err)
	}
	// 另一工单的当前提交可正常验收（对 T0001 的误验收不影响他单）。
	if _, _, err := s.acceptSubmission(tk2.ID, otherSeq, decisionApprove, "他单验收"); err != nil {
		t.Fatalf("他单当前提交验收应成功: %v", err)
	}
	// 未提交过的工单验收应失败。
	if _, err := s.registerAsset("EQ-3", "投影仪", "三楼"); err != nil {
		t.Fatal(err)
	}
	tk3, _, err := s.report("EQ-3", "无法开机", "req-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.acceptSubmission(tk3.ID, 1, decisionApprove, "x"); !errors.Is(err, errConflict) {
		t.Fatalf("未提交工单验收应失败，得到 %v", err)
	}
}

// 待验收期间的限制：拒绝新报修、停用、派工、备件领用退回与重复提交；旧请求
// 重放只读返回原单当前状态；待验收可取消，取消后拒绝提交与验收。
func TestPendingRestrictions(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	p, err := s.withdrawPart(tk.ID, "FILTER-01", 2, "更换滤芯")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.submitRepair(tk.ID, "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}

	// 新报修、停用、派工、领用、退回、重复提交、直接关闭均拒绝。
	if _, _, err := s.report("EQ-1", "新故障", "req-2"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时新报修应失败，得到 %v", err)
	}
	if _, err := s.deactivateAsset("EQ-1", "调拨"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时停用应失败，得到 %v", err)
	}
	if _, err := s.assignTicket(tk.ID, "李四", "转派"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时派工应失败，得到 %v", err)
	}
	if _, err := s.withdrawPart(tk.ID, "FILTER-02", 1, "再领"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时领用应失败，得到 %v", err)
	}
	if _, _, err := s.returnPart(p.ID, 1, "多余退回"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时退回应失败，得到 %v", err)
	}
	if _, _, err := s.submitRepair(tk.ID, "重复提交"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时重复提交应失败，得到 %v", err)
	}
	if _, _, err := s.closeTicket(tk.ID, "直接关闭"); !errors.Is(err, errConflict) {
		t.Fatalf("待验收时直接关闭应失败，得到 %v", err)
	}
	// 旧请求重放：只读返回原单当前状态（待验收），不新增记录。
	before := len(s.data.Events)
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.Status != ticketPending {
		t.Fatalf("重放应返回待验收原单: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Events) != before {
		t.Fatal("重放不应新增履历")
	}
	// detail 仍列出待验收工单。
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != tk.ID {
		t.Fatalf("待验收工单应仍列为未终结，得到 %v", got)
	}

	// 待验收可取消：保留提交，不填写维修结果，资产恢复可用。
	tk, asset, err := s.cancelTicket(tk.ID, "误报，设备实际正常")
	if err != nil {
		t.Fatalf("待验收取消: %v", err)
	}
	if tk.Status != ticketCancelled || tk.Result != "" {
		t.Fatalf("取消后工单异常: %+v", tk)
	}
	if asset.Status != statusAvailable {
		t.Fatalf("取消后资产状态 = %q", asset.Status)
	}
	if !s.hasSubmission(tk.ID) {
		t.Fatal("取消后提交履历应保留")
	}
	// 终结后拒绝提交与验收。
	if _, _, err := s.submitRepair(tk.ID, "再提交"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单提交应失败，得到 %v", err)
	}
	if _, _, err := s.acceptSubmission(tk.ID, 3, decisionApprove, "x"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单验收应失败，得到 %v", err)
	}
	// 退回备件在终结后同样拒绝（旧单操作不影响新单）。
	if _, _, err := s.returnPart(p.ID, 1, "多余退回"); !errors.Is(err, errConflict) {
		t.Fatalf("终结后退回应失败，得到 %v", err)
	}
}

// 迁移接续：import 复制提交与验收履历并随重编号替换验收引用，导入后用目标
// 序号继续验收。
func TestImportPendingSubmissionContinues(t *testing.T) {
	src := newTestStore(t)
	_, _ = src.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := src.report("EQ-1", "卡纸", "req-1")
	_, srcSubmitSeq, _ := src.submitRepair(tk.ID, "已更换搓纸轮")
	if err := src.save(); err != nil {
		t.Fatal(err)
	}

	dst := newTestStore(t)
	_, _ = dst.registerAsset("EQ-9", "空调", "九楼")
	if _, _, err := dst.report("EQ-9", "不制冷", "req-9"); err != nil {
		t.Fatal(err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}

	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("importAssets: %v", err)
	}
	if len(outcome.submissions) != 1 || outcome.submissions[0].OldSeq != srcSubmitSeq {
		t.Fatalf("提交序号映射不对: %+v", outcome.submissions)
	}
	newSubmitSeq := outcome.submissions[0].NewSeq
	if newSubmitSeq == srcSubmitSeq {
		t.Fatal("目标已有履历时提交序号应重新分配")
	}

	// 导入后工单为待验收，当前提交序号为映射后的新序号。
	s2, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	nt := s2.findTicket(outcome.tickets[0].NewID)
	if nt == nil || nt.Status != ticketPending {
		t.Fatalf("导入后工单应为待验收: %+v", nt)
	}
	seq, result, ok := s2.currentSubmission(nt.ID)
	if !ok || seq != newSubmitSeq || result != "已更换搓纸轮" {
		t.Fatalf("导入后当前提交 = (%d, %q, %v)，想得到 (%d, 已更换搓纸轮, true)",
			seq, result, ok, newSubmitSeq)
	}
	// 用目标序号继续验收：通过并关闭。
	nt, asset, err := s2.acceptSubmission(nt.ID, newSubmitSeq, decisionApprove, "复核通过")
	if err != nil {
		t.Fatalf("导入后验收: %v", err)
	}
	if nt.Status != ticketClosed || nt.Result != "已更换搓纸轮" || asset.Status != statusAvailable {
		t.Fatalf("导入后验收结果异常: %+v / %q", nt, asset.Status)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	// 重载后验收履历的目标序号与重编号一致。
	s3, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	var acceptEv *Event
	for i := range s3.data.Events {
		if s3.data.Events[i].Kind == eventAccept {
			acceptEv = &s3.data.Events[i]
		}
	}
	if acceptEv == nil || acceptEv.TargetSeq != newSubmitSeq {
		t.Fatalf("验收履历目标序号应为 %d: %+v", newSubmitSeq, acceptEv)
	}
	// 源台账只读：源库工单仍为待验收。
	src2, err := openStore(src.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := src2.findTicket("T0001"); got.Status != ticketPending {
		t.Fatalf("源工单应保持待验收，得到 %q", got.Status)
	}
}

// 矛盾台账：提交/验收链的归属、转换、引用与保存值矛盾时全部读写拒绝且不修复。
func TestContradictorySubmissionLedgers(t *testing.T) {
	submitEvent := func(seq int, ticketID, result string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": ticketID,
			"kind": "提交", "content": result, "time": "2026-10-01T10:30:00Z",
		}
	}
	acceptEvent := func(seq int, ticketID string, target int, decision string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": ticketID,
			"kind": "验收", "content": "意见", "decision": decision,
			"target_seq": target, "time": "2026-10-01T11:00:00Z",
		}
	}
	pendingTicket := func(m map[string]any) {
		m["tickets"].([]any)[0].(map[string]any)["status"] = "待验收"
		appendTo(m, "events", submitEvent(4, "T0001", "已更换搓纸轮"))
	}
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"保存待验收但无提交履历", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["status"] = "待验收"
		}, "状态矛盾"},
		{"有提交履历但保存未关闭", func(m map[string]any) {
			appendTo(m, "events", submitEvent(4, "T0001", "已更换搓纸轮"))
		}, "状态矛盾"},
		{"验收目标不是当前提交", func(m map[string]any) {
			pendingTicket(m)
			appendTo(m, "events", acceptEvent(5, "T0001", 99, "通过"))
		}, "履历矛盾"},
		{"验收决定无效", func(m map[string]any) {
			pendingTicket(m)
			appendTo(m, "events", acceptEvent(5, "T0001", 4, "待定"))
		}, "履历矛盾"},
		{"提交退回后直接关闭", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "直接关闭"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
			appendTo(m, "events", submitEvent(4, "T0001", "已更换搓纸轮"))
			appendTo(m, "events", acceptEvent(5, "T0001", 4, "退回"))
			appendTo(m, "events", eventJSONMap(6, "T0001", "关闭", "直接关闭"))
		}, "履历矛盾"},
		{"验收通过但缺关闭履历", func(m map[string]any) {
			pendingTicket(m)
			appendTo(m, "events", acceptEvent(5, "T0001", 4, "通过"))
		}, "状态矛盾"},
		{"跨单验收引用", func(m map[string]any) {
			pendingTicket(m)
			m["assets"] = append(m["assets"].([]any), map[string]any{
				"id": "EQ-2", "name": "空调", "location": "二楼", "status": "维修中",
			})
			m["tickets"] = append(m["tickets"].([]any), map[string]any{
				"id": "T0002", "asset_id": "EQ-2", "description": "不制冷",
				"request_id": "req-2", "status": "待验收", "created_at": "2026-10-01T09:30:00Z",
			})
			m["requests"] = append(m["requests"].([]any), map[string]any{
				"request_id": "req-2", "asset_id": "EQ-2", "description": "不制冷", "ticket_id": "T0002",
			})
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-2", "ticket_id": "T0002",
				"kind": "报修", "content": "不制冷", "time": "2026-10-01T09:30:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 6, "asset_id": "EQ-2", "ticket_id": "T0002",
				"kind": "提交", "content": "已加氟", "time": "2026-10-01T10:00:00Z",
			})
			// T0001 的验收指向 T0002 的提交序号。
			appendTo(m, "events", acceptEvent(7, "T0001", 6, "通过"))
		}, "履历矛盾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, tc.mutate)
			if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("矛盾台账应拒绝（%s），得到 %v", tc.wantErr, err)
			}
			var out, errBuf bytes.Buffer
			if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 1 {
				t.Fatalf("查询应退出 1，得到 %d（%s）", code, errBuf.String())
			}
			if code := run([]string{"submit", "--data-dir", dir, "--ticket-id", "T0001",
				"--repair-result", "x"}, &out, &errBuf); code != 1 {
				t.Fatalf("写入应退出 1，得到 %d（%s）", code, errBuf.String())
			}
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("矛盾台账原文件字节应保持不变")
			}
		})
	}
}

// 有效的待验收旧库（序号有间隔）直接兼容：加载后可用当前提交序号继续验收。
func TestValidPendingLegacyLedger(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		m["tickets"].([]any)[0].(map[string]any)["status"] = "待验收"
		appendTo(m, "events", map[string]any{
			"seq": 7, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "提交", "content": "已更换搓纸轮", "time": "2026-10-01T10:30:00Z",
		})
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效待验收旧库应能加载: %v", err)
	}
	seq, result, ok := s.currentSubmission("T0001")
	if !ok || seq != 7 || result != "已更换搓纸轮" {
		t.Fatalf("当前提交 = (%d, %q, %v)", seq, result, ok)
	}
	tk, asset, err := s.acceptSubmission("T0001", 7, decisionApprove, "复核通过")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != ticketClosed || tk.Result != "已更换搓纸轮" || asset.Status != statusAvailable {
		t.Fatalf("验收后状态异常: %+v / %q", tk, asset.Status)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findTicket("T0001"); got.Status != ticketClosed || got.Result != "已更换搓纸轮" {
		t.Fatalf("重载后工单异常: %+v", got)
	}
}

// 保存失败重载重试：提交与验收在写入失败时保留原文件字节、不留部分状态或
// 履历、不消耗编号，恢复后可按原输入重试。
func TestSubmitAcceptSaveFailureRetry(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, dataFileName)
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 提交后保存失败：文件字节不变，恢复后重试成功。
	_, seq, err := s.submitRepair("T0001", "已更换搓纸轮")
	if err != nil {
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
	rawNow, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(rawNow, rawBefore) {
		t.Fatal("保存失败不应留下部分变化")
	}
	if err := s.save(); err != nil {
		t.Fatalf("恢复后重试保存应成功: %v", err)
	}

	// 重载后待验收状态与提交序号保持，验收同样可重试。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	tk2 := s2.findTicket("T0001")
	if tk2.Status != ticketPending {
		t.Fatalf("重载后应为待验收，得到 %q", tk2.Status)
	}
	if got, _, ok := s2.currentSubmission("T0001"); !ok || got != seq {
		t.Fatalf("重载后提交序号 = %d, %v；想得到 %d, true", got, ok, seq)
	}
	rawMid, _ := os.ReadFile(path)
	if _, _, err := s2.acceptSubmission("T0001", seq, decisionApprove, "复核通过"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s2.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr = s2.save()
	if err := os.Chmod(s2.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过验收保存失败分支")
	}
	rawNow, _ = os.ReadFile(path)
	if !bytes.Equal(rawNow, rawMid) {
		t.Fatal("验收保存失败不应留下部分变化")
	}
	if err := s2.save(); err != nil {
		t.Fatalf("恢复后验收重试保存应成功: %v", err)
	}
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findTicket("T0001"); got.Status != ticketClosed || got.Result != "已更换搓纸轮" {
		t.Fatalf("重试后工单异常: %+v", got)
	}
}

// replay 还原截止处的工单状态与待验收提交，后续验收不提前生效。
func TestReplayPendingSubmission(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")                      // 履历 1
	_, seq1, _ := s.submitRepair(tk.ID, "第一次结果")                     // 履历 2
	_, _, _ = s.acceptSubmission(tk.ID, seq1, decisionReject, "返修")  // 履历 3
	_, seq2, _ := s.submitRepair(tk.ID, "第二次结果")                     // 履历 4
	_, _, _ = s.acceptSubmission(tk.ID, seq2, decisionApprove, "通过") // 履历 5、6（关闭）

	// 截止 2：待验收，当前提交为第一次。
	snap := s.replayAsset("EQ-1", 2)
	if snap.openTicket == nil || snap.openTicket.status != ticketPending ||
		snap.openTicket.submitSeq != seq1 || snap.openTicket.submitResult != "第一次结果" {
		t.Fatalf("截止 2 回看异常: %+v", snap.openTicket)
	}
	if snap.status != statusRepairing {
		t.Fatalf("截止 2 资产状态 = %q", snap.status)
	}
	// 截止 3：退回后恢复未关闭，无待验收提交。
	snap = s.replayAsset("EQ-1", 3)
	if snap.openTicket == nil || snap.openTicket.status != ticketOpen || snap.openTicket.submitSeq != 0 {
		t.Fatalf("截止 3 回看异常: %+v", snap.openTicket)
	}
	// 截止 4：再次待验收，当前提交为第二次。
	snap = s.replayAsset("EQ-1", 4)
	if snap.openTicket == nil || snap.openTicket.status != ticketPending ||
		snap.openTicket.submitSeq != seq2 || snap.openTicket.submitResult != "第二次结果" {
		t.Fatalf("截止 4 回看异常: %+v", snap.openTicket)
	}
	// 截止 5：验收通过已发生但关闭履历未到，仍待验收（后续关闭不提前生效）。
	snap = s.replayAsset("EQ-1", 5)
	if snap.openTicket == nil || snap.openTicket.status != ticketPending {
		t.Fatalf("截止 5 回看异常: %+v", snap.openTicket)
	}
	// 截止 6：工单已关闭，资产可用。
	snap = s.replayAsset("EQ-1", 6)
	if snap.openTicket != nil || snap.status != statusAvailable {
		t.Fatalf("截止 6 回看异常: %+v / %q", snap.openTicket, snap.status)
	}
}

// 命令行入口：参数错误退出 2，业务失败退出 1；成功输出提交序号、工单编号与新状态。
func TestSubmitAcceptCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	runCmd := func(args ...string) (int, string, string) {
		out.Reset()
		errBuf.Reset()
		code := run(append(args, "--data-dir", dir), &out, &errBuf)
		return code, out.String(), errBuf.String()
	}
	if code, _, _ := runCmd("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼"); code != 0 {
		t.Fatal("register 失败")
	}
	if code, _, _ := runCmd("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1"); code != 0 {
		t.Fatal("report 失败")
	}
	// 参数错误：缺少维修结果、非法决定、非法序号。
	if code, _, _ := runCmd("submit", "--ticket-id", "T0001"); code != 2 {
		t.Fatalf("缺少维修结果应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("accept", "--ticket-id", "T0001", "--seq", "1", "--decision", "待定", "--comment", "x"); code != 2 {
		t.Fatalf("非法决定应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("accept", "--ticket-id", "T0001", "--seq", "0", "--decision", "通过", "--comment", "x"); code != 2 {
		t.Fatalf("非法序号应退出 2，得到 %d", code)
	}
	// 业务失败：未知工单提交、未提交工单验收。
	if code, _, _ := runCmd("submit", "--ticket-id", "T9999", "--repair-result", "x"); code != 1 {
		t.Fatalf("未知工单提交应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("accept", "--ticket-id", "T0001", "--seq", "1", "--decision", "通过", "--comment", "x"); code != 1 {
		t.Fatalf("无待验收提交时验收应退出 1，得到 %d", code)
	}
	// 提交成功：输出提交序号与待验收状态。
	code, stdout, _ := runCmd("submit", "--ticket-id", "T0001", "--repair-result", "已更换搓纸轮")
	if code != 0 || !strings.Contains(stdout, "提交序号: ") || !strings.Contains(stdout, "工单状态: 待验收") {
		t.Fatalf("提交输出异常（%d）: %s", code, stdout)
	}
	// ticket 显示当前待验收提交；detail 仍列出待验收工单。
	code, stdout, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(stdout, "待验收提交序号: ") || !strings.Contains(stdout, "待验收提交结果: 已更换搓纸轮") {
		t.Fatalf("ticket 输出异常（%d）: %s", code, stdout)
	}
	code, stdout, _ = runCmd("detail", "--asset-id", "EQ-1")
	if code != 0 || !strings.Contains(stdout, "未关闭工单: T0001") {
		t.Fatalf("detail 输出异常（%d）: %s", code, stdout)
	}
	// 待验收时直接关闭失败（退出 1）。
	if code, _, _ := runCmd("close", "--ticket-id", "T0001", "--repair-result", "绕过验收"); code != 1 {
		t.Fatalf("待验收直接关闭应退出 1，得到 %d", code)
	}
	// 验收通过：显示工单编号及新状态，资产恢复可用。
	code, stdout, _ = runCmd("accept", "--ticket-id", "T0001", "--seq", "2", "--decision", "通过", "--comment", "复核通过")
	if code != 0 || !strings.Contains(stdout, "T0001") || !strings.Contains(stdout, "工单状态: 已关闭") ||
		!strings.Contains(stdout, "当前状态: 可用") {
		t.Fatalf("验收输出异常（%d）: %s", code, stdout)
	}
	// history 展示提交、验收与关闭的内容、关联与时间。
	code, stdout, _ = runCmd("history", "--asset-id", "EQ-1")
	if code != 0 || !strings.Contains(stdout, "提交 工单 T0001: 已更换搓纸轮") ||
		!strings.Contains(stdout, "验收 工单 T0001: 通过，目标提交序号 2（复核通过）") {
		t.Fatalf("history 输出异常（%d）: %s", code, stdout)
	}
	// ticket 对非待验收工单明确提示无待验收提交。
	code, stdout, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(stdout, "待验收提交: 无") {
		t.Fatalf("ticket 输出异常（%d）: %s", code, stdout)
	}
}
