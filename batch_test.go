package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest 把报修清单写入临时文件，返回路径与字节内容。
func writeManifest(t *testing.T, content string) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	raw := []byte(content)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

// runBatch 执行 batch-report 命令，返回退出码与输出。
func runBatch(t *testing.T, dir, manifest string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run([]string{"batch-report", "--file", manifest, "--data-dir", dir}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// 混合批量报修：既有绑定重放（原单已关闭）、批内重复项合并、新请求开单、
// 旧请求重放与同资产的一项合法新报修共存。新单按清单首次出现顺序分配连续
// 工单编号与履历序号；与单项 report 共享请求绑定；批量工单可正常派工、关闭、取消。
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

	manifest, manifestBytes := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-old"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-b"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-b"},
  {"asset_id":"EQ-3","description":"漏水","request_id":"req-c"},
  {"asset_id":"EQ-1","description":"无法开机","request_id":"req-d"}
]`)

	code, out, errOut := runBatch(t, dir, manifest)
	if code != 0 {
		t.Fatalf("批量报修应成功，退出码 = %d: %s", code, errOut)
	}
	if !strings.Contains(out, "共 4 项请求（新增 3 项，重放 1 项）") {
		t.Fatalf("输出应汇总 4 项唯一请求（重复项合并），得到:\n%s", out)
	}
	// 按请求标识首次出现顺序逐项显示，每个标识只显示一次。
	wantRows := []string{
		"req-old\tEQ-1\tT0001\t已关闭\t重放",
		"req-b\tEQ-2\tT0002\t未关闭\t新增",
		"req-c\tEQ-3\tT0003\t未关闭\t新增",
		"req-d\tEQ-1\tT0004\t未关闭\t新增",
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1+len(wantRows) {
		t.Fatalf("输出应为表头加 %d 行，得到 %d 行:\n%s", len(wantRows), len(lines), out)
	}
	for i, row := range wantRows {
		if lines[i+1] != row {
			t.Fatalf("第 %d 行 = %q，应为 %q", i+1, lines[i+1], row)
		}
	}
	// 清单文件只读。
	got, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(got, manifestBytes) {
		t.Fatal("批量报修不应修改清单文件")
	}

	// 重载核对：新单连续编号、每张新单恰一条报修履历、资产状态正确、
	// 不追加批次业务事件。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.data.NextTicketSeq != 5 || len(s2.data.Tickets) != 4 || len(s2.data.Requests) != 4 {
		t.Fatalf("应有 4 张工单、4 条绑定、下一序号 5，得到 next=%d tickets=%d requests=%d",
			s2.data.NextTicketSeq, len(s2.data.Tickets), len(s2.data.Requests))
	}
	reportEvents := map[string]int{}
	for _, e := range s2.data.Events {
		if e.Kind == eventReport {
			reportEvents[e.TicketID]++
		}
	}
	if len(s2.data.Events) != 5 { // 报修+关闭（既有）+ 3 条新报修履历
		t.Fatalf("不应追加批次业务事件，履历总数应为 5，得到 %d", len(s2.data.Events))
	}
	for _, id := range []string{"T0002", "T0003", "T0004"} {
		tk := s2.findTicket(id)
		if tk == nil || tk.Status != ticketOpen {
			t.Fatalf("新单 %s 应为未关闭，得到 %+v", id, tk)
		}
		if reportEvents[id] != 1 {
			t.Fatalf("新单 %s 应恰有一条报修履历，得到 %d", id, reportEvents[id])
		}
	}
	for _, id := range []string{"EQ-1", "EQ-2", "EQ-3"} {
		if a := s2.findAsset(id); a.Status != statusRepairing {
			t.Fatalf("资产 %s 应为维修中，得到 %s", id, a.Status)
		}
	}

	// 单项 report 与批量入口共享请求绑定：重放 req-b 返回 T0002，不新增记录。
	var out2, errBuf2 bytes.Buffer
	if code := run([]string{"report", "--data-dir", dir, "--asset-id", "EQ-2",
		"--description", "异响", "--request-id", "req-b"}, &out2, &errBuf2); code != 0 {
		t.Fatalf("单项 report 重放应成功: %s", errBuf2.String())
	}
	if !strings.Contains(out2.String(), "T0002（既有工单）") {
		t.Fatalf("单项 report 重放应返回 T0002，得到:\n%s", out2.String())
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s3.data.Tickets) != 4 || s3.data.NextTicketSeq != 5 {
		t.Fatal("重放不应新增工单或消耗编号")
	}

	// 批量创建的工单可正常派工、关闭、取消。
	var o, e bytes.Buffer
	if code := run([]string{"assign", "--data-dir", dir, "--ticket-id", "T0002",
		"--assignee", "张三", "--note", "首次派工"}, &o, &e); code != 0 {
		t.Fatalf("批量工单应可派工: %s", e.String())
	}
	o.Reset()
	e.Reset()
	if code := run([]string{"close", "--data-dir", dir, "--ticket-id", "T0003",
		"--repair-result", "已修复"}, &o, &e); code != 0 {
		t.Fatalf("批量工单应可关闭: %s", e.String())
	}
	o.Reset()
	e.Reset()
	if code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T0004",
		"--reason", "误报"}, &o, &e); code != 0 {
		t.Fatalf("批量工单应可取消: %s", e.String())
	}
}

// 批内冲突：同一请求标识搭配不同资产或不同描述，整批拒绝（退出码 1），
// 指出冲突双方位置，不留下任何记录，数据目录不被初始化。
func TestBatchReportInBatchConflict(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
	}{
		{"不同资产", `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-x"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-x"}
]`},
		{"同资产不同描述", `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-x"},
  {"asset_id":"EQ-1","description":"无法开机","request_id":"req-x"}
]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			manifest, _ := writeManifest(t, tc.manifest)
			code, out, errOut := runBatch(t, dir, manifest)
			if code != 1 {
				t.Fatalf("批内冲突应退出 1，得到 %d（输出 %s）", code, out)
			}
			if !strings.Contains(errOut, "第 2 项") || !strings.Contains(errOut, "第 1 项") ||
				!strings.Contains(errOut, "req-x") {
				t.Fatalf("应指出冲突双方位置与请求标识，得到: %s", errOut)
			}
			if out != "" {
				t.Fatalf("整批失败不应输出部分成功结果，得到: %s", out)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("整批失败不应初始化数据目录或留下部分记录")
			}
		})
	}
}

// 与已有绑定冲突：清单中的请求标识已被绑定到其他描述，即使前项是合法新报修，
// 也整批拒绝，不留下部分记录或请求绑定、不消耗编号。
func TestBatchReportConflictWithExistingBinding(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-2","description":"异响","request_id":"req-new"},
  {"asset_id":"EQ-1","description":"不同描述","request_id":"req-1"}
]`)
	code, out, errOut := runBatch(t, dir, manifest)
	if code != 1 {
		t.Fatalf("与已有绑定冲突应退出 1，得到 %d（输出 %s）", code, out)
	}
	if !strings.Contains(errOut, "清单第 2 项") || !strings.Contains(errOut, "req-1") {
		t.Fatalf("应指出冲突清单项位置与请求标识，得到: %s", errOut)
	}
	if out != "" {
		t.Fatalf("整批失败不应输出部分成功结果，得到: %s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("整批失败不应改动台账文件")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 1 || len(s2.data.Requests) != 1 || s2.data.NextTicketSeq != 2 {
		t.Fatal("整批失败不应留下部分记录或请求绑定、不应消耗编号")
	}
}

// 末项失败：最后一项为未知资产时整批失败，台账字节不变、编号未消耗；
// 修正清单后可重试成功。
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

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-a"},
  {"asset_id":"EQ-9","description":"不存在","request_id":"req-b"}
]`)
	code, _, errOut := runBatch(t, dir, manifest)
	if code != 1 {
		t.Fatalf("末项未知资产应整批失败（退出 1），得到 %d", code)
	}
	if !strings.Contains(errOut, "清单第 2 项") || !strings.Contains(errOut, "未知资产") {
		t.Fatalf("应指出失败项位置与原因，得到: %s", errOut)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("整批失败不应改动台账文件")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 0 || len(s2.data.Requests) != 0 || s2.data.NextTicketSeq != 1 {
		t.Fatal("末项失败不应留下部分记录或请求绑定、不应消耗编号")
	}

	// 修正清单后重试：编号从 1 连续分配。
	manifest2, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-a"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-b"}
]`)
	code, out, errOut := runBatch(t, dir, manifest2)
	if code != 0 {
		t.Fatalf("修正后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "req-a\tEQ-1\tT0001") || !strings.Contains(out, "req-b\tEQ-2\tT0002") {
		t.Fatalf("重试应从下一序号连续分配 T0001、T0002，得到:\n%s", out)
	}
}

// 容量边界：工单编号只够一张新单时，含两张新单的批次整批失败；编号与履历
// 序号耗尽都不妨碍纯重放（只读，不写文件）。
func TestBatchReportCapacityBoundary(t *testing.T) {
	// 下一工单序号为 math.MaxInt-1：只够分配一张新工单。
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		m["next_ticket_seq"] = math.MaxInt - 1
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
		})
		appendTo(m, "assets", map[string]any{
			"id": "EQ-3", "name": "饮水机", "location": "三楼", "status": "可用",
		})
	})
	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-new1"},
  {"asset_id":"EQ-3","description":"漏水","request_id":"req-new2"}
]`)
	code, _, errOut := runBatch(t, dir, manifest)
	if code != 1 || !strings.Contains(errOut, "耗尽") {
		t.Fatalf("编号容量不足应整批失败并说明原因，退出码 = %d: %s", code, errOut)
	}
	if !bytes.Equal(readFileBytes(t, dir), raw) {
		t.Fatal("容量不足不应改动台账文件")
	}

	// 纯重放不受编号耗尽影响，且不写文件。
	replayOnly, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-1"}
]`)
	code, out, errOut := runBatch(t, dir, replayOnly)
	if code != 0 {
		t.Fatalf("编号耗尽不应妨碍合法重放: %s", errOut)
	}
	if !strings.Contains(out, "req-1\tEQ-1\tT0001\t未关闭\t重放") {
		t.Fatalf("纯重放应返回原工单及当前状态，得到:\n%s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), raw) {
		t.Fatal("纯重放为只读，不应改动台账文件")
	}

	// 履历序号耗尽：新单整批失败，纯重放仍成功。
	dir2 := t.TempDir()
	raw2 := writeLedger(t, dir2, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
		})
	})
	manifest2, _ := writeManifest(t, `[
  {"asset_id":"EQ-2","description":"异响","request_id":"req-new"}
]`)
	code, _, errOut = runBatch(t, dir2, manifest2)
	if code != 1 || !strings.Contains(errOut, "履历序号") {
		t.Fatalf("履历序号耗尽应整批失败并说明原因，退出码 = %d: %s", code, errOut)
	}
	if !bytes.Equal(readFileBytes(t, dir2), raw2) {
		t.Fatal("履历序号耗尽不应改动台账文件")
	}
	code, _, errOut = runBatch(t, dir2, replayOnly)
	if code != 0 {
		t.Fatalf("履历序号耗尽不应妨碍合法重放: %s", errOut)
	}
	if !bytes.Equal(readFileBytes(t, dir2), raw2) {
		t.Fatal("纯重放为只读，不应改动台账文件")
	}
}

// 纯重放只读：原单已关闭且资产停用、或原单仍未关闭，重放都返回原工单及当前
// 状态，不重开原单、不改变资产或后来工单，台账字节不变。
func TestBatchReportPureReplayReadOnly(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-old"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-2", "异响", "req-live"); err != nil { // T0002
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-old"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-live"}
]`)
	code, out, errOut := runBatch(t, dir, manifest)
	if code != 0 {
		t.Fatalf("纯重放应成功: %s", errOut)
	}
	if !strings.Contains(out, "req-old\tEQ-1\tT0001\t已关闭\t重放") ||
		!strings.Contains(out, "req-live\tEQ-2\tT0002\t未关闭\t重放") {
		t.Fatalf("重放应返回原工单及当前状态，得到:\n%s", out)
	}
	if !strings.Contains(out, "未创建工单、未写入任何记录") {
		t.Fatalf("纯重放应明确提示未写入记录，得到:\n%s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("纯重放不应改动台账文件")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := s2.findAsset("EQ-1"); a.Status != statusDeactivated {
		t.Fatalf("重放不应改变资产状态，EQ-1 应为停用，得到 %s", a.Status)
	}
	if tk := s2.findTicket("T0001"); tk.Status != ticketClosed {
		t.Fatalf("重放不应重开原单，T0001 应为已关闭，得到 %s", tk.Status)
	}
}

// 保存失败（目录不可写）：整批失败，原文件字节不变，不留下部分记录或请求
// 绑定、不消耗编号；恢复写入条件后重载重试成功。
func TestBatchReportSaveFailureReloadRetry(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"req-a"},
  {"asset_id":"EQ-2","description":"异响","request_id":"req-b"}
]`)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runBatch(t, dir, manifest)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	if code != 1 {
		t.Fatalf("写入失败应退出 1，得到 %d", code)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	// 重载后看不到部分变化：无工单、无绑定，编号未消耗。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if len(s2.data.Tickets) != 0 || len(s2.data.Requests) != 0 || s2.data.NextTicketSeq != 1 {
		t.Fatal("写入失败不应留下部分记录或请求绑定、不应消耗编号")
	}
	// 恢复条件后重试成功，编号从 1 连续分配。
	code, out, errOut := runBatch(t, dir, manifest)
	if code != 0 {
		t.Fatalf("恢复写入条件后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "req-a\tEQ-1\tT0001") || !strings.Contains(out, "req-b\tEQ-2\tT0002") {
		t.Fatalf("重试应分配 T0001、T0002，得到:\n%s", out)
	}
}

// 清单格式错误（退出码 2）：空清单、格式错误、缺少必填内容、未知字段、
// 多余内容均整批拒绝并指出位置；清单文件读取失败为退出码 1。
func TestBatchReportManifestFormatErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"空文件", "", "为空"},
		{"仅空白", "  \n ", "为空"},
		{"空数组", "[]", "为空"},
		{"非法 JSON", "{", "格式错误"},
		{"非数组", `{"asset_id":"EQ-1"}`, "格式错误"},
		{"多余内容", "[] {}", "多余内容"},
		{"未知字段", `[{"asset_id":"EQ-1","description":"卡纸","request_id":"r1","batch_id":"b1"}]`, "格式错误"},
		{"缺资产编号", `[{"description":"卡纸","request_id":"r1"}]`, "清单第 1 项"},
		{"空故障描述", `[{"asset_id":"EQ-1","description":"","request_id":"r1"}]`, "清单第 1 项"},
		{"缺请求标识", `[{"asset_id":"EQ-1","description":"卡纸"}]`, "清单第 1 项"},
		{"空请求标识", `[{"asset_id":"EQ-1","description":"卡纸","request_id":" "}]`, ""}, // 非空即合法
		{"第二项缺内容", `[
  {"asset_id":"EQ-1","description":"卡纸","request_id":"r1"},
  {"asset_id":"EQ-2","request_id":"r2"}
]`, "清单第 2 项"},
		{"空对象项", `[null]`, "清单第 1 项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			manifest, _ := writeManifest(t, tc.content)
			code, _, errOut := runBatch(t, dir, manifest)
			if tc.name == "空请求标识" {
				// 仅空白字符的请求标识非空，进入业务流程：未知资产（退出码 1）。
				if code != 1 {
					t.Fatalf("非空请求标识应进入业务流程，退出码 = %d", code)
				}
				return
			}
			if code != 2 {
				t.Fatalf("清单格式错误应退出 2，得到 %d: %s", code, errOut)
			}
			if tc.want != "" && !strings.Contains(errOut, tc.want) {
				t.Fatalf("错误信息应包含 %q，得到: %s", tc.want, errOut)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("清单格式错误不应初始化数据目录")
			}
		})
	}

	// 清单文件不存在：读取失败，退出码 1。
	dir := t.TempDir()
	code, _, errOut := runBatch(t, dir, filepath.Join(dir, "不存在.json"))
	if code != 1 || !strings.Contains(errOut, "读取报修清单失败") {
		t.Fatalf("清单读取失败应退出 1 并说明原因，退出码 = %d: %s", code, errOut)
	}
	// 缺少 --file 参数：退出码 2。
	var out, errBuf bytes.Buffer
	if code := run([]string{"batch-report", "--data-dir", dir}, &out, &errBuf); code != 2 {
		t.Fatalf("缺少 --file 应退出 2，得到 %d", code)
	}
}
