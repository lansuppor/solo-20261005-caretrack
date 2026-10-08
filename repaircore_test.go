package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 返修重提的核心状态推导：提交→待验收，退回→未关闭且当前提交清空，再次提交
// 产生新身份；每个关键节点的状态、当前待验收提交与“曾提交”标记都以共用核心
// 为准，而不是各路径各自维护的判定。
func TestRepairCoreRejectResubmitState(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")

	core, err := s.repairCore()
	if err != nil {
		t.Fatal(err)
	}
	if got := core.status(tk.ID); got != ticketOpen {
		t.Fatalf("报修后核心状态 = %q", got)
	}
	if core.submittedEver(tk.ID) {
		t.Fatal("报修后不应视为曾提交")
	}

	// 第一次提交：待验收，提交身份为履历序号 2。
	tk, seq1, err := s.submitRepair(tk.ID, "第一次结果")
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 2 {
		t.Fatalf("第一次提交序号 = %d，想得到 2", seq1)
	}
	if got, r, ok := s.currentSubmission(tk.ID); !ok || got != 2 || r != "第一次结果" {
		t.Fatalf("待验收当前提交 = (%d,%q,%v)", got, r, ok)
	}
	// 退回：恢复未关闭，当前待验收提交清空，但曾提交标记保留。
	if _, _, err := s.acceptSubmission(tk.ID, seq1, decisionReject, "需返修"); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := s.currentSubmission(tk.ID); ok || got != 0 {
		t.Fatalf("退回后不应有待验收提交，得到 %d/%v", got, ok)
	}
	if !s.hasSubmission(tk.ID) {
		t.Fatal("退回后仍应视为曾提交，不能直接关闭")
	}
	// 再次提交：新身份严格大于旧身份，当前提交切换为新序号。
	tk, seq2, err := s.submitRepair(tk.ID, "第二次结果")
	if err != nil {
		t.Fatal(err)
	}
	if seq2 <= seq1 {
		t.Fatalf("再次提交序号 %d 应大于 %d", seq2, seq1)
	}
	if got, r, ok := s.currentSubmission(tk.ID); !ok || got != seq2 || r != "第二次结果" {
		t.Fatalf("再次待验收当前提交 = (%d,%q,%v)", got, r, ok)
	}
	if tk.Status != ticketPending {
		t.Fatalf("工单状态 = %q", tk.Status)
	}
}

// 直接以履历驱动共用核心，覆盖验收目标的四类拒绝（未知、非提交、跨单、已处理）
// 以及提交/关闭/取消在错误状态下的拒绝；一次判定失败不得改变派生状态，随后
// 正确的履历仍应被采纳。
func TestRepairCoreTargetClassificationAndGuards(t *testing.T) {
	// 构造两张工单：T1 提交(2)→退回(3)→提交(4)；T2 报修(5)→提交(6)。
	ev := func(seq int, ticket, kind, content, decision string, target int) Event {
		return Event{Seq: seq, AssetID: "EQ-1", TicketID: ticket, Kind: kind,
			Content: content, Decision: decision, TargetSeq: target,
			Time: time.Unix(int64(seq), 0)}
	}
	history := []Event{
		ev(1, "T1", eventReport, "卡纸", "", 0),
		ev(2, "T1", eventSubmit, "第一次", "", 0),
		ev(3, "T1", eventAccept, "退回", decisionReject, 2),
		ev(4, "T1", eventSubmit, "第二次", "", 0),
		ev(5, "T2", eventReport, "不制冷", "", 0),
		ev(6, "T2", eventSubmit, "他单结果", "", 0),
	}
	replay := func(upto int) *repairCore {
		c := newRepairCore()
		for _, e := range history[:upto] {
			if err := c.apply(e); err != nil {
				t.Fatalf("合法前置履历重放失败（%d）: %v", e.Seq, err)
			}
		}
		return c
	}
	wantCode := func(e Event, want repairRuleCode) {
		t.Helper()
		c := replay(len(history))
		err := c.apply(e)
		var re *repairRuleError
		if !errors.As(err, &re) {
			t.Fatalf("履历 %+v 应被核心拒绝，得到 %v", e, err)
		}
		if re.code != want {
			t.Fatalf("履历 %+v 冲突类别 = %d，想得到 %d（%v）", e, re.code, want, err)
		}
	}
	// T1 当前待验收提交为序号 4。
	c := replay(len(history))
	if seq, _, ok := c.pending("T1"); !ok || seq != 4 {
		t.Fatalf("T1 当前待验收提交应为 4，得到 %d/%v", seq, ok)
	}

	// 未知序号、非提交序号（报修 1）、跨单提交（T2 的 6）、已处理旧提交（2）。
	wantCode(ev(7, "T1", eventAccept, "意见", decisionApprove, 99), ruleAcceptTargetUnknown)
	wantCode(ev(7, "T1", eventAccept, "意见", decisionApprove, 1), ruleAcceptTargetNotSubmit)
	wantCode(ev(7, "T1", eventAccept, "意见", decisionApprove, 6), ruleAcceptTargetCrossTicket)
	wantCode(ev(7, "T1", eventAccept, "意见", decisionApprove, 2), ruleAcceptTargetProcessed)
	// 非待验收状态验收：T2 待验收可验收，未提交工单不存在；以已终结场景验证。
	c2 := replay(2) // T1 处于待验收
	if err := c2.apply(ev(7, "T1", eventSubmit, "重复提交", "", 0)); !ruleCodeIs(err, ruleSubmitNotOpen) {
		t.Fatalf("待验收重复提交应拒绝，得到 %v", err)
	}
	// 判定失败不改变状态：序号 4 的正确验收仍可进行。
	c = replay(4) // T1 退回后再次待验收（序号 4）
	if err := c.apply(ev(5, "T1", eventAccept, "错误目标", decisionApprove, 2)); err == nil {
		t.Fatal("旧目标验收应失败")
	}
	if seq, _, ok := c.pending("T1"); !ok || seq != 4 {
		t.Fatalf("失败判定后当前提交应仍是 4，得到 %d/%v", seq, ok)
	}
	if err := c.apply(ev(5, "T1", eventAccept, "通过", decisionApprove, 4)); err != nil {
		t.Fatalf("正确目标验收应成功: %v", err)
	}
	if got := c.status("T1"); got != derivedApproved {
		t.Fatalf("验收通过后应为待关闭中间态，得到 %q", got)
	}
	if _, _, ok := c.pending("T1"); ok {
		t.Fatal("待关闭中间态当前待验收提交应为无")
	}
	// 中间态：内容不匹配的关闭拒绝；取消也拒绝；内容匹配的关闭才终结。
	if err := c.apply(ev(6, "T1", eventClose, "错误结果", "", 0)); !ruleCodeIs(err, ruleCloseApprovedMismatch) {
		t.Fatalf("内容不匹配关闭应拒绝，得到 %v", err)
	}
	if err := c.apply(ev(6, "T1", eventCancel, "取消", "", 0)); !ruleCodeIs(err, ruleCancelApprovedPending) {
		t.Fatalf("待关闭中间态取消应拒绝，得到 %v", err)
	}
	if err := c.apply(ev(6, "T1", eventClose, "第二次", "", 0)); err != nil {
		t.Fatalf("内容匹配的关闭应成功: %v", err)
	}
	if got := c.status("T1"); got != ticketClosed {
		t.Fatalf("关闭后状态 = %q", got)
	}
	// 首次提交后直接关闭拒绝（退回后的未关闭态）。
	c3 := replay(3) // T1 已退回、未关闭、曾提交
	if err := c3.apply(ev(4, "T1", eventClose, "绕过验收", "", 0)); !ruleCodeIs(err, ruleCloseAfterSubmit) {
		t.Fatalf("首次提交后直接关闭应拒绝，得到 %v", err)
	}
	// 待验收直接关闭拒绝。
	c4 := replay(2)
	if err := c4.apply(ev(3, "T1", eventClose, "直接关闭", "", 0)); !ruleCodeIs(err, ruleClosePending) {
		t.Fatalf("待验收直接关闭应拒绝，得到 %v", err)
	}
}

// ruleCodeIs 报告 err 是否为指定类别的维修链规则冲突。
func ruleCodeIs(err error, code repairRuleCode) bool {
	var re *repairRuleError
	return errors.As(err, &re) && re.code == code
}

// 验收与关闭之间的截止：replay 在已纳入验收通过、未纳入关闭时显示“验收通过
// 待关闭”——工单仍未终结、资产仍维修中、当前待验收提交为无；纳入关闭后才
// 释放资产。命令行输出同样体现该中间态。
func TestReplayApprovedPendingCutoff(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")                       // 1
	_, seq, _ := s.submitRepair(tk.ID, "已更换搓纸轮")                      // 2
	_, _, _ = s.acceptSubmission(tk.ID, seq, decisionApprove, "复核通过") // 3 验收、4 关闭

	// 截止 3：中间态。
	snap := s.replayAsset("EQ-1", 3)
	if snap.openTicket == nil {
		t.Fatal("截止 3 工单应仍未终结")
	}
	if snap.openTicket.status != derivedApproved {
		t.Fatalf("工单状态 = %q，想得到 %q", snap.openTicket.status, derivedApproved)
	}
	if snap.openTicket.submitSeq != 0 || snap.openTicket.submitResult != "" {
		t.Fatalf("中间态当前待验收提交应为无: %+v", snap.openTicket)
	}
	if snap.status != statusRepairing {
		t.Fatalf("中间态资产应仍维修中，得到 %q", snap.status)
	}
	var buf bytes.Buffer
	printReplay(&buf, snap)
	out := buf.String()
	if !strings.Contains(out, "工单状态: 验收通过待关闭") ||
		!strings.Contains(out, "待验收提交: 无") ||
		!strings.Contains(out, "当时状态: 维修中") {
		t.Fatalf("中间态回看输出不符:\n%s", out)
	}
	// 截止 4：关闭纳入后才终结并释放资产。
	snap = s.replayAsset("EQ-1", 4)
	if snap.openTicket != nil || snap.status != statusAvailable {
		t.Fatalf("截止 4 应已关闭可用: %+v / %q", snap.openTicket, snap.status)
	}
	// 待关闭中间态绝不能持久化：台账保存状态只能是四种合法工单状态。
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		tk0 := m["tickets"].([]any)[0].(map[string]any)
		tk0["status"] = "待验收"
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "提交", "content": "已更换搓纸轮", "time": "2026-10-01T10:30:00Z",
		})
		// 有验收通过履历却没有紧随关闭：任何保存状态都无法与推导一致。
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "验收", "content": "复核通过", "decision": "通过",
			"target_seq": 4, "time": "2026-10-01T11:00:00Z",
		})
	})
	if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), "状态矛盾") {
		t.Fatalf("待关闭中间态不得持久化，应状态矛盾拒绝，得到 %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, dataFileName)); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("拒绝加载不应改写原文件")
	}
}

// 取消待验收工单：清除当前待验收提交、不填维修结果、资产恢复可用，但提交与
// 验收历史全部保留；取消后不能把已处理提交再列为待验收，也不能再提交或验收；
// 取消后该资产可开新工单，新工单从未提交可直接关闭。
func TestCancelPendingClearsSubmissionKeepsHistory(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	_, seq1, _ := s.submitRepair(tk.ID, "第一次结果")
	_, _, _ = s.acceptSubmission(tk.ID, seq1, decisionReject, "返修意见")
	_, seq2, _ := s.submitRepair(tk.ID, "第二次结果")

	tk, asset, err := s.cancelTicket(tk.ID, "不再需要维修")
	if err != nil {
		t.Fatalf("取消待验收工单: %v", err)
	}
	if tk.Status != ticketCancelled || tk.Result != "" || tk.CancelReason != "不再需要维修" {
		t.Fatalf("取消后工单异常: %+v", tk)
	}
	if asset.Status != statusAvailable {
		t.Fatalf("取消后资产应可用，得到 %q", asset.Status)
	}
	if got, _, ok := s.currentSubmission(tk.ID); ok || got != 0 {
		t.Fatalf("取消后不应有待验收提交，得到 %d/%v", got, ok)
	}
	// 历史保留：两条提交、一条退回验收、一条取消。
	var submits, accepts, cancels int
	for _, e := range s.eventsOf("EQ-1") {
		switch e.Kind {
		case eventSubmit:
			submits++
		case eventAccept:
			accepts++
		case eventCancel:
			cancels++
		}
	}
	if submits != 2 || accepts != 1 || cancels != 1 {
		t.Fatalf("历史统计 = 提交%d/验收%d/取消%d，想得到 2/1/1", submits, accepts, cancels)
	}
	// 终结后提交、验收、再次取消都拒绝。
	if _, _, err := s.submitRepair(tk.ID, "再提交"); !errors.Is(err, errConflict) {
		t.Fatalf("取消后提交应失败，得到 %v", err)
	}
	if _, _, err := s.acceptSubmission(tk.ID, seq2, decisionApprove, "x"); !errors.Is(err, errConflict) {
		t.Fatalf("取消后验收应失败，得到 %v", err)
	}
	// 资产释放后可开新工单，新工单未提交可直接关闭。
	tk2, _, err := s.report("EQ-1", "新故障", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk2.ID, "直接修复"); err != nil {
		t.Fatalf("新工单应可直接关闭: %v", err)
	}
	// 回看取消时刻：旧单已终结、无待验收提交，已处理提交不被列出。
	snap := s.replayAsset("EQ-1", seq2+1)
	if snap.openTicket != nil {
		t.Fatalf("取消截止处不应有未关闭工单: %+v", snap.openTicket)
	}
}

// 导入接续：源库含“待验收（退回后重提）”与“验收通过关闭”两张工单（分属两项
// 资产）时，导入重编号同步替换验收目标提交序号，合并后台账按共用规则通过校验；
// 导入后可用新序号继续验收，可在导入的验收/关闭截止处回看中间态，请求重放
// 返回映射工单。
func TestImportRepairChainRemapAndReplay(t *testing.T) {
	src := newTestStore(t)
	_, _ = src.registerAsset("EQ-1", "打印机", "一楼")
	_, _ = src.registerAsset("EQ-2", "空调", "二楼")
	// EQ-1 / T0001：提交→退回→再提交（待验收，占用资产）。
	t1, _, _ := src.report("EQ-1", "卡纸", "req-1")
	_, seqA, _ := src.submitRepair(t1.ID, "第一次")
	if _, _, err := src.acceptSubmission(t1.ID, seqA, decisionReject, "返修"); err != nil {
		t.Fatal(err)
	}
	_, seqPending, _ := src.submitRepair(t1.ID, "第二次")
	// EQ-2 / T0002：提交→验收通过→关闭（资产已释放）。
	t2, _, _ := src.report("EQ-2", "异响", "req-2")
	_, seqAppr, _ := src.submitRepair(t2.ID, "已紧固")
	if _, _, err := src.acceptSubmission(t2.ID, seqAppr, decisionApprove, "通过"); err != nil {
		t.Fatal(err)
	}
	if err := src.save(); err != nil {
		t.Fatal(err)
	}

	dst := newTestStore(t)
	_, _ = dst.registerAsset("EQ-9", "饮水机", "九楼")
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}
	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-1", "EQ-2"})
	if err != nil {
		t.Fatalf("importAssets: %v", err)
	}
	// 合并后台账按共用规则校验通过（openStore 即整库校验）。
	s2, err := openStore(dst.dir)
	if err != nil {
		t.Fatalf("合并后台账应一致: %v", err)
	}
	// 提交序号映射：EQ-1 两次提交 + EQ-2 一次提交。
	if len(outcome.submissions) != 3 {
		t.Fatalf("提交映射条数 = %d，想得到 3: %+v", len(outcome.submissions), outcome.submissions)
	}
	newSeqs := map[int]bool{}
	for _, m := range outcome.submissions {
		newSeqs[m.NewSeq] = true
	}
	// 工单按源序号升序映射：T0001 待验收，T0002 已关闭。
	nt1 := s2.findTicket(outcome.tickets[0].NewID)
	nt2 := s2.findTicket(outcome.tickets[1].NewID)
	if nt1 == nil || nt1.AssetID != "EQ-1" || nt1.Status != ticketPending {
		t.Fatalf("导入的待验收工单状态异常: %+v", nt1)
	}
	// 由映射结果反查待验收提交对应的新序号。
	var newPending int
	for _, m := range outcome.submissions {
		if m.OldSeq == seqPending {
			newPending = m.NewSeq
		}
	}
	if newPending == 0 {
		t.Fatalf("待验收提交源序号 %d 缺少映射", seqPending)
	}
	if got, r, ok := s2.currentSubmission(nt1.ID); !ok || got != newPending || r != "第二次" {
		t.Fatalf("导入后当前提交 = (%d,%q,%v)，想得到 (%d, 第二次, true)", got, r, ok, newPending)
	}
	if nt2 == nil || nt2.AssetID != "EQ-2" || nt2.Status != ticketClosed || nt2.Result != "已紧固" {
		t.Fatalf("导入的验收关闭工单异常: %+v", nt2)
	}
	// 验收履历的目标序号已随重编号同步替换：每个验收目标都应是新提交序号。
	for _, e := range s2.data.Events {
		if e.Kind == eventAccept && !newSeqs[e.TargetSeq] {
			t.Fatalf("验收履历目标 %d 不在新提交序号集合中（未正确重编号）", e.TargetSeq)
		}
	}
	// 用映射后的新序号继续验收并关闭待验收工单。
	nt1, asset, err := s2.acceptSubmission(nt1.ID, newPending, decisionApprove, "复核通过")
	if err != nil {
		t.Fatalf("导入后续验收: %v", err)
	}
	if nt1.Status != ticketClosed || nt1.Result != "第二次" || asset.Status != statusAvailable {
		t.Fatalf("续验结果异常: %+v / %q", nt1, asset.Status)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dst.dir); err != nil {
		t.Fatalf("续验保存后整库应一致: %v", err)
	}
	// 请求重放返回映射后的工单当前状态，不开新单。
	old, replayed, err := s2.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replayed || old.ID != nt1.ID || old.Status != ticketClosed {
		t.Fatalf("重放应返回映射工单已关闭: %v replay=%v err=%v", old, replayed, err)
	}
	// 在 EQ-2 的已关闭工单上取验收与关闭履历序号，截止回看验收处应为“验收
	// 通过待关闭”中间态（两履历为同次保存的相邻全库序号）。
	var acceptSeq, closeSeq int
	for _, e := range s2.data.Events {
		if e.TicketID == nt2.ID {
			switch e.Kind {
			case eventAccept:
				acceptSeq = e.Seq
			case eventClose:
				closeSeq = e.Seq
			}
		}
	}
	if acceptSeq == 0 || closeSeq != acceptSeq+1 {
		t.Fatalf("验收 %d 与关闭 %d 应为同次保存的相邻序号", acceptSeq, closeSeq)
	}
	snap := s2.replayAsset("EQ-2", acceptSeq)
	if snap.openTicket == nil || snap.openTicket.id != nt2.ID ||
		snap.openTicket.status != derivedApproved {
		t.Fatalf("导入工单截止验收处应为待关闭中间态: %+v", snap.openTicket)
	}
}

// 校验、预判与查询纯度：validateData、repairCore 重放、currentSubmission、
// hasSubmission 与 replayAsset 都不修改传入记录；查询不写文件、不初始化目录。
func TestRepairChainPurityAndReadOnly(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	_, seq, _ := s.submitRepair(tk.ID, "已更换搓纸轮")

	eventsBefore := append([]Event(nil), s.data.Events...)
	ticketsBefore := make([]Ticket, len(s.data.Tickets))
	for i, x := range s.data.Tickets {
		ticketsBefore[i] = *x
	}
	assetsBefore := make([]Asset, len(s.data.Assets))
	for i, x := range s.data.Assets {
		assetsBefore[i] = *x
	}

	// 校验、核心重放与只读查询反复调用。
	if err := validateData(s.data); err != nil {
		t.Fatalf("合法数据校验失败: %v", err)
	}
	for range 3 {
		if _, err := s.repairCore(); err != nil {
			t.Fatal(err)
		}
		if got, r, ok := s.currentSubmission(tk.ID); !ok || got != seq || r != "已更换搓纸轮" {
			t.Fatalf("当前提交查询异常: %d %q %v", got, r, ok)
		}
		if !s.hasSubmission(tk.ID) {
			t.Fatal("hasSubmission 应为真")
		}
		_ = s.replayAsset("EQ-1", seq)
	}
	if !reflect.DeepEqual(s.data.Events, eventsBefore) {
		t.Fatal("校验/预判/查询不得修改履历")
	}
	for i, x := range s.data.Tickets {
		if !reflect.DeepEqual(*x, ticketsBefore[i]) {
			t.Fatalf("工单记录被修改: %+v != %+v", *x, ticketsBefore[i])
		}
	}
	for i, x := range s.data.Assets {
		if !reflect.DeepEqual(*x, assetsBefore[i]) {
			t.Fatalf("资产记录被修改: %+v != %+v", *x, assetsBefore[i])
		}
	}

	// 查询不写文件、不初始化目录：在尚不存在的目录上只读打开并查询。
	dir := filepath.Join(t.TempDir(), "not-created")
	empty, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.registerAsset("EQ-X", "投影仪", "三楼"); err != nil {
		t.Fatal(err)
	}
	// 不保存即查询：目录与文件都不应被创建。
	_ = empty.replayAsset("EQ-X", 0)
	if _, _, ok := empty.currentSubmission("T0001"); ok {
		t.Fatal("空库不应有待验收提交")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("只读查询不应初始化目录，Stat = %v", err)
	}
}

// 保存失败后先重载再按原业务输入重试：提交、验收（通过）在写入失败时原文件
// 字节保持、不消耗序号；重载后状态回到保存点，用相同的维修结果/决定/意见
// 重试成功，履历不出现半条重复。
func TestRepairChainSaveFailureReloadThenRetry(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, dataFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 提交保存失败：内存改动丢弃，原文件字节保持，序号未消耗。
	_, seq, err := s.submitRepair("T0001", "已更换搓纸轮")
	if err != nil {
		t.Fatal(err)
	}
	makeReadOnly(t, s.dir)
	saveErr := s.save()
	restoreWritable(t, s.dir)
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("提交保存失败应保持原文件字节")
	}

	// 先重载，再按原业务输入重试：得到相同序号（编号未消耗），保存成功。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.now = s.now
	tk := s2.findTicket("T0001")
	if tk.Status != ticketOpen {
		t.Fatalf("重载后应回到未关闭保存点，得到 %q", tk.Status)
	}
	tk, seqRetry, err := s2.submitRepair("T0001", "已更换搓纸轮")
	if err != nil {
		t.Fatalf("重载后按原输入重试提交: %v", err)
	}
	if seqRetry != seq {
		t.Fatalf("失败提交不应消耗序号：重试序号 %d != %d", seqRetry, seq)
	}
	if err := s2.save(); err != nil {
		t.Fatalf("重试保存应成功: %v", err)
	}

	// 验收通过保存失败：同样先重载，再以相同目标序号、决定、意见重试。
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s3.now = s2.now
	rawMid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s3.acceptSubmission("T0001", seq, decisionApprove, "复核通过"); err != nil {
		t.Fatal(err)
	}
	makeReadOnly(t, s3.dir)
	saveErr = s3.save()
	restoreWritable(t, s3.dir)
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过验收保存失败分支")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, rawMid) {
		t.Fatal("验收保存失败应保持重载点文件字节")
	}
	s4, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s4.now = s3.now
	if tk4 := s4.findTicket("T0001"); tk4.Status != ticketPending {
		t.Fatalf("重载后应仍待验收，得到 %q", tk4.Status)
	}
	tk4, asset, err := s4.acceptSubmission("T0001", seq, decisionApprove, "复核通过")
	if err != nil {
		t.Fatalf("重载后按原输入重试验收: %v", err)
	}
	if tk4.Status != ticketClosed || tk4.Result != "已更换搓纸轮" || asset.Status != statusAvailable {
		t.Fatalf("重试验收结果异常: %+v / %q", tk4, asset.Status)
	}
	if err := s4.save(); err != nil {
		t.Fatal(err)
	}
	// 最终履历：报修、提交、验收、关闭各一条，无半条重复。
	s5, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range s5.eventsOf("EQ-1") {
		counts[e.Kind]++
	}
	for _, k := range []string{eventReport, eventSubmit, eventAccept, eventClose} {
		if counts[k] != 1 {
			t.Fatalf("履历 %s 条数 = %d，想得到 1（全部计数 %v）", k, counts[k], counts)
		}
	}
}

// export/restore 保留提交身份、顺序与时间精度：待验收工单与验收通过关闭工单
// 经导出还原后序号、目标引用、小数秒时间全部保持，可在还原库继续验收并回看
// 验收/关闭之间的截止。
func TestExportRestorePreservesSubmissionIdentity(t *testing.T) {
	s := newStoreAt(t, filepath.Join(t.TempDir(), "src"))
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	base := time.Date(2026, 10, 1, 8, 0, 0, 123456789, time.UTC)
	i := 0
	s.now = func() time.Time {
		cur := base.Add(time.Duration(i) * time.Second).Add(time.Duration(i) * time.Nanosecond)
		i++
		return cur
	}
	t1, _, _ := s.report("EQ-1", "卡纸", "req-1")
	_, submitSeq, _ := s.submitRepair(t1.ID, "已更换搓纸轮")
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	// 时间精度快照。
	timesBefore := map[int]time.Time{}
	for _, e := range s.data.Events {
		timesBefore[e.Seq] = e.Time
	}

	pkg := filepath.Join(t.TempDir(), "eq1.zip")
	if _, err := exportPackage(s.dir, pkg, []string{"EQ-1"}); err != nil {
		t.Fatalf("exportPackage: %v", err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := restorePackage(pkg, target); err != nil {
		t.Fatalf("restorePackage: %v", err)
	}
	s2, err := openStore(target)
	if err != nil {
		t.Fatal(err)
	}
	// 身份与待验收状态保持。
	nt := s2.findTicket("T0001")
	if nt == nil || nt.Status != ticketPending {
		t.Fatalf("还原后工单应为待验收: %+v", nt)
	}
	if got, r, ok := s2.currentSubmission("T0001"); !ok || got != submitSeq || r != "已更换搓纸轮" {
		t.Fatalf("还原后当前提交 = (%d,%q,%v)，想得到 (%d, ...)", got, r, ok, submitSeq)
	}
	// 顺序与时间精度（含小数秒）逐条保持。
	var restored []Event
	for _, e := range s2.data.Events {
		restored = append(restored, e)
	}
	if len(restored) != len(timesBefore) {
		t.Fatalf("还原后履历条数 %d != %d", len(restored), len(timesBefore))
	}
	for seq, want := range timesBefore {
		var got time.Time
		found := false
		for _, e := range restored {
			if e.Seq == seq {
				got, found = e.Time, true
			}
		}
		if !found {
			t.Fatalf("还原后缺少履历序号 %d", seq)
		}
		if !got.Equal(want) || got.Format(time.RFC3339Nano) != want.Format(time.RFC3339Nano) {
			t.Fatalf("序号 %d 时间精度损失: %s != %s", seq, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}
	// 还原库可继续验收并关闭，随后整库一致。
	nt, asset, err := s2.acceptSubmission("T0001", submitSeq, decisionApprove, "复核通过")
	if err != nil {
		t.Fatalf("还原后续验收: %v", err)
	}
	if nt.Status != ticketClosed || asset.Status != statusAvailable {
		t.Fatalf("还原后续验收结果异常: %+v / %q", nt, asset.Status)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(target); err != nil {
		t.Fatalf("还原库续验后应一致: %v", err)
	}
}

func makeReadOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
}

func restoreWritable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
