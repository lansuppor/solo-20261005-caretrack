package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 延期完成的撤销：下一到期日恢复为该完成的周期到期日，跨过的周期不补记录；
// detail、due 反映回退，重启后保持。
func TestRevokeDelayedCompletionRestoresCycle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	// 延期到 01-25 完成，跨过 01-11、01-21 两个周期，下一到期日 01-31。
	_, next, doneSeq, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-25", "已更换")
	if err != nil || next != "2026-01-31" {
		t.Fatalf("完成登记: next=%q err=%v", next, err)
	}
	// 撤销：下一到期日恢复为该完成的周期到期日 01-01，而不是中间跨过的周期。
	p, target, err := s.revokeCompletion("EQ-1", doneSeq, "误登记")
	if err != nil {
		t.Fatalf("revokeCompletion: %v", err)
	}
	if p.NextDue != "2026-01-01" || target.Due != "2026-01-01" {
		t.Fatalf("恢复下一到期日 = %q，想得到 2026-01-01", p.NextDue)
	}
	// 原完成的日期、结果与时间保留；追加一条撤销履历；跨过的周期不补记录。
	events := s.eventsOf("EQ-1")
	if len(events) != 3 || events[0].Kind != eventPlanCreate ||
		events[1].Kind != eventPlanDone || events[2].Kind != eventPlanRevoke {
		t.Fatalf("履历不对: %+v", events)
	}
	done := events[1]
	if done.Due != "2026-01-01" || done.Done != "2026-01-25" || done.Content != "已更换" || done.Time.IsZero() {
		t.Fatalf("原完成履历应保留日期、结果与时间: %+v", done)
	}
	rev := events[2]
	if rev.TargetSeq != doneSeq || rev.Content != "误登记" || rev.Time.IsZero() ||
		rev.Due != "" || rev.Done != "" || rev.Interval != 0 {
		t.Fatalf("撤销履历不对: %+v", rev)
	}
	if !rev.Time.After(done.Time) && !rev.Time.Equal(done.Time) {
		t.Fatal("撤销时间不应早于完成时间")
	}

	// 重启后保持；detail、due 反映回退。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重开后下一到期日 = %q", got.NextDue)
	}
	var buf bytes.Buffer
	if err := cmdDetail([]string{"--data-dir", s.dir, "--asset-id", "EQ-1"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "下一到期日: 2026-01-01") {
		t.Fatalf("detail 应反映回退:\n%s", buf.String())
	}
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", s.dir, "--date", "2026-01-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "EQ-1") {
		t.Fatalf("due 应包含已回退的计划:\n%s", buf.String())
	}
	// 查询只读：不初始化目录。
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	buf.Reset()
	if err := cmdDue([]string{"--data-dir", missing, "--date", "2026-01-01"}, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("查询不应初始化目录: %v", err)
	}
}

// 仅可撤销按序号最新的未撤销完成；撤销后可继续撤销此前最新有效完成；
// 未知资产或序号、目标不是该资产的完成、重复撤销均失败；维修或其他资产
// 事件不阻止撤销。
func TestRevokeChainLatestOnly(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-2", "空调", "二楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-2", "清洗", "2026-03-01", 30); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	// 维修事件与其他资产的保养事件穿插，不阻止撤销。
	tk, _, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.completePlan("EQ-2", "2026-03-01", "2026-03-02", "其他资产"); err != nil {
		t.Fatal(err)
	}
	_, _, seq2, err := s.completePlan("EQ-1", "2026-01-11", "2026-01-12", "第二次")
	if err != nil {
		t.Fatal(err)
	}

	// 存在更晚有效完成时，撤销较早完成拒绝。
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "误登记"); !errors.Is(err, errConflict) {
		t.Fatalf("存在更晚有效完成时应拒绝，得到 %v", err)
	}
	// 未知资产、未知序号、目标不是该资产的完成（建立履历、报修履历、其他资产的完成）。
	if _, _, err := s.revokeCompletion("NOPE", seq2, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知资产应失败，得到 %v", err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", 9999, "理由"); !errors.Is(err, errNotFound) {
		t.Fatalf("未知序号应失败，得到 %v", err)
	}
	createSeq := s.eventsOf("EQ-1")[0].Seq
	reportSeq := 0
	otherDoneSeq := 0
	for _, e := range s.data.Events {
		switch {
		case e.Kind == eventReport:
			reportSeq = e.Seq
		case e.Kind == eventPlanDone && e.AssetID == "EQ-2":
			otherDoneSeq = e.Seq
		}
	}
	for _, bad := range []int{createSeq, reportSeq, otherDoneSeq} {
		if _, _, err := s.revokeCompletion("EQ-1", bad, "理由"); !errors.Is(err, errConflict) {
			t.Fatalf("目标序号 %d 不是该资产的完成，应拒绝，得到 %v", bad, err)
		}
	}
	// 空理由拒绝。
	if _, _, err := s.revokeCompletion("EQ-1", seq2, ""); !errors.Is(err, errConflict) {
		t.Fatalf("空理由应拒绝，得到 %v", err)
	}
	// 失败路径不产生履历、不推进日期。
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-21" {
		t.Fatalf("失败的撤销不应改动下一到期日，得到 %q", got.NextDue)
	}

	// 维修中撤销最新完成：成功，下一到期日恢复；工单与资产状态不变。
	if _, _, err := s.revokeCompletion("EQ-1", seq2, "误登记"); err != nil {
		t.Fatalf("撤销最新完成: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("撤销后下一到期日 = %q，想得到 2026-01-11", got.NextDue)
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatalf("撤销不应改变资产状态，得到 %q", got)
	}
	if got := s.findTicket(tk.ID); got.Status != ticketOpen {
		t.Fatalf("撤销不应终结工单，得到 %q", got.Status)
	}
	// 重复撤销同一完成拒绝。
	if _, _, err := s.revokeCompletion("EQ-1", seq2, "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重复撤销应拒绝，得到 %v", err)
	}
	// 可继续撤销此前最新有效完成。
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "继续撤销"); err != nil {
		t.Fatalf("连续撤销: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("连续撤销后下一到期日 = %q，想得到 2026-01-01", got.NextDue)
	}
	// 重启后撤销状态保持：两个完成均已撤销，再次撤销仍拒绝。
	s = saveAndReopen(t, s)
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("重开后重复撤销应拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重开后下一到期日 = %q", got.NextDue)
	}
}

// 撤销后恢复的周期可按原规则重新完成，生成新序号；旧序号的再次撤销仍拒绝，
// 不能误撤销新登记。
func TestRevokeThenRecompleteSameCycle(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-05", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "误登记"); err != nil {
		t.Fatal(err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("撤销后下一到期日 = %q", got.NextDue)
	}
	// 同一周期按原 maintain 规则重新完成，生成新序号。
	_, next, seq2, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-02", "重新登记")
	if err != nil || next != "2026-01-11" {
		t.Fatalf("重新完成: next=%q err=%v", next, err)
	}
	if seq2 == seq1 {
		t.Fatal("重新完成应生成新序号")
	}
	// 旧序号的再次撤销仍拒绝（已撤销），不会误撤销新登记。
	if _, _, err := s.revokeCompletion("EQ-1", seq1, "再次"); !errors.Is(err, errConflict) {
		t.Fatalf("旧序号再次撤销应拒绝，得到 %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("旧序号撤销不应影响新登记，下一到期日 = %q", got.NextDue)
	}
	// 新登记可用新序号正常撤销。
	if _, _, err := s.revokeCompletion("EQ-1", seq2, "又登错了"); err != nil {
		t.Fatalf("新序号撤销: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("撤销新登记后下一到期日 = %q", got.NextDue)
	}
	// 重启后保持。
	s = saveAndReopen(t, s)
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重开后下一到期日 = %q", got.NextDue)
	}
}

// 导入复制完成与撤销履历：撤销引用随履历重编号同步替换，输出完成序号的
// 原、新映射；撤销状态与下一到期日保持，导入后可用新序号继续撤销；源只读。
func TestImportRemapsRevokeReferences(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src := newStoreAt(t, srcDir)
	if _, err := src.registerAsset("EQ-A", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.createPlan("EQ-A", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := src.completePlan("EQ-A", "2026-01-01", "2026-01-01", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	_, _, seq2, err := src.completePlan("EQ-A", "2026-01-11", "2026-01-11", "第二次")
	if err != nil {
		t.Fatal(err)
	}
	// 撤销最新完成：下一到期日回到 2026-01-11。
	if _, _, err := src.revokeCompletion("EQ-A", seq2, "误登记"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, src)

	// 目标已有履历，导入履历在其后重编号。
	dst := newStoreAt(t, dstDir)
	if _, err := dst.registerAsset("EQ-X", "叉车", "仓库"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dst.report("EQ-X", "异响", "req-x"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, dst)

	srcBefore := readFileBytes(t, srcDir)
	outcome, err := importAssets(dstDir, srcDir, []string{"EQ-A"})
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if got := readFileBytes(t, srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("源台账应保持只读不变")
	}
	// 完成序号的原、新映射：两条完成，按源顺序重编号。
	if len(outcome.completions) != 2 ||
		outcome.completions[0].OldSeq != seq1 || outcome.completions[1].OldSeq != seq2 {
		t.Fatalf("完成序号映射不对: %+v", outcome.completions)
	}
	newSeq1 := outcome.completions[0].NewSeq
	newSeq2 := outcome.completions[1].NewSeq
	if newSeq1 == seq1 && newSeq2 == seq2 {
		t.Fatal("目标已有履历时完成序号应重新编号")
	}

	re, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	// 撤销状态与下一到期日保持；撤销引用已替换为新序号。
	if got := re.findPlan("EQ-A"); got == nil || got.NextDue != "2026-01-11" {
		t.Fatalf("导入后下一到期日应保持，得到 %+v", got)
	}
	var revokes int
	for _, e := range re.eventsOf("EQ-A") {
		if e.Kind == eventPlanRevoke {
			revokes++
			if e.TargetSeq != newSeq2 {
				t.Fatalf("撤销引用应替换为新序号 %d，得到 %d", newSeq2, e.TargetSeq)
			}
		}
	}
	if revokes != 1 {
		t.Fatalf("应导入 1 条撤销履历，得到 %d", revokes)
	}
	// 导入后可用新序号继续撤销此前最新有效完成；旧序号（指向目标既有履历或
	// 已撤销完成）不能误用。
	if _, _, err := re.revokeCompletion("EQ-A", newSeq1, "继续撤销"); err != nil {
		t.Fatalf("导入后用新序号撤销: %v", err)
	}
	if got := re.findPlan("EQ-A"); got.NextDue != "2026-01-01" {
		t.Fatalf("继续撤销后下一到期日 = %q", got.NextDue)
	}
	mustSave(t, re)
	// 重启后保持。
	re2, err := openStore(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := re2.findPlan("EQ-A"); got.NextDue != "2026-01-01" {
		t.Fatalf("重启后下一到期日 = %q", got.NextDue)
	}
}

// 矛盾撤销数据（目标未知或更晚、非完成、其他资产、重复撤销、非最新、
// 撤销在建立之前、携带保养日期字段、推出日期与保存不符）加载即报错，
// 不自动修复，原文件保留。
func TestRevokeConsistencyRejected(t *testing.T) {
	// 合法基础：计划 + 建立（seq 4）+ 两次完成（seq 5、6）。
	base := func(m map[string]any) {
		m["plans"] = []any{map[string]any{
			"asset_id": "EQ-1", "content": "更换滤芯",
			"first_due": "2026-01-01", "interval_days": 10, "next_due": "2026-01-21",
		}}
		appendTo(m, "events", map[string]any{
			"seq": 4, "asset_id": "EQ-1", "kind": "保养建立", "content": "更换滤芯",
			"due": "2026-01-01", "interval": 10, "time": "2026-10-01T12:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 5, "asset_id": "EQ-1", "kind": "保养完成", "content": "第一次",
			"due": "2026-01-01", "done": "2026-01-01", "time": "2026-10-01T13:00:00Z",
		})
		appendTo(m, "events", map[string]any{
			"seq": 6, "asset_id": "EQ-1", "kind": "保养完成", "content": "第二次",
			"due": "2026-01-11", "done": "2026-01-11", "time": "2026-10-01T14:00:00Z",
		})
	}
	// 合法撤销：撤销 seq 6，下一到期日恢复为 2026-01-11。
	legalRevoke := func(m map[string]any) {
		base(m)
		m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-01-11"
		appendTo(m, "events", map[string]any{
			"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
			"target_seq": 6, "time": "2026-10-01T15:00:00Z",
		})
	}
	cases := map[string]func(m map[string]any){
		"目标序号不存在": func(m map[string]any) {
			base(m)
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 99, "time": "2026-10-01T15:00:00Z",
			})
		},
		"目标为更晚的完成": func(m map[string]any) {
			base(m)
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-01-11"
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 8, "time": "2026-10-01T15:00:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 8, "asset_id": "EQ-1", "kind": "保养完成", "content": "第三次",
				"due": "2026-01-11", "done": "2026-01-11", "time": "2026-10-01T16:00:00Z",
			})
		},
		"目标不是完成履历": func(m map[string]any) {
			base(m)
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 4, "time": "2026-10-01T15:00:00Z",
			})
		},
		"目标为其他资产的完成": func(m map[string]any) {
			base(m)
			appendTo(m, "assets", map[string]any{
				"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
			})
			m["plans"] = append(m["plans"].([]any), map[string]any{
				"asset_id": "EQ-2", "content": "清洗",
				"first_due": "2026-03-01", "interval_days": 30, "next_due": "2026-03-31",
			})
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-2", "kind": "保养建立", "content": "清洗",
				"due": "2026-03-01", "interval": 30, "time": "2026-10-01T15:00:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 8, "asset_id": "EQ-2", "kind": "保养完成", "content": "完成",
				"due": "2026-03-01", "done": "2026-03-01", "time": "2026-10-01T16:00:00Z",
			})
			appendTo(m, "events", map[string]any{
				"seq": 9, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 8, "time": "2026-10-01T17:00:00Z",
			})
		},
		"重复撤销同一完成": func(m map[string]any) {
			legalRevoke(m)
			appendTo(m, "events", map[string]any{
				"seq": 8, "asset_id": "EQ-1", "kind": "保养撤销", "content": "再次",
				"target_seq": 6, "time": "2026-10-01T16:00:00Z",
			})
		},
		"撤销非最新有效完成": func(m map[string]any) {
			base(m)
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 5, "time": "2026-10-01T15:00:00Z",
			})
		},
		"撤销在建立之前": func(m map[string]any) {
			m["plans"] = []any{map[string]any{
				"asset_id": "EQ-1", "content": "更换滤芯",
				"first_due": "2026-01-01", "interval_days": 10, "next_due": "2026-01-01",
			}}
			appendTo(m, "events", map[string]any{
				"seq": 4, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 5, "time": "2026-10-01T12:00:00Z",
			})
		},
		"撤销携带来路不明的日期": func(m map[string]any) {
			legalRevoke(m)
			m["events"].([]any)[4].(map[string]any)["due"] = "2026-01-11"
		},
		"撤销目标序号非正": func(m map[string]any) {
			base(m)
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 0, "time": "2026-10-01T15:00:00Z",
			})
		},
		"撤销后推出日期与保存不符": func(m map[string]any) {
			base(m)
			// 撤销 seq 6 后下一到期日应为 2026-01-11，却保存 2026-01-21。
			appendTo(m, "events", map[string]any{
				"seq": 7, "asset_id": "EQ-1", "kind": "保养撤销", "content": "误登记",
				"target_seq": 6, "time": "2026-10-01T15:00:00Z",
			})
		},
		"完成后未接续撤销恢复的日期": func(m map[string]any) {
			legalRevoke(m)
			// 撤销后下一到期日为 2026-01-11，却登记了 2026-01-21 的周期。
			m["plans"].([]any)[0].(map[string]any)["next_due"] = "2026-01-31"
			appendTo(m, "events", map[string]any{
				"seq": 8, "asset_id": "EQ-1", "kind": "保养完成", "content": "第三次",
				"due": "2026-01-21", "done": "2026-01-21", "time": "2026-10-01T16:00:00Z",
			})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			raw := writeLedger(t, dir, mutate)
			if _, err := openStore(dir); err == nil {
				t.Fatal("矛盾数据应报错")
			} else if !strings.Contains(err.Error(), "矛盾") {
				t.Fatalf("错误应指出矛盾，得到 %v", err)
			}
			if got := readFileBytes(t, dir); !bytes.Equal(got, raw) {
				t.Fatal("原文件不应被修改")
			}
		})
	}
	// 合法撤销链可以加载：撤销后下一到期日恢复，撤销状态保持。
	dir := t.TempDir()
	writeLedger(t, dir, legalRevoke)
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法撤销数据应能加载: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("下一到期日 = %q，想得到 2026-01-11", got.NextDue)
	}
	// 旧台账的完成用原序号定位，无需转换：可继续撤销 seq 5。
	if _, _, err := s.revokeCompletion("EQ-1", 5, "继续撤销"); err != nil {
		t.Fatalf("旧台账原序号撤销: %v", err)
	}
	if got := s.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("连续撤销后下一到期日 = %q", got.NextDue)
	}
	// 数组乱序、序号间隔、时间不递增仍合法：把履历顺序打乱后重存重载。
	events := s.data.Events
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	if err := s.save(); err != nil {
		t.Fatalf("乱序数组应能保存: %v", err)
	}
	if _, err := openStore(dir); err != nil {
		t.Fatalf("乱序数组应能加载: %v", err)
	}
}

// 撤销为一次原子保存：写入失败保留原文件字节，不留下部分变化、不消耗序号，
// 恢复后可重试。
func TestRevokeSaveFailureAndRetry(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createPlan("EQ-1", "更换滤芯", "2026-01-01", 10); err != nil {
		t.Fatal(err)
	}
	_, _, seq1, err := s.completePlan("EQ-1", "2026-01-01", "2026-01-01", "第一次")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := readFileBytes(t, s.dir)

	if _, _, err := s.revokeCompletion("EQ-1", seq1, "误登记"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s.save()
	if err := os.Chmod(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	// 原文件字节不变。
	if got := readFileBytes(t, s.dir); !bytes.Equal(got, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：下一到期日未回退，完成未撤销。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.findPlan("EQ-1"); got.NextDue != "2026-01-11" {
		t.Fatalf("写入失败不应留下部分变化，下一到期日 = %q", got.NextDue)
	}
	// 恢复后可重试：同一目标序号仍有效，撤销履历序号未消耗。
	p, _, err := s2.revokeCompletion("EQ-1", seq1, "误登记")
	if err != nil {
		t.Fatalf("恢复后重试: %v", err)
	}
	if p.NextDue != "2026-01-01" {
		t.Fatalf("重试后下一到期日 = %q", p.NextDue)
	}
	events := s2.eventsOf("EQ-1")
	if got := events[len(events)-1].Seq; got != seq1+1 {
		t.Fatalf("失败不应消耗履历序号，撤销序号 = %d，想得到 %d", got, seq1+1)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.findPlan("EQ-1"); got.NextDue != "2026-01-01" {
		t.Fatalf("重载后下一到期日 = %q", got.NextDue)
	}
}

// maintain 显示完成履历序号；history 显示各次完成的序号及有效或已撤销状态；
// 参数错误退出码 2，业务失败退出码 1。
func TestUnmaintainCommandAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	runOK := func(args ...string) string {
		t.Helper()
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("%v 应成功: %d %s", args, code, errOut.String())
		}
		return out.String()
	}
	runOK("register", "--data-dir", dir, "--asset-id", "EQ-1", "--name", "打印机", "--location", "一楼")
	runOK("plan", "--data-dir", dir, "--asset-id", "EQ-1", "--content", "更换滤芯",
		"--first-due", "2026-01-01", "--interval-days", "10")
	// maintain 成功显示完成履历的全库序号。
	o := runOK("maintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--due", "2026-01-01", "--done", "2026-01-02", "--result", "已更换")
	if !strings.Contains(o, "完成履历序号: 2") || !strings.Contains(o, "下一到期日: 2026-01-11") {
		t.Fatalf("maintain 输出不对:\n%s", o)
	}
	o = runOK("maintain", "--data-dir", dir, "--asset-id", "EQ-1",
		"--due", "2026-01-11", "--done", "2026-01-11", "--result", "第二次")
	if !strings.Contains(o, "完成履历序号: 3") {
		t.Fatalf("maintain 输出不对:\n%s", o)
	}

	// 参数错误 → 2：缺参数、非正序号。
	for _, args := range [][]string{
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "4"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "-1", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--seq", "4", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("参数错误应退出 2: %v → %d (%s)", args, code, errOut.String())
		}
	}
	// 业务失败 → 1：未知资产、未知序号、非最新完成。
	for _, args := range [][]string{
		{"unmaintain", "--data-dir", dir, "--asset-id", "NOPE", "--seq", "3", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "99", "--reason", "x"},
		{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "2", "--reason", "x"},
	} {
		out.Reset()
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 1 {
			t.Fatalf("业务失败应退出 1: %v → %d", args, code)
		}
	}
	// 成功撤销：输出目标序号与恢复的日期。
	o = runOK("unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "3", "--reason", "误登记")
	if !strings.Contains(o, "序号 3") || !strings.Contains(o, "恢复下一到期日: 2026-01-11") {
		t.Fatalf("unmaintain 输出不对:\n%s", o)
	}
	// 重复撤销 → 1。
	out.Reset()
	errOut.Reset()
	if code := run([]string{"unmaintain", "--data-dir", dir, "--asset-id", "EQ-1", "--seq", "3", "--reason", "x"},
		&out, &errOut); code != 1 {
		t.Fatalf("重复撤销应退出 1，得到 %d", code)
	}

	// history 显示各次完成的序号及有效或已撤销状态，以及撤销履历。
	o = runOK("history", "--data-dir", dir, "--asset-id", "EQ-1")
	for _, want := range []string{"保养完成（序号 2，有效）", "保养完成（序号 3，已撤销）",
		"保养撤销: 目标完成履历序号 3（误登记）"} {
		if !strings.Contains(o, want) {
			t.Fatalf("history 缺少 %q:\n%s", want, o)
		}
	}
	// 重启后结果保持。
	o = runOK("detail", "--data-dir", dir, "--asset-id", "EQ-1")
	if !strings.Contains(o, "下一到期日: 2026-01-11") {
		t.Fatalf("重启后 detail 应保持回退:\n%s", o)
	}
}
