package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// migrateFixture 建立用于迁移测试的源台账：
//   - EQ-A：T0001（报修/派工/领用 P0001/附件 A0001、A0002 同路径/A0002 撤销/
//     搬移/关闭）、保养计划与一次完成、未关闭工单 T0002（附件 A0003）。
//   - EQ-B：未关闭工单 T0003（附件 A0004）。
//   - EQ-C：工单 T0004（不导出）。
//   - EQ-D：无工单与附件的资产。
//
// root 同时容纳源台账目录与原资料目录，测试可整体改名以模拟“源被移走”。
type migrateFixture struct {
	root    string
	srcDir  string
	fileDir string
	files   map[string][]byte // 资料路径 -> 原始内容
}

func newMigrateFixture(t *testing.T) *migrateFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	srcDir := filepath.Join(root, "src")
	fileDir := filepath.Join(root, "资料")
	if err := os.MkdirAll(fileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := newStoreAt(t, srcDir)
	for _, a := range [][3]string{
		{"EQ-A", "打印机", "一楼"}, {"EQ-B", "空调", "二楼"},
		{"EQ-C", "投影仪", "三楼"}, {"EQ-D", "饮水机", "四楼"},
	} {
		if _, err := s.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string) string {
		p := filepath.Join(fileDir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f1 := write("p1.jpg", "资料一内容")
	f3 := write("p3.jpg", "资料三内容")
	f4 := write("p4.jpg", "资料四内容")

	if _, _, err := s.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := s.assignTicket("T0001", "张三", "首次派工"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withdrawPart("T0001", "FILTER-01", 2, "更换滤芯"); err != nil { // P0001
		t.Fatal(err)
	}
	if _, err := s.attach("T0001", f1, "卡纸照片"); err != nil { // A0001
		t.Fatal(err)
	}
	if _, err := s.attach("T0001", f1, "同路径另一角度"); err != nil { // A0002
		t.Fatal(err)
	}
	if _, err := s.revokeAttachment("A0002", "照片拍错设备"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.relocateAsset("EQ-A", "二楼", "移送维修区"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-A", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-A", "2026-11-01", "2026-11-02", "已更换滤芯"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, err := s.attach("T0002", f3, "电源照片"); err != nil { // A0003
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-B", "不制冷", "req-b1"); err != nil { // T0003
		t.Fatal(err)
	}
	if _, err := s.attach("T0003", f4, "管路照片"); err != nil { // A0004
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-C", "漏氟", "req-c1"); err != nil { // T0004，不导出
		t.Fatal(err)
	}
	mustSave(t, s)
	return &migrateFixture{
		root:    root,
		srcDir:  srcDir,
		fileDir: fileDir,
		files: map[string][]byte{
			f1: []byte("资料一内容"),
			f3: []byte("资料三内容"),
			f4: []byte("资料四内容"),
		},
	}
}

// 导出后还原：资料脱离源目录仍可读取；关联路径（含撤销履历）同步替换；
// 编号、计数器、报修地点、位置起点、保养方案段与完成、请求绑定保持；
// 已撤销索引仍撤销；重载、查询、请求重放及后续维修、保养、搬移、编号延续
// 按原规则运行。
func TestMigrateExportRestoreRoundTrip(t *testing.T) {
	fx := newMigrateFixture(t)
	pkg := filepath.Join(t.TempDir(), "eq-a.zip")

	// 重复编号按一项处理；导出前源台账字节留档。
	srcBefore := readFileBytes(t, fx.srcDir)
	out, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B", "EQ-D", "EQ-A"})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(out.assetIDs) != 3 || out.attachments != 4 {
		t.Fatalf("导出摘要不符: %+v", out)
	}
	if string(readFileBytes(t, fx.srcDir)) != string(srcBefore) {
		t.Fatal("导出不应修改源台账")
	}
	pkgBefore, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}

	// 还原前整体移走源根目录（台账与原资料一起消失）：证明还原不访问外部资料。
	moved := fx.root + "-moved"
	if err := os.Rename(fx.root, moved); err != nil {
		t.Fatal(err)
	}
	for p := range fx.files {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("原资料 %s 应已随源目录移走", p)
		}
	}
	target := filepath.Join(t.TempDir(), "restored")
	r, err := restorePackage(pkg, target)
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if r.assets != 3 || r.attachments != 4 {
		t.Fatalf("还原摘要不符: %+v", r)
	}
	if got, _ := os.ReadFile(pkg); string(got) != string(pkgBefore) {
		t.Fatal("还原不应修改迁移包")
	}

	// 目标目录结构：台账 + 资料目录，两项。
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("目标目录应只有台账与资料目录，得到 %v", entries)
	}
	s, err := openStore(target)
	if err != nil {
		t.Fatalf("还原后台账应可加载: %v", err)
	}
	if s.findAsset("EQ-C") != nil || s.findRequest("req-c1") != nil || s.findTicket("T0004") != nil {
		t.Fatal("未选择的 EQ-C/T0004/req-c1 不应进入迁移包")
	}
	if s.data.NextTicketSeq != 5 || s.data.NextPartSeq != 2 || s.data.NextAttachSeq != 5 {
		t.Fatalf("原计数器应保持，得到 ticket=%d part=%d attach=%d",
			s.data.NextTicketSeq, s.data.NextPartSeq, s.data.NextAttachSeq)
	}
	// 位置起点、当前位置与报修地点保持。
	a := s.findAsset("EQ-A")
	if a == nil || a.Location != "二楼" || a.Status != statusRepairing {
		t.Fatalf("EQ-A 当前位置与状态不符: %+v", a)
	}
	if t1 := s.findTicket("T0001"); t1 == nil || t1.ReportLocation != "一楼" ||
		t1.Status != ticketClosed || t1.Result != "已更换搓纸轮" || t1.Assignee != "张三" {
		t.Fatalf("T0001 报修地点与业务内容不符: %+v", t1)
	}
	if t2 := s.findTicket("T0002"); t2 == nil || t2.ReportLocation != "二楼" {
		t.Fatalf("T0002 报修地点应为二楼: %+v", t2)
	}
	// 保养计划与下一到期日保持。
	plan := s.findPlan("EQ-A")
	if plan == nil || plan.Content != "更换滤芯" || plan.IntervalDays != 90 || plan.NextDue != "2027-01-30" {
		t.Fatalf("保养计划与下一到期日应保持，得到 %+v", plan)
	}
	// 全部附件路径都已替换为目标内副本的绝对路径，且内容与原资料一致；
	// 同路径的两条索引各为独立副本；已撤销索引仍撤销且资料仍在。
	rawLedger, err := os.ReadFile(filepath.Join(target, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawLedger), "资料") && strings.Contains(string(rawLedger), fx.fileDir) {
		t.Fatalf("台账中不应残留源资料目录路径 %s", fx.fileDir)
	}
	if strings.Contains(string(rawLedger), fx.fileDir) {
		t.Fatalf("台账中不应残留源资料的绝对路径 %s", fx.fileDir)
	}
	wantContent := map[string]string{
		"A0001": "资料一内容", "A0002": "资料一内容",
		"A0003": "资料三内容", "A0004": "资料四内容",
	}
	paths := map[string]string{}
	for _, id := range []string{"A0001", "A0002", "A0003", "A0004"} {
		att := s.findAttachment(id)
		if att == nil {
			t.Fatalf("附件 %s 应存在", id)
		}
		if filepath.Dir(att.Path) != filepath.Join(target, restoreAttachDir) || !filepath.IsAbs(att.Path) {
			t.Fatalf("附件 %s 路径应为目标内副本绝对路径，得到 %s", id, att.Path)
		}
		got, rerr := os.ReadFile(att.Path)
		if rerr != nil || string(got) != wantContent[id] {
			t.Fatalf("附件 %s 副本内容不符: %v %q", id, rerr, got)
		}
		paths[id] = att.Path
		// 登记与撤销履历中的路径也已同步替换。
		var found int
		for _, e := range s.data.Events {
			if (e.Kind == eventAttach || e.Kind == eventAttachRevoke) && e.AttachmentID == id {
				if e.Path != att.Path {
					t.Fatalf("附件 %s 履历路径 %s 未同步为 %s", id, e.Path, att.Path)
				}
				found++
			}
		}
		if found == 0 {
			t.Fatalf("附件 %s 缺少登记/撤销履历", id)
		}
	}
	if a2 := s.findAttachment("A0002"); a2 == nil || !a2.Revoked || a2.RevokeReason != "照片拍错设备" {
		t.Fatalf("A0002 应保持撤销: %+v", a2)
	}
	if paths["A0001"] == paths["A0002"] {
		t.Fatal("同路径的两条索引在目标内应保持为各自独立的副本")
	}

	// 后续业务按原规则运行：请求重放、撤销保养完成、领用、新登记、搬移、编号延续。
	if tk, replayed, err := s.report("EQ-A", "卡纸", "req-a1"); err != nil || !replayed || tk.ID != "T0001" {
		t.Fatalf("原请求重放应返回 T0001: %v %+v", err, tk)
	}
	var doneSeq int
	for _, e := range s.data.Events {
		if e.Kind == eventPlanDone {
			doneSeq = e.Seq
		}
	}
	if doneSeq == 0 {
		t.Fatal("应能找到保养完成履历")
	}
	if _, _, err := s.revokeCompletion("EQ-A", doneSeq, "误登记"); err != nil {
		t.Fatalf("还原后应可按原序号撤销保养完成: %v", err)
	}
	p, err := s.withdrawPart("T0002", "FILTER-02", 1, "维修领用")
	if err != nil || p.ID != "P0002" {
		t.Fatalf("编号延续应分配 P0002: %v %+v", err, p)
	}
	newFile := filepath.Join(t.TempDir(), "new.jpg")
	if err := os.WriteFile(newFile, []byte("新资料"), 0o644); err != nil {
		t.Fatal(err)
	}
	na, err := s.attach("T0002", newFile, "补充资料")
	if err != nil || na.ID != "A0005" {
		t.Fatalf("编号延续应分配 A0005: %v %+v", err, na)
	}
	if moved, _, err := s.relocateAsset("EQ-A", "三楼", "工位再调整"); err != nil || moved.Location != "三楼" {
		t.Fatalf("还原后搬移应按原规则运行: %v %+v", err, moved)
	}
	mustSave(t, s)
	if _, err := openStore(target); err != nil {
		t.Fatalf("后续业务保存后台账仍须一致: %v", err)
	}
	// ticket 查询：有效附件文件可读，已撤销附件保持撤销显示（源已移走）。
	var buf bytes.Buffer
	if err := cmdTicket([]string{"--ticket-id", "T0002", "--data-dir", target}, &buf); err != nil {
		t.Fatalf("ticket 查询失败: %v", err)
	}
	ticketOut := buf.String()
	if !strings.Contains(ticketOut, "有效（文件可读）") {
		t.Fatalf("移走源目录后有效附件应仍可读: %s", ticketOut)
	}
	if err := cmdHistory([]string{"--asset-id", "EQ-A", "--data-dir", target}, &buf); err != nil {
		t.Fatalf("history 查询失败: %v", err)
	}
}

// 数组乱序、序号间隔、时间不递增的有效旧库同样可以打包，履历顺序、
// 报修地点等在还原后保持。
func TestMigrateExportUnorderedLegacyLedger(t *testing.T) {
	fx := newMigrateFixture(t)
	raw := readFileBytes(t, fx.srcDir)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	evs := m["events"].([]any)
	// 先按 seq 升序以便赋递减时间，再整体随机打乱数组存放。
	sort.Slice(evs, func(i, j int) bool {
		return int(evs[i].(map[string]any)["seq"].(float64)) < int(evs[j].(map[string]any)["seq"].(float64))
	})
	for i, e := range evs {
		e.(map[string]any)["time"] = fmt.Sprintf("2026-12-31T23:59:%02dZ", len(evs)-i)
	}
	r := rand.New(rand.NewSource(42))
	shuffle := func(key string) {
		if v, ok := m[key].([]any); ok {
			r.Shuffle(len(v), func(i, j int) { v[i], v[j] = v[j], v[i] })
		}
	}
	for _, key := range []string{"events", "assets", "tickets", "attachments", "parts"} {
		shuffle(key)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.srcDir, dataFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(fx.srcDir); err != nil {
		t.Fatalf("乱序且时间不递增的旧库应仍为有效台账: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "pkg.zip")
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B"}); err != nil {
		t.Fatalf("乱序旧库应可打包: %v", err)
	}
	target := filepath.Join(t.TempDir(), "out")
	if _, err := restorePackage(pkg, target); err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	s, err := openStore(target)
	if err != nil {
		t.Fatalf("还原后加载: %v", err)
	}
	evs2 := s.eventsOf("EQ-A")
	for i := 1; i < len(evs2); i++ {
		if evs2[i].Seq <= evs2[i-1].Seq {
			t.Fatal("履历应按序号有序展示")
		}
	}
	if t1 := s.findTicket("T0001"); t1 == nil || t1.ReportLocation != "一楼" {
		t.Fatalf("报修地点应保持，得到 %+v", t1)
	}
	// 时间精度（此处为逆序的赋值时间）保持。
	var sawNonMonotonic bool
	for i := 1; i < len(evs2); i++ {
		if evs2[i].Time.Before(evs2[i-1].Time) {
			sawNonMonotonic = true
		}
	}
	if !sawNonMonotonic {
		t.Fatal("时间不递增的旧库时间在还原后应保持原样")
	}
}

// 导出失败路径：未知资产、资料缺失、源台账缺失、包路径已存在；任何失败
// 都不留下成品包或临时文件，源台账字节保持，恢复资料后可用原参数重试。
func TestMigrateExportFailures(t *testing.T) {
	fx := newMigrateFixture(t)
	work := t.TempDir()
	srcBefore := readFileBytes(t, fx.srcDir)

	// 未知资产整次拒绝。
	pkg := filepath.Join(work, "unknown.zip")
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-NOPE"}); err == nil {
		t.Fatal("未知资产应拒绝打包")
	}
	if _, err := os.Lstat(pkg); !os.IsNotExist(err) {
		t.Fatal("失败后不应留下包文件")
	}
	// 空资产列表在命令入口为参数错误（退出 2）。
	var out, errOut bytes.Buffer
	if code := run([]string{"export", "--data-dir", fx.srcDir, "--package", pkg}, &out, &errOut); code != 2 {
		t.Fatalf("缺少 --asset-id 应退出 2，得到 %d", code)
	}

	// 资料缺失：删除 A0003 指向的文件，错误须指出附件编号与原因。
	s, err := openStore(fx.srcDir)
	if err != nil {
		t.Fatal(err)
	}
	missingPath := s.findAttachment("A0003").Path
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}
	pkg = filepath.Join(work, "missing.zip")
	_, err = exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B"})
	if err == nil || !strings.Contains(err.Error(), "A0003") {
		t.Fatalf("资料缺失应拒绝并指出附件编号 A0003，得到 %v", err)
	}
	if _, err := os.Lstat(pkg); !os.IsNotExist(err) {
		t.Fatal("资料缺失不应留下成品包")
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("失败后不应留下临时文件，得到 %v", entries)
	}

	// 资料成为目录：不是可读普通文件，同样整次拒绝并指出附件编号。
	if err := os.WriteFile(missingPath, []byte("资料三内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirPath := s.findAttachment("A0004").Path
	if err := os.Remove(dirPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exportPackage(fx.srcDir, filepath.Join(work, "dir.zip"),
		[]string{"EQ-B"}); err == nil || !strings.Contains(err.Error(), "A0004") {
		t.Fatalf("资料为目录应拒绝并指出附件编号 A0004，得到 %v", err)
	}
	if err := os.Remove(dirPath); err != nil {
		t.Fatal(err)
	}

	// 只导出不引用缺失资料的资产可以成功。
	if _, err := exportPackage(fx.srcDir, filepath.Join(work, "d.zip"), []string{"EQ-D"}); err != nil {
		t.Fatalf("不引用缺失资料的资产应可导出: %v", err)
	}

	// 源台账不存在（数据目录存在但无台账）不能当空库。
	emptyDir := filepath.Join(work, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exportPackage(emptyDir, filepath.Join(work, "e.zip"), []string{"EQ-A"}); err == nil {
		t.Fatal("缺少源台账应拒绝")
	}

	// 恢复缺失资料后用原参数重试成功，证明失败未消耗任何东西。
	if err := os.WriteFile(missingPath, []byte("资料三内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirPath, []byte("资料四内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B"}); err != nil {
		t.Fatalf("恢复资料后应可用原参数重试成功: %v", err)
	}

	// 已有包文件不得覆盖：原包字节保持。
	existing := pkg
	before, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exportPackage(fx.srcDir, existing, []string{"EQ-D"}); err == nil {
		t.Fatal("已有包文件不得覆盖")
	}
	if after, _ := os.ReadFile(existing); string(after) != string(before) {
		t.Fatal("拒绝覆盖时原包文件字节应保持")
	}
	// 源台账字节始终保持。
	if string(readFileBytes(t, fx.srcDir)) != string(srcBefore) {
		t.Fatal("导出失败不应修改源台账字节")
	}
}

// pkgEntry 为测试用 zip 成员描述；mode 非零时可表达目录或符号链接。
type pkgEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func readPkgEntries(t *testing.T, path string) []pkgEntry {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("打开迁移包: %v", err)
	}
	defer zr.Close()
	entries := make([]pkgEntry, 0, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, pkgEntry{name: f.Name, data: data, mode: f.Mode()})
	}
	return entries
}

func writePkgEntries(t *testing.T, path string, entries []pkgEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		fh.SetMode(mode)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func findEntry(entries []pkgEntry, name string) *pkgEntry {
	for i := range entries {
		if entries[i].name == name {
			return &entries[i]
		}
	}
	return nil
}

func withoutEntry(entries []pkgEntry, name string) []pkgEntry {
	out := make([]pkgEntry, 0, len(entries))
	for _, e := range entries {
		if e.name != name {
			out = append(out, e)
		}
	}
	return out
}

// 损坏包与非法成员：内容损坏、缺失/额外/重复成员、越界路径、目录与链接
// 成员、未知格式或版本均拒绝，且不创建目标目录、不在父目录留暂存。
func TestMigrateRestoreRejectsCorruptPackages(t *testing.T) {
	fx := newMigrateFixture(t)
	pkg := filepath.Join(t.TempDir(), "good.zip")
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B"}); err != nil {
		t.Fatal(err)
	}
	good := readPkgEntries(t, pkg)

	setManifestField := func(es []pkgEntry, key string, value any) []pkgEntry {
		e := findEntry(es, migrateManifestName)
		var mm map[string]any
		if err := json.Unmarshal(e.data, &mm); err != nil {
			t.Fatal(err)
		}
		mm[key] = value
		b, err := json.Marshal(mm)
		if err != nil {
			t.Fatal(err)
		}
		e.data = b
		return es
	}

	cases := []struct {
		name    string
		mutate  func(entries []pkgEntry) []pkgEntry
		wantErr string
	}{
		{"台账内容损坏", func(es []pkgEntry) []pkgEntry {
			e := findEntry(es, migrateLedgerName)
			b := append([]byte(nil), e.data...)
			b[len(b)-20] ^= 0xFF
			e.data = b
			return es
		}, "校验值"},
		{"资料内容损坏", func(es []pkgEntry) []pkgEntry {
			e := findEntry(es, "files/f000003")
			e.data = append(append([]byte(nil), e.data...), 'X')
			return es
		}, "内容损坏"},
		{"缺少清单", func(es []pkgEntry) []pkgEntry {
			return withoutEntry(es, migrateManifestName)
		}, "清单"},
		{"缺少资料成员", func(es []pkgEntry) []pkgEntry {
			return withoutEntry(es, "files/f000004")
		}, "成员"},
		{"额外成员", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: "evil.txt", data: []byte("x")})
		}, "额外"},
		{"重复成员", func(es []pkgEntry) []pkgEntry {
			e := *findEntry(es, migrateLedgerName)
			return append(es, e)
		}, "重复"},
		{"越界成员路径", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: "../evil", data: []byte("x")})
		}, "越界"},
		{"嵌套越界成员路径", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: "files/../../evil", data: []byte("x")})
		}, "越界"},
		{"反斜杠成员名", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: `files\f000009`, data: []byte("x")})
		}, "越界"},
		{"目录成员", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: "dir/", mode: os.ModeDir | 0o755})
		}, "普通文件"},
		{"符号链接成员", func(es []pkgEntry) []pkgEntry {
			return append(es, pkgEntry{name: "link", data: []byte("../target"), mode: os.ModeSymlink})
		}, "普通文件"},
		{"未知包版本", func(es []pkgEntry) []pkgEntry {
			return setManifestField(es, "version", 9)
		}, "版本"},
		{"未知包格式", func(es []pkgEntry) []pkgEntry {
			return setManifestField(es, "format", "something-else/1")
		}, "格式"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := make([]pkgEntry, len(good))
			copy(entries, good)
			bad := filepath.Join(t.TempDir(), "bad.zip")
			writePkgEntries(t, bad, tc.mutate(entries))
			parent := t.TempDir()
			target := filepath.Join(parent, "restored")
			_, err := restorePackage(bad, target)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应拒绝（错误含 %q），得到 %v", tc.wantErr, err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("拒绝后不应留下目标目录")
			}
			if le, _ := os.ReadDir(parent); len(le) != 0 {
				t.Fatalf("拒绝后不应留下暂存目录，得到 %v", le)
			}
		})
	}

	// 非 zip 包结构。
	t.Run("非zip内容", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "notzip.zip")
		if err := os.WriteFile(bad, []byte("this is not a zip"), 0o644); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "out")
		if _, err := restorePackage(bad, target); err == nil {
			t.Fatal("非 zip 内容应拒绝")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatal("拒绝后不应留下目标目录")
		}
	})
	t.Run("目标目录已存在", func(t *testing.T) {
		target := t.TempDir()
		if _, err := restorePackage(pkg, target); err == nil {
			t.Fatal("已有目标目录应拒绝")
		}
	})
}

// 保存失败后不留目标目录或暂存，输入包字节保持；恢复条件后可用原包重试。
func TestMigrateRestoreSaveFailureRetry(t *testing.T) {
	fx := newMigrateFixture(t)
	pkg := filepath.Join(t.TempDir(), "pkg.zip")
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B", "EQ-D"}); err != nil {
		t.Fatal(err)
	}
	pkgBefore, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}

	parent := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "restored")
	_, saveErr := restorePackage(pkg, target)
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过保存失败分支")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("保存失败不应留下目标目录")
	}
	if le, _ := os.ReadDir(parent); len(le) != 0 {
		t.Fatalf("保存失败不应留下暂存目录，得到 %v", le)
	}
	if got, _ := os.ReadFile(pkg); string(got) != string(pkgBefore) {
		t.Fatal("保存失败不应修改输入包字节")
	}

	// 恢复条件后用原包重试成功。
	r, err := restorePackage(pkg, target)
	if err != nil {
		t.Fatalf("恢复写入条件后重试应成功: %v", err)
	}
	if r.assets != 3 || r.attachments != 4 {
		t.Fatalf("重试摘要不符: %+v", r)
	}
	if _, err := openStore(target); err != nil {
		t.Fatalf("重试还原后台账应一致: %v", err)
	}
}

// 命令行入口：参数错误退出 2，业务失败退出 1，成功输出数量。
func TestMigrateCommandLine(t *testing.T) {
	fx := newMigrateFixture(t)
	work := t.TempDir()
	pkg := filepath.Join(work, "pkg.zip")
	var out, errOut bytes.Buffer
	runCmd := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := run(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	// 参数错误：缺 --package、缺 --asset-id、restore 缺 --target-dir，退出 2。
	if code, _, _ := runCmd("export", "--data-dir", fx.srcDir, "--asset-id", "EQ-A"); code != 2 {
		t.Fatalf("export 缺 --package 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("export", "--data-dir", fx.srcDir, "--package", pkg); code != 2 {
		t.Fatalf("export 缺 --asset-id 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("restore", "--package", pkg); code != 2 {
		t.Fatalf("restore 缺 --target-dir 应退出 2，得到 %d", code)
	}
	// 业务失败：未知资产导出退出 1；不存在的包还原退出 1。
	if code, _, _ := runCmd("export", "--data-dir", fx.srcDir, "--package", pkg, "--asset-id", "NOPE"); code != 1 {
		t.Fatalf("未知资产导出应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("restore", "--package", filepath.Join(work, "missing.zip"),
		"--target-dir", filepath.Join(work, "t1")); code != 1 {
		t.Fatalf("包不存在还原应退出 1，得到 %d", code)
	}

	// 导出成功并显示资产与附件索引数量。
	code, o, _ := runCmd("export", "--data-dir", fx.srcDir, "--package", pkg,
		"--asset-id", "EQ-A", "--asset-id", "EQ-B")
	if code != 0 || !strings.Contains(o, "所选资产: 2 项") || !strings.Contains(o, "附件索引: 4 条") {
		t.Fatalf("导出应成功并显示数量，得到 %d:\n%s", code, o)
	}
	// 还原成功并显示数量。
	target := filepath.Join(work, "restored")
	code, o, _ = runCmd("restore", "--package", pkg, "--target-dir", target)
	if code != 0 || !strings.Contains(o, "资产: 2 项") || !strings.Contains(o, "附件索引: 4 条") {
		t.Fatalf("还原应成功并显示数量，得到 %d:\n%s", code, o)
	}
	// 目标已存在退出 1。
	if code, _, _ := runCmd("restore", "--package", pkg, "--target-dir", target); code != 1 {
		t.Fatalf("目标已存在应退出 1，得到 %d", code)
	}
	// 已有包文件不得覆盖，退出 1。
	if code, _, _ := runCmd("export", "--data-dir", fx.srcDir, "--package", pkg, "--asset-id", "EQ-D"); code != 1 {
		t.Fatalf("包文件已存在应退出 1，得到 %d", code)
	}
}
