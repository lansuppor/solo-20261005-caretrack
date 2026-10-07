package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 报修与搬移交错：维修中搬移不改旧单报修地点，搬移后新报修采用新地点，
// 旧请求重放返回原工单且不改地点、不追加履历；搬移不改变资产与工单状态、
// 负责人、请求绑定等其他记录。
func TestRelocateInterleavedWithTickets(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼大厅"); err != nil {
		t.Fatal(err)
	}
	t1, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || replay {
		t.Fatalf("首次报修: %v replay=%v", t1, replay)
	}
	if t1.ReportLocation != "一楼大厅" {
		t.Fatalf("T0001 报修地点 = %q，想得到 一楼大厅", t1.ReportLocation)
	}

	// 维修中也可以搬移。
	a, from, err := s.relocateAsset("EQ-1", "二楼维修间", "搬去检修")
	if err != nil {
		t.Fatalf("维修中搬移应成功: %v", err)
	}
	if from != "一楼大厅" || a.Location != "二楼维修间" || a.Status != statusRepairing {
		t.Fatalf("搬移结果异常: from=%q asset=%+v", from, a)
	}
	// 旧单报修地点不变，工单仍未关闭，负责人/绑定不变。
	if got := s.findTicket("T0001"); got.ReportLocation != "一楼大厅" || got.Status != ticketOpen {
		t.Fatalf("搬移不应改旧单地点或状态: %+v", got)
	}

	// 旧请求重放：返回原工单，不改地点、不追加履历。
	eventsBefore := len(s.eventsOf("EQ-1"))
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.ReportLocation != "一楼大厅" {
		t.Fatalf("重放应返回原工单及原地点: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.eventsOf("EQ-1")) != eventsBefore {
		t.Fatal("重放不应追加履历")
	}

	// 关闭旧单后新报修：新单地点取当前位置。
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	t2, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay {
		t.Fatalf("新报修: %v replay=%v err=%v", t2, replay, err)
	}
	if t2.ReportLocation != "二楼维修间" {
		t.Fatalf("T0002 报修地点 = %q，想得到 二楼维修间", t2.ReportLocation)
	}

	// 已取消工单的报修地点同样保持：取消后再搬，地点仍是当时位置。
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "三楼库房", "转库房"); err != nil {
		t.Fatal(err)
	}
	if got := s.findTicket("T0002"); got.ReportLocation != "二楼维修间" || got.Status != ticketCancelled {
		t.Fatalf("已取消工单地点应保持: %+v", got)
	}
	if got := s.findTicket("T0001"); got.ReportLocation != "一楼大厅" || got.Status != ticketClosed {
		t.Fatalf("已关闭工单地点应保持: %+v", got)
	}

	// 履历按全库序号：报修、位置变更、关闭、报修、取消、位置变更。
	events := s.eventsOf("EQ-1")
	wantKinds := []string{
		eventReport, eventRelocate, eventClose, eventReport, eventCancel, eventRelocate,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("履历条数 = %d，想得到 %d", len(events), len(wantKinds))
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Fatalf("第 %d 条履历 = %q，想得到 %q（全部: %+v）", i, events[i].Kind, k, events)
		}
	}
	if events[1].From != "一楼大厅" || events[1].To != "二楼维修间" || events[1].Content != "搬去检修" {
		t.Fatalf("位置变更履历内容不对: %+v", events[1])
	}
	if events[5].From != "二楼维修间" || events[5].To != "三楼库房" {
		t.Fatalf("第二次位置变更履历不对: %+v", events[5])
	}

	// 保存重开后地点、当前位置与履历全部保持。
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1"); got.Location != "三楼库房" {
		t.Fatalf("重开后当前位置 = %q", got.Location)
	}
	if got := s.findTicket("T0001"); got.ReportLocation != "一楼大厅" {
		t.Fatalf("重开后 T0001 地点 = %q", got.ReportLocation)
	}
	if got := s.findTicket("T0002"); got.ReportLocation != "二楼维修间" {
		t.Fatalf("重开后 T0002 地点 = %q", got.ReportLocation)
	}
}

// 停用资产同样可以搬移，搬移不改变停用状态；停用期间新报修仍被拒绝，
// 恢复使用后的报修采用新地点。
func TestRelocateWhileDeactivated(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.relocateAsset("EQ-1", "地下室", "随调拨搬移")
	if err != nil {
		t.Fatalf("停用资产应可搬移: %v", err)
	}
	if a.Status != statusDeactivated || a.Location != "地下室" {
		t.Fatalf("搬移后资产应为停用/地下室: %+v", a)
	}
	if _, _, err := s.report("EQ-1", "坏了", "req-x"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间新报修仍应拒绝，得到 %v", err)
	}
	if _, err := s.reactivateAsset("EQ-1", "调回"); err != nil {
		t.Fatal(err)
	}
	tk, replay, err := s.report("EQ-1", "坏了", "req-x")
	if err != nil || replay || tk.ReportLocation != "地下室" {
		t.Fatalf("恢复后报修地点应为地下室: %v replay=%v err=%v", tk, replay, err)
	}
}

// 位置变更的各类拒绝：未知资产、空位置、空理由、新旧位置相同。
// 失败路径不更新位置、不产生履历、不消耗序号。
func TestRelocateRejects(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	maxSeq := func() int {
		m := 0
		for _, e := range s.data.Events {
			if e.Seq > m {
				m = e.Seq
			}
		}
		return m
	}
	cases := []struct {
		name     string
		assetID  string
		location string
		reason   string
		want     error
	}{
		{"未知资产", "NOPE", "二楼", "理由", errNotFound},
		{"空新位置", "EQ-1", "", "理由", errConflict},
		{"空理由", "EQ-1", "二楼", "", errConflict},
		{"位置相同", "EQ-1", "一楼", "理由", errConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.eventsOf("EQ-1"))
			seqBefore := maxSeq()
			_, _, err := s.relocateAsset(tc.assetID, tc.location, tc.reason)
			if !errors.Is(err, tc.want) {
				t.Fatalf("应失败 %v，得到 %v", tc.want, err)
			}
			if len(s.eventsOf("EQ-1")) != before || maxSeq() != seqBefore {
				t.Fatal("失败的变更不应产生履历或消耗序号")
			}
			if a := s.findAsset("EQ-1"); a != nil && a.Location != "一楼" {
				t.Fatalf("失败的变更不应改动位置，得到 %q", a.Location)
			}
		})
	}
}

// 命令行入口：成功输出资产编号、原位置和新位置；参数错误退出 2，
// 业务失败退出 1。
func TestRelocateCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer
	if code := run([]string{"register", "--data-dir", dir,
		"--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼大厅"}, &out, &errBuf); code != 0 {
		t.Fatalf("register: %s", errBuf.String())
	}
	out.Reset()
	if code := run([]string{"relocate", "--data-dir", dir,
		"--asset-id", "EQ-1", "--location", "二楼维修间", "--reason", "搬去检修"}, &out, &errBuf); code != 0 {
		t.Fatalf("relocate 应成功: %s", errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "EQ-1") || !strings.Contains(got, "原位置: 一楼大厅") ||
		!strings.Contains(got, "新位置: 二楼维修间") {
		t.Fatalf("成功输出应含资产编号、原位置与新位置: %s", got)
	}

	// list 与 detail 显示当前位置。
	out.Reset()
	if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("list: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "二楼维修间") {
		t.Fatalf("list 应显示当前位置: %s", out.String())
	}
	out.Reset()
	if code := run([]string{"detail", "--data-dir", dir, "--asset-id", "EQ-1"}, &out, &errBuf); code != 0 {
		t.Fatalf("detail: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "位置: 二楼维修间") {
		t.Fatalf("detail 应显示当前位置: %s", out.String())
	}

	// 相同位置拒绝（退出码 1）。
	errBuf.Reset()
	if code := run([]string{"relocate", "--data-dir", dir,
		"--asset-id", "EQ-1", "--location", "二楼维修间", "--reason", "再搬一次"}, &out, &errBuf); code != 1 {
		t.Fatalf("相同位置应退出 1，得到 %d", code)
	}
	if !strings.Contains(errBuf.String(), "相同位置") {
		t.Fatalf("应说明相同位置，得到 %s", errBuf.String())
	}
	// 缺少必填参数（退出码 2）。
	errBuf.Reset()
	if code := run([]string{"relocate", "--data-dir", dir,
		"--asset-id", "EQ-1", "--reason", "缺位置"}, &out, &errBuf); code != 2 {
		t.Fatalf("缺 --location 应退出 2，得到 %d", code)
	}
	// 未知资产（退出码 1）。
	errBuf.Reset()
	if code := run([]string{"relocate", "--data-dir", dir,
		"--asset-id", "NOPE", "--location", "二楼", "--reason", "理由"}, &out, &errBuf); code != 1 {
		t.Fatalf("未知资产应退出 1，得到 %d", code)
	}
}

// ticket 查询显示报修地点，适用于未关闭、已关闭与已取消工单；
// history 按全库序号展示位置变更事件。
func TestTicketAndHistoryShowLocations(t *testing.T) {
	dir := t.TempDir()
	run0 := func(argv []string) string {
		var out, errBuf bytes.Buffer
		if code := run(argv, &out, &errBuf); code != 0 {
			t.Fatalf("%v 失败: %s", argv, errBuf.String())
		}
		return out.String()
	}
	must := func(argv ...string) { run0(argv) }
	must("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	must("report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")
	must("relocate", "--data-dir", dir, "--asset-id", "EQ-1", "--location", "二楼", "--reason", "搬移")
	must("cancel", "--data-dir", dir, "--ticket-id", "T0001", "--reason", "误报")
	must("report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "无法开机", "--request-id", "req-2")
	must("close", "--data-dir", dir, "--ticket-id", "T0002", "--repair-result", "已修复")

	out1 := run0([]string{"ticket", "--data-dir", dir, "--ticket-id", "T0001"})
	if !strings.Contains(out1, "报修地点: 一楼") {
		t.Fatalf("已取消旧单应显示原报修地点一楼: %s", out1)
	}
	out2 := run0([]string{"ticket", "--data-dir", dir, "--ticket-id", "T0002"})
	if !strings.Contains(out2, "报修地点: 二楼") {
		t.Fatalf("搬移后新单应显示报修地点二楼: %s", out2)
	}

	hist := run0([]string{"history", "--data-dir", dir, "--asset-id", "EQ-1"})
	idxRelocate := strings.Index(hist, "位置变更: 一楼 -> 二楼（搬移）")
	idxReport2 := strings.Index(hist, "报修 工单 T0002")
	if idxRelocate < 0 || idxReport2 < 0 || idxRelocate > idxReport2 {
		t.Fatalf("history 应按序号先展示位置变更再展示 T0002 报修: %s", hist)
	}
}

// 没有位置履历的有效旧库：工单无报修地点字段，以保存位置作为起点，无需转换；
// 查询不补写数据；首次变更保存成功后，旧单仍无该字段且地点还原为起点，
// 新报修采用新地点。
func TestLegacyStoreWithoutLocationHistory(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // EQ-1 一楼，T0001 未关闭，无 report_location。

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧库应能加载: %v", err)
	}
	if got := s.findTicket("T0001"); got.ReportLocation != "" {
		t.Fatalf("旧库工单不应被补写报修地点，得到 %q", got.ReportLocation)
	}
	// 查询展示按起点推导。
	var out, errBuf bytes.Buffer
	if code := run([]string{"ticket", "--data-dir", dir, "--ticket-id", "T0001"}, &out, &errBuf); code != 0 {
		t.Fatalf("旧库 ticket 查询应可用: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "报修地点: 一楼") {
		t.Fatalf("旧库应按起点显示报修地点一楼: %s", out.String())
	}

	// 维修中搬移（旧库 T0001 未关闭），保存成功。
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "搬移"); err != nil {
		t.Fatalf("旧库首次搬移应成功: %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	// 旧单仍无报修地点字段（查询不补写、保存不补写），展示仍还原为起点一楼；
	// 当前位置为二楼。
	if got := s2.findTicket("T0001"); got.ReportLocation != "" {
		t.Fatalf("旧单不应被补写报修地点，得到 %q", got.ReportLocation)
	}
	if got := s2.reportLocationOf(s2.findTicket("T0001")); got != "一楼" {
		t.Fatalf("旧单报修地点应还原为一楼，得到 %q", got)
	}
	if got := s2.findAsset("EQ-1").Location; got != "二楼" {
		t.Fatalf("当前位置应为二楼，得到 %q", got)
	}
	// 搬移后的新报修采用新地点并显式写入字段。
	if _, _, err := s2.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	t2, replay, err := s2.report("EQ-1", "新故障", "req-2")
	if err != nil || replay || t2.ID != "T0005" || t2.ReportLocation != "二楼" {
		t.Fatalf("新报修应为 T0005 且地点二楼: %v replay=%v err=%v", t2, replay, err)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	raw := readFileBytes(t, dir)
	if !bytes.Contains(raw, []byte(`"report_location": "二楼"`)) {
		t.Fatalf("新单应显式保存报修地点二楼")
	}
	if bytes.Contains(raw, []byte(`"report_location": "一楼"`)) {
		t.Fatal("旧单不应被补写报修地点字段")
	}
}

// shuffledLocationLedger 构造一份事件数组乱序、序号有间隔、时间不递增的
// 合法台账：EQ-1 当前“维修中/三楼”；T0001 在一楼报修并已关闭，
// T0002 在三楼报修仍未关闭。
func shuffledLocationLedger(t *testing.T, dir string) {
	t.Helper()
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "三楼", "status": "维修中"},
		},
		"tickets": []any{
			map[string]any{
				"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-1",
				"status": "已关闭", "report_location": "一楼",
				"result": "已修复", "closed_at": "2026-10-01T09:30:00Z",
				"created_at": "2026-10-01T08:00:00Z",
			},
			map[string]any{
				"id": "T0002", "asset_id": "EQ-1", "description": "无法开机", "request_id": "req-2",
				"status": "未关闭", "report_location": "三楼",
				"created_at": "2026-10-01T11:00:00Z",
			},
		},
		"events": []any{
			// 故意乱序、时间逆序、序号有间隔（缺 4、6-9）。
			map[string]any{"seq": 10, "asset_id": "EQ-1", "ticket_id": "T0002", "kind": "报修", "content": "无法开机", "time": "2026-10-01T07:30:00Z"},
			map[string]any{"seq": 5, "asset_id": "EQ-1", "kind": "位置变更", "content": "再搬", "from": "二楼", "to": "三楼", "time": "2026-10-01T06:00:00Z"},
			map[string]any{"seq": 3, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "关闭", "content": "已修复", "time": "2026-10-01T09:30:00Z"},
			map[string]any{"seq": 2, "asset_id": "EQ-1", "kind": "位置变更", "content": "先搬", "from": "一楼", "to": "二楼", "time": "2026-10-01T10:00:00Z"},
			map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "报修", "content": "卡纸", "time": "2026-10-01T12:00:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
			map[string]any{"request_id": "req-2", "asset_id": "EQ-1", "description": "无法开机", "ticket_id": "T0002"},
		},
		"next_ticket_seq": 3,
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 数组乱序、序号间隔、时间不递增的位置履历仍合法；地点严格按全库序号推导。
func TestShuffledLocationEventsValid(t *testing.T) {
	dir := t.TempDir()
	shuffledLocationLedger(t, dir)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序位置履历的合法台账应能加载: %v", err)
	}
	if got := s.findTicket("T0001").ReportLocation; got != "一楼" {
		t.Fatalf("T0001 地点 = %q，想得到 一楼", got)
	}
	if got := s.findTicket("T0002").ReportLocation; got != "三楼" {
		t.Fatalf("T0002 地点 = %q，想得到 三楼", got)
	}
	if got := s.findAsset("EQ-1").Location; got != "三楼" {
		t.Fatalf("当前位置 = %q，想得到 三楼", got)
	}
	// history 仍按全库序号排列：报修 T1、搬一楼->二楼、关闭、搬二楼->三楼、报修 T2。
	events := s.eventsOf("EQ-1")
	want := []struct {
		kind string
		from string
		to   string
	}{
		{eventReport, "", ""},
		{eventRelocate, "一楼", "二楼"},
		{eventClose, "", ""},
		{eventRelocate, "二楼", "三楼"},
		{eventReport, "", ""},
	}
	if len(events) != len(want) {
		t.Fatalf("履历条数 = %d，想得到 %d", len(events), len(want))
	}
	for i, w := range want {
		if events[i].Kind != w.kind || events[i].From != w.from || events[i].To != w.to {
			t.Fatalf("第 %d 条履历不对: %+v，想得到 %+v", i, events[i], w)
		}
	}
	// 可继续变更，保存后仍合法。
	if _, _, err := s.relocateAsset("EQ-1", "四楼", "继续搬"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := openStore(dir); err != nil {
		t.Fatalf("重开应成功: %v", err)
	}
}

// 各类位置矛盾：所有读写命令退出 1 并说明“位置矛盾/履历矛盾”，原文件字节
// 保持，不自动修复。
func TestLocationContradictionsRejected(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"位置履历原位置不接续", func(m map[string]any) {
			evs := m["events"].([]any)
			evs[3].(map[string]any)["from"] = "地下室" // seq 2: 一楼 -> 二楼 改为 地下室 -> 二楼
		}, "位置矛盾"},
		{"最终位置与保存值不一致", func(m map[string]any) {
			m["assets"].([]any)[0].(map[string]any)["location"] = "四楼"
		}, "位置矛盾"},
		{"工单报修地点与报修时位置不符", func(m map[string]any) {
			m["tickets"].([]any)[1].(map[string]any)["report_location"] = "二楼" // T0002 应为三楼
		}, "位置矛盾"},
		{"位置变更新旧位置相同", func(m map[string]any) {
			evs := m["events"].([]any)
			evs[1].(map[string]any)["to"] = "二楼"
			evs[1].(map[string]any)["from"] = "二楼" // seq 5: 二楼 -> 三楼 改为 二楼 -> 二楼
		}, "履历矛盾"},
		{"位置变更新位置为空", func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["to"] = ""
		}, "履历矛盾"},
		{"位置变更引用不存在的资产", func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["asset_id"] = "EQ-9"
		}, "履历矛盾"},
		{"位置变更带有工单编号", func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["ticket_id"] = "T0001"
		}, "履历矛盾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			shuffledLocationLedger(t, dir)
			// 按 mutate 调整 JSON。
			raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			raw, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应报 %q，得到 %v", tc.wantErr, err)
			}
			// 读、写命令都失败（退出码 1）并指出类别。
			for _, argv := range [][]string{
				{"list", "--data-dir", dir},
				{"relocate", "--data-dir", dir, "--asset-id", "EQ-1", "--location", "五楼", "--reason", "理由"},
			} {
				var out, errBuf bytes.Buffer
				if code := run(argv, &out, &errBuf); code != 1 {
					t.Fatalf("%v 应退出 1，得到 %d（%s）", argv[0], code, errBuf.String())
				}
				if !strings.Contains(errBuf.String(), tc.wantErr) {
					t.Fatalf("%v 应指出 %q，得到 %s", argv[0], tc.wantErr, errBuf.String())
				}
			}
			// 原文件字节保持。
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("矛盾台账被命令改动")
			}
		})
	}
}

// 没有位置履历但工单保存了错误的报修地点，同样按位置矛盾拒绝。
func TestWrongReportLocationWithoutHistoryRejected(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		m["tickets"].([]any)[0].(map[string]any)["report_location"] = "五楼"
	})
	_, err := openStore(dir)
	if err == nil || !strings.Contains(err.Error(), "位置矛盾") {
		t.Fatalf("应报位置矛盾，得到 %v", err)
	}
}

// 履历序号容量耗尽时位置变更失败：不更新位置、不产生履历、不消耗序号。
func TestRelocateEventSeqExhaustion(t *testing.T) {
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "理由"); err == nil ||
		!strings.Contains(err.Error(), "履历序号") {
		t.Fatalf("序号耗尽应拒绝搬移，得到 %v", err)
	}
	if got := s.findAsset("EQ-1").Location; got != "一楼" {
		t.Fatalf("失败的搬移不应改动位置，得到 %q", got)
	}
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的搬移不应改动原文件")
	}
}

// 保存失败不留部分位置或履历、不消耗序号；恢复写入条件后可按原输入重试。
func TestRelocateSaveFailureRetry(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, dir)

	if _, _, err := s.relocateAsset("EQ-1", "二楼", "搬移"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s.save()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	// 原文件字节不变。
	if got := readFileBytes(t, dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：位置仍为一楼，没有位置履历。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findAsset("EQ-1").Location; got != "一楼" {
		t.Fatalf("重载后位置应为一楼，得到 %q", got)
	}
	for _, e := range s2.data.Events {
		if e.Kind == eventRelocate {
			t.Fatal("写入失败不应留下位置履历")
		}
	}
	// 按原输入重试：成功，履历序号从下一个序号开始（未被消耗）。
	a, from, err := s2.relocateAsset("EQ-1", "二楼", "搬移")
	if err != nil {
		t.Fatalf("恢复后重试应成功: %v", err)
	}
	if from != "一楼" || a.Location != "二楼" {
		t.Fatalf("重试结果异常: from=%q a=%+v", from, a)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findAsset("EQ-1").Location; got != "二楼" {
		t.Fatalf("重试保存后位置应为二楼，得到 %q", got)
	}
	loc := sortedLocationEvents(s3.data.Events)
	if len(loc) != 1 || loc[0].Seq != 1 || loc[0].From != "一楼" || loc[0].To != "二楼" {
		t.Fatalf("应只有一条序号为 1 的位置履历: %+v", loc)
	}
}

// 导入复制位置起点、当前位置与全部位置履历：报修地点保持，履历沿用重编号，
// 保留先后关系、理由与时间精度；导入不新增搬移事件，源只读，目标原有记录
// 不变；导入后可继续变更位置。
func TestImportCarriesLocationHistory(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.relocateAsset("EQ-1", "二楼", "第一次搬"); err != nil {
		t.Fatal(err)
	}
	// 给位置履历打上小数秒时间，验证时间精度随导入保持。
	src.data.Events[1].Time = time.Date(2026, 10, 1, 9, 30, 0, 123400000, time.UTC)
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.relocateAsset("EQ-1", "三楼", "第二次搬"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)
	srcBytes := readFileBytes(t, srcDir)

	// 目标先放一项自有资产与工单，占用部分序号。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-0", "空调", "楼顶"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-0", "不制冷", "req-0"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(outcome.tickets) != 2 {
		t.Fatalf("应映射 2 张工单，得到 %d", len(outcome.tickets))
	}
	// 源只读。
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBytes) {
		t.Fatal("导入后源台账字节应保持不变")
	}

	res, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("导入后目标台账应能加载（位置链校验通过）: %v", err)
	}
	// 目标原有记录不变。
	if got := res.findAsset("EQ-0"); got == nil || got.Location != "楼顶" {
		t.Fatalf("目标原有资产应不变: %+v", got)
	}
	if got := res.findTicket("T0001"); got == nil || got.AssetID != "EQ-0" || got.ReportLocation != "楼顶" {
		t.Fatalf("目标原有工单应不变: %+v", got)
	}
	// 导入的资产、工单与地点：源 T0001->目标 T0002，源 T0002->目标 T0003。
	if got := res.findAsset("EQ-1"); got == nil || got.Location != "三楼" {
		t.Fatalf("导入资产当前位置应为三楼: %+v", got)
	}
	if got := res.findTicket("T0002"); got == nil || got.ReportLocation != "一楼" {
		t.Fatalf("导入旧单 T0002 报修地点应为一楼: %+v", got)
	}
	if got := res.findTicket("T0003"); got == nil || got.ReportLocation != "二楼" {
		t.Fatalf("导入新单 T0003 报修地点应为二楼: %+v", got)
	}
	// 履历重编号且不新增搬移事件：EQ-1 的 5 条履历（报修、搬移、关闭、报修、
	// 搬移）序号接续目标最大序号（目标已有 seq 1，导入为 seq 2..6）。
	events := res.eventsOf("EQ-1")
	if len(events) != 5 {
		t.Fatalf("导入履历应为 5 条且不新增事件，得到 %d: %+v", len(events), events)
	}
	wantSeq := []int{2, 3, 4, 5, 6}
	wantKinds := []string{eventReport, eventRelocate, eventClose, eventReport, eventRelocate}
	for i := range events {
		if events[i].Seq != wantSeq[i] || events[i].Kind != wantKinds[i] {
			t.Fatalf("导入履历重编号/顺序不对: %+v", events)
		}
	}
	if events[1].From != "一楼" || events[1].To != "二楼" || events[1].Content != "第一次搬" {
		t.Fatalf("第一条位置履历内容不对: %+v", events[1])
	}
	if events[4].From != "二楼" || events[4].To != "三楼" {
		t.Fatalf("第二条位置履历不对: %+v", events[4])
	}
	if got := events[1].Time; got.Nanosecond() != 123400000 || !got.Equal(
		time.Date(2026, 10, 1, 9, 30, 0, 123400000, time.UTC)) {
		t.Fatalf("位置履历小数秒时间精度应保持，得到 %s", got.Format(time.RFC3339Nano))
	}
	// 导入后可继续变更位置。
	if _, _, err := res.relocateAsset("EQ-1", "四楼", "导入后搬移"); err != nil {
		t.Fatalf("导入后应可继续变更位置: %v", err)
	}
	if err := res.save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("再次搬移后重开失败: %v", err)
	}
	// 旧请求在目标重放：返回映射后的原工单，地点不变、不增履历。
	old, replay, err := res.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0002" || old.ReportLocation != "一楼" {
		t.Fatalf("导入后旧请求重放应返回 T0002/一楼: %v replay=%v err=%v", old, replay, err)
	}
	if len(res.eventsOf("EQ-1")) != 6 {
		t.Fatal("重放不应追加履历")
	}
}

// 整批导入沿用整批冲突拒绝：位置数据本身不产生部分写入（由 save 的一致性
// 校验兜底）；容量不足等失败时目标原有记录不变。
func TestImportLocationFailureKeepsTarget(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.relocateAsset("EQ-1", "二楼", "搬移"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-1", "别的设备", "楼顶"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)
	before := readFileBytes(t, dstDir)

	if _, err := importAssets(dstDir, srcDir, []string{"EQ-1"}); err == nil {
		t.Fatal("资产编号冲突时整批导入应失败")
	}
	if got := readFileBytes(t, dstDir); !bytes.Equal(got, before) {
		t.Fatal("整批失败不应改动目标台账")
	}
}
