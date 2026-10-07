package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 停用与恢复的基本生命周期：状态转换、履历追加、各类拒绝与履历保留。
func TestDecommissionRestoreLifecycle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}

	// 未知资产、空理由拒绝。
	if _, err := s.decommissionAsset("NOPE", "淘汰"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产停用应失败，得到 %v", err)
	}
	if _, err := s.decommissionAsset("EQ-1", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由停用应失败，得到 %v", err)
	}
	if _, err := s.restoreAsset("EQ-1", "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("未停用时恢复应失败，得到 %v", err)
	}

	// 维修中不能停用。
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); !errors.Is(err, errConflict) {
		t.Fatalf("维修中停用应失败，得到 %v", err)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("失败的停用不应改变状态，得到 %q", got)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}

	// 可用资产停用成功：状态变为停用，追加资产级履历。
	a, err := s.decommissionAsset("EQ-1", "设备淘汰，待处置")
	if err != nil {
		t.Fatalf("decommissionAsset: %v", err)
	}
	if a.Status != statusDecommissioned {
		t.Fatalf("停用后状态 = %q", a.Status)
	}
	// 重复停用拒绝。
	if _, err := s.decommissionAsset("EQ-1", "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复停用应失败，得到 %v", err)
	}

	// 恢复成功：状态变为可用。
	a, err = s.restoreAsset("EQ-1", "重新启用")
	if err != nil {
		t.Fatalf("restoreAsset: %v", err)
	}
	if a.Status != statusAvailable {
		t.Fatalf("恢复后状态 = %q", a.Status)
	}
	// 重复恢复拒绝。
	if _, err := s.restoreAsset("EQ-1", "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复恢复应失败，得到 %v", err)
	}

	// 履历：停用、恢复事件按序号共同展示，含原状态、新状态与理由；恢复后不删除。
	events := s.eventsOf("EQ-1")
	if len(events) != 4 {
		t.Fatalf("履历条数 = %d，想得到 4", len(events))
	}
	de, re := events[2], events[3]
	if de.Kind != eventDecommission || de.TicketID != "" ||
		de.From != statusAvailable || de.To != statusDecommissioned || de.Content != "设备淘汰，待处置" {
		t.Fatalf("停用履历不对: %+v", de)
	}
	if re.Kind != eventRestore || re.TicketID != "" ||
		re.From != statusDecommissioned || re.To != statusAvailable || re.Content != "重新启用" {
		t.Fatalf("恢复履历不对: %+v", re)
	}
	if de.Time.IsZero() || re.Time.IsZero() {
		t.Fatal("停用、恢复履历应保留操作时间")
	}

	// 停用、恢复不消耗工单编号：恢复后报修仍从下一序号继续。
	tk2, _, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || tk2.ID != "T0002" {
		t.Fatalf("恢复后报修应开出 T0002: %v err=%v", tk2, err)
	}
}

// 停用期间拒绝新报修且不绑定请求标识；已有请求的相同重放仍返回原工单；
// 恢复后可用同一标识重试。
func TestDecommissionBlocksReportButNotReplay(t *testing.T) {
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
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}

	// 新报修拒绝，不绑定请求标识。
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间新报修应失败，得到 %v", err)
	}
	if s.findRequest("req-2") != nil {
		t.Fatal("被拒绝的报修不应绑定请求标识")
	}

	// 已有请求的相同重放：即使资产停用仍返回原工单及当前状态，不开单、不改状态。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Status != ticketClosed {
		t.Fatalf("停用期间重放应返回已关闭的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 1 || len(s.data.Requests) != 1 {
		t.Fatal("重放不应新增工单或请求绑定")
	}
	if got := s.findAsset("EQ-1").Status; got != statusDecommissioned {
		t.Fatalf("重放不应改变资产状态，得到 %q", got)
	}
	// 冲突请求（同标识不同描述）仍拒绝。
	if _, _, err := s.report("EQ-1", "别的故障", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("冲突请求应拒绝，得到 %v", err)
	}

	// 恢复后可用同一标识重试报修。
	if _, err := s.restoreAsset("EQ-1", "重新启用"); err != nil {
		t.Fatal(err)
	}
	tk2, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay || tk2.ID != "T0002" {
		t.Fatalf("恢复后重试应开出 T0002: %v replay=%v err=%v", tk2, replay, err)
	}
}

// 停用不删除工单、负责人、备件或附件记录；终态工单在停用期间仍可补充、
// 撤销附件。
func TestDecommissionKeepsTicketRecords(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk.ID, "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withdrawPart(tk.ID, "FILTER-01", 2, "更换滤芯"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}

	// 记录保留。
	got := s.findTicket(tk.ID)
	if got.Assignee != "张三" || got.Status != ticketClosed {
		t.Fatalf("停用不应改动工单记录: %+v", got)
	}
	if parts := s.partsOf(tk.ID); len(parts) != 1 || parts[0].Quantity != 2 {
		t.Fatalf("停用不应改动备件记录: %+v", parts)
	}

	// 终态工单仍可补充、撤销附件。
	file := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := s.attach(tk.ID, file, "维修照片")
	if err != nil {
		t.Fatalf("停用期间终态工单补充附件应成功: %v", err)
	}
	if _, err := s.revokeAttachment(att.ID, "拍错设备"); err != nil {
		t.Fatalf("停用期间撤销附件应成功: %v", err)
	}
}

// 停用、恢复不改变保养计划；停用资产可建立计划、撤销已登记完成，due 排除
// 停用资产；恢复后按保存的下一到期日参与到期查询，完成沿用原推进规则。
func TestDecommissionMaintenanceInterleaving(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	// 先完成一个周期，下一到期日推进到 2027-01-30。
	if _, _, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-11-03", "已更换"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}

	// 停用期间拒绝保养完成登记。
	if _, _, _, err := s.completePlan("EQ-1", "2027-01-30", "2027-02-01", "再次更换"); !errors.Is(err, errConflict) {
		t.Fatalf("停用期间登记完成应失败，得到 %v", err)
	}
	// 计划内容、间隔与下一到期日不变。
	p := s.findPlan("EQ-1")
	if p.NextDue != "2027-01-30" || p.IntervalDays != 90 || p.Content != "更换滤芯" {
		t.Fatalf("停用不应改变保养计划: %+v", p)
	}
	// 停用期间可按原规则撤销已登记完成（保养回退）。
	if _, _, err := s.revokeCompletion("EQ-1", 2, "误登记"); err != nil {
		t.Fatalf("停用期间撤销完成应成功: %v", err)
	}
	if got := s.findPlan("EQ-1").NextDue; got != "2026-11-01" {
		t.Fatalf("撤销后下一到期日应恢复为 2026-11-01，得到 %s", got)
	}
	// 停用资产仍可建立计划（EQ-1 停用期间建立会被拒绝重复建立，故用 EQ-2 正常
	// 建立作对照；停用资产的建立在 TestDecommissionedAssetCanCreatePlan 覆盖）。
	if _, err := s.createPlan("EQ-2", "清洗滤网", "2026-10-01", 30); err != nil {
		t.Fatalf("建立计划应成功: %v", err)
	}
	// due 排除停用资产：EQ-1 到期但不出现，EQ-2 正常出现。
	rows := s.duePlans("2026-12-31")
	if len(rows) != 1 || rows[0].AssetID != "EQ-2" {
		t.Fatalf("due 应排除停用资产，得到 %+v", rows)
	}

	// 恢复后按保存的下一到期日参与到期查询：逾期计划仍显示原到期日。
	if _, err := s.restoreAsset("EQ-1", "重新启用"); err != nil {
		t.Fatal(err)
	}
	rows = s.duePlans("2026-12-31")
	if len(rows) != 2 || rows[0].AssetID != "EQ-2" || rows[1].AssetID != "EQ-1" || rows[1].Due != "2026-11-01" {
		t.Fatalf("恢复后 due 应包含 EQ-1 原到期日，得到 %+v", rows)
	}
	// 随后完成沿用按首次到期日和间隔推进的规则。
	_, next, _, err := s.completePlan("EQ-1", "2026-11-01", "2026-12-01", "已更换")
	if err != nil {
		t.Fatalf("恢复后登记完成应成功: %v", err)
	}
	if next != "2027-01-30" {
		t.Fatalf("下一到期日应为 2027-01-30，得到 %s", next)
	}
}

// 停用资产仍可建立保养计划（维修中与可用资产同样可以）。
func TestDecommissionedAssetCanCreatePlan(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatalf("停用资产建立计划应成功: %v", err)
	}
	if got := s.findPlan("EQ-1").NextDue; got != "2026-11-01" {
		t.Fatalf("下一到期日 = %s", got)
	}
	// detail 展示计划（store 层）：计划存在；due 仍排除。
	if rows := s.duePlans("2026-12-31"); len(rows) != 0 {
		t.Fatalf("停用资产的计划不应出现在 due 中，得到 %+v", rows)
	}
}

// 停机统计仍只计算工单占用：停用区间不计入。
func TestDowntimeIgnoresDecommission(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1") // t+1s
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil { // t+2s
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil { // t+3s
		t.Fatal(err)
	}
	if _, err := s.restoreAsset("EQ-1", "重新启用"); err != nil { // t+4s
		t.Fatal(err)
	}
	start := s.data.Events[0].Time.Add(-3600 * 1e9)
	end := s.data.Events[3].Time.Add(3600 * 1e9)
	results, err := s.downtimeForAssets([]string{"EQ-1"}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	// 只有报修履历到关闭履历的区间计入；停用区间不计入。
	report := s.data.Events[0].Time
	closed := s.data.Events[1].Time
	want := int64(closed.Sub(report).Seconds())
	if results[0].Seconds != want {
		t.Fatalf("停机秒数应只含工单占用（%d 秒），得到 %d", want, results[0].Seconds)
	}
}

// 重启后停用、恢复状态与履历保持；有效旧库（含停用履历链、序号间隔、数组
// 乱序、时间不递增）无需转换即可加载并继续操作。
func TestDecommissionPersistenceAndLegacyLedger(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.restoreAsset("EQ-1", "重新启用"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.decommissionAsset("EQ-1", "再次淘汰"); err != nil {
		t.Fatal(err)
	}
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1").Status; got != statusDecommissioned {
		t.Fatalf("重开后状态应保持停用，得到 %q", got)
	}
	events := s.eventsOf("EQ-1")
	if len(events) != 3 || events[0].Kind != eventDecommission ||
		events[1].Kind != eventRestore || events[2].Kind != eventDecommission {
		t.Fatalf("重开后履历链不对: %+v", events)
	}
	// 重开后可继续恢复。
	if _, err := s.restoreAsset("EQ-1", "再次启用"); err != nil {
		t.Fatalf("重开后恢复应成功: %v", err)
	}
	mustSave(t, s)

	// 旧库：无停用、恢复履历的台账无需转换即可加载，之后可正常停用。
	dir := t.TempDir()
	writeLedger(t, dir, cancelLedger) // EQ-1 可用，T0001 已取消
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧库应能加载: %v", err)
	}
	if _, err := s2.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatalf("旧库停用应成功: %v", err)
	}
	mustSave(t, s2)

	// 含停用履历链的旧库：序号间隔、数组乱序、时间不递增均合法。
	dir2 := t.TempDir()
	writeLedger(t, dir2, func(m map[string]any) {
		m["assets"] = []any{map[string]any{
			"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "停用",
		}}
		m["tickets"] = []any{}
		m["requests"] = []any{}
		m["next_ticket_seq"] = 1
		m["events"] = []any{
			map[string]any{"seq": 9, "asset_id": "EQ-1", "kind": "停用", "content": "再次淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-01T08:00:00Z"},
			map[string]any{"seq": 2, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-03T08:00:00Z"},
			map[string]any{"seq": 5, "asset_id": "EQ-1", "kind": "恢复", "content": "启用",
				"from": "停用", "to": "可用", "time": "2026-10-02T08:00:00Z"},
		}
	})
	s3, err := openStore(dir2)
	if err != nil {
		t.Fatalf("含停用履历链的旧库应能加载: %v", err)
	}
	if got := s3.findAsset("EQ-1").Status; got != statusDecommissioned {
		t.Fatalf("旧库推导状态应为停用，得到 %q", got)
	}
	if _, err := s3.restoreAsset("EQ-1", "启用"); err != nil {
		t.Fatalf("旧库恢复应成功: %v", err)
	}
	mustSave(t, s3)
}

// 矛盾台账（停用状态链）拒绝加载与读写，原文件字节保留，不自动修复。
func TestDecommissionContradictoryLedgerRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"停用资产有未关闭工单", func(m map[string]any) {
			// 报修后未终结就停用，保存状态为停用。
			m["assets"].([]any)[0].(map[string]any)["status"] = "停用"
			appendTo(m, "events", map[string]any{
				"seq": 4, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-01T10:00:00Z",
			})
		}},
		{"停用期间新报修", func(m map[string]any) {
			// T0001 已取消；先停用（seq 5），再报修 T0005（seq 6）。
			cancelLedger(m)
			m["assets"].([]any)[0].(map[string]any)["status"] = "维修中"
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-01T12:00:00Z",
			})
			appendTo(m, "tickets", openTicketJSON("T0005", "req-2"))
			appendTo(m, "requests", map[string]any{
				"request_id": "req-2", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0005",
			})
			appendTo(m, "events", eventJSONMap(6, "T0005", "报修", "卡纸"))
		}},
		{"停用履历状态链不符", func(m map[string]any) {
			cancelLedger(m)
			m["assets"].([]any)[0].(map[string]any)["status"] = "停用"
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "维修中", "to": "停用", "time": "2026-10-01T12:00:00Z",
			})
		}},
		{"重复停用", func(m map[string]any) {
			cancelLedger(m)
			m["assets"].([]any)[0].(map[string]any)["status"] = "停用"
			for i, seq := range []int{5, 6} {
				appendTo(m, "events", map[string]any{
					"seq": seq, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
					"from": "可用", "to": "停用", "time": "2026-10-01T1" + string(rune('2'+i)) + ":00:00Z",
				})
			}
		}},
		{"未停用即恢复", func(m map[string]any) {
			cancelLedger(m)
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "恢复", "content": "启用",
				"from": "停用", "to": "可用", "time": "2026-10-01T12:00:00Z",
			})
		}},
		{"推导状态与保存状态不符", func(m map[string]any) {
			// T0001 已取消；有停用履历但保存状态仍为可用。
			cancelLedger(m)
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-01T12:00:00Z",
			})
		}},
		{"停用期间登记保养完成", func(m map[string]any) {
			cancelLedger(m)
			m["assets"].([]any)[0].(map[string]any)["status"] = "停用"
			m["plans"] = []any{map[string]any{
				"asset_id": "EQ-1", "content": "更换滤芯", "first_due": "2026-11-01",
				"interval_days": 90, "next_due": "2027-01-30",
			}}
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
				"due": "2026-11-01", "interval": 90, "time": "2026-10-01T12:00:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 6, "asset_id": "EQ-1", "kind": "停用", "content": "淘汰",
				"from": "可用", "to": "停用", "time": "2026-10-01T13:00:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养完成", "content": "已更换",
				"due": "2026-11-01", "done": "2026-11-03", "time": "2026-10-01T14:00:00Z",
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := writeLedger(t, dir, tc.mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			}
			// 所有读写命令拒绝且原文件字节保留。
			var out, errOut bytes.Buffer
			if code := run([]string{"list", "--data-dir", dir}, &out, &errOut); code != 1 {
				t.Fatalf("矛盾台账读取应退出 1，得到 %d", code)
			}
			out.Reset()
			errOut.Reset()
			if code := run([]string{"restore", "--data-dir", dir, "--asset-id", "EQ-1", "--reason", "x"}, &out, &errOut); code != 1 {
				t.Fatalf("矛盾台账写入应退出 1，得到 %d", code)
			}
			if got := readFileBytes(t, dir); !bytes.Equal(got, before) {
				t.Fatal("矛盾台账的原文件字节应保持不变")
			}
		})
	}
}

// 导入连同所选资产复制停用、恢复履历和当前状态；导入后可在目标继续恢复或
// 停用，源只读。
func TestImportDecommissionedAsset(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.decommissionAsset("EQ-A", "淘汰"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.restoreAsset("EQ-A", "启用"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.decommissionAsset("EQ-A", "再次淘汰"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)
	srcBefore := readFileBytes(t, srcDir)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("importAssets: %v", err)
	}
	if len(outcome.tickets) != 1 || outcome.tickets[0].OldID != "T0001" || outcome.tickets[0].NewID != "T0001" {
		t.Fatalf("工单映射不对: %+v", outcome.tickets)
	}
	// 源只读。
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("导入不应改动源台账")
	}

	dst, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dst.findAsset("EQ-A").Status; got != statusDecommissioned {
		t.Fatalf("导入后资产应保持停用，得到 %q", got)
	}
	events := dst.eventsOf("EQ-A")
	if len(events) != 5 || events[2].Kind != eventDecommission ||
		events[3].Kind != eventRestore || events[4].Kind != eventDecommission {
		t.Fatalf("导入后履历链不对: %+v", events)
	}
	// 履历时间保留原瞬间。
	if !events[2].Time.Equal(src.eventsOf("EQ-A")[2].Time) {
		t.Fatal("导入应保留履历时间")
	}
	// 停用期间拒绝新报修；恢复后可报修，也可再次停用。
	if _, _, err := dst.report("EQ-A", "无法开机", "req-a2"); !errors.Is(err, errConflict) {
		t.Fatalf("导入的停用资产报修应失败，得到 %v", err)
	}
	if _, err := dst.restoreAsset("EQ-A", "重新启用"); err != nil {
		t.Fatalf("导入后恢复应成功: %v", err)
	}
	tk, _, err := dst.report("EQ-A", "无法开机", "req-a2")
	if err != nil || tk.ID != "T0002" {
		t.Fatalf("恢复后报修应开出 T0002: %v err=%v", tk, err)
	}
	if _, _, err := dst.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.decommissionAsset("EQ-A", "再次淘汰"); err != nil {
		t.Fatalf("导入后再次停用应成功: %v", err)
	}
	mustSave(t, dst)

	// 再次导入同一批资产按编号冲突拒绝。
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A"}); !errors.Is(err, errConflict) {
		t.Fatalf("再次导入应冲突拒绝，得到 %v", err)
	}
}

// 失败路径不留下部分状态、不消耗履历序号，原文件字节不变，恢复条件后可重试。
func TestDecommissionFailureAndRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	dir := s.dir
	before := readFileBytes(t, dir)

	// 业务拒绝（恢复未停用资产）不改动文件、不消耗履历序号。
	if _, err := s.restoreAsset("EQ-1", "理由"); !errors.Is(err, errConflict) {
		t.Fatalf("未停用时恢复应失败，得到 %v", err)
	}
	if got := readFileBytes(t, dir); !bytes.Equal(got, before) {
		t.Fatal("失败操作不应改动原文件字节")
	}
	if _, err := s.decommissionAsset("EQ-1", "淘汰"); err != nil {
		t.Fatal(err)
	}
	// 履历序号为 1：失败的恢复没有消耗序号。
	events := s.eventsOf("EQ-1")
	if len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("失败操作不应消耗履历序号: %+v", events)
	}
	mustSave(t, s)

	// 写入失败：目录只读时保存失败，原文件字节不变，恢复后可重试。
	before = readFileBytes(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.restoreAsset("EQ-1", "重新启用"); err != nil {
		t.Fatal(err)
	}
	saveErr := s2.save()
	if cerr := os.Chmod(dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if got := readFileBytes(t, dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if got := s3.findAsset("EQ-1").Status; got != statusDecommissioned {
		t.Fatalf("写入失败不应留下部分状态，得到 %q", got)
	}
	if _, err := s3.restoreAsset("EQ-1", "重新启用"); err != nil {
		t.Fatalf("恢复条件后重试应成功: %v", err)
	}
	mustSave(t, s3)
}

// 命令行入口：参数错误退出 2，业务失败退出 1；成功显示资产编号与新状态；
// list、detail 显示停用状态，history 展示停用、恢复履历。
func TestDecommissionCommandLine(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runCmd := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := run(append(args, "--data-dir", dir), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	runCmd("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")

	// 参数错误：缺少 --asset-id 或 --reason，退出 2。
	if code, _, _ := runCmd("decommission", "--asset-id", "EQ-1"); code != 2 {
		t.Fatalf("缺少 --reason 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("restore", "--reason", "x"); code != 2 {
		t.Fatalf("缺少 --asset-id 应退出 2，得到 %d", code)
	}
	// 业务失败：未知资产、未停用即恢复，退出 1。
	if code, _, _ := runCmd("decommission", "--asset-id", "NOPE", "--reason", "x"); code != 1 {
		t.Fatalf("未知资产停用应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("restore", "--asset-id", "EQ-1", "--reason", "x"); code != 1 {
		t.Fatalf("未停用即恢复应退出 1，得到 %d", code)
	}

	// 停用成功：显示资产编号与新状态。
	code, o, _ := runCmd("decommission", "--asset-id", "EQ-1", "--reason", "设备淘汰")
	if code != 0 || !strings.Contains(o, "EQ-1") || !strings.Contains(o, "停用") {
		t.Fatalf("停用应成功并显示编号与状态，得到 %d:\n%s", code, o)
	}
	// 重复停用退出 1。
	if code, _, _ := runCmd("decommission", "--asset-id", "EQ-1", "--reason", "再次"); code != 1 {
		t.Fatalf("重复停用应退出 1，得到 %d", code)
	}
	// list、detail 显示停用状态。
	code, o, _ = runCmd("list")
	if code != 0 || !strings.Contains(o, "EQ-1\t打印机\t一楼\t停用") {
		t.Fatalf("list 应显示停用状态，得到 %d:\n%s", code, o)
	}
	code, o, _ = runCmd("detail", "--asset-id", "EQ-1")
	if code != 0 || !strings.Contains(o, "当前状态: 停用") {
		t.Fatalf("detail 应显示停用状态，得到 %d:\n%s", code, o)
	}
	// 恢复成功；history 展示停用、恢复履历且恢复后不删除。
	code, o, _ = runCmd("restore", "--asset-id", "EQ-1", "--reason", "重新启用")
	if code != 0 || !strings.Contains(o, "EQ-1") || !strings.Contains(o, "可用") {
		t.Fatalf("恢复应成功并显示编号与状态，得到 %d:\n%s", code, o)
	}
	code, o, _ = runCmd("history", "--asset-id", "EQ-1")
	if code != 0 || !strings.Contains(o, "停用: 可用 -> 停用（设备淘汰）") ||
		!strings.Contains(o, "恢复: 停用 -> 可用（重新启用）") {
		t.Fatalf("history 应展示停用、恢复履历，得到 %d:\n%s", code, o)
	}
}
