package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStoreAt 在指定目录打开（或初始化）测试台账，使用确定性时钟。
func newStoreAt(t *testing.T, dir string) *store {
	t.Helper()
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore(%s): %v", dir, err)
	}
	clock := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	return s
}

func mustSave(t *testing.T, s *store) {
	t.Helper()
	if err := s.save(); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func readFileBytes(t *testing.T, dir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatalf("读取数据文件: %v", err)
	}
	return raw
}

// 编号重映射：工单按源序号升序从目标下一序号重排，履历在目标最大序号后续排，
// 请求绑定同步替换；重放、再次导入、编号延续与后续工单操作行为保持。
func TestImportRemapsTicketsAndEvents(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	// 源台账：EQ-A 一关一开两张工单（含派工），EQ-B 一张未关闭，
	// EQ-C 不导入，EQ-D 无工单。
	src := newStoreAt(t, srcDir)
	for _, a := range [][3]string{
		{"EQ-A", "打印机", "一楼"}, {"EQ-B", "空调", "二楼"},
		{"EQ-C", "投影仪", "三楼"}, {"EQ-D", "饮水机", "四楼"},
	} {
		if _, err := src.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := src.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-B", "异响", "req-b1"); err != nil { // T0003
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-C", "漏氟", "req-c1"); err != nil { // T0004，不导入
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标台账：EQ-X 一张未关闭工单 T0001，下一序号 2，最大履历序号 1。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "抖动", "req-x1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	dstBefore := readFileBytes(t, dstDir)

	// 重复编号按一项处理。
	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A", "EQ-B", "EQ-A", "EQ-D"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(outcome.assetIDs) != 3 {
		t.Fatalf("导入资产数 = %d, 想得到 3（重复编号按一项）", len(outcome.assetIDs))
	}
	wantMap := []ticketRemap{{"T0001", "T0002"}, {"T0002", "T0003"}, {"T0003", "T0004"}}
	if len(outcome.tickets) != len(wantMap) {
		t.Fatalf("映射工单数 = %d, 想得到 %d", len(outcome.tickets), len(wantMap))
	}
	for i, m := range outcome.tickets {
		if m != wantMap[i] {
			t.Fatalf("映射[%d] = %v, 想得到 %v", i, m, wantMap[i])
		}
	}

	// 源台账字节不变（只读）。
	if got := readFileBytes(t, srcDir); string(got) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}
	if string(dstBefore) == string(readFileBytes(t, dstDir)) {
		t.Fatal("目标台账应已更新")
	}

	// 重载后核对合并结果。
	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextTicketSeq != 5 {
		t.Fatalf("下一工单序号 = %d, 想得到 5", s.data.NextTicketSeq)
	}
	if s.findAsset("EQ-C") != nil {
		t.Fatal("未选择的 EQ-C 不应被导入")
	}
	a := s.findAsset("EQ-A")
	if a == nil || a.Name != "打印机" || a.Location != "一楼" || a.Status != statusRepairing {
		t.Fatalf("EQ-A 业务信息应原样保留，得到 %+v", a)
	}
	if d := s.findAsset("EQ-D"); d == nil || d.Status != statusAvailable {
		t.Fatalf("无工单的 EQ-D 应导入为可用，得到 %+v", d)
	}
	// 目标原有记录不改编号、不改变业务含义。
	orig := s.findTicket("T0001")
	if orig == nil || orig.AssetID != "EQ-X" || orig.RequestID != "req-x1" || orig.Status != ticketOpen {
		t.Fatalf("目标原有工单 T0001 不应改变，得到 %+v", orig)
	}
	// 映射后的工单保留业务信息。
	t2 := s.findTicket("T0002")
	if t2 == nil || t2.AssetID != "EQ-A" || t2.Description != "卡纸" || t2.RequestID != "req-a1" ||
		t2.Status != ticketClosed || t2.Result != "已更换搓纸轮" || t2.Assignee != "张三" {
		t.Fatalf("映射工单 T0002 业务信息应保留，得到 %+v", t2)
	}
	if t3 := s.findTicket("T0003"); t3 == nil || t3.Status != ticketOpen || t3.RequestID != "req-a2" {
		t.Fatalf("映射工单 T0003 应保持未关闭，得到 %+v", t3)
	}
	if t4 := s.findTicket("T0004"); t4 == nil || t4.AssetID != "EQ-B" || t4.RequestID != "req-b1" {
		t.Fatalf("映射工单 T0004 应属于 EQ-B，得到 %+v", t4)
	}
	// 请求绑定同步替换为新工单编号。
	if r := s.findRequest("req-a1"); r == nil || r.TicketID != "T0002" {
		t.Fatalf("req-a1 应绑定到 T0002，得到 %+v", r)
	}
	// 履历：在目标最大序号 1 之后按源顺序分配 2..6，工单引用已替换。
	evA := s.eventsOf("EQ-A")
	if len(evA) != 4 {
		t.Fatalf("EQ-A 履历数 = %d, 想得到 4", len(evA))
	}
	wantKinds := []string{eventReport, eventAssign, eventClose, eventReport}
	wantTickets := []string{"T0002", "T0002", "T0002", "T0003"}
	for i, e := range evA {
		if e.Seq != i+2 || e.Kind != wantKinds[i] || e.TicketID != wantTickets[i] {
			t.Fatalf("履历[%d] = seq %d %s %s, 想得到 seq %d %s %s",
				i, e.Seq, e.Kind, e.TicketID, i+2, wantKinds[i], wantTickets[i])
		}
	}
	if evB := s.eventsOf("EQ-B"); len(evB) != 1 || evB[0].Seq != 6 || evB[0].TicketID != "T0004" {
		t.Fatalf("EQ-B 履历应为 seq 6 的 T0004 报修，得到 %+v", evB)
	}
	// 履历时间保留原瞬间：与源台账逐条相等。
	srcRe, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	srcEv := append(srcRe.eventsOf("EQ-A"), srcRe.eventsOf("EQ-B")...)
	dstEv := append(s.eventsOf("EQ-A"), s.eventsOf("EQ-B")...)
	for i := range srcEv {
		if !srcEv[i].Time.Equal(dstEv[i].Time) {
			t.Fatalf("履历[%d] 时间 %s 应等于源时间 %s",
				i, dstEv[i].Time, srcEv[i].Time)
		}
	}

	// 用原请求标识、资产与描述重放：返回映射后的工单及其当前状态，不创建记录。
	before := len(s.data.Tickets)
	tk, replay, err := s.report("EQ-A", "卡纸", "req-a1")
	if err != nil || !replay || tk.ID != "T0002" || tk.Status != ticketClosed {
		t.Fatalf("重放应返回已关闭的 T0002: %v replay=%v err=%v", tk, replay, err)
	}
	if len(s.data.Tickets) != before {
		t.Fatal("重放不应创建记录")
	}

	// 再次导入同一批资产按编号冲突拒绝，不作为报修请求重放。
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A"}); !errors.Is(err, errConflict) {
		t.Fatalf("再次导入应冲突，得到 %v", err)
	}

	// 导入的未关闭工单可继续派工、关闭；编号从 5 延续。
	s2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.assignTicket("T0004", "李四", "接手空调"); err != nil {
		t.Fatalf("导入工单应可派工: %v", err)
	}
	if _, _, err := s2.closeTicket("T0003", "已更换电源"); err != nil {
		t.Fatalf("导入工单应可关闭: %v", err)
	}
	if _, _, err := s2.closeTicket("T0004", "已更换风扇"); err != nil {
		t.Fatal(err)
	}
	nk, replay, err := s2.report("EQ-B", "再次异响", "req-b2")
	if err != nil || replay || nk.ID != "T0005" {
		t.Fatalf("新工单应为 T0005: %v replay=%v err=%v", nk, replay, err)
	}
	mustSave(t, s2)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("后续操作后的台账应保持一致: %v", err)
	}
}

// 旧库时间精度：源与目标原有履历的小数秒在导入与重新保存后都不损失；
// 序号间隔、数组乱序、时间不递增的旧台账均可导入；停机统计规则不变。
func TestImportPreservesLegacyTimePrecision(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 源：序号有间隔、事件数组乱序、派工时间早于报修时间（时间不递增），
	// 履历时间含小数秒。
	srcLedger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"可用"}],
  "tickets": [{"id":"T0003","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"已关闭",
    "result":"已修复","created_at":"2026-10-01T08:00:00.123456789Z","closed_at":"2026-10-01T09:30:00.000000001Z",
    "assignee":"张三","assigned_at":"2026-10-01T07:00:00.25Z","assign_note":"上门"}],
  "events": [
    {"seq":7,"asset_id":"EQ-1","ticket_id":"T0003","kind":"关闭","content":"已修复","time":"2026-10-01T09:30:00.000000001Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0003","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00.123456789Z"},
    {"seq":5,"asset_id":"EQ-1","ticket_id":"T0003","kind":"派工","content":"上门","from":"","to":"张三","time":"2026-10-01T07:00:00.25Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0003"}],
  "next_ticket_seq": 9
}`
	if err := os.WriteFile(filepath.Join(srcDir, dataFileName), []byte(srcLedger), 0o644); err != nil {
		t.Fatal(err)
	}
	// 目标：原有履历带小数秒 .75，最大履历序号 4。
	dstLedger := `{
  "version": 1,
  "assets": [{"id":"EQ-9","name":"空调","location":"二楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-9","description":"不制冷","request_id":"req-9","status":"未关闭","created_at":"2026-09-01T10:00:00Z"}],
  "events": [{"seq":4,"asset_id":"EQ-9","ticket_id":"T0001","kind":"报修","content":"不制冷","time":"2026-09-01T10:00:00.75Z"}],
  "requests": [{"request_id":"req-9","asset_id":"EQ-9","description":"不制冷","ticket_id":"T0001"}],
  "next_ticket_seq": 2
}`
	if err := os.WriteFile(filepath.Join(dstDir, dataFileName), []byte(dstLedger), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-1"})
	if err != nil {
		t.Fatalf("导入旧库失败: %v", err)
	}
	if len(outcome.tickets) != 1 || outcome.tickets[0] != (ticketRemap{"T0003", "T0002"}) {
		t.Fatalf("映射应为 T0003 -> T0002，得到 %+v", outcome.tickets)
	}

	// 目标文件字节层面保留小数秒：导入的与目标原有的都不被截断。
	raw := readFileBytes(t, dstDir)
	for _, frag := range []string{"123456789", "000000001", "07:00:00.25", "10:00:00.75"} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("保存后的目标文件应保留小数秒片段 %q", frag)
		}
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	// 履历按源序号顺序（报修、派工、关闭）排在目标最大序号 4 之后，不按时间重排。
	ev := s.eventsOf("EQ-1")
	if len(ev) != 3 {
		t.Fatalf("履历数 = %d, 想得到 3", len(ev))
	}
	want := []struct {
		seq  int
		kind string
		ts   string
	}{
		{5, eventReport, "2026-10-01T08:00:00.123456789Z"},
		{6, eventAssign, "2026-10-01T07:00:00.25Z"},
		{7, eventClose, "2026-10-01T09:30:00.000000001Z"},
	}
	for i, w := range want {
		wantTime, err := time.Parse(time.RFC3339Nano, w.ts)
		if err != nil {
			t.Fatal(err)
		}
		if ev[i].Seq != w.seq || ev[i].Kind != w.kind || !ev[i].Time.Equal(wantTime) {
			t.Fatalf("履历[%d] = seq %d %s %s, 想得到 seq %d %s %s",
				i, ev[i].Seq, ev[i].Kind, ev[i].Time, w.seq, w.kind, w.ts)
		}
		if ev[i].TicketID != "T0002" {
			t.Fatalf("履历[%d 工单引用应为 T0002，得到 %s", i, ev[i].TicketID)
		}
	}
	// 目标原有履历时间精度不损失。
	evX := s.eventsOf("EQ-9")
	wantX, _ := time.Parse(time.RFC3339Nano, "2026-09-01T10:00:00.75Z")
	if len(evX) != 1 || !evX[0].Time.Equal(wantX) {
		t.Fatalf("目标原有履历时间应保持 .75，得到 %+v", evX)
	}
	// 工单记录上的时间字符串原样保留。
	if tk := s.findTicket("T0002"); tk.CreatedAt != "2026-10-01T08:00:00.123456789Z" ||
		tk.ClosedAt != "2026-10-01T09:30:00.000000001Z" {
		t.Fatalf("工单时间字段应原样保留，得到 %+v", tk)
	}
	// 导入资产的停机统计规则不变：08:00:00.123456789 -> 09:30:00.000000001
	// 精确时长 5399.876543212 秒，向下取整 5399 秒。
	start, _ := time.Parse(time.RFC3339, "2026-10-01T00:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-10-02T00:00:00Z")
	res, err := s.downtimeForAssets([]string{"EQ-1"}, start, end)
	if err != nil {
		t.Fatalf("停机统计: %v", err)
	}
	if res[0].Seconds != 5399 {
		t.Fatalf("停机秒数 = %d, 想得到 5399", res[0].Seconds)
	}
}

// 冲突：资产编号在目标已存在、或所选工单的请求标识已在目标绑定时整批拒绝，
// 两边原文件字节保持不变。
func TestImportConflictRejectsBatch(t *testing.T) {
	newSource := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "src")
		src := newStoreAt(t, dir)
		if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil {
			t.Fatal(err)
		}
		mustSave(t, src)
		return dir
	}

	// 资产编号冲突。
	srcDir := newSource(t)
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-A", "另一台打印机", "二楼"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)
	srcBefore, dstBefore := readFileBytes(t, srcDir), readFileBytes(t, dstDir)
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A"}); !errors.Is(err, errConflict) {
		t.Fatalf("资产编号冲突应拒绝，得到 %v", err)
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) ||
		string(readFileBytes(t, dstDir)) != string(dstBefore) {
		t.Fatal("冲突拒绝不应改动任何文件")
	}

	// 请求标识冲突：目标已有不同资产绑定了 req-a1。
	srcDir2 := newSource(t)
	dstDir2 := filepath.Join(t.TempDir(), "dst")
	dst2 := newStoreAt(t, dstDir2)
	if _, err := dst2.registerAsset("EQ-Z", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst2.report("EQ-Z", "抖动", "req-a1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst2)
	srcBefore2, dstBefore2 := readFileBytes(t, srcDir2), readFileBytes(t, dstDir2)
	if _, err := importAssets(dstDir2, srcDir2, []string{"EQ-A"}); !errors.Is(err, errConflict) {
		t.Fatalf("请求标识冲突应拒绝，得到 %v", err)
	}
	if string(readFileBytes(t, srcDir2)) != string(srcBefore2) ||
		string(readFileBytes(t, dstDir2)) != string(dstBefore2) {
		t.Fatal("请求标识冲突拒绝不应改动任何文件")
	}
	s, err := openStore(dstDir2)
	if err != nil {
		t.Fatal(err)
	}
	if s.findAsset("EQ-A") != nil || len(s.data.Tickets) != 1 {
		t.Fatal("整批拒绝不应留下部分资产或工单")
	}
}

// 容量：目标工单编号或履历序号容量不足时整批失败，不消耗编号，文件不变。
func TestImportCapacityFailures(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-S", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-S", "卡纸", "req-s1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	writeDst := func(t *testing.T, nextTicketSeq, maxEventSeq int) (string, []byte) {
		t.Helper()
		dir := t.TempDir()
		ledger := fmt.Sprintf(`{
  "version": 1,
  "assets": [{"id":"EQ-T","name":"空调","location":"二楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-T","description":"不制冷","request_id":"req-t1","status":"未关闭","created_at":"2026-09-01T10:00:00Z"}],
  "events": [{"seq":%[2]d,"asset_id":"EQ-T","ticket_id":"T0001","kind":"报修","content":"不制冷","time":"2026-09-01T10:00:00Z"}],
  "requests": [{"request_id":"req-t1","asset_id":"EQ-T","description":"不制冷","ticket_id":"T0001"}],
  "next_ticket_seq": %[1]d
}`, nextTicketSeq, maxEventSeq)
		raw := []byte(ledger)
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, raw
	}

	// 工单编号容量不足：下一序号已为 math.MaxInt，无法再分配。
	dstDir, dstBefore := writeDst(t, math.MaxInt, 1)
	srcBefore := readFileBytes(t, srcDir)
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-S"}); !errors.Is(err, errConflict) {
		t.Fatalf("工单编号容量不足应冲突拒绝，得到 %v", err)
	}
	if string(readFileBytes(t, dstDir)) != string(dstBefore) ||
		string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("容量不足拒绝不应改动任何文件")
	}

	// 履历序号容量不足：目标最大履历序号已为 math.MaxInt。
	dstDir2, dstBefore2 := writeDst(t, 2, math.MaxInt)
	if _, err := importAssets(dstDir2, srcDir, []string{"EQ-S"}); !errors.Is(err, errConflict) {
		t.Fatalf("履历序号容量不足应冲突拒绝，得到 %v", err)
	}
	if string(readFileBytes(t, dstDir2)) != string(dstBefore2) {
		t.Fatal("容量不足拒绝不应改动任何文件")
	}
}

// 写入失败：目标目录不可写时整批失败，两边原文件字节不变，
// 重载后无部分资产、履历或请求绑定，目标编号未被消耗。
func TestImportWriteFailureLeavesFilesUntouched(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-S", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-S", "卡纸", "req-s1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	dstDir := filepath.Join(t.TempDir(), "dst")
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)
	srcBefore, dstBefore := readFileBytes(t, srcDir), readFileBytes(t, dstDir)

	if err := os.Chmod(dstDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, err := importAssets(dstDir, srcDir, []string{"EQ-S"})
	if cerr := os.Chmod(dstDir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) ||
		string(readFileBytes(t, dstDir)) != string(dstBefore) {
		t.Fatal("写入失败不应改动任何文件")
	}
	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("写入失败后目标台账应可正常重载: %v", err)
	}
	if s.findAsset("EQ-S") != nil || len(s.data.Tickets) != 0 || len(s.data.Events) != 0 ||
		len(s.data.Requests) != 0 || s.data.NextTicketSeq != 1 {
		t.Fatal("写入失败不应留下部分导入，也不应消耗目标编号")
	}
	// 恢复可写后同一批导入可以重新提交成功。
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-S"}); err != nil {
		t.Fatalf("恢复可写后导入应成功: %v", err)
	}
	s2, err := openStore(dstDir)
	if err != nil || s2.findAsset("EQ-S") == nil {
		t.Fatalf("重新提交后应能重载到导入资产: %v", err)
	}
}

// 源台账不存在时不能当空库初始化；目标无台账时成功导入会创建；
// 同一台账不能导入自身。
func TestImportSourceAndSelfChecks(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-S", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	// 源不存在：失败，且目标不会被创建。
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	dstDir := filepath.Join(t.TempDir(), "dst")
	if _, err := importAssets(dstDir, missing, []string{"EQ-S"}); err == nil {
		t.Fatal("源台账不存在应失败")
	}
	if _, err := os.Stat(filepath.Join(dstDir, dataFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("源不存在时不应创建目标台账")
	}

	// 同一台账不能导入自身（同一路径）。
	if _, err := importAssets(srcDir, srcDir, []string{"EQ-S"}); err == nil {
		t.Fatal("同一台账导入自身应失败")
	}

	// 目标无台账：成功导入时创建。
	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-S"})
	if err != nil {
		t.Fatalf("目标无台账时应能创建并导入: %v", err)
	}
	if len(outcome.tickets) != 0 {
		t.Fatalf("无工单资产不应产生编号映射，得到 %+v", outcome.tickets)
	}
	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("导入后目标台账应可打开: %v", err)
	}
	if a := s.findAsset("EQ-S"); a == nil || a.Status != statusAvailable {
		t.Fatalf("导入资产应存在且为可用，得到 %+v", a)
	}
}

// 命令行入口：参数错误退出 2，业务失败退出 1，成功输出资产数量与编号映射。
func TestImportCommandLine(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-S", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-S", "卡纸", "req-s1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)
	dstDir := filepath.Join(t.TempDir(), "dst")

	var out, errOut bytes.Buffer
	if code := run([]string{"import", "--source-dir", srcDir, "--data-dir", dstDir}, &out, &errOut); code != 2 {
		t.Fatalf("缺少 --asset-id 应退出 2，得到 %d（%s）", code, errOut.String())
	}
	out.Reset()
	if code := run([]string{"import", "--source-dir", srcDir, "--asset-id", "EQ-S", "--data-dir", dstDir}, &out, &errOut); code != 0 {
		t.Fatalf("导入应成功，得到 %d（%s）", code, errOut.String())
	}
	if !strings.Contains(out.String(), "已导入 1 项资产") ||
		!strings.Contains(out.String(), "T0001 -> T0001") {
		t.Fatalf("输出应包含资产数量与编号映射，得到:\n%s", out.String())
	}
	// 再次导入同一批资产：编号冲突，退出 1。
	out.Reset()
	errOut.Reset()
	if code := run([]string{"import", "--source-dir", srcDir, "--asset-id", "EQ-S", "--data-dir", dstDir}, &out, &errOut); code != 1 {
		t.Fatalf("再次导入应退出 1，得到 %d（%s）", code, errOut.String())
	}
}
