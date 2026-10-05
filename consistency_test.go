package main

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseLedger 返回一份有效旧格式台账：工单编号（T0003、T0007）与履历序号
// （10、20、30）均带间隔，next_ticket_seq 指向 8。
func baseLedger(t *testing.T) map[string]any {
	t.Helper()
	raw := `{
	  "version": 1,
	  "assets": [
	    {"id":"EQ-1","name":"打印机","location":"一楼","status":"可用"},
	    {"id":"EQ-2","name":"空调","location":"二楼","status":"维修中"}
	  ],
	  "tickets": [
	    {"id":"T0003","asset_id":"EQ-1","description":"卡纸","request_id":"req-a",
	     "status":"已关闭","result":"已修复","created_at":"2026-01-01T00:00:00Z","closed_at":"2026-01-02T00:00:00Z"},
	    {"id":"T0007","asset_id":"EQ-2","description":"不制冷","request_id":"req-b",
	     "status":"未关闭","created_at":"2026-01-03T00:00:00Z"}
	  ],
	  "events": [
	    {"seq":10,"asset_id":"EQ-1","ticket_id":"T0003","kind":"报修","content":"卡纸","time":"2026-01-01T00:00:00Z"},
	    {"seq":20,"asset_id":"EQ-1","ticket_id":"T0003","kind":"关闭","content":"已修复","time":"2026-01-02T00:00:00Z"},
	    {"seq":30,"asset_id":"EQ-2","ticket_id":"T0007","kind":"报修","content":"不制冷","time":"2026-01-03T00:00:00Z"}
	  ],
	  "requests": [
	    {"request_id":"req-a","asset_id":"EQ-1","description":"卡纸","ticket_id":"T0003"},
	    {"request_id":"req-b","asset_id":"EQ-2","description":"不制冷","ticket_id":"T0007"}
	  ],
	  "next_ticket_seq": 8
	}`
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("baseLedger 不是有效 JSON: %v", err)
	}
	return m
}

func writeLedger(t *testing.T, dir string, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, dataFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func assets(m map[string]any) []any  { return m["assets"].([]any) }
func tickets(m map[string]any) []any { return m["tickets"].([]any) }
func eventsL(m map[string]any) []any { return m["events"].([]any) }
func reqs(m map[string]any) []any    { return m["requests"].([]any) }

func TestValidLegacyLedgerLoads(t *testing.T) {
	dir := t.TempDir()
	writeLedger(t, dir, baseLedger(t))

	// 带间隔的编号与履历序号的有效旧库应继续加载，查询正常。
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("有效旧库应加载成功: %v", err)
	}
	if got := s.openTicketOf("EQ-2"); got == nil || got.ID != "T0007" {
		t.Fatalf("EQ-2 的未关闭工单应为 T0007，得到 %v", got)
	}
	if evs := s.eventsOf("EQ-1"); len(evs) != 2 || evs[0].Seq != 10 || evs[1].Seq != 20 {
		t.Fatalf("EQ-1 履历不对: %+v", evs)
	}

	// 去重保持：重放已关闭工单的原请求，返回原工单，不新增记录。
	old, replay, err := s.report("EQ-1", "卡纸", "req-a")
	if err != nil || !replay || old.ID != "T0003" || old.Status != ticketClosed {
		t.Fatalf("重放 req-a 应返回已关闭的 T0003: %v replay=%v err=%v", old, replay, err)
	}
	if len(s.data.Tickets) != 2 {
		t.Fatalf("重放不应新增工单，工单数 = %d", len(s.data.Tickets))
	}

	// 编号延续：新报修从 next_ticket_seq 继续，不复用、不跳回。
	tk, replay, err := s.report("EQ-1", "无法开机", "req-c")
	if err != nil || replay || tk.ID != "T0008" {
		t.Fatalf("新报修应开出 T0008: %v replay=%v err=%v", tk, replay, err)
	}

	// 重启后查询结果、编号延续与去重保持。
	s = saveAndReopen(t, s)
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0008" {
		t.Fatalf("重开后 EQ-1 的未关闭工单应为 T0008，得到 %v", got)
	}
	again, replay, err := s.report("EQ-1", "无法开机", "req-c")
	if err != nil || !replay || again.ID != "T0008" {
		t.Fatalf("重开后重放 req-c 应返回 T0008: %v replay=%v err=%v", again, replay, err)
	}
	if _, _, err := s.report("EQ-1", "别的故障", "req-d"); err == nil {
		t.Fatal("已有未关闭工单时应拒绝新报修")
	}
	if evs := s.eventsOf("EQ-1"); len(evs) != 3 {
		t.Fatalf("EQ-1 履历应为 3 条，得到 %d", len(evs))
	}
}

// TestContradictoryLedgerRejected 覆盖计数器、请求绑定、状态、履历四类矛盾：
// 查询与写操作均报错并指出类别、以退出码 1 结束，原文件字节保持不变。
func TestContradictoryLedgerRejected(t *testing.T) {
	addOpenTicket := func(m map[string]any) {
		m["tickets"] = append(tickets(m), map[string]any{
			"id": "T0008", "asset_id": "EQ-2", "description": "又坏了", "request_id": "req-d",
			"status": "未关闭", "created_at": "2026-01-04T00:00:00Z",
		})
		m["requests"] = append(reqs(m), map[string]any{
			"request_id": "req-d", "asset_id": "EQ-2", "description": "又坏了", "ticket_id": "T0008",
		})
		m["events"] = append(eventsL(m), map[string]any{
			"seq": 40, "asset_id": "EQ-2", "ticket_id": "T0008", "kind": "报修",
			"content": "又坏了", "time": "2026-01-04T00:00:00Z",
		})
		m["next_ticket_seq"] = 9
	}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"计数器回退", func(m map[string]any) { m["next_ticket_seq"] = 7 }, "计数器矛盾"},
		{"计数器非正", func(m map[string]any) { m["next_ticket_seq"] = 0 }, "计数器矛盾"},
		{"编号形式不规范", func(m map[string]any) {
			tickets(m)[1].(map[string]any)["id"] = "T7"
			reqs(m)[1].(map[string]any)["ticket_id"] = "T7"
			eventsL(m)[2].(map[string]any)["ticket_id"] = "T7"
		}, "计数器矛盾"},
		{"工单缺少请求绑定", func(m map[string]any) {
			m["requests"] = reqs(m)[:1]
		}, "请求绑定矛盾"},
		{"绑定描述与工单不符", func(m map[string]any) {
			reqs(m)[1].(map[string]any)["description"] = "其他故障"
		}, "请求绑定矛盾"},
		{"旧请求指向另一张工单", func(m map[string]any) {
			reqs(m)[0].(map[string]any)["ticket_id"] = "T0007"
		}, "请求绑定矛盾"},
		{"有未关闭工单但状态可用", func(m map[string]any) {
			assets(m)[1].(map[string]any)["status"] = "可用"
		}, "状态矛盾"},
		{"无未关闭工单但状态维修中", func(m map[string]any) {
			assets(m)[0].(map[string]any)["status"] = "维修中"
		}, "状态矛盾"},
		{"未关闭工单带维修结果", func(m map[string]any) {
			tickets(m)[1].(map[string]any)["result"] = "误填"
		}, "状态矛盾"},
		{"同一资产两张未关闭工单", addOpenTicket, "状态矛盾"},
		{"履历序号重复", func(m map[string]any) {
			eventsL(m)[1].(map[string]any)["seq"] = 10
		}, "履历矛盾"},
		{"履历序号非正", func(m map[string]any) {
			eventsL(m)[0].(map[string]any)["seq"] = 0
		}, "履历矛盾"},
		{"已关闭工单缺关闭履历", func(m map[string]any) {
			m["events"] = []any{eventsL(m)[0], eventsL(m)[2]}
		}, "履历矛盾"},
		{"工单缺报修履历", func(m map[string]any) {
			m["events"] = eventsL(m)[:2]
		}, "履历矛盾"},
		{"未关闭工单有关闭履历", func(m map[string]any) {
			m["events"] = append(eventsL(m), map[string]any{
				"seq": 40, "asset_id": "EQ-2", "ticket_id": "T0007", "kind": "关闭",
				"content": "x", "time": "2026-01-04T00:00:00Z",
			})
		}, "履历矛盾"},
		{"关闭先于报修", func(m map[string]any) {
			eventsL(m)[0].(map[string]any)["seq"] = 25
			eventsL(m)[1].(map[string]any)["seq"] = 15
		}, "履历矛盾"},
		{"报修履历内容与故障描述不符", func(m map[string]any) {
			eventsL(m)[0].(map[string]any)["content"] = "其他"
		}, "履历矛盾"},
		{"关闭履历内容与维修结果不符", func(m map[string]any) {
			eventsL(m)[1].(map[string]any)["content"] = "其他"
		}, "履历矛盾"},
		{"履历资产与工单归属不符", func(m map[string]any) {
			eventsL(m)[0].(map[string]any)["asset_id"] = "EQ-2"
		}, "履历矛盾"},
		{"前单未关闭即产生新报修", func(m map[string]any) {
			m["tickets"] = append(tickets(m), map[string]any{
				"id": "T0008", "asset_id": "EQ-2", "description": "再次不制冷", "request_id": "req-d",
				"status": "已关闭", "result": "换压缩机",
				"created_at": "2026-01-04T00:00:00Z", "closed_at": "2026-01-05T00:00:00Z",
			})
			m["requests"] = append(reqs(m), map[string]any{
				"request_id": "req-d", "asset_id": "EQ-2", "description": "再次不制冷", "ticket_id": "T0008",
			})
			m["events"] = append(eventsL(m),
				map[string]any{"seq": 35, "asset_id": "EQ-2", "ticket_id": "T0008", "kind": "报修",
					"content": "再次不制冷", "time": "2026-01-04T00:00:00Z"},
				map[string]any{"seq": 40, "asset_id": "EQ-2", "ticket_id": "T0008", "kind": "关闭",
					"content": "换压缩机", "time": "2026-01-05T00:00:00Z"},
			)
			m["next_ticket_seq"] = 9
		}, "履历矛盾"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m := baseLedger(t)
			tc.mutate(m)
			raw := writeLedger(t, dir, m)

			_, err := openStore(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 %q，得到 %v", tc.want, err)
			}
			// 查询与写操作均以退出码 1 结束。
			if code := run([]string{"list", "--data-dir", dir}, io.Discard, io.Discard); code != 1 {
				t.Fatalf("矛盾台账上 list 退出码 = %d，想得到 1", code)
			}
			if code := run([]string{"register", "--data-dir", dir,
				"--asset-id", "EQ-9", "--name", "x", "--location", "y"}, io.Discard, io.Discard); code != 1 {
				t.Fatalf("矛盾台账上 register 退出码 = %d，想得到 1", code)
			}
			// 原文件字节保持不动。
			got, err := os.ReadFile(filepath.Join(dir, dataFileName))
			if err != nil || string(got) != string(raw) {
				t.Fatalf("原文件应保留不变: %v", err)
			}
		})
	}
}

// TestTicketNumberExhausted 编号耗尽时拒绝新报修，但有效台账仍可查询和关闭已有工单。
func TestTicketNumberExhausted(t *testing.T) {
	dir := t.TempDir()
	m := map[string]any{
		"version": 1,
		"assets": []any{
			map[string]any{"id": "EQ-1", "name": "打印机", "location": "一楼", "status": "维修中"},
			map[string]any{"id": "EQ-2", "name": "空调", "location": "二楼", "status": "可用"},
		},
		"tickets": []any{
			map[string]any{"id": "T0001", "asset_id": "EQ-1", "description": "卡纸", "request_id": "req-a",
				"status": "未关闭", "created_at": "2026-01-01T00:00:00Z"},
		},
		"events": []any{
			map[string]any{"seq": 1, "asset_id": "EQ-1", "ticket_id": "T0001", "kind": "报修",
				"content": "卡纸", "time": "2026-01-01T00:00:00Z"},
		},
		"requests": []any{
			map[string]any{"request_id": "req-a", "asset_id": "EQ-1", "description": "卡纸", "ticket_id": "T0001"},
		},
		"next_ticket_seq": math.MaxInt,
	}
	writeLedger(t, dir, m)

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("编号耗尽的台账只要一致就应加载: %v", err)
	}
	// 查询正常。
	if got := s.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatalf("应能查询未关闭工单 T0001，得到 %v", got)
	}
	// 新报修被拒绝，不消耗编号、不绑定请求。
	if _, _, err := s.report("EQ-2", "不制冷", "req-b"); err == nil || !strings.Contains(err.Error(), "耗尽") {
		t.Fatalf("编号耗尽应拒绝新报修，得到 %v", err)
	}
	if s.findRequest("req-b") != nil || s.data.NextTicketSeq != math.MaxInt {
		t.Fatal("被拒绝的报修不应绑定请求标识或推进计数器")
	}
	// 关闭已有工单仍可成功并保存。
	if _, _, err := s.closeTicket("T0001", "已修复"); err != nil {
		t.Fatalf("编号耗尽不应影响关闭已有工单: %v", err)
	}
	if err := s.save(); err != nil {
		t.Fatalf("关闭后应能保存: %v", err)
	}
	s = saveAndReopen(t, s)
	if got := s.findAsset("EQ-1").Status; got != statusAvailable {
		t.Fatalf("重开后 EQ-1 状态 = %q", got)
	}
	if _, _, err := s.report("EQ-2", "不制冷", "req-b"); err == nil {
		t.Fatal("重开后编号耗尽仍应拒绝新报修")
	}
}

// TestSaveFailureKeepsFileAndCounter 写入失败时不留下部分业务变化：
// 原文件字节不变，工单编号与请求标识不被消耗，恢复写入条件后可重新提交。
func TestSaveFailureKeepsFileAndCounter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 可写只读目录，跳过")
	}
	dir := t.TempDir()
	data := filepath.Join(dir, "data")

	s, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.registerAsset("EQ-1", "打印机", "一楼"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(data, dataFileName))
	if err != nil {
		t.Fatal(err)
	}

	// 数据目录只读：报修在内存中成功，但保存必须失败且不改动原文件。
	if err := os.Chmod(data, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(data, 0o700) }()
	s2, err := openStore(data)
	if err != nil {
		t.Fatalf("只读目录应仍能读取: %v", err)
	}
	if _, _, err := s2.report("EQ-1", "卡纸", "req-1"); err != nil {
		t.Fatal(err)
	}
	if err := s2.save(); err == nil {
		t.Fatal("只读目录下保存应失败")
	}
	got, err := os.ReadFile(filepath.Join(data, dataFileName))
	if err != nil || string(got) != string(before) {
		t.Fatalf("保存失败后原文件应保持不变: %v", err)
	}

	// 恢复写入条件后重新提交：编号未消耗（仍为 T0001），请求标识未绑定。
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	s3, err := openStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if s3.findRequest("req-1") != nil {
		t.Fatal("失败的写入不应绑定请求标识")
	}
	tk, replay, err := s3.report("EQ-1", "卡纸", "req-1")
	if err != nil || replay || tk.ID != "T0001" {
		t.Fatalf("重新提交应开出 T0001: %v replay=%v err=%v", tk, replay, err)
	}
	if err := s3.save(); err != nil {
		t.Fatal(err)
	}
	s4 := saveAndReopen(t, s3)
	if got := s4.openTicketOf("EQ-1"); got == nil || got.ID != "T0001" {
		t.Fatalf("重载后应看到未关闭工单 T0001，得到 %v", got)
	}
}
