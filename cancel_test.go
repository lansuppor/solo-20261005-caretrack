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

func TestCancelLifecycle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}

	// 未知工单取消失败。
	if _, _, err := s.cancelTicket("T9999", "误报"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单应失败，得到 %v", err)
	}

	cancelled, asset, err := s.cancelTicket(tk.ID, "误报，设备实际正常")
	if err != nil {
		t.Fatalf("cancelTicket: %v", err)
	}
	if cancelled.Status != ticketCancelled || cancelled.CancelReason != "误报，设备实际正常" ||
		cancelled.CancelledAt == "" {
		t.Fatalf("取消后工单状态异常: %+v", cancelled)
	}
	if cancelled.Result != "" || cancelled.ClosedAt != "" {
		t.Fatal("取消不应填写维修结果或关闭时间")
	}
	if asset.Status != statusAvailable {
		t.Fatalf("取消后资产状态 = %q", asset.Status)
	}
	if got := s.openTicketOf("EQ-1"); got != nil {
		t.Fatalf("取消后不应有未关闭工单，得到 %v", got)
	}

	// 已取消工单不能再次取消，也不能关闭。
	if _, _, err := s.cancelTicket(tk.ID, "再次取消"); !errors.Is(err, errConflict) {
		t.Fatalf("重复取消应失败，得到 %v", err)
	}
	if _, _, err := s.closeTicket(tk.ID, "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单关闭应失败，得到 %v", err)
	}

	// 取消后可用新请求创建新工单。
	tk2, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay || tk2.ID != "T0002" {
		t.Fatalf("取消后新报修应开出 T0002: %v replay=%v err=%v", tk2, replay, err)
	}

	// 旧请求重放返回原工单的已取消状态，不影响新工单，不重新占用资产。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Status != ticketCancelled {
		t.Fatalf("重放应返回已取消的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 2 || len(s.data.Requests) != 2 {
		t.Fatalf("重放不应增加记录: tickets=%d requests=%d", len(s.data.Tickets), len(s.data.Requests))
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0002" {
		t.Fatalf("新工单 T0002 应仍为未关闭，得到 %v", got)
	}

	// 旧工单的失败操作不影响新工单：新工单可正常关闭。
	if _, _, err := s.closeTicket(tk2.ID, "已更换电源"); err != nil {
		t.Fatalf("新工单应可关闭: %v", err)
	}
	// 已关闭工单不能取消。
	if _, _, err := s.cancelTicket(tk2.ID, "太迟了"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单取消应失败，得到 %v", err)
	}
}

func TestCancelHistoryAndPersistence(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	if _, _, err := s.cancelTicket(tk.ID, "误报"); err != nil {
		t.Fatal(err)
	}
	tk2, _, _ := s.report("EQ-1", "无法开机", "req-2")
	if _, _, err := s.closeTicket(tk2.ID, "已修复"); err != nil {
		t.Fatal(err)
	}

	events := s.eventsOf("EQ-1")
	if len(events) != 4 {
		t.Fatalf("履历条数 = %d，想得到 4", len(events))
	}
	if events[0].Kind != eventReport || events[0].TicketID != "T0001" ||
		events[1].Kind != eventCancel || events[1].TicketID != "T0001" ||
		events[1].Content != "误报" ||
		events[2].Kind != eventReport || events[2].TicketID != "T0002" ||
		events[3].Kind != eventClose || events[3].TicketID != "T0002" {
		t.Fatalf("履历顺序/内容不对: %+v", events)
	}

	s = saveAndReopen(t, s)
	tk1 := s.findTicket("T0001")
	if tk1.Status != ticketCancelled || tk1.CancelReason != "误报" || tk1.CancelledAt == "" {
		t.Fatalf("重开后取消状态未保持: %+v", tk1)
	}
	if s.findAsset("EQ-1").Status != statusAvailable {
		t.Fatal("重开后资产应为可用")
	}
	// 重开后旧请求重放仍返回已取消的原工单。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Status != ticketCancelled {
		t.Fatalf("重开后重放应返回已取消的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	// 重开后已取消工单仍不能取消或关闭。
	if _, _, err := s.cancelTicket("T0001", "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重开后重复取消应失败，得到 %v", err)
	}
	if _, _, err := s.closeTicket("T0001", "结果"); !errors.Is(err, errConflict) {
		t.Fatalf("重开后已取消工单关闭应失败，得到 %v", err)
	}
	// 编号延续：下一张工单为 T0003。
	tk3, _, err := s.report("EQ-1", "又卡纸", "req-3")
	if err != nil || tk3.ID != "T0003" {
		t.Fatalf("重开后编号应延续为 T0003: %v err=%v", tk3, err)
	}
}

// 取消相关命令行行为：成功输出、参数错误退出码 2、业务失败退出码 1。
func TestCancelCommand(t *testing.T) {
	dir := t.TempDir()
	var out, errBuf bytes.Buffer

	// 准备数据。
	if code := run([]string{"register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼"}, &out, &errBuf); code != 0 {
		t.Fatalf("register: %s", errBuf.String())
	}
	out.Reset()
	if code := run([]string{"report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1"}, &out, &errBuf); code != 0 {
		t.Fatalf("report: %s", errBuf.String())
	}

	// 缺少取消理由：退出码 2。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T0001"}, &out, &errBuf); code != 2 {
		t.Fatalf("空理由退出码 = %d，应为 2（%s）", code, errBuf.String())
	}
	// 未知工单：退出码 1。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T9999", "--reason", "误报"}, &out, &errBuf); code != 1 {
		t.Fatalf("未知工单退出码 = %d，应为 1（%s）", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "未知工单编号") {
		t.Fatalf("应说明未知工单，得到 %s", errBuf.String())
	}
	// 成功取消：输出原工单编号与已取消状态。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T0001", "--reason", "误报"}, &out, &errBuf); code != 0 {
		t.Fatalf("取消应成功: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "T0001") || !strings.Contains(out.String(), "已取消") {
		t.Fatalf("成功输出应包含工单编号与已取消状态: %s", out.String())
	}
	// 详情中未关闭工单显示“无”。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"detail", "--data-dir", dir, "--asset-id", "EQ-1"}, &out, &errBuf); code != 0 {
		t.Fatalf("detail: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "未关闭工单: 无") || !strings.Contains(out.String(), "可用") {
		t.Fatalf("取消后详情异常: %s", out.String())
	}
	// 履历包含取消事件与理由。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"history", "--data-dir", dir, "--asset-id", "EQ-1"}, &out, &errBuf); code != 0 {
		t.Fatalf("history: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "取消") || !strings.Contains(out.String(), "误报") ||
		!strings.Contains(out.String(), "T0001") {
		t.Fatalf("履历应包含取消事件: %s", out.String())
	}
	// 重复取消：退出码 1 并说明原因。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T0001", "--reason", "再次"}, &out, &errBuf); code != 1 {
		t.Fatalf("重复取消退出码 = %d，应为 1", code)
	}
	if !strings.Contains(errBuf.String(), "已取消") {
		t.Fatalf("应说明已取消，得到 %s", errBuf.String())
	}
	// 已取消工单不能关闭。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"close", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "结果"}, &out, &errBuf); code != 1 {
		t.Fatalf("已取消工单关闭退出码 = %d，应为 1", code)
	}
	// 取消后可重新报修。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "无法开机", "--request-id", "req-2"}, &out, &errBuf); code != 0 {
		t.Fatalf("取消后应可重新报修: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "T0002") {
		t.Fatalf("新工单应为 T0002: %s", out.String())
	}
	// 旧请求重放返回已取消的原工单，不影响新单。
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1"}, &out, &errBuf); code != 0 {
		t.Fatalf("旧请求重放应成功: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "T0001") || !strings.Contains(out.String(), "已取消") {
		t.Fatalf("重放应返回已取消的 T0001: %s", out.String())
	}
}

// 工单编号耗尽时仍可取消已有工单。
func TestCancelWithTicketNumberExhaustion(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) { m["next_ticket_seq"] = math.MaxInt })
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("编号耗尽的合法台账应能加载: %v", err)
	}
	tk, asset, err := s.cancelTicket("T0001", "误报")
	if err != nil {
		t.Fatalf("编号耗尽不应影响取消已有工单: %v", err)
	}
	if tk.Status != ticketCancelled || asset.Status != statusAvailable {
		t.Fatalf("取消后状态异常: %s / %s", tk.Status, asset.Status)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
}

// 履历序号耗尽时拒绝取消，不留下任何变化；恢复条件后可重新取消。
func TestCancelEventSeqExhaustion(t *testing.T) {
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0001", "误报"); err == nil ||
		!strings.Contains(err.Error(), "履历序号") {
		t.Fatalf("履历序号耗尽应拒绝取消，得到 %v", err)
	}
	// 内存中的工单与资产状态未被修改。
	if s.findTicket("T0001").Status != ticketOpen || s.findAsset("EQ-1").Status != statusRepairing {
		t.Fatal("失败的取消不应修改工单或资产状态")
	}
	if len(s.data.Events) != 1 {
		t.Fatal("失败的取消不应追加履历")
	}
	// 原文件字节不变。
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的取消不应改动原文件")
	}
}

// 取消相关矛盾台账：加载与保存均拒绝并指出问题类别。
func TestCancelContradictionsRejected(t *testing.T) {
	cancelledTicket := func(m map[string]any) map[string]any {
		tk := m["tickets"].([]any)[0].(map[string]any)
		tk["status"] = "已取消"
		tk["cancel_reason"] = "误报"
		tk["cancelled_at"] = "2026-10-01T11:00:00Z"
		m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		return tk
	}
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"已取消工单缺少取消理由", func(m map[string]any) {
			tk := cancelledTicket(m)
			delete(tk, "cancel_reason")
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
		}, "状态矛盾"},
		{"已取消工单带有维修结果", func(m map[string]any) {
			tk := cancelledTicket(m)
			tk["result"] = "已修复"
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
		}, "状态矛盾"},
		{"已取消工单缺少取消履历", func(m map[string]any) { cancelledTicket(m) }, "履历矛盾"},
		{"已取消工单同时有关闭履历", func(m map[string]any) {
			cancelledTicket(m)
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
			appendTo(m, "events", eventJSONMap(5, "T0001", "关闭", "已修复"))
		}, "履历矛盾"},
		{"未关闭工单有取消履历", func(m map[string]any) {
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
		}, "履历矛盾"},
		{"取消履历早于报修履历", func(m map[string]any) {
			cancelledTicket(m)
			appendTo(m, "events", eventJSONMap(2, "T0001", "取消", "误报"))
		}, "履历矛盾"},
		{"取消履历内容与理由不一致", func(m map[string]any) {
			cancelledTicket(m)
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "别的理由"))
		}, "履历矛盾"},
		{"取消后资产仍维修中", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已取消"
			tk["cancel_reason"] = "误报"
			tk["cancelled_at"] = "2026-10-01T11:00:00Z"
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
		}, "状态矛盾"},
		{"取消后又报修但取消履历缺失", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已取消"
			tk["cancel_reason"] = "误报"
			tk["cancelled_at"] = "2026-10-01T11:00:00Z"
			appendTo(m, "tickets", openTicketJSON("T0002", "req-2"))
			appendTo(m, "requests", map[string]any{
				"request_id": "req-2", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0002",
			})
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
			appendTo(m, "events", eventJSONMap(5, "T0002", "报修", "卡纸"))
		}, ""}, // 合法：取消后可再报修，作为正向用例
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, tc.mutate)
			s, err := openStore(dir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("合法台账应能加载: %v", err)
				}
				if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0002" {
					t.Fatalf("取消后新工单 T0002 应为未关闭，得到 %v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应报 %q，得到 %v", tc.wantErr, err)
			}
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("原文件应保留不变")
			}
		})
	}
}
