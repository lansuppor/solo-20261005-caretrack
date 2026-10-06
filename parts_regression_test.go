package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 乱序与编号逆序：记录数组乱序、履历序号有间隔、履历时间不递增、领用编号大小
// 与领用先后不一致的有效台账无需转换即可加载；ticket 按各笔唯一领用履历的全库
// 序号顺序展示，查询只读不写文件，之后可继续领用与退回，编号从计数器延续。
func TestPartOrderByWithdrawEventSeq(t *testing.T) {
	// 领用先后（按履历序号）：P0001(20) < P0002(30) < P0003(40)，
	// 与编号大小相反；数组乱序、序号有间隔、时间不递增。
	ledger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":40,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"领用 B","part_id":"B-2","quantity":2,"withdrawal_id":"P0003","time":"2026-10-01T09:00:00Z"},
    {"seq":10,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":30,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"领用 A 二","part_id":"A-1","quantity":3,"withdrawal_id":"P0002","time":"2026-10-01T08:30:00Z"},
    {"seq":50,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"多余","part_id":"B-2","quantity":1,"withdrawal_id":"P0003","time":"2026-10-01T10:00:00Z"},
    {"seq":20,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"领用 A 一","part_id":"A-1","quantity":5,"withdrawal_id":"P0001","time":"2026-10-01T09:30:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "parts": [
    {"id":"P0002","ticket_id":"T0001","asset_id":"EQ-1","part_id":"A-1","quantity":3,"note":"领用 A 二","returned":0},
    {"id":"P0003","ticket_id":"T0001","asset_id":"EQ-1","part_id":"B-2","quantity":2,"note":"领用 B","returned":1},
    {"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_id":"A-1","quantity":5,"note":"领用 A 一","returned":0}
  ],
  "next_ticket_seq": 2,
  "next_part_seq": 4
}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dataFileName), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, dir)

	// 有效台账无需转换即可加载。
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序、序号间隔、时间不递增、编号逆序的有效台账应能加载: %v", err)
	}
	// partsOf 按领用履历序号排序，而非数组位置或编号大小。
	var order []string
	for _, p := range s.partsOf("T0001") {
		order = append(order, p.ID)
	}
	if !reflect.DeepEqual(order, []string{"P0001", "P0002", "P0003"}) {
		t.Fatalf("领用顺序应按领用履历序号，得到 %v", order)
	}

	// ticket 命令按同一顺序展示；查询为只读，文件字节不变。
	var out, errOut bytes.Buffer
	code := run([]string{"ticket", "--ticket-id", "T0001", "--data-dir", dir}, &out, &errOut)
	if code != 0 {
		t.Fatalf("ticket 查询应成功，得到 %d: %s", code, errOut.String())
	}
	o := out.String()
	i1 := strings.Index(o, "P0001\tA-1\t原数量 5\t累计退回 0\t净量 5")
	i2 := strings.Index(o, "P0002\tA-1\t原数量 3\t累计退回 0\t净量 3")
	i3 := strings.Index(o, "P0003\tB-2\t原数量 2\t累计退回 1\t净量 1")
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Fatalf("ticket 应按领用履历序号顺序展示，得到:\n%s", o)
	}
	if !strings.Contains(o, "A-1\t净量 8") || !strings.Contains(o, "B-2\t净量 1") {
		t.Fatalf("净量汇总错误，得到:\n%s", o)
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("查询不得写文件")
	}

	// 无需转换即可继续领用与退回：编号从计数器延续，履历序号在最大值后续排。
	p, err := s.withdrawPart("T0001", "C-3", 1, "补充")
	if err != nil || p.ID != "P0004" {
		t.Fatalf("继续领用应分配 P0004: %v %+v", err, p)
	}
	if _, _, err := s.returnPart("P0001", 2, "多余"); err != nil {
		t.Fatalf("继续退回: %v", err)
	}
	mustSave(t, s)

	// 重载后展示顺序、净量与编号延续保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	order = order[:0]
	for _, p := range s2.partsOf("T0001") {
		order = append(order, p.ID)
	}
	if !reflect.DeepEqual(order, []string{"P0001", "P0002", "P0003", "P0004"}) {
		t.Fatalf("重载后领用顺序应保持，得到 %v", order)
	}
	if got := netRowsText(s2.partNetSummary("T0001")); !reflect.DeepEqual(got, []string{"A-1=6", "B-2=1", "C-3=1"}) {
		t.Fatalf("重载后净量汇总应保持，得到 %v", got)
	}
}

// 跨工单导入接续：源台账中领用先后与编号大小相反（P0002 先于 P0001 领用，
// 分属不同工单），导入按源领用履历序号整体排序分配新编号并输出映射；导入的
// 未关闭工单可按映射后的编号继续部分退回，重载后展示顺序、净量与编号延续保持。
func TestImportOrdersPartsBySourceWithdrawSeq(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	// 源：领用先后为 P0002（T0001，履历序号 2）然后 P0001（T0002，履历序号 5），
	// 编号大小与领用先后相反；履历数组乱序。
	srcLedger := `{
  "version": 1,
  "assets": [{"id":"EQ-A","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [
    {"id":"T0001","asset_id":"EQ-A","description":"卡纸","request_id":"req-a1","status":"已关闭","result":"已修复","created_at":"2026-10-01T08:00:00Z","closed_at":"2026-10-01T10:00:00Z"},
    {"id":"T0002","asset_id":"EQ-A","description":"无法开机","request_id":"req-a2","status":"未关闭","created_at":"2026-10-01T11:00:00Z"}
  ],
  "events": [
    {"seq":5,"asset_id":"EQ-A","ticket_id":"T0002","kind":"领用","content":"领用 Y","part_id":"Y","quantity":2,"withdrawal_id":"P0001","time":"2026-10-01T12:00:00Z"},
    {"seq":1,"asset_id":"EQ-A","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-A","ticket_id":"T0001","kind":"领用","content":"领用 X","part_id":"X","quantity":4,"withdrawal_id":"P0002","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-A","ticket_id":"T0001","kind":"关闭","content":"已修复","time":"2026-10-01T10:00:00Z"},
    {"seq":4,"asset_id":"EQ-A","ticket_id":"T0002","kind":"报修","content":"无法开机","time":"2026-10-01T11:00:00Z"},
    {"seq":6,"asset_id":"EQ-A","ticket_id":"T0002","kind":"退回","content":"多余","part_id":"Y","quantity":1,"withdrawal_id":"P0001","time":"2026-10-01T13:00:00Z"}
  ],
  "requests": [
    {"request_id":"req-a1","asset_id":"EQ-A","description":"卡纸","ticket_id":"T0001"},
    {"request_id":"req-a2","asset_id":"EQ-A","description":"无法开机","ticket_id":"T0002"}
  ],
  "parts": [
    {"id":"P0001","ticket_id":"T0002","asset_id":"EQ-A","part_id":"Y","quantity":2,"note":"领用 Y","returned":1},
    {"id":"P0002","ticket_id":"T0001","asset_id":"EQ-A","part_id":"X","quantity":4,"note":"领用 X","returned":0}
  ],
  "next_ticket_seq": 3,
  "next_part_seq": 3
}`
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, dataFileName), []byte(srcLedger), 0o644); err != nil {
		t.Fatal(err)
	}

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

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 领用映射按源领用履历序号整体排序：P0002（先领用）先分配，P0001 后分配。
	wantParts := []partRemap{{"P0002", "P0002"}, {"P0001", "P0003"}}
	if !reflect.DeepEqual(outcome.parts, wantParts) {
		t.Fatalf("领用映射应按源领用履历序号排序，得到 %v", outcome.parts)
	}
	wantTickets := []ticketRemap{{"T0001", "T0002"}, {"T0002", "T0003"}}
	if !reflect.DeepEqual(outcome.tickets, wantTickets) {
		t.Fatalf("工单映射 = %v, 想得到 %v", outcome.tickets, wantTickets)
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextPartSeq != 4 {
		t.Fatalf("下一领用序号 = %d, 想得到 4", s.data.NextPartSeq)
	}
	p2 := s.findPart("P0002")
	if p2 == nil || p2.TicketID != "T0002" || p2.PartID != "X" || p2.Quantity != 4 || p2.Returned != 0 {
		t.Fatalf("先领用的源 P0002 应映射为 P0002，得到 %+v", p2)
	}
	p3 := s.findPart("P0003")
	if p3 == nil || p3.TicketID != "T0003" || p3.PartID != "Y" || p3.Quantity != 2 || p3.Returned != 1 {
		t.Fatalf("后领用的源 P0001 应映射为 P0003，得到 %+v", p3)
	}
	// 退回履历的领用引用同步替换。
	var retEv *Event
	for i := range s.data.Events {
		if s.data.Events[i].Kind == eventPartReturn {
			retEv = &s.data.Events[i]
		}
	}
	if retEv == nil || retEv.WithdrawalID != "P0003" || retEv.TicketID != "T0003" {
		t.Fatalf("退回履历引用应同步替换，得到 %+v", retEv)
	}

	// 导入的未关闭工单（映射后 T0003）可按新编号继续部分退回。
	got, _, err := s.returnPart("P0003", 1, "继续退回")
	if err != nil {
		t.Fatalf("导入的未关闭工单应可继续退回: %v", err)
	}
	if got.Returned != 2 || got.Quantity-got.Returned != 0 {
		t.Fatalf("退回后累计应为 2、净量 0，得到 %+v", got)
	}
	// 编号延续：新领用从 P0004 开始。
	p4, err := s.withdrawPart("T0003", "Z", 1, "新领用")
	if err != nil || p4.ID != "P0004" {
		t.Fatalf("导入后领用编号应延续为 P0004: %v %+v", err, p4)
	}
	mustSave(t, s)

	// 重载后展示顺序、净量与编号延续保持。
	s2, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("退回后重载: %v", err)
	}
	var order []string
	for _, p := range s2.partsOf("T0003") {
		order = append(order, p.ID)
	}
	if !reflect.DeepEqual(order, []string{"P0003", "P0004"}) {
		t.Fatalf("重载后 T0003 的领用顺序应保持，得到 %v", order)
	}
	if got := netRowsText(s2.partNetSummary("T0003")); !reflect.DeepEqual(got, []string{"Y=0", "Z=1"}) {
		t.Fatalf("重载后净量汇总应保持（零值仍显示），得到 %v", got)
	}
}

// 数量上限附近退回：单笔数量为 int 上限的领用可精确退满；每次退回不得超过
// 剩余可退数量，大数量退回不能因整数溢出被错误接受。
func TestPartReturnNearQuantityLimit(t *testing.T) {
	s, dir := newPartStore(t)

	// 上限数量领用并一次退满。
	p1, err := s.withdrawPart("T0001", "BIG-1", math.MaxInt, "上限领用")
	if err != nil || p1.ID != "P0001" {
		t.Fatalf("上限领用: %v %+v", err, p1)
	}
	if _, _, err := s.returnPart("P0001", math.MaxInt, "退满"); err != nil {
		t.Fatalf("一次退满应成功: %v", err)
	}
	if p1.Returned != math.MaxInt || p1.Quantity-p1.Returned != 0 {
		t.Fatalf("退满后累计应为 %d、净量 0，得到 %+v", math.MaxInt, p1)
	}
	if _, _, err := s.returnPart("P0001", 1, "超额"); !errors.Is(err, errConflict) {
		t.Fatalf("退满后再退应拒绝，得到 %v", err)
	}

	// 已退 1 后再退上限数量： Returned+quantity 会回绕，必须拒绝且不改动记录。
	p2, err := s.withdrawPart("T0001", "BIG-2", 5, "小额领用")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0002", 1, "部分"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0002", math.MaxInt, "回绕尝试"); !errors.Is(err, errConflict) {
		t.Fatalf("回绕的超额退回应拒绝，得到 %v", err)
	}
	if p2.Returned != 1 {
		t.Fatalf("被拒绝的退回不应改动累计，得到 %d", p2.Returned)
	}
	if _, _, err := s.returnPart("P0002", 4, "恰好退满"); err != nil {
		t.Fatalf("恰好退满应成功: %v", err)
	}

	// 上限领用分两次退：MaxInt-1 后只能再退 1。
	if _, err := s.withdrawPart("T0001", "BIG-3", math.MaxInt, "上限领用二"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0003", math.MaxInt-1, "大部分"); err != nil {
		t.Fatalf("退回 MaxInt-1 应成功: %v", err)
	}
	if _, _, err := s.returnPart("P0003", 2, "超额"); !errors.Is(err, errConflict) {
		t.Fatalf("超过剩余 1 的退回应拒绝，得到 %v", err)
	}
	if _, _, err := s.returnPart("P0003", 1, "退满"); err != nil {
		t.Fatalf("退回剩余 1 应成功: %v", err)
	}
	mustSave(t, s)

	// 重载后累计与净量保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	for id, want := range map[string]int{"P0001": math.MaxInt, "P0002": 5, "P0003": math.MaxInt} {
		p := s2.findPart(id)
		if p == nil || p.Returned != want || p.Quantity-p.Returned != 0 {
			t.Fatalf("重载后 %s 累计退回应为 %d、净量 0，得到 %+v", id, want, p)
		}
	}
}

// 累计回绕：退回履历累计在 int 上回绕后恰好等于保存值的台账，实际已超额，
// 加载与所有业务命令都必须拒绝并指出备件数量问题，不自动修复、原文件不变。
func TestPartCumulativeWraparoundRejected(t *testing.T) {
	max := int64(math.MaxInt64)
	// 领用 MaxInt64；退回 MaxInt64、MaxInt64、3：累计回绕为 1，恰与保存值相符。
	ledger := fmt.Sprintf(`{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"领用","content":"上限领用","part_id":"F-1","quantity":%[1]d,"withdrawal_id":"P0001","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退一","part_id":"F-1","quantity":%[1]d,"withdrawal_id":"P0001","time":"2026-10-01T10:00:00Z"},
    {"seq":4,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退二","part_id":"F-1","quantity":%[1]d,"withdrawal_id":"P0001","time":"2026-10-01T11:00:00Z"},
    {"seq":5,"asset_id":"EQ-1","ticket_id":"T0001","kind":"退回","content":"退三","part_id":"F-1","quantity":3,"withdrawal_id":"P0001","time":"2026-10-01T12:00:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "parts": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_id":"F-1","quantity":%[1]d,"note":"上限领用","returned":1}],
  "next_ticket_seq": 2,
  "next_part_seq": 2
}`, max)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dataFileName), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, dir)

	if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), "备件数量") {
		t.Fatalf("累计回绕的台账应拒绝加载并指出备件数量问题，得到 %v", err)
	}
	// 读取与写入命令都以退出码 1 失败并指出备件数量问题。
	for _, args := range [][]string{
		{"ticket", "--ticket-id", "T0001"},
		{"history", "--asset-id", "EQ-1"},
		{"withdraw", "--ticket-id", "T0001", "--part-id", "F-2", "--quantity", "1", "--note", "x"},
		{"return", "--withdrawal-id", "P0001", "--quantity", "1", "--reason", "x"},
	} {
		var out, errOut bytes.Buffer
		code := run(append(args, "--data-dir", dir), &out, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "备件数量") {
			t.Fatalf("%v 应退出 1 并指出备件数量问题，得到 %d: %s", args, code, errOut.String())
		}
	}
	// 不自动修复：原文件字节不变。
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("矛盾台账不应被修改")
	}
}

// 精确汇总：同一备件多笔净量的合计超出单笔数量范围时，汇总输出精确十进制
// 整数，不回绕、不截断，也不因此拒绝合法台账或领用。
func TestPartNetSummaryExact(t *testing.T) {
	s, dir := newPartStore(t)
	// 两笔上限领用：单笔净量合法，合计 2*MaxInt64 超出单笔范围。
	if _, err := s.withdrawPart("T0001", "BIG-1", math.MaxInt, "上限领用一"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withdrawPart("T0001", "BIG-1", math.MaxInt, "上限领用二"); err != nil {
		t.Fatal(err)
	}
	rows := s.partNetSummary("T0001")
	if got := netRowsText(rows); !reflect.DeepEqual(got, []string{"BIG-1=18446744073709551614"}) {
		t.Fatalf("汇总应输出精确十进制整数，得到 %v", got)
	}
	mustSave(t, s)

	// 合法台账不因此被拒绝：重载、查询与继续领用均正常。
	if _, err := openStore(dir); err != nil {
		t.Fatalf("合计超范围的合法台账不应被拒绝: %v", err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"ticket", "--ticket-id", "T0001", "--data-dir", dir}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "BIG-1\t净量 18446744073709551614") {
		t.Fatalf("ticket 应输出精确汇总，得到 %d: %s%s", code, out.String(), errOut.String())
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s2.withdrawPart("T0001", "BIG-1", 1, "继续领用")
	if err != nil || p.ID != "P0003" {
		t.Fatalf("合法台账应可继续领用: %v %+v", err, p)
	}
	mustSave(t, s2)
}

// 失败后重载：超额退回被拒绝时不写文件、不改动记录；重载后状态保持，
// 合法退回可重试并成功。
func TestPartFailedReturnReloadAndRetry(t *testing.T) {
	s, dir := newPartStore(t)
	if _, err := s.withdrawPart("T0001", "FILTER-01", 3, "更换滤芯"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	// 超额退回经命令入口被拒绝：退出 1，文件字节不变。
	var out, errOut bytes.Buffer
	code := run([]string{"return", "--withdrawal-id", "P0001", "--quantity", "4", "--reason", "超额", "--data-dir", dir}, &out, &errOut)
	if code != 1 {
		t.Fatalf("超额退回应退出 1，得到 %d", code)
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("被拒绝的退回不应写文件")
	}

	// 重载后无部分记录，合法退回可重试。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("失败后应可正常重载: %v", err)
	}
	if p := s2.findPart("P0001"); p == nil || p.Returned != 0 {
		t.Fatalf("被拒绝的退回不应留下部分记录，得到 %+v", p)
	}
	p, _, err := s2.returnPart("P0001", 2, "多余退回")
	if err != nil || p.Returned != 2 {
		t.Fatalf("恢复后重试应成功: %v %+v", err, p)
	}
	mustSave(t, s2)
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("重试后重载: %v", err)
	}
	if p := s3.findPart("P0001"); p == nil || p.Returned != 2 {
		t.Fatalf("重试成功后累计应保留，得到 %+v", p)
	}
}

// 查询只读：ticket、history、due 等查询不写文件，也不初始化不存在的目录。
func TestPartQueryDoesNotWriteOrInitDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "不存在")
	for _, args := range [][]string{
		{"ticket", "--ticket-id", "T0001"},
		{"history", "--asset-id", "EQ-1"},
		{"due", "--date", "2026-12-31"},
		{"list"},
	} {
		var out, errOut bytes.Buffer
		run(append(args, "--data-dir", dir), &out, &errOut)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%v 不应初始化目录", args)
		}
	}
}
