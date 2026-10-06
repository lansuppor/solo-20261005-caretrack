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

// newPartStore 建立含一项资产与一张未关闭工单 T0001 的测试台账。
func newPartStore(t *testing.T) (*store, string) {
	t.Helper()
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil { // T0001
		t.Fatal(err)
	}
	mustSave(t, s)
	return s, dir
}

// 分笔退回：多次部分退回累计更新，净量 = 原数量 - 累计退回；超额、未知编号、
// 冲减另一笔均拒绝；重载后编号、净量与关联保持。
func TestPartWithdrawAndPartialReturns(t *testing.T) {
	s, dir := newPartStore(t)

	// 同备件两次领用各自独立，编号不复用。
	p1, err := s.withdrawPart("T0001", "FILTER-01", 5, "更换滤芯")
	if err != nil || p1.ID != "P0001" {
		t.Fatalf("首次领用: %v %+v", err, p1)
	}
	p2, err := s.withdrawPart("T0001", "FILTER-01", 3, "备件")
	if err != nil || p2.ID != "P0002" {
		t.Fatalf("第二次领用: %v %+v", err, p2)
	}
	p3, err := s.withdrawPart("T0001", "ROLLER-02", 2, "更换搓纸轮")
	if err != nil || p3.ID != "P0003" {
		t.Fatalf("第三次领用: %v %+v", err, p3)
	}
	mustSave(t, s)

	// 分笔退回：5 退 2 再退 2，累计 4，净量 1。
	if _, _, err := s.returnPart("P0001", 2, "多余退回"); err != nil {
		t.Fatalf("第一次退回: %v", err)
	}
	got, _, err := s.returnPart("P0001", 2, "再次退回")
	if err != nil {
		t.Fatalf("第二次退回: %v", err)
	}
	if got.Returned != 4 || got.Quantity-got.Returned != 1 {
		t.Fatalf("累计退回应为 4、净量 1，得到 %+v", got)
	}
	// 超额退回拒绝：再退 2 将超过原数量 5。
	if _, _, err := s.returnPart("P0001", 2, "超额"); !errors.Is(err, errConflict) {
		t.Fatalf("超额退回应拒绝，得到 %v", err)
	}
	// 未知领用编号拒绝。
	if _, _, err := s.returnPart("P9999", 1, "未知"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知领用编号应拒绝，得到 %v", err)
	}
	// 不能冲减另一笔：P0002 的退回不影响 P0001 的累计。
	if _, _, err := s.returnPart("P0002", 1, "部分退回"); err != nil {
		t.Fatal(err)
	}
	if p1.Returned != 4 {
		t.Fatalf("另一笔退回不应影响 P0001，得到累计 %d", p1.Returned)
	}
	mustSave(t, s)

	// 重载后编号、净量与关联保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if s2.data.NextPartSeq != 4 {
		t.Fatalf("下一领用序号 = %d, 想得到 4", s2.data.NextPartSeq)
	}
	r1 := s2.findPart("P0001")
	if r1 == nil || r1.TicketID != "T0001" || r1.AssetID != "EQ-1" ||
		r1.Quantity != 5 || r1.Returned != 4 {
		t.Fatalf("重载后 P0001 应保持原数量 5、累计退回 4，得到 %+v", r1)
	}
	// 新领用编号从 4 延续，不复用。
	p4, err := s2.withdrawPart("T0001", "FILTER-01", 1, "补充")
	if err != nil || p4.ID != "P0004" {
		t.Fatalf("重载后领用编号应延续为 P0004: %v %+v", err, p4)
	}
	mustSave(t, s2)

	// 汇总：FILTER-01 净量 (5-4)+(3-1)+1 = 4，ROLLER-02 净量 2。
	rows := s2.partNetSummary("T0001")
	if len(rows) != 2 || rows[0].PartID != "FILTER-01" || rows[0].Net.String() != "4" ||
		rows[1].PartID != "ROLLER-02" || rows[1].Net.String() != "2" {
		t.Fatalf("净量汇总应按备件编号字典序，得到 %+v", rows)
	}
}

// 终结：关闭或取消后保留记录与净量，不清零；终结后拒绝领用与退回；
// 旧单操作不影响新工单。
func TestPartTerminalStateKeepsRecords(t *testing.T) {
	s, dir := newPartStore(t)
	if _, err := s.withdrawPart("T0001", "FILTER-01", 4, "更换滤芯"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0001", 1, "多余"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	// 终结后拒绝领用与退回。
	if _, err := s.withdrawPart("T0001", "FILTER-01", 1, "再领"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单领用应拒绝，得到 %v", err)
	}
	if _, _, err := s.returnPart("P0001", 1, "再退"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单退回应拒绝，得到 %v", err)
	}
	// 记录与净量保留，不自动清零。
	p := s.findPart("P0001")
	if p == nil || p.Quantity != 4 || p.Returned != 1 {
		t.Fatalf("关闭后记录与净量应保留，得到 %+v", p)
	}

	// 新工单可正常领用，旧单记录不影响新单。
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil { // T0002
		t.Fatal(err)
	}
	p2, err := s.withdrawPart("T0002", "FILTER-01", 2, "新单领用")
	if err != nil || p2.ID != "P0002" {
		t.Fatalf("新工单领用: %v %+v", err, p2)
	}
	if got := s.partNetSummary("T0002"); len(got) != 1 || got[0].PartID != "FILTER-01" || got[0].Net.String() != "2" {
		t.Fatalf("新单汇总应独立于旧单，得到 %+v", got)
	}
	mustSave(t, s)

	// 取消同样是终态：拒绝退回，净量保留。
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0002", 1, "退回"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单退回应拒绝，得到 %v", err)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if p := s2.findPart("P0002"); p == nil || p.Returned != 0 || p.Quantity != 2 {
		t.Fatalf("取消后净量应保留，得到 %+v", p)
	}
}

// 导入接续：备件记录与履历随资产复制，领用编号按源领用顺序重分配并输出映射，
// 工单及退回引用同步更新；源只读，目标原有记录不变，导入的未关闭工单可继续退回。
func TestImportPartsRemapAndContinue(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	// 源：EQ-A 一张已关闭工单（领用 4 退 1）与一张未关闭工单（领用 2）。
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := src.withdrawPart("T0001", "FILTER-01", 4, "更换滤芯"); err != nil { // P0001
		t.Fatal(err)
	}
	if _, _, err := src.returnPart("P0001", 1, "多余"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, err := src.withdrawPart("T0002", "ROLLER-02", 2, "更换搓纸轮"); err != nil { // P0002
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标：EQ-X 一张未关闭工单，已有一笔领用 P0001。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "抖动", "req-x1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := dst.withdrawPart("T0001", "BELT-01", 1, "更换皮带"); err != nil { // P0001
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	dstBefore := readFileBytes(t, dstDir)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	wantParts := []partRemap{{"P0001", "P0002"}, {"P0002", "P0003"}}
	if len(outcome.parts) != len(wantParts) {
		t.Fatalf("领用映射数 = %d, 想得到 %d", len(outcome.parts), len(wantParts))
	}
	for i, m := range outcome.parts {
		if m != wantParts[i] {
			t.Fatalf("领用映射[%d] = %v, 想得到 %v", i, m, wantParts[i])
		}
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}
	if string(dstBefore) == string(readFileBytes(t, dstDir)) {
		t.Fatal("目标台账应已更新")
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextPartSeq != 4 {
		t.Fatalf("下一领用序号 = %d, 想得到 4", s.data.NextPartSeq)
	}
	// 目标原有记录不变。
	orig := s.findPart("P0001")
	if orig == nil || orig.TicketID != "T0001" || orig.PartID != "BELT-01" || orig.Quantity != 1 {
		t.Fatalf("目标原有领用 P0001 不应改变，得到 %+v", orig)
	}
	// 导入记录：工单引用同步替换，数量与累计退回保持。
	p2 := s.findPart("P0002")
	if p2 == nil || p2.TicketID != "T0002" || p2.AssetID != "EQ-A" ||
		p2.Quantity != 4 || p2.Returned != 1 || p2.Note != "更换滤芯" {
		t.Fatalf("导入的 P0002 业务信息应保留，得到 %+v", p2)
	}
	// 退回履历的领用引用同步更新。
	var retEv *Event
	for i := range s.data.Events {
		if s.data.Events[i].Kind == eventPartReturn {
			retEv = &s.data.Events[i]
		}
	}
	if retEv == nil || retEv.WithdrawalID != "P0002" || retEv.TicketID != "T0002" || retEv.Quantity != 1 {
		t.Fatalf("退回履历引用应同步更新，得到 %+v", retEv)
	}
	// 导入的未关闭工单（映射后 T0003）可继续退回。
	got, _, err := s.returnPart("P0003", 1, "多余退回")
	if err != nil {
		t.Fatalf("导入的未关闭工单应可继续退回: %v", err)
	}
	if got.Returned != 1 || got.Quantity-got.Returned != 1 {
		t.Fatalf("退回后累计应为 1、净量 1，得到 %+v", got)
	}
	mustSave(t, s)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("退回后的台账应保持一致: %v", err)
	}
}

// 矛盾：退回指向未知或更晚的领用、累计退回超原数量、终结后出现领用/退回履历、
// 领用编号重复等，加载即拒绝并保留原文件字节，不自动修复。
func TestPartContradictoryLedgerRejected(t *testing.T) {
	// 合法基准：T0001 未关闭，P0001 领用 2，一条退回 1。
	base := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"更换滤芯","part_id":"FILTER-01","quantity":2,"withdrawal_id":"P0001","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"多余","part_id":"FILTER-01","quantity":1,"withdrawal_id":"P0001","time":"2026-10-01T10:00:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "parts": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_id":"FILTER-01","quantity":2,"note":"更换滤芯","returned":1}],
  "next_ticket_seq": 2,
  "next_part_seq": 2
}`
	write := func(t *testing.T, mutate func(m map[string]any)) (string, []byte) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(base), &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, raw
	}
	eventsOf := func(m map[string]any) []any { return m["events"].([]any) }

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"退回指向未知领用编号", func(m map[string]any) {
			eventsOf(m)[2].(map[string]any)["withdrawal_id"] = "P0009"
		}},
		{"累计退回超过原数量", func(m map[string]any) {
			eventsOf(m)[2].(map[string]any)["quantity"] = 2 // 累计 2+? -> 退回 2 > 剩余 1
		}},
		{"保存的累计退回与履历不符", func(m map[string]any) {
			m["parts"].([]any)[0].(map[string]any)["returned"] = 0
		}},
		{"领用记录缺少领用履历", func(m map[string]any) {
			m["events"] = eventsOf(m)[:2]
			m["parts"].([]any)[0].(map[string]any)["returned"] = 0
			// 再加一笔没有履历的领用记录。
			m["parts"] = append(m["parts"].([]any), map[string]any{
				"id": "P0002", "ticket_id": "T0001", "asset_id": "EQ-1",
				"part_id": "ROLLER-02", "quantity": 1, "note": "搓纸轮", "returned": 0,
			})
			m["next_part_seq"] = 3
		}},
		{"终结后出现退回履历", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T09:30:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
			ev := eventsOf(m)
			// 在领用之后、退回之前插入关闭履历，退回仍在终结之后。
			closeEv := map[string]any{
				"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "关闭",
				"content": "已修复", "time": "2026-10-01T09:30:00Z",
			}
			retEv := ev[2].(map[string]any)
			retEv["seq"] = 5
			m["events"] = []any{ev[0], ev[1], closeEv, retEv}
		}},
		{"领用编号重复", func(m map[string]any) {
			m["parts"] = append(m["parts"].([]any), map[string]any{
				"id": "P0001", "ticket_id": "T0001", "asset_id": "EQ-1",
				"part_id": "ROLLER-02", "quantity": 1, "note": "搓纸轮", "returned": 0,
			})
		}},
		{"下一领用序号不大于已用序号", func(m map[string]any) {
			m["next_part_seq"] = 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, raw := write(t, tc.mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			}
			if got := readFileBytes(t, dir); string(got) != string(raw) {
				t.Fatal("拒绝加载不应改动原文件字节")
			}
		})
	}

	// 基准台账本身合法，可正常加载。
	dir, _ := write(t, func(m map[string]any) {})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if p := s.findPart("P0001"); p == nil || p.Returned != 1 {
		t.Fatalf("合法台账的领用记录应加载，得到 %+v", p)
	}
}

// 无备件记录的有效旧库直接使用：缺省视为没有领用记录，领用编号从 P0001 开始。
func TestLegacyStoreWithoutParts(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // 旧格式台账，无 parts 与 next_part_seq 字段
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无备件记录的旧库应能加载: %v", err)
	}
	p, err := s.withdrawPart("T0001", "FILTER-01", 2, "更换滤芯")
	if err != nil || p.ID != "P0001" {
		t.Fatalf("旧库首笔领用编号应为 P0001: %v %+v", err, p)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("领用后重载: %v", err)
	}
	if got := s2.findPart("P0001"); got == nil || got.Quantity != 2 {
		t.Fatalf("重载后领用记录应保持，得到 %+v", got)
	}
}

// 失败重载：写入失败保留原文件字节、不留下部分记录、不消耗编号，恢复后可重试。
func TestPartSaveFailureLeavesFileUntouched(t *testing.T) {
	s, dir := newPartStore(t)
	if _, err := s.withdrawPart("T0001", "FILTER-01", 3, "更换滤芯"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.withdrawPart("T0001", "ROLLER-02", 1, "搓纸轮"); err != nil {
		t.Fatal(err)
	}
	saveErr := s2.save()
	if cerr := os.Chmod(dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	// 重载后无部分记录、编号未消耗，恢复后可重试。
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if len(s3.data.Parts) != 1 || s3.data.NextPartSeq != 2 {
		t.Fatal("写入失败不应留下部分记录，也不应消耗领用编号")
	}
	p, err := s3.withdrawPart("T0001", "ROLLER-02", 1, "搓纸轮")
	if err != nil || p.ID != "P0002" {
		t.Fatalf("恢复后重试应分配 P0002: %v %+v", err, p)
	}
	mustSave(t, s3)
}

// 命令行入口：参数错误退出 2，业务失败退出 1；ticket 按领用顺序显示各笔与
// 净量汇总（零值仍显示），history 展示领用、退回履历。
func TestPartCommandLine(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runCmd := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := run(append(args, "--data-dir", dir), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	runCmd("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runCmd("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")

	// 参数错误：缺说明、数量非正，退出 2。
	if code, _, _ := runCmd("withdraw", "--ticket-id", "T0001", "--part-id", "F-1", "--quantity", "1"); code != 2 {
		t.Fatalf("缺少 --note 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("withdraw", "--ticket-id", "T0001", "--part-id", "F-1", "--quantity", "0", "--note", "x"); code != 2 {
		t.Fatalf("数量非正应退出 2，得到 %d", code)
	}
	// 业务失败：未知工单领用退出 1。
	if code, _, _ := runCmd("withdraw", "--ticket-id", "T9999", "--part-id", "F-1", "--quantity", "1", "--note", "x"); code != 1 {
		t.Fatalf("未知工单领用应退出 1，得到 %d", code)
	}

	// 成功领用并输出领用编号。
	code, o, _ := runCmd("withdraw", "--ticket-id", "T0001", "--part-id", "B-2", "--quantity", "2", "--note", "领用 B")
	if code != 0 || !strings.Contains(o, "领用编号: P0001") {
		t.Fatalf("领用应成功并输出 P0001，得到 %d:\n%s", code, o)
	}
	runCmd("withdraw", "--ticket-id", "T0001", "--part-id", "A-1", "--quantity", "1", "--note", "领用 A")
	// 全部退回 A-1：净量零仍显示。
	code, o, _ = runCmd("return", "--withdrawal-id", "P0002", "--quantity", "1", "--reason", "多余")
	if code != 0 || !strings.Contains(o, "累计退回: 1") || !strings.Contains(o, "净量: 0") {
		t.Fatalf("退回应显示累计退回与净量，得到 %d:\n%s", code, o)
	}
	// 超额退回退出 1。
	if code, _, _ := runCmd("return", "--withdrawal-id", "P0001", "--quantity", "3", "--reason", "超额"); code != 1 {
		t.Fatalf("超额退回应退出 1，得到 %d", code)
	}
	// 未知领用编号退出 1。
	if code, _, _ := runCmd("return", "--withdrawal-id", "P9999", "--quantity", "1", "--reason", "x"); code != 1 {
		t.Fatalf("未知领用编号应退出 1，得到 %d", code)
	}

	// ticket：按领用顺序显示各笔，按备件编号字典序汇总，零值仍显示。
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 {
		t.Fatalf("ticket 查询应成功，得到 %d", code)
	}
	i1 := strings.Index(o, "P0001\tB-2\t原数量 2\t累计退回 0\t净量 2")
	i2 := strings.Index(o, "P0002\tA-1\t原数量 1\t累计退回 1\t净量 0")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("ticket 应按领用顺序显示各笔记录，得到:\n%s", o)
	}
	s1 := strings.Index(o, "A-1\t净量 0")
	s2 := strings.Index(o, "B-2\t净量 2")
	if s1 < 0 || s2 < 0 || s1 > s2 {
		t.Fatalf("ticket 应按备件编号字典序汇总净量（零值仍显示），得到:\n%s", o)
	}

	// history：展示领用与退回履历。
	code, o, _ = runCmd("history", "--asset-id", "EQ-1")
	if code != 0 || !strings.Contains(o, "领用 工单 T0001: 领用编号 P0001，备件 B-2，数量 2（领用 B）") ||
		!strings.Contains(o, "退回 工单 T0001: 领用编号 P0002，备件 A-1，数量 1（多余）") {
		t.Fatalf("history 应展示领用与退回履历，得到 %d:\n%s", code, o)
	}

	// 无记录明确提示。
	runCmd("register", "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼")
	runCmd("report", "--asset-id", "EQ-2", "--description", "不制冷", "--request-id", "req-2")
	code, o, _ = runCmd("ticket", "--ticket-id", "T0002")
	if code != 0 || !strings.Contains(o, "备件领用: 无") {
		t.Fatalf("无备件记录应明确提示，得到 %d:\n%s", code, o)
	}
}
