package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeMaintManifest 把保养完成清单写入临时文件，返回路径与字节内容。
func writeMaintManifest(t *testing.T, content string) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maint-manifest.json")
	raw := []byte(content)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

// runBatchMaint 执行 batch-maintain 命令，返回退出码与输出。
func runBatchMaint(t *testing.T, dir, manifest string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run([]string{"batch-maintain", "--file", manifest, "--data-dir", dir}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// batchRow 解析批量保养成功输出中的一行结果。
func parseBatchMaintRow(t *testing.T, line string) (pos int, asset, due string, seq int, next string) {
	t.Helper()
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		t.Fatalf("结果行应有 5 个字段，得到 %q", line)
	}
	pos, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("项号不是整数: %q", line)
	}
	seq, err = strconv.Atoi(fields[3])
	if err != nil {
		t.Fatalf("完成序号不是整数: %q", line)
	}
	return pos, fields[1], fields[2], seq, fields[4]
}

// 交错接续与延期：多资产交错、同一资产多次出现，延期跳过的周期不补记录；
// 完成序号按清单顺序连续分配；维修中（含待验收）可登记且工单与资产状态不变；
// detail、due 显示最终日期，replay 反映各项进度；清单文件只读。
func TestBatchMaintainInterleavedDelayAndReplay(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{
		{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}, {"EQ-3", "饮水机", "三楼"},
	} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-02-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-3", "巡检", "2026-03-01", 30); err != nil {
		t.Fatal(err)
	}
	// EQ-3 维修中（未关闭工单）。
	if _, _, err := s.report("EQ-3", "漏水", "req-3"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	seg1, seg2, seg3 := s.currentSegmentSeq("EQ-1"), s.currentSegmentSeq("EQ-2"), s.currentSegmentSeq("EQ-3")

	manifest, manifestBytes := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-25","result":"延期完成","segment_seq":%d},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"按时完成","segment_seq":%d},
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-01-31","result":"第二次","segment_seq":%d},
  {"asset_id":"EQ-1","due":"2026-02-10","done":"2026-02-10","result":"第三次","segment_seq":%d},
  {"asset_id":"EQ-2","due":"2026-03-03","done":"2026-03-10","result":"延期完成","segment_seq":%d},
  {"asset_id":"EQ-3","due":"2026-03-01","done":"2026-03-01","result":"维修中完成","segment_seq":%d}
]`, seg1, seg2, seg1, seg1, seg2, seg3))

	code, out, errOut := runBatchMaint(t, dir, manifest)
	if code != 0 {
		t.Fatalf("批量登记应成功，退出码 = %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	wantRows := []struct {
		pos        int
		asset, due string
		seq        int
		next       string
	}{
		{1, "EQ-1", "2026-01-01", 5, "2026-01-31"}, // 既有 3 条建立 + 1 条报修履历
		{2, "EQ-2", "2026-02-01", 6, "2026-03-03"},
		{3, "EQ-1", "2026-01-31", 7, "2026-02-10"},
		{4, "EQ-1", "2026-02-10", 8, "2026-02-20"},
		{5, "EQ-2", "2026-03-03", 9, "2026-04-02"}, // 02-01 + 2*30，严格晚于 03-10
		{6, "EQ-3", "2026-03-01", 10, "2026-03-31"},
	}
	if len(lines) != 2+len(wantRows) {
		t.Fatalf("输出应为表头两行加 %d 行，得到 %d 行:\n%s", len(wantRows), len(lines), out)
	}
	if !strings.Contains(lines[0], "共 6 项") {
		t.Fatalf("汇总行不对: %q", lines[0])
	}
	for i, want := range wantRows {
		pos, asset, due, seq, next := parseBatchMaintRow(t, lines[i+2])
		if pos != want.pos || asset != want.asset || due != want.due || seq != want.seq || next != want.next {
			t.Fatalf("第 %d 行 = (%d,%s,%s,%d,%s)，想得到 %+v", i+1, pos, asset, due, seq, next, want)
		}
	}
	// 清单文件只读。
	if got, err := os.ReadFile(manifest); err != nil || !bytes.Equal(got, manifestBytes) {
		t.Fatal("批量登记不应修改清单文件")
	}

	// 重载核对：计划最终日期、履历条数、不追加批次事件。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-02-20" {
		t.Fatalf("EQ-1 最终下一到期日 = %q，想得到 2026-02-20", got.NextDue)
	}
	if got := s2.findPlan("EQ-2"); got.NextDue != "2026-04-02" {
		t.Fatalf("EQ-2 最终下一到期日 = %q，想得到 2026-04-02", got.NextDue)
	}
	if got := s2.findPlan("EQ-3"); got.NextDue != "2026-03-31" {
		t.Fatalf("EQ-3 最终下一到期日 = %q，想得到 2026-03-31", got.NextDue)
	}
	if len(s2.data.Events) != 10 {
		t.Fatalf("履历总数应为 10（不追加批次事件），得到 %d", len(s2.data.Events))
	}
	// 维修中资产状态与工单不变，保养不消耗工单编号。
	if a := s2.findAsset("EQ-3"); a.Status != statusRepairing {
		t.Fatalf("EQ-3 应保持维修中，得到 %s", a.Status)
	}
	if tk := s2.openTicketOf("EQ-3"); tk == nil || tk.ID != "T0001" || tk.Status != ticketOpen {
		t.Fatalf("EQ-3 的未关闭工单应保持不变，得到 %v", tk)
	}
	if s2.data.NextTicketSeq != 2 {
		t.Fatalf("保养不应消耗工单编号，NextTicketSeq = %d", s2.data.NextTicketSeq)
	}

	// replay 反映各项进度：在中间完成序号处回看，下一到期日为当时推进值。
	for _, tc := range []struct {
		cutoff int
		next   string
	}{
		{5, "2026-01-31"}, // 仅第 1 项后
		{7, "2026-02-10"}, // 第 3 项后
		{8, "2026-02-20"}, // 第 4 项后
	} {
		snap := s2.replayAsset("EQ-1", tc.cutoff)
		if snap.plan == nil || snap.plan.nextDue != tc.next {
			got := ""
			if snap.plan != nil {
				got = snap.plan.nextDue
			}
			t.Fatalf("replay 截止 %d 的下一到期日 = %q，想得到 %q", tc.cutoff, got, tc.next)
		}
	}

	// detail、due 显示最终日期。
	var buf bytes.Buffer
	if err := cmdDetail([]string{"--data-dir", dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "下一到期日: 2026-02-20") {
		t.Fatalf("detail 应显示最终日期:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", dir, "--date", "2026-03-31"}, &buf); err != nil {
		t.Fatal(err)
	}
	dueOut := buf.String()
	if !strings.Contains(dueOut, "EQ-1") || !strings.Contains(dueOut, "EQ-3") {
		t.Fatalf("due 应包含已到期的 EQ-1、EQ-3:\n%s", dueOut)
	}
	if strings.Contains(dueOut, "EQ-2") {
		t.Fatalf("EQ-2 下一到期日为 2026-04-02，不应在 2026-03-31 到期:\n%s", dueOut)
	}

	// 待验收期间仍可登记，工单与资产状态保持待验收/维修中。
	var o, e bytes.Buffer
	if code := run([]string{"submit", "--data-dir", dir, "--ticket-id", "T0001",
		"--repair-result", "待验收结果"}, &o, &e); code != 0 {
		t.Fatalf("submit: %d %s", code, e.String())
	}
	manifest2, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-3","due":"2026-03-31","done":"2026-04-01","result":"待验收中完成","segment_seq":%d}
]`, seg3))
	code, out, errOut = runBatchMaint(t, dir, manifest2)
	if code != 0 {
		t.Fatalf("待验收期间应可登记保养完成: %s", errOut)
	}
	if !strings.Contains(out, "EQ-3\t2026-03-31\t12\t2026-04-30") {
		// submit 占序号 11，本次完成序号 12；03-01 + 2*30 = 04-30 严格晚于 04-01。
		t.Fatalf("待验收中登记的行不对:\n%s", out)
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := s3.findAsset("EQ-3"); a.Status != statusRepairing {
		t.Fatalf("登记后资产应保持维修中，得到 %s", a.Status)
	}
	if tk := s3.findTicket("T0001"); tk == nil || tk.Status != ticketPending {
		t.Fatalf("工单应保持待验收，得到 %v", tk)
	}
}

// 同日期旧段：旧方案段与新方案段的周期网格恰有相同日期，且该日期正是当前
// 下一到期日；用旧段序号登记仍整批拒绝，改用当前段序号才成功。
func TestBatchMaintainSameDateOldSegment(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "首周期"); err != nil {
		t.Fatal(err)
	}
	// 新段首次到期日 2026-01-31（严格晚于完成日 2026-01-01），与旧段网格
	// （01-01 + 30k）重合：2026-01-31 同时是旧段下一周期与新段首次到期日。
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-01-31", 30, "型号升级"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	oldSeg, curSeg := 1, s.currentSegmentSeq("EQ-1")
	if curSeg == oldSeg {
		t.Fatalf("调整后当前段序号应变化，得到 %d", curSeg)
	}
	before := readFileBytes(t, dir)

	manifest, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-02-01","result":"误用旧段","segment_seq":%d}
]`, oldSeg))
	code, out, errOut := runBatchMaint(t, dir, manifest)
	if code != 1 {
		t.Fatalf("同日期旧段应整批拒绝（退出 1），得到 %d: %s", code, out)
	}
	if !strings.Contains(errOut, "清单第 1 项") ||
		!strings.Contains(errOut, "不是资产 EQ-1 的当前方案段") ||
		!strings.Contains(errOut, fmt.Sprintf("当前段序号为 %d", curSeg)) {
		t.Fatalf("应指出项号、旧段与当前段序号，得到: %s", errOut)
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
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-31" {
		t.Fatalf("拒绝后不应推进日期，下一到期日 = %q", got.NextDue)
	}
	if got := len(s2.data.Events); got != 3 {
		t.Fatalf("拒绝后不应新增履历，履历数 = %d", got)
	}

	// 用当前段序号登记同一日期成功。
	manifest2, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-02-01","result":"当前段完成","segment_seq":%d}
]`, curSeg))
	code, out, errOut = runBatchMaint(t, dir, manifest2)
	if code != 0 {
		t.Fatalf("当前段登记应成功: %s", errOut)
	}
	if !strings.Contains(out, "EQ-1\t2026-01-31\t4\t2026-03-02") {
		t.Fatalf("成功行不对（01-31 + 2*30 = 03-02 严格晚于 02-01）:\n%s", out)
	}
}

// 末项失败与重复周期：未知资产末项、同一周期重复出现均整批失败，台账字节
// 不变、日期不推进、序号不消耗；修正后用原目录重试成功，序号接续。
func TestBatchMaintainLastItemAndDuplicateFail(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-02-01", 30); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	seg1, seg2 := s.currentSegmentSeq("EQ-1"), s.currentSegmentSeq("EQ-2")
	before := readFileBytes(t, dir)

	// 末项未知资产：整批失败，前项也不生效。
	badLast, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%d},
  {"asset_id":"EQ-9","due":"2026-01-01","done":"2026-01-01","result":"未知资产","segment_seq":%d}
]`, seg1, seg2))
	code, out, errOut := runBatchMaint(t, dir, badLast)
	if code != 1 {
		t.Fatalf("末项未知资产应整批失败，得到 %d", code)
	}
	if !strings.Contains(errOut, "清单第 2 项") || !strings.Contains(errOut, "未知资产") {
		t.Fatalf("应指出第 2 项及原因，得到: %s", errOut)
	}
	if out != "" || !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatalf("整批失败不应输出结果或改动台账，out=%q", out)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("失败不应推进日期，下一到期日 = %q", got.NextDue)
	}
	if len(s2.data.Events) != 2 {
		t.Fatalf("失败不应新增履历，履历数 = %d", len(s2.data.Events))
	}

	// 重复周期：两项登记同一到期日，第二项撞上推进后的下一到期日而整批失败。
	dup, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"第一次","segment_seq":%d},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-05","result":"重复周期","segment_seq":%d}
]`, seg2, seg2))
	code, out, errOut = runBatchMaint(t, dir, dup)
	if code != 1 {
		t.Fatalf("重复周期应整批失败，得到 %d", code)
	}
	if !strings.Contains(errOut, "清单第 2 项") || !strings.Contains(errOut, "下一到期日") {
		t.Fatalf("应指出第 2 项周期不接续，得到: %s", errOut)
	}
	if out != "" || !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatalf("整批失败不应输出结果或改动台账，out=%q", out)
	}

	// 修正清单后重试：完成序号从既有最大序号之后连续分配（建立序号 1、2 → 完成 3、4）。
	good, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%d},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"第一次","segment_seq":%d}
]`, seg1, seg2))
	code, out, errOut = runBatchMaint(t, dir, good)
	if code != 0 {
		t.Fatalf("修正后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "EQ-1\t2026-01-01\t3\t2026-01-11") ||
		!strings.Contains(out, "EQ-2\t2026-02-01\t4\t2026-03-03") {
		t.Fatalf("重试应连续分配序号 3、4，得到:\n%s", out)
	}
}

// 业务拒绝：无计划、停用资产、完成日早于到期日、段序号不存在均整批失败，
// 指出项号，台账不变。
func TestBatchMaintainBusinessRejects(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	for _, a := range [][3]string{{"EQ-1", "打印机", "一楼"}, {"EQ-2", "空调", "二楼"}, {"EQ-3", "叉车", "仓库"}, {"EQ-4", "投影仪", "四楼"}} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-02-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-3", "润滑", "2026-03-01", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deactivateAsset("EQ-3", "调拨停用"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	seg1, seg2, seg3 := s.currentSegmentSeq("EQ-1"), s.currentSegmentSeq("EQ-2"), s.currentSegmentSeq("EQ-3")
	before := readFileBytes(t, dir)

	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{"无计划", fmt.Sprintf(`[
  {"asset_id":"EQ-4","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":%d}
]`, seg1), "没有保养计划"},
		{"停用资产", fmt.Sprintf(`[
  {"asset_id":"EQ-3","due":"2026-03-01","done":"2026-03-01","result":"x","segment_seq":%d}
]`, seg3), "已停用"},
		{"完成日早于到期日", fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2025-12-31","result":"x","segment_seq":%d}
]`, seg1), "早于周期到期日"},
		{"到期日不接续", fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-11","done":"2026-01-11","result":"x","segment_seq":%d}
]`, seg1), "下一到期日"},
		{"段序号不属于当前段", fmt.Sprintf(`[
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"x","segment_seq":999999}
]`), "当前方案段"},
		{"越界整批拒绝", fmt.Sprintf(`[
  {"asset_id":"EQ-2","due":"2026-02-01","done":"9999-12-31","result":"x","segment_seq":%d}
]`, seg2), "9999-12-31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, _ := writeMaintManifest(t, tc.manifest)
			code, out, errOut := runBatchMaint(t, dir, manifest)
			if code != 1 {
				t.Fatalf("%s应退出 1，得到 %d: %s", tc.name, code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("%s错误信息应包含 %q，得到: %s", tc.name, tc.want, errOut)
			}
			if out != "" {
				t.Fatalf("%s不应输出部分成功结果: %s", tc.name, out)
			}
			if !bytes.Equal(readFileBytes(t, dir), before) {
				t.Fatalf("%s不应改动台账文件", tc.name)
			}
		})
	}
}

// 容量不足：履历序号耗尽时整批失败，原文件字节保持、不消耗序号。
func TestBatchMaintainEventSeqCapacity(t *testing.T) {
	dir := t.TempDir()
	// baseLedger 中 EQ-1 维修中（T0001），把报修履历序号设为 MaxInt 使序号
	// 耗尽；另加合法保养计划与建立履历（序号间隔允许）。
	raw := writeLedger(t, dir, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
		m["plans"] = []any{map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": "2026-01-01", "interval_days": 10, "next_due": "2026-01-01",
		}}
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
			"due": "2026-01-01", "interval": 10, "time": "2026-10-01T12:00:00Z",
		})
	})
	manifest, _ := writeMaintManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"完成","segment_seq":4}
]`)
	code, out, errOut := runBatchMaint(t, dir, manifest)
	if code != 1 || !strings.Contains(errOut, "履历序号") {
		t.Fatalf("履历序号耗尽应整批失败并说明原因，退出码 = %d: %s", code, errOut)
	}
	if out != "" {
		t.Fatalf("容量不足不应输出成功结果: %s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), raw) {
		t.Fatal("容量不足不应改动台账文件")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("容量不足不应推进日期，得到 %q", got.NextDue)
	}
}

// 保存失败（目录不可写）：整批失败，原文件字节不变，不推进计划、不留履历、
// 不消耗序号；恢复写入条件后重载重试成功。
func TestBatchMaintainSaveFailureReloadRetry(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-02-01", 30); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	seg1, seg2 := s.currentSegmentSeq("EQ-1"), s.currentSegmentSeq("EQ-2")
	before := readFileBytes(t, dir)

	manifest, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%d},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"第一次","segment_seq":%d}
]`, seg1, seg2))
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runBatchMaint(t, dir, manifest)
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
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if len(s2.data.Events) != 2 {
		t.Fatalf("写入失败不应留下履历，履历数 = %d", len(s2.data.Events))
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("写入失败不应推进计划，得到 %q", got.NextDue)
	}
	// 恢复写入条件后用原清单重试成功，序号从 3、4 连续分配。
	code, out, errOut := runBatchMaint(t, dir, manifest)
	if code != 0 {
		t.Fatalf("恢复写入条件后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "EQ-1\t2026-01-01\t3\t2026-01-11") ||
		!strings.Contains(out, "EQ-2\t2026-02-01\t4\t2026-03-03") {
		t.Fatalf("重试应分配序号 3、4，得到:\n%s", out)
	}
}

// 撤销后重登记：unmaintain 撤销批内最新完成后，可用批量入口把恢复的周期
// 登记为新序号；旧序号不能误撤销新完成，原完成与撤销履历全部保留。
func TestBatchMaintainRevokeAndReregister(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	seg := s.currentSegmentSeq("EQ-1")

	manifest, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%d},
  {"asset_id":"EQ-1","due":"2026-01-11","done":"2026-01-12","result":"第二次","segment_seq":%d}
]`, seg, seg))
	code, out, errOut := runBatchMaint(t, dir, manifest)
	if code != 0 {
		t.Fatalf("批量登记应成功: %s", errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	_, _, _, seq1, _ := parseBatchMaintRow(t, lines[2])
	_, _, _, seq2, _ := parseBatchMaintRow(t, lines[3])
	if seq1 != 2 || seq2 != 3 {
		t.Fatalf("完成序号应为 2、3，得到 %d、%d", seq1, seq2)
	}

	// 撤销最新完成 seq2：下一到期日恢复为 2026-01-11。
	var o, e bytes.Buffer
	if code := run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--seq", strconv.Itoa(seq2), "--reason", "误登记"}, &o, &e); code != 0 {
		t.Fatalf("unmaintain 应成功: %s", e.String())
	}
	if !strings.Contains(o.String(), "恢复下一到期日: 2026-01-11") {
		t.Fatalf("撤销后应恢复周期 2026-01-11:\n%s", o.String())
	}

	// 恢复的周期用批量入口重新登记，生成新序号（撤销履历占序号 4，重登记为 5），
	// 旧序号不复用。
	reManifest, _ := writeMaintManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-11","done":"2026-01-13","result":"重新完成","segment_seq":%d}
]`, seg))
	code, out, errOut = runBatchMaint(t, dir, reManifest)
	if code != 0 {
		t.Fatalf("撤销后重登记应成功: %s", errOut)
	}
	if !strings.Contains(out, "EQ-1\t2026-01-11\t5\t2026-01-21") {
		t.Fatalf("重登记应为新序号 5 并推进到 2026-01-21（01-01 + 2*10，严格晚于 01-13）:\n%s", out)
	}

	// 旧序号 seq2 的撤销不能误撤销新完成（seq2 已撤销；新完成序号 5 才是最新有效）。
	o.Reset()
	e.Reset()
	if code := run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--seq", strconv.Itoa(seq2), "--reason", "旧序号再撤"}, &o, &e); code != 1 {
		t.Fatalf("旧序号的再次撤销应退出 1，得到 %d: %s", code, e.String())
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-21" {
		t.Fatalf("旧序号撤销失败不应影响新完成，下一到期日 = %q", got.NextDue)
	}
	// 撤销新完成（序号 5）成功后，下一到期日再次恢复为 2026-01-11，旧完成保留。
	if _, target, err := s2.revokeCompletion("EQ-1", 5, "撤销新完成"); err != nil {
		t.Fatalf("应能撤销新完成序号 5: %v", err)
	} else if target.Due != "2026-01-11" {
		t.Fatalf("撤销新完成应恢复周期 2026-01-11，得到 %s", target.Due)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("撤销新完成后下一到期日 = %q", got.NextDue)
	}
	// 历史保留：三次原完成、两次撤销、建立均在履历中。
	events := s2.eventsOf("EQ-1")
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
	}
	if kinds[eventPlanDone] != 3 || kinds[eventPlanRevoke] != 2 || kinds[eventPlanCreate] != 1 {
		t.Fatalf("履历保留不对: %+v（全部事件 %d 条）", kinds, len(events))
	}
	revoked := s2.revokedDoneSeqs()
	if !revoked[seq2] || !revoked[5] || revoked[seq1] {
		t.Fatalf("撤销状态不对: %v", revoked)
	}
}

// 清单格式错误（退出码 2）：空清单、非法 JSON、非数组、空项、缺项、
// 段序号非正整数或类型错误、非法日期、未知字段、多余内容均指出项号；
// 清单文件读取失败为退出码 1，缺少 --file 为退出码 2。
func TestBatchMaintainManifestFormatErrors(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"空文件", "", "为空"},
		{"仅空白", "  \n\t ", "为空"},
		{"空数组", "[]", "为空"},
		{"非法 JSON", "{", "格式错误"},
		{"非数组", `{"asset_id":"EQ-1"}`, "格式错误"},
		{"多余内容", "[\n] {}", "多余内容"},
		{"空对象项", "[null]", "第 1 项"},
		{"缺资产编号", `[{"due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"空结果", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"","segment_seq":1}]`, "第 1 项"},
		{"缺到期日", `[{"asset_id":"EQ-1","done":"2026-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"缺完成日", `[{"asset_id":"EQ-1","due":"2026-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"缺段序号", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x"}]`, "第 1 项"},
		{"段序号为零", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":0}]`, "第 1 项"},
		{"段序号为负", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":-3}]`, "第 1 项"},
		{"段序号为字符串", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":"3"}]`, "第 1 项"},
		{"段序号为小数", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":3.5}]`, "第 1 项"},
		{"到期日非法", `[{"asset_id":"EQ-1","due":"2026-13-01","done":"2026-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"到期日非零填充", `[{"asset_id":"EQ-1","due":"2026-1-1","done":"2026-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"完成日非法", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"0000-01-01","result":"x","segment_seq":1}]`, "第 1 项"},
		{"未知字段", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":1,"batch":1}]`, "第 1 项"},
		{"第二项段序号为零", `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":1},
  {"asset_id":"EQ-2","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":0}
]`, "第 2 项"},
		{"第二项日期非法", `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"x","segment_seq":1},
  {"asset_id":"EQ-2","due":"2026-02-30","done":"2026-01-01","result":"x","segment_seq":1}
]`, "第 2 项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			manifest, _ := writeMaintManifest(t, tc.content)
			code, _, errOut := runBatchMaint(t, dir, manifest)
			if code != 2 {
				t.Fatalf("清单格式错误应退出 2，得到 %d: %s", code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("错误信息应包含 %q，得到: %s", tc.want, errOut)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("清单格式错误不应初始化数据目录")
			}
		})
	}

	// 清单文件不存在：读取失败，退出码 1。
	dir := t.TempDir()
	code, _, errOut := runBatchMaint(t, dir, filepath.Join(dir, "不存在.json"))
	if code != 1 || !strings.Contains(errOut, "读取保养清单失败") {
		t.Fatalf("清单读取失败应退出 1 并说明原因，退出码 = %d: %s", code, errOut)
	}
	// 缺少 --file 参数：退出码 2。
	var out, errBuf bytes.Buffer
	if code := run([]string{"batch-maintain", "--data-dir", dir}, &out, &errBuf); code != 2 {
		t.Fatalf("缺少 --file 应退出 2，得到 %d", code)
	}
}
