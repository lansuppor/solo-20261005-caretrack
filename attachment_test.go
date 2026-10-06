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

// writeAttachFile 在目录中创建一个资料文件并返回绝对路径。
func writeAttachFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("资料"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 登记与撤销：各状态工单均可登记；同路径独立登记不合并；撤销保留记录、路径、
// 说明与理由；未知编号、重复撤销拒绝；已撤销不能恢复，可重新登记为新索引；
// 重载后状态与编号延续保持。
func TestAttachRegisterAndRevoke(t *testing.T) {
	s, dir := newAttachStore(t)
	file1 := writeAttachFile(t, dir, "photo.jpg")

	// 未关闭工单登记。
	a1, err := s.attach("T0001", file1, "卡纸位置照片")
	if err != nil || a1.ID != "A0001" {
		t.Fatalf("首次登记: %v %+v", err, a1)
	}
	// 同路径再次登记：独立编号，不合并。
	a2, err := s.attach("T0001", file1, "另一角度照片")
	if err != nil || a2.ID != "A0002" {
		t.Fatalf("同路径应独立登记: %v %+v", err, a2)
	}
	// 未知工单拒绝，不消耗编号。
	if _, err := s.attach("T9999", file1, "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知工单应拒绝，得到 %v", err)
	}
	// 空说明拒绝。
	if _, err := s.attach("T0001", file1, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空说明应拒绝，得到 %v", err)
	}
	mustSave(t, s)

	// 关闭工单后仍可补充资料。
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	a3, err := s.attach("T0001", file1, "维修后照片")
	if err != nil || a3.ID != "A0003" {
		t.Fatalf("已关闭工单登记: %v %+v", err, a3)
	}
	// 取消的工单同样可补充资料。
	if _, _, err := s.report("EQ-1", "无法开机", "req-2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, _, err := s.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	a4, err := s.attach("T0002", file1, "误报说明截图")
	if err != nil || a4.ID != "A0004" {
		t.Fatalf("已取消工单登记: %v %+v", err, a4)
	}
	mustSave(t, s)

	// 撤销：保留记录、路径、说明与理由。
	rev, err := s.revokeAttachment("A0001", "照片拍错设备")
	if err != nil {
		t.Fatalf("撤销: %v", err)
	}
	if !rev.Revoked || rev.RevokeReason != "照片拍错设备" ||
		rev.Path != file1 || rev.Note != "卡纸位置照片" {
		t.Fatalf("撤销后应保留记录、路径、说明与理由，得到 %+v", rev)
	}
	// 未知编号、重复撤销拒绝。
	if _, err := s.revokeAttachment("A9999", "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知附件编号应拒绝，得到 %v", err)
	}
	if _, err := s.revokeAttachment("A0001", "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复撤销应拒绝，得到 %v", err)
	}
	// 空理由拒绝。
	if _, err := s.revokeAttachment("A0002", ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应拒绝，得到 %v", err)
	}
	// 撤销不影响同路径其他索引。
	if a2.Revoked || a3.Revoked {
		t.Fatal("撤销不应影响同路径其他索引")
	}
	// 工单终结不自动撤销附件。
	if a4.Revoked {
		t.Fatal("工单取消不应自动撤销附件")
	}
	mustSave(t, s)

	// 重载后索引状态与编号延续保持；已撤销编号不能恢复，可重新登记为新索引。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if s2.data.NextAttachSeq != 5 {
		t.Fatalf("下一附件序号 = %d, 想得到 5", s2.data.NextAttachSeq)
	}
	got := s2.findAttachment("A0001")
	if got == nil || !got.Revoked || got.RevokeReason != "照片拍错设备" || got.Note != "卡纸位置照片" {
		t.Fatalf("重载后撤销状态应保持，得到 %+v", got)
	}
	if _, err := s2.revokeAttachment("A0001", "恢复"); !errors.Is(err, errConflict) {
		t.Fatalf("已撤销编号不能恢复，得到 %v", err)
	}
	a5, err := s2.attach("T0001", file1, "重新登记同一文件")
	if err != nil || a5.ID != "A0005" {
		t.Fatalf("重新登记应为新索引 A0005: %v %+v", err, a5)
	}
	mustSave(t, s2)
}

// 路径解析与文件检查：相对路径按调用时工作目录解析并保存绝对路径；
// 不存在、目录形式的路径拒绝，且不消耗附件编号。
func TestAttachPathResolutionAndFileChecks(t *testing.T) {
	s, dir := newAttachStore(t)
	work := t.TempDir()
	writeAttachFile(t, work, "photo.jpg")
	t.Chdir(work)

	// 相对路径按调用时工作目录解析，保存绝对路径（经命令入口）。
	var out, errOut bytes.Buffer
	code := run([]string{"attach", "--ticket-id", "T0001", "--path", "photo.jpg",
		"--note", "现场照片", "--data-dir", dir}, &out, &errOut)
	if code != 0 {
		t.Fatalf("相对路径登记应成功，得到 %d: %s", code, errOut.String())
	}
	want := filepath.Join(work, "photo.jpg")
	a := s.findAttachment("A0001")
	if a == nil {
		s2, err := openStore(dir)
		if err != nil {
			t.Fatalf("重载: %v", err)
		}
		a = s2.findAttachment("A0001")
	}
	if a == nil || a.Path != want || !filepath.IsAbs(a.Path) {
		t.Fatalf("应保存绝对路径 %q，得到 %+v", want, a)
	}

	// 不存在的文件拒绝。
	if _, err := s.attach("T0001", filepath.Join(work, "missing.jpg"), "x"); err == nil {
		t.Fatal("不存在的文件应拒绝登记")
	}
	// 目录不是普通文件，拒绝。
	if _, err := s.attach("T0001", work, "x"); err == nil {
		t.Fatal("目录应拒绝登记")
	}
	// 命令入口同样拒绝，且不消耗编号。
	out.Reset()
	errOut.Reset()
	code = run([]string{"attach", "--ticket-id", "T0001", "--path", work,
		"--note", "x", "--data-dir", dir}, &out, &errOut)
	if code != 1 {
		t.Fatalf("目录登记应退出 1，得到 %d", code)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if s2.data.NextAttachSeq != 2 || len(s2.data.Attachments) != 1 {
		t.Fatal("失败的登记不应消耗编号或留下记录")
	}
	if got := s2.findAttachment("A0001"); got == nil || got.Path != want {
		t.Fatalf("重载后路径应保持，得到 %+v", got)
	}
}

// 文件失联：登记后文件消失或成为目录仅使引用不可用，不阻止撤销及其他业务，
// 也不影响整库一致性校验。
func TestAttachFileLaterUnavailable(t *testing.T) {
	s, dir := newAttachStore(t)
	gone := writeAttachFile(t, dir, "gone.jpg")
	becomeDir := writeAttachFile(t, dir, "become-dir.jpg")
	if _, err := s.attach("T0001", gone, "会消失的文件"); err != nil { // A0001
		t.Fatal(err)
	}
	if _, err := s.attach("T0001", becomeDir, "会变成目录的文件"); err != nil { // A0002
		t.Fatal(err)
	}
	mustSave(t, s)

	// 文件日后消失、成为目录。
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(becomeDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(becomeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if attachmentReadable(gone) || attachmentReadable(becomeDir) {
		t.Fatal("消失或成为目录的文件应不可用")
	}

	// 整库一致性校验不受文件可用性影响：重载正常。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("文件不可用不应影响加载: %v", err)
	}
	// 其他业务不受影响：关闭工单、新报修、新登记（指向存在的文件）。
	if _, _, err := s2.closeTicket("T0001", "已修复"); err != nil {
		t.Fatalf("文件不可用不应阻止关闭工单: %v", err)
	}
	ok := writeAttachFile(t, dir, "ok.jpg")
	if _, err := s2.attach("T0001", ok, "新资料"); err != nil { // A0003
		t.Fatalf("文件不可用不应阻止新登记: %v", err)
	}
	// 撤销不受文件可用性影响。
	if _, err := s2.revokeAttachment("A0001", "文件已丢失"); err != nil {
		t.Fatalf("文件不可用不应阻止撤销: %v", err)
	}
	mustSave(t, s2)

	// 重载后状态保持。
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if a := s3.findAttachment("A0001"); a == nil || !a.Revoked || a.RevokeReason != "文件已丢失" {
		t.Fatalf("撤销状态应保持，得到 %+v", a)
	}
	if a := s3.findAttachment("A0002"); a == nil || a.Revoked {
		t.Fatalf("A0002 应仍为有效，得到 %+v", a)
	}
}

// 无附件字段的有效旧库直接使用：缺省视为没有附件索引，附件编号从 A0001 开始。
func TestLegacyStoreWithoutAttachments(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil) // 旧格式台账，无 attachments 与 next_attach_seq 字段
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("无附件字段的旧库应能加载: %v", err)
	}
	file := writeAttachFile(t, dir, "photo.jpg")
	a, err := s.attach("T0001", file, "现场照片")
	if err != nil || a.ID != "A0001" {
		t.Fatalf("旧库首条附件编号应为 A0001: %v %+v", err, a)
	}
	mustSave(t, s)
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("登记后重载: %v", err)
	}
	if got := s2.findAttachment("A0001"); got == nil || got.Path != file || got.Note != "现场照片" {
		t.Fatalf("重载后附件记录应保持，得到 %+v", got)
	}
}

// 导入接续：有效与已撤销索引及履历随资产复制，附件编号按源登记履历序号整体
// 重分配并输出映射，工单及履历引用同步替换；路径、状态、说明、理由保持；
// 不复制文件，文件不可用不使导入失败；源只读，目标原有记录不变，导入后可继续登记。
func TestImportAttachmentsRemapAndContinue(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	files := t.TempDir()

	// 源：EQ-A 一张已关闭工单（有效附件 A0001、已撤销附件 A0002）与一张未关闭
	// 工单（附件 A0003，登记后文件删除——文件不可用不使导入失败）。
	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "卡纸", "req-a1"); err != nil { // T0001
		t.Fatal(err)
	}
	f1 := writeAttachFile(t, files, "p1.jpg")
	f2 := writeAttachFile(t, files, "p2.jpg")
	f3 := writeAttachFile(t, files, "p3.jpg")
	if _, err := src.attach("T0001", f1, "卡纸照片"); err != nil { // A0001
		t.Fatal(err)
	}
	if _, err := src.attach("T0001", f2, "拍错的照片"); err != nil { // A0002
		t.Fatal(err)
	}
	if _, err := src.revokeAttachment("A0002", "拍错设备"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, err := src.attach("T0002", f3, "电源照片"); err != nil { // A0003
		t.Fatal(err)
	}
	mustSave(t, src)
	// 登记后文件消失：只使引用不可用，不影响导入。
	if err := os.Remove(f3); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(srcDir); err != nil {
		t.Fatalf("文件不可用不应影响源台账加载: %v", err)
	}

	// 目标：EQ-X 一张未关闭工单，已有一条附件 A0001。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "抖动", "req-x1"); err != nil { // T0001
		t.Fatal(err)
	}
	fx := writeAttachFile(t, files, "px.jpg")
	if _, err := dst.attach("T0001", fx, "抖动视频"); err != nil { // A0001
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	dstBefore := readFileBytes(t, dstDir)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	wantAttach := []attachRemap{{"A0001", "A0002"}, {"A0002", "A0003"}, {"A0003", "A0004"}}
	if len(outcome.attachments) != len(wantAttach) {
		t.Fatalf("附件映射数 = %d, 想得到 %d", len(outcome.attachments), len(wantAttach))
	}
	for i, m := range outcome.attachments {
		if m != wantAttach[i] {
			t.Fatalf("附件映射[%d] = %v, 想得到 %v", i, m, wantAttach[i])
		}
	}
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}
	if string(dstBefore) == string(readFileBytes(t, dstDir)) {
		t.Fatal("目标台账应已更新")
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
	if orig == nil || orig.TicketID != "T0001" || orig.Path != fx || orig.Note != "抖动视频" || orig.Revoked {
		t.Fatalf("目标原有附件 A0001 不应改变，得到 %+v", orig)
	}
	// 导入记录：工单引用同步替换，路径、状态、说明、理由保持。
	a2 := s.findAttachment("A0002")
	if a2 == nil || a2.TicketID != "T0002" || a2.AssetID != "EQ-A" ||
		a2.Path != f1 || a2.Note != "卡纸照片" || a2.Revoked {
		t.Fatalf("导入的 A0002 业务信息应保留，得到 %+v", a2)
	}
	a3 := s.findAttachment("A0003")
	if a3 == nil || !a3.Revoked || a3.RevokeReason != "拍错设备" || a3.Path != f2 {
		t.Fatalf("导入的已撤销附件状态与理由应保持，得到 %+v", a3)
	}
	// 撤销履历的附件引用同步更新。
	var revEv *Event
	for i := range s.data.Events {
		if s.data.Events[i].Kind == eventAttachRevoke {
			revEv = &s.data.Events[i]
		}
	}
	if revEv == nil || revEv.AttachmentID != "A0003" || revEv.TicketID != "T0002" ||
		revEv.Path != f2 || revEv.Content != "拍错设备" {
		t.Fatalf("撤销履历引用应同步更新，得到 %+v", revEv)
	}
	// 文件不可用的索引照常导入。
	a4 := s.findAttachment("A0004")
	if a4 == nil || a4.TicketID != "T0003" || a4.Path != f3 || a4.Revoked {
		t.Fatalf("文件不可用的索引应照常导入，得到 %+v", a4)
	}
	// 导入后可继续登记，编号延续。
	f5 := writeAttachFile(t, files, "p5.jpg")
	a5, err := s.attach("T0003", f5, "补充资料")
	if err != nil || a5.ID != "A0005" {
		t.Fatalf("导入后登记应延续编号 A0005: %v %+v", err, a5)
	}
	mustSave(t, s)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("登记后的台账应保持一致: %v", err)
	}
}

// 矛盾：附件编号重复、计数器回退、相对路径、缺少或重复登记履历、撤销在登记
// 之前、重复撤销、记录内容与履历不一致、撤销状态与履历不符等，加载即拒绝并
// 保留原文件字节，不自动修复。
func TestAttachmentContradictoryLedgerRejected(t *testing.T) {
	// 合法基准：T0001 未关闭，A0001 已撤销（登记 + 撤销履历齐全）。
	base := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"附件登记","content":"现场照片","attachment_id":"A0001","path":"/tmp/photo.jpg","time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"附件撤销","content":"拍错了","attachment_id":"A0001","path":"/tmp/photo.jpg","time":"2026-10-01T10:00:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "attachments": [{"id":"A0001","ticket_id":"T0001","asset_id":"EQ-1","path":"/tmp/photo.jpg","note":"现场照片","revoked":true,"revoke_reason":"拍错了"}],
  "next_ticket_seq": 2,
  "next_attach_seq": 2
}`
	write := func(t *testing.T, mutate func(m map[string]any)) (string, []byte) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(base), &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, raw
	}
	eventsOf := func(m map[string]any) []any { return m["events"].([]any) }
	attachOf := func(m map[string]any) map[string]any { return m["attachments"].([]any)[0].(map[string]any) }

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"附件编号重复", func(m map[string]any) {
			m["attachments"] = append(m["attachments"].([]any), map[string]any{
				"id": "A0001", "ticket_id": "T0001", "asset_id": "EQ-1",
				"path": "/tmp/other.jpg", "note": "另一张", "revoked": false,
			})
		}},
		{"下一附件序号不大于已用序号", func(m map[string]any) {
			m["next_attach_seq"] = 1
		}},
		{"附件路径不是绝对路径", func(m map[string]any) {
			attachOf(m)["path"] = "tmp/photo.jpg"
		}},
		{"附件记录引用不存在的工单", func(m map[string]any) {
			attachOf(m)["ticket_id"] = "T9999"
		}},
		{"附件记录缺少登记履历", func(m map[string]any) {
			m["events"] = eventsOf(m)[:1]
			attachOf(m)["revoked"] = false
			delete(attachOf(m), "revoke_reason")
		}},
		{"附件记录有多条登记履历", func(m map[string]any) {
			dup := map[string]any{
				"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "附件登记",
				"content": "现场照片", "attachment_id": "A0001", "path": "/tmp/photo.jpg",
				"time": "2026-10-01T11:00:00Z",
			}
			m["events"] = append(eventsOf(m), dup)
		}},
		{"撤销履历在登记履历之前", func(m map[string]any) {
			eventsOf(m)[1].(map[string]any)["seq"] = 3
			eventsOf(m)[2].(map[string]any)["seq"] = 2
		}},
		{"附件记录有多条撤销履历", func(m map[string]any) {
			dup := map[string]any{
				"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "附件撤销",
				"content": "拍错了", "attachment_id": "A0001", "path": "/tmp/photo.jpg",
				"time": "2026-10-01T11:00:00Z",
			}
			m["events"] = append(eventsOf(m), dup)
		}},
		{"登记履历与记录说明不一致", func(m map[string]any) {
			eventsOf(m)[1].(map[string]any)["content"] = "另一段说明"
		}},
		{"撤销履历与记录理由不一致", func(m map[string]any) {
			eventsOf(m)[2].(map[string]any)["content"] = "另一个理由"
		}},
		{"撤销状态与履历不符", func(m map[string]any) {
			attachOf(m)["revoked"] = false
			delete(attachOf(m), "revoke_reason")
		}},
		{"登记履历在报修之前", func(m map[string]any) {
			eventsOf(m)[0].(map[string]any)["seq"] = 5
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, raw := write(t, tc.mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾台账应拒绝加载")
			}
			if got := readFileBytes(t, dir); string(got) != string(raw) {
				t.Fatal("拒绝加载不应改动原文件字节")
			}
		})
	}

	// 基准台账本身合法，可正常加载（路径指向的文件是否存在不参与校验）。
	dir, _ := write(t, func(m map[string]any) {})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if a := s.findAttachment("A0001"); a == nil || !a.Revoked || a.RevokeReason != "拍错了" {
		t.Fatalf("合法台账的附件记录应加载，得到 %+v", a)
	}
}

// 失败重载：写入失败保留原文件字节、不留下部分记录、不消耗编号，恢复后可重试。
func TestAttachSaveFailureLeavesFileUntouched(t *testing.T) {
	s, dir := newAttachStore(t)
	file := writeAttachFile(t, dir, "photo.jpg")
	if _, err := s.attach("T0001", file, "现场照片"); err != nil {
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
	if _, err := s2.attach("T0001", file, "另一张"); err != nil {
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
	a, err := s3.attach("T0001", file, "另一张")
	if err != nil || a.ID != "A0002" {
		t.Fatalf("恢复后重试应分配 A0002: %v %+v", err, a)
	}
	mustSave(t, s3)
}

// 命令行入口：参数错误退出 2，业务失败退出 1；ticket 按登记顺序显示附件及
// 文件可用性，history 展示登记、撤销履历。
func TestAttachCommandLine(t *testing.T) {
	dir := t.TempDir()
	files := t.TempDir()
	photo := writeAttachFile(t, files, "photo.jpg")
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
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--path", photo); code != 2 {
		t.Fatalf("缺少 --note 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--note", "x"); code != 2 {
		t.Fatalf("缺少 --path 应退出 2，得到 %d", code)
	}
	if code, _, _ := runCmd("revoke", "--attachment-id", "A0001"); code != 2 {
		t.Fatalf("缺少 --reason 应退出 2，得到 %d", code)
	}
	// 业务失败：未知工单、不存在的文件、未知附件编号，退出 1。
	if code, _, _ := runCmd("attach", "--ticket-id", "T9999", "--path", photo, "--note", "x"); code != 1 {
		t.Fatalf("未知工单登记应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("attach", "--ticket-id", "T0001", "--path", filepath.Join(files, "missing.jpg"), "--note", "x"); code != 1 {
		t.Fatalf("不存在的文件应退出 1，得到 %d", code)
	}
	if code, _, _ := runCmd("revoke", "--attachment-id", "A9999", "--reason", "x"); code != 1 {
		t.Fatalf("未知附件编号应退出 1，得到 %d", code)
	}

	// 成功登记并输出附件编号。
	code, o, _ := runCmd("attach", "--ticket-id", "T0001", "--path", photo, "--note", "卡纸照片")
	if code != 0 || !strings.Contains(o, "附件编号: A0001") {
		t.Fatalf("登记应成功并输出 A0001，得到 %d:\n%s", code, o)
	}
	// ticket：有效附件显示文件可读。
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(o, "A0001\t"+photo+"\t卡纸照片\t有效（文件可读）") {
		t.Fatalf("ticket 应显示有效附件及文件可读，得到 %d:\n%s", code, o)
	}
	// 撤销成功显示已撤销；重复撤销退出 1。
	code, o, _ = runCmd("revoke", "--attachment-id", "A0001", "--reason", "拍错设备")
	if code != 0 || !strings.Contains(o, "附件 A0001 已撤销。") {
		t.Fatalf("撤销应成功并显示已撤销，得到 %d:\n%s", code, o)
	}
	if code, _, _ := runCmd("revoke", "--attachment-id", "A0001", "--reason", "再次"); code != 1 {
		t.Fatalf("重复撤销应退出 1，得到 %d", code)
	}
	// ticket：已撤销状态保留显示。
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(o, "A0001\t"+photo+"\t卡纸照片\t已撤销") {
		t.Fatalf("ticket 应显示已撤销附件，得到 %d:\n%s", code, o)
	}
	// 文件消失后有效附件显示不可用。
	gone := writeAttachFile(t, files, "gone.jpg")
	runCmd("attach", "--ticket-id", "T0001", "--path", gone, "--note", "会消失")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	code, o, _ = runCmd("ticket", "--ticket-id", "T0001")
	if code != 0 || !strings.Contains(o, "A0002\t"+gone+"\t会消失\t有效（文件不可用）") {
		t.Fatalf("ticket 应显示文件不可用，得到 %d:\n%s", code, o)
	}
	// history：展示登记与撤销履历。
	code, o, _ = runCmd("history", "--asset-id", "EQ-1")
	if code != 0 ||
		!strings.Contains(o, "附件登记 工单 T0001: 附件编号 A0001，路径 "+photo+"（卡纸照片）") ||
		!strings.Contains(o, "附件撤销 工单 T0001: 附件编号 A0001，路径 "+photo+"（拍错设备）") {
		t.Fatalf("history 应展示附件登记与撤销履历，得到 %d:\n%s", code, o)
	}
	// 无记录明确提示。
	runCmd("register", "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼")
	runCmd("report", "--asset-id", "EQ-2", "--description", "不制冷", "--request-id", "req-2")
	code, o, _ = runCmd("ticket", "--ticket-id", "T0002")
	if code != 0 || !strings.Contains(o, "附件: 无") {
		t.Fatalf("无附件记录应明确提示，得到 %d:\n%s", code, o)
	}
}
