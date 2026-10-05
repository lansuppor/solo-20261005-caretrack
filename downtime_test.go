package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedClock 让履历时间完全可控：每次业务操作内多次取时都返回同一时刻。
type fixedClock struct{ cur time.Time }

func (c *fixedClock) now() time.Time { return c.cur }

func newClockStore(t *testing.T) (*store, *fixedClock) {
	t.Helper()
	s := newTestStore(t)
	clk := &fixedClock{}
	s.now = clk.now
	return s, clk
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("解析时刻 %q: %v", s, err)
	}
	return tm
}

func writeRawLedger(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lineOf(t *testing.T, r *DowntimeResult, assetID string) DowntimeAssetLine {
	t.Helper()
	for _, l := range r.Lines {
		if l.AssetID == assetID {
			return l
		}
	}
	t.Fatalf("结果中缺少资产 %s: %+v", assetID, r.Lines)
	return DowntimeAssetLine{}
}

// 窗口裁剪：整段在外、贴边、跨窗口的交集与半开语义；取消计入；未关闭暂算到终点。
func TestDowntimeWindowClipping(t *testing.T) {
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// T0001：08:00 报修，09:00 关闭 -> [08:00,09:00)。
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	// T0002：10:00 报修后保持未关闭 -> [10:00, 窗口终点)。
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		start, end  string
		wantSeconds int64
	}{
		{"窗口整体包住两段，未关闭算到终点", "2026-10-01T00:00:00Z", "2026-10-01T11:00:00Z", 3600 + 3600},
		{"窗口 [08:30,11:00) 裁剪 T0001 前部、T0002 后部", "2026-10-01T08:30:00Z", "2026-10-01T11:00:00Z", 1800 + 3600},
		{"恰好等于终结区间 [08:00,09:00)", "2026-10-01T08:00:00Z", "2026-10-01T09:00:00Z", 3600},
		{"终点贴 T0001 起点、起点贴 T0002 终点，均为零", "2026-10-01T09:00:00Z", "2026-10-01T10:00:00Z", 0},
		{"窗口整体早于全部区间", "2026-10-01T00:00:00Z", "2026-10-01T08:00:00Z", 0},
		{"未关闭在窗口之前报修，从窗口起点算到终点", "2026-10-01T11:00:00Z", "2026-10-01T12:00:00Z", 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.downtimeStats("EQ-1", mustTime(t, tc.start), mustTime(t, tc.end))
			if err != nil {
				t.Fatalf("统计失败: %v", err)
			}
			if got := r.Lines[0].Seconds; got != tc.wantSeconds {
				t.Fatalf("停机秒数 = %d, 想得到 %d", got, tc.wantSeconds)
			}
		})
	}
	// 窗口内无任何工单的资产（注意不能用 EQ-1：它的 T0002 未关闭，会暂算到窗口终点）。
	if _, err := s.registerAsset("EQ-9", "监控", "四楼"); err != nil {
		t.Fatal(err)
	}
	rZero, err := s.downtimeStats("EQ-9",
		mustTime(t, "2026-10-02T00:00:00Z"), mustTime(t, "2026-10-02T01:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if rZero.Lines[0].Seconds != 0 {
		t.Fatalf("无工单资产应记零，得到 %d", rZero.Lines[0].Seconds)
	}

	// 未关闭工单在终点或终点之后报修：记零。
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T12:00:00Z")
	if _, _, err := s.report("EQ-2", "不制冷", "req-3"); err != nil {
		t.Fatal(err)
	}
	r, err := s.downtimeStats("EQ-2",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-01T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Lines[0].Seconds != 0 {
		t.Fatalf("未关闭工单在终点报修应记零，得到 %d", r.Lines[0].Seconds)
	}

	// 取消前的占用计入；派工、转派不另起区间、不影响时长。
	s2, clk2 := newClockStore(t)
	if _, err := s2.registerAsset("EQ-3", "电梯", "三楼"); err != nil {
		t.Fatal(err)
	}
	clk2.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s2.report("EQ-3", "异响", "req-4"); err != nil {
		t.Fatal(err)
	}
	clk2.cur = mustTime(t, "2026-10-01T08:30:00Z")
	if _, err := s2.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	clk2.cur = mustTime(t, "2026-10-01T09:30:00Z")
	if _, err := s2.assignTicket("T0001", "李四", "转派"); err != nil {
		t.Fatal(err)
	}
	clk2.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s2.cancelTicket("T0001", "误报，设备实际正常"); err != nil {
		t.Fatal(err)
	}
	r2, err := s2.downtimeStats("EQ-3",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r2.Lines[0].Seconds; got != 7200 {
		t.Fatalf("取消前占用应计入 [08:00,10:00)=7200，得到 %d", got)
	}
}

// 跨时区：不同时区表示的同一窗口与同一批时刻结果相同；窗口按实际瞬间比较。
func TestDowntimeCrossTimezone(t *testing.T) {
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}

	z := func(start, end string) int64 {
		r, err := s.downtimeStats("EQ-1", mustTime(t, start), mustTime(t, end))
		if err != nil {
			t.Fatal(err)
		}
		return r.Lines[0].Seconds
	}
	want := int64(1800) // 窗口 [08:30,09:00)
	utc := z("2026-10-01T08:30:00Z", "2026-10-01T09:00:00Z")
	plus8 := z("2026-10-01T16:30:00+08:00", "2026-10-01T17:00:00+08:00")
	minus5 := z("2026-10-01T03:30:00-05:00", "2026-10-01T04:00:00-05:00")
	if utc != want || plus8 != want || minus5 != want {
		t.Fatalf("同一窗口跨时区表示结果应一致: utc=%d +08=%d -05=%d want=%d", utc, plus8, minus5, want)
	}
}

// 同一资产不同工单因时间逆序重叠：交集取并集，不能直接相加。
func TestDowntimeOverlappingUnion(t *testing.T) {
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// 履历序号顺序合法，但时刻逆序重叠：
	// seq: T0001 报修(08:00) -> T0001 关闭(12:00) -> T0002 报修(10:00) -> T0002 关闭(11:00)
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T12:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T11:00:00Z")
	if _, _, err := s.closeTicket("T0002", "已更换电源"); err != nil {
		t.Fatal(err)
	}

	r, err := s.downtimeStats("",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	got := lineOf(t, r, "EQ-1").Seconds
	if got != 4*3600 {
		t.Fatalf("重叠区间应取并集 [08:00,12:00)=14400，直接相加会得到 18000，实际 %d", got)
	}

	// 与窗口裁剪叠加：窗口 [09:00,11:30)，并集 [08,12) 裁剪后 [09,11:30)=9000。
	r2, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T09:00:00Z"), mustTime(t, "2026-10-01T11:30:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r2.Lines[0].Seconds; got != 9000 {
		t.Fatalf("裁剪后并集应为 9000，得到 %d", got)
	}
}

// shuffledLegacyLedger：编号与履历序号有间隔、数组乱序、时间不按数组顺序；
// 工单 closed_at（11:00）与关闭履历时间（12:00）不同，统计必须采用履历时间。
const shuffledLegacyLedger = `{
  "version": 1,
  "assets": [
    {"id":"EQ-2","name":"空调","location":"二楼","status":"可用"},
    {"id":"EQ-1","name":"打印机","location":"一楼","status":"可用"}
  ],
  "tickets": [
    {"id":"T0007","asset_id":"EQ-2","description":"不制冷","request_id":"req-7","status":"已取消","created_at":"2026-10-01T08:30:00Z","cancel_reason":"误报","cancelled_at":"2026-10-01T10:00:00Z"},
    {"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"已关闭","created_at":"2026-10-01T08:00:00Z","result":"已修复","closed_at":"2026-10-01T11:00:00Z"}
  ],
  "events": [
    {"seq":9,"asset_id":"EQ-1","ticket_id":"T0001","kind":"关闭","content":"已修复","time":"2026-10-01T12:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":8,"asset_id":"EQ-2","ticket_id":"T0007","kind":"取消","content":"误报","time":"2026-10-01T10:00:00Z"},
    {"seq":2,"asset_id":"EQ-2","ticket_id":"T0007","kind":"报修","content":"不制冷","time":"2026-10-01T08:30:00Z"}
  ],
  "requests": [
    {"request_id":"req-7","asset_id":"EQ-2","description":"不制冷","ticket_id":"T0007"},
    {"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}
  ],
  "next_ticket_seq": 12
}`

// 有效旧台账无需转换：间隔、乱序、取消单、重复时间字段不一致都按履历序号配对。
func TestDowntimeLegacyShuffledLedger(t *testing.T) {
	dir := t.TempDir()
	writeRawLedger(t, dir, shuffledLegacyLedger)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧台账应能加载: %v", err)
	}
	r, err := s.downtimeStats("",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	// 字典序输出：EQ-1 用关闭履历时间 12:00 -> 4h（不是 closed_at 的 3h）；
	// EQ-2 取消单 [08:30,10:00) -> 1.5h。
	want := []DowntimeAssetLine{
		{AssetID: "EQ-1", Name: "打印机", Seconds: 14400},
		{AssetID: "EQ-2", Name: "空调", Seconds: 5400},
	}
	if len(r.Lines) != 2 {
		t.Fatalf("应有两行，得到 %+v", r.Lines)
	}
	for i, w := range want {
		if r.Lines[i].AssetID != w.AssetID || r.Lines[i].Seconds != w.Seconds {
			t.Fatalf("第 %d 行 = %+v，想得到 %+v", i, r.Lines[i], w)
		}
	}
	if r.TotalSeconds != 19800 {
		t.Fatalf("合计 = %d，想得到 19800", r.TotalSeconds)
	}
	// 单项查询同样按履历时间配对。
	r1, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Lines[0].Seconds != 14400 {
		t.Fatalf("EQ-1 应按关闭履历时间算 14400，得到 %d", r1.Lines[0].Seconds)
	}
}

// 小数秒：履历与窗口可含小数秒，按实际瞬间裁剪，合计向下取整为整秒。
func TestDowntimeFractionalSecondsFloor(t *testing.T) {
	openFractionLedger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00.2Z"}],
  "events": [{"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00.2Z"}],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "next_ticket_seq": 5
}`
	dir := t.TempDir()
	writeRawLedger(t, dir, openFractionLedger)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("含小数秒的台账应能加载: %v", err)
	}
	// 未关闭：[08:00:00.2, 08:00:02.8) = 2.6s -> 向下取整 2。
	r, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T08:00:00.2Z"), mustTime(t, "2026-10-01T08:00:02.8Z"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Lines[0].Seconds != 2 {
		t.Fatalf("2.6s 应向下取整为 2，得到 %d", r.Lines[0].Seconds)
	}
	// 同一窗口用 +08:00 带小数秒表示，结果一致。
	r2, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T16:00:00.2+08:00"), mustTime(t, "2026-10-01T16:00:02.8+08:00"))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Lines[0].Seconds != 2 {
		t.Fatalf("跨时区小数秒窗口结果应为 2，得到 %d", r2.Lines[0].Seconds)
	}

	// 终结工单 0.7s -> 0；1.9s -> 1（均向下取整）。
	closedFraction := func(reportAt, closeAt string) string {
		return `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"可用"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"已关闭","created_at":"` + reportAt + `","result":"已修复","closed_at":"` + closeAt + `"}],
  "events": [
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"` + reportAt + `"},
    {"seq":4,"asset_id":"EQ-1","ticket_id":"T0001","kind":"关闭","content":"已修复","time":"` + closeAt + `"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "next_ticket_seq": 5
}`
	}
	dir2 := t.TempDir()
	writeRawLedger(t, dir2, closedFraction("2026-10-01T08:00:00.2Z", "2026-10-01T08:00:00.9Z"))
	s2, err := openStore(dir2)
	if err != nil {
		t.Fatal(err)
	}
	r3, err := s2.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Lines[0].Seconds != 0 {
		t.Fatalf("0.7s 应向下取整为 0，得到 %d", r3.Lines[0].Seconds)
	}
	dir3 := t.TempDir()
	writeRawLedger(t, dir3, closedFraction("2026-10-01T08:00:00.2Z", "2026-10-01T08:00:02.1Z"))
	s3, err := openStore(dir3)
	if err != nil {
		t.Fatal(err)
	}
	r4, err := s3.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if r4.Lines[0].Seconds != 1 {
		t.Fatalf("1.9s 应向下取整为 1，得到 %d", r4.Lines[0].Seconds)
	}
}

// 终结工单结束早于开始：即使完全在窗口外，整次统计也失败，指出资产与工单；
// 结束等于开始合法。该规则只用于统计，不影响原有查询与工单操作。
func TestDowntimeTerminalEndBeforeStart(t *testing.T) {
	// EQ-1 正常；EQ-2 已关闭但关闭时刻早于报修时刻；EQ-3 已取消但取消时刻早于报修。
	s, clk := newClockStore(t)
	for _, id := range []string{"EQ-1", "EQ-2", "EQ-3"} {
		if _, err := s.registerAsset(id, "设备-"+id, "一楼"); err != nil {
			t.Fatal(err)
		}
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "故障1", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.report("EQ-2", "故障2", "req-2"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z") // 时刻逆序，但履历序号仍递增，台账一致
	if _, _, err := s.closeTicket("T0002", "已修复"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.report("EQ-3", "故障3", "req-3"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:30:00Z")
	if _, _, err := s.cancelTicket("T0003", "误报"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	// 单项查询正常资产不受影响。
	r, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatalf("正常资产统计不应受他资产时间异常影响: %v", err)
	}
	if r.Lines[0].Seconds != 3600 {
		t.Fatalf("EQ-1 应为 3600，得到 %d", r.Lines[0].Seconds)
	}

	// 指定异常资产：任意窗口（含完全不覆盖该工单的窗口）都失败，指出资产与工单。
	for _, win := range [][2]string{
		{"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z"},
		{"2026-12-01T00:00:00Z", "2026-12-02T00:00:00Z"}, // 窗口外
	} {
		_, err := s.downtimeStats("EQ-2", mustTime(t, win[0]), mustTime(t, win[1]))
		if err == nil || !strings.Contains(err.Error(), "EQ-2") || !strings.Contains(err.Error(), "T0002") {
			t.Fatalf("EQ-2 时间逆序应失败并指出资产与工单，得到 %v", err)
		}
	}
	_, err = s.downtimeStats("EQ-3",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "EQ-3") || !strings.Contains(err.Error(), "T0003") {
		t.Fatalf("EQ-3 取消时刻逆序也应失败，得到 %v", err)
	}

	// 全部查询：任一所选资产异常即整体失败。
	_, err = s.downtimeStats("",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err == nil || !strings.Contains(err.Error(), "EQ-2") {
		t.Fatalf("全部查询应被异常资产阻断，得到 %v", err)
	}

	// 原有查询与工单操作不受影响：list/history 仍可用，新资产可正常报修流转。
	if a := s.findAsset("EQ-2"); a == nil {
		t.Fatal("EQ-2 应仍可查询")
	}
	if _, err := s.registerAsset("EQ-4", "设备-EQ-4", "二楼"); err != nil {
		t.Fatalf("时间异常不应影响登记: %v", err)
	}
	clk.cur = mustTime(t, "2026-10-03T08:00:00Z")
	tk, _, err := s.report("EQ-4", "故障4", "req-4")
	if err != nil {
		t.Fatalf("时间异常不应影响其他资产报修: %v", err)
	}
	clk.cur = mustTime(t, "2026-10-03T09:00:00Z")
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatalf("时间异常不应影响关闭: %v", err)
	}
}

// 结束等于开始合法：零长度区间不贡献，统计成功。
func TestDowntimeTerminalEndEqualsStart(t *testing.T) {
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z") // 关闭时刻 == 报修时刻
	if _, _, err := s.closeTicket("T0001", "误报修复"); err != nil {
		t.Fatal(err)
	}
	r, err := s.downtimeStats("EQ-1",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatalf("结束等于开始应合法: %v", err)
	}
	if r.Lines[0].Seconds != 0 {
		t.Fatalf("零长度区间应记零，得到 %d", r.Lines[0].Seconds)
	}
}

// 统计为纯读取：空库不初始化目录；失败不输出部分结果、不改文件；损坏台账被拒绝。
func TestDowntimeReadOnlyAndFileProtection(t *testing.T) {
	// 空库（目录尚不存在）：成功输出零合计，且不创建目录或文件。
	missing := filepath.Join(t.TempDir(), "not-created")
	var out, errBuf bytes.Buffer
	code := run([]string{"downtime", "--data-dir", missing,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("空库查询应成功，退出码 %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "没有记录") || !strings.Contains(out.String(), "停机合计: 0 秒") {
		t.Fatalf("空库应提示无记录并显示零合计: %s", out.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("只读统计不应初始化数据目录，Stat = %v", err)
	}

	// 时间逆序：CLI 退出 1，stdout 无部分结果，stderr 指出资产与工单，文件字节不变。
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, dataFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errBuf.Reset()
	code = run([]string{"downtime", "--data-dir", s.dir,
		"--start", "2026-12-01T00:00:00Z", "--end", "2026-12-02T00:00:00Z", // 窗口外
		"--asset-id", "EQ-1"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("时间逆序退出码应为 1，得到 %d", code)
	}
	if out.Len() != 0 {
		t.Fatalf("失败时不应输出部分结果，stdout = %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "EQ-1") || !strings.Contains(errBuf.String(), "T0001") {
		t.Fatalf("错误应指出资产与工单: %s", errBuf.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("统计失败不应改动数据文件")
	}

	// 损坏/矛盾台账：退出 1 并保留原文件。
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) { m["next_ticket_seq"] = 1 })
	out.Reset()
	errBuf.Reset()
	code = run([]string{"downtime", "--data-dir", dir,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"}, &out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "计数器矛盾") {
		t.Fatalf("矛盾台账应退出 1 并指出问题，code=%d err=%s", code, errBuf.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的统计应保留原文件字节")
	}
}

// 全部查询字典序、零停机资产显示零、合计为各资产秒数之和；单项查询与未知资产。
func TestDowntimeCLIFormatAndSorting(t *testing.T) {
	s, clk := newClockStore(t)
	// 登记顺序故意乱序：EQ-3、EQ-1、EQ-2。
	if _, err := s.registerAsset("EQ-3", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-3", "不制冷", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T10:00:00Z")
	if _, _, err := s.closeTicket("T0001", "已加氟"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-2"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:00:00Z")
	if _, _, err := s.closeTicket("T0002", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "电梯", "三楼"); err != nil {
		t.Fatal(err) // 无工单 -> 0
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := run([]string{"downtime", "--data-dir", s.dir,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("全部查询应成功: %s", errBuf.String())
	}
	text := out.String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	// 末行合计；前面为表头 + 3 行资产。
	if !strings.HasSuffix(text, "停机合计: 10800 秒\n") {
		t.Fatalf("合计应为 10800 秒，输出: %s", text)
	}
	wantRows := []string{"EQ-1\t打印机\t3600 秒", "EQ-2\t电梯\t0 秒", "EQ-3\t空调\t7200 秒"}
	for i, w := range wantRows {
		if lines[i+1] != w {
			t.Fatalf("第 %d 行 = %q，想得到 %q（全部输出:\n%s）", i+1, lines[i+1], w, text)
		}
	}

	// 单项查询：编号、名称、秒数；无停机也显示零。
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"EQ-1", "停机时长: 3600 秒"},
		{"EQ-2", "停机时长: 0 秒"},
	} {
		out.Reset()
		code = run([]string{"downtime", "--data-dir", s.dir,
			"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z",
			"--asset-id", tc.id}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("单项查询 %s 应成功: %s", tc.id, errBuf.String())
		}
		if !strings.Contains(out.String(), "资产编号: "+tc.id) ||
			!strings.Contains(out.String(), tc.want) {
			t.Fatalf("单项查询 %s 输出不符:\n%s", tc.id, out.String())
		}
	}

	// 未知资产：退出 1 并说明原因。
	out.Reset()
	errBuf.Reset()
	code = run([]string{"downtime", "--data-dir", s.dir,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z",
		"--asset-id", "NOPE"}, &out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "未知资产编号") {
		t.Fatalf("未知资产应退出 1 并说明，code=%d err=%s", code, errBuf.String())
	}
	if out.Len() != 0 {
		t.Fatal("未知资产不应有标准输出")
	}
}

// 参数错误退出 2。
func TestDowntimeUsageErrors(t *testing.T) {
	dir := t.TempDir()
	base := []string{"downtime", "--data-dir", dir}
	cases := [][]string{
		{"--end", "2026-10-02T00:00:00Z"},                                    // 缺 --start
		{"--start", "2026-10-01T00:00:00Z"},                                  // 缺 --end
		{"--start", "not-a-time", "--end", "2026-10-02T00:00:00Z"},           // 起点格式错
		{"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-01T00:00:00Z"}, // 起点 == 终点
		{"--start", "2026-10-02T00:00:00Z", "--end", "2026-10-01T00:00:00Z"}, // 起点晚于终点
		{"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z", "多余位置参数"},
		{"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z", "--unknown-flag", "x"},
	}
	for i, extra := range cases {
		var out, errBuf bytes.Buffer
		args := append(append([]string{}, base...), extra...)
		if code := run(args, &out, &errBuf); code != 2 {
			t.Fatalf("用例 %d 退出码 = %d，应为 2（%s）", i, code, errBuf.String())
		}
	}
	// 子命令帮助仍退出 0。
	var out, errBuf bytes.Buffer
	if code := run([]string{"downtime", "--help"}, &out, &errBuf); code != 0 {
		t.Fatalf("downtime --help 退出码 = %d", code)
	}
}

// 相同文件与输入在重启后结果一致：保存前后、两次 CLI 调用输出完全相同。
func TestDowntimeDeterministicAcrossRestart(t *testing.T) {
	s, clk := newClockStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T08:00:00Z")
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	clk.cur = mustTime(t, "2026-10-01T09:30:00Z")
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	args := []string{"downtime", "--data-dir", s.dir,
		"--start", "2026-10-01T00:00:00Z", "--end", "2026-10-02T00:00:00Z"}

	before, err := s.downtimeStats("",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s2.downtimeStats("",
		mustTime(t, "2026-10-01T00:00:00Z"), mustTime(t, "2026-10-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Lines) != len(after.Lines) ||
		before.Lines[0].Seconds != after.Lines[0].Seconds ||
		before.TotalSeconds != after.TotalSeconds {
		t.Fatalf("重启后统计不一致: before=%+v after=%+v", before, after)
	}

	var o1, e1, o2, e2 bytes.Buffer
	if code := run(args, &o1, &e1); code != 0 {
		t.Fatalf("第一次查询失败: %s", e1.String())
	}
	if code := run(args, &o2, &e2); code != 0 {
		t.Fatalf("第二次查询失败: %s", e2.String())
	}
	if o1.String() != o2.String() {
		t.Fatalf("相同文件与输入输出应完全一致:\n%s\n---\n%s", o1.String(), o2.String())
	}
}
