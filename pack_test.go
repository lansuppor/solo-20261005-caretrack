package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildPackSource 构造带附件资料的源台账：EQ-A 一张已关闭工单（含同路径两条
// 附件索引，其中一条已撤销，以及备件领用）加一项保养计划；EQ-B 一张未关闭
// 工单（含一条附件索引）；EQ-C 不参与导出。
func buildPackSource(t *testing.T) (srcDir, matDir, photo1, photo2 string) {
	t.Helper()
	base := t.TempDir()
	srcDir = filepath.Join(base, "src")
	matDir = filepath.Join(base, "mats")
	if err := os.MkdirAll(matDir, 0o755); err != nil {
		t.Fatal(err)
	}
	photo1 = filepath.Join(matDir, "photo1.bin")
	photo2 = filepath.Join(matDir, "photo2.bin")
	if err := os.WriteFile(photo1, []byte("photo-one-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(photo2, []byte("photo-two-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := newStoreAt(t, srcDir)
	for _, a := range [][3]string{
		{"EQ-A", "打印机", "一楼"}, {"EQ-B", "空调", "二楼"}, {"EQ-C", "投影仪", "三楼"},
	} {
		if _, err := src.registerAsset(a[0], a[1], a[2]); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := src.attach("T0001", photo1, "卡纸照片"); err != nil { // A0001
		t.Fatal(err)
	}
	if _, err := src.attach("T0001", photo1, "同路径另一索引"); err != nil { // A0002，同路径
		t.Fatal(err)
	}
	if _, err := src.revokeAttachment("A0002", "照片拍错设备"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.withdrawPart("T0001", "FILTER-01", 2, "更换滤芯"); err != nil { // P0001
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-A", "更换滤芯", "2026-11-01", 90); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-B", "异响", "req-b1"); err != nil { // T0002，未关闭
		t.Fatal(err)
	}
	if _, err := src.attach("T0002", photo2, "异响录音"); err != nil { // A0003
		t.Fatal(err)
	}
	mustSave(t, src)
	return srcDir, matDir, photo1, photo2
}

// 还原后资料脱离源目录：移走源台账与原资料，有效附件仍可从目标内副本读取，
// 已撤销索引保持撤销；同路径索引各自独立；计数器、请求重放与后续业务按原规则运行。
func TestRestoreDetachesFromSource(t *testing.T) {
	srcDir, matDir, _, _ := buildPackSource(t)
	srcBefore := readFileBytes(t, srcDir)

	pkg := filepath.Join(t.TempDir(), "pack.tar")
	outcome, err := exportAssets(srcDir, []string{"EQ-A", "EQ-B", "EQ-A"}, pkg)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(outcome.assetIDs) != 2 || outcome.assetIDs[0] != "EQ-A" || outcome.assetIDs[1] != "EQ-B" {
		t.Fatalf("重复编号应按一项处理并保持首次顺序，得到 %v", outcome.assetIDs)
	}
	if outcome.attachments != 3 {
		t.Fatalf("附件索引数 = %d, 想得到 3", outcome.attachments)
	}
	// 源台账与原资料只读。
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导出不应修改源台账")
	}

	dst := filepath.Join(t.TempDir(), "dst")
	rout, err := restorePackage(pkg, dst)
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if len(rout.assetIDs) != 2 || rout.attachments != 3 {
		t.Fatalf("还原摘要 = %+v, 想得到 2 项资产、3 条附件索引", rout)
	}

	// 移走源目录与原资料：还原后的台账与附件不再依赖它们。
	if err := os.RemoveAll(srcDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(matDir); err != nil {
		t.Fatal(err)
	}

	s, err := openStore(dst)
	if err != nil {
		t.Fatalf("重载还原台账: %v", err)
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		t.Fatal(err)
	}
	// 各类编号与计数器保持原值。
	if s.data.NextTicketSeq != 3 || s.data.NextPartSeq != 2 || s.data.NextAttachSeq != 4 {
		t.Fatalf("计数器应保持原值，得到 T=%d P=%d A=%d",
			s.data.NextTicketSeq, s.data.NextPartSeq, s.data.NextAttachSeq)
	}
	atts := s.attachmentsOf("T0001")
	if len(atts) != 2 || atts[0].ID != "A0001" || atts[1].ID != "A0002" {
		t.Fatalf("T0001 附件应为 A0001、A0002，得到 %+v", atts)
	}
	// 同路径的两条索引在目标内各自独立成副本。
	for _, a := range atts {
		want := filepath.Join(dstAbs, "materials", a.ID)
		if a.Path != want {
			t.Fatalf("附件 %s 路径应为 %s，得到 %s", a.ID, want, a.Path)
		}
		content, err := os.ReadFile(a.Path)
		if err != nil || string(content) != "photo-one-content" {
			t.Fatalf("附件 %s 副本内容应为原资料内容: %v %q", a.ID, err, content)
		}
	}
	if atts[0].Revoked {
		t.Fatal("A0001 应保持有效")
	}
	if !atts[1].Revoked || atts[1].RevokeReason != "照片拍错设备" {
		t.Fatalf("A0002 应保持已撤销及理由，得到 %+v", atts[1])
	}
	// 有效附件在源资料移除后仍可读；已撤销索引仍保持撤销。
	if !attachmentReadable(atts[0].Path) {
		t.Fatal("移走源资料后 A0001 副本仍应可读")
	}
	a3 := s.findAttachment("A0003")
	if a3 == nil || a3.Revoked || !attachmentReadable(a3.Path) {
		t.Fatalf("A0003 应存在、有效且可读，得到 %+v", a3)
	}
	// 请求重放返回原工单及当前状态，不新增记录。
	before := len(s.data.Tickets)
	tk, replay, err := s.report("EQ-A", "卡纸", "req-a1")
	if err != nil || !replay || tk.ID != "T0001" || tk.Status != ticketClosed {
		t.Fatalf("重放应返回已关闭的 T0001: %v replay=%v err=%v", tk, replay, err)
	}
	if len(s.data.Tickets) != before {
		t.Fatal("重放不应创建记录")
	}
	// 后续维修、保养、搬移与编号延续按原规则运行。
	s2, err := openStore(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.relocateAsset("EQ-A", "三楼维修间", "工位调整"); err != nil {
		t.Fatalf("还原后应可搬移: %v", err)
	}
	if _, _, _, err := s2.completePlan("EQ-A", "2026-11-01", "2026-11-02", "已更换滤芯"); err != nil {
		t.Fatalf("还原后应可登记保养完成: %v", err)
	}
	nk, replay, err := s2.report("EQ-A", "无法开机", "req-a2")
	if err != nil || replay || nk.ID != "T0003" {
		t.Fatalf("新工单应延续编号为 T0003: %v replay=%v err=%v", nk, replay, err)
	}
	newMat := filepath.Join(t.TempDir(), "new.bin")
	if err := os.WriteFile(newMat, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	na, err := s2.attach("T0003", newMat, "新资料")
	if err != nil || na.ID != "A0004" {
		t.Fatalf("新附件应延续编号为 A0004: %v err=%v", na, err)
	}
	mustSave(t, s2)
	if _, err := openStore(dst); err != nil {
		t.Fatalf("后续操作后的台账应保持一致: %v", err)
	}
}

// 关联路径同步：附件记录及对应登记、撤销履历中的路径同步替换为目标内副本的
// 绝对路径；其余身份与业务含义（说明、理由、时间、顺序）保持，不新增业务履历。
func TestRestoreRewritesAttachmentPaths(t *testing.T) {
	srcDir, _, photo1, _ := buildPackSource(t)
	src, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	srcEvents := src.eventsOf("EQ-A")

	pkg := filepath.Join(t.TempDir(), "pack.tar")
	if _, err := exportAssets(srcDir, []string{"EQ-A"}, pkg); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	// 包内台账保留原绝对路径（业务内容原样打包）。
	pkgRaw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkgRaw), photo1) {
		t.Fatal("包内台账应保留附件原绝对路径")
	}

	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := restorePackage(pkg, dst); err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	dstAbs, _ := filepath.Abs(dst)
	s, err := openStore(dst)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := map[string]string{}
	for _, id := range []string{"A0001", "A0002"} {
		wantPath[id] = filepath.Join(dstAbs, "materials", id)
	}
	for _, a := range s.data.Attachments {
		if a.Path != wantPath[a.ID] {
			t.Fatalf("附件记录 %s 路径应替换为 %s，得到 %s", a.ID, wantPath[a.ID], a.Path)
		}
	}
	// 登记与撤销履历中的路径同步替换为副本路径。
	seenAttach, seenRevoke := false, false
	for _, e := range s.data.Events {
		if e.Kind == eventAttach {
			seenAttach = true
			if e.Path != wantPath[e.AttachmentID] {
				t.Fatalf("登记履历（序号 %d）路径应为 %s，得到 %s", e.Seq, wantPath[e.AttachmentID], e.Path)
			}
		}
		if e.Kind == eventAttachRevoke {
			seenRevoke = true
			if e.Path != wantPath[e.AttachmentID] {
				t.Fatalf("撤销履历（序号 %d）路径应为 %s，得到 %s", e.Seq, wantPath[e.AttachmentID], e.Path)
			}
		}
	}
	if !seenAttach || !seenRevoke {
		t.Fatal("还原后应保留附件登记与撤销履历")
	}
	// 履历数量、序号与时间与源一致：不新增业务履历，时间精度保持。
	dstEvents := s.eventsOf("EQ-A")
	if len(dstEvents) != len(srcEvents) {
		t.Fatalf("履历数 = %d, 源为 %d（不应新增业务履历）", len(dstEvents), len(srcEvents))
	}
	for i := range srcEvents {
		if dstEvents[i].Seq != srcEvents[i].Seq || !dstEvents[i].Time.Equal(srcEvents[i].Time) ||
			dstEvents[i].Kind != srcEvents[i].Kind {
			t.Fatalf("履历[%d] 应保持序号、时间与类型：源 %+v，目标 %+v",
				i, srcEvents[i], dstEvents[i])
		}
	}
	// 说明与理由等业务内容保持。
	a1, a2 := s.findAttachment("A0001"), s.findAttachment("A0002")
	if a1.Note != "卡纸照片" || a2.Note != "同路径另一索引" || a2.RevokeReason != "照片拍错设备" {
		t.Fatalf("附件说明与撤销理由应保持，得到 %+v / %+v", a1, a2)
	}
}

// 损坏包：台账或资料内容被篡改、未知包版本均拒绝；不留下目标目录，输入包字节保持。
func TestRestoreRejectsCorruptedPackage(t *testing.T) {
	srcDir, _, _, _ := buildPackSource(t)
	pkg := filepath.Join(t.TempDir(), "pack.tar")
	if _, err := exportAssets(srcDir, []string{"EQ-A"}, pkg); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := func(needle string) []byte {
		b := append([]byte(nil), raw...)
		i := bytes.Index(b, []byte(needle))
		if i < 0 {
			t.Fatalf("包内找不到片段 %q", needle)
		}
		b[i] ^= 0xFF
		return b
	}
	writePkg := func(content []byte) string {
		p := filepath.Join(t.TempDir(), "bad.tar")
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name    string
		content []byte
		want    string
	}{
		{"台账内容损坏", corrupt("卡纸"), "校验"},
		{"资料内容损坏", corrupt("photo-one-content"), "校验"},
		{"未知包版本", bytes.Replace(raw, []byte(`"version": 1`), []byte(`"version": 9`), 1), "未知的迁移包版本"},
		{"非包文件", []byte("not a tar at all"), "损坏"},
	}
	for _, c := range cases {
		bad := writePkg(c.content)
		dst := filepath.Join(t.TempDir(), "dst")
		if _, err := restorePackage(bad, dst); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：应拒绝并说明 %q，得到 %v", c.name, c.want, err)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s：失败后不应留下目标目录", c.name)
		}
		got, err := os.ReadFile(bad)
		if err != nil || !bytes.Equal(got, c.content) {
			t.Fatalf("%s：输入包字节应保持不变", c.name)
		}
	}
	// 原包未受影响，仍可正常还原。
	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := restorePackage(pkg, dst); err != nil {
		t.Fatalf("原包应仍可还原: %v", err)
	}
}

// 越界成员路径、链接成员、缺失、额外或重复成员均拒绝，不留下目标目录。
func TestRestoreRejectsBadMembers(t *testing.T) {
	srcDir, _, _, _ := buildPackSource(t)
	pkg := filepath.Join(t.TempDir(), "pack.tar")
	if _, err := exportAssets(srcDir, []string{"EQ-B"}, pkg); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	raw, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	members := readTarMembers(t, raw)

	repack := func(mut func(m map[string][]byte)) []byte {
		m := make(map[string][]byte, len(members))
		for k, v := range members {
			m[k] = v
		}
		if mut != nil {
			mut(m)
		}
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, n := range names {
			if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(m[n])), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(m[n]); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"越界成员路径", repack(func(m map[string][]byte) { m["../evil"] = []byte("x") }), "越界"},
		{"绝对路径成员", repack(func(m map[string][]byte) { m["/etc/evil"] = []byte("x") }), "越界"},
		{"额外成员", repack(func(m map[string][]byte) { m["extra.txt"] = []byte("x") }), "额外成员"},
		{"缺失资料成员", repack(func(m map[string][]byte) { delete(m, "materials/A0003") }), "缺少"},
		{"缺失清单", repack(func(m map[string][]byte) { delete(m, "manifest.json") }), "缺少清单"},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "bad.tar")
		if err := os.WriteFile(p, c.raw, 0o644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "dst")
		if _, err := restorePackage(p, dst); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：应拒绝并说明 %q，得到 %v", c.name, c.want, err)
		}
		if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s：失败后不应留下目标目录", c.name)
		}
	}

	// 重复成员。
	var dup bytes.Buffer
	tw := tar.NewWriter(&dup)
	for i := 0; i < 2; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(members["manifest.json"])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(members["manifest.json"]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "dup.tar")
	if err := os.WriteFile(p, dup.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := restorePackage(p, dst); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复成员应拒绝，得到 %v", err)
	}

	// 链接成员。
	var link bytes.Buffer
	tw = tar.NewWriter(&link)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(t.TempDir(), "link.tar")
	if err := os.WriteFile(p2, link.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst2 := filepath.Join(t.TempDir(), "dst")
	if _, err := restorePackage(p2, dst2); err == nil || !strings.Contains(err.Error(), "不是普通文件") {
		t.Fatalf("链接成员应拒绝，得到 %v", err)
	}
}

// 保存失败后重试：目标不可写时失败且不留下目标目录、输入包字节保持；
// 恢复条件后可用原包重试成功。
func TestRestoreRetryAfterSaveFailure(t *testing.T) {
	srcDir, _, _, _ := buildPackSource(t)
	pkg := filepath.Join(t.TempDir(), "pack.tar")
	if _, err := exportAssets(srcDir, []string{"EQ-A"}, pkg); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	pkgBefore, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	dst := filepath.Join(parent, "dst")
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	_, err = restorePackage(pkg, dst)
	if cerr := os.Chmod(parent, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
		t.Fatal("写入失败不应留下目标目录或部分资料")
	}
	got, err := os.ReadFile(pkg)
	if err != nil || !bytes.Equal(got, pkgBefore) {
		t.Fatal("失败后输入包字节应保持不变")
	}
	// 恢复条件后可用原包重试。
	outcome, err := restorePackage(pkg, dst)
	if err != nil {
		t.Fatalf("恢复条件后重试应成功: %v", err)
	}
	if len(outcome.assetIDs) != 1 || outcome.attachments != 2 {
		t.Fatalf("重试摘要 = %+v, 想得到 1 项资产、2 条附件索引", outcome)
	}
	s, err := openStore(dst)
	if err != nil || s.findAsset("EQ-A") == nil {
		t.Fatalf("重试后应能重载到还原资产: %v", err)
	}
}

// 导出校验：未知资产、缺失资料（说明附件编号）、已有包文件不得覆盖；
// 源台账与原资料只读。
func TestExportValidationAndReadOnly(t *testing.T) {
	srcDir, _, photo1, _ := buildPackSource(t)
	srcBefore := readFileBytes(t, srcDir)

	// 未知资产整次拒绝，不留下包文件。
	pkg := filepath.Join(t.TempDir(), "p.tar")
	if _, err := exportAssets(srcDir, []string{"EQ-Z"}, pkg); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应拒绝，得到 %v", err)
	}
	if _, err := os.Stat(pkg); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("未知资产拒绝不应留下包文件")
	}

	// 重复编号按一项处理；成功显示数量。
	outcome, err := exportAssets(srcDir, []string{"EQ-A", "EQ-A"}, pkg)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(outcome.assetIDs) != 1 || outcome.attachments != 2 {
		t.Fatalf("摘要 = %+v, 想得到 1 项资产、2 条附件索引", outcome)
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导出不应修改源台账")
	}

	// 已有包文件不得覆盖。
	pkgBytes, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exportAssets(srcDir, []string{"EQ-A"}, pkg); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("已有包文件应拒绝覆盖，得到 %v", err)
	}
	got, err := os.ReadFile(pkg)
	if err != nil || !bytes.Equal(got, pkgBytes) {
		t.Fatal("拒绝覆盖时已有包文件字节应保持不变")
	}

	// 任一所指资料当前不可读：整次拒绝并说明附件编号，不留下包文件。
	if err := os.Remove(photo1); err != nil {
		t.Fatal(err)
	}
	pkg2 := filepath.Join(t.TempDir(), "p2.tar")
	_, err = exportAssets(srcDir, []string{"EQ-A"}, pkg2)
	if err == nil || !strings.Contains(err.Error(), "A0001") {
		t.Fatalf("缺失资料应拒绝并说明附件编号 A0001，得到 %v", err)
	}
	if _, err := os.Stat(pkg2); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("资料缺失拒绝不应留下包文件")
	}

	// 源台账不存在：不能当空库打包。
	if _, err := exportAssets(filepath.Join(t.TempDir(), "no-such"), []string{"EQ-A"}, filepath.Join(t.TempDir(), "p3.tar")); err == nil {
		t.Fatal("源台账不存在应失败")
	}
}

// 命令行入口：参数错误退出 2，业务或读写失败退出 1，成功显示资产及附件索引数量。
func TestPackCommandLine(t *testing.T) {
	srcDir, _, _, _ := buildPackSource(t)
	pkg := filepath.Join(t.TempDir(), "pack.tar")
	dst := filepath.Join(t.TempDir(), "dst")

	var out, errOut bytes.Buffer
	run1 := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := run(args, &out, &errOut)
		return code, out.String(), errOut.String()
	}

	if code, _, _ := run1("export", "--data-dir", srcDir, "--package", pkg); code != 2 {
		t.Fatalf("export 缺少 --asset-id 应退出 2，得到 %d", code)
	}
	if code, _, _ := run1("export", "--data-dir", srcDir, "--asset-id", "EQ-A"); code != 2 {
		t.Fatalf("export 缺少 --package 应退出 2，得到 %d", code)
	}
	if code, _, _ := run1("restore", "--data-dir", dst); code != 2 {
		t.Fatalf("restore 缺少 --package 应退出 2，得到 %d", code)
	}
	if code, _, _ := run1("export", "--data-dir", srcDir, "--asset-id", "EQ-Z", "--package", pkg); code != 1 {
		t.Fatalf("未知资产应退出 1，得到 %d", code)
	}

	code, stdout, _ := run1("export", "--data-dir", srcDir, "--asset-id", "EQ-A", "--asset-id", "EQ-B", "--package", pkg)
	if code != 0 {
		t.Fatalf("导出应成功，得到 %d", code)
	}
	if !strings.Contains(stdout, "已打包 2 项资产") || !strings.Contains(stdout, "附件索引: 3 条") {
		t.Fatalf("导出输出应包含资产及附件索引数量，得到:\n%s", stdout)
	}

	code, stdout, _ = run1("restore", "--package", pkg, "--data-dir", dst)
	if code != 0 {
		t.Fatalf("还原应成功，得到 %d", code)
	}
	if !strings.Contains(stdout, "已还原 2 项资产") || !strings.Contains(stdout, "附件索引: 3 条") {
		t.Fatalf("还原输出应包含资产及附件索引数量，得到:\n%s", stdout)
	}
	// 目标目录已存在：拒绝，退出 1。
	if code, _, _ := run1("restore", "--package", pkg, "--data-dir", dst); code != 1 {
		t.Fatalf("目标目录已存在应退出 1，得到 %d", code)
	}
	// 还原后的台账可正常使用原命令查询。
	code, stdout, _ = run1("ticket", "--data-dir", dst, "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(stdout, "A0001") {
		t.Fatalf("还原后 ticket 查询应正常，得到 %d:\n%s", code, stdout)
	}
}

// readTarMembers 读取 tar 包全部成员（测试辅助）。
func readTarMembers(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	m := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		m[hdr.Name] = b
	}
	return m
}
