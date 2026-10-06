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
)

// 备件测试的准备：登记资产并开出一张未关闭工单 T0001。
func setupPartTicket(t *testing.T, s *store, assetID string) {
	t.Helper()
	if _, err := s.registerAsset(assetID, "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report(assetID, "卡纸", "req-"+assetID); err != nil {
		t.Fatal(err)
	}
}

// 分笔退回：同一备件多笔领用各自独立；部分退回累计推进，超额与未知编号拒绝，
// 不能冲减另一笔；重载后编号、累计退回与净量保持。
func TestPartIssueAndPartialReturns(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	setupPartTicket(t, s, "EQ-1")

	pi1, err := s.issuePart("T0001", "SP-1", 5, "更换搓纸轮")
	if err != nil || pi1.ID != "P0001" {
		t.Fatalf("首次领用应为 P0001: %+v err=%v", pi1, err)
	}
	// 同一备件再次领用是独立的一笔。
	pi2, err := s.issuePart("T0001", "SP-1", 3, "备件补仓")
	if err != nil || pi2.ID != "P0002" {
		t.Fatalf("再次领用应为 P0002: %+v err=%v", pi2, err)
	}
	pi3, err := s.issuePart("T0001", "SP-2", 2, "更换传感器")
	if err != nil || pi3.ID != "P0003" {
		t.Fatalf("第三笔领用应为 P0003: %+v err=%v", pi3, err)
	}

	// 分笔退回：2 + 3 = 5，恰好等于原数量。
	if _, returned, err := s.returnPart("P0001", 2, "剩余退回"); err != nil || returned != 2 {
		t.Fatalf("第一次退回累计应为 2: %d err=%v", returned, err)
	}
	if _, returned, err := s.returnPart("P0001", 3, "维修完成退回"); err != nil || returned != 5 {
		t.Fatalf("第二次退回累计应为 5: %d err=%v", returned, err)
	}
	// 超额退回拒绝：P0001 已退满。
	if _, _, err := s.returnPart("P0001", 1, "再退"); !errors.Is(err, errConflict) {
		t.Fatalf("超额退回应拒绝，得到 %v", err)
	}
	// 不能冲减另一笔：P0001 退满不影响 P0002 的额度（3），退 4 仍超额。
	if _, _, err := s.returnPart("P0002", 4, "超额"); !errors.Is(err, errConflict) {
		t.Fatalf("冲减另一笔的超额退回应拒绝，得到 %v", err)
	}
	// 未知领用编号拒绝。
	if _, _, err := s.returnPart("P9999", 1, "未知"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知领用编号应拒绝，得到 %v", err)
	}
	// SP-2 全部退回，净量为零仍应出现在汇总中。
	if _, returned, err := s.returnPart("P0003", 2, "未使用"); err != nil || returned != 2 {
		t.Fatalf("SP-2 退回应成功: %d err=%v", returned, err)
	}
	mustSave(t, s)

	// 重载后编号、累计退回与净量保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	rows := s2.partIssuesOf("T0001")
	want := []partIssueRow{
		{ID: "P0001", PartNo: "SP-1", Qty: 5, Returned: 5, Net: 0},
		{ID: "P0002", PartNo: "SP-1", Qty: 3, Returned: 0, Net: 3},
		{ID: "P0003", PartNo: "SP-2", Qty: 2, Returned: 2, Net: 0},
	}
	if len(rows) != len(want) {
		t.Fatalf("领用记录数 = %d, 想得到 %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i] != w {
			t.Fatalf("领用行[%d] = %+v, 想得到 %+v", i, rows[i], w)
		}
	}
	// 按备件编号字典序汇总净量，零值仍显示。
	summary := summarizePartNet(rows)
	wantSummary := []partNetRow{{PartNo: "SP-1", Net: 3}, {PartNo: "SP-2", Net: 0}}
	if len(summary) != len(wantSummary) {
		t.Fatalf("汇总行数 = %d, 想得到 %d", len(summary), len(wantSummary))
	}
	for i, w := range wantSummary {
		if summary[i] != w {
			t.Fatalf("汇总[%d] = %+v, 想得到 %+v", i, summary[i], w)
		}
	}
	// 领用编号不复用：下一笔为 P0004。
	pi4, err := s2.issuePart("T0001", "SP-3", 1, "新领用")
	if err != nil || pi4.ID != "P0004" {
		t.Fatalf("重载后领用应为 P0004: %+v err=%v", pi4, err)
	}
	mustSave(t, s2)
}

// 终结：工单关闭或取消后拒绝领用与退回，但记录与净量保留、不清零；
// 旧单操作不影响同一资产之后的新工单。
func TestPartIssueReturnAfterTermination(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	setupPartTicket(t, s, "EQ-1")
	if _, err := s.issuePart("T0001", "SP-1", 4, "更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.returnPart("P0001", 1, "剩余"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	// 终结后拒绝领用与退回。
	if _, err := s.issuePart("T0001", "SP-1", 1, "再领"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单领用应拒绝，得到 %v", err)
	}
	if _, _, err := s.returnPart("P0001", 1, "再退"); !errors.Is(err, errConflict) {
		t.Fatalf("已关闭工单退回应拒绝，得到 %v", err)
	}
	mustSave(t, s)

	// 重载后记录与净量保留，不自动清零。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	rows := s2.partIssuesOf("T0001")
	if len(rows) != 1 || rows[0].Qty != 4 || rows[0].Returned != 1 || rows[0].Net != 3 {
		t.Fatalf("关闭后领用记录与净量应保留，得到 %+v", rows)
	}

	// 同一资产报修新工单后可正常领用，旧单记录不受影响。
	if _, _, err := s2.report("EQ-1", "无法开机", "req-2"); err != nil { // T0002
		t.Fatal(err)
	}
	pi, err := s2.issuePart("T0002", "SP-1", 2, "新工单领用")
	if err != nil || pi.ID != "P0002" {
		t.Fatalf("新工单领用应为 P0002: %+v err=%v", pi, err)
	}
	if _, returned, err := s2.returnPart("P0002", 2, "未使用"); err != nil || returned != 2 {
		t.Fatalf("新工单退回应成功: %d err=%v", returned, err)
	}
	if rows := s2.partIssuesOf("T0001"); len(rows) != 1 || rows[0].Net != 3 {
		t.Fatalf("旧单净量不应受新单影响，得到 %+v", rows)
	}

	// 取消同样是终态。
	if _, _, err := s2.cancelTicket("T0002", "误报"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.issuePart("T0002", "SP-1", 1, "再领"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单领用应拒绝，得到 %v", err)
	}
	if _, _, err := s2.returnPart("P0002", 1, "再退"); !errors.Is(err, errConflict) {
		t.Fatalf("已取消工单退回应拒绝，得到 %v", err)
	}
	mustSave(t, s2)
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if rows := s3.partIssuesOf("T0002"); len(rows) != 1 || rows[0].Net != 0 || rows[0].Returned != 2 {
		t.Fatalf("取消后领用记录与净量应保留，得到 %+v", rows)
	}
}

// 导入接续：备件记录与履历随资产复制，领用编号按源领用顺序重分配并输出映射，
// 工单及退回引用同步更新；源只读，目标原有记录不变，导入的未关闭工单可继续退回。
func TestImportPartIssues(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	// 源：EQ-A 未关闭工单 T0001（P0001 退过 2、P0002 未退），已关闭工单 T0002
	// （P0003 全部退回）；EQ-B 有领用但不导入。
	src := newStoreAt(t, srcDir)
	setupPartTicket(t, src, "EQ-A")                                       // T0001
	if _, err := src.issuePart("T0001", "SP-1", 5, "更换搓纸轮"); err != nil { // P0001
		t.Fatal(err)
	}
	if _, _, err := src.returnPart("P0001", 2, "剩余退回"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.issuePart("T0001", "SP-2", 1, "更换传感器"); err != nil { // P0002
		t.Fatal(err)
	}
	if _, _, err := src.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.report("EQ-A", "无法开机", "req-a2"); err != nil { // T0002
		t.Fatal(err)
	}
	if _, err := src.issuePart("T0002", "SP-1", 2, "再次更换"); err != nil { // P0003
		t.Fatal(err)
	}
	setupPartTicket(t, src, "EQ-B")                                     // T0003，不导入
	if _, err := src.issuePart("T0003", "SP-9", 1, "不导入"); err != nil { // P0004
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标：EQ-X 一张未关闭工单 T0001，已有一笔领用 P0001。
	dst := newStoreAt(t, dstDir)
	setupPartTicket(t, dst, "EQ-X")
	if _, err := dst.issuePart("T0001", "SP-0", 7, "目标原有领用"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)
	srcBefore := readFileBytes(t, srcDir)
	dstBefore := readFileBytes(t, dstDir)

	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	wantIssues := []issueRemap{{"P0001", "P0002"}, {"P0002", "P0003"}, {"P0003", "P0004"}}
	if len(outcome.issues) != len(wantIssues) {
		t.Fatalf("领用映射数 = %d, 想得到 %d", len(outcome.issues), len(wantIssues))
	}
	for i, m := range wantIssues {
		if outcome.issues[i] != m {
			t.Fatalf("领用映射[%d] = %v, 想得到 %v", i, outcome.issues[i], m)
		}
	}
	// 源只读，目标已更新。
	if string(readFileBytes(t, srcDir)) != string(srcBefore) {
		t.Fatal("导入不应修改源台账")
	}
	if string(readFileBytes(t, dstDir)) == string(dstBefore) {
		t.Fatal("目标台账应已更新")
	}

	s, err := openStore(dstDir)
	if err != nil {
		t.Fatalf("重载目标: %v", err)
	}
	if s.data.NextIssueSeq != 5 {
		t.Fatalf("下一领用序号 = %d, 想得到 5", s.data.NextIssueSeq)
	}
	// 目标原有领用记录不变。
	rows := s.partIssuesOf("T0001")
	if len(rows) != 1 || rows[0].ID != "P0001" || rows[0].PartNo != "SP-0" || rows[0].Net != 7 {
		t.Fatalf("目标原有领用不应改变，得到 %+v", rows)
	}
	// 导入的领用记录：工单引用已替换，数量与累计退回保持。
	// 源 T0001 -> 目标 T0002，源 T0002 -> 目标 T0003。
	rows2 := s.partIssuesOf("T0002")
	want2 := []partIssueRow{
		{ID: "P0002", PartNo: "SP-1", Qty: 5, Returned: 2, Net: 3},
		{ID: "P0003", PartNo: "SP-2", Qty: 1, Returned: 0, Net: 1},
	}
	if len(rows2) != len(want2) {
		t.Fatalf("导入工单领用数 = %d, 想得到 %d", len(rows2), len(want2))
	}
	for i, w := range want2 {
		if rows2[i] != w {
			t.Fatalf("导入领用行[%d] = %+v, 想得到 %+v", i, rows2[i], w)
		}
	}
	if rows3 := s.partIssuesOf("T0003"); len(rows3) != 1 || rows3[0].ID != "P0004" ||
		rows3[0].Returned != 0 || rows3[0].Net != 2 {
		t.Fatalf("映射工单 T0003 的领用应保持，得到 %+v", rows3)
	}
	// 退回履历的领用引用已同步更新。
	found := false
	for _, e := range s.data.Events {
		if e.Kind == eventPartReturn {
			if e.IssueID != "P0002" || e.TicketID != "T0002" || e.Qty != 2 || e.Content != "剩余退回" {
				t.Fatalf("退回履历引用应同步更新，得到 %+v", e)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("导入后应存在退回履历")
	}
	// 履历时间精度保持：与源逐条相等。
	srcRe, err := openStore(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	srcEv, dstEv := srcRe.eventsOf("EQ-A"), s.eventsOf("EQ-A")
	if len(srcEv) != len(dstEv) {
		t.Fatalf("履历数 = %d, 想得到 %d", len(dstEv), len(srcEv))
	}
	for i := range srcEv {
		if !srcEv[i].Time.Equal(dstEv[i].Time) || srcEv[i].Kind != dstEv[i].Kind {
			t.Fatalf("履历[%d] 应保持时间与类型: 源 %+v 目标 %+v", i, srcEv[i], dstEv[i])
		}
	}

	// 导入的未关闭工单（T0003）可继续退回，并可在目标继续领用（编号接续 P0005）。
	if _, returned, err := s.returnPart("P0004", 1, "导入后继续退回"); err != nil || returned != 1 {
		t.Fatalf("导入后应可继续退回: %d err=%v", returned, err)
	}
	pi, err := s.issuePart("T0003", "SP-3", 1, "导入后新领用")
	if err != nil || pi.ID != "P0005" {
		t.Fatalf("导入后新领用应为 P0005: %+v err=%v", pi, err)
	}
	mustSave(t, s)
	if _, err := openStore(dstDir); err != nil {
		t.Fatalf("后续操作后的台账应保持一致: %v", err)
	}
}

// 矛盾数据：超额退回、退回先于领用、终结后领用、编号计数器矛盾均拒绝加载，
// 原文件字节保持不变；无备件字段的有效旧库直接使用。
func TestPartLedgerContradictions(t *testing.T) {
	// 合法基准：一张未关闭工单，P0001 领用 5 退 2。
	base := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [
    {"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"},
    {"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件领用","content":"更换用","issue_id":"P0001","part_no":"SP-1","qty":5,"time":"2026-10-01T09:00:00Z"},
    {"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件退回","content":"剩余","issue_id":"P0001","part_no":"SP-1","qty":2,"time":"2026-10-01T10:00:00Z"}
  ],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "part_issues": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_no":"SP-1","qty":5,"note":"更换用"}],
  "next_ticket_seq": 2,
  "next_issue_seq": 2
}`

	write := func(t *testing.T, content string) (string, []byte) {
		t.Helper()
		dir := t.TempDir()
		raw := []byte(content)
		if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return dir, raw
	}
	mustReject := func(t *testing.T, name, content string) {
		t.Helper()
		dir, raw := write(t, content)
		if _, err := openStore(dir); err == nil {
			t.Fatalf("%s：矛盾数据应拒绝加载", name)
		}
		if string(readFileBytes(t, dir)) != string(raw) {
			t.Fatalf("%s：拒绝加载不应改动原文件", name)
		}
	}

	// 基准本身合法。
	dir, _ := write(t, base)
	if _, err := openStore(dir); err != nil {
		t.Fatalf("合法备件台账应可加载: %v", err)
	}

	// 累计退回超过原数量（6 > 5）。
	mustReject(t, "超额退回", strings.Replace(base,
		`"qty":2,"time":"2026-10-01T10:00:00Z"`, `"qty":6,"time":"2026-10-01T10:00:00Z"`, 1))
	// 退回先于领用（交换履历序号）。
	mustReject(t, "退回先于领用", strings.NewReplacer(
		`"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件领用"`, `"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件领用"`,
		`"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件退回"`, `"seq":2,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件退回"`,
	).Replace(base))
	// 终结后领用：关闭履历序号 4 之后又出现领用履历序号 5。
	terminated := strings.NewReplacer(
		`"status":"未关闭"`, `"status":"已关闭","result":"已修复","closed_at":"2026-10-01T11:00:00Z"`,
		`"status":"维修中"`, `"status":"可用"`,
		"  ],\n  \"requests\"",
		`    ,{"seq":4,"asset_id":"EQ-1","ticket_id":"T0001","kind":"关闭","content":"已修复","time":"2026-10-01T11:00:00Z"},
    {"seq":5,"asset_id":"EQ-1","ticket_id":"T0001","kind":"备件领用","content":"终结后领用","issue_id":"P0002","part_no":"SP-2","qty":1,"time":"2026-10-01T12:00:00Z"}
  ],
  "requests"`,
		`"part_issues": [{"id":"P0001"`,
		`"part_issues": [{"id":"P0002","ticket_id":"T0001","asset_id":"EQ-1","part_no":"SP-2","qty":1,"note":"终结后领用"},{"id":"P0001"`,
		`"next_issue_seq": 2`, `"next_issue_seq": 3`,
	).Replace(base)
	mustReject(t, "终结后领用", terminated)
	// 领用编号计数器矛盾：已用 P0001，下一序号却为 1。
	mustReject(t, "领用计数器矛盾", strings.Replace(base, `"next_issue_seq": 2`, `"next_issue_seq": 1`, 1))
	// 领用编号重复。
	mustReject(t, "领用编号重复", strings.Replace(base,
		`"part_issues": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_no":"SP-1","qty":5,"note":"更换用"}]`,
		`"part_issues": [{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_no":"SP-1","qty":5,"note":"更换用"},{"id":"P0001","ticket_id":"T0001","asset_id":"EQ-1","part_no":"SP-1","qty":5,"note":"更换用"}]`, 1))
	// 退回指向不存在的领用记录。
	mustReject(t, "退回指向未知领用", strings.Replace(base,
		`"kind":"备件退回","content":"剩余","issue_id":"P0001"`, `"kind":"备件退回","content":"剩余","issue_id":"P0009"`, 1))

	// 无备件字段的有效旧库直接使用：可加载、可领用（编号从 P0001 开始）。
	old := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [{"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"}],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "next_ticket_seq": 2
}`
	oldDir, _ := write(t, old)
	if _, err := openStore(oldDir); err != nil {
		t.Fatalf("无备件字段的旧库应直接使用: %v", err)
	}
	s2 := newStoreAt(t, oldDir)
	pi, err := s2.issuePart("T0001", "SP-1", 1, "旧库新领用")
	if err != nil || pi.ID != "P0001" {
		t.Fatalf("旧库领用应从 P0001 开始: %+v err=%v", pi, err)
	}
	mustSave(t, s2)
	if _, err := openStore(oldDir); err != nil {
		t.Fatalf("旧库领用保存后应可重载: %v", err)
	}
}

// 失败重载：保存失败（目录不可写）保留原文件字节、不留下部分记录、不消耗
// 领用编号；恢复后可重试成功。
func TestPartSaveFailureRetry(t *testing.T) {
	dir := t.TempDir()
	s := newStoreAt(t, dir)
	setupPartTicket(t, s, "EQ-1")
	mustSave(t, s)
	before := readFileBytes(t, dir)

	s2 := newStoreAt(t, dir)
	if _, err := s2.issuePart("T0001", "SP-1", 3, "更换搓纸轮"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s2.save()
	if cerr := os.Chmod(dir, 0o755); cerr != nil {
		t.Fatal(cerr)
	}
	if saveErr == nil {
		t.Skip("当前环境允许只读目录写入，跳过写入失败分支")
	}
	// 原文件字节不变；重载后无部分记录、编号未消耗。
	if string(readFileBytes(t, dir)) != string(before) {
		t.Fatal("保存失败不应改动原文件")
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatalf("保存失败后应可正常重载: %v", err)
	}
	if len(s3.data.PartIssues) != 0 || s3.data.NextIssueSeq != 1 {
		t.Fatal("保存失败不应留下部分领用记录，也不应消耗领用编号")
	}
	// 恢复后可重试成功，编号仍为 P0001。
	s4 := newStoreAt(t, dir)
	pi, err := s4.issuePart("T0001", "SP-1", 3, "更换搓纸轮")
	if err != nil || pi.ID != "P0001" {
		t.Fatalf("重试领用应为 P0001: %+v err=%v", pi, err)
	}
	mustSave(t, s4)
	s5, err := openStore(dir)
	if err != nil || len(s5.data.PartIssues) != 1 {
		t.Fatalf("重试保存后应能重载到领用记录: %v", err)
	}
}

// 命令行入口：参数错误退出 2，业务失败退出 1；ticket 与 history 展示备件台账。
func TestPartCommandLine(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runOK := func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		full := append(args, "--data-dir", dir)
		if code := run(full, &out, &errOut); code != 0 {
			t.Fatalf("%v 应成功，得到 %d（%s）", args, code, errOut.String())
		}
		return out.String()
	}
	runOK("register", "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runOK("report", "--asset-id", "EQ-1", "--description", "卡纸", "--request-id", "req-1")

	// 参数错误退出 2：缺数量、缺说明、缺备件编号、缺工单编号。
	for _, args := range [][]string{
		{"issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--note", "更换"},
		{"issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--qty", "1"},
		{"issue", "--ticket-id", "T0001", "--qty", "1", "--note", "更换"},
		{"issue", "--part-no", "SP-1", "--qty", "1", "--note", "更换"},
		{"issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--qty", "0", "--note", "更换"},
		{"return", "--issue-id", "P0001", "--reason", "剩余"},
		{"return", "--qty", "1", "--reason", "剩余"},
		{"return", "--issue-id", "P0001", "--qty", "1"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(append(args, "--data-dir", dir), &out, &errOut); code != 2 {
			t.Fatalf("%v 应退出 2，得到 %d（%s）", args, code, errOut.String())
		}
	}
	// 业务失败退出 1：未知工单领用、未知领用编号退回。
	for _, args := range [][]string{
		{"issue", "--ticket-id", "T9999", "--part-no", "SP-1", "--qty", "1", "--note", "更换"},
		{"return", "--issue-id", "P0001", "--qty", "1", "--reason", "剩余"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(append(args, "--data-dir", dir), &out, &errOut); code != 1 {
			t.Fatalf("%v 应退出 1，得到 %d（%s）", args, code, errOut.String())
		}
	}

	// 成功领用并输出唯一编号；同备件再次领用各自独立。
	o := runOK("issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--qty", "5", "--note", "更换搓纸轮")
	if !strings.Contains(o, "领用编号: P0001") {
		t.Fatalf("领用输出应含 P0001，得到:\n%s", o)
	}
	runOK("issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--qty", "3", "--note", "补仓")
	// 分笔退回并显示累计退回与净量。
	o = runOK("return", "--issue-id", "P0001", "--qty", "2", "--reason", "剩余退回")
	if !strings.Contains(o, "累计退回: 2") || !strings.Contains(o, "净量: 3") {
		t.Fatalf("退回输出应含累计退回与净量，得到:\n%s", o)
	}
	// 超额退回退出 1。
	out.Reset()
	errOut.Reset()
	if code := run([]string{"return", "--issue-id", "P0001", "--qty", "4", "--reason", "超额", "--data-dir", dir}, &out, &errOut); code != 1 {
		t.Fatalf("超额退回应退出 1，得到 %d（%s）", code, errOut.String())
	}

	// ticket 查询：按领用顺序显示各笔，并按备件字典序汇总净量。
	o = runOK("ticket", "--ticket-id", "T0001")
	for _, frag := range []string{
		"备件领用（共 2 笔）",
		"P0001\tSP-1\t原数量 5\t累计退回 2\t净量 3",
		"P0002\tSP-1\t原数量 3\t累计退回 0\t净量 3",
		"按备件汇总净量:",
		"SP-1\t净量 6",
	} {
		if !strings.Contains(o, frag) {
			t.Fatalf("ticket 输出应含 %q，得到:\n%s", frag, o)
		}
	}
	// 无记录明确提示。
	if o := runOK("ticket", "--ticket-id", "T0001"); strings.Contains(o, "备件领用: 无") {
		t.Fatalf("有记录时不应提示无，得到:\n%s", o)
	}
	// history 展示领用与退回履历。
	o = runOK("history", "--asset-id", "EQ-1")
	for _, frag := range []string{
		"备件领用 工单 T0001 领用编号 P0001: 备件 SP-1，数量 5，更换搓纸轮",
		"备件退回 工单 T0001 领用编号 P0001: 备件 SP-1，数量 2，剩余退回",
	} {
		if !strings.Contains(o, frag) {
			t.Fatalf("history 输出应含 %q，得到:\n%s", frag, o)
		}
	}

	// 关闭后 ticket 仍显示保留的净量；另一工单无记录时明确提示。
	runOK("close", "--ticket-id", "T0001", "--repair-result", "已修复")
	o = runOK("ticket", "--ticket-id", "T0001")
	if !strings.Contains(o, "P0001\tSP-1\t原数量 5\t累计退回 2\t净量 3") {
		t.Fatalf("关闭后净量应保留显示，得到:\n%s", o)
	}
	runOK("report", "--asset-id", "EQ-1", "--description", "无法开机", "--request-id", "req-2")
	if o := runOK("ticket", "--ticket-id", "T0002"); !strings.Contains(o, "备件领用: 无") {
		t.Fatalf("无记录应明确提示，得到:\n%s", o)
	}
	// 终结后领用、退回退出 1。
	out.Reset()
	errOut.Reset()
	if code := run([]string{"issue", "--ticket-id", "T0001", "--part-no", "SP-1", "--qty", "1", "--note", "再领", "--data-dir", dir}, &out, &errOut); code != 1 {
		t.Fatalf("终结后领用应退出 1，得到 %d（%s）", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"return", "--issue-id", "P0001", "--qty", "1", "--reason", "再退", "--data-dir", dir}, &out, &errOut); code != 1 {
		t.Fatalf("终结后退回应退出 1，得到 %d（%s）", code, errOut.String())
	}
}

// 领用编号容量：计数器耗尽时拒绝领用，不消耗编号，原文件不变。
func TestPartIssueCapacityExhausted(t *testing.T) {
	dir := t.TempDir()
	ledger := `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [{"seq":1,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"}],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "next_ticket_seq": 2,
  "next_issue_seq": %d
}`
	raw := []byte(fmt.Sprintf(ledger, math.MaxInt))
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s := newStoreAt(t, dir)
	if _, err := s.issuePart("T0001", "SP-1", 1, "容量耗尽"); !errors.Is(err, errConflict) {
		t.Fatalf("编号容量耗尽应冲突拒绝，得到 %v", err)
	}
	if string(readFileBytes(t, dir)) != string(raw) {
		t.Fatal("容量耗尽拒绝不应改动原文件")
	}
}
