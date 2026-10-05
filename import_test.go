package main

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeImportSource 构造源台账：EQ-1（T0002 已关闭、T0005 未关闭）、
// EQ-2（T0003 已取消）、EQ-3（无工单，不参与导入）。
// 数组故意乱序、编号与履历序号故意留间隔，报修履历含小数秒。
func writeImportSource(t *testing.T, dir string) {
	t.Helper()
	assets := []*Asset{
		{ID: "EQ-2", Name: "空调", Location: "二楼", Status: statusAvailable},
		{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusRepairing},
		{ID: "EQ-3", Name: "投影仪", Location: "三楼", Status: statusAvailable},
	}
	tickets := []*Ticket{
		openTicket("T0005", "EQ-1", "异响", "req-b", "2026-09-03T08:00:00Z"),
		{ID: "T0002", AssetID: "EQ-1", Description: "卡纸", RequestID: "req-a",
			Status: ticketClosed, Result: "已修复",
			CreatedAt: "2026-09-01T08:00:00Z", ClosedAt: "2026-09-01T10:00:00Z",
			Assignee: "张三", AssignedAt: "2026-09-01T08:30:00Z", AssignNote: "首次派工"},
		cancelledTicket("T0003", "EQ-2", "不制冷", "req-c",
			"2026-09-02T08:00:00Z", "2026-09-02T09:00:00Z", "误报"),
	}
	events := []eventJSON{
		ev(17, "EQ-1", "T0005", eventReport, "异响", "2026-09-03T08:00:00Z"),
		ev(2, "EQ-1", "T0002", eventReport, "卡纸", "2026-09-01T08:00:00.25Z"),
		ev(9, "EQ-1", "T0002", eventClose, "已修复", "2026-09-01T10:00:00Z"),
		ev(14, "EQ-2", "T0003", eventCancel, "误报", "2026-09-02T09:00:00Z"),
		{Seq: 5, AssetID: "EQ-1", TicketID: "T0002", Kind: eventAssign, Content: "首次派工",
			To: "张三", Time: "2026-09-01T08:30:00Z"},
		ev(11, "EQ-2", "T0003", eventReport, "不制冷", "2026-09-02T08:00:00Z"),
	}
	requests := []requestBinding{
		req("req-b", "EQ-1", "异响", "T0005"),
		req("req-a", "EQ-1", "卡纸", "T0002"),
		req("req-c", "EQ-2", "不制冷", "T0003"),
	}
	writeDowntimeLedger(t, dir, assets, tickets, events, requests, 9)
}

// writeImportTarget 构造目标台账：EQ-9（T0001 已关闭），下一工单序号 4，
// 最大履历序号 8，报修履历含小数秒。
func writeImportTarget(t *testing.T, dir string) {
	t.Helper()
	assets := []*Asset{{ID: "EQ-9", Name: "扫描仪", Location: "九楼", Status: statusAvailable}}
	tickets := []*Ticket{closedTicket("T0001", "EQ-9", "无法扫描", "req-x",
		"2026-08-01T08:00:00Z", "2026-08-01T10:00:00Z", "已更换主板")}
	events := []eventJSON{
		ev(4, "EQ-9", "T0001", eventReport, "无法扫描", "2026-08-01T08:00:00.5Z"),
		ev(8, "EQ-9", "T0001", eventClose, "已更换主板", "2026-08-01T10:00:00Z"),
	}
	requests := []requestBinding{req("req-x", "EQ-9", "无法扫描", "T0001")}
	writeDowntimeLedger(t, dir, assets, tickets, events, requests, 4)
}

func readFileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openPair(t *testing.T, srcDir, dstDir string) (*store, *store) {
	t.Helper()
	src, err := openStore(srcDir)
	if err != nil {
		t.Fatalf("openStore 源: %v", err)
	}
	dst, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("openStore 目标: %v", err)
	}
	return src, dst
}

// 编号重映射：工单按源序号升序从目标下一序号重新分配，履历与请求绑定中的
// 工单引用同步替换；目标原有记录不变；重载后重放、编号延续与工单操作保持。
func TestImportRemapsTicketNumbers(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	writeImportSource(t, srcDir)
	writeImportTarget(t, dstDir)
	srcBefore := readFileBytes(t, srcDir)

	src, dst := openPair(t, srcDir, dstDir)
	// 重复编号按一项处理。
	res, err := importAssets(dst, src, []string{"EQ-1", "EQ-2", "EQ-1"})
	if err != nil {
		t.Fatalf("导入应成功: %v", err)
	}
	if res.AssetCount != 2 {
		t.Fatalf("导入资产数量 = %d，想得到 2", res.AssetCount)
	}
	wantMap := []ticketIDMapping{
		{OldID: "T0002", NewID: "T0004"},
		{OldID: "T0003", NewID: "T0005"},
		{OldID: "T0005", NewID: "T0006"},
	}
	if len(res.Mappings) != len(wantMap) {
		t.Fatalf("映射数量 = %d，想得到 %d", len(res.Mappings), len(wantMap))
	}
	for i, m := range res.Mappings {
		if m != wantMap[i] {
			t.Fatalf("映射[%d] = %+v，想得到 %+v（须按源工单序号升序）", i, m, wantMap[i])
		}
	}
	if err := dst.save(); err != nil {
		t.Fatalf("保存合并数据: %v", err)
	}

	// 源台账始终只读：字节保持不变。
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("导入不得修改源台账")
	}

	// 重载后检查合并结果。
	d2, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if d2.data.NextTicketSeq != 7 {
		t.Fatalf("下一工单序号 = %d，想得到 7", d2.data.NextTicketSeq)
	}
	// 资产编号、名称、位置、状态保留。
	a1 := d2.findAsset("EQ-1")
	if a1 == nil || a1.Name != "打印机" || a1.Location != "一楼" || a1.Status != statusRepairing {
		t.Fatalf("EQ-1 导入后信息不对: %+v", a1)
	}
	if a := d2.findAsset("EQ-2"); a == nil || a.Status != statusAvailable {
		t.Fatalf("EQ-2 导入后状态不对: %+v", a)
	}
	// 目标原有记录不改编号、不改变业务含义。
	old := d2.findTicket("T0001")
	if old == nil || old.AssetID != "EQ-9" || old.Status != ticketClosed || old.Result != "已更换主板" {
		t.Fatalf("目标原有工单 T0001 不应改变: %+v", old)
	}
	// 映射后的工单保留业务信息。
	nt := d2.findTicket("T0004")
	if nt == nil || nt.AssetID != "EQ-1" || nt.Description != "卡纸" || nt.RequestID != "req-a" ||
		nt.Status != ticketClosed || nt.Result != "已修复" || nt.Assignee != "张三" || nt.AssignNote != "首次派工" {
		t.Fatalf("映射后工单 T0004 业务信息不对: %+v", nt)
	}
	if t6 := d2.findTicket("T0006"); t6 == nil || t6.Status != ticketOpen || t6.RequestID != "req-b" {
		t.Fatalf("映射后工单 T0006 不对: %+v", t6)
	}
	// 请求绑定同步替换工单引用。
	if r := d2.findRequest("req-a"); r == nil || r.TicketID != "T0004" || r.AssetID != "EQ-1" || r.Description != "卡纸" {
		t.Fatalf("请求绑定 req-a 未重映射: %+v", r)
	}
	// 履历：接在目标最大序号 8 之后按源序号顺序分配，工单引用已替换，
	// 时间保留原瞬间与小数秒。
	evs := d2.eventsOf("EQ-1")
	if len(evs) != 4 {
		t.Fatalf("EQ-1 履历条数 = %d，想得到 4", len(evs))
	}
	wantSeq := []int{9, 10, 11, 14}
	wantKind := []string{eventReport, eventAssign, eventClose, eventReport}
	wantTicket := []string{"T0004", "T0004", "T0004", "T0006"}
	for i := range evs {
		if evs[i].Seq != wantSeq[i] || evs[i].Kind != wantKind[i] || evs[i].TicketID != wantTicket[i] {
			t.Fatalf("履历[%d] = %+v，想序号 %d、类型 %s、工单 %s",
				i, evs[i], wantSeq[i], wantKind[i], wantTicket[i])
		}
	}
	if evs[0].Time.Nanosecond() != 250_000_000 {
		t.Fatalf("报修履历小数秒丢失: %s", evs[0].Time.Format(time.RFC3339Nano))
	}
	if evs[1].From != "" || evs[1].To != "张三" || evs[1].Content != "首次派工" {
		t.Fatalf("派工履历信息不对: %+v", evs[1])
	}
	// 目标原有履历的时间精度也不能损失。
	var oldReport *Event
	for i := range d2.data.Events {
		if d2.data.Events[i].Seq == 4 {
			oldReport = &d2.data.Events[i]
		}
	}
	if oldReport == nil || oldReport.Time.Nanosecond() != 500_000_000 {
		t.Fatalf("目标原有履历小数秒丢失: %+v", oldReport)
	}

	// 用原请求、资产与描述重放：返回映射后的工单及其当前状态，不创建记录。
	before := len(d2.data.Tickets)
	old2, replay, err := d2.report("EQ-1", "卡纸", "req-a")
	if err != nil || !replay || old2.ID != "T0004" || old2.Status != ticketClosed {
		t.Fatalf("重放应返回已关闭的 T0004: %v replay=%v err=%v", old2, replay, err)
	}
	if len(d2.data.Tickets) != before || len(d2.data.Events) != 8 {
		t.Fatal("重放不应创建工单或履历")
	}
	// 再次导入同一批资产按编号冲突拒绝，不作为报修请求重放。
	if _, err := importAssets(d2, src, []string{"EQ-1", "EQ-2"}); !errors.Is(err, errConflict) {
		t.Fatalf("再次导入应按编号冲突拒绝，得到 %v", err)
	}

	// 编号延续：目标既有资产新报修从 7 继续。
	tk, replay, err := d2.report("EQ-9", "卡纸", "req-y")
	if err != nil || replay || tk.ID != "T0007" {
		t.Fatalf("导入后新报修应为 T0007: %v replay=%v err=%v", tk, replay, err)
	}
	// 导入的未关闭工单可继续派工、关闭。
	if _, err := d2.assignTicket("T0006", "李四", "接手维修"); err != nil {
		t.Fatalf("导入的未关闭工单应可派工: %v", err)
	}
	if _, _, err := d2.closeTicket("T0006", "已更换风扇"); err != nil {
		t.Fatalf("导入的未关闭工单应可关闭: %v", err)
	}
	if err := d2.save(); err != nil {
		t.Fatal(err)
	}
	// 重启后查询、请求重放与编号延续保持。
	d3, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := d3.findTicket("T0006"); got == nil || got.Status != ticketClosed || got.Assignee != "李四" {
		t.Fatalf("重载后 T0006 状态未保持: %+v", got)
	}
	if d3.data.NextTicketSeq != 8 {
		t.Fatalf("重载后下一工单序号 = %d，想得到 8", d3.data.NextTicketSeq)
	}
	if _, replay, err := d3.report("EQ-1", "异响", "req-b"); err != nil || !replay {
		t.Fatalf("重载后重放 req-b 应返回映射工单: replay=%v err=%v", replay, err)
	}
}

// 旧库时间精度：源履历含纳秒级小数与时区偏移，导入后瞬间与精度不变；
// 目标原有含小数秒的履历在导入保存后同样不损失；停机统计规则不变。
func TestImportPreservesLegacyTimePrecision(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	// 源旧台账：纳秒级小数秒、时区偏移、数组乱序、序号间隔。
	assets := []*Asset{{ID: "EQ-1", Name: "打印机", Location: "一楼", Status: statusAvailable}}
	tickets := []*Ticket{closedTicket("T0007", "EQ-1", "卡纸", "req-a",
		"2026-09-01T08:00:00+08:00", "2026-09-01T10:00:01+08:00", "已修复")}
	events := []eventJSON{
		ev(42, "EQ-1", "T0007", eventClose, "已修复", "2026-09-01T10:00:01.000000001+08:00"),
		ev(30, "EQ-1", "T0007", eventReport, "卡纸", "2026-09-01T08:00:00.123456789+08:00"),
	}
	requests := []requestBinding{req("req-a", "EQ-1", "卡纸", "T0007")}
	writeDowntimeLedger(t, srcDir, assets, tickets, events, requests, 10)
	writeImportTarget(t, dstDir)

	src, dst := openPair(t, srcDir, dstDir)
	if _, err := importAssets(dst, src, []string{"EQ-1"}); err != nil {
		t.Fatalf("导入应成功: %v", err)
	}
	if err := dst.save(); err != nil {
		t.Fatal(err)
	}
	// 落盘文本直接保留小数秒。
	if raw := readFileBytes(t, dstDir); !bytes.Contains(raw, []byte("08:00:00.123456789")) {
		t.Fatalf("保存后的文件应保留小数秒: %s", raw)
	}

	d2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	evs := d2.eventsOf("EQ-1")
	if len(evs) != 2 {
		t.Fatalf("履历条数 = %d，想得到 2", len(evs))
	}
	wantStart, _ := time.Parse(time.RFC3339, "2026-09-01T08:00:00.123456789+08:00")
	wantEnd, _ := time.Parse(time.RFC3339, "2026-09-01T10:00:01.000000001+08:00")
	if !evs[0].Time.Equal(wantStart) || evs[0].Time.Nanosecond() != 123456789 {
		t.Fatalf("报修履历时间精度丢失: %s", evs[0].Time.Format(time.RFC3339Nano))
	}
	if !evs[1].Time.Equal(wantEnd) || evs[1].Time.Nanosecond() != 1 {
		t.Fatalf("关闭履历时间精度丢失: %s", evs[1].Time.Format(time.RFC3339Nano))
	}
	// 目标原有履历（.5Z）精度不变。
	var kept *Event
	for i := range d2.data.Events {
		if d2.data.Events[i].Seq == 4 {
			kept = &d2.data.Events[i]
		}
	}
	if kept == nil || kept.Time.Nanosecond() != 500_000_000 {
		t.Fatalf("目标原有履历精度丢失: %+v", kept)
	}
	// 导入资产在相同窗口的停机秒数规则不变：7200.876543212 秒向下取整 7200。
	res, err := d2.downtimeForAssets([]string{"EQ-1"},
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("导入资产的停机统计应可用: %v", err)
	}
	if res[0].Seconds != 7200 {
		t.Fatalf("导入资产停机秒数 = %d，想得到 7200", res[0].Seconds)
	}
}

// 冲突整批拒绝：资产编号已存在、请求标识已绑定、所选资产不存在；
// 不留下部分变化，两边原文件字节不变，不消耗目标编号。
func TestImportConflictRejectsWholeBatch(t *testing.T) {
	t.Run("资产编号已存在", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()
		writeImportSource(t, srcDir)
		// 目标已有同名资产 EQ-1（无工单）。
		writeDowntimeLedger(t, dstDir,
			[]*Asset{{ID: "EQ-1", Name: "旧打印机", Location: "地下一层", Status: statusAvailable}},
			[]*Ticket{}, []eventJSON{}, []requestBinding{}, 1)
		srcBefore := readFileBytes(t, srcDir)
		dstBefore := readFileBytes(t, dstDir)

		src, dst := openPair(t, srcDir, dstDir)
		// EQ-2 本身可导入，但 EQ-1 冲突必须整批拒绝。
		_, err := importAssets(dst, src, []string{"EQ-2", "EQ-1"})
		if !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "已存在") {
			t.Fatalf("应按编号冲突拒绝，得到 %v", err)
		}
		if len(dst.data.Assets) != 1 || dst.data.NextTicketSeq != 1 {
			t.Fatal("整批拒绝不应留下部分资产，也不应消耗编号")
		}
		if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
			t.Fatal("源文件不应改变")
		}
		if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
			t.Fatal("目标文件不应改变")
		}
	})

	t.Run("请求标识已绑定", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()
		writeImportSource(t, srcDir)
		// 目标已绑定 req-a（源中 EQ-1 的工单 T0002 的请求标识）。
		writeDowntimeLedger(t, dstDir,
			[]*Asset{{ID: "EQ-9", Name: "扫描仪", Location: "九楼", Status: statusAvailable}},
			[]*Ticket{closedTicket("T0001", "EQ-9", "无法扫描", "req-a",
				"2026-08-01T08:00:00Z", "2026-08-01T10:00:00Z", "已修复")},
			[]eventJSON{
				ev(4, "EQ-9", "T0001", eventReport, "无法扫描", "2026-08-01T08:00:00Z"),
				ev(8, "EQ-9", "T0001", eventClose, "已修复", "2026-08-01T10:00:00Z"),
			},
			[]requestBinding{req("req-a", "EQ-9", "无法扫描", "T0001")}, 4)
		dstBefore := readFileBytes(t, dstDir)

		src, dst := openPair(t, srcDir, dstDir)
		_, err := importAssets(dst, src, []string{"EQ-1"})
		if !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "req-a") {
			t.Fatalf("应按请求标识冲突拒绝，得到 %v", err)
		}
		if len(dst.data.Tickets) != 1 || len(dst.data.Requests) != 1 || dst.data.NextTicketSeq != 4 {
			t.Fatal("整批拒绝不应留下部分工单或绑定，也不应消耗编号")
		}
		if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
			t.Fatal("目标文件不应改变")
		}
	})

	t.Run("所选资产不存在", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()
		writeImportSource(t, srcDir)
		writeImportTarget(t, dstDir)
		src, dst := openPair(t, srcDir, dstDir)
		if _, err := importAssets(dst, src, []string{"EQ-1", "NOPE"}); !errors.Is(err, errNotFound) {
			t.Fatalf("所选资产不存在应整批失败，得到 %v", err)
		}
		if len(dst.data.Assets) != 1 || len(dst.data.Tickets) != 1 {
			t.Fatal("整批失败不应留下部分变化")
		}
	})
}

// 容量不足：目标工单编号或履历序号容量不足时整批失败，不消耗编号，
// 原文件字节不变。
func TestImportCapacityFailures(t *testing.T) {
	t.Run("工单编号容量不足", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()
		writeImportSource(t, srcDir)
		writeImportTarget(t, dstDir)
		// 目标下一工单序号接近耗尽：剩余容量不足两张工单。
		writeDowntimeLedger(t, dstDir,
			[]*Asset{{ID: "EQ-9", Name: "扫描仪", Location: "九楼", Status: statusAvailable}},
			[]*Ticket{closedTicket("T0001", "EQ-9", "无法扫描", "req-x",
				"2026-08-01T08:00:00Z", "2026-08-01T10:00:00Z", "已修复")},
			[]eventJSON{
				ev(4, "EQ-9", "T0001", eventReport, "无法扫描", "2026-08-01T08:00:00Z"),
				ev(8, "EQ-9", "T0001", eventClose, "已修复", "2026-08-01T10:00:00Z"),
			},
			[]requestBinding{req("req-x", "EQ-9", "无法扫描", "T0001")}, math.MaxInt)
		dstBefore := readFileBytes(t, dstDir)

		src, dst := openPair(t, srcDir, dstDir)
		_, err := importAssets(dst, src, []string{"EQ-1"})
		if !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "工单编号容量不足") {
			t.Fatalf("应报工单编号容量不足，得到 %v", err)
		}
		if dst.data.NextTicketSeq != math.MaxInt || len(dst.data.Tickets) != 1 {
			t.Fatal("容量不足不应消耗编号或留下部分工单")
		}
		if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
			t.Fatal("目标文件不应改变")
		}
	})

	t.Run("履历序号容量不足", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()
		writeImportSource(t, srcDir)
		// 目标最大履历序号已耗尽。
		writeDowntimeLedger(t, dstDir,
			[]*Asset{{ID: "EQ-9", Name: "扫描仪", Location: "九楼", Status: statusAvailable}},
			[]*Ticket{closedTicket("T0001", "EQ-9", "无法扫描", "req-x",
				"2026-08-01T08:00:00Z", "2026-08-01T10:00:00Z", "已修复")},
			[]eventJSON{
				ev(math.MaxInt-1, "EQ-9", "T0001", eventReport, "无法扫描", "2026-08-01T08:00:00Z"),
				ev(math.MaxInt, "EQ-9", "T0001", eventClose, "已修复", "2026-08-01T10:00:00Z"),
			},
			[]requestBinding{req("req-x", "EQ-9", "无法扫描", "T0001")}, 4)
		dstBefore := readFileBytes(t, dstDir)

		src, dst := openPair(t, srcDir, dstDir)
		_, err := importAssets(dst, src, []string{"EQ-2"})
		if !errors.Is(err, errConflict) || !strings.Contains(err.Error(), "履历序号容量不足") {
			t.Fatalf("应报履历序号容量不足，得到 %v", err)
		}
		if len(dst.data.Events) != 2 || dst.data.NextTicketSeq != 4 {
			t.Fatal("容量不足不应留下部分履历或消耗编号")
		}
		if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
			t.Fatal("目标文件不应改变")
		}
	})
}

// 写入失败后的重载：整批变化不留下部分资产、履历或请求绑定，不消耗目标
// 编号；恢复写入条件后同一批可重新导入。
func TestImportWriteFailureReload(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	writeImportSource(t, srcDir)
	writeImportTarget(t, dstDir)
	srcBefore := readFileBytes(t, srcDir)
	dstBefore := readFileBytes(t, dstDir)

	src, dst := openPair(t, srcDir, dstDir)
	if _, err := importAssets(dst, src, []string{"EQ-1", "EQ-2"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dstDir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := dst.save()
	if err := os.Chmod(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	// 两边原文件字节保持不变。
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("写入失败不应改动源文件")
	}
	if got := readFileBytes(t, dstDir); !bytes.Equal(got, dstBefore) {
		t.Fatal("写入失败不应改动目标文件")
	}
	// 重载：看不到部分变化，编号未消耗。
	d2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.data.Assets) != 1 || len(d2.data.Tickets) != 1 || len(d2.data.Events) != 2 ||
		len(d2.data.Requests) != 1 || d2.data.NextTicketSeq != 4 {
		t.Fatal("写入失败不应留下部分资产、履历或请求绑定，也不应消耗编号")
	}
	// 恢复写入条件后同一批可重新导入，编号映射不变。
	src2, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := importAssets(d2, src2, []string{"EQ-1", "EQ-2"})
	if err != nil {
		t.Fatalf("恢复后应能重新导入: %v", err)
	}
	if res.Mappings[0].NewID != "T0004" {
		t.Fatalf("重新导入映射应为 T0004，得到 %s", res.Mappings[0].NewID)
	}
	if err := d2.save(); err != nil {
		t.Fatal(err)
	}
	d3, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重新导入后应能重载: %v", err)
	}
	if len(d3.data.Assets) != 3 || d3.data.NextTicketSeq != 7 {
		t.Fatalf("重新导入后数据不对: 资产 %d 项，下一序号 %d", len(d3.data.Assets), d3.data.NextTicketSeq)
	}
}

// 命令行行为：参数错误退出 2，业务或读写失败退出 1；源不存在不当空库
// 初始化；同一台账不能导入自身；目标无台账时成功导入会创建。
func TestImportCommandLine(t *testing.T) {
	// 参数错误：退出码 2。
	for _, args := range [][]string{
		{"import", "--source-dir", "x"},
		{"import", "--asset-id", "EQ-1"},
		{"import", "--source-dir", "x", "--asset-id", ""},
	} {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code != 2 {
			t.Fatalf("%v 退出码 = %d，应为 2（%s）", args, code, errBuf.String())
		}
	}

	// 源不存在：退出码 1，且不创建目标目录。
	missingSrc := filepath.Join(t.TempDir(), "nope")
	dstDir := filepath.Join(t.TempDir(), "dst")
	var out, errBuf bytes.Buffer
	code := run([]string{"import", "--source-dir", missingSrc, "--asset-id", "EQ-1", "--data-dir", dstDir},
		&out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "不存在") {
		t.Fatalf("源不存在应退出 1 并说明原因: code=%d err=%s", code, errBuf.String())
	}
	if _, err := os.Stat(dstDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("源不存在时不应创建目标目录")
	}

	// 同一台账不能导入自身。
	sameDir := t.TempDir()
	writeImportSource(t, sameDir)
	out.Reset()
	errBuf.Reset()
	code = run([]string{"import", "--source-dir", sameDir, "--asset-id", "EQ-1", "--data-dir", sameDir},
		&out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "同一台账") {
		t.Fatalf("同一台账应退出 1: code=%d err=%s", code, errBuf.String())
	}

	// 成功导入：目标无台账时创建，输出资产数量与编号映射。
	srcDir := t.TempDir()
	writeImportSource(t, srcDir)
	newDst := filepath.Join(t.TempDir(), "new-dst")
	out.Reset()
	errBuf.Reset()
	code = run([]string{"import", "--source-dir", srcDir, "--asset-id", "EQ-1", "--data-dir", newDst},
		&out, &errBuf)
	if code != 0 {
		t.Fatalf("导入应成功: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "已导入资产数量: 1") ||
		!strings.Contains(out.String(), "T0002 -> T0001") ||
		!strings.Contains(out.String(), "T0005 -> T0002") {
		t.Fatalf("输出应含资产数量与编号映射: %s", out.String())
	}
	if _, err := openStore(newDst); err != nil {
		t.Fatalf("导入后目标台账应可加载: %v", err)
	}
	// 再次导入同一批资产按编号冲突拒绝（退出码 1）。
	out.Reset()
	errBuf.Reset()
	code = run([]string{"import", "--source-dir", srcDir, "--asset-id", "EQ-1", "--data-dir", newDst},
		&out, &errBuf)
	if code != 1 || !strings.Contains(errBuf.String(), "已存在") {
		t.Fatalf("再次导入应按编号冲突拒绝: code=%d err=%s", code, errBuf.String())
	}
}
