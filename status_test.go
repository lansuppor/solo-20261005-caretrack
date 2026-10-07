package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 停用/恢复使用的状态交错：报修、关闭、停用、恢复交替进行，维修中不能停用，
// 重复停用/恢复拒绝，重启后状态与履历保持，停用不消耗工单编号。
func TestDeactivateReactivateInterleaving(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}

	// 未知资产、空理由停用拒绝。
	if _, err := s.deactivateAsset("NOPE", "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产停用应失败，得到 %v", err)
	}
	if _, err := s.deactivateAsset("EQ-1", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由停用应失败，得到 %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("失败停用不应改变状态，得到 %q", got)
	}

	// 维修中（有未关闭工单）不能停用。
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "维修中尝试停用"); !errors.Is(err, errConflict) {
		t.Fatalf("维修中停用应失败，得到 %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("失败停用不应改变维修中状态，得到 %q", got)
	}

	if _, _, err := s.closeTicket(tk.ID, "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	eventsBefore := len(s.data.Events)

	// 可用资产停用成功。
	a, err := s.deactivateAsset("EQ-1", "设备调拨，暂停使用")
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if a.Status != statusDeactivated {
		t.Fatalf("停用后状态 = %q", a.Status)
	}
	if len(s.data.Events) != eventsBefore+1 {
		t.Fatalf("停用应追加恰一条履历，events=%d", len(s.data.Events))
	}
	ev := s.eventsOf("EQ-1")
	last := ev[len(ev)-1]
	if last.Kind != eventDeactivate || last.From != statusAvailable || last.To != statusDeactivated ||
		last.Content != "设备调拨，暂停使用" || last.TicketID != "" || last.Time.IsZero() {
		t.Fatalf("停用履历内容不对: %+v", last)
	}
	// 停用不创建工单、不消耗工单编号。
	if s.data.NextTicketSeq != 2 {
		t.Fatalf("停用不应消耗工单编号，NextTicketSeq=%d", s.data.NextTicketSeq)
	}

	// 重复停用、对停用资产恢复之外的操作边界。
	if _, err := s.deactivateAsset("EQ-1", "再次停用"); !errors.Is(err, errConflict) {
		t.Fatalf("重复停用应失败，得到 %v", err)
	}
	if _, err := s.reactivateAsset("EQ-1", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空恢复理由应失败，得到 %v", err)
	}

	// 恢复使用成功，历史记录保留。
	a, err = s.reactivateAsset("EQ-1", "调拨回库，恢复使用")
	if err != nil {
		t.Fatalf("恢复使用失败: %v", err)
	}
	if a.Status != statusAvailable {
		t.Fatalf("恢复后状态 = %q", a.Status)
	}
	rev := s.eventsOf("EQ-1")
	last = rev[len(rev)-1]
	if last.Kind != eventReactivate || last.From != statusDeactivated || last.To != statusAvailable ||
		last.Content != "调拨回库，恢复使用" {
		t.Fatalf("恢复履历内容不对: %+v", last)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("恢复不应删除历史，履历数 = %d", got)
	}

	// 对可用资产重复恢复拒绝。
	if _, err := s.reactivateAsset("EQ-1", "再次恢复"); !errors.Is(err, errConflict) {
		t.Fatalf("可用资产恢复应失败，得到 %v", err)
	}

	// 失败操作不消耗履历序号：下一条履历序号应紧跟现有最大序号。
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	got := s.eventsOf("EQ-1")
	if got[len(got)-1].Seq != maxSeq+1 {
		t.Fatalf("失败的停用/恢复不应消耗履历序号，新报修序号 = %d，想得到 %d",
			got[len(got)-1].Seq, maxSeq+1)
	}

	// 重启后停用状态与履历链保持：先把当前未关闭工单关闭，再停用，落盘重开。
	tk2 := s.findTicket(s.data.Tickets[len(s.data.Tickets)-1].ID)
	if _, _, err := s.closeTicket(tk2.ID, "已更换电源"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "再次调拨"); err != nil {
		t.Fatal(err)
	}
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1").Status; got != statusDeactivated {
		t.Fatalf("重启后停用状态未保持，得到 %q", got)
	}
	if _, err := s.reactivateAsset("EQ-1", "恢复"); err != nil {
		t.Fatalf("重启后应能继续恢复: %v", err)
	}
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("重启后可用状态未保持，得到 %q", got)
	}
}

// 停用期间的请求重放：新报修被拒绝且不绑定请求标识；已有请求的相同重放返回
// 原工单及当前状态，不开单、不改资产状态；冲突重放仍拒绝；恢复后可重试。
func TestDeactivationRequestReplay(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}

	// 停用期间新报修被拒绝，不绑定请求标识，也不改变资产状态。
	if _, _, err := s.report("EQ-1", "停用期间故障", "req-new"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间新报修应拒绝，得到 %v", err)
	}
	if s.findRequest("req-new") != nil {
		t.Fatal("被拒绝的新报修不应绑定请求标识")
	}
	if len(s.data.Tickets) != 1 {
		t.Fatalf("停用期间不应开出工单，工单数 = %d", len(s.data.Tickets))
	}
	if got := s.findAsset("EQ-1").Status; got != statusDeactivated {
		t.Fatalf("被拒绝的报修不应改变资产状态，得到 %q", got)
	}

	// 已有请求的相同重放即使停用仍返回原工单及当前状态，不开单、不改状态。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != tk.ID || old.Status != ticketClosed {
		t.Fatalf("停用期间相同重放应返回原工单: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 1 || s.data.NextTicketSeq != 2 {
		t.Fatalf("重放不应开单或消耗编号: tickets=%d next=%d", len(s.data.Tickets), s.data.NextTicketSeq)
	}
	if got := s.findAsset("EQ-1").Status; got != statusDeactivated {
		t.Fatalf("重放不应改变资产状态，得到 %q", got)
	}

	// 冲突请求（相同标识、不同描述）仍拒绝。
	if _, _, err := s.report("EQ-1", "不同故障", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间冲突重放应拒绝，得到 %v", err)
	}

	// 恢复后用同一新标识重试成功，开出新工单。
	if _, err := s.reactivateAsset("EQ-1", "恢复使用"); err != nil {
		t.Fatal(err)
	}
	tk2, replay, err := s.report("EQ-1", "停用期间故障", "req-new")
	if err != nil || replay {
		t.Fatalf("恢复后重试应开出新工单: %v replay=%v err=%v", tk2, replay, err)
	}
	if tk2.ID != "T0002" {
		t.Fatalf("恢复后新工单编号 = %q，想得到 T0002", tk2.ID)
	}
}

// 停用与保养：停用不改变计划内容、间隔或下一到期日；停用期间不能登记完成，
// 但可建立计划、撤销已登记完成；due 排除停用资产，恢复后按保存的下一到期日
// 参与查询（逾期显示原到期日），随后完成沿用原推进规则。
func TestDeactivationMaintenance(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	// 停用资产仍可建立保养计划。
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	p, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90)
	if err != nil {
		t.Fatalf("停用资产应能建立计划: %v", err)
	}

	// 停用期间登记完成被拒绝，下一到期日与履历不变。
	if _, _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-03", "已更换"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间登记完成应拒绝，得到 %v", err)
	}
	if p.NextDue != "2026-11-01" {
		t.Fatalf("被拒绝的完成不应推进下一到期日，得到 %q", p.NextDue)
	}
	// due 排除停用资产（即使已逾期）。
	if rows := s.duePlans("2027-01-01"); len(rows) != 0 {
		t.Fatalf("停用资产不应出现在 due 结果中，得到 %+v", rows)
	}

	// 恢复后逾期计划按保存的下一到期日参与查询。
	if _, err := s.reactivateAsset("EQ-1", "恢复使用"); err != nil {
		t.Fatal(err)
	}
	rows := s.duePlans("2027-01-01")
	if len(rows) != 1 || rows[0].Due != "2026-11-01" {
		t.Fatalf("恢复后逾期计划应按原到期日显示，得到 %+v", rows)
	}

	// 随后完成沿用首次到期日 + 间隔推进规则（延期跨周期不补记录）。
	p2, next, seq, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-03", "已更换滤芯")
	if err != nil {
		t.Fatalf("恢复后登记完成失败: %v", err)
	}
	if next != "2027-01-30" || seq < 1 {
		t.Fatalf("完成后下一到期日 = %q 序号 = %d", next, seq)
	}
	_ = p2

	// 停用期间可按原规则撤销已登记完成，下一到期日恢复。
	if _, err := s.deactivateAsset("EQ-1", "再次停用"); err != nil {
		t.Fatal(err)
	}
	pp, target, err := s.revokeCompletion("EQ-1", seq, "误登记，实际未保养")
	if err != nil {
		t.Fatalf("停用期间撤销完成应允许: %v", err)
	}
	if pp.NextDue != target.Due || pp.NextDue != "2026-11-01" {
		t.Fatalf("撤销后应恢复周期到期日 2026-11-01，得到 %q", pp.NextDue)
	}
	// 计划内容、间隔未受停用影响。
	if pp.IntervalDays != 90 || pp.FirstDue != "2026-11-01" || pp.Content != "更换滤芯" {
		t.Fatalf("停用不应改变计划内容/间隔/首次到期日: %+v", pp)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-03", "已更换"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间重新完成仍应拒绝，得到 %v", err)
	}
}

// 停用不删除工单与附件：终态工单在停用期间仍可补充、撤销附件；停用区间不计入
// 维修停机统计。
func TestDeactivationKeepsRecordsAndDowntime(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	// 一张备件领用记录随工单保留。
	wp, err := s.withdrawPart(tk.ID, "FILTER-01", 2, "更换滤芯")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	file := filepath.Join(work, "photo.jpg")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := s.attach(tk.ID, file, "故障照片")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	reportTime := time.Time{}
	closeTime := time.Time{}
	for _, e := range s.eventsOf("EQ-1") {
		switch e.Kind {
		case eventReport:
			reportTime = e.Time
		case eventClose:
			closeTime = e.Time
		}
	}

	// 停用不删除工单、负责人、备件、附件。
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	if s.findTicket(tk.ID) == nil || s.findPart(wp.ID) == nil || s.findAttachment(att.ID) == nil {
		t.Fatal("停用不应删除工单、备件或附件记录")
	}

	// 终态工单停用期间仍可补充并撤销附件。
	file2 := filepath.Join(work, "photo2.jpg")
	if err := os.WriteFile(file2, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	att2, err := s.attach(tk.ID, file2, "补充照片")
	if err != nil {
		t.Fatalf("停用期间终态工单应能补充附件: %v", err)
	}
	if _, err := s.revokeAttachment(att2.ID, "资料拍错"); err != nil {
		t.Fatalf("停用期间应能撤销附件: %v", err)
	}
	if _, err := s.revokeAttachment(att.ID, "资料作废"); err != nil {
		t.Fatalf("停用期间应能撤销原有附件: %v", err)
	}

	// 停机统计只算工单占用：窗口覆盖停用区间时仍只有报修→关闭一段。
	start := reportTime.Add(-time.Hour)
	end := closeTime.Add(24 * time.Hour)
	results, err := s.downtimeForAssets([]string{"EQ-1"}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	want := closeTime.Sub(reportTime).Nanoseconds() / int64(time.Second)
	if results[0].Seconds != want {
		t.Fatalf("停机秒数 = %d，想得到 %d（停用区间不计入）", results[0].Seconds, want)
	}
}

// 有效旧库（无任何停用字段/履历）无需转换即可继续使用。
func TestLegacyStoreWithoutStatusEvents(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // baseLedger：T0001 未关闭，资产维修中
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧库应能加载: %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("旧库资产状态 = %q", got)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "旧库资产停用"); err != nil {
		t.Fatalf("旧库资产关闭后应能停用: %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatalf("含停用履历的台账应能保存: %v", err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("含停用履历的台账应能重载: %v", err)
	}
	if got := s2.findAsset("EQ-1").Status; got != statusDeactivated {
		t.Fatalf("重载后状态 = %q", got)
	}
}

// 含停用/恢复履历的有效台账允许履历数组乱序、序号间隔、时间不递增。
func TestValidDeactivationLedgerWithGapsAndDisorder(t *testing.T) {
	dir := t.TempDir()
	// 报修(5) 早于关闭(2) 的时间，但序号更大；停用序号 9，中间有间隔，
	// 且数组故意乱序排列。最终资产停用。
	ledger := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "停用"},
		},
		"tickets": []any{
			map[string]any{
				"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-1",
				"status": "已关闭", "created_at": "2026-10-01T08:00:00Z",
				"result": "已修复", "closed_at": "2026-10-01T09:00:00Z",
			},
		},
		"events": []any{
			map[string]any{"seq": 9, "asset_id": "EQ-1", "kind": "停用",
				"content": "调拨", "from": "可用", "to": "停用", "time": "2026-10-03T08:00:00Z"},
			map[string]any{"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "关闭", "content": "已修复", "time": "2026-10-01T07:30:00Z"},
			map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "报修", "content": "卡纸", "time": "2026-10-01T10:00:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
		},
		"next_ticket_seq": 2,
	}
	raw := mustMarshal(t, ledger)
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("乱序/间隔/时间不递增的有效停用台账应能加载: %v", err)
	}
	// 下一条履历序号应接续当前最大序号 9（间隔不补齐）。
	if _, err := s.reactivateAsset("EQ-1", "恢复"); err != nil {
		t.Fatalf("停用资产应能恢复: %v", err)
	}
	last := s.eventsOf("EQ-1")
	if last[len(last)-1].Seq != 10 {
		t.Fatalf("新履历序号应为 10，得到 %d", last[len(last)-1].Seq)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 矛盾台账：各种停用/恢复相关的矛盾都必须在加载时拒绝，且所有读写命令都失败、
// 原文件字节保持不变，不自动修复。
func TestDeactivationContradictoryLedgers(t *testing.T) {
	closedTicket := []any{
		map[string]any{
			"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-1",
			"status": "已关闭", "created_at": "2026-10-01T08:00:00Z",
			"result": "已修复", "closed_at": "2026-10-01T09:00:00Z",
		},
	}
	reportClose := func(deactSeq, reportSeq, closeSeq int, deactBetween bool) []any {
		// 返回 报修/关闭/停用 履历：deactBetween 时停用序号位于报修与关闭之间
		// （维修中停用，矛盾），否则位于关闭之后（合法顺序）。
		evs := []any{
			map[string]any{"seq": reportSeq, "asset_id": "EQ-1", "ticket_id": "T0001",
				"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
		}
		if deactBetween {
			evs = append(evs, map[string]any{"seq": deactSeq, "asset_id": "EQ-1",
				"kind": "停用", "content": "维修中停用", "from": "可用", "to": "停用",
				"time": "2026-10-01T08:30:00Z"})
		}
		evs = append(evs, map[string]any{"seq": closeSeq, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"})
		if !deactBetween {
			evs = append(evs, map[string]any{"seq": deactSeq, "asset_id": "EQ-1",
				"kind": "停用", "content": "停用", "from": "可用", "to": "停用",
				"time": "2026-10-01T10:00:00Z"})
		}
		return evs
	}
	requests := []any{
		map[string]any{"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
	}
	base := func(assetStatus string, tickets, events []any) map[string]any {
		return map[string]any{
			"version": 1,
			"assets": []any{
				map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": assetStatus},
			},
			"tickets":         tickets,
			"events":          events,
			"requests":        requests,
			"next_ticket_seq": 2,
		}
	}

	cases := []struct {
		name   string
		ledger map[string]any
	}{
		{
			name: "停用资产存在未关闭工单",
			ledger: base("停用", []any{
				map[string]any{"id": "T0001", "asset_id": "EQ-1", "description": "卡纸",
					"request_id": "req-1", "status": "未关闭", "created_at": "2026-10-01T08:00:00Z"},
			}, []any{
				map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
			}),
		},
		{
			name:   "维修中停用（停用履历夹在报修与关闭之间）",
			ledger: base("停用", closedTicket, reportClose(3, 1, 4, true)),
		},
		{
			name: "停用期间新报修",
			ledger: func() map[string]any {
				m := base("停用", append(append([]any{}, closedTicket...),
					map[string]any{"id": "T0002", "asset_id": "EQ-1", "description": "停用期间故障",
						"request_id": "req-2", "status": "已关闭", "created_at": "2026-10-02T08:00:00Z",
						"result": "已修复", "closed_at": "2026-10-02T10:00:00Z"}),
					[]any{
						map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
							"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
						map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
							"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
						map[string]any{"seq": 3, "asset_id": "EQ-1", "kind": "停用",
							"content": "停用", "from": "可用", "to": "停用", "time": "2026-10-01T10:00:00Z"},
						map[string]any{"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0002",
							"kind": "报修", "content": "停用期间故障", "time": "2026-10-02T08:00:00Z"},
						map[string]any{"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0002",
							"kind": "关闭", "content": "已修复", "time": "2026-10-02T10:00:00Z"},
					})
				m["requests"] = append(requests,
					map[string]any{"request_id": "req-2", "asset_id": "EQ-1", "description": "停用期间故障", "ticket_id": "T0002"})
				m["next_ticket_seq"] = 3
				return m
			}(),
		},
		{
			name: "保存状态与履历不符（履历可用，保存停用）",
			ledger: base("停用", closedTicket, []any{
				map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
				map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
			}),
		},
		{
			name: "停用履历原状态错误",
			ledger: base("停用", closedTicket, []any{
				map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
				map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
				map[string]any{"seq": 3, "asset_id": "EQ-1", "kind": "停用",
					"content": "停用", "from": "维修中", "to": "停用", "time": "2026-10-01T10:00:00Z"},
			}),
		},
		{
			name: "未停用却恢复",
			ledger: base("可用", closedTicket, []any{
				map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
				map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
					"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
				map[string]any{"seq": 3, "asset_id": "EQ-1", "kind": "恢复使用",
					"content": "恢复", "from": "停用", "to": "可用", "time": "2026-10-01T10:00:00Z"},
			}),
		},
		{
			name: "停用期间登记保养完成",
			ledger: func() map[string]any {
				m := base("停用", closedTicket, []any{
					map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001",
						"kind": "报修", "content": "卡纸", "time": "2026-10-01T08:00:00Z"},
					map[string]any{"seq": 2, "asset_id": "EQ-1", "ticket_id": "T0001",
						"kind": "关闭", "content": "已修复", "time": "2026-10-01T09:00:00Z"},
					map[string]any{"seq": 3, "asset_id": "EQ-1", "kind": "保养建立",
						"content": "更换滤芯", "due": "2026-11-01", "interval": 90,
						"time": "2026-10-01T12:00:00Z"},
					map[string]any{"seq": 4, "asset_id": "EQ-1", "kind": "停用",
						"content": "停用", "from": "可用", "to": "停用", "time": "2026-10-02T08:00:00Z"},
					map[string]any{"seq": 5, "asset_id": "EQ-1", "kind": "保养完成",
						"content": "已更换", "due": "2026-11-01", "done": "2026-11-03",
						"time": "2026-11-03T08:00:00Z"},
				})
				m["plans"] = []any{
					map[string]any{"asset_id": "EQ-1", "content": "更换滤芯", "first_due": "2026-11-01",
						"interval_days": 90, "next_due": "2027-01-30"},
				}
				return m
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := mustMarshal(t, tc.ledger)
			path := filepath.Join(dir, dataFileName)
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := openStore(dir); err == nil {
				t.Fatalf("矛盾台账应加载失败: %s", tc.name)
			}
			// 读命令与写命令都拒绝，退出码 1，原文件字节不变。
			var out, errBuf bytes.Buffer
			if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 1 {
				t.Fatalf("矛盾台账 list 应退出 1，得到 %d", code)
			}
			if code := run([]string{"reactivate", "--data-dir", dir,
				"--asset-id", "EQ-1", "--reason", "恢复"}, &out, &errBuf); code != 1 {
				t.Fatalf("矛盾台账 reactivate 应退出 1，得到 %d", code)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("原文件字节应保持不变: %s", tc.name)
			}
		})
	}
}

// 导入连同停用/恢复履历与当前状态复制；履历重编号、顺序与时间精度保留，
// 整批冲突拒绝；导入后可继续恢复或停用，源只读。
func TestImportDeactivationContinuation(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	for _, a := range [][3]string{
		{"EQ-OFF", "封存打印机", "仓库"}, {"EQ-ON", "空调", "二楼"},
	} {
		if _, err := src.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	// EQ-OFF：报修→关闭→停用（最终停用）；EQ-ON：保持可用。
	tk, _, err := src.report("EQ-OFF", "卡纸", "req-off")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.deactivateAsset("EQ-OFF", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)
	srcBefore := readFileBytes(t, srcDir)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-OFF", "EQ-ON"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 履历应整体重编号（源 3 条 → 目标 1..3），停用履历在其中。
	if len(outcome.assetIDs) != 2 {
		t.Fatalf("应导入 2 项资产，得到 %d", len(outcome.assetIDs))
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读，字节不变")
	}

	dst := newStoreAt(t, dstDir)
	if got := dst.findAsset("EQ-OFF").Status; got != statusDeactivated {
		t.Fatalf("导入资产应保持停用状态，得到 %q", got)
	}
	if got := dst.findAsset("EQ-ON").Status; got != statusAvailable {
		t.Fatalf("导入资产应保持可用状态，得到 %q", got)
	}
	evs := dst.eventsOf("EQ-OFF")
	if len(evs) != 3 || evs[2].Kind != eventDeactivate ||
		evs[2].From != statusAvailable || evs[2].To != statusDeactivated {
		t.Fatalf("停用履历应随导入复制并重编号: %+v", evs)
	}
	if evs[0].Seq != 1 || evs[2].Seq != 3 {
		t.Fatalf("履历应在目标从 1 开始重编号，得到 %d..%d", evs[0].Seq, evs[2].Seq)
	}

	// 导入后可继续恢复使用；请求重放返回映射后的原工单及当前状态。
	if _, err := dst.reactivateAsset("EQ-OFF", "恢复使用"); err != nil {
		t.Fatalf("导入的停用资产应能恢复: %v", err)
	}
	mapped := outcome.tickets[0].NewID
	old, replay, err := dst.report("EQ-OFF", "卡纸", "req-off")
	if err != nil || !replay || old.ID != mapped || old.Status != ticketClosed {
		t.Fatalf("导入后请求重放应返回映射工单 %s: %v replay=%v err=%v", mapped, old, replay, err)
	}
	// 导入的可用资产可继续停用。
	if _, err := dst.deactivateAsset("EQ-ON", "停用"); err != nil {
		t.Fatalf("导入的可用资产应能停用: %v", err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}
	dst2, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("接续操作后的台账应能重载: %v", err)
	}
	if dst2.findAsset("EQ-OFF").Status != statusAvailable ||
		dst2.findAsset("EQ-ON").Status != statusDeactivated {
		t.Fatalf("重载后状态不对: EQ-OFF=%s EQ-ON=%s",
			dst2.findAsset("EQ-OFF").Status, dst2.findAsset("EQ-ON").Status)
	}

	// 再次导入同一批资产按编号冲突整批拒绝，目标不变。
	dstBefore := readFileBytes(t, dstDir)
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-OFF"}); !errors.Is(err, errConflict) {
		t.Fatalf("重复导入应冲突拒绝，得到 %v", err)
	}
	if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
		t.Fatal("整批冲突拒绝时目标文件字节应不变")
	}
}

// CLI：命令输出、退出码与 list/detail/history 展示。
func TestDeactivateCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer

	runOrFail := func(codeWant int, args ...string) {
		t.Helper()
		out.Reset()
		errBuf.Reset()
		if code := run(args, &out, &errBuf); code != codeWant {
			t.Fatalf("run %v 退出码 = %d，想得到 %d；stderr=%s", args, code, codeWant, errBuf.String())
		}
	}

	runOrFail(0, "register", "--data-dir", dir, "--asset-id", "EQ-001", "--name", "打印机", "--location", "一楼")

	// 参数错误退出 2。
	runOrFail(2, "deactivate", "--data-dir", dir, "--asset-id", "EQ-001")
	runOrFail(2, "reactivate", "--data-dir", dir, "--reason", "恢复")

	// 未知资产业务失败退出 1。
	runOrFail(1, "deactivate", "--data-dir", dir, "--asset-id", "NOPE", "--reason", "停用")

	// 成功停用：显示资产编号与新状态。
	runOrFail(0, "deactivate", "--data-dir", dir, "--asset-id", "EQ-001", "--reason", "设备调拨")
	if s := out.String(); !strings.Contains(s, "EQ-001") || !strings.Contains(s, statusDeactivated) {
		t.Fatalf("停用输出应含资产编号与新状态: %s", s)
	}

	// list 显示停用状态。
	runOrFail(0, "list", "--data-dir", dir)
	if !strings.Contains(out.String(), statusDeactivated) {
		t.Fatalf("list 应显示停用状态: %s", out.String())
	}

	// detail 显示停用状态与计划区域。
	runOrFail(0, "detail", "--data-dir", dir, "--asset-id", "EQ-001")
	if !strings.Contains(out.String(), statusDeactivated) {
		t.Fatalf("detail 应显示停用状态: %s", out.String())
	}

	// 重复停用退出 1。
	runOrFail(1, "deactivate", "--data-dir", dir, "--asset-id", "EQ-001", "--reason", "再次停用")

	// history 展示停用事件的原状态、新状态与理由。
	runOrFail(0, "history", "--data-dir", dir, "--asset-id", "EQ-001")
	h := out.String()
	if !strings.Contains(h, eventDeactivate) || !strings.Contains(h, statusAvailable) ||
		!strings.Contains(h, statusDeactivated) || !strings.Contains(h, "设备调拨") {
		t.Fatalf("history 应展示停用事件: %s", h)
	}

	// 停用期间新报修退出 1。
	runOrFail(1, "report", "--data-dir", dir, "--asset-id", "EQ-001",
		"--description", "停用期间故障", "--request-id", "req-x")

	// 恢复成功，history 追加恢复事件。
	runOrFail(0, "reactivate", "--data-dir", dir, "--asset-id", "EQ-001", "--reason", "调拨回库")
	if !strings.Contains(out.String(), statusAvailable) {
		t.Fatalf("恢复输出应含可用状态: %s", out.String())
	}
	runOrFail(0, "history", "--data-dir", dir, "--asset-id", "EQ-001")
	if !strings.Contains(out.String(), eventReactivate) {
		t.Fatalf("history 应展示恢复使用事件: %s", out.String())
	}

	// 恢复后新报修成功，且使用停用期间被拒绝的同一请求标识（此前未绑定）。
	runOrFail(0, "report", "--data-dir", dir, "--asset-id", "EQ-001",
		"--description", "停用期间故障", "--request-id", "req-x")
	if !strings.Contains(out.String(), "T0001") {
		t.Fatalf("恢复后重试应开出 T0001: %s", out.String())
	}
}
