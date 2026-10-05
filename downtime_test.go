package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- 区间工具的单元测试 ----

func TestClipWindow(t *testing.T) {
	winLo := t1(1, 9)
	winHi := t1(1, 10)
	z := time.Time{}
	cases := []struct {
		name           string
		occLo, occHi   time.Time
		wantLo, wantHi time.Time
		wantOK         bool
	}{
		{"完全在内", t1(1, 9, 15), t1(1, 9, 45), t1(1, 9, 15), t1(1, 9, 45), true},
		{"完全覆盖窗口", t1(1, 8), t1(1, 11), winLo, winHi, true},
		{"跨越起点", t1(1, 8, 30), t1(1, 9, 30), winLo, t1(1, 9, 30), true},
		{"跨越终点", t1(1, 9, 30), t1(1, 10, 30), t1(1, 9, 30), winHi, true},
		{"完全在窗口前", t1(1, 7), t1(1, 8), z, z, false},
		{"完全在窗口后", t1(1, 10), t1(1, 11), z, z, false},
		{"占用终点恰为窗口起点", t1(1, 8), winLo, z, z, false},
		{"占用起点恰为窗口终点", winHi, t1(1, 11), z, z, false},
		{"零长度占用落在窗口内", t1(1, 9, 30), t1(1, 9, 30), z, z, false},
	}
	for _, c := range cases {
		got, ok := clipWindow(c.occLo, c.occHi, winLo, winHi)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v 想得到 %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && (!got.start.Equal(c.wantLo) || !got.end.Equal(c.wantHi)) {
			t.Errorf("%s: 交集=[%v,%v) 想得到 [%v,%v)", c.name, got.start, got.end, c.wantLo, c.wantHi)
		}
	}
}

// t1 构造 2026-10-01 UTC 时刻：日、时、可选分、秒。
func t1(day, hour int, extra ...int) time.Time {
	min, sec := 0, 0
	if len(extra) > 0 {
		min = extra[0]
	}
	if len(extra) > 1 {
		sec = extra[1]
	}
	return time.Date(2026, 10, day, hour, min, sec, 0, time.UTC)
}

func TestMergeWindowsAndUnion(t *testing.T) {
	// [8,10) 与 [9,11) 重叠：并集 [8,11) = 10800 秒，而非 7200+7200。
	in := []timeWindow{{t1(1, 8), t1(1, 10)}, {t1(1, 9), t1(1, 11)}}
	if got := unionDurationSeconds(in); got != 10800 {
		t.Fatalf("重叠并集 = %d，想得到 10800", got)
	}
	// 乱序 + 内含 + 相邻（边界相接不算零长度追加，合并为连续区间）。
	in = []timeWindow{
		{t1(1, 12), t1(1, 13)},
		{t1(1, 8), t1(1, 10)},
		{t1(1, 9), t1(1, 9, 30)},
		{t1(1, 10), t1(1, 12)},
	}
	merged := mergeWindows(in)
	if len(merged) != 1 || !merged[0].start.Equal(t1(1, 8)) || !merged[0].end.Equal(t1(1, 13)) {
		t.Fatalf("相接区间应合并: %+v", merged)
	}
	if got := unionDurationSeconds(nil); got != 0 {
		t.Fatalf("空并集 = %d，想得到 0", got)
	}
}

func TestUnionFractionalAggregation(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := func(hour, min, sec, nano int) time.Time {
		return time.Date(2026, 10, 1, hour, min, sec, nano, loc)
	}
	// 单个区间 7200.7 秒：向下取整 7200。
	single := []timeWindow{{at(8, 0, 0, 500_000_000), at(10, 0, 1, 200_000_000)}}
	if got := unionDurationSeconds(single); got != 7200 {
		t.Fatalf("7200.7 秒应取整为 7200，得到 %d", got)
	}
	// 两个不相邻的 0.6 秒区间：先合计 1.2 秒，再整体取整为 1 秒
	// （若每段分别取整会错算成 0）。
	two := []timeWindow{
		{at(8, 0, 0, 200_000_000), at(8, 0, 0, 800_000_000)},
		{at(9, 0, 0, 200_000_000), at(9, 0, 0, 800_000_000)},
	}
	if got := unionDurationSeconds(two); got != 1 {
		t.Fatalf("两段 0.6 秒合计 1.2 秒应取整为 1，得到 %d", got)
	}
	// 两个 -0.4 秒形态不会出现（区间恒为正）；再验 1.6 秒单段取整为 1。
	one := []timeWindow{
		{time.Date(2026, 10, 1, 8, 0, 0, 200_000_000, time.UTC),
			time.Date(2026, 10, 1, 8, 0, 1, 800_000_000, time.UTC)},
	}
	if got := unionDurationSeconds(one); got != 1 {
		t.Fatalf("1.6 秒应取整为 1，得到 %d", got)
	}
}

// ---- 台账构造辅助 ----

func ev(seq int, asset, ticket, kind, content, at string) eventJSON {
	return eventJSON{Seq: seq, AssetID: asset, TicketID: ticket, Kind: kind, Content: content, Time: at}
}

func closedTicket(id, asset, desc, req, createdAt, closedAt, result string) *Ticket {
	return &Ticket{
		ID: id, AssetID: asset, Description: desc, RequestID: req,
		Status: ticketClosed, Result: result, CreatedAt: createdAt, ClosedAt: closedAt,
	}
}

func cancelledTicket(id, asset, desc, req, createdAt, cancelledAt, reason string) *Ticket {
	return &Ticket{
		ID: id, AssetID: asset, Description: desc, RequestID: req,
		Status: ticketCancelled, CancelReason: reason, CreatedAt: createdAt, CancelledAt: cancelledAt,
	}
}

func openTicket(id, asset, desc, req, createdAt string) *Ticket {
	return &Ticket{
		ID: id, AssetID: asset, Description: desc, RequestID: req,
		Status: ticketOpen, CreatedAt: createdAt,
	}
}

func req(id, asset, desc, ticket string) requestBinding {
	return requestBinding{RequestID: id, AssetID: asset, Description: desc, TicketID: ticket}
}

// writeDowntimeLedger 直接组装一份通过一致性检查的台账，允许序号间隔、
// 数组乱序与履历时间不递增。
func writeDowntimeLedger(t *testing.T, dir string, assets []*Asset, tickets []*Ticket,
	events []eventJSON, requests []requestBinding, nextSeq int) {
	t.Helper()
	d := storeData{
		Version:       storeVersion,
		Assets:        assets,
		Tickets:       tickets,
		EventsJSON:    events,
		Requests:      requests,
		NextTicketSeq: nextSeq,
	}
	raw, err := json.MarshalIndent(&d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	t01    = "2026-10-01T08:00:00Z"
	t01End = "2026-10-01T10:00:00Z"
)

// mainLedger 构造多工单台账：
// EQ-1: T0001 已关闭 [08:00,10:00)；T0002 已取消 [次日08:00,09:30)；T0003 未关闭 第三日08:00 报修。
// EQ-2: T0004 已关闭，含小数秒 [20:00:00.5,22:00:01.2)。
// 工单与履历数组故意乱序，序号故意留间隔。
func mainLedger(t *testing.T, dir string) {
	assets := []*Asset{
		{ID: "EQ-2", Name: "空调", Location: "二楼", Status: statusAvailable},
		{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusRepairing},
	}
	tickets := []*Ticket{
		openTicket("T0003", "EQ-1", "异响", "req-3", "2026-10-03T08:00:00Z"),
		closedTicket("T0001", "EQ-1", "卡纸", "req-1", t01, t01End, "已修复"),
		{ID: "T0004", AssetID: "EQ-2", Description: "不制冷", RequestID: "req-4",
			Status: ticketClosed, Result: "已加氟", CreatedAt: "2026-10-01T20:00:00Z",
			ClosedAt: "2026-10-01T22:00:01Z"},
		cancelledTicket("T0002", "EQ-1", "无法开机", "req-2",
			"2026-10-02T08:00:00Z", "2026-10-02T09:30:00Z", "误报，设备实际正常"),
	}
	events := []eventJSON{
		ev(13, "EQ-1", "T0002", eventCancel, "误报，设备实际正常", "2026-10-02T09:30:00Z"),
		ev(1, "EQ-1", "T0001", eventReport, "卡纸", t01),
		ev(20, "EQ-1", "T0003", eventReport, "异响", "2026-10-03T08:00:00Z"),
		ev(7, "EQ-1", "T0001", eventClose, "已修复", t01End),
		ev(22, "EQ-2", "T0004", eventClose, "已加氟", "2026-10-01T22:00:01.2Z"),
		ev(10, "EQ-1", "T0002", eventReport, "无法开机", "2026-10-02T08:00:00Z"),
		ev(21, "EQ-2", "T0004", eventReport, "不制冷", "2026-10-01T20:00:00.5Z"),
	}
	requests := []requestBinding{
		req("req-3", "EQ-1", "异响", "T0003"),
		req("req-1", "EQ-1", "卡纸", "T0001"),
		req("req-4", "EQ-2", "不制冷", "T0004"),
		req("req-2", "EQ-1", "无法开机", "T0002"),
	}
	writeDowntimeLedger(t, dir, assets, tickets, events, requests, 10)
}

func runDowntime(t *testing.T, dir string, extra ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"downtime", "--data-dir", dir}, extra...)
	var out, errBuf bytes.Buffer
	code := run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// parseSingleOutput 解析单项查询输出的停机秒数与合计。
func parseSingleOutput(t *testing.T, out string) (secs, total int64) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.HasPrefix(line, "停机秒数: "):
			secs = atoi64(t, strings.TrimPrefix(line, "停机秒数: "))
		case strings.HasPrefix(line, "合计: "):
			total = atoi64(t, strings.TrimSuffix(strings.TrimPrefix(line, "合计: "), " 秒"))
		}
	}
	return secs, total
}

func atoi64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		t.Fatalf("无法解析数字 %q: %v", s, err)
	}
	return n
}

// ---- 端到端统计行为 ----

func TestDowntimeSingleAssetWindowing(t *testing.T) {
	dir := t.TempDir()
	mainLedger(t, dir)

	// 整个大窗口：T0001 7200 + T0002 5400 + 未关闭 T0003 截到终点 16h=57600。
	code, out, errb := runDowntime(t, dir,
		"--asset-id", "EQ-1",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-04T00:00:00Z")
	if code != 0 {
		t.Fatalf("统计应成功: %s", errb)
	}
	secs, total := parseSingleOutput(t, out)
	if secs != 70200 || total != 70200 {
		t.Fatalf("EQ-1 大窗口 = %d (合计 %d)，想得到 70200", secs, total)
	}
	if !strings.Contains(out, "资产编号: EQ-1") || !strings.Contains(out, "名称: 打印机") {
		t.Fatalf("单项输出应含编号与名称: %s", out)
	}

	// 窗口只裁出 T0001 的 [09:00,09:30) = 1800。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T09:00:00Z", "--end", "2026-10-01T09:30:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 1800 {
		t.Fatalf("窗口裁剪 = %d，想得到 1800", secs)
	}

	// 窗口在所有工单（含未关闭 T0003）之前：零。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-09-30T00:00:00Z", "--end", "2026-10-01T00:00:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("窗口外 = %d，想得到 0", secs)
	}
	// 未关闭工单在窗口之前报修并仍未关闭：覆盖整窗 86400（暂算到窗口终点）。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-05T00:00:00Z", "--end", "2026-10-06T00:00:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 86400 {
		t.Fatalf("未关闭工单应覆盖窗口外之后的整窗 86400，得到 %d", secs)
	}

	// 窗口终点恰好是报修时间（终点不含）：零。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T07:00:00Z", "--end", t01)
	if secs, _ = parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("终点=报修时刻应记零，得到 %d", secs)
	}
	// 窗口起点恰好是关闭时间（起点含，但占用已结束）：零。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", t01End, "--end", "2026-10-01T11:00:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("起点=关闭时刻应记零，得到 %d", secs)
	}

	// 未关闭工单：在终点时刻才报修记零；包含起点则截到窗口终点。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-03T07:00:00Z", "--end", "2026-10-03T08:00:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("未关闭工单终点时刻报修应记零，得到 %d", secs)
	}
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-03T08:00:00Z", "--end", "2026-10-03T09:00:00Z")
	if secs, _ = parseSingleOutput(t, out); secs != 3600 {
		t.Fatalf("未关闭工单应截到窗口终点 = 3600，得到 %d", secs)
	}
}

func TestDowntimeCrossTimezoneAndFractional(t *testing.T) {
	dir := t.TempDir()
	mainLedger(t, dir)

	want := [][2]string{
		{"2026-10-01T08:00:00Z", "2026-10-01T10:00:00Z"},
		{"2026-10-01T16:00:00+08:00", "2026-10-01T18:00:00+08:00"},
		{"2026-10-01T01:00:00-07:00", "2026-10-01T03:00:00-07:00"},
	}
	for _, w := range want {
		_, out, errb := runDowntime(t, dir, "--asset-id", "EQ-1", "--start", w[0], "--end", w[1])
		secs, _ := parseSingleOutput(t, out)
		if secs != 7200 {
			t.Fatalf("窗口 %s~%s 跨时区应得 7200，得到 %d（%s）", w[0], w[1], secs, errb)
		}
	}

	// EQ-2 的工单含小数秒：7200.7 秒向下取整为 7200。
	_, out, _ := runDowntime(t, dir, "--asset-id", "EQ-2",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z")
	if secs, total := parseSingleOutput(t, out); secs != 7200 || total != 7200 {
		t.Fatalf("小数秒向下取整 = %d (合计 %d)，想得到 7200", secs, total)
	}

	// 窗口边界带小数秒：[08:00:00.9, 10:00:00) = 7199.1 → 7199。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T08:00:00.9Z", "--end", "2026-10-01T10:00:00Z")
	if secs, _ := parseSingleOutput(t, out); secs != 7199 {
		t.Fatalf("小数边界裁剪 = %d，想得到 7199", secs)
	}
}

// 同一资产多张工单因履历时间逆序发生重叠：交集取并集，不相加。
func TestDowntimeOverlappingTicketsUnion(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusAvailable}}
	tickets := []*Ticket{
		closedTicket("T0001", "EQ-1", "卡纸", "req-1", t01, t01End, "已修复"),
		closedTicket("T0002", "EQ-1", "异响", "req-2",
			"2026-10-01T09:00:00Z", "2026-10-01T11:00:00Z", "已紧固"),
	}
	// T0002 的报修履历时间（09:00）早于 T0001 的关闭履历时间（10:00），
	// 但序号在后——这是允许的时间不递增，窗口内两段占用重叠。
	events := []eventJSON{
		ev(1, "EQ-1", "T0001", eventReport, "卡纸", t01),
		ev(2, "EQ-1", "T0001", eventClose, "已修复", t01End),
		ev(3, "EQ-1", "T0002", eventReport, "异响", "2026-10-01T09:00:00Z"),
		ev(4, "EQ-1", "T0002", eventClose, "已紧固", "2026-10-01T11:00:00Z"),
	}
	requests := []requestBinding{
		req("req-1", "EQ-1", "卡纸", "T0001"),
		req("req-2", "EQ-1", "异响", "T0002"),
	}
	writeDowntimeLedger(t, dir, assets, tickets, events, requests, 9)

	// 整天窗口：并集 [08:00,11:00) = 10800，而非 14400。
	_, out, _ := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z")
	if secs, _ := parseSingleOutput(t, out); secs != 10800 {
		t.Fatalf("重叠工单应取并集 10800，得到 %d", secs)
	}
	// 窗口 [09:30,10:30)：两段交集 [09:30,10:00) 与 [09:30,10:30) 并集 3600。
	_, out, _ = runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T09:30:00Z", "--end", "2026-10-01T10:30:00Z")
	if secs, _ := parseSingleOutput(t, out); secs != 3600 {
		t.Fatalf("重叠裁剪应取并集 3600（直接相加会得 5400），得到 %d", secs)
	}
}

// 取消前的占用计入停机；全部查询按字典序排列并输出各资产之和。
func TestDowntimeCancelledAndAllAssets(t *testing.T) {
	dir := t.TempDir()
	mainLedger(t, dir)

	// EQ-1 的 T0002 已取消 [08:00,09:30)，占用计入 5400 秒。
	_, out, _ := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-02T08:00:00Z", "--end", "2026-10-02T10:00:00Z")
	if secs, _ := parseSingleOutput(t, out); secs != 5400 {
		t.Fatalf("已取消工单占用 = %d，想得到 5400", secs)
	}

	// 全部查询：字典序 EQ-1 在前，合计为两资产之和（窗口内互不影响）。
	code, out, errb := runDowntime(t, dir,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z")
	if code != 0 {
		t.Fatalf("全部查询应成功: %s", errb)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "共 2 项资产") {
		t.Fatalf("全部查询抬头不对: %s", lines[0])
	}
	f1 := strings.Split(lines[1], "\t")
	f2 := strings.Split(lines[2], "\t")
	if f1[0] != "EQ-1" || atoi64(t, f1[2]) != 7200 {
		t.Fatalf("第一行应为 EQ-1/7200: %v", f1)
	}
	if f2[0] != "EQ-2" || atoi64(t, f2[2]) != 7200 {
		t.Fatalf("第二行应为 EQ-2/7200: %v", f2)
	}
	if !strings.HasSuffix(lines[3], "合计: 14400 秒") {
		t.Fatalf("合计应为 14400: %s", lines[3])
	}
}

// 空库明确提示且零合计，且只读统计不会创建数据目录。
func TestDowntimeEmptyStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet-created")
	code, out, errb := runDowntime(t, dir,
		"--start", t01, "--end", t01End)
	if code != 0 {
		t.Fatalf("空库查询应成功: %s", errb)
	}
	if !strings.Contains(out, "没有记录。") || !strings.Contains(out, "合计: 0 秒") {
		t.Fatalf("空库输出不对: %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("只读统计不应初始化数据目录，Stat err=%v", err)
	}
}

// 未选中资产的时间异常不影响本资产；全部查询则整体失败。
func TestDowntimeAnomalyFailsAtomically(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{
		{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusAvailable},
		{ID: "EQ-2", Name: "空调", Location: "二楼", Status: statusAvailable},
	}
	// T0001 关闭履历时间 07:00 早于报修履历时间 08:00（序号仍然合法，台账一致）。
	tickets := []*Ticket{
		closedTicket("T0001", "EQ-1", "卡纸", "req-1", t01, "2026-10-01T07:00:00Z", "已修复"),
	}
	events := []eventJSON{
		ev(1, "EQ-1", "T0001", eventReport, "卡纸", t01),
		ev(2, "EQ-1", "T0001", eventClose, "已修复", "2026-10-01T07:00:00Z"),
	}
	requests := []requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}
	writeDowntimeLedger(t, dir, assets, tickets, events, requests, 5)
	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 即使异常工单完全在窗口外，单项统计也失败，且不输出部分结果。
	code, out, errb := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-05T00:00:00Z", "--end", "2026-10-06T00:00:00Z")
	if code != 1 {
		t.Fatalf("时间异常退出码 = %d，想得到 1", code)
	}
	if out != "" {
		t.Fatalf("失败时不应输出部分结果，得到 %q", out)
	}
	if !strings.Contains(errb, "EQ-1") || !strings.Contains(errb, "T0001") ||
		!strings.Contains(errb, "早于") {
		t.Fatalf("错误应指出资产与工单: %s", errb)
	}
	// 全部查询同样失败。
	if code, _, errb = runDowntime(t, dir, "--start", t01, "--end", t01End); code != 1 {
		t.Fatalf("全部查询遇到异常应失败，code=%d", code)
	}
	if !strings.Contains(errb, "T0001") {
		t.Fatalf("错误应指出工单: %s", errb)
	}
	// 未选中该资产的单项查询照常成功并记零。
	code, out, errb = runDowntime(t, dir, "--asset-id", "EQ-2",
		"--start", t01, "--end", t01End)
	if code != 0 {
		t.Fatalf("未选中异常资产的查询应成功: %s", errb)
	}
	if secs, _ := parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("EQ-2 无停机应为 0，得到 %d", secs)
	}
	// 原文件字节不变；原查询与工单操作不受此统计规则影响。
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的统计不应改动原文件")
	}
	var lbuf, ebuf bytes.Buffer
	if code := run([]string{"list", "--data-dir", dir}, &lbuf, &ebuf); code != 0 {
		t.Fatalf("异常台账上 list 仍应可用: %s", ebuf.String())
	}
	if code := run([]string{"history", "--data-dir", dir, "--asset-id", "EQ-1"}, &lbuf, &ebuf); code != 0 {
		t.Fatalf("异常台账上 history 仍应可用: %s", ebuf.String())
	}
	// 资产可用，允许继续报维修单（工单操作不被统计规则拒绝）。
	if code := run([]string{"report", "--data-dir", dir, "--asset-id", "EQ-1",
		"--description", "新故障", "--request-id", "req-2"}, &lbuf, &ebuf); code != 0 {
		t.Fatalf("异常台账上 report 仍应可用: %s", ebuf.String())
	}
}

// 结束等于开始合法（零长度记零）。
func TestDowntimeEndEqualsStartLegal(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusAvailable}}
	tickets := []*Ticket{closedTicket("T0001", "EQ-1", "卡纸", "req-1", t01, t01, "已修复")}
	events := []eventJSON{
		ev(1, "EQ-1", "T0001", eventReport, "卡纸", t01),
		ev(2, "EQ-1", "T0001", eventClose, "已修复", t01),
	}
	writeDowntimeLedger(t, dir, assets, tickets, events,
		[]requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}, 3)

	code, out, errb := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z")
	if code != 0 {
		t.Fatalf("结束等于开始应合法: %s", errb)
	}
	if secs, _ := parseSingleOutput(t, out); secs != 0 {
		t.Fatalf("零长度工单应记零，得到 %d", secs)
	}
}

// 统计只采信履历时间，忽略工单记录里的重复时间字段。
func TestDowntimeUsesEventTimesNotTicketFields(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusAvailable}}
	// 工单记录上的时间与履历时间不同。
	tickets := []*Ticket{closedTicket("T0001", "EQ-1", "卡纸", "req-1",
		"2026-10-01T00:00:00Z", "2026-10-01T06:00:00Z", "已修复")}
	events := []eventJSON{
		ev(1, "EQ-1", "T0001", eventReport, "卡纸", t01),
		ev(2, "EQ-1", "T0001", eventClose, "已修复", t01End),
	}
	writeDowntimeLedger(t, dir, assets, tickets, events,
		[]requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}, 3)

	_, out, _ := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z")
	if secs, _ := parseSingleOutput(t, out); secs != 7200 {
		t.Fatalf("应以履历时间计得 7200，忽略工单字段，得到 %d", secs)
	}
}

// 有效旧台账（编号/履历序号有间隔）无需转换即可统计。
func TestDowntimeLegacyLedger(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // T0001 未关闭，报修履历 2026-10-01T08:00:00Z，下一序号 5。
	_, out, errb := runDowntime(t, dir, "--asset-id", "EQ-1",
		"--start", "2026-10-01T08:00:00Z", "--end", "2026-10-01T09:00:00Z")
	if secs, total := parseSingleOutput(t, out); secs != 3600 || total != 3600 {
		t.Fatalf("旧库统计 = %d/%d (%s)", secs, total, errb)
	}
}

// 损坏文件：统计先做整库检查，退出 1 并保留原文件。
func TestDowntimeCorruptFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dataFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runDowntime(t, dir, "--start", t01, "--end", t01End)
	if code != 1 {
		t.Fatalf("损坏文件退出码 = %d，想得到 1", code)
	}
	if out != "" || !strings.Contains(errb, "已损坏") {
		t.Fatalf("损坏文件应只在 stderr 报错: out=%q err=%s", out, errb)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "{not json" {
		t.Fatalf("原文件应保留: %q %v", raw, err)
	}
}

// 参数错误退出 2；未知资产退出 1。
func TestDowntimeArgsAndUnknownAsset(t *testing.T) {
	dir := t.TempDir()
	mainLedger(t, dir)
	bad := [][]string{
		{"--end", t01End}, // 缺 --start
		{"--start", t01},  // 缺 --end
		{"--start", "not-a-time", "--end", t01End},      // 起点非法
		{"--start", t01, "--end", "2026-10-01T10:00"},   // 终点缺时区
		{"--start", t01End, "--end", t01},               // 起点晚于终点
		{"--start", t01, "--end", t01},                  // 起点等于终点
		{"--start", t01, "--end", t01End, "positional"}, // 位置参数
	}
	for _, b := range bad {
		code, out, errb := runDowntime(t, dir, b...)
		if code != 2 {
			t.Fatalf("%v 退出码 = %d，想得到 2（out=%s err=%s）", b, code, out, errb)
		}
		if out != "" {
			t.Fatalf("%v 参数错误不应有标准输出", b)
		}
	}
	code, out, errb := runDowntime(t, dir, "--asset-id", "NOPE",
		"--start", t01, "--end", t01End)
	if code != 1 {
		t.Fatalf("未知资产退出码 = %d，想得到 1", code)
	}
	if !strings.Contains(errb, "未知资产") || out != "" {
		t.Fatalf("未知资产错误信息不对: %s / %q", errb, out)
	}
}

// 相同文件与输入在重启（重新打开进程式调用）后结果一致。
func TestDowntimeDeterministicAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	mainLedger(t, dir)
	args := []string{"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-04T00:00:00Z"}
	_, first, _ := runDowntime(t, dir, args...)
	_, second, _ := runDowntime(t, dir, args...)
	if first != second {
		t.Fatalf("重复调用结果应一致:\n%s\n!=\n%s", first, second)
	}
	// 通过 store 层重新打开两次计算也应一致。
	s1, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	en := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	r1, err := s1.downtimeForAssets(s1.allAssetIDs(), st, en)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s2.downtimeForAssets(s2.allAssetIDs(), st, en)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) != len(r2) {
		t.Fatalf("重开后结果条数变化: %d != %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("重开后 %s 结果变化: %+v != %+v", r1[i].AssetID, r1[i], r2[i])
		}
	}
}
