package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBatchMaintain 执行 batch-maintain 命令，返回退出码与输出。
func runBatchMaintain(t *testing.T, dir, manifest string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run([]string{"batch-maintain", "--file", manifest, "--data-dir", dir}, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// 交错接续与延期：两项资产交错出现、同一资产连续多周期登记，后项接续前项推进
// 后的日期；延期完成跳过中间周期不补记录。待验收工单资产仍可登记，工单与资产
// 状态不变。按清单顺序分配全库序号，detail、due 显示最终日期，replay 反映各项
// 进度。
func TestBatchMaintainInterleavedDelayed(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-2", "异响", "req-1"); err != nil { // 履历 1，T0001
		t.Fatal(err)
	}
	if _, _, err := s.submitRepair("T0001", "已临时处理"); err != nil { // 履历 2，待验收
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-10", 10); err != nil { // 履历 3，段 3
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗滤网", "2026-02-01", 30); err != nil { // 履历 4，段 4
		t.Fatal(err)
	}
	mustSave(t, s)

	manifest, manifestBytes := writeManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-10","done":"2026-01-10","result":"按期完成","segment_seq":3},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"维修中照常保养","segment_seq":4},
  {"asset_id":"EQ-1","due":"2026-01-20","done":"2026-02-05","result":"延期完成","segment_seq":3},
  {"asset_id":"EQ-1","due":"2026-02-09","done":"2026-02-09","result":"接续完成","segment_seq":3}
]`)
	code, out, errOut := runBatchMaintain(t, dir, manifest)
	if code != 0 {
		t.Fatalf("批量保养登记应成功，退出码 = %d: %s", code, errOut)
	}
	// 逐项显示项号、资产、周期、完成序号及推进后的日期；序号按清单顺序分配。
	wantRows := []string{
		"1\tEQ-1\t周期 2026-01-10\t完成序号 5\t下一到期日 2026-01-20",
		"2\tEQ-2\t周期 2026-02-01\t完成序号 6\t下一到期日 2026-03-03",
		"3\tEQ-1\t周期 2026-01-20\t完成序号 7\t下一到期日 2026-02-09", // 延期跳过 2026-01-30
		"4\tEQ-1\t周期 2026-02-09\t完成序号 8\t下一到期日 2026-02-19",
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1+len(wantRows) || lines[0] != "批量保养登记完成：共 4 项。" {
		t.Fatalf("输出应为表头加 %d 行，得到:\n%s", len(wantRows), out)
	}
	for i, row := range wantRows {
		if lines[i+1] != row {
			t.Fatalf("第 %d 行 = %q，应为 %q", i+1, lines[i+1], row)
		}
	}
	// 清单文件只读。
	got, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(got, manifestBytes) {
		t.Fatal("批量保养登记不应修改清单文件")
	}

	// 重载核对：每项恰一条普通完成履历（不追加批次事件），计划推进到最终日期；
	// 待验收工单与资产维修状态不变。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Events) != 8 {
		t.Fatalf("不应追加批次事件，履历总数应为 8，得到 %d", len(s2.data.Events))
	}
	if p := s2.findPlan("EQ-1"); p.NextDue != "2026-02-19" {
		t.Fatalf("EQ-1 下一到期日应为 2026-02-19，得到 %s", p.NextDue)
	}
	if p := s2.findPlan("EQ-2"); p.NextDue != "2026-03-03" {
		t.Fatalf("EQ-2 下一到期日应为 2026-03-03，得到 %s", p.NextDue)
	}
	dones := map[int]Event{}
	for _, e := range s2.data.Events {
		if e.Kind == eventPlanDone {
			dones[e.Seq] = e
		}
	}
	if len(dones) != 4 {
		t.Fatalf("应有 4 条完成履历，得到 %d", len(dones))
	}
	if d := dones[7]; d.AssetID != "EQ-1" || d.Due != "2026-01-20" || d.Done != "2026-02-05" || d.Content != "延期完成" {
		t.Fatalf("延期完成履历内容不符: %+v", d)
	}
	if a := s2.findAsset("EQ-2"); a.Status != statusRepairing {
		t.Fatalf("登记不应改变资产状态，EQ-2 应为维修中，得到 %s", a.Status)
	}
	if tk := s2.findTicket("T0001"); tk.Status != ticketPending {
		t.Fatalf("登记不应改变工单状态，T0001 应为待验收，得到 %s", tk.Status)
	}

	// detail 显示最终日期；due 按最终日期参与查询。
	var o, e bytes.Buffer
	if code := run([]string{"detail", "--data-dir", dir, "--asset-id", "EQ-1"}, &o, &e); code != 0 {
		t.Fatalf("detail 应成功: %s", e.String())
	}
	if !strings.Contains(o.String(), "下一到期日: 2026-02-19") {
		t.Fatalf("detail 应显示最终下一到期日，得到:\n%s", o.String())
	}
	o.Reset()
	e.Reset()
	if code := run([]string{"due", "--data-dir", dir, "--date", "2026-02-19"}, &o, &e); code != 0 {
		t.Fatalf("due 应成功: %s", e.String())
	}
	if !strings.Contains(o.String(), "EQ-1") || strings.Contains(o.String(), "EQ-2") {
		t.Fatalf("due 2026-02-19 应只列出 EQ-1，得到:\n%s", o.String())
	}

	// replay 反映各项进度：截止 5 只有第一项，截止 7 含延期完成。
	for cutoff, want := range map[int]string{5: "2026-01-20", 7: "2026-02-09", 8: "2026-02-19"} {
		o.Reset()
		e.Reset()
		if code := run([]string{"replay", "--data-dir", dir, "--asset-id", "EQ-1",
			"--seq", fmt.Sprint(cutoff)}, &o, &e); code != 0 {
			t.Fatalf("replay --seq %d 应成功: %s", cutoff, e.String())
		}
		if !strings.Contains(o.String(), "下一到期日: "+want) {
			t.Fatalf("replay --seq %d 下一到期日应为 %s，得到:\n%s", cutoff, want, o.String())
		}
	}
}

// 同日期的旧段也拒绝：调整后新段首次到期日与旧段下一到期日相同，清单引用旧段
// 序号（或不存在的段序号）仍整批拒绝；引用当前段序号才接受。
func TestBatchMaintainOldSegmentSameDate(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "按期完成"); err != nil { // 履历 2
		t.Fatal(err)
	}
	// 调整开启新段（履历 3）：新首次到期日 2026-01-31 恰与旧段推进后的下一到期日相同。
	if _, _, err := s.adjustPlan("EQ-1", "更换高效滤芯", "2026-01-31", 30, "滤芯升级"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	for _, seg := range []int{1, 99} {
		manifest, _ := writeManifest(t, fmt.Sprintf(
			`[{"asset_id":"EQ-1","due":"2026-01-31","done":"2026-01-31","result":"新段首保","segment_seq":%d}]`, seg))
		code, out, errOut := runBatchMaintain(t, dir, manifest)
		if code != 1 {
			t.Fatalf("段序号 %d 应整批拒绝（退出 1），得到 %d（输出 %s）", seg, code, out)
		}
		if !strings.Contains(errOut, "清单第 1 项") || !strings.Contains(errOut, "方案段") {
			t.Fatalf("应指出项号与方案段不符，得到: %s", errOut)
		}
		if out != "" {
			t.Fatalf("整批失败不应输出部分成功结果，得到: %s", out)
		}
		if !bytes.Equal(readFileBytes(t, dir), before) {
			t.Fatal("整批失败不应改动台账文件")
		}
	}

	// 引用当前段序号（调整履历 3）：同一到期日正常登记。
	manifest, _ := writeManifest(t,
		`[{"asset_id":"EQ-1","due":"2026-01-31","done":"2026-01-31","result":"新段首保","segment_seq":3}]`)
	code, out, errOut := runBatchMaintain(t, dir, manifest)
	if code != 0 {
		t.Fatalf("当前段登记应成功: %s", errOut)
	}
	if !strings.Contains(out, "完成序号 4\t下一到期日 2026-03-02") {
		t.Fatalf("应按新段方案推进到 2026-03-02，得到:\n%s", out)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := s2.findPlan("EQ-1"); p.NextDue != "2026-03-02" {
		t.Fatalf("下一到期日应为 2026-03-02，得到 %s", p.NextDue)
	}
}

// 末项失败：最后一项到期日不接续时整批失败，台账字节不变、不留履历、不消耗
// 序号；修正清单后重试成功，序号从原位置连续分配。
func TestBatchMaintainLastItemFails(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗滤网", "2026-02-01", 30); err != nil { // 履历 2，段 2
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"按期完成","segment_seq":1},
  {"asset_id":"EQ-2","due":"2026-02-02","done":"2026-02-02","result":"日期填错","segment_seq":2}
]`)
	code, out, errOut := runBatchMaintain(t, dir, manifest)
	if code != 1 {
		t.Fatalf("末项到期日不接续应整批失败（退出 1），得到 %d（输出 %s）", code, out)
	}
	if !strings.Contains(errOut, "清单第 2 项") || !strings.Contains(errOut, "2026-02-01") {
		t.Fatalf("应指出失败项位置与当前下一到期日，得到: %s", errOut)
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
	if len(s2.data.Events) != 2 {
		t.Fatalf("末项失败不应留下部分履历，履历总数应为 2，得到 %d", len(s2.data.Events))
	}
	if p := s2.findPlan("EQ-1"); p.NextDue != "2026-01-01" {
		t.Fatalf("末项失败不应推进计划，EQ-1 下一到期日应为 2026-01-01，得到 %s", p.NextDue)
	}

	// 修正清单后重试：序号从 3 连续分配。
	manifest2, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"按期完成","segment_seq":1},
  {"asset_id":"EQ-2","due":"2026-02-01","done":"2026-02-01","result":"按期完成","segment_seq":2}
]`)
	code, out, errOut = runBatchMaintain(t, dir, manifest2)
	if code != 0 {
		t.Fatalf("修正后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "完成序号 3") || !strings.Contains(out, "完成序号 4") {
		t.Fatalf("重试应从序号 3 连续分配，得到:\n%s", out)
	}
}

// 容量边界：剩余履历序号不够本批项数时整批失败，指出容量不足，台账字节不变、
// 不留履历；恢复容量后可正常登记。
func TestBatchMaintainCapacityFailure(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
		t.Fatal(err)
	}
	mustSave(t, s)

	// 把建立履历序号改为 math.MaxInt-1：只剩一个可用序号，两项清单整批失败。
	bump := func(seq int) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m["events"].([]any)[0].(map[string]any)["seq"] = seq
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dataFileName), out, 0o644); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := bump(math.MaxInt - 1)
	manifest, _ := writeManifest(t, fmt.Sprintf(`[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%[1]d},
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-01-31","result":"第二次","segment_seq":%[1]d}
]`, math.MaxInt-1))
	code, out, errOut := runBatchMaintain(t, dir, manifest)
	if code != 1 || !strings.Contains(errOut, "容量不足") {
		t.Fatalf("履历序号容量不足应整批失败并说明原因，退出码 = %d: %s", code, errOut)
	}
	if out != "" {
		t.Fatalf("容量不足不应输出部分成功结果，得到: %s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("容量不足不应改动台账文件")
	}

	// 序号完全耗尽：单项清单同样整批失败。
	before = bump(math.MaxInt)
	manifest1, _ := writeManifest(t, fmt.Sprintf(
		`[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":%d}]`, math.MaxInt))
	code, _, errOut = runBatchMaintain(t, dir, manifest1)
	if code != 1 || !strings.Contains(errOut, "容量不足") {
		t.Fatalf("履历序号耗尽应整批失败并说明原因，退出码 = %d: %s", code, errOut)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("履历序号耗尽不应改动台账文件")
	}

	// 恢复容量后同一清单可正常登记。
	bump(1)
	manifestOK, _ := writeManifest(t,
		`[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":1}]`)
	code, out, errOut = runBatchMaintain(t, dir, manifestOK)
	if code != 0 {
		t.Fatalf("恢复容量后应可正常登记: %s", errOut)
	}
	if !strings.Contains(out, "完成序号 2") {
		t.Fatalf("应从序号 2 分配，得到:\n%s", out)
	}
}

// 保存失败（目录不可写）：整批失败，原文件字节不变，不推进计划、不留履历、
// 不消耗序号；恢复写入条件后重载，可用原清单重试成功。
func TestBatchMaintainSaveFailureReloadRetry(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"第一次","segment_seq":1},
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-01-31","result":"第二次","segment_seq":1}
]`)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runBatchMaintain(t, dir, manifest)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	if code != 1 {
		t.Fatalf("写入失败应退出 1，得到 %d", code)
	}
	if out != "" {
		t.Fatalf("写入失败不应输出部分成功结果，得到: %s", out)
	}
	if !bytes.Equal(readFileBytes(t, dir), before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	// 重载后看不到部分变化：无完成履历，计划未推进。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if len(s2.data.Events) != 1 {
		t.Fatalf("写入失败不应留下部分履历，履历总数应为 1，得到 %d", len(s2.data.Events))
	}
	if p := s2.findPlan("EQ-1"); p.NextDue != "2026-01-01" {
		t.Fatalf("写入失败不应推进计划，下一到期日应为 2026-01-01，得到 %s", p.NextDue)
	}
	// 恢复条件后可用原清单重试成功，序号从 2 连续分配。
	code, out, errOut := runBatchMaintain(t, dir, manifest)
	if code != 0 {
		t.Fatalf("恢复写入条件后重试应成功: %s", errOut)
	}
	if !strings.Contains(out, "完成序号 2") || !strings.Contains(out, "完成序号 3") {
		t.Fatalf("重试应从序号 2 连续分配，得到:\n%s", out)
	}
}

// 撤销重登记：批量登记的完成可按 unmaintain 撤销，恢复的周期可再登记为新序号；
// 旧序号的再次撤销仍被拒绝，不会误撤销新登记，历史保留。
func TestBatchMaintainRevokeAndReregister(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
		t.Fatal(err)
	}
	mustSave(t, s)

	manifest, _ := writeManifest(t, `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-02","result":"第一次","segment_seq":1},
  {"asset_id":"EQ-1","due":"2026-01-31","done":"2026-02-01","result":"第二次","segment_seq":1}
]`)
	code, _, errOut := runBatchMaintain(t, dir, manifest)
	if code != 0 {
		t.Fatalf("批量登记应成功: %s", errOut)
	}

	// 撤销最新完成（序号 3）：下一到期日恢复为 2026-01-31。
	var o, e bytes.Buffer
	if code := run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--seq", "3", "--reason", "误登记"}, &o, &e); code != 0 {
		t.Fatalf("撤销应成功: %s", e.String())
	}
	if !strings.Contains(o.String(), "恢复下一到期日: 2026-01-31") {
		t.Fatalf("撤销后应恢复下一到期日 2026-01-31，得到:\n%s", o.String())
	}

	// 恢复的周期按原规则重新登记：生成新序号 5（撤销履历为 4）。
	manifest2, _ := writeManifest(t,
		`[{"asset_id":"EQ-1","due":"2026-01-31","done":"2026-02-05","result":"重新登记","segment_seq":1}]`)
	code, out, errOut := runBatchMaintain(t, dir, manifest2)
	if code != 0 {
		t.Fatalf("恢复周期重新登记应成功: %s", errOut)
	}
	if !strings.Contains(out, "完成序号 5\t下一到期日 2026-03-02") {
		t.Fatalf("重新登记应生成新序号 5 并推进到 2026-03-02，得到:\n%s", out)
	}

	// 旧序号 3 已撤销：再次撤销被拒绝，不会误撤销新登记（序号 5）。
	o.Reset()
	e.Reset()
	code = run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--seq", "3", "--reason", "再次撤销"}, &o, &e)
	if code != 1 || !strings.Contains(e.String(), "已撤销") {
		t.Fatalf("重复撤销旧序号应被拒绝（退出 1），退出码 = %d: %s", code, e.String())
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := s2.findPlan("EQ-1"); p.NextDue != "2026-03-02" {
		t.Fatalf("重复撤销被拒绝不应影响新登记，下一到期日应为 2026-03-02，得到 %s", p.NextDue)
	}

	// 历史保留：两次完成与撤销均可查，各自标注有效或已撤销状态。
	o.Reset()
	e.Reset()
	if code := run([]string{"history", "--data-dir", dir, "--asset-id", "EQ-1"}, &o, &e); code != 0 {
		t.Fatalf("history 应成功: %s", e.String())
	}
	hist := o.String()
	for _, want := range []string{"序号 3，已撤销", "序号 5，有效", "目标完成履历序号 3"} {
		if !strings.Contains(hist, want) {
			t.Fatalf("history 应包含 %q，得到:\n%s", want, hist)
		}
	}
}

// 业务拒绝：未知资产、无计划、停用资产、完成日早于到期日、同一周期重复出现
// 均整批失败（退出码 1），指出项号，台账字节不变。
func TestBatchMaintainBusinessRejections(t *testing.T) {
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		s := newStoreAt(t, dir)
		if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 30); err != nil { // 履历 1，段 1
			t.Fatal(err)
		}
		mustSave(t, s)
		return dir
	}
	item := `{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"完成","segment_seq":1}`
	cases := []struct {
		name     string
		manifest string
		mutate   func(t *testing.T, dir string) // 在 setup 之后追加的台账准备
		want     string
	}{
		{"未知资产", `[{"asset_id":"EQ-9","due":"2026-01-01","done":"2026-01-01","result":"完成","segment_seq":1}]`,
			nil, "未知资产"},
		{"无计划", `[{"asset_id":"EQ-2","due":"2026-01-01","done":"2026-01-01","result":"完成","segment_seq":1}]`,
			nil, "没有保养计划"},
		{"停用资产", `[` + item + `]`,
			func(t *testing.T, dir string) {
				s := newStoreAt(t, dir)
				if _, err := s.deactivateAsset("EQ-1", "调拨停用"); err != nil {
					t.Fatal(err)
				}
				mustSave(t, s)
			}, "已停用"},
		{"完成日早于到期日", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2025-12-31","result":"完成","segment_seq":1}]`,
			nil, "早于"},
		{"重复周期", `[` + item + `,` + item + `]`,
			nil, "清单第 2 项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := setup(t)
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			before := readFileBytes(t, dir)
			manifest, _ := writeManifest(t, tc.manifest)
			code, out, errOut := runBatchMaintain(t, dir, manifest)
			if code != 1 {
				t.Fatalf("应整批失败（退出 1），得到 %d（输出 %s）", code, out)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("错误信息应包含 %q，得到: %s", tc.want, errOut)
			}
			if out != "" {
				t.Fatalf("整批失败不应输出部分成功结果，得到: %s", out)
			}
			if !bytes.Equal(readFileBytes(t, dir), before) {
				t.Fatal("整批失败不应改动台账文件")
			}
		})
	}
}

// 清单格式错误（退出码 2）：空清单、格式错误、缺少必填内容、非正整数段序号、
// 非法日期、未知字段、多余内容均整批拒绝并指出位置；清单文件读取失败为退出码 1。
func TestBatchMaintainManifestFormatErrors(t *testing.T) {
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
		{"未知字段", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":1,"batch_id":"b1"}]`, "格式错误"},
		{"缺资产编号", `[{"due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":1}]`, "清单第 1 项"},
		{"空保养结果", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"","segment_seq":1}]`, "清单第 1 项"},
		{"缺段序号", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好"}]`, "清单第 1 项"},
		{"段序号为零", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":0}]`, "正整数"},
		{"段序号为负", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":-2}]`, "正整数"},
		{"段序号非整数", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":1.5}]`, "格式错误"},
		{"非法到期日", `[{"asset_id":"EQ-1","due":"2026-13-01","done":"2026-01-01","result":"好","segment_seq":1}]`, "清单第 1 项"},
		{"非法完成日", `[{"asset_id":"EQ-1","due":"2026-01-01","done":"2026-02-30","result":"好","segment_seq":1}]`, "清单第 1 项"},
		{"第二项缺内容", `[
  {"asset_id":"EQ-1","due":"2026-01-01","done":"2026-01-01","result":"好","segment_seq":1},
  {"asset_id":"EQ-2","due":"2026-01-01","done":"2026-01-01","segment_seq":2}
]`, "清单第 2 项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			manifest, _ := writeManifest(t, tc.content)
			code, _, errOut := runBatchMaintain(t, dir, manifest)
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
	code, _, errOut := runBatchMaintain(t, dir, filepath.Join(dir, "不存在.json"))
	if code != 1 || !strings.Contains(errOut, "读取保养清单失败") {
		t.Fatalf("清单读取失败应退出 1 并说明原因，退出码 = %d: %s", code, errOut)
	}
	// 缺少 --file 参数：退出码 2。
	var out, errBuf bytes.Buffer
	if code := run([]string{"batch-maintain", "--data-dir", dir}, &out, &errBuf); code != 2 {
		t.Fatalf("缺少 --file 应退出 2，得到 %d", code)
	}
}
