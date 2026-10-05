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

// 首次派工、转派、转派后终结：负责人与派工履历保留，重启后保持。
func TestAssignReassignThenClose(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Assignee != "" {
		t.Fatalf("新工单应未派工，得到 %q", tk.Assignee)
	}

	// 未知工单派工失败，不产生履历。
	if _, err := s.assignTicket("T9999", "张三", "去修"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单派工应失败，得到 %v", err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 1 {
		t.Fatalf("失败的派工不应产生履历，履历数 = %d", got)
	}

	// 首次派工：原负责人为“未派工”。
	assigned, err := s.assignTicket(tk.ID, "张三", "联系用户后上门")
	if err != nil {
		t.Fatalf("首次派工: %v", err)
	}
	if assigned.Assignee != "张三" || assigned.AssignedAt == "" || assigned.AssignNote != "联系用户后上门" {
		t.Fatalf("派工后工单字段异常: %+v", assigned)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("派工不应改变资产状态，得到 %q", got)
	}

	// 重复派给当前人员失败，不产生履历。
	if _, err := s.assignTicket(tk.ID, "张三", "再派一次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复派给当前人员应失败，得到 %v", err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 2 {
		t.Fatalf("失败的派工不应产生履历，履历数 = %d", got)
	}

	// 转派给不同人员。
	if _, err := s.assignTicket(tk.ID, "李四", "张三请假，转李四"); err != nil {
		t.Fatalf("转派: %v", err)
	}

	// 履历：报修、派工、派工按序号排列，含原/新负责人与说明。
	events := s.eventsOf("EQ-1")
	if len(events) != 3 {
		t.Fatalf("履历条数 = %d，想得到 3", len(events))
	}
	if events[1].Kind != eventAssign || events[1].FromAssignee != unassigned ||
		events[1].ToAssignee != "张三" || events[1].Content != "联系用户后上门" {
		t.Fatalf("首次派工履历不对: %+v", events[1])
	}
	if events[2].Kind != eventAssign || events[2].FromAssignee != "张三" ||
		events[2].ToAssignee != "李四" || events[2].Content != "张三请假，转李四" {
		t.Fatalf("转派履历不对: %+v", events[2])
	}

	// 转派后关闭：终结后保留最后负责人与派工履历。
	if _, _, err := s.closeTicket(tk.ID, "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	closed := s.findTicket(tk.ID)
	if closed.Assignee != "李四" || closed.AssignNote != "张三请假，转李四" || closed.AssignedAt == "" {
		t.Fatalf("关闭后应保留最后负责人: %+v", closed)
	}
	// 已关闭工单不能再派工。
	if _, err := s.assignTicket(tk.ID, "王五", "试试"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单派工应失败，得到 %v", err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatalf("失败的派工不应产生履历，履历数 = %d", got)
	}

	// 重启后负责人、履历保持；请求重放不改负责人、不追加履历。
	s = saveAndReopen(t, s)
	got := s.findTicket(tk.ID)
	if got.Assignee != "李四" || got.Status != ticketClosed {
		t.Fatalf("重开后负责人/状态未保持: %+v", got)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatal("重开后履历条数应保持")
	}
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != tk.ID || old.Assignee != "李四" {
		t.Fatalf("重放不应改负责人: %v replay=%v err=%v", old, replay, err)
	}
	if got := len(s.eventsOf("EQ-1")); got != 4 {
		t.Fatal("重放不应追加履历")
	}
}

// 未派工工单可直接关闭、取消；已取消工单不能派工；取消后保留负责人。
func TestAssignWithCancel(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")

	// 未派工工单可按原规则取消。
	if _, _, err := s.cancelTicket(tk.ID, "误报"); err != nil {
		t.Fatalf("未派工工单应可取消: %v", err)
	}
	if got := s.findTicket(tk.ID); got.Assignee != "" {
		t.Fatalf("取消的未派工工单负责人应为空，得到 %q", got.Assignee)
	}
	// 已取消工单派工失败。
	if _, err := s.assignTicket(tk.ID, "张三", "去修"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单派工应失败，得到 %v", err)
	}

	// 已派工工单取消后保留最后负责人。
	tk2, _, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk2.ID, "张三", "上门检查"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket(tk2.ID, "用户自行解决"); err != nil {
		t.Fatal(err)
	}
	got := s.findTicket(tk2.ID)
	if got.Status != ticketCancelled || got.Assignee != "张三" {
		t.Fatalf("取消后应保留最后负责人: %+v", got)
	}
	// 旧单误操作不影响后来的新单：对已取消旧单派工失败，新单不受影响。
	tk3, _, err := s.report("EQ-1", "又卡纸", "req-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket(tk2.ID, "李四", "误操作"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消旧单派工应失败，得到 %v", err)
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != tk3.ID || got.Assignee != "" {
		t.Fatalf("旧单误操作不应影响新单: %+v", got)
	}
}

// 无派工字段的有效旧台账无需转换即可加载，作为未派工继续使用。
func TestLegacyLedgerWithoutAssignFields(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无派工字段的旧库应能加载: %v", err)
	}
	tk := s.findTicket("T0001")
	if tk.Assignee != "" || tk.AssignedAt != "" || tk.AssignNote != "" {
		t.Fatalf("旧工单应视为未派工: %+v", tk)
	}
	// 旧库上首次派工，原负责人记为“未派工”，保存后重载保持。
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findTicket("T0001"); got.Assignee != "张三" {
		t.Fatalf("重载后负责人未保持: %+v", got)
	}
	events := s2.eventsOf("EQ-1")
	if len(events) != 2 || events[1].Kind != eventAssign || events[1].FromAssignee != unassigned {
		t.Fatalf("旧库派工履历不对: %+v", events)
	}
	// 派工不消耗工单编号：关闭后新报修仍从计数器 5 延续。
	if _, _, err := s2.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	tk2, _, err := s2.report("EQ-1", "无法开机", "req-2")
	if err != nil || tk2.ID != "T0005" {
		t.Fatalf("派工不应消耗工单编号，新单应为 T0005: %v err=%v", tk2, err)
	}
}

// 派工链相互矛盾的台账：加载即拒绝并保留原文件。
func TestAssignChainContradictions(t *testing.T) {
	// assignedLedger 把 baseLedger 改为合法的已派工台账（一次派工）。
	assignedLedger := func(m map[string]any) {
		tk := m["tickets"].([]any)[0].(map[string]any)
		tk["assignee"] = "张三"
		tk["assigned_at"] = "2026-10-01T10:00:00Z"
		tk["assign_note"] = "上门检查"
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "派工",
			"content": "上门检查", "from_assignee": "未派工", "to_assignee": "张三",
			"time": "2026-10-01T10:00:00Z",
		})
	}
	// reassignedLedger 在 assignedLedger 基础上再转派给李四。
	reassignedLedger := func(m map[string]any) {
		assignedLedger(m)
		tk := m["tickets"].([]any)[0].(map[string]any)
		tk["assignee"] = "李四"
		tk["assigned_at"] = "2026-10-01T11:00:00Z"
		tk["assign_note"] = "转李四"
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "派工",
			"content": "转李四", "from_assignee": "张三", "to_assignee": "李四",
			"time": "2026-10-01T11:00:00Z",
		})
	}
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"首次派工原负责人不是未派工", func(m map[string]any) {
			assignedLedger(m)
			m["events"].([]any)[1].(map[string]any)["from_assignee"] = "王五"
		}, "履历矛盾"},
		{"转派原负责人未接续上次记录", func(m map[string]any) {
			reassignedLedger(m)
			m["events"].([]any)[2].(map[string]any)["from_assignee"] = "王五"
		}, "履历矛盾"},
		{"派工新负责人为空", func(m map[string]any) {
			assignedLedger(m)
			delete(m["events"].([]any)[1].(map[string]any), "to_assignee")
		}, "履历矛盾"},
		{"派工新负责人与原负责人相同", func(m map[string]any) {
			reassignedLedger(m)
			ev := m["events"].([]any)[2].(map[string]any)
			ev["to_assignee"] = "张三"
			m["tickets"].([]any)[0].(map[string]any)["assignee"] = "张三"
		}, "履历矛盾"},
		{"派工履历在报修之前", func(m map[string]any) {
			assignedLedger(m)
			m["events"].([]any)[1].(map[string]any)["seq"] = 2
		}, "履历矛盾"},
		{"派工履历在关闭之后", func(m map[string]any) {
			assignedLedger(m)
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T12:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "关闭",
				"content": "已修复", "time": "2026-10-01T12:00:00Z",
			})
			// 派工履历（序号 4）晚于关闭（序号 5）？改为派工晚于关闭。
			m["events"].([]any)[1].(map[string]any)["seq"] = 6
		}, "履历矛盾"},
		{"派工履历在取消之后", func(m map[string]any) {
			assignedLedger(m)
			cancelLedger(m)
			m["events"].([]any)[1].(map[string]any)["seq"] = 6
		}, "履历矛盾"},
		{"履历推出的负责人与工单记录不一致", func(m map[string]any) {
			assignedLedger(m)
			m["tickets"].([]any)[0].(map[string]any)["assignee"] = "李四"
		}, "状态矛盾"},
		{"有派工履历但工单未派工", func(m map[string]any) {
			assignedLedger(m)
			tk := m["tickets"].([]any)[0].(map[string]any)
			delete(tk, "assignee")
			delete(tk, "assigned_at")
			delete(tk, "assign_note")
		}, "状态矛盾"},
		{"无派工履历但工单有负责人", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "上门检查"
		}, "状态矛盾"},
		{"已派工工单缺少派工时间", func(m map[string]any) {
			assignedLedger(m)
			delete(m["tickets"].([]any)[0].(map[string]any), "assigned_at")
		}, "状态矛盾"},
		{"未派工工单带有派工说明", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["assign_note"] = "不该有"
		}, "状态矛盾"},
		{"非派工履历带有负责人字段", func(m map[string]any) {
			m["events"].([]any)[0].(map[string]any)["to_assignee"] = "张三"
		}, "履历矛盾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, tc.mutate)
			_, err := openStore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应报 %q，得到 %v", tc.wantErr, err)
			}
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("矛盾数据应保留原文件不变: %v", err)
			}
		})
	}
}

// 合法的派工台账（含转派后关闭）应能加载。
func TestValidAssignedLedgerLoads(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		tk := m["tickets"].([]any)[0].(map[string]any)
		tk["status"] = "已关闭"
		tk["result"] = "已修复"
		tk["closed_at"] = "2026-10-01T12:00:00Z"
		tk["assignee"] = "李四"
		tk["assigned_at"] = "2026-10-01T11:00:00Z"
		tk["assign_note"] = "转李四"
		m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "派工",
			"content": "上门检查", "from_assignee": "未派工", "to_assignee": "张三",
			"time": "2026-10-01T10:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "派工",
			"content": "转李四", "from_assignee": "张三", "to_assignee": "李四",
			"time": "2026-10-01T11:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 6, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "关闭",
			"content": "已修复", "time": "2026-10-01T12:00:00Z",
		})
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法派工台账应能加载: %v", err)
	}
	got := s.findTicket("T0001")
	if got.Assignee != "李四" || got.Status != ticketClosed {
		t.Fatalf("终结后应保留最后负责人: %+v", got)
	}
}

// 派工写入失败不留下部分变化：原文件字节不变，重载后负责人保持，恢复后可重试。
func TestAssignSaveFailureReload(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
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
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.assignTicket("T0001", "张三", "上门检查"); err != nil {
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
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：工单仍未派工，无派工履历。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findTicket("T0001"); got.Assignee != "" {
		t.Fatalf("写入失败不应留下部分变化，负责人 = %q", got.Assignee)
	}
	if got := len(s2.eventsOf("EQ-1")); got != 1 {
		t.Fatalf("写入失败不应留下派工履历，履历数 = %d", got)
	}
	// 恢复写入条件后可重试并成功。
	if _, err := s2.assignTicket("T0001", "张三", "上门检查"); err != nil {
		t.Fatal(err)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findTicket("T0001"); got.Assignee != "张三" {
		t.Fatalf("重试后负责人应为张三，得到 %+v", got)
	}
}

// 命令行：assign/ticket/detail/history 的输出与退出码。
func TestAssignCommands(t *testing.T) {
	dir := t.TempDir()
	runOK := func(args ...string) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		if code := run(append(args, "--data-dir", dir), &out, &errBuf); code != 0 {
			t.Fatalf("%v 退出码 = %d: %s", args, code, errBuf.String())
		}
		return out.String()
	}
	runOK("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runOK("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")

	// 未派工时工单查询显示“未派工”。
	out := runOK("ticket", "--ticket-id", "T0001")
	if !strings.Contains(out, "工单状态: 未关闭") || !strings.Contains(out, "负责人: 未派工") {
		t.Fatalf("未派工工单查询输出异常: %s", out)
	}
	// 资产详情显示未关闭工单的负责人（未派工）。
	out = runOK("detail", "--asset-id", "EQ-1")
	if !strings.Contains(out, "未关闭工单: T0001（负责人: 未派工）") {
		t.Fatalf("详情输出异常: %s", out)
	}

	// 派工成功输出工单编号和负责人。
	out = runOK("assign", "--ticket-id", "T0001", "--assignee", "张三", "--note", "上门检查")
	if !strings.Contains(out, "工单编号: T0001") || !strings.Contains(out, "负责人: 张三") {
		t.Fatalf("派工输出异常: %s", out)
	}
	// 转派。
	runOK("assign", "--ticket-id", "T0001", "--assignee", "李四", "--note", "转李四")
	out = runOK("ticket", "--ticket-id", "T0001")
	if !strings.Contains(out, "负责人: 李四") || !strings.Contains(out, "派工说明: 转李四") {
		t.Fatalf("转派后查询输出异常: %s", out)
	}
	out = runOK("detail", "--asset-id", "EQ-1")
	if !strings.Contains(out, "未关闭工单: T0001（负责人: 李四）") {
		t.Fatalf("详情应显示当前负责人: %s", out)
	}
	// 履历中派工与报修按序号共同排列。
	out = runOK("history", "--asset-id", "EQ-1")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 { // 标题行 + 报修/派工/派工
		t.Fatalf("履历行数不对: %s", out)
	}
	if !strings.Contains(lines[1], "报修") || !strings.Contains(lines[2], "派工") ||
		!strings.Contains(lines[2], "未派工 → 张三：上门检查") ||
		!strings.Contains(lines[3], "派工") || !strings.Contains(lines[3], "张三 → 李四：转李四") {
		t.Fatalf("履历输出异常: %s", out)
	}

	// 关闭后工单查询显示最终负责人，资产详情显示“无”。
	runOK("close", "--ticket-id", "T0001", "--repair-result", "已更换搓纸轮")
	out = runOK("ticket", "--ticket-id", "T0001")
	if !strings.Contains(out, "工单状态: 已关闭") || !strings.Contains(out, "负责人: 李四") {
		t.Fatalf("关闭后查询应显示最终负责人: %s", out)
	}
	out = runOK("detail", "--asset-id", "EQ-1")
	if !strings.Contains(out, "未关闭工单: 无") {
		t.Fatalf("无未关闭工单时应显示“无”: %s", out)
	}

	// 参数错误退出码 2：空人员、空说明。
	for _, args := range [][]string{
		{"assign", "--ticket-id", "T0001", "--note", "说明"},
		{"assign", "--ticket-id", "T0001", "--assignee", "张三"},
	} {
		var out, errBuf bytes.Buffer
		if code := run(append(args, "--data-dir", dir), &out, &errBuf); code != 2 {
			t.Fatalf("%v 退出码 = %d，应为 2", args, code)
		}
	}
	// 业务失败退出码 1：未知工单、重复派给当前人员、已关闭工单。
	for _, args := range [][]string{
		{"assign", "--ticket-id", "T9999", "--assignee", "张三", "--note", "去修"},
		{"assign", "--ticket-id", "T0001", "--assignee", "李四", "--note", "重复"},
		{"ticket", "--ticket-id", "T9999"},
	} {
		var out, errBuf bytes.Buffer
		if code := run(append(args, "--data-dir", dir), &out, &errBuf); code != 1 {
			t.Fatalf("%v 退出码 = %d，应为 1", args, code)
		}
	}
	// 失败的派工不产生履历。
	out = runOK("history", "--asset-id", "EQ-1")
	if got := strings.Count(out, "] 派工 工单"); got != 2 {
		t.Fatalf("失败的派工不应产生履历，履历中派工条数 = %d: %s", got, out)
	}
}

// 请求重放不追加履历、不改负责人；派工不影响报修去重绑定。
func TestAssignDoesNotAffectRequestDedup(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	if _, err := s.assignTicket(tk.ID, "张三", "上门"); err != nil {
		t.Fatal(err)
	}
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != tk.ID || old.Assignee != "张三" {
		t.Fatalf("重放应返回原工单且不改负责人: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 1 || len(s.data.Requests) != 1 || len(s.data.Events) != 2 {
		t.Fatal("重放不应新增工单、绑定或履历")
	}
	// 派工后冲突请求仍被拒绝。
	if _, _, err := s.report("EQ-1", "别的故障", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("冲突请求应被拒绝，得到 %v", err)
	}
	// 持久化后 JSON 中派工字段与履历齐全。
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	tj := m["tickets"].([]any)[0].(map[string]any)
	if tj["assignee"] != "张三" || tj["assigned_at"] == "" || tj["assign_note"] != "上门" {
		t.Fatalf("持久化工单派工字段不全: %v", tj)
	}
	ev := m["events"].([]any)[1].(map[string]any)
	if ev["kind"] != "派工" || ev["from_assignee"] != "未派工" || ev["to_assignee"] != "张三" {
		t.Fatalf("持久化派工履历不全: %v", ev)
	}
}
