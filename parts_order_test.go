package main

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 乱序与编号逆序：有效台账允许记录数组乱序、履历序号有间隔、履历时间不递增、
// 领用编号大小与领用先后不一致。各笔先后由该笔唯一领用履历的全库序号决定：
// ticket 按此顺序展示，汇总净量正确，查询不写文件；继续领用时编号与履历序号
// 正常延续，重载后顺序保持。
func TestPartOrderByWithdrawEventSeq(t *testing.T) {
	// 领用履历序号 10/20/25 分别对应 P0003/P0001/P0002（编号大小与先后相反），
	// 事件数组与 parts 数组均乱序，履历时间不递增（报修时间晚于各领用时间）。
	ledger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0002","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":30,"asset_id":"EQ-1","ticket_id":"T0002","kind":"退回","content":"多余","part_id":"B-2","quantity":1,"withdrawal_id":"P0001","time":"2026-10-01T08:30:00Z"},
    {"seq":5,"asset_id":"EQ-1","ticket_id":"T0002","kind":"报修","content":"卡纸","time":"2026-10-01T09:00:00Z"},
    {"seq":20,"asset_id":"EQ-1","ticket_id":"T0002","kind":"领用","content":"领用 B","part_id":"B-2","quantity":4,"withdrawal_id":"P0001","time":"2026-10-01T08:40:00Z"},
    {"seq":25,"asset_id":"EQ-1","ticket_id":"T0002","kind":"领用","content":"领用 C","part_id":"C-3","quantity":2,"withdrawal_id":"P0002","time":"2026-10-01T08:35:00Z"},
    {"seq":10,"asset_id":"EQ-1","ticket_id":"T0002","kind":"领用","content":"领用 A","part_id":"A-1","quantity":3,"withdrawal_id":"P0003","time":"2026-10-01T08:50:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0002"}],
  "parts": [
    {"id":"P0002","ticket_id":"T0002","asset_id":"EQ-1","part_id":"C-3","quantity":2,"note":"领用 C","returned":0},
    {"id":"P0003","ticket_id":"T0002","asset_id":"EQ-1","part_id":"A-1","quantity":3,"note":"领用 A","returned":0},
    {"id":"P0001","ticket_id":"T0002","asset_id":"EQ-1","part_id":"B-2","quantity":4,"note":"领用 B","returned":1}
  ],
  "next_ticket_seq": 3,
  "next_part_seq": 4
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dataFileName), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, dir)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序、序号间隔、时间逆序、编号逆序的合法台账应能加载: %v", err)
	}
	parts := s.partsOf("T0002")
	wantOrder := []string{"P0003", "P0001", "P0002"}
	if len(parts) != len(wantOrder) {
		t.Fatalf("领用笔数 = %d, 想得到 %d", len(parts), len(wantOrder))
	}
	for i, id := range wantOrder {
		if parts[i].ID != id {
			t.Fatalf("第 %d 笔应为 %s（按领用履历序号），得到 %s", i, id, parts[i].ID)
		}
	}

	// ticket 查询按领用履历序号顺序展示，汇总按备件编号字典序；查询不写文件。
	var out, errOut bytes.Buffer
	code := run([]string{"ticket", "--ticket-id", "T0002", "--data-dir", dir}, &out, &errOut)
	if code != 0 {
		t.Fatalf("ticket 查询应成功，得到 %d（%s）", code, errOut.String())
	}
	o := out.String()
	i1 := strings.Index(o, "P0003\tA-1\t原数量 3\t累计退回 0\t净量 3")
	i2 := strings.Index(o, "P0001\tB-2\t原数量 4\t累计退回 1\t净量 3")
	i3 := strings.Index(o, "P0002\tC-3\t原数量 2\t累计退回 0\t净量 2")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("ticket 应按领用履历序号顺序 P0003/P0001/P0002 展示，得到:\n%s", o)
	}
	n1 := strings.Index(o, "A-1\t净量 3")
	n2 := strings.Index(o, "B-2\t净量 3")
	n3 := strings.Index(o, "C-3\t净量 2")
	if n1 < 0 || n2 < 0 || n3 < 0 || !(n1 < n2 && n2 < n3) {
		t.Fatalf("ticket 应按备件编号字典序汇总净量，得到:\n%s", o)
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("查询不得写文件")
	}

	// 继续领用：编号从 P0004 延续，履历序号在最大序号 30 之后；重载后顺序保持。
	p4, err := s.withdrawPart("T0002", "A-1", 1, "补充")
	if err != nil || p4.ID != "P0004" {
		t.Fatalf("领用编号应延续为 P0004: %v %+v", err, p4)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	parts = s2.partsOf("T0002")
	wantOrder = append(wantOrder, "P0004")
	if len(parts) != len(wantOrder) {
		t.Fatalf("重载后领用笔数 = %d, 想得到 %d", len(parts), len(wantOrder))
	}
	for i, id := range wantOrder {
		if parts[i].ID != id {
			t.Fatalf("重载后第 %d 笔应为 %s，得到 %s", i, id, parts[i].ID)
		}
	}

	// 查询不得初始化目录：不存在的数据目录查询失败且不创建目录。
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"ticket", "--ticket-id", "T0001", "--data-dir", missing}, &out, &errOut); code != 1 {
		t.Fatalf("未知工单查询应退出 1，得到 %d", code)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("查询不得初始化数据目录")
	}
}

// 跨工单导入接续：源台账中各笔领用编号大小与领用先后不一致且跨工单交错，
// 导入按源领用履历序号整体排序分配新编号并输出映射；导入的未关闭工单可按
// 映射后的领用编号继续部分退回，重载后展示顺序、净量与编号延续保持。
func TestImportPartOrderBySourceWithdrawSeq(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 领用履历顺序：P0002（序号 2，T0001）→ P0001（序号 6，T0002）→ P0003（序号 7，T0002），
	// 编号大小与领用先后不一致；parts 数组乱序。
	srcLedger := `{
  "version": 1,
  "assets": [{"id":"EQ-A","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [
    {"id":"T0001","asset_id":"EQ-A","description":"卡纸","request_id":"req-a1","status":"已关闭","result":"已修复","created_at":"2026-10-01T08:00:00Z","closed_at":"2026-10-01T12:00:00Z"},
    {"id":"T0002","asset_id":"EQ-A","description":"无法开机","request_id":"req-a2","status":"未关闭","created_at":"2026-10-01T13:00:00Z"}
  ],
  "events": [
    {"seq":1,"asset_id":"EQ-A","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-A","ticket_id":"T0001","kind":"领用","content":"领用滤芯","part_id":"F-1","quantity":5,"withdrawal_id":"P0002","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-A","ticket_id":"T0001","kind":"退回","content":"多余","part_id":"F-1","quantity":2,"withdrawal_id":"P0002","time":"2026-10-01T10:00:00Z"},
    {"seq":4,"asset_id":"EQ-A","ticket_id":"T0001","kind":"关闭","content":"已修复","time":"2026-10-01T12:00:00Z"},
    {"seq":5,"asset_id":"EQ-A","ticket_id":"T0002","kind":"报修","content":"无法开机","time":"2026-10-01T13:00:00Z"},
    {"seq":6,"asset_id":"EQ-A","ticket_id":"T0002","kind":"领用","content":"领用电源","part_id":"PS-1","quantity":4,"withdrawal_id":"P0001","time":"2026-10-01T14:00:00Z"},
    {"seq":7,"asset_id":"EQ-A","ticket_id":"T0002","kind":"领用","content":"领用主板","part_id":"MB-1","quantity":1,"withdrawal_id":"P0003","time":"2026-10-01T15:00:00Z"}
  ],
  "requests": [
    {"request_id":"req-a1","asset_id":"EQ-A","description":"卡纸","ticket_id":"T0001"},
    {"request_id":"req-a2","asset_id":"EQ-A","description":"无法开机","ticket_id":"T0002"}
  ],
  "parts": [
    {"id":"P0003","ticket_id":"T0002","asset_id":"EQ-A","part_id":"MB-1","quantity":1,"note":"领用主板","returned":0},
    {"id":"P0002","ticket_id":"T0001","asset_id":"EQ-A","part_id":"F-1","quantity":5,"note":"领用滤芯","returned":2},
    {"id":"P0001","ticket_id":"T0002","asset_id":"EQ-A","part_id":"PS-1","quantity":4,"note":"领用电源","returned":0}
  ],
  "next_ticket_seq": 3,
  "next_part_seq": 4
}`
	if err := os.WriteFile(filepath.Join(srcDir, dataFileName), []byte(srcLedger), 0o644); err != nil {
		t.Fatal(err)
	}

	// 目标：EQ-X 一张未关闭工单，已有一笔领用 P0001，下一领用序号为 2。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "抖动", "req-x1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := dst.withdrawPart("T0001", "BELT-1", 1, "更换皮带"); err != nil { // P0001
		t.Fatal(err)
	}
	mustSave(t, dst)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 按源领用履历序号（P0002、P0001、P0003）从目标下一领用序号 2 依次分配。
	wantParts := []partRemap{{"P0002", "P0002"}, {"P0001", "P0003"}, {"P0003", "P0004"}}
	if len(outcome.parts) != len(wantParts) {
		t.Fatalf("领用映射数 = %d, 想得到 %d", len(outcome.parts), len(wantParts))
	}
	for i, m := range outcome.parts {
		if m != wantParts[i] {
			t.Fatalf("领用映射[%d] = %v, 想得到 %v（按源领用履历序号排序）", i, m, wantParts[i])
		}
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextPartSeq != 5 {
		t.Fatalf("下一领用序号 = %d, 想得到 5", s.data.NextPartSeq)
	}
	// 数量、累计退回、说明保持；工单引用同步替换。
	p2 := s.findPart("P0002")
	if p2 == nil || p2.TicketID != "T0002" || p2.Quantity != 5 || p2.Returned != 2 || p2.Note != "领用滤芯" {
		t.Fatalf("导入的 P0002 业务信息应保留，得到 %+v", p2)
	}
	// 导入的未关闭工单（T0003）各笔按领用履历顺序展示：P0003、P0004。
	parts := s.partsOf("T0003")
	if len(parts) != 2 || parts[0].ID != "P0003" || parts[1].ID != "P0004" {
		t.Fatalf("T0003 的领用顺序应为 P0003、P0004，得到 %+v", parts)
	}
	// 按映射后的领用编号继续部分退回。
	got, _, err := s.returnPart("P0003", 1, "多余退回")
	if err != nil {
		t.Fatalf("导入的未关闭工单应可继续退回: %v", err)
	}
	if got.Returned != 1 || got.Quantity-got.Returned != 3 {
		t.Fatalf("退回后累计应为 1、净量 3，得到 %+v", got)
	}
	// 编号延续：新领用为 P0005。
	p5, err := s.withdrawPart("T0003", "PS-1", 2, "补充电源")
	if err != nil || p5.ID != "P0005" {
		t.Fatalf("新领用编号应延续为 P0005: %v %+v", err, p5)
	}
	mustSave(t, s)

	// 重载后展示顺序、净量与编号延续保持。
	s2, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("退回后重载: %v", err)
	}
	parts = s2.partsOf("T0003")
	wantOrder := []string{"P0003", "P0004", "P0005"}
	if len(parts) != len(wantOrder) {
		t.Fatalf("重载后领用笔数 = %d, 想得到 %d", len(parts), len(wantOrder))
	}
	for i, id := range wantOrder {
		if parts[i].ID != id {
			t.Fatalf("重载后第 %d 笔应为 %s，得到 %s", i, id, parts[i].ID)
		}
	}
	rows := s2.partNetSummary("T0003")
	if len(rows) != 2 || rows[0].PartID != "MB-1" || rows[0].Net.String() != "1" ||
		rows[1].PartID != "PS-1" || rows[1].Net.String() != "5" {
		t.Fatalf("重载后净量汇总应为 MB-1=1、PS-1=5，得到 %+v", rows)
	}
	if s2.data.NextPartSeq != 6 {
		t.Fatalf("重载后下一领用序号 = %d, 想得到 6", s2.data.NextPartSeq)
	}
}

// 数量上限附近退回：每次退回不得超过该笔剩余可退数量；超额判断不做可能整数
// 溢出的加法比较，巨大退回数量必须被拒绝；失败的退回不改动记录，重载后一致。
func TestPartReturnNearQuantityLimit(t *testing.T) {
	s, dir := newPartStore(t)

	// 领用 MaxInt-1，退回 MaxInt-2 后剩余 1：退 2 拒绝，退 1 成功。
	if _, err := s.withdrawPart("T0001", "F-1", math.MaxInt-1, "大额领用"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0001", math.MaxInt-2, "退回大部分"); err != nil {
		t.Fatalf("上限附近的合法退回应成功: %v", err)
	}
	if _, _, err := s.returnPart("P0001", 2, "超额"); !errors.Is(err, errConflict) {
		t.Fatalf("超过剩余可退数量应拒绝，得到 %v", err)
	}
	got, _, err := s.returnPart("P0001", 1, "退清")
	if err != nil {
		t.Fatalf("退回剩余全部应成功: %v", err)
	}
	if got.Returned != math.MaxInt-1 || got.Quantity-got.Returned != 0 {
		t.Fatalf("累计退回应为 %d、净量 0，得到 %+v", math.MaxInt-1, got)
	}

	// 溢出陷阱：已退 5、原数量 10，再退 MaxInt-3。若用 Returned+quantity 比较，
	// 加法回绕成负数会错误接受；必须拒绝且不改动累计退回。
	if _, err := s.withdrawPart("T0001", "F-2", 10, "小额领用"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0002", 5, "部分退回"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0002", math.MaxInt-3, "溢出超额"); !errors.Is(err, errConflict) {
		t.Fatalf("回绕式超额退回应拒绝，得到 %v", err)
	}
	if p := s.findPart("P0002"); p.Returned != 5 {
		t.Fatalf("失败的退回应不改动累计退回，得到 %d", p.Returned)
	}
	mustSave(t, s)

	// 失败后重载：无部分记录，数据与失败前一致，合法操作可继续。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("失败后重载: %v", err)
	}
	p1 := s2.findPart("P0001")
	p2 := s2.findPart("P0002")
	if p1 == nil || p1.Returned != math.MaxInt-1 || p2 == nil || p2.Returned != 5 {
		t.Fatalf("重载后累计退回应保持，得到 P0001=%+v P0002=%+v", p1, p2)
	}
	if _, _, err := s2.returnPart("P0002", 5, "退清"); err != nil {
		t.Fatalf("恢复后合法退回应成功: %v", err)
	}
	mustSave(t, s2)
}

// 累计回绕：退回履历累计数量整数溢出后恰好等于保存值时，仍须按实际超额拒绝。
// 所有读取或写入台账的命令遇到这种矛盾退出 1 并指出备件数量问题，不自动修复。
func TestPartReturnedWraparoundRejected(t *testing.T) {
	// 原数量 MaxInt，三笔退回 MaxInt、MaxInt、2：真实累计 2^64 远超原数量，
	// 但回绕后恰为 0，与保存的 returned=0 相等。
	ledger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"领用","part_id":"F-1","quantity":9223372036854775807,"withdrawal_id":"P0001","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退一","part_id":"F-1","quantity":9223372036854775807,"withdrawal_id":"P0001","time":"2026-10-01T10:00:00Z"},
    {"seq":4,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退二","part_id":"F-1","quantity":9223372036854775807,"withdrawal_id":"P0001","time":"2026-10-01T11:00:00Z"},
    {"seq":5,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退三","part_id":"F-1","quantity":2,"withdrawal_id":"P0001","time":"2026-10-01T12:00:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "parts": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_id":"F-1","quantity":9223372036854775807,"note":"领用","returned":0}],
  "next_ticket_seq": 2,
  "next_part_seq": 2
}`
	dir := t.TempDir()
	path := filepath.Join(dir, dataFileName)
	if err := os.WriteFile(path, []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, dir)

	if _, err := openStore(dir); err == nil {
		t.Fatal("累计回绕的矛盾台账应拒绝加载")
	} else if !strings.Contains(err.Error(), "备件数量") {
		t.Fatalf("错误应指出备件数量问题，得到: %v", err)
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("拒绝加载不应改动原文件字节")
	}

	// 读取与写入命令都退出 1 并指出备件数量问题。
	var out, errOut bytes.Buffer
	for _, args := range [][]string{
		{"ticket", "--ticket-id", "T0001"},
		{"return", "--withdrawal-id", "P0001", "--quantity", "1", "--reason", "x"},
		{"withdraw", "--ticket-id", "T0001", "--part-id", "F-2", "--quantity", "1", "--note", "x"},
	} {
		out.Reset()
		errOut.Reset()
		code := run(append(args, "--data-dir", dir), &out, &errOut)
		if code != 1 {
			t.Fatalf("%v 应退出 1，得到 %d", args, code)
		}
		if !strings.Contains(errOut.String(), "备件数量") {
			t.Fatalf("%v 的错误应指出备件数量问题，得到: %s", args, errOut.String())
		}
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("矛盾台账不应被任何命令修改")
	}
}

// 精确汇总：各笔合法净量的汇总输出精确十进制整数；合计超出单笔数量范围
// 不回绕、不截断，也不因此拒绝合法台账或领用。
func TestPartNetSummaryExact(t *testing.T) {
	s, dir := newPartStore(t)
	q := math.MaxInt - 1
	if _, err := s.withdrawPart("T0001", "F-1", q, "第一笔"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withdrawPart("T0001", "F-1", q, "第二笔"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	// 2*(MaxInt-1) = 18446744073709551612，超出 int64 正数范围，须精确输出。
	want := "18446744073709551612"
	rows := s.partNetSummary("T0001")
	if len(rows) != 1 || rows[0].PartID != "F-1" || rows[0].Net.String() != want {
		t.Fatalf("汇总净量应为精确十进制 %s，得到 %+v", want, rows)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"ticket", "--ticket-id", "T0001", "--data-dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("合法台账的 ticket 查询不应被拒绝，得到 %d（%s）", code, errOut.String())
	}
	if !strings.Contains(out.String(), "F-1\t净量 "+want) {
		t.Fatalf("ticket 汇总应输出精确十进制 %s，得到:\n%s", want, out.String())
	}

	// 合法台账上的领用不被拒绝：第三笔后合计为 18446744073709551613。
	if _, err := s.withdrawPart("T0001", "F-1", 1, "第三笔"); err != nil {
		t.Fatalf("合计超出单笔范围不应拒绝合法领用: %v", err)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	rows = s2.partNetSummary("T0001")
	if len(rows) != 1 || rows[0].Net.String() != "18446744073709551613" {
		t.Fatalf("第三笔后汇总应为 18446744073709551613，得到 %+v", rows)
	}
}
