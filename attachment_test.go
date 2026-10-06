package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 在目录下建立指定内容的资料文件，返回其绝对路径。
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// newAttachStore 建立含一项资产与一张未关闭工单 T0001 的测试台账。
func newAttachStore(t *testing.T) (*store, string) {
	t.Helper()
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil { // T0001
		t.Fatal(err)
	}
	mustSave(t, s)
	return s, dir
}

// 登记与撤销：各状态工单均可登记；同路径不合并；撤销保留记录、路径、说明与理由；
// 未知编号、重复撤销拒绝；已撤销编号不能恢复，可重新登记为新索引；重载后保持。
func TestAttachAndRevoke(t *testing.T) {
	s, dir := newAttachStore(t)
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "manual.pdf", "v1")
	f2 := writeFile(t, fileDir, "photo.jpg", "v2")

	// 未关闭工单登记；同路径再次登记各自独立，不合并。
	a1, err := s.attachFile("T0001", f1, "维修手册")
	if err != nil || a1.ID != "A0001" {
		t.Fatalf("首次登记: %v %+v", err, a1)
	}
	if a1.Path != f1 || !filepath.IsAbs(a1.Path) {
		t.Fatalf("保存的应为绝对路径: %q", a1.Path)
	}
	a2, err := s.attachFile("T0001", f1, "同一手册另一索引")
	if err != nil || a2.ID != "A0002" {
		t.Fatalf("同路径应独立登记为 A0002: %v %+v", err, a2)
	}
	// 空说明拒绝。
	if _, err := s.attachFile("T0001", f2, ""); err == nil {
		t.Fatal("空说明应拒绝")
	}
	// 未知工单拒绝。
	if _, err := s.attachFile("T9999", f2, "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单应拒绝，得到 %v", err)
	}
	mustSave(t, s)

	// 已关闭工单也可补充资料。
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	a3, err := s.attachFile("T0001", f2, "现场照片")
	if err != nil || a3.ID != "A0003" {
		t.Fatalf("已关闭工单登记: %v %+v", err, a3)
	}
	// 已取消工单同样可补充资料。
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	a4, err := s.attachFile("T0002", f2, "取消工单的照片")
	if err != nil || a4.ID != "A0004" {
		t.Fatalf("已取消工单登记: %v %+v", err, a4)
	}
	mustSave(t, s)

	// 撤销：保留记录、路径、说明与理由。
	got, err := s.revokeAttachment("A0001", "手册版本过期")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Revoked || got.RevokeReason != "手册版本过期" ||
		got.Path != f1 || got.Note != "维修手册" {
		t.Fatalf("撤销后记录应完整保留: %+v", got)
	}
	// 重复撤销与未知编号拒绝；空理由拒绝。
	if _, err := s.revokeAttachment("A0001", "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复撤销应拒绝，得到 %v", err)
	}
	if _, err := s.revokeAttachment("A9999", "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知附件编号应拒绝，得到 %v", err)
	}
	if _, err := s.revokeAttachment("A0002", ""); err == nil {
		t.Fatal("空撤销理由应拒绝")
	}
	// 撤销不影响同路径的其他索引。
	if s.findAttachment("A0002").Revoked {
		t.Fatal("撤销 A0001 不应影响同路径的 A0002")
	}
	// 已撤销编号不能恢复，同路径可重新登记为新索引。
	a5, err := s.attachFile("T0001", f1, "手册新版")
	if err != nil || a5.ID != "A0005" {
		t.Fatalf("撤销后同路径重新登记应为新编号 A0005: %v %+v", err, a5)
	}
	mustSave(t, s)

	// 重载后索引状态与编号延续保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	r1 := s2.findAttachment("A0001")
	if r1 == nil || !r1.Revoked || r1.RevokeReason != "手册版本过期" || r1.Note != "维修手册" {
		t.Fatalf("重载后 A0001 撤销状态未保持: %+v", r1)
	}
	if s2.data.NextAttachSeq != 6 {
		t.Fatalf("下一附件序号 = %d, 想得到 6", s2.data.NextAttachSeq)
	}
	a6, err := s2.attachFile("T0002", f1, "继续登记")
	if err != nil || a6.ID != "A0006" {
		t.Fatalf("重载后编号应延续为 A0006: %v %+v", err, a6)
	}
	mustSave(t, s2)

	// attachmentsOf 按登记履历序号排序，含已撤销。
	list := s2.attachmentsOf("T0001")
	if len(list) != 4 || list[0].ID != "A0001" || list[1].ID != "A0002" ||
		list[2].ID != "A0003" || list[3].ID != "A0005" {
		t.Fatalf("附件顺序应为登记顺序: %+v", list)
	}
}

// 文件失联：登记后文件消失或成为目录仅使引用不可用，不阻止撤销及其他业务；
// 登记时文件不存在、为目录或不可读均拒绝。
func TestAttachFileAvailability(t *testing.T) {
	s, dir := newAttachStore(t)
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "doc.txt", "x")

	// 登记时不存在的路径、目录均拒绝。
	if _, err := s.attachFile("T0001", filepath.Join(fileDir, "nope.txt"), "不存在"); err == nil {
		t.Fatal("不存在的文件应拒绝登记")
	}
	if _, err := s.attachFile("T0001", fileDir, "目录"); err == nil {
		t.Fatal("目录应拒绝登记")
	}
	if len(s.data.Attachments) != 0 || s.data.NextAttachSeq != 1 {
		t.Fatal("失败的登记不应留下记录或消耗编号")
	}

	if _, err := s.attachFile("T0001", f1, "文档"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	// 文件消失：引用变为不可用，但台账仍一致，撤销及其他业务不受影响。
	if err := os.Remove(f1); err != nil {
		t.Fatal(err)
	}
	if regularFileReadable(f1) {
		t.Fatal("文件删除后应不可用")
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("文件消失不应影响台账加载: %v", err)
	}
	if _, err := s2.revokeAttachment("A0001", "文件已清理"); err != nil {
		t.Fatalf("文件消失不应阻止撤销: %v", err)
	}
	// 其他业务不受影响：关闭工单。
	if _, _, err := s2.closeTicket("T0001", "已修复"); err != nil {
		t.Fatalf("文件消失不应影响关闭工单: %v", err)
	}
	mustSave(t, s2)

	// 路径成为目录同样仅使引用不可用。
	f2 := writeFile(t, fileDir, "pic.bin", "y")
	if _, err := s2.attachFile("T0001", f2, "照片"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f2, 0o755); err != nil {
		t.Fatal(err)
	}
	if regularFileReadable(f2) {
		t.Fatal("目录应视为不可用")
	}
	if _, err := openStore(dir); err != nil {
		t.Fatalf("路径成为目录不应影响台账加载: %v", err)
	}
}

// 无附件字段的有效旧库直接使用：缺省视为没有附件索引，附件编号从 A0001 开始。
func TestLegacyStoreWithoutAttachments(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // 旧格式台账，无 attachments 与 next_attach_seq 字段
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "old.pdf", "x")

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无附件字段的旧库应能加载: %v", err)
	}
	a, err := s.attachFile("T0001", f1, "旧库首条附件")
	if err != nil || a.ID != "A0001" {
		t.Fatalf("旧库首条附件编号应为 A0001: %v %+v", err, a)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("登记后重载: %v", err)
	}
	if got := s2.findAttachment("A0001"); got == nil || got.Path != f1 || got.Revoked {
		t.Fatalf("重载后附件记录应保持，得到 %+v", got)
	}
}

// 导入接续：所选资产的全部有效、已撤销索引及履历随资产复制，不复制文件；
// 附件编号按源登记履历序号整体重分配并同步替换引用；路径、状态、说明、理由
// 保持；文件不可用不使导入失败；目标原有记录不变，编号延续。
func TestImportAttachments(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "a.pdf", "a")
	f2 := writeFile(t, fileDir, "b.pdf", "b")

	// 源：EQ-A 两张工单，T0001 已关闭（附件 A0001 有效、A0002 已撤销），
	// T0002 未关闭（附件 A0003）；EQ-B 不导入。
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.registerAsset("EQ-B", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	if _, err := src.attachFile("T0001", f1, "手册"); err != nil { // A0001
		t.Fatal(err)
	}
	if _, err := src.attachFile("T0001", f2, "旧照片"); err != nil { // A0002
		t.Fatal(err)
	}
	if _, err := src.revokeAttachment("A0002", "照片作废"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, err := src.attachFile("T0002", f1, "检测记录"); err != nil { // A0003
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-B", "不制冷", "req-b1"); err != nil { // T0003
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标：已有一项资产与一条附件，编号从 A0002 继续。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "抖动", "req-x1"); err != nil { // T0001
		t.Fatal(err)
	}
	fx := writeFile(t, fileDir, "x.pdf", "x")
	if _, err := dst.attachFile("T0001", fx, "目标原有附件"); err != nil { // A0001
		t.Fatal(err)
	}
	mustSave(t, dst)
	srcBefore := readFileBytes(t, srcDir)

	// 源文件随后不可用：不使导入失败。
	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	want := []attachRemap{{"A0001", "A0002"}, {"A0002", "A0003"}, {"A0003", "A0004"}}
	if len(outcome.attachments) != len(want) {
		t.Fatalf("附件映射数 = %d, 想得到 %d", len(outcome.attachments), len(want))
	}
	for i, m := range outcome.attachments {
		if m != want[i] {
			t.Fatalf("附件映射[%d] = %v, 想得到 %v", i, m, want[i])
		}
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextAttachSeq != 5 {
		t.Fatalf("下一附件序号 = %d, 想得到 5", s.data.NextAttachSeq)
	}
	// 目标原有记录不变。
	orig := s.findAttachment("A0001")
	if orig == nil || orig.TicketID != "T0001" || orig.Note != "目标原有附件" {
		t.Fatalf("目标原有附件 A0001 不应改变，得到 %+v", orig)
	}
	// 导入记录：工单引用同步替换（源 T0001->T0002、T0002->T0003），路径、说明、
	// 撤销状态与理由保持。
	a2 := s.findAttachment("A0002")
	if a2 == nil || a2.TicketID != "T0002" || a2.AssetID != "EQ-A" ||
		a2.Path != f1 || a2.Note != "手册" || a2.Revoked {
		t.Fatalf("导入的 A0002 业务信息应保留，得到 %+v", a2)
	}
	a3 := s.findAttachment("A0003")
	if a3 == nil || !a3.Revoked || a3.RevokeReason != "照片作废" || a3.Path != f2 {
		t.Fatalf("导入的 A0003 撤销状态与理由应保留，得到 %+v", a3)
	}
	a4 := s.findAttachment("A0004")
	if a4 == nil || a4.TicketID != "T0003" || a4.Note != "检测记录" {
		t.Fatalf("导入的 A0004 工单引用应同步替换，得到 %+v", a4)
	}
	// 履历中的附件引用同步替换：目标应能继续撤销导入的索引。
	if _, err := s.revokeAttachment("A0004", "导入后撤销"); err != nil {
		t.Fatalf("导入的附件应可继续撤销: %v", err)
	}
	mustSave(t, s)
	// 新登记从目标计数器延续。
	a5, err := s.attachFile("T0003", f1, "导入后继续登记")
	if err != nil || a5.ID != "A0005" {
		t.Fatalf("导入后新登记应为 A0005: %v %+v", err, a5)
	}
	mustSave(t, s)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("继续操作后的台账应保持一致: %v", err)
	}
	// 再次导入同一批资产按编号冲突拒绝。
	if _, err := importAssets(dstDir, srcDir, []string{"EQ-A"}); err == nil {
		t.Fatal("再次导入同一批资产应拒绝")
	}
}

// 失败重载：登记与撤销写入失败保留原文件字节、不留下部分记录、不消耗编号，
// 恢复后可重试。
func TestAttachSaveFailureLeavesFileUntouched(t *testing.T) {
	s, dir := newAttachStore(t)
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "doc.pdf", "x")
	if _, err := s.attachFile("T0001", f1, "文档"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)
	before := readFileBytes(t, dir)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.attachFile("T0001", f1, "再登记"); err != nil {
		t.Fatal(err)
	}
	saveErr := s2.save()
	if cerr := os.Chmod(dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("写入失败不应改动原文件字节")
	}
	// 重载后无部分记录、编号未消耗，恢复后可重试。
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("写入失败后应可正常重载: %v", err)
	}
	if len(s3.data.Attachments) != 1 || s3.data.NextAttachSeq != 2 {
		t.Fatal("写入失败不应留下部分记录，也不应消耗附件编号")
	}
	a, err := s3.attachFile("T0001", f1, "再登记")
	if err != nil || a.ID != "A0002" {
		t.Fatalf("恢复后重试应分配 A0002: %v %+v", err, a)
	}
	mustSave(t, s3)

	// 撤销的写入失败同样不留下部分变化。
	before = readFileBytes(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	s4, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s4.revokeAttachment("A0002", "作废"); err != nil {
		t.Fatal(err)
	}
	saveErr = s4.save()
	if cerr := os.Chmod(dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("撤销写入失败不应改动原文件字节")
	}
	s5, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s5.findAttachment("A0002").Revoked {
		t.Fatal("失败的撤销不应留下部分变化")
	}
	if _, err := s5.revokeAttachment("A0002", "作废"); err != nil {
		t.Fatalf("恢复后应可重试撤销: %v", err)
	}
	mustSave(t, s5)
}

// 矛盾附件数据：加载即拒绝并保留原文件字节，不自动修复；
// 文件不可用不参与整库一致性校验。
func TestAttachmentContradictoryLedgerRejected(t *testing.T) {
	// withAttach 把一条合法附件（记录+登记履历）加入台账；每次生成独立副本，
	// 避免用例间互相污染。
	withAttach := func(m map[string]any) {
		m["attachments"] = []any{map[string]any{
			"id": "A0001", "ticket_id": "T0001", "asset_id": "EQ-1",
			"path": "/tmp/doc.pdf", "note": "说明书", "revoked": false,
		}}
		m["next_attach_seq"] = 2.0
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "附件登记",
			"content": "说明书", "attachment_id": "A0001", "path": "/tmp/doc.pdf",
			"time": "2026-10-01T10:00:00Z",
		})
	}
	// fresh 先加入合法附件再应用用例修改。
	fresh := func(mutate func(m map[string]any)) func(m map[string]any) {
		return func(m map[string]any) {
			withAttach(m)
			mutate(m)
		}
	}
	revokeEvent := func(seq float64, content string) map[string]any {
		return map[string]any{
			"seq": seq, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "附件撤销",
			"content": content, "attachment_id": "A0001", "path": "/tmp/doc.pdf",
			"time": "2026-10-01T11:00:00Z",
		}
	}

	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"附件编号重复", fresh(func(m map[string]any) {
			appendTo(m, "attachments", map[string]any{
				"id": "A0001", "ticket_id": "T0001", "asset_id": "EQ-1",
				"path": "/tmp/other.pdf", "note": "另一份", "revoked": false,
			})
		}), "数据矛盾"},
		{"路径不是绝对路径", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["path"] = "doc.pdf"
		}), "数据矛盾"},
		{"下一附件序号不大于已用序号", fresh(func(m map[string]any) {
			m["next_attach_seq"] = 1.0
		}), "计数器矛盾"},
		{"附件编号未补零", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["id"] = "A001"
		}), "计数器矛盾"},
		{"已撤销缺少理由", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["revoked"] = true
			appendTo(m, "events", revokeEvent(5, "作废"))
		}), "状态矛盾"},
		{"未撤销带有理由", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["revoke_reason"] = "作废"
		}), "状态矛盾"},
		{"附件引用不存在的工单", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["ticket_id"] = "T0009"
		}), "数据矛盾"},
		{"附件资产与工单归属不一致", fresh(func(m map[string]any) {
			m["attachments"].([]any)[0].(map[string]any)["asset_id"] = "EQ-9"
		}), "数据矛盾"},
		{"附件记录缺少登记履历", fresh(func(m map[string]any) {
			m["events"] = eventsOfLedger(m)[:1] // 只留报修履历
		}), "履历矛盾"},
		{"多条登记履历", fresh(func(m map[string]any) {
			appendTo(m, "events", map[string]any{
				"seq": 5, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "附件登记",
				"content": "说明书", "attachment_id": "A0001", "path": "/tmp/doc.pdf",
				"time": "2026-10-01T10:30:00Z",
			})
		}), "履历矛盾"},
		{"登记履历在报修之前", fresh(func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["seq"] = 2.0
		}), "履历矛盾"},
		{"撤销履历在登记之前", fresh(func(m map[string]any) {
			rec := m["attachments"].([]any)[0].(map[string]any)
			rec["revoked"] = true
			rec["revoke_reason"] = "作废"
			m["events"].([]any)[1].(map[string]any)["seq"] = 5.0
			appendTo(m, "events", revokeEvent(4, "作废")) // 序号 4 早于登记序号 5
		}), "履历矛盾"},
		{"多条撤销履历", fresh(func(m map[string]any) {
			rec := m["attachments"].([]any)[0].(map[string]any)
			rec["revoked"] = true
			rec["revoke_reason"] = "作废"
			appendTo(m, "events", revokeEvent(5, "作废"))
			appendTo(m, "events", revokeEvent(6, "作废"))
		}), "履历矛盾"},
		{"撤销状态与履历不一致", fresh(func(m map[string]any) {
			rec := m["attachments"].([]any)[0].(map[string]any)
			rec["revoked"] = true
			rec["revoke_reason"] = "作废"
			// 不加撤销履历
		}), "状态矛盾"},
		{"撤销理由与履历不一致", fresh(func(m map[string]any) {
			rec := m["attachments"].([]any)[0].(map[string]any)
			rec["revoked"] = true
			rec["revoke_reason"] = "别的理由"
			appendTo(m, "events", revokeEvent(5, "作废"))
		}), "履历矛盾"},
		{"登记履历说明与记录不一致", fresh(func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["content"] = "别的说明"
		}), "履历矛盾"},
		{"登记履历路径与记录不一致", fresh(func(m map[string]any) {
			m["events"].([]any)[1].(map[string]any)["path"] = "/tmp/other.pdf"
		}), "履历矛盾"},
		{"维修履历带有附件字段", func(m map[string]any) {
			m["events"].([]any)[0].(map[string]any)["attachment_id"] = "A0001"
		}, "履历矛盾"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, tc.mutate)
			_, err := openStore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("应报 %q，得到 %v", tc.wantErr, err)
			}
			if got := readFileBytes(t, dir); !bytes.Equal(got, raw) {
				t.Fatal("拒绝加载不应改动原文件字节")
			}
		})
	}

	// 合法附件台账（含已撤销、文件不存在）可正常加载：文件可用性不参与校验。
	dir := t.TempDir()
	writeLedger(t, dir, fresh(func(m map[string]any) {
		rec := m["attachments"].([]any)[0].(map[string]any)
		rec["revoked"] = true
		rec["revoke_reason"] = "作废"
		appendTo(m, "events", revokeEvent(5, "作废"))
	}))
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法附件台账应能加载（文件可用性不参与校验）: %v", err)
	}
	if got := s.findAttachment("A0001"); got == nil || !got.Revoked {
		t.Fatalf("已撤销附件应加载，得到 %+v", got)
	}
}

// eventsOfLedger 返回台账 map 的履历数组。
func eventsOfLedger(m map[string]any) []any { return m["events"].([]any) }

// 命令行入口：参数错误退出 2，业务失败退出 1；ticket 按登记顺序显示附件及
// 有效/已撤销与文件可用性，history 展示登记与撤销履历；相对路径按工作目录解析。
func TestAttachCommandLine(t *testing.T) {
	dir := t.TempDir()
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "manual.pdf", "x")

	var out, errOut bytes.Buffer
	runCmd := func(args ...string) (int, string, string) {
		out.Reset()
		errOut.Reset()
		code := run(append(args, "--data-dir", dir), &out, &errOut)
		return code, out.String(), errOut.String()
	}

	runCmd("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runCmd("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")

	// 参数错误：缺说明、缺路径，退出 2。
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--path", f1); code != 2 {
		t.Fatalf("缺少 --note 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--note", "x"); code != 2 {
		t.Fatalf("缺少 --path 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("revoke", "--attachment-id", "A0001"); code != 2 {
		t.Fatalf("缺少 --reason 应退出 2，得到 %d", code)
	}
	// 业务失败：未知工单、未知附件编号、文件不存在，退出 1。
	if code, _, _ := runCmd("attach", "--ticket-id", "T9999", "--path", f1, "--note", "x"); code != 1 {
		t.Fatalf("未知工单登记应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--path", filepath.Join(fileDir, "nope"), "--note", "x"); code != 1 {
		t.Fatalf("文件不存在应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("revoke", "--attachment-id", "A9999", "--reason", "x"); code != 1 {
		t.Fatalf("未知附件编号应退出 1，得到 %d", code)
	}

	// 相对路径按调用时工作目录解析并保存绝对路径。
	relDir := t.TempDir()
	writeFile(t, relDir, "rel.txt", "y")
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(relDir); err != nil {
		t.Fatal(err)
	}
	// 工作目录可能含符号链接（如 macOS 的 /var），以解析后的目录为准。
	resolvedWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	code, o, _ := runCmd("attach", "--ticket-id", "T0001", "--path", "rel.txt", "--note", "相对路径")
	if err := os.Chdir(oldWd); err != nil {
		t.Fatal(err)
	}
	wantAbs := filepath.Join(resolvedWd, "rel.txt")
	if code != 0 || !strings.Contains(o, "附件编号: A0001") || !strings.Contains(o, "路径: "+wantAbs) {
		t.Fatalf("相对路径登记应保存绝对路径，得到 %d:\n%s", code, o)
	}

	// 绝对路径登记第二条并撤销。
	code, o, _ = runCmd("attach", "--ticket-id", "T0001", "--path", f1, "--note", "维修手册")
	if code != 0 || !strings.Contains(o, "附件编号: A0002") {
		t.Fatalf("登记应输出 A0002，得到 %d:\n%s", code, o)
	}
	code, o, _ = runCmd("revoke", "--attachment-id", "A0002", "--reason", "手册过期")
	if code != 0 || !strings.Contains(o, "附件 A0002 已撤销") {
		t.Fatalf("撤销应成功，得到 %d:\n%s", code, o)
	}
	// 重复撤销退出 1。
	if code, _, _ := runCmd("revoke", "--attachment-id", "A0002", "--reason", "再次"); code != 1 {
		t.Fatalf("重复撤销应退出 1，得到 %d", code)
	}

	// ticket：按登记顺序显示编号、路径、说明与状态；有效附件显示可读/不可用。
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 {
		t.Fatalf("ticket 查询应成功，得到 %d", code)
	}
	i1 := strings.Index(o, "A0001\t"+wantAbs+"\t相对路径\t有效\t可读")
	i2 := strings.Index(o, "A0002\t"+f1+"\t维修手册\t已撤销")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("ticket 应按登记顺序显示附件及状态，得到:\n%s", o)
	}
	// 文件失联后有效附件显示不可用，其余业务不受影响。
	if err := os.Remove(wantAbs); err != nil {
		t.Fatal(err)
	}
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(o, "A0001\t"+wantAbs+"\t相对路径\t有效\t不可用") {
		t.Fatalf("文件消失后应显示不可用，得到 %d:\n%s", code, o)
	}

	// history：展示附件登记与撤销履历（时间、工单、附件编号、路径、说明或理由）。
	code, o, _ = runCmd("history", "--asset-id", "EQ-1")
	if code != 0 ||
		!strings.Contains(o, "附件登记 工单 T0001: 附件编号 A0001，路径 "+wantAbs+"（相对路径）") ||
		!strings.Contains(o, "附件撤销 工单 T0001: 附件编号 A0002，路径 "+f1+"（手册过期）") {
		t.Fatalf("history 应展示附件登记与撤销履历，得到 %d:\n%s", code, o)
	}

	// 无附件明确提示。
	runCmd("register", "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼")
	runCmd("report", "--asset-id", "EQ-2", "--description", "不制冷", "--request-id", "req-2")
	code, o, _ = runCmd("ticket", "--ticket-id", "T0002")
	if code != 0 || !strings.Contains(o, "附件: 无") {
		t.Fatalf("无附件应明确提示，得到 %d:\n%s", code, o)
	}

	// 工单终结不自动撤销附件：关闭工单后附件仍有效。
	runCmd("close", "--ticket-id", "T0001", "--repair-result", "已修复")
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(o, "A0002\t"+f1+"\t维修手册\t已撤销") ||
		strings.Contains(o, "A0001\t"+wantAbs+"\t相对路径\t已撤销") {
		t.Fatalf("关闭工单不应自动撤销附件，得到 %d:\n%s", code, o)
	}
}

// 导入命令行输出附件编号映射。
func TestImportCommandShowsAttachmentMapping(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "a.pdf", "a")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.attachFile("T0001", f1, "手册"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	var out, errOut bytes.Buffer
	code := run([]string{"import", "--source-dir", srcDir, "--asset-id", "EQ-A", "--data-dir", dstDir},
		&out, &errOut)
	if code != 0 {
		t.Fatalf("导入应成功: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "附件编号映射（原编号 -> 新编号）:") ||
		!strings.Contains(out.String(), "A0001 -> A0001") {
		t.Fatalf("导入应输出附件编号映射，得到:\n%s", out.String())
	}
}

// 确保 JSON 序列化往返保持附件字段（含已撤销理由）。
func TestAttachmentJSONRoundTrip(t *testing.T) {
	s, dir := newAttachStore(t)
	fileDir := t.TempDir()
	f1 := writeFile(t, fileDir, "doc.pdf", "x")
	if _, err := s.attachFile("T0001", f1, "文档"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.revokeAttachment("A0001", "作废"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s)

	raw := readFileBytes(t, dir)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	recs, ok := m["attachments"].([]any)
	if !ok || len(recs) != 1 {
		t.Fatalf("台账应含一条附件记录: %v", m["attachments"])
	}
	rec := recs[0].(map[string]any)
	if rec["id"] != "A0001" || rec["path"] != f1 || rec["note"] != "文档" ||
		rec["revoked"] != true || rec["revoke_reason"] != "作废" {
		t.Fatalf("附件记录字段不完整: %v", rec)
	}
	if m["next_attach_seq"] != 2.0 {
		t.Fatalf("下一附件序号应为 2: %v", m["next_attach_seq"])
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	got := s2.findAttachment("A0001")
	if got == nil || !got.Revoked || got.RevokeReason != "作废" || got.Path != f1 {
		t.Fatalf("重载后附件应保持: %+v", got)
	}
}
