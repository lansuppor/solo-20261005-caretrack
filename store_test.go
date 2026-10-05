package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	clock := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	return s
}

func saveAndReopen(t *testing.T, s *store) *store {
	t.Helper()
	if err := s.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.now = s.now
	return s2
}

func TestRegisterAndList(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼大厅"); err != nil {
		t.Fatalf("register: %v", err)
	}
	a2, err := s.registerAsset("EQ-2", "空调", "二楼会议室")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if a2.Status != statusAvailable {
		t.Fatalf("新资产状态 = %q, 想得到 %q", a2.Status, statusAvailable)
	}
	if _, err := s.registerAsset("EQ-1", "x", "y"); !errors.Is(err, errConflict) {
		t.Fatalf("重复编号应冲突，得到 %v", err)
	}
	if len(s.data.Assets) != 2 {
		t.Fatalf("失败登记不应产生记录，资产数 = %d", len(s.data.Assets))
	}
}

func TestReportLifecycleAndDedup(t *testing.T) {
	s := newTestStore(t)

	// 未知资产报修失败。
	if _, _, err := s.report("NOPE", "坏了", "req-1"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	if s.findRequest("req-1") != nil {
		t.Fatal("失败的报修不应绑定请求标识")
	}

	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	tk, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || replay {
		t.Fatalf("首次报修: ticket=%v replay=%v err=%v", tk, replay, err)
	}
	if tk.ID != "T0001" {
		t.Fatalf("首单编号 = %q, 想得到 T0001", tk.ID)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("报修后资产状态 = %q", got)
	}

	// 同资产已有未关闭工单，新报修失败。
	if _, _, err := s.report("EQ-1", "又坏了", "req-2"); !errors.Is(err, errConflict) {
		t.Fatalf("已有未关闭工单时应失败，得到 %v", err)
	}
	if s.findRequest("req-2") != nil {
		t.Fatal("失败的报修不应绑定请求标识 req-2")
	}

	// 相同标识+资产+描述重放：返回原工单，不增加记录。
	again, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || again.ID != tk.ID {
		t.Fatalf("重放应返回原工单: %v replay=%v err=%v", again, replay, err)
	}
	if len(s.data.Tickets) != 1 || len(s.data.Requests) != 1 {
		t.Fatalf("重放不应增加记录: tickets=%d requests=%d", len(s.data.Tickets), len(s.data.Requests))
	}

	// 同一标识搭配不同描述或不同资产必须拒绝。
	if _, _, err := s.report("EQ-1", "不同故障", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("同标识不同描述应拒绝，得到 %v", err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-2", "卡纸", "req-1"); !errors.Is(err, errConflict) {
		t.Fatalf("同标识不同资产应拒绝，得到 %v", err)
	}

	// 关闭工单，设备恢复可用。
	closed, asset, err := s.closeTicket(tk.ID, "已更换搓纸轮")
	if err != nil {
		t.Fatalf("closeTicket: %v", err)
	}
	if closed.Status != ticketClosed || asset.Status != statusAvailable {
		t.Fatalf("关闭后状态异常: ticket=%s asset=%s", closed.Status, asset.Status)
	}

	// 重复关闭失败。
	if _, _, err := s.closeTicket(tk.ID, "再来一次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复关闭应失败，得到 %v", err)
	}

	// 关闭后重放原请求：仍返回原工单及其当前状态，不重开。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != tk.ID || old.Status != ticketClosed {
		t.Fatalf("关闭后重放应返回原已关闭工单: %v replay=%v err=%v", old, replay, err)
	}
	if s.findAsset("EQ-1").Status != statusAvailable {
		t.Fatal("重放不应改动资产状态")
	}

	// 资产可用后可用新请求开新工单。
	tk2, replay, err := s.report("EQ-1", "无法开机", "req-3")
	if err != nil || replay || tk2.ID != "T0002" {
		t.Fatalf("新报修应开出 T0002: %v replay=%v err=%v", tk2, replay, err)
	}

	// 旧请求重放不影响新单。
	old, replay, _ = s.report("EQ-1", "卡纸", "req-1")
	if old.ID != "T0001" || old.Status != ticketClosed || !replay {
		t.Fatalf("旧请求重放应返回 T0001/已关闭，得到 %v/%v replay=%v", old.ID, old.Status, replay)
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0002" {
		t.Fatalf("新工单 T0002 应仍为未关闭，得到 %v", got)
	}

	// 未知工单关闭失败。
	if _, _, err := s.closeTicket("T9999", "结果"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单应失败，得到 %v", err)
	}
}

func TestHistoryOrder(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket(tk.ID, "已修复"); err != nil {
		t.Fatal(err)
	}
	tk2, _, _ := s.report("EQ-1", "再次卡纸", "req-2")
	if tk2 == nil || tk2.ID != "T0002" {
		t.Fatalf("第二张工单编号 = %v", tk2)
	}
	events := s.eventsOf("EQ-1")
	if len(events) != 3 {
		t.Fatalf("履历条数 = %d，想得到 3", len(events))
	}
	if events[0].Kind != eventReport || events[0].TicketID != "T0001" ||
		events[1].Kind != eventClose || events[1].TicketID != "T0001" ||
		events[2].Kind != eventReport || events[2].TicketID != "T0002" {
		t.Fatalf("履历顺序/内容不对: %+v", events)
	}
	if _, _, err := s.closeTicket("T9999", "x"); err == nil {
		t.Fatal("未知工单关闭应失败")
	}
	if len(s.eventsOf("EQ-1")) != 3 {
		t.Fatal("失败操作不应产生履历")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.registerAsset("EQ-1", "打印机", "一楼")
	_, _ = s.registerAsset("EQ-2", "空调", "二楼")
	tk, _, _ := s.report("EQ-1", "卡纸", "req-1")
	_, _, _ = s.closeTicket(tk.ID, "已修复")
	s = saveAndReopen(t, s)

	if len(s.data.Assets) != 2 || len(s.data.Tickets) != 1 {
		t.Fatalf("重开后数据条数不对: assets=%d tickets=%d", len(s.data.Assets), len(s.data.Tickets))
	}
	if s.findAsset("EQ-1").Status != statusAvailable {
		t.Fatalf("重开后 EQ-1 状态 = %q", s.findAsset("EQ-1").Status)
	}
	// 去重规则保持：重放返回原工单；下一张工单编号继续递增。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" {
		t.Fatalf("重开后去重失效: %v replay=%v err=%v", old, replay, err)
	}
	next, _, err := s.report("EQ-2", "不制冷", "req-2")
	if err != nil || next.ID != "T0002" {
		t.Fatalf("重开后编号应递增为 T0002: %v err=%v", next, err)
	}
	events := s.eventsOf("EQ-1")
	if len(events) != 2 || events[0].Kind != eventReport || events[1].Kind != eventClose {
		t.Fatalf("重开后履历不对: %+v", events)
	}
}

func TestSeparateDirsAreIndependent(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	s1, _ := openStore(d1)
	if _, err := s1.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if err := s1.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(d2)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.data.Assets); got != 0 {
		t.Fatalf("不同目录应互不影响，却看到 %d 项资产", got)
	}
}

func TestCorruptFilePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dataFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir); err == nil || !strings.Contains(err.Error(), "已损坏") {
		t.Fatalf("损坏文件应报错，得到 %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "{not json" {
		t.Fatalf("原文件应保留，得到 %q / %v", raw, err)
	}
	// 空文件同样视为损坏。
	dir2 := t.TempDir()
	path2 := filepath.Join(dir2, dataFileName)
	if err := os.WriteFile(path2, []byte("  "), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir2); err == nil {
		t.Fatal("空文件应视为损坏而不是空库")
	}
}
