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
)

// 报修与位置变更交错：维修中搬移不改旧单报修地点，搬移后新报修采用新地点；
// 旧请求重放仍返回原工单，不改地点也不追加履历；停用资产仍可变更位置；
// 位置变更不改变资产状态、工单状态与负责人。
func TestRelocateInterleavedWithTickets(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}

	// 未知资产、空位置、空理由、新旧位置相同均拒绝，且不追加履历、不消耗序号。
	before := len(s.data.Events)
	if _, _, err := s.relocateAsset("NOPE", "二楼", "搬移"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产变更应失败，得到 %v", err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "", "搬移"); !errors.Is(err, errConflict) {
		t.Fatalf("空位置应失败，得到 %v", err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "二楼", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应失败，得到 %v", err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "一楼", "搬回原点"); !errors.Is(err, errConflict) {
		t.Fatalf("新旧位置相同应失败，得到 %v", err)
	}
	if len(s.data.Events) != before {
		t.Fatalf("失败的位置变更不应追加履历，events=%d", len(s.data.Events))
	}

	// 一楼报修 T0001，资产维修中。
	tk1, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || replay {
		t.Fatalf("首次报修: %v replay=%v", tk1, replay)
	}
	if got := s.reportLocationOf(tk1); got != "一楼" {
		t.Fatalf("T0001 报修地点 = %q，想得到 一楼", got)
	}

	// 维修中可以搬移：一楼 -> 二楼。
	a, from, err := s.relocateAsset("EQ-1", "二楼", "维修工位调整")
	if err != nil {
		t.Fatalf("维修中搬移应成功: %v", err)
	}
	if from != "一楼" || a.Location != "二楼" || a.Status != statusRepairing {
		t.Fatalf("搬移结果不对: from=%q loc=%q status=%q", from, a.Location, a.Status)
	}
	// 旧单报修地点不被搬移改写。
	if got := s.findTicket(tk1.ID).ReportLocation; got != "一楼" {
		t.Fatalf("搬移后旧单报修地点 = %q，应保持 一楼", got)
	}
	if got := s.reportLocationOf(s.findTicket(tk1.ID)); got != "一楼" {
		t.Fatalf("追溯旧单报修地点 = %q，应保持 一楼", got)
	}

	// 维修中仍不能开第二张单；旧请求重放返回原单、不改地点、不追加履历。
	eventsBefore := len(s.data.Events)
	if _, _, err := s.report("EQ-1", "又坏了", "req-2"); !errors.Is(err, errConflict) {
		t.Fatalf("维修中资产新报修应失败，得到 %v", err)
	}
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != tk1.ID || old.ReportLocation != "一楼" {
		t.Fatalf("重放应返回原单且地点不变: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Events) != eventsBefore {
		t.Fatal("重放不应追加履历")
	}

	// 关闭旧单后在二楼新报修 T0002：新单报修地点为二楼。
	if _, _, err := s.closeTicket(tk1.ID, "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	tk2, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay || tk2.ID != "T0002" {
		t.Fatalf("新报修应开出 T0002: %v replay=%v err=%v", tk2, replay, err)
	}
	if got := tk2.ReportLocation; got != "二楼" {
		t.Fatalf("T0002 报修地点 = %q，想得到 二楼", got)
	}

	// 搬移到三楼后取消 T0002：取消不改变报修地点。
	if _, _, err := s.relocateAsset("EQ-1", "三楼", "转仓"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket(tk2.ID, "误报，设备实际正常"); err != nil {
		t.Fatal(err)
	}
	if got := s.findTicket(tk2.ID).ReportLocation; got != "二楼" {
		t.Fatalf("搬移并取消后 T0002 报修地点 = %q，应保持 二楼", got)
	}
	cur := s.findAsset("EQ-1")
	if cur.Location != "三楼" || cur.Status != statusAvailable {
		t.Fatalf("取消后资产位置/状态 = %q/%q", cur.Location, cur.Status)
	}

	// 停用资产也可以变更位置，且不改变停用状态。
	if _, err := s.deactivateAsset("EQ-1", "设备调拨，暂停使用"); err != nil {
		t.Fatal(err)
	}
	a, _, err = s.relocateAsset("EQ-1", "四楼库房", "停用设备集中存放")
	if err != nil {
		t.Fatalf("停用资产搬移应成功: %v", err)
	}
	if a.Status != statusDeactivated || a.Location != "四楼库房" {
		t.Fatalf("停用资产搬移后 = %q/%q", a.Location, a.Status)
	}

	// 履历按全库序号排列，两条位置变更的原/新位置与理由正确。
	events := s.eventsOf("EQ-1")
	wantKinds := []string{
		eventReport, eventRelocate, eventClose, eventReport,
		eventRelocate, eventCancel, eventDeactivate, eventRelocate,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("履历条数 = %d，想得到 %d: %+v", len(events), len(wantKinds), events)
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Fatalf("第 %d 条履历 = %q，想得到 %q（全部: %+v）", i, events[i].Kind, k, events)
		}
	}
	if events[1].From != "一楼" || events[1].To != "二楼" || events[1].Content != "维修工位调整" ||
		events[1].TicketID != "" {
		t.Fatalf("第一条位置变更履历不对: %+v", events[1])
	}
	if events[4].From != "二楼" || events[4].To != "三楼" {
		t.Fatalf("第二条位置变更履历不对: %+v", events[4])
	}

	// 重载：当前位置、位置起点、各单报修地点与重放行为全部保持。
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1").Location; got != "四楼库房" {
		t.Fatalf("重载后当前位置 = %q", got)
	}
	if got := s.locationOrigin("EQ-1"); got != "一楼" {
		t.Fatalf("重载后位置起点 = %q，想得到 一楼", got)
	}
	if got := s.reportLocationOf(s.findTicket("T0001")); got != "一楼" {
		t.Fatalf("重载后 T0001 报修地点 = %q", got)
	}
	if got := s.reportLocationOf(s.findTicket("T0002")); got != "二楼" {
		t.Fatalf("重载后 T0002 报修地点 = %q", got)
	}
	old, replay, err = s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" {
		t.Fatalf("重载后重放应返回 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.eventsOf("EQ-1")) != len(wantKinds) {
		t.Fatal("重载后重放不应追加履历")
	}
}

// 位置变更不改变保养周期：搬移前后计划的下一到期日保持。
func TestRelocateKeepsPlanAndAssignment(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	p, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "搬移"); err != nil {
		t.Fatal(err)
	}
	if got := s.findTicket(tk.ID).Assignee; got != "张三" {
		t.Fatalf("搬移后负责人改变: %q", got)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != p.NextDue || got.Content != p.Content ||
		got.IntervalDays != p.IntervalDays {
		t.Fatalf("搬移后保养计划改变: %+v", got)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("搬移不应改变资产状态，得到 %q", got)
	}
}

// writeRelocateLedger 直接组装一份台账 JSON：允许显式指定资产、工单（含
// 报修地点）与乱序履历。
func writeRelocateLedger(t *testing.T, dir string, assets []*Asset, tickets []*Ticket,
	events []eventJSON, requests []requestBinding, nextSeq int) []byte {
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
	return raw
}

func relocateEventJSON(seq int, asset, from, to, reason, at string) eventJSON {
	return eventJSON{
		Seq: seq, AssetID: asset, Kind: eventRelocate,
		Content: reason, From: from, To: to, Time: at,
	}
}

// 没有位置履历的有效旧库以保存位置为起点，无需转换：查询能追溯报修地点，
// 查询不补写数据；首次变更保存后重载仍可还原起点。
func TestLegacyStoreWithoutRelocationHistory(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusRepairing}}
	tickets := []*Ticket{{
		ID: "T0001", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-1",
		Status: ticketOpen, CreatedAt: "2026-10-01T08:00:00Z",
		// 旧库工单没有报修地点字段。
	}}
	events := []eventJSON{ev(3, "EQ-1", "T0001", eventReport, "卡纸", "2026-10-01T08:00:00Z")}
	requests := []requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}
	raw := writeRelocateLedger(t, dir, assets, tickets, events, requests, 5)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无位置履历旧库应能加载: %v", err)
	}
	tk := s.findTicket("T0001")
	if tk.ReportLocation != "" {
		t.Fatalf("旧库不应被补写报修地点，得到 %q", tk.ReportLocation)
	}
	if got := s.reportLocationOf(tk); got != "一楼" {
		t.Fatalf("旧库报修地点应以保存位置追溯，得到 %q", got)
	}
	if got := s.locationOrigin("EQ-1"); got != "一楼" {
		t.Fatalf("旧库位置起点 = %q，应为保存位置 一楼", got)
	}

	// 查询（ticket/history/list/detail）不得补写数据或初始化之外的修改。
	var out, errBuf bytes.Buffer
	if code := run([]string{"ticket", "--data-dir", dir, "--ticket-id", "T0001"}, &out, &errBuf); code != 0 {
		t.Fatalf("ticket 查询失败 code=%d err=%s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "报修地点: 一楼") {
		t.Fatalf("ticket 输出应含报修地点，得到:\n%s", out.String())
	}
	rawAfter, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAfter) != string(raw) {
		t.Fatal("只读查询不应改写旧库文件")
	}

	// 首次变更后保存：起点（最早位置履历的原位置）重载后仍可还原。
	a, from, err := s.relocateAsset("EQ-1", "二楼", "搬维修间")
	if err != nil {
		t.Fatal(err)
	}
	if from != "一楼" || a.Location != "二楼" {
		t.Fatalf("首次变更结果不对: %q -> %q", from, a.Location)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.locationOrigin("EQ-1"); got != "一楼" {
		t.Fatalf("重载后起点 = %q，想得到 一楼", got)
	}
	// 旧单报修地点仍按报修履历序号（3）追溯到一楼。
	if got := s2.reportLocationOf(s2.findTicket("T0001")); got != "一楼" {
		t.Fatalf("首次变更后旧单报修地点 = %q，应仍为 一楼", got)
	}
	if got := s2.findAsset("EQ-1").Location; got != "二楼" {
		t.Fatalf("重载后当前位置 = %q", got)
	}
}

// 乱序履历、序号间隔、时间不递增仍合法：位置链按全库序号重放，
// 起点与报修地点可正确追溯，重载后保持。
func TestRelocateUnorderedGapsAndNonMonotonicTime(t *testing.T) {
	dir := t.TempDir()
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "三楼", Status: statusAvailable}}
	tickets := []*Ticket{{
		ID: "T0001", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-1",
		Status: ticketClosed, Result: "已修复",
		CreatedAt: "2026-10-01T08:00:00Z", ClosedAt: "2026-10-03T09:00:00Z",
	}}
	// 序号顺序应为：1 搬移 一楼->二楼；2 报修（当时在二楼）；5 搬移 二楼->三楼；
	// 6 关闭。数组故意乱序，时间故意不递增。
	events := []eventJSON{
		relocateEventJSON(5, "EQ-1", "二楼", "三楼", "转仓", "2026-10-01T06:00:00Z"),
		ev(6, "EQ-1", "T0001", eventClose, "已修复", "2026-10-03T09:00:00Z"),
		relocateEventJSON(1, "EQ-1", "一楼", "二楼", "上楼", "2026-10-02T10:00:00Z"),
		ev(2, "EQ-1", "T0001", eventReport, "卡纸", "2026-10-02T08:00:00Z"),
	}
	requests := []requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}
	writeRelocateLedger(t, dir, assets, tickets, events, requests, 10)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序/间隔/时间不递增的台账应合法: %v", err)
	}
	if got := s.locationOrigin("EQ-1"); got != "一楼" {
		t.Fatalf("位置起点 = %q，想得到 一楼", got)
	}
	// 报修履历序号 2 当时的位置是二楼（seq 1 的搬移之后、seq 5 之前）。
	if got := s.reportLocationOf(s.findTicket("T0001")); got != "二楼" {
		t.Fatalf("报修地点 = %q，想得到 二楼", got)
	}
	if got := s.findAsset("EQ-1").Location; got != "三楼" {
		t.Fatalf("当前位置 = %q，想得到 三楼", got)
	}
	// 重载后仍可在链尾继续变更；下一条履历序号为最大序号 6 之后。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, from, err := s2.relocateAsset("EQ-1", "四楼", "再次搬移")
	if err != nil {
		t.Fatalf("乱序台账上继续变更应成功: %v", err)
	}
	if from != "三楼" || a.Location != "四楼" {
		t.Fatalf("接续位置不对: %q -> %q", from, a.Location)
	}
	last := s2.eventsOf("EQ-1")
	if last[len(last)-1].Seq != 7 {
		t.Fatalf("新履历序号 = %d，想得到 7", last[len(last)-1].Seq)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir); err != nil {
		t.Fatalf("保存并重载应通过: %v", err)
	}
}

// 各类位置矛盾：读写命令都退出 1 并指出类别，原文件字节保持，不自动修复。
func TestRelocateContradictionsRejected(t *testing.T) {
	baseAssets := func(location string) []*Asset {
		return []*Asset{{ID: "EQ-1", Name: "打印机", Location: location, Status: statusAvailable}}
	}
	baseTickets := []*Ticket{{
		ID: "T0001", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-1",
		Status: ticketClosed, Result: "已修复",
		CreatedAt: "2026-10-01T08:00:00Z", ClosedAt: "2026-10-03T09:00:00Z",
		ReportLocation: "一楼",
	}}
	baseRequests := []requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}
	chain := func(reportAt string) []eventJSON {
		return []eventJSON{
			ev(2, "EQ-1", "T0001", eventReport, "卡纸", "2026-10-01T08:00:00Z"),
			relocateEventJSON(1, "EQ-1", "一楼", "二楼", "上楼", "2026-09-30T08:00:00Z"),
			relocateEventJSON(5, "EQ-1", "二楼", "三楼", "转仓", reportAt),
			ev(6, "EQ-1", "T0001", eventClose, "已修复", "2026-10-03T09:00:00Z"),
		}
	}
	cases := []struct {
		name    string
		assets  []*Asset
		tickets []*Ticket
		events  []eventJSON
		want    string
	}{
		{
			name:    "原位置不接续",
			assets:  baseAssets("三楼"),
			tickets: baseTickets,
			events: func() []eventJSON {
				es := chain("2026-10-02T08:00:00Z")
				es[2] = relocateEventJSON(5, "EQ-1", "地下室", "三楼", "转仓", "2026-10-02T08:00:00Z")
				return es
			}(),
			want: "位置矛盾",
		},
		{
			name:    "最终位置与保存值不符",
			assets:  baseAssets("四楼"),
			tickets: baseTickets,
			events:  chain("2026-10-02T08:00:00Z"),
			want:    "位置矛盾",
		},
		{
			name:   "工单保存报修地点与当时位置不符",
			assets: baseAssets("三楼"),
			tickets: []*Ticket{{
				ID: "T0001", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-1",
				Status: ticketClosed, Result: "已修复",
				CreatedAt: "2026-10-01T08:00:00Z", ClosedAt: "2026-10-03T09:00:00Z",
				ReportLocation: "三楼", // 报修（seq 2）当时应为二楼
			}},
			events: chain("2026-10-02T08:00:00Z"),
			want:   "位置矛盾",
		},
		{
			name:    "位置变更引用不存在的资产",
			assets:  baseAssets("三楼"),
			tickets: baseTickets,
			events: func() []eventJSON {
				es := chain("2026-10-02T08:00:00Z")
				es[2] = relocateEventJSON(5, "EQ-9", "二楼", "三楼", "转仓", "2026-10-02T08:00:00Z")
				return es
			}(),
			want: "位置矛盾",
		},
		{
			name:    "新旧位置相同",
			assets:  baseAssets("三楼"),
			tickets: baseTickets,
			events: func() []eventJSON {
				es := chain("2026-10-02T08:00:00Z")
				// 让 seq1 与 seq5 都停在二楼：seq5 新旧相同，属履历字段矛盾。
				es[2] = relocateEventJSON(5, "EQ-1", "二楼", "二楼", "空转", "2026-10-02T08:00:00Z")
				// 资产保存位置仍为二楼，与链终值一致。
				return es
			}(),
			want: "履历矛盾",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeRelocateLedger(t, dir, tc.assets, tc.tickets, tc.events, baseRequests, 10)
			if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 %s，得到 %v", tc.want, err)
			}
			// 读命令与写命令都退出 1，且原文件字节保持。
			for _, args := range [][]string{
				{"list", "--data-dir", dir},
				{"history", "--data-dir", dir, "--asset-id", "EQ-1"},
				{"move", "--data-dir", dir, "--asset-id", "EQ-1", "--location", "九楼", "--reason", "尝试搬移"},
			} {
				var out, errBuf bytes.Buffer
				code := run(args, &out, &errBuf)
				if code != 1 {
					t.Fatalf("命令 %v 应退出 1，得到 %d（out=%s err=%s）",
						args[0], code, out.String(), errBuf.String())
				}
				if !strings.Contains(errBuf.String(), tc.want) {
					t.Fatalf("命令 %s 错误应说明 %s，得到 %s", args[0], tc.want, errBuf.String())
				}
			}
			rawAfter, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil {
				t.Fatal(err)
			}
			if string(rawAfter) != string(raw) {
				t.Fatal("矛盾数据被命令改写，原文件字节应保持")
			}
		})
	}
}

// 导入接续：源资产的位置起点、当前位置、全部位置履历随资产复制并沿用履历
// 重编号；导入工单的报修地点保持；导入后可继续变更位置；源只读。
func TestImportRelocationChain(t *testing.T) {
	src := newTestStore(t)
	if _, err := src.registerAsset("EQ-S", "打印机", "车间A"); err != nil {
		t.Fatal(err)
	}
	// 车间A 报修 T0001，搬移到车间B 后关闭；在车间B 再报修 T0002（未关闭）。
	t1, _, err := src.report("EQ-S", "卡纸", "req-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.relocateAsset("EQ-S", "车间B", "设备挪到 B 线"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket(t1.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-S", "异响", "req-b"); err != nil {
		t.Fatal(err)
	}
	if err := src.save(); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(src.dir, dataFileName)
	srcBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	// 目标库已有一项资产和几张履历，使导入序号发生平移。
	dst := newTestStore(t)
	if _, err := dst.registerAsset("EQ-T", "空调", "主楼"); err != nil {
		t.Fatal(err)
	}
	td, _, err := dst.report("EQ-T", "不制冷", "req-t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.closeTicket(td.ID, "已加氟"); err != nil {
		t.Fatal(err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}
	dstMaxSeq := 0
	for _, e := range dst.data.Events {
		if e.Seq > dstMaxSeq {
			dstMaxSeq = e.Seq
		}
	}

	outcome, err := importAssets(dst.dir, src.dir, []string{"EQ-S"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(outcome.tickets) != 2 {
		t.Fatalf("应映射 2 张工单，得到 %d", len(outcome.tickets))
	}
	newT1, newT2 := outcome.tickets[0].NewID, outcome.tickets[1].NewID

	dst2, err := openStore(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	a := dst2.findAsset("EQ-S")
	if a == nil || a.Location != "车间B" || a.Status != statusRepairing {
		t.Fatalf("导入资产位置/状态不对: %+v", a)
	}
	// 位置起点仍为车间A（最早位置履历的原位置）。
	if got := dst2.locationOrigin("EQ-S"); got != "车间A" {
		t.Fatalf("导入后位置起点 = %q，想得到 车间A", got)
	}
	// 两张工单的报修地点保持为源台账报修履历序号当时的位置。
	if got := dst2.findTicket(newT1).ReportLocation; got != "车间A" {
		t.Fatalf("导入 T1 报修地点 = %q，想得到 车间A", got)
	}
	if got := dst2.findTicket(newT2).ReportLocation; got != "车间B" {
		t.Fatalf("导入 T2 报修地点 = %q，想得到 车间B", got)
	}
	// 位置履历一并复制并重编号，先后关系保持（报A -> 搬A到B -> 关 -> 报B）。
	events := dst2.eventsOf("EQ-S")
	wantKinds := []string{eventReport, eventRelocate, eventClose, eventReport}
	if len(events) != len(wantKinds) {
		t.Fatalf("导入履历条数 = %d，想得到 4", len(events))
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Fatalf("导入履历顺序不对: %+v", events)
		}
	}
	if events[0].Seq != dstMaxSeq+1 || events[1].Seq != dstMaxSeq+2 {
		t.Fatalf("导入履历应从目标最大序号 %d 之后接续: %+v", dstMaxSeq, events)
	}
	if events[1].From != "车间A" || events[1].To != "车间B" || events[1].Content != "设备挪到 B 线" {
		t.Fatalf("导入的位置履历内容不对: %+v", events[1])
	}

	// 源台账只读。
	gotSrc, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSrc) != string(srcBytes) {
		t.Fatal("导入后源台账字节发生变化")
	}

	// 导入后资产可继续变更位置：维修中（T2 未关闭）搬移到车间C，
	// T2 报修地点保持车间B；旧请求重放返回映射后的工单，不追加履历。
	if _, _, err := dst2.relocateAsset("EQ-S", "车间C", "导入后继续搬移"); err != nil {
		t.Fatalf("导入后继续变更应成功: %v", err)
	}
	if got := dst2.findTicket(newT2).ReportLocation; got != "车间B" {
		t.Fatalf("导入后搬移不改 T2 报修地点，得到 %q", got)
	}
	nEvents := len(dst2.eventsOf("EQ-S"))
	old, replay, err := dst2.report("EQ-S", "卡纸", "req-a")
	if err != nil || !replay || old.ID != newT1 {
		t.Fatalf("导入后原请求重放应返回 %s: %v replay=%v err=%v", newT1, old, replay, err)
	}
	if len(dst2.eventsOf("EQ-S")) != nEvents {
		t.Fatal("导入后重放不应追加履历")
	}
	if err := dst2.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dst.dir); err != nil {
		t.Fatalf("接续变更保存后重载应通过: %v", err)
	}
}

// 保存失败后的重载重试：写入失败时原文件字节保持、不落部分位置或履历；
// 恢复写入条件后可按原输入重试成功。
func TestRelocateSaveFailureRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
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
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "搬维修间"); err != nil {
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
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAfter) != string(rawBefore) {
		t.Fatal("保存失败不应改变原文件字节")
	}
	// 重载确认未落任何部分变化，然后按原输入重试，保存成功。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findAsset("EQ-1").Location; got != "一楼" {
		t.Fatalf("失败保存后重载位置应为 一楼，得到 %q", got)
	}
	if len(s2.eventsOf("EQ-1")) != 0 {
		t.Fatalf("失败保存后不应有位置履历，得到 %d 条", len(s2.eventsOf("EQ-1")))
	}
	if _, _, err := s2.relocateAsset("EQ-1", "二楼", "搬维修间"); err != nil {
		t.Fatalf("恢复后按原输入重试应成功: %v", err)
	}
	if err := s2.save(); err != nil {
		t.Fatalf("恢复后保存应成功: %v", err)
	}
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findAsset("EQ-1").Location; got != "二楼" {
		t.Fatalf("重试后位置 = %q，想得到 二楼", got)
	}
	if evs := s3.eventsOf("EQ-1"); len(evs) != 1 || evs[0].From != "一楼" || evs[0].To != "二楼" {
		t.Fatalf("重试后履历不对: %+v", evs)
	}
}

// 履历序号容量不足：不追加履历、不改位置、不消耗序号，原文件字节保持。
func TestRelocateSeqExhausted(t *testing.T) {
	dir := t.TempDir()
	// 报修履历占用最大正整数序号，资产因这张未关闭工单处于维修中；下一条履历
	// 无序号可用。
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusRepairing}}
	tickets := []*Ticket{{
		ID: "T0001", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-1",
		Status: ticketOpen, CreatedAt: "2026-10-01T08:00:00Z",
		ReportLocation: "一楼",
	}}
	events := []eventJSON{
		ev(math.MaxInt, "EQ-1", "T0001", eventReport, "卡纸", "2026-10-01T08:00:00Z"),
	}
	requests := []requestBinding{req("req-1", "EQ-1", "卡纸", "T0001")}
	raw := writeRelocateLedger(t, dir, assets, tickets, events, requests, 2)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("容量耗尽台账本身应合法: %v", err)
	}
	if _, _, err := s.relocateAsset("EQ-1", "二楼", "搬移"); !errors.Is(err, errConflict) {
		t.Fatalf("序号耗尽应冲突失败，得到 %v", err)
	}
	if got := s.findAsset("EQ-1").Location; got != "一楼" {
		t.Fatalf("容量不足失败不应改位置，得到 %q", got)
	}
	if len(s.data.Events) != 1 {
		t.Fatalf("容量不足失败不应追加履历，events=%d", len(s.data.Events))
	}
	rawAfter, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAfter) != string(raw) {
		t.Fatal("容量不足失败后进程内未保存，文件应保持原样")
	}
}

// 命令行入口：成功输出、退出码与 ticket/history 展示。
func TestMoveCommandAndDisplays(t *testing.T) {
	dir := t.TempDir()
	runOK := func(args ...string) string {
		var out, errBuf bytes.Buffer
		code := run(args, &out, &errBuf)
		if code != 0 {
			t.Fatalf("命令 %v 退出 %d: %s", args, code, errBuf.String())
		}
		return out.String()
	}
	runCode := func(wantCode int, args ...string) string {
		var out, errBuf bytes.Buffer
		code := run(args, &out, &errBuf)
		if code != wantCode {
			t.Fatalf("命令 %v 退出 %d，想得到 %d（err=%s）", args, code, wantCode, errBuf.String())
		}
		return errBuf.String()
	}

	runOK("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	out := runOK("report", "--data-dir", dir, "--asset-id", "EQ-1",
		"--description", "卡纸", "--request-id", "req-1")
	if !strings.Contains(out, "T0001") {
		t.Fatalf("报修输出异常: %s", out)
	}

	// 缺少必填参数退出 2；未知资产、相同位置退出 1。
	runCode(2, "move", "--data-dir", dir, "--asset-id", "EQ-1", "--location", "二楼")
	runCode(1, "move", "--data-dir", dir, "--asset-id", "NOPE", "--location", "二楼", "--reason", "搬")
	runCode(1, "move", "--data-dir", dir, "--asset-id", "EQ-1", "--location", "一楼", "--reason", "原地")

	// 维修中搬移成功，输出资产编号、原位置与新位置。
	out = runOK("move", "--data-dir", dir, "--asset-id", "EQ-1",
		"--location", "二楼", "--reason", "维修工位调整")
	if !strings.Contains(out, "资产 EQ-1 位置已变更。") ||
		!strings.Contains(out, "原位置: 一楼") || !strings.Contains(out, "新位置: 二楼") {
		t.Fatalf("move 输出不对: %s", out)
	}

	// list/detail 显示当前位置。
	if out := runOK("list", "--data-dir", dir); !strings.Contains(out, "二楼") {
		t.Fatalf("list 应显示当前位置: %s", out)
	}
	if out := runOK("detail", "--data-dir", dir, "--asset-id", "EQ-1"); !strings.Contains(out, "位置: 二楼") {
		t.Fatalf("detail 应显示当前位置: %s", out)
	}

	// ticket 显示报修地点（旧单仍为一楼）。
	out = runOK("ticket", "--data-dir", dir, "--ticket-id", "T0001")
	if !strings.Contains(out, "报修地点: 一楼") {
		t.Fatalf("ticket 应显示旧单报修地点 一楼: %s", out)
	}

	// history 按全库序号展示位置变更。
	out = runOK("history", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(out, "位置变更: 一楼 -> 二楼（维修工位调整）") {
		t.Fatalf("history 应展示位置变更: %s", out)
	}
	if strings.Index(out, "报修") > strings.Index(out, "位置变更") {
		// 报修应排在位置变更之前。
		t.Fatalf("history 顺序不对: %s", out)
	}

	// 搬移后新报修采用新地点。
	runOK("close", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "已修复")
	out = runOK("report", "--data-dir", dir, "--asset-id", "EQ-1",
		"--description", "无法开机", "--request-id", "req-2")
	if !strings.Contains(out, "T0002") {
		t.Fatalf("第二次报修应开 T0002: %s", out)
	}
	if out := runOK("ticket", "--data-dir", dir, "--ticket-id", "T0002"); !strings.Contains(out, "报修地点: 二楼") {
		t.Fatalf("新单报修地点应为二楼: %s", out)
	}
	// 已关闭旧单查询仍显示原报修地点。
	if out := runOK("ticket", "--data-dir", dir, "--ticket-id", "T0001"); !strings.Contains(out, "报修地点: 一楼") {
		t.Fatalf("已关闭旧单应保持报修地点 一楼: %s", out)
	}
}

// 帮助文本包含 move 命令简述。
func TestMoveHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"--help"}, &out, &errBuf); code != 0 {
		t.Fatalf("help 退出 %d", code)
	}
	if !strings.Contains(out.String(), "move") || !strings.Contains(out.String(), "位置变更") {
		t.Fatalf("帮助应包含 move 与位置变更说明:\n%s", out.String())
	}
}
