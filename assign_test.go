package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 报修 -> 派工 -> 转派 -> 关闭：终结后保留最后负责人与完整派工履历。
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

	// 首次派工：原负责人为空（展示为“未派工”）。
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatalf("首次派工: %v", err)
	}
	// 转派给不同人员。
	tk, err = s.assignTicket("T0001", "李四", "张三请假，转派")
	if err != nil {
		t.Fatalf("转派: %v", err)
	}
	if tk.Assignee != "李四" || tk.AssignNote != "张三请假，转派" || tk.AssignedAt == "" {
		t.Fatalf("转派后工单记录不正确: %+v", tk)
	}
	// 派工不改变资产状态。
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("派工不应改变资产状态，得到 %q", got)
	}

	// 关闭后保留最后负责人与履历。
	if _, _, err := s.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if tk.Assignee != "李四" {
		t.Fatalf("关闭后应保留最后负责人，得到 %q", tk.Assignee)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}

	s2 := saveAndReopen(t, s)
	tk2 := s2.findTicket("T0001")
	if tk2.Assignee != "李四" || tk2.AssignNote != "张三请假，转派" || tk2.AssignedAt == "" {
		t.Fatalf("重载后负责人信息丢失: %+v", tk2)
	}
	events := s2.eventsOf("EQ-1")
	wantKinds := []string{eventReport, eventAssign, eventAssign, eventClose}
	if len(events) != len(wantKinds) {
		t.Fatalf("履历条数 = %d, 想得到 %d", len(events), len(wantKinds))
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Fatalf("履历 %d 类型 = %q, 想得到 %q", i, events[i].Kind, k)
		}
	}
	if events[1].From != "" || events[1].To != "张三" || events[1].Content != "首次派工" {
		t.Fatalf("首次派工履历不正确: %+v", events[1])
	}
	if events[2].From != "张三" || events[2].To != "李四" || events[2].Content != "张三请假，转派" {
		t.Fatalf("转派履历不正确: %+v", events[2])
	}
}

// 未派工工单可直接取消；已派工工单可取消，终态保留负责人。
func TestAssignThenCancel(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	// 未派工直接取消。
	if _, _, err := s.cancelTicket("T0001", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0002", "王五", "上门检修"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.cancelTicket("T0002", "设备已报废")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Assignee != "王五" {
		t.Fatalf("取消后应保留最后负责人，得到 %q", tk.Assignee)
	}
	s2 := saveAndReopen(t, s)
	if got := s2.findTicket("T0002").Assignee; got != "王五" {
		t.Fatalf("重载后已取消工单负责人 = %q, 想得到 王五", got)
	}
}

// 各类非法派工均被拒绝且不产生履历、不改变数据。
func TestAssignRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	before := len(s.data.Events)

	checks := []struct {
		name string
		id   string
		err  error
	}{
		{"未知工单", "T9999", errNotFound},
	}
	for _, c := range checks {
		if _, err := s.assignTicket(c.id, "张三", "说明"); !errors.Is(err, c.err) {
			t.Fatalf("%s 应失败(%v)，得到 %v", c.name, c.err, err)
		}
	}
	if _, err := s.assignTicket("T0001", "", "说明"); err == nil {
		t.Fatal("空人员应被拒绝")
	}
	if _, err := s.assignTicket("T0001", "张三", ""); err == nil {
		t.Fatal("空说明应被拒绝")
	}
	if _, err := s.assignTicket("T0001", "张三", "首次"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "重复"); !errors.Is(err, errConflict) {
		t.Fatalf("重复派给当前人员应冲突，得到 %v", err)
	}
	if len(s.data.Events) != before+1 {
		t.Fatalf("失败的派工不应产生履历，履历数 = %d, 想得到 %d", len(s.data.Events), before+1)
	}
	// 失败派工不消耗工单编号。
	if s.data.NextTicketSeq != 2 {
		t.Fatalf("派工不应消耗工单编号，NextTicketSeq = %d", s.data.NextTicketSeq)
	}

	// 已关闭工单不能派工。
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "李四", "关闭后派工"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单派工应冲突，得到 %v", err)
	}
	// 已取消工单不能派工。
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0002", "李四", "取消后派工"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单派工应冲突，得到 %v", err)
	}
	if got := s.findTicket("T0002").Assignee; got != "" {
		t.Fatalf("失败派工不应留下负责人，得到 %q", got)
	}
}

// 相同请求重放返回原工单及当前状态，不改负责人、不追加履历、不影响新单。
func TestReportReplayKeepsAssignee(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil {
		t.Fatal(err)
	}
	before := len(s.data.Events)

	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" {
		t.Fatalf("重放应返回原工单 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if old.Status != ticketClosed || old.Assignee != "张三" {
		t.Fatalf("重放不应改变负责人与状态: %+v", old)
	}
	if len(s.data.Events) != before {
		t.Fatal("重放不应追加履历")
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0002" {
		t.Fatal("重放旧请求不应影响新工单")
	}
	// 冲突请求仍拒绝。
	if _, _, err := s.report("EQ-1", "别的故障", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("冲突请求应拒绝，得到 %v", err)
	}
}

// 无派工字段的有效旧台账无需转换，作为未派工继续使用。
func TestLegacyLedgerWithoutAssignFields(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无派工字段的旧库应能加载: %v", err)
	}
	tk := s.findTicket("T0001")
	if tk.Assignee != "" || tk.AssignedAt != "" || tk.AssignNote != "" {
		t.Fatalf("旧库工单应未派工: %+v", tk)
	}
	// 旧库可直接派工并保存。
	if _, err := s.assignTicket("T0001", "张三", "旧库首次派工"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findTicket("T0001").Assignee; got != "张三" {
		t.Fatalf("重载后负责人 = %q, 想得到 张三", got)
	}
	events := s2.eventsOf("EQ-1")
	if len(events) != 2 || events[1].Kind != eventAssign || events[1].From != "" || events[1].To != "张三" {
		t.Fatalf("旧库派工履历不正确: %+v", events)
	}
}

// 派工链矛盾的数据在加载时报错并保留原文件。
func TestAssignChainContradiction(t *testing.T) {
	assignEvent := func(seq int, from, to string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "派工", "content": "说明", "from": from, "to": to,
			"time": "2026-10-01T10:00:00Z",
		}
	}
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"首次派工原负责人不为空", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "张三", "李四"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "李四"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "说明"
		}},
		{"转派原负责人不接续", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", "张三"))
			appendTo(m, "events", assignEvent(5, "王五", "李四"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "李四"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "说明"
		}},
		{"派工新负责人为空", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", ""))
		}},
		{"派工新旧负责人相同", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "张三", "张三"))
		}},
		{"工单负责人与履历不符", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", "张三"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "李四"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "说明"
		}},
		{"有派工履历但工单未派工", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", "张三"))
		}},
		{"工单已派工但无派工履历", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "说明"
		}},
		{"未派工工单带派工时间", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["assigned_at"] = "2026-10-01T10:00:00Z"
		}},
		{"已派工工单缺派工说明", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", "张三"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
		}},
		{"保存说明与最后派工履历不符", func(m map[string]any) {
			appendTo(m, "events", assignEvent(4, "", "张三"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T10:00:00Z"
			tk["assign_note"] = "另一份说明"
		}},
		{"终结后出现派工履历", func(m map[string]any) {
			cancelLedger(m)
			appendTo(m, "events", assignEvent(5, "", "张三"))
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T10:30:00Z"
			tk["assign_note"] = "说明"
		}},
		{"报修之前出现派工履历", func(m map[string]any) {
			ev := assignEvent(2, "", "张三")
			ev["time"] = "2026-10-01T07:00:00Z"
			appendTo(m, "events", ev)
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["assignee"] = "张三"
			tk["assigned_at"] = "2026-10-01T07:00:00Z"
			tk["assign_note"] = "说明"
		}},
		{"非派工履历带派工字段", func(m map[string]any) {
			m["events"].([]any)[0].(map[string]any)["to"] = "张三"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, c.mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾数据应报错")
			} else if !strings.Contains(err.Error(), "原文件已保留") {
				t.Fatalf("错误应说明保留原文件: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(raw) {
				t.Fatal("加载失败后原文件字节应不变")
			}
		})
	}
}

// 派工变化一次原子保存；失败不留部分变化，恢复后可重试，重载后状态保持。
func TestAssignFailureThenReload(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
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

	// 失败派工（重复派给当前人员）不产生任何变化，保存后文件字节不变。
	if _, err := s.assignTicket("T0001", "张三", "重复"); !errors.Is(err, errConflict) {
		t.Fatalf("重复派工应冲突，得到 %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawBefore) != string(rawAfter) {
		t.Fatal("失败派工后保存不应改变文件内容")
	}

	// 目录不可写时保存失败：内存中的转派不落入文件，恢复后可重试。
	if _, err := s.assignTicket("T0001", "李四", "转派"); err != nil {
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
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	rawNow, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawNow) != string(rawAfter) {
		t.Fatal("保存失败不应留下部分变化")
	}
	// 恢复写入条件后重试成功。
	if err := s.save(); err != nil {
		t.Fatalf("恢复后重试保存应成功: %v", err)
	}

	// 重载：负责人、履历与请求去重结果保持。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	tk := s2.findTicket("T0001")
	if tk.Assignee != "李四" || tk.AssignNote != "转派" {
		t.Fatalf("重载后负责人信息不正确: %+v", tk)
	}
	events := s2.eventsOf("EQ-1")
	if len(events) != 3 || events[2].Kind != eventAssign || events[2].From != "张三" || events[2].To != "李四" {
		t.Fatalf("重载后履历不正确: %+v", events)
	}
	old, replay, err := s2.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Assignee != "李四" {
		t.Fatalf("重载后请求去重应保持: %v replay=%v err=%v", old, replay, err)
	}
}

// 待保存数据若被破坏（派工链矛盾），保存拒绝且不写文件。
func TestSaveRejectsBrokenAssignChain(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
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
	// 直接篡改内存数据制造矛盾：负责人与履历不符。
	s.findTicket("T0001").Assignee = "李四"
	if err := s.save(); err == nil {
		t.Fatal("矛盾数据保存应失败")
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawBefore) != string(rawAfter) {
		t.Fatal("校验失败不应写入任何数据")
	}
}

// 序列化后的派工履历字段应使用 from/to 且空值省略。
func TestAssignEventJSONShape(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
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
	events := m["events"].([]any)
	assign := events[1].(map[string]any)
	if assign["kind"] != "派工" || assign["to"] != "张三" {
		t.Fatalf("派工履历序列化不正确: %v", assign)
	}
	if _, ok := assign["from"]; ok {
		t.Fatal("首次派工的空原负责人应省略 from 字段")
	}
	if _, ok := events[0].(map[string]any)["to"]; ok {
		t.Fatal("报修履历不应带有派工字段")
	}
	tk := m["tickets"].([]any)[0].(map[string]any)
	if tk["assignee"] != "张三" || tk["assigned_at"] == "" || tk["assign_note"] != "首次派工" {
		t.Fatalf("工单派工字段序列化不正确: %v", tk)
	}
}
