package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest 把清单内容写入临时文件，返回路径。
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入清单文件: %v", err)
	}
	return path
}

// runBatch 执行 batch-report 命令，返回退出码与输出。
func runBatch(t *testing.T, dir, manifestPath string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run([]string{"batch-report", "--file", manifestPath, "--data-dir", dir}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// 混合批量报修：重放已关闭工单、新请求、批内重复项合并；新请求按清单首次
// 出现顺序分配连续工单编号与履历序号，重放与重复项不消耗编号；批量创建的
// 工单可正常派工、关闭、取消，并与单项 report 共享请求绑定。
func TestBatchReportMixedReplayAndNew(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{
		{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}, {"EQ-3", "饮水机", "三楼"},
	} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-old"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-2","description":"异响","request_id":"req-a"},
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-old"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-a"},
  {"asset_id":"EQ-3","description":"漏水","request_id":"req-b"}
]`)
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runBatch(t, dir, manifestPath)
	if code != 0 {
		t.Fatalf("批量报修应成功，退出码 %d: %s", code, errOut)
	}
	// 按请求标识首次出现顺序逐项显示，每个标识只显示一次。
	wantLines := []string{
		"批量报修完成：共 3 项请求（新增 2 项，重放 1 项）。",
		"req-a\tEQ-2\tT0002\t未关闭\t新增",
		"req-old\tEQ-1\tT0001\t已关闭\t重放",
		"req-b\tEQ-3\tT0003\t未关闭\t新增",
	}
	for _, l := range wantLines {
		if !strings.Contains(out, l) {
			t.Fatalf("输出缺少 %q:\n%s", l, out)
		}
	}
	if strings.Count(out, "req-a") != 1 {
		t.Fatalf("重复项应合并为一行输出:\n%s", out)
	}
	// 清单只读。
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(manifestBefore, manifestAfter) {
		t.Fatal("清单文件不应被修改")
	}

	// 重载核对：工单、状态、履历与请求绑定。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 3 || s2.data.NextTicketSeq != 4 {
		t.Fatalf("应有 3 张工单、下一序号 4，得到 %d 张、序号 %d",
			len(s2.data.Tickets), s2.data.NextTicketSeq)
	}
	if len(s2.data.Requests) != 3 {
		t.Fatalf("应有 3 条请求绑定，得到 %d", len(s2.data.Requests))
	}
	for id, want := range map[string]string{"EQ-1": statusAvailable, "EQ-2": statusRepairing, "EQ-3": statusRepairing} {
		if got := s2.findAsset(id).Status; got != want {
			t.Fatalf("资产 %s 状态 = %s, 想得到 %s", id, got, want)
		}
	}
	// 每张新单恰有一条报修履历；重放不新增履历（全库共 4 条：旧报修、关闭、两条新报修）。
	if len(s2.data.Events) != 4 {
		t.Fatalf("履历总数 = %d, 想得到 4", len(s2.data.Events))
	}
	reportCount := map[string]int{}
	reportSeq := map[string]int{}
	for _, e := range s2.data.Events {
		if e.Kind == eventReport {
			reportCount[e.TicketID]++
			reportSeq[e.TicketID] = e.Seq
		}
	}
	for _, tid := range []string{"T0001", "T0002", "T0003"} {
		if reportCount[tid] != 1 {
			t.Fatalf("工单 %s 报修履历 = %d 条, 想得到恰 1 条", tid, reportCount[tid])
		}
	}
	// 新请求按首次出现顺序分配连续履历序号。
	if !(reportSeq["T0002"] < reportSeq["T0003"]) {
		t.Fatalf("新单履历序号应按首次出现顺序连续分配: T0002=%d T0003=%d",
			reportSeq["T0002"], reportSeq["T0003"])
	}

	// 与单项 report 共享请求绑定：重放返回原工单，不新增记录。
	t2, replayed, err := s2.report("EQ-2", "异响", "req-a")
	if err != nil || !replayed || t2.ID != "T0002" {
		t.Fatalf("单项 report 重放批量请求应返回 T0002: ticket=%v replayed=%v err=%v", t2, replayed, err)
	}
	if len(s2.data.Events) != 4 {
		t.Fatal("重放不应新增履历")
	}
	// 批量创建的工单可正常派工、关闭、取消。
	if _, err := s2.assignTicket("T0002", "张三", "首次派工"); err != nil {
		t.Fatalf("批量工单派工失败: %v", err)
	}
	if _, _, err := s2.closeTicket("T0002", "已更换风扇"); err != nil {
		t.Fatalf("批量工单关闭失败: %v", err)
	}
	if _, _, err := s2.cancelTicket("T0003", "误报"); err != nil {
		t.Fatalf("批量工单取消失败: %v", err)
	}
	mustSave(t, s2)
}

// 批内冲突：同标识搭配不同描述（或不同资产）整批拒绝，指出两项位置，
// 不留下部分记录、不消耗编号。
func TestBatchReportInBatchConflict(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-x"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-y"},
  {"asset_id":"EQ-1","description":"无法开机","request_id":"req-x"}
]`)
	code, _, errOut := runBatch(t, dir, manifestPath)
	if code != 1 {
		t.Fatalf("批内冲突应整批失败（退出码 1），得到 %d", code)
	}
	if !strings.Contains(errOut, "第 3 项") || !strings.Contains(errOut, "第 1 项") ||
		!strings.Contains(errOut, "req-x") {
		t.Fatalf("错误应指出冲突两项的位置与标识: %s", errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("整批失败不应改动台账字节")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 0 || len(s2.data.Requests) != 0 || s2.data.NextTicketSeq != 1 {
		t.Fatal("整批失败不应留下工单、请求绑定或消耗编号")
	}

	// 同标识搭配不同资产同样整批拒绝。
	manifestPath2 := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-z"},
  {"asset_id":"EQ-2","description":"卡纸","request_id":"req-z"}
]`)
	if code, _, _ := runBatch(t, dir, manifestPath2); code != 1 {
		t.Fatalf("同标识不同资产应整批失败，得到 %d", code)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("整批失败不应改动台账字节")
	}
}

// 与已有绑定冲突：清单中的标识已在台账绑定，搭配不同资产或描述时整批拒绝。
func TestBatchReportConflictWithExistingBinding(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-old"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"另一种描述","request_id":"req-old"}
]`)
	code, _, errOut := runBatch(t, dir, manifestPath)
	if code != 1 {
		t.Fatalf("与已有绑定冲突应整批失败，得到 %d", code)
	}
	if !strings.Contains(errOut, "第 1 项") || !strings.Contains(errOut, "req-old") {
		t.Fatalf("错误应指出清单项位置与标识: %s", errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("整批失败不应改动台账字节")
	}
}

// 末项失败：前面的新请求都合法，最后一项资产未知，整批失败，
// 不留下部分记录或请求绑定、不消耗编号，台账字节不变。
func TestBatchReportLastItemFails(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-2"},
  {"asset_id":"EQ-X","description":"不存在","request_id":"req-3"}
]`)
	code, out, errOut := runBatch(t, dir, manifestPath)
	if code != 1 {
		t.Fatalf("末项失败应整批失败（退出码 1），得到 %d", code)
	}
	if !strings.Contains(errOut, "第 3 项") || !strings.Contains(errOut, "EQ-X") {
		t.Fatalf("错误应指出末项位置与资产: %s", errOut)
	}
	if strings.Contains(out, "新增") {
		t.Fatalf("不应输出部分成功结果:\n%s", out)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("整批失败不应改动台账字节")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 0 || len(s2.data.Requests) != 0 || len(s2.data.Events) != 0 ||
		s2.data.NextTicketSeq != 1 {
		t.Fatal("整批失败不应留下工单、履历、请求绑定或消耗编号")
	}
	// 恢复条件（修正清单）后可重试。
	manifestPath2 := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-2"}
]`)
	if code, _, errOut := runBatch(t, dir, manifestPath2); code != 0 {
		t.Fatalf("修正后重试应成功: %s", errOut)
	}
}

// 同一资产在本批出现两个不同的新请求（即使描述相同）整批拒绝；
// 旧请求重放与该资产的一项合法新报修可以共存。
func TestBatchReportSameAssetRules(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-old"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	// 两个不同的新请求指向同一资产：整批拒绝。
	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"相同描述","request_id":"req-n1"},
  {"asset_id":"EQ-1","description":"相同描述","request_id":"req-n2"}
]`)
	code, _, errOut := runBatch(t, dir, manifestPath)
	if code != 1 {
		t.Fatalf("同一资产两个新请求应整批失败，得到 %d", code)
	}
	if !strings.Contains(errOut, "第 2 项") || !strings.Contains(errOut, "第 1 项") {
		t.Fatalf("错误应指出两项位置: %s", errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("整批失败不应改动台账字节")
	}

	// 旧请求重放 + 该资产的一项合法新报修：可以共存。
	manifestPath2 := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-old"},
  {"asset_id":"EQ-1","description":"无法开机","request_id":"req-new"}
]`)
	code, out, errOut := runBatch(t, dir, manifestPath2)
	if code != 0 {
		t.Fatalf("重放与一项新报修应可共存: %s", errOut)
	}
	if !strings.Contains(out, "req-old\tEQ-1\tT0001\t已关闭\t重放") ||
		!strings.Contains(out, "req-new\tEQ-1\tT0002\t未关闭\t新增") {
		t.Fatalf("输出异常:\n%s", out)
	}
}

// 容量边界：工单编号或履历序号耗尽时含新请求的批次整批拒绝，
// 但纯重放不受影响（只读，不写文件）。
func TestBatchReportCapacityExhausted(t *testing.T) {
	// 工单编号耗尽：下一序号为 math.MaxInt。
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		m["next_ticket_seq"] = math.MaxInt
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
		})
	})
	before := readFileBytes(t, dir)

	mixed := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"不制冷","request_id":"req-2"}
]`)
	code, _, errOut := runBatch(t, dir, mixed)
	if code != 1 || !strings.Contains(errOut, "容量不足") {
		t.Fatalf("编号耗尽时含新请求的批次应整批失败: code=%d err=%s", code, errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("容量不足不应改动台账字节")
	}
	// 纯重放不受编号耗尽影响。
	replayOnly := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"}
]`)
	code, out, errOut := runBatch(t, dir, replayOnly)
	if code != 0 {
		t.Fatalf("纯重放不应受编号耗尽影响: %s", errOut)
	}
	if !strings.Contains(out, "req-1\tEQ-1\tT0001\t未关闭\t重放") {
		t.Fatalf("纯重放输出异常:\n%s", out)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("纯重放不应写文件")
	}

	// 履历序号耗尽：最大履历序号为 math.MaxInt。
	dir2 := t.TempDir()
	writeLedger(t, dir2, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
		})
	})
	before2 := readFileBytes(t, dir2)
	code, _, errOut = runBatch(t, dir2, mixed)
	if code != 1 || !strings.Contains(errOut, "履历序号容量不足") {
		t.Fatalf("履历序号耗尽时含新请求的批次应整批失败: code=%d err=%s", code, errOut)
	}
	if !bytes.Equal(before2, readFileBytes(t, dir2)) {
		t.Fatal("容量不足不应改动台账字节")
	}
	if code, _, errOut := runBatch(t, dir2, replayOnly); code != 0 {
		t.Fatalf("纯重放不应受履历序号耗尽影响: %s", errOut)
	}
	if !bytes.Equal(before2, readFileBytes(t, dir2)) {
		t.Fatal("纯重放不应写文件")
	}
}

// 保存失败：读写失败整批失败、原文件字节不变；恢复条件后重载重试成功，
// 不留下部分记录、不重复消耗编号。
func TestBatchReportSaveFailureThenRetry(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-2"}
]`)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runBatch(t, dir, manifestPath)
	if code != 1 {
		t.Fatalf("保存失败应整批失败（退出码 1），得到 %d", code)
	}
	if !strings.Contains(errOut, "失败") {
		t.Fatalf("应说明读写失败原因: %s", errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("保存失败不应改动台账字节")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 重载后重试：从下一序号正常分配，不重复消耗编号。
	code, out, errOut := runBatch(t, dir, manifestPath)
	if code != 0 {
		t.Fatalf("恢复后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "req-1\tEQ-1\tT0001\t未关闭\t新增") ||
		!strings.Contains(out, "req-2\tEQ-2\tT0002\t未关闭\t新增") {
		t.Fatalf("重试输出异常:\n%s", out)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 2 || len(s2.data.Requests) != 2 || len(s2.data.Events) != 2 ||
		s2.data.NextTicketSeq != 3 {
		t.Fatal("重试后应恰有两张工单、两条绑定、两条履历，下一序号为 3")
	}
}

// 清单格式错误（退出码 2）：空文件、空数组、非数组、缺字段、空字段、
// 多余字段、多余内容；缺 --file 参数也是退出码 2；清单文件读不到为退出码 1。
func TestBatchReportManifestFormatErrors(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"空文件", "", "清单文件为空"},
		{"空数组", "[]", "清单为空"},
		{"null", "null", "清单为空"},
		{"非法 JSON", "[{", "清单格式错误"},
		{"非数组", `{"asset_id":"EQ-1"}`, "清单格式错误"},
		{"多余内容", `[{"asset_id":"EQ-1","description":"x","request_id":"r"}] []`, "多余内容"},
		{"缺 request_id", `[{"asset_id":"EQ-1","description":"卡纸"}]`, "第 1 项"},
		{"空 asset_id", `[{"asset_id":"","description":"卡纸","request_id":"r"}]`, "第 1 项"},
		{"空 description", `[{"asset_id":"EQ-1","description":"","request_id":"r"}]`, "第 1 项"},
		{"多余字段", `[{"asset_id":"EQ-1","description":"卡纸","request_id":"r","batch_id":"b1"}]`, "清单格式错误"},
		{"第二项缺字段", `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"r1"},
  {"asset_id":"EQ-1","description":"卡纸"}
]`, "第 2 项"},
	}
	for _, c := range cases {
		manifestPath := writeManifest(t, c.content)
		code, _, errOut := runBatch(t, dir, manifestPath)
		if code != 2 {
			t.Errorf("%s: 应为退出码 2，得到 %d（%s）", c.name, code, errOut)
			continue
		}
		if !strings.Contains(errOut, c.want) {
			t.Errorf("%s: 错误应包含 %q: %s", c.name, c.want, errOut)
		}
	}
	// 缺 --file 参数。
	var out, errBuf bytes.Buffer
	if code := run([]string{"batch-report", "--data-dir", dir}, &out, &errBuf); code != 2 {
		t.Fatalf("缺少 --file 应为退出码 2，得到 %d", code)
	}
	// 清单文件不存在为读取失败（退出码 1）。
	code, _, _ := runBatch(t, dir, filepath.Join(t.TempDir(), "no-such.json"))
	if code != 1 {
		t.Fatalf("清单读不到应为退出码 1，得到 %d", code)
	}
	// 格式错误一律不改动台账。
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("清单格式错误不应改动台账字节")
	}
}

// 纯重放为只读：全部项都是已有绑定（含已关闭、已取消工单与停用资产），
// 不写文件、不初始化目录，返回原工单及当前状态，不重开原单。
func TestBatchReportPureReplayReadOnly(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}, {"EQ-3", "饮水机", "三楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-2", "异响", "req-2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-2", "不制冷", "req-3"); err != nil { // T0003，后来工单
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-3", "调拨"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifestPath := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-2"},
  {"asset_id":"EQ-2","description":"不制冷","request_id":"req-3"}
]`)
	code, out, errOut := runBatch(t, dir, manifestPath)
	if code != 0 {
		t.Fatalf("纯重放应成功: %s", errOut)
	}
	for _, l := range []string{
		"批量报修完成：共 3 项请求（新增 0 项，重放 3 项）。",
		"req-1\tEQ-1\tT0001\t已关闭\t重放",
		"req-2\tEQ-2\tT0002\t已取消\t重放",
		"req-3\tEQ-2\tT0003\t未关闭\t重放",
	} {
		if !strings.Contains(out, l) {
			t.Fatalf("输出缺少 %q:\n%s", l, out)
		}
	}
	// 只读：台账字节不变，原单不重开，资产与后来工单不变。
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("纯重放不应写文件")
	}
}

// 新报修的业务拒绝：未知资产、停用资产、已有未关闭工单均整批失败（退出码 1）。
func TestBatchReportBusinessRejections(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-open"); err != nil { // T0001 未关闭
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-2", "调拨"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	// 已有未关闭工单。
	m1 := writeManifest(t, `[{"asset_id":"EQ-1","description":"新故障","request_id":"req-n1"}]`)
	code, _, errOut := runBatch(t, dir, m1)
	if code != 1 || !strings.Contains(errOut, "已有未关闭工单 T0001") {
		t.Fatalf("已有未关闭工单应拒绝: code=%d err=%s", code, errOut)
	}
	// 停用资产。
	m2 := writeManifest(t, `[{"asset_id":"EQ-2","description":"新故障","request_id":"req-n2"}]`)
	code, _, errOut = runBatch(t, dir, m2)
	if code != 1 || !strings.Contains(errOut, "已停用") {
		t.Fatalf("停用资产应拒绝: code=%d err=%s", code, errOut)
	}
	// 未知资产。
	m3 := writeManifest(t, `[{"asset_id":"EQ-X","description":"新故障","request_id":"req-n3"}]`)
	code, _, errOut = runBatch(t, dir, m3)
	if code != 1 || !strings.Contains(errOut, "未知资产") {
		t.Fatalf("未知资产应拒绝: code=%d err=%s", code, errOut)
	}
	if !bytes.Equal(before, readFileBytes(t, dir)) {
		t.Fatal("业务拒绝不应改动台账字节")
	}
	// 被拒绝的新报修不绑定请求标识：恢复条件后可用同一标识重试。
	if _, err := s.reactivateAsset("EQ-2", "调拨回库"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	code, out, errOut := runBatch(t, dir, m2)
	if code != 0 || !strings.Contains(out, "req-n2\tEQ-2\tT0002\t未关闭\t新增") {
		t.Fatalf("恢复后同一标识重试应成功: code=%d out=%s err=%s", code, out, errOut)
	}
}

// 空数据目录：含新请求的批次在首次成功写入时自动初始化目录。
func TestBatchReportInitializesEmptyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	manifestPath := writeManifest(t, `[{"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"}]`)
	// 资产尚未登记，整批失败且不初始化目录。
	if code, _, _ := runBatch(t, dir, manifestPath); code != 1 {
		t.Fatalf("未知资产应整批失败，得到 %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, dataFileName)); !os.IsNotExist(err) {
		t.Fatal("失败批次不应初始化数据目录")
	}
	// 先登记资产，再批量报修成功并初始化目录。
	var out, errBuf bytes.Buffer
	if code := run([]string{"register", "--data-dir", dir, "--asset-id", "EQ-1",
		"--name", "打印机", "--location", "一楼"}, &out, &errBuf); code != 0 {
		t.Fatalf("登记资产失败: %s", errBuf.String())
	}
	if code, _, errOut := runBatch(t, dir, manifestPath); code != 0 {
		t.Fatalf("批量报修应成功: %s", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, dataFileName)); err != nil {
		t.Fatal("成功批次应已初始化数据目录")
	}
}
