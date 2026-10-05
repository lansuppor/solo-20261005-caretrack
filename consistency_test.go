package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseLedger 是一份合法但编号与履历序号都有间隔的旧格式台账：
// 工单 T0001 未关闭，下一工单序号 5，履历序号 3。
const baseLedger = `{
  "version": 1,
  "assets": [{"id":"EQ-1","name":"打印机","location":"一楼","status":"维修中"}],
  "tickets": [{"id":"T0001","asset_id":"EQ-1","description":"卡纸","request_id":"req-1","status":"未关闭","created_at":"2026-10-01T08:00:00Z"}],
  "events": [{"seq":3,"asset_id":"EQ-1","ticket_id":"T0001","kind":"报修","content":"卡纸","time":"2026-10-01T08:00:00Z"}],
  "requests": [{"request_id":"req-1","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0001"}],
  "next_ticket_seq": 5
}`

// writeLedger 把 baseLedger 经 mutate 修改后写入数据目录，返回写入的字节。
func writeLedger(t *testing.T, dir string, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(baseLedger), &m); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func openTicketJSON(id, req string) map[string]any {
	return map[string]any{
		"id": id, "asset_id": "EQ-1", "description": "卡纸",
		"request_id": req, "status": "未关闭", "created_at": "2026-10-01T09:00:00Z",
	}
}

func eventJSONMap(seq int, ticketID, kind, content string) map[string]any {
	return map[string]any{
		"seq": seq, "asset_id": "EQ-1", "ticket_id": ticketID,
		"kind": kind, "content": content, "time": "2026-10-01T10:00:00Z",
	}
}

func appendTo(m map[string]any, key string, v any) {
	m[key] = append(m[key].([]any), v)
}

// cancelLedger 把 baseLedger 中的 T0001 改为合法的已取消终态（含取消履历、资产可用）。
func cancelLedger(m map[string]any) {
	tk := m["tickets"].([]any)[0].(map[string]any)
	tk["status"] = "已取消"
	tk["cancel_reason"] = "误报，设备实际正常"
	tk["cancelled_at"] = "2026-10-01T11:00:00Z"
	m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
	appendTo(m, "events", map[string]any{
		"seq": 4, "asset_id": "EQ-1", "ticket_id": "T0001",
		"kind": "取消", "content": "误报，设备实际正常", "time": "2026-10-01T11:00:00Z",
	})
}

// 有效旧库（编号与履历序号有间隔）应继续加载，编号从计数器延续，去重保持。
func TestValidLegacyStoreWithGaps(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, nil)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧库应能加载: %v", err)
	}
	// 去重：相同请求+资产+描述返回原工单。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" {
		t.Fatalf("重放应返回 T0001: %v replay=%v err=%v", old, replay, err)
	}
	// 新报修前先把当前修改保存，关闭旧单后开新单，编号应从计数器 5 继续，不补间隔。
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatal(err)
	}
	tk2, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay || tk2.ID != "T0005" {
		t.Fatalf("下一工单应为 T0005: %v replay=%v err=%v", tk2, replay, err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	// 重载后查询结果、编号延续与去重保持。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.openTicketOf("EQ-1"); got == nil || got.ID != "T0005" {
		t.Fatalf("重载后未关闭工单应为 T0005，得到 %v", got)
	}
	old, replay, err = s2.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Status != ticketClosed {
		t.Fatalf("重载后重放旧请求应返回已关闭的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if got := s2.openTicketOf("EQ-1"); got == nil || got.ID != "T0005" {
		t.Fatal("重放旧请求不应影响新工单")
	}
	if _, _, err := s2.report("EQ-1", "卡纸", "req-9"); err == nil {
		t.Fatal("已有未关闭工单时新报修应失败")
	}
}

// 各类相互矛盾的台账：查询与写入都必须拒绝，指出问题类别，原文件保留。
func TestContradictoryLedgersRejected(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"下一序号等于已用序号", func(m map[string]any) { m["next_ticket_seq"] = 1 }, "计数器矛盾"},
		{"下一序号为零", func(m map[string]any) { m["next_ticket_seq"] = 0 }, "计数器矛盾"},
		{"下一序号为负", func(m map[string]any) { m["next_ticket_seq"] = -3 }, "计数器矛盾"},
		{"工单编号未补零", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["id"] = "T001"
			m["events"].([]any)[0].(map[string]any)["ticket_id"] = "T001"
			m["requests"].([]any)[0].(map[string]any)["ticket_id"] = "T001"
		}, "计数器矛盾"},
		{"工单编号序号为零", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["id"] = "T0000"
			m["events"].([]any)[0].(map[string]any)["ticket_id"] = "T0000"
			m["requests"].([]any)[0].(map[string]any)["ticket_id"] = "T0000"
		}, "计数器矛盾"},
		{"工单缺少请求绑定", func(m map[string]any) { m["requests"] = []any{} }, "请求绑定矛盾"},
		{"绑定指向不存在的工单", func(m map[string]any) {
			m["requests"].([]any)[0].(map[string]any)["ticket_id"] = "T0002"
		}, "请求绑定矛盾"},
		{"绑定描述与工单不一致", func(m map[string]any) {
			m["requests"].([]any)[0].(map[string]any)["description"] = "别的故障"
		}, "请求绑定矛盾"},
		{"绑定请求标识与工单不一致", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["request_id"] = "req-2"
		}, "请求绑定矛盾"},
		{"同一请求标识绑定两次", func(m map[string]any) {
			appendTo(m, "requests", map[string]any{
				"request_id": "req-1", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001",
			})
		}, "请求绑定矛盾"},
		{"有未关闭工单但资产可用", func(m map[string]any) {
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		}, "状态矛盾"},
		{"无未关闭工单但资产维修中", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			appendTo(m, "events", eventJSONMap(4, "T0001", "关闭", "已修复"))
		}, "状态矛盾"},
		{"同一资产两张未关闭工单", func(m map[string]any) {
			appendTo(m, "tickets", openTicketJSON("T0002", "req-2"))
			appendTo(m, "requests", map[string]any{
				"request_id": "req-2", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0002",
			})
			appendTo(m, "events", eventJSONMap(4, "T0002", "报修", "卡纸"))
		}, "状态矛盾"},
		{"未关闭工单带有维修结果", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["result"] = "已修复"
		}, "状态矛盾"},
		{"已关闭工单缺少维修结果", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			appendTo(m, "events", eventJSONMap(4, "T0001", "关闭", "已修复"))
		}, "状态矛盾"},
		{"工单缺少报修履历", func(m map[string]any) { m["events"] = []any{} }, "履历矛盾"},
		{"已关闭工单缺少关闭履历", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		}, "履历矛盾"},
		{"未关闭工单有关闭履历", func(m map[string]any) {
			appendTo(m, "events", eventJSONMap(4, "T0001", "关闭", "已修复"))
		}, "履历矛盾"},
		{"关闭履历早于报修履历", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
			appendTo(m, "events", eventJSONMap(2, "T0001", "关闭", "已修复"))
		}, "履历矛盾"},
		{"履历序号重复", func(m map[string]any) {
			appendTo(m, "events", eventJSONMap(3, "T0001", "报修", "卡纸"))
		}, "履历矛盾"},
		{"履历序号非正", func(m map[string]any) {
			m["events"].([]any)[0].(map[string]any)["seq"] = 0
		}, "履历矛盾"},
		{"报修履历内容与描述不一致", func(m map[string]any) {
			m["events"].([]any)[0].(map[string]any)["content"] = "别的故障"
		}, "履历矛盾"},
		{"关闭履历内容与维修结果不一致", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
			appendTo(m, "events", eventJSONMap(4, "T0001", "关闭", "别的方法"))
		}, "履历矛盾"},
		{"履历资产与工单归属不一致", func(m map[string]any) {
			appendTo(m, "assets", map[string]any{
				"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
			})
			m["events"].([]any)[0].(map[string]any)["asset_id"] = "EQ-2"
		}, "履历矛盾"},
		{"上一张未关闭就产生下一张报修", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已关闭"
			tk["result"] = "已修复"
			tk["closed_at"] = "2026-10-01T11:00:00Z"
			appendTo(m, "tickets", openTicketJSON("T0002", "req-2"))
			appendTo(m, "requests", map[string]any{
				"request_id": "req-2", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0002",
			})
			// T0002 的报修（序号 4）早于 T0001 的关闭（序号 5）。
			appendTo(m, "events", eventJSONMap(4, "T0002", "报修", "卡纸"))
			appendTo(m, "events", eventJSONMap(5, "T0001", "关闭", "已修复"))
		}, "履历矛盾"},
		{"工单描述为空", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["description"] = ""
		}, "数据矛盾"},
		{"已取消工单缺少取消理由", func(m map[string]any) {
			cancelLedger(m)
			delete(m["tickets"].([]any)[0].(map[string]any), "cancel_reason")
		}, "状态矛盾"},
		{"已取消工单带有维修结果", func(m map[string]any) {
			cancelLedger(m)
			m["tickets"].([]any)[0].(map[string]any)["result"] = "已修复"
		}, "状态矛盾"},
		{"未取消工单带有取消理由", func(m map[string]any) {
			m["tickets"].([]any)[0].(map[string]any)["cancel_reason"] = "误报"
		}, "状态矛盾"},
		{"已取消工单仍占用资产", func(m map[string]any) {
			cancelLedger(m)
			m["assets"].([]any)[0].(map[string]any)["status"] = "维修中"
		}, "状态矛盾"},
		{"已取消工单缺少取消履历", func(m map[string]any) {
			tk := m["tickets"].([]any)[0].(map[string]any)
			tk["status"] = "已取消"
			tk["cancel_reason"] = "误报"
			tk["cancelled_at"] = "2026-10-01T11:00:00Z"
			m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		}, "履历矛盾"},
		{"已取消工单同时有关闭履历", func(m map[string]any) {
			cancelLedger(m)
			appendTo(m, "events", eventJSONMap(5, "T0001", "关闭", "已修复"))
		}, "履历矛盾"},
		{"未关闭工单有取消履历", func(m map[string]any) {
			appendTo(m, "events", eventJSONMap(4, "T0001", "取消", "误报"))
		}, "履历矛盾"},
		{"取消履历内容与取消理由不一致", func(m map[string]any) {
			cancelLedger(m)
			m["events"].([]any)[1].(map[string]any)["content"] = "别的理由"
		}, "履历矛盾"},
		{"取消履历早于报修履历", func(m map[string]any) {
			cancelLedger(m)
			m["events"].([]any)[1].(map[string]any)["seq"] = 2
		}, "履历矛盾"},
		{"取消履历重复", func(m map[string]any) {
			cancelLedger(m)
			appendTo(m, "events", eventJSONMap(5, "T0001", "取消", "误报，设备实际正常"))
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
			// 查询命令也应失败，退出码 1。
			var out, errBuf bytes.Buffer
			if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 1 {
				t.Fatalf("矛盾台账上 list 退出码 = %d，应为 1（%s）", code, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), tc.wantErr) {
				t.Fatalf("list 错误应指出 %q，得到 %s", tc.wantErr, errBuf.String())
			}
			// 原文件字节保留。
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("原文件应保留不变: %v", err)
			}
		})
	}
}

// 矛盾台账上写命令同样失败，且不留下任何业务变化。
func TestContradictoryLedgerRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) { m["next_ticket_seq"] = 1 })
	for _, args := range [][]string{
		{"report", "--data-dir", dir, "--asset-id", "EQ-1", "--description", "新故障", "--request-id", "req-9"},
		{"close", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "已修复"},
		{"register", "--data-dir", dir, "--asset-id", "EQ-2", "--name", "空调", "--location", "二楼"},
	} {
		var out, errBuf bytes.Buffer
		if code := run(args, &out, &errBuf); code != 1 {
			t.Fatalf("%v 退出码 = %d，应为 1", args[:1], code)
		}
		if !strings.Contains(errBuf.String(), "计数器矛盾") {
			t.Fatalf("%v 应指出计数器矛盾，得到 %s", args[:1], errBuf.String())
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的写命令不应改动原文件")
	}
}

// 写入失败（目录不可写）不留下部分业务变化，不消耗工单编号与请求绑定；
// 恢复写入条件后可重新提交。
func TestSaveFailureLeavesNoPartialChange(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	saveErr := s.save()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if saveErr == nil {
		t.Skip("当前环境忽略目录写权限，无法模拟写入失败")
	}
	// 原文件字节不变。
	after, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("写入失败不应改动原文件")
	}
	// 重载后看不到部分变化：无工单、无绑定，编号未消耗。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.data.Tickets) != 0 || len(s2.data.Requests) != 0 {
		t.Fatal("写入失败不应留下部分业务变化")
	}
	tk, replay, err := s2.report("EQ-1", "卡纸", "req-1")
	if err != nil || replay || tk.ID != "T0001" {
		t.Fatalf("恢复写入条件后应能重新提交并得到 T0001: %v replay=%v err=%v", tk, replay, err)
	}
	if err := s2.save(); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatalf("重新提交后应存在未关闭工单 T0001，得到 %v", got)
	}
}

// 保存前校验待提交数据：内存中被破坏的数据不得写入，原文件保持不变。
func TestSaveValidatesPendingData(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, dataFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 破坏计数器：下一序号回退到已用序号。
	s.data.NextTicketSeq = 1
	if err := s.save(); err == nil || !strings.Contains(err.Error(), "计数器矛盾") {
		t.Fatalf("待提交数据矛盾时保存应失败，得到 %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("校验失败不应写入任何数据")
	}
	// 破坏请求绑定：删除绑定记录。
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.data.Requests = []requestBinding{}
	if err := s2.save(); err == nil || !strings.Contains(err.Error(), "请求绑定矛盾") {
		t.Fatalf("缺少绑定时保存应失败，得到 %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("校验失败不应写入任何数据")
	}
	// 原数据仍可正常加载与查询。
	s3, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatalf("原台账应保持有效，得到 %v", got)
	}
}

// 编号耗尽时拒绝新报修，但有效台账仍可查询和关闭已有工单。
func TestTicketNumberExhaustion(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		m["next_ticket_seq"] = math.MaxInt
		appendTo(m, "assets", map[string]any{
			"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用",
		})
	})

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("编号耗尽的合法台账应能加载: %v", err)
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatalf("既有请求重放不应受编号耗尽影响: %v", err)
	}
	if _, _, err := s.report("EQ-2", "不制冷", "req-2"); err == nil ||
		!strings.Contains(err.Error(), "耗尽") {
		t.Fatalf("编号耗尽时应拒绝新报修，得到 %v", err)
	}
	// 查询与关闭已有工单不受影响。
	var out, errBuf bytes.Buffer
	if code := run([]string{"list", "--data-dir", dir}, &out, &errBuf); code != 0 {
		t.Fatalf("编号耗尽时 list 应可用: %s", errBuf.String())
	}
	if code := run([]string{"close", "--data-dir", dir, "--ticket-id", "T0001", "--repair-result", "已修复"},
		&out, &errBuf); code != 0 {
		t.Fatalf("编号耗尽时关闭已有工单应可用: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "已关闭") {
		t.Fatalf("关闭输出异常: %s", out.String())
	}
	// 关闭后新报修仍被拒绝（编号依旧耗尽）。
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.report("EQ-1", "新故障", "req-2"); err == nil {
		t.Fatal("关闭工单后编号仍然耗尽，新报修应继续被拒绝")
	}
}

// 履历序号计数器耗尽时同样拒绝追加履历，不消耗工单编号。
func TestEventSeqExhaustion(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) {
		tk := m["tickets"].([]any)[0].(map[string]any)
		tk["status"] = "已关闭"
		tk["result"] = "已修复"
		tk["closed_at"] = "2026-10-01T11:00:00Z"
		m["assets"].([]any)[0].(map[string]any)["status"] = "可用"
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt - 1
		appendTo(m, "events", map[string]any{
			"seq": math.MaxInt, "asset_id": "EQ-1", "ticket_id": "T0001",
			"kind": "关闭", "content": "已修复", "time": "2026-10-01T11:00:00Z",
		})
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if _, _, err := s.report("EQ-1", "新故障", "req-2"); err == nil ||
		!strings.Contains(err.Error(), "履历序号") {
		t.Fatalf("履历序号耗尽应拒绝新报修，得到 %v", err)
	}
	if s.data.NextTicketSeq != 5 || len(s.data.Tickets) != 1 || len(s.data.Requests) != 1 {
		t.Fatal("失败的报修不应消耗工单编号或绑定请求标识")
	}
}

// 含已取消工单的有效旧台账无需转换即可加载：去重重放返回已取消状态，
// 新报修从计数器延续编号，取消后资产可再次报修，重启后状态保持。
func TestValidCancelledLegacyLedger(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, cancelLedger)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("含已取消工单的有效旧库应能加载: %v", err)
	}
	// 重放旧请求：返回原工单的已取消状态，不产生工单或履历，不重新占用资产。
	old, replay, err := s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.ID != "T0001" || old.Status != ticketCancelled {
		t.Fatalf("重放应返回已取消的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 1 || len(s.data.Events) != 2 {
		t.Fatal("重放不应产生工单或履历")
	}
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("重放不应重新占用资产，状态 = %q", got)
	}
	// 新报修从计数器 5 延续编号。
	tk, replay, err := s.report("EQ-1", "无法开机", "req-2")
	if err != nil || replay || tk.ID != "T0005" {
		t.Fatalf("新报修应开出 T0005: %v replay=%v err=%v", tk, replay, err)
	}
	// 旧请求重放不影响新单。
	old, replay, err = s.report("EQ-1", "卡纸", "req-1")
	if err != nil || !replay || old.Status != ticketCancelled {
		t.Fatalf("已有新单时重放仍返回已取消的 T0001: %v replay=%v err=%v", old, replay, err)
	}
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0005" {
		t.Fatalf("新工单 T0005 应仍为未关闭，得到 %v", got)
	}
	// 取消新工单并保存重开：取消状态、理由、履历与去重结果保持。
	if _, _, err := s.cancelTicket("T0005", "用户自行解决"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.findTicket("T0005")
	if got.Status != ticketCancelled || got.CancelReason != "用户自行解决" || got.CancelledAt == "" {
		t.Fatalf("重开后 T0005 取消状态未保持: %+v", got)
	}
	if got := s2.findTicket("T0001"); got.Status != ticketCancelled ||
		got.CancelReason != "误报，设备实际正常" {
		t.Fatalf("重开后 T0001 取消状态未保持: %+v", got)
	}
	events := s2.eventsOf("EQ-1")
	if len(events) != 4 || events[0].Kind != eventReport || events[1].Kind != eventCancel ||
		events[2].Kind != eventReport || events[3].Kind != eventCancel {
		t.Fatalf("重开后履历不对: %+v", events)
	}
	if events[1].Content != "误报，设备实际正常" || events[3].Content != "用户自行解决" {
		t.Fatalf("取消履历内容应为取消理由: %+v", events)
	}
	// 同标识搭配不同资产或描述仍被拒绝，取消不释放旧请求标识。
	if _, _, err := s2.report("EQ-1", "别的故障", "req-1"); err == nil {
		t.Fatal("同标识不同描述应被拒绝")
	}
}

// 工单编号耗尽时仍可取消已有工单。
func TestTicketNumberExhaustionAllowsCancel(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, func(m map[string]any) { m["next_ticket_seq"] = math.MaxInt })

	var out, errBuf bytes.Buffer
	code := run([]string{"cancel", "--data-dir", dir, "--ticket-id", "T0001", "--reason", "误报"},
		&out, &errBuf)
	if code != 0 {
		t.Fatalf("编号耗尽时取消已有工单应可用: %s", errBuf.String())
	}
	if !strings.Contains(out.String(), "T0001") || !strings.Contains(out.String(), "已取消") {
		t.Fatalf("取消输出应包含原工单编号与已取消状态: %s", out.String())
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.findTicket("T0001"); got.Status != ticketCancelled {
		t.Fatalf("取消后状态 = %q", got.Status)
	}
	// 取消后新报修仍因编号耗尽被拒绝。
	if _, _, err := s.report("EQ-1", "新故障", "req-2"); err == nil {
		t.Fatal("编号耗尽时新报修应继续被拒绝")
	}
}

// 履历序号耗尽时取消失败，不留下部分变化，原文件字节不变。
func TestEventSeqExhaustionRejectsCancel(t *testing.T) {
	dir := t.TempDir()
	raw := writeLedger(t, dir, func(m map[string]any) {
		m["events"].([]any)[0].(map[string]any)["seq"] = math.MaxInt
	})
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("合法台账应能加载: %v", err)
	}
	if _, _, err := s.cancelTicket("T0001", "误报"); err == nil ||
		!strings.Contains(err.Error(), "履历序号") {
		t.Fatalf("履历序号耗尽应拒绝取消，得到 %v", err)
	}
	if got := s.findTicket("T0001"); got.Status != ticketOpen {
		t.Fatal("失败的取消不应改动工单状态")
	}
	if got := s.findAsset("EQ-1").Status; got != statusRepairing {
		t.Fatal("失败的取消不应改动资产状态")
	}
	got, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("失败的取消不应改动原文件")
	}
}

// 报修计数器检查应先于任何数据修改：失败路径不绑定请求标识。
func TestFailedReportBindsNothing(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.report("NOPE", "卡纸", "req-x"); err == nil {
		t.Fatal("未知资产报修应失败")
	}
	if s.findRequest("req-x") != nil {
		t.Fatal("失败的报修不应绑定请求标识")
	}
	if _, _, err := s.report("EQ-1", "卡纸", "req-x"); err != nil {
		t.Fatalf("同一标识应可用于后续成功报修: %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.data.Requests); got != 1 {
		t.Fatalf("重载后应只有一条绑定，得到 %d", got)
	}
}
