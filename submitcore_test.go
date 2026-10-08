package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 返修重提：退回后再次提交产生全新身份；旧身份在重提后按“已处理”拒绝，
// 新身份验收通过后按新提交结果关闭。与既有端到端用例相互独立，只核对规则
// 核心推导出的身份与状态。
func TestRegressChainResubmitIdentity(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")

	_, seq1, err := s.submitRepair(tk.ID, "第一次结果")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.acceptSubmission(tk.ID, seq1, decisionReject, "返修"); err != nil {
		t.Fatal(err)
	}
	// 退回后：工单未关闭、无当前待验收提交，但已算曾提交（不能直接关闭）。
	if got, _, ok := s.currentSubmission(tk.ID); ok || got != 0 {
		t.Fatalf("退回后不应有待验收提交，得到 %d/%v", got, ok)
	}
	if !s.hasSubmission(tk.ID) {
		t.Fatal("退回后仍应视为曾提交，不能直接关闭")
	}
	_, seq2, err := s.submitRepair(tk.ID, "第二次结果")
	if err != nil {
		t.Fatal(err)
	}
	if seq2 == seq1 {
		t.Fatalf("再次提交必须产生新身份，仍为 %d", seq2)
	}
	// 旧身份在重提后属于“已处理”，新身份才是当前待验收提交。
	cur, result, ok := s.currentSubmission(tk.ID)
	if !ok || cur != seq2 || result != "第二次结果" {
		t.Fatalf("当前待验收提交应为 (%d, 第二次结果)，得到 (%d, %q, %v)", seq2, cur, result, ok)
	}
	if _, _, err := s.acceptSubmission(tk.ID, seq1, decisionApprove, "用旧身份"); !strings.Contains(
		err.Error(), "已处理") {
		t.Fatalf("旧身份验收应按已处理拒绝，得到 %v", err)
	}
	tk, _, err = s.acceptSubmission(tk.ID, seq2, decisionApprove, "通过")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != ticketClosed || tk.Result != "第二次结果" {
		t.Fatalf("应按第二次提交结果关闭，得到 %+v", tk)
	}
}

// 旧身份拒绝的各类别：未知序号、非提交序号、跨单提交分别给出对应错误，
// 且拒绝不产生履历、不改变当前待验收提交（判定纯度）。
func TestRegressChainTargetCategories(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	_, _ = s.registerAsset("EQ-2", "空调", "二楼")
	tk1, _, _ := s.report("EQ-1", "卡纸", "req-1")
	tk2, _, _ := s.report("EQ-2", "不制冷", "req-2")
	_, seq1, _ := s.submitRepair(tk1.ID, "结果一")
	_, otherSeq, _ := s.submitRepair(tk2.ID, "他单结果")

	cases := []struct {
		name string
		seq  int
		want string
	}{
		{"未知序号", 999999, "未知提交序号"},
		{"非提交序号", 1, "不是维修提交序号"},
		{"跨单提交序号", otherSeq, "属于工单"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.data.Events)
			_, _, err := s.acceptSubmission(tk1.ID, tc.seq, decisionApprove, "意见")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应拒绝并说明 %q，得到 %v", tc.want, err)
			}
			if len(s.data.Events) != before {
				t.Fatal("被拒绝的验收不得追加履历")
			}
		})
	}
	cur, _, ok := s.currentSubmission(tk1.ID)
	if !ok || cur != seq1 {
		t.Fatalf("拒绝后当前待验收提交应保持 %d，得到 %d/%v", seq1, cur, ok)
	}
}

// 验收与关闭之间的截止：验收通过与关闭是同次保存的两条履历，截止纳入验收、
// 尚未纳入关闭时显示“验收通过待关闭”：工单仍未终结、资产仍维修中、当前待
// 验收提交为无；纳入关闭后才释放资产并采用最终状态。退回后的回看不得把已
// 处理提交列为待验收。
func TestRegressReplayApprovedAwaitingClose(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")                        // seq 1
	_, seqSub, _ := s.submitRepair(tk.ID, "已更换搓纸轮")                    // seq 2
	_, _, _ = s.acceptSubmission(tk.ID, seqSub, decisionApprove, "通过") // seq 3 验收、seq 4 关闭
	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	// 截止落在验收（3）与关闭（4）之间。
	snap := s.replayAsset("EQ-1", 3)
	if snap.openTicket == nil {
		t.Fatal("验收通过待关闭时工单仍未终结，应仍列为未关闭工单")
	}
	if snap.openTicket.status != derivedApproved {
		t.Fatalf("工单状态应为 %q，得到 %q", derivedApproved, snap.openTicket.status)
	}
	if snap.status != statusRepairing {
		t.Fatalf("资产应仍为维修中，得到 %q", snap.status)
	}
	if snap.openTicket.submitSeq != 0 || snap.openTicket.submitResult != "" {
		t.Fatalf("待关闭中间态当前待验收提交应为无，得到 %+v", snap.openTicket)
	}
	// 纳入关闭履历后才终结并释放资产。
	snap = s.replayAsset("EQ-1", 4)
	if snap.openTicket != nil || snap.status != statusAvailable {
		t.Fatalf("纳入关闭后应无工单且资产可用，得到 %+v/%q", snap.openTicket, snap.status)
	}

	// CLI 回看输出：显示中间态、资产维修中，且不输出待验收提交序号。
	var out, errBuf bytes.Buffer
	if code := run([]string{"replay", "--data-dir", s.dir, "--asset-id", "EQ-1", "--seq", "3"},
		&out, &errBuf); code != 0 {
		t.Fatalf("replay 退出 %d: %s", code, errBuf.String())
	}
	o := out.String()
	if !strings.Contains(o, "工单状态: 验收通过待关闭") || !strings.Contains(o, "当时状态: 维修中") {
		t.Fatalf("回看输出缺少待关闭中间态或资产状态:\n%s", o)
	}
	if strings.Contains(o, "待验收提交序号") {
		t.Fatalf("待关闭中间态不得列出待验收提交:\n%s", o)
	}
}

// 取消后的回看不得把已处理（待验收）提交再列为待验收；取消终结工单、释放
// 资产，历史保留且不填写维修结果。
func TestRegressReplayAfterCancel(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")     // 1
	_, seqSub, _ := s.submitRepair(tk.ID, "已更换搓纸轮") // 2
	tk, _, _ = mustCancel(t, s, tk.ID, "误报，设备实际正常") // 3
	if tk.Result != "" || tk.Status != ticketCancelled {
		t.Fatalf("取消后不应填写维修结果: %+v", tk)
	}
	if !s.hasSubmission(tk.ID) {
		t.Fatal("取消应保留提交历史")
	}
	snap := s.replayAsset("EQ-1", 3)
	if snap.openTicket != nil || snap.status != statusAvailable {
		t.Fatalf("取消回看应无工单且资产可用，得到 %+v/%q", snap.openTicket, snap.status)
	}
	// 截止 2（提交刚发生）仍是正常待验收，证明取消不影响更早截止的回看。
	snap = s.replayAsset("EQ-1", 2)
	if snap.openTicket == nil || snap.openTicket.status != ticketPending ||
		snap.openTicket.submitSeq != seqSub {
		t.Fatalf("截止 2 应为待验收提交 %d，得到 %+v", seqSub, snap.openTicket)
	}
}

func mustCancel(t *testing.T, s *store, ticketID, reason string) (*Ticket, *Asset, error) {
	t.Helper()
	tk, a, err := s.cancelTicket(ticketID, reason)
	if err != nil {
		t.Fatal(err)
	}
	return tk, a, err
}

// 导入接续：源库待验收工单导入后用映射后的新序号退回、重提并验收通过；
// 源序号在目标库被拒绝，导入后可继续验收且回看使用新身份；重载后保持。
func TestRegressImportChainContinues(t *testing.T) {
	src := newTestStore(t)
	_, _ = src.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := src.report("EQ-1", "卡纸", "req-1")
	_, oldSeq, _ := src.submitRepair(tk.ID, "第一次结果")
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
	if len(outcome.submissions) != 1 || outcome.submissions[0].OldSeq != oldSeq {
		t.Fatalf("提交映射不对: %+v", outcome.submissions)
	}
	newSeq := outcome.submissions[0].NewSeq
	newTicket := outcome.tickets[0].NewID

	s2, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	// 源序号在目标库已不是该提交（被报修履历占用），必须拒绝，且不得误伤
	// 当前待验收提交。
	if _, _, err := s2.acceptSubmission(newTicket, oldSeq, decisionApprove, "x"); err == nil ||
		!strings.Contains(err.Error(), "提交序号") {
		t.Fatalf("源序号不应能验收目标提交，得到 %v", err)
	}
	if cur, _, ok := s2.currentSubmission(newTicket); !ok || cur != newSeq {
		t.Fatalf("拒绝源序号后当前提交应仍为 %d，得到 %d/%v", newSeq, cur, ok)
	}
	// 退回映射后的当前提交，再重提产生又一个新身份。
	if _, _, err := s2.acceptSubmission(newTicket, newSeq, decisionReject, "返修"); err != nil {
		t.Fatalf("映射序号退回应成功: %v", err)
	}
	_, resubSeq, err := s2.submitRepair(newTicket, "第二次结果")
	if err != nil {
		t.Fatal(err)
	}
	if resubSeq == newSeq {
		t.Fatal("重提必须产生新序号")
	}
	nt, asset, err := s2.acceptSubmission(newTicket, resubSeq, decisionApprove, "通过")
	if err != nil {
		t.Fatal(err)
	}
	if nt.Status != ticketClosed || nt.Result != "第二次结果" || asset.Status != statusAvailable {
		t.Fatalf("导入后验收结果异常: %+v / %q", nt, asset.Status)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	// 回看在新身份截止处显示待验收，旧序号截止处无此提交。
	snap := s2.replayAsset("EQ-1", resubSeq)
	if snap.openTicket == nil || snap.openTicket.status != ticketPending ||
		snap.openTicket.submitSeq != resubSeq {
		t.Fatalf("导入后回看新身份失败: %+v", snap.openTicket)
	}
	// 重载：履历中的目标引用随重编号替换，工单关闭保持。
	s3, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findTicket(newTicket); got.Status != ticketClosed || got.Result != "第二次结果" {
		t.Fatalf("重载后工单异常: %+v", got)
	}
	for _, e := range s3.data.Events {
		if e.Kind == eventAccept && e.TargetSeq == oldSeq {
			t.Fatalf("不应再存在指向源序号 %d 的验收引用", oldSeq)
		}
	}
}

// 校验纯度：validateData 与查询/判定不得修改传入记录、补写字段或重排数组；
// 查询不写文件、不初始化目录。
func TestRegressChainValidationPurity(t *testing.T) {
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		// 合法的待验收台账：提交序号刻意大于 4（保留序号间隔）。
		m["tickets"].([]any)[0].(map[string]any)["status"] = "待验收"
		appendTo(m, "events", map[string]any{
			"seq": 7, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "提交", "content": "已更换搓纸轮", "time": "2026-10-01T10:30:00Z",
		})
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 把履历数组故意打乱（合法台账允许乱序），快照全部输入记录。
	shuffled := append([]Event(nil), s.data.Events...)
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	s.data.Events = shuffled
	ticketsBefore := make([]Ticket, len(s.data.Tickets))
	for i, tk := range s.data.Tickets {
		ticketsBefore[i] = *tk
	}
	eventsBefore := append([]Event(nil), s.data.Events...)

	// 校验与各类只读推导都不得修改输入。
	if err := validateData(s.data); err != nil {
		t.Fatalf("合法乱序台账应通过校验: %v", err)
	}
	if seq, result, ok := s.currentSubmission("T0001"); !ok || seq != 7 || result != "已更换搓纸轮" {
		t.Fatalf("当前待验收提交推导错误: %d %q %v", seq, result, ok)
	}
	if !s.hasSubmission("T0001") {
		t.Fatal("hasSubmission 应为真")
	}
	_ = s.replayAsset("EQ-1", 7)
	_ = s.eventsOf("EQ-1")
	if !reflect.DeepEqual(s.data.Events, eventsBefore) {
		t.Fatal("校验/查询不得重排或修改履历数组")
	}
	ticketsAfter := make([]Ticket, len(s.data.Tickets))
	for i, tk := range s.data.Tickets {
		ticketsAfter[i] = *tk
	}
	if !reflect.DeepEqual(ticketsAfter, ticketsBefore) {
		t.Fatal("校验/查询不得修改工单记录（含补写报修地点等字段）")
	}
	for _, tk := range s.data.Tickets {
		if tk.ReportLocation != "" {
			t.Fatal("旧库缺少报修地点时查询不得补写")
		}
	}
	// 文件字节保持不变；eventsOf/replay 等只读查询不写文件。
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("只读校验与查询不得改动台账文件字节")
	}

	// 查询不初始化目录：对不存在的目录回看，目录仍不存在。
	missing := filepath.Join(t.TempDir(), "never-created")
	var out, errBuf bytes.Buffer
	if code := run([]string{"replay", "--data-dir", missing, "--asset-id", "EQ-1", "--seq", "0"},
		&out, &errBuf); code != 1 {
		t.Fatalf("未知目录回看应退出 1，得到 %d", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("只读查询不得初始化目录，Stat err=%v", err)
	}
}

// 保存失败后先重载再按原业务输入重试：提交与验收在写入失败时保留原文件
// 字节、不消耗履历序号；重载后状态回到保存点，用相同输入可成功重试。
func TestRegressReloadAfterSaveFailureThenRetry(t *testing.T) {
	dir := t.TempDir()
	setup := func() *store {
		s, err := openStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := setup()
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, dataFileName)
	raw, _ := os.ReadFile(path)

	// 提交保存失败。
	s = setup()
	if _, _, err := s.submitRepair("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	submitSaveErr := s.save()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if submitSaveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
		t.Fatal("提交保存失败应保持原文件字节")
	}

	// 先重载：磁盘上仍是未关闭工单、无提交履历，序号未被消耗。
	s = setup()
	if tk := s.findTicket("T0001"); tk.Status != ticketOpen {
		t.Fatalf("重载后应为未关闭，得到 %q", tk.Status)
	}
	if len(s.data.Events) != 1 {
		t.Fatalf("失败的提交不得留下履历，得到 %d 条", len(s.data.Events))
	}
	// 再按原业务输入重试提交：序号仍为 2。
	tk, seq, err := s.submitRepair("T0001", "已更换搓纸轮")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 {
		t.Fatalf("失败保存不应消耗序号，重试提交序号应为 2，得到 %d", seq)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if tk.Status != ticketPending {
		t.Fatalf("重试提交后应为待验收，得到 %q", tk.Status)
	}

	// 验收同样：保存失败后重载，用相同提交序号与意见重试成功。
	raw2, _ := os.ReadFile(path)
	s = setup()
	if _, _, err := s.acceptSubmission("T0001", seq, decisionApprove, "复核通过"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	acceptSaveErr := s.save()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if acceptSaveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过验收保存失败分支")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, raw2) {
		t.Fatal("验收保存失败应保持原文件字节")
	}
	s = setup()
	if tk := s.findTicket("T0001"); tk.Status != ticketPending {
		t.Fatalf("重载后应回到待验收，得到 %q", tk.Status)
	}
	closed, asset, err := s.acceptSubmission("T0001", seq, decisionApprove, "复核通过")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if closed.Status != ticketClosed || closed.Result != "已更换搓纸轮" || asset.Status != statusAvailable {
		t.Fatalf("重试验收结果异常: %+v / %q", closed, asset.Status)
	}
	// 最终重载：关闭保持，履历恰为 报修/提交/验收/关闭 四条，序号连续，
	// 没有部分履历或重复消耗的序号。
	s = setup()
	if tk := s.findTicket("T0001"); tk.Status != ticketClosed || tk.Result != "已更换搓纸轮" {
		t.Fatalf("最终重载工单异常: %+v", tk)
	}
	seqs := make([]int, 0)
	for _, e := range s.data.Events {
		seqs = append(seqs, e.Seq)
	}
	sort.Ints(seqs)
	if !reflect.DeepEqual(seqs, []int{1, 2, 3, 4}) {
		t.Fatalf("失败重试不得留下部分履历或重复消耗序号，得到 %v", seqs)
	}
}
