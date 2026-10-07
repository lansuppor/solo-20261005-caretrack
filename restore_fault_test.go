package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// errInjectedRestoreFault 为写入类接缝在真实写入点返回的确定性故障。故障只在
// 指定的一次 writeAtomicFile 调用上注入；复制失败用例会先向临时文件写入部分
// 真实字节再返回该错误，与真实“写了一半后失败”走完全相同的清理路径。
var errInjectedRestoreFault = errors.New("测试注入的确定性写入故障")

// restoreStageMarks 记录一次还原实际到达过的阶段，由真实流程在接缝回调中置位。
// 负例据此断言“指定阶段确已到达”，且后续阶段未被提前阻断或跳过。
type restoreStageMarks struct {
	mkdirAttach       bool // 已到达暂存内资料目录创建
	ledgerWrite       bool // 已到达台账原子写入
	copyCalls         int  // 资料副本 writeAtomicFile 到达次数
	copySucceededSeen bool // 失败发生前至少一份资料副本已完整落盘
	partialWritten    bool // 失败的临时文件中已写入部分真实字节
	preVerify         bool // 已到达发布前复核
	publish           bool // 已到达目标目录改名发布
	postVerify        bool // 已到达发布后复核
}

// restoreFaultCase 描述一个在真实还原操作处确定性触发的中途失败用例。
type restoreFaultCase struct {
	name    string
	wantErr string // stderr（原因说明）必须包含的片段
	// hooks 在各用例自己的临时目录上构造接缝；故障通过制造真实文件系统状态
	// 让真实操作本身失败，或在真实写入点返回一次注入错误。
	hooks func(t *testing.T, m *restoreStageMarks) *restoreTestHooks
	// reached 断言指定阶段已到达、且不应到达的阶段确未到达。
	reached func(t *testing.T, m *restoreStageMarks)
}

func restoreFaultCases() []restoreFaultCase {
	return []restoreFaultCase{
		{
			name:    "暂存建成后资料目录创建失败",
			wantErr: "创建资料目录失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					beforeMkdirAttach: func(stage string) {
						m.mkdirAttach = true
						// 暂存目录已由真实 MkdirTemp 建成；把它替换成普通文件，
						// 随后真实的 MkdirAll(stage/attachments) 必以 ENOTDIR 失败。
						if err := os.Remove(stage); err != nil {
							t.Fatalf("移除空暂存目录: %v", err)
						}
						if err := os.WriteFile(stage, []byte("占位普通文件"), 0o644); err != nil {
							t.Fatalf("以普通文件占位暂存路径: %v", err)
						}
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if !m.mkdirAttach {
					t.Fatal("应已到达资料目录创建")
				}
				if m.ledgerWrite || m.preVerify || m.publish || m.postVerify {
					t.Fatalf("资料目录创建失败后不应进入后续阶段: %+v", m)
				}
			},
		},
		{
			name:    "台账写入失败",
			wantErr: "写入台账失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					writeFault: func(path string, tmp *os.File, data []byte) error {
						if filepath.Base(path) != dataFileName {
							return nil
						}
						m.ledgerWrite = true
						return errInjectedRestoreFault
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if !m.ledgerWrite {
					t.Fatal("应已到达台账写入")
				}
				if m.copyCalls != 0 || m.preVerify || m.publish || m.postVerify {
					t.Fatalf("台账写入失败后不应写入资料或进入复核: %+v", m)
				}
			},
		},
		{
			name:    "首份副本写好后后续副本写入失败",
			wantErr: "写入资料副本失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					writeFault: func(path string, tmp *os.File, data []byte) error {
						if filepath.Base(path) == dataFileName {
							return nil // 台账写入不受影响
						}
						m.copyCalls++
						if m.copyCalls == 1 {
							return nil // 第一份资料副本完整写好
						}
						// 第二份起：先把一半真实字节写入其临时文件，再令本次写入失败。
						n, werr := tmp.Write(data[:len(data)/2])
						if werr != nil {
							t.Fatalf("向临时文件写入部分真实字节: %v", werr)
						}
						m.partialWritten = n > 0
						ad := filepath.Dir(path)
						entries, rerr := os.ReadDir(ad)
						if rerr != nil {
							t.Fatalf("查看资料暂存目录: %v", rerr)
						}
						for _, e := range entries {
							if strings.HasPrefix(e.Name(), "f") && e.Type().IsRegular() {
								m.copySucceededSeen = true
							}
						}
						return errInjectedRestoreFault
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if m.copyCalls < 2 {
					t.Fatalf("故障应在第二份资料副本写入时触发，实际到达 %d 次", m.copyCalls)
				}
				if !m.copySucceededSeen {
					t.Fatal("失败前应至少有一份资料副本已完整写入暂存目录")
				}
				if !m.partialWritten {
					t.Fatal("失败用例应先在临时文件中写入部分真实字节")
				}
				if m.preVerify || m.publish || m.postVerify {
					t.Fatalf("副本写入失败后不应进入复核或发布: %+v", m)
				}
			},
		},
		{
			name:    "发布前复核失败",
			wantErr: "暂存台账复核失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					beforePreVerify: func(stage string) {
						m.preVerify = true
						// 台账与全部资料副本均已真实写好；破坏暂存台账字节，
						// 随后真实的 loadStore 复核必失败（包格式与目标存在性
						// 检查早已通过，不会提前阻断本阶段）。
						bad := []byte("{这不是有效台账 JSON")
						if err := os.WriteFile(filepath.Join(stage, dataFileName), bad, 0o644); err != nil {
							t.Fatalf("破坏暂存台账: %v", err)
						}
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if !m.preVerify {
					t.Fatal("应已到达发布前复核")
				}
				if m.publish || m.postVerify {
					t.Fatalf("发布前复核失败后不应发布: %+v", m)
				}
			},
		},
		{
			name:    "目标目录发布失败",
			wantErr: "发布目标目录失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					beforePreVerify: func(stage string) { m.preVerify = true },
					beforePublish: func(stage, target string) {
						m.publish = true
						// 在目标路径放置一个非空目录：真实 rename(stage,target)
						// 必以 EEXIST/ENOTEMPTY 确定性失败。
						if err := os.MkdirAll(target, 0o755); err != nil {
							t.Fatalf("占位目标目录: %v", err)
						}
						if err := os.WriteFile(filepath.Join(target, "占位文件"), []byte("x"), 0o644); err != nil {
							t.Fatalf("占位目标目录内容: %v", err)
						}
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if !m.preVerify || !m.publish {
					t.Fatalf("应已通过发布前复核并到达发布: %+v", m)
				}
				if m.postVerify {
					t.Fatal("发布失败后不应进入发布后复核")
				}
			},
		},
		{
			name:    "发布后复核失败",
			wantErr: "还原后复核失败",
			hooks: func(t *testing.T, m *restoreStageMarks) *restoreTestHooks {
				return &restoreTestHooks{
					beforePreVerify: func(stage string) { m.preVerify = true },
					beforePublish:   func(stage, target string) { m.publish = true },
					beforePostVerify: func(target string) {
						m.postVerify = true
						// 目标已由真实改名发布生成且台账完好；把其中一份资料副本
						// 换成同名目录，真实的发布后可读性检查必失败。
						p := filepath.Join(target, restoreAttachDir, "f000001")
						if err := os.RemoveAll(p); err != nil {
							t.Fatalf("移除已发布副本: %v", err)
						}
						if err := os.Mkdir(p, 0o755); err != nil {
							t.Fatalf("以目录占位资料副本: %v", err)
						}
					},
				}
			},
			reached: func(t *testing.T, m *restoreStageMarks) {
				if !m.preVerify || !m.publish || !m.postVerify {
					t.Fatalf("应依次到达发布前复核、发布与发布后复核: %+v", m)
				}
			},
		},
	}
}

// TestMigrateRestoreFaultRollbackAndRetry 覆盖还原流程六个中途失败阶段：每个
// 故障都在真实读写/文件系统操作处确定性触发，命令入口退出 1、不输出成功摘要，
// 目标目录、本次暂存目录、部分副本与临时文件全部清除，输入包、源台账、原资料
// 字节与目标父目录内的无关文件保持；移除故障后以原包、原目标路径重试成功，
// 并按固定业务数据逐项核对还原内容。
func TestMigrateRestoreFaultRollbackAndRetry(t *testing.T) {
	fx := newMigrateFixture(t)
	pkg := filepath.Join(t.TempDir(), "pkg.zip")
	if out, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B"}); err != nil {
		t.Fatalf("导出迁移包: %v", err)
	} else if len(out.assetIDs) != 2 || out.attachments != 4 {
		t.Fatalf("导出摘要不符: %+v", out)
	}
	pkgBefore, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	srcBefore := readFileBytes(t, fx.srcDir)
	// 期望数据以固定夹具源台账独立确定，不依赖还原实现的内部一致性。
	wantStore, err := openStore(fx.srcDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range restoreFaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "parent")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			// 目标父目录内预先放置无关文件，任何阶段失败后都必须原样保留。
			unrelated := filepath.Join(parent, "无关文件.txt")
			const unrelatedBytes = "与本次还原无关的既有内容\n"
			if err := os.WriteFile(unrelated, []byte(unrelatedBytes), 0o644); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(parent, "restored")

			m := &restoreStageMarks{}
			restoreFault = tc.hooks(t, m)
			t.Cleanup(func() { restoreFault = nil })
			var stdout, stderr bytes.Buffer
			code := run([]string{"restore", "--package", pkg, "--target-dir", target}, &stdout, &stderr)
			restoreFault = nil // 故障在用例结束（重试之前）即恢复
			if code != 1 {
				t.Fatalf("中途失败应退出 1，得到 %d；stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 || strings.Contains(stdout.String(), "迁移包已还原") {
				t.Fatalf("失败时不得输出成功摘要，得到 stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "caretrack:") || !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("失败应经命令入口说明原因（含 %q），得到 stderr=%q", tc.wantErr, stderr.String())
			}

			// 指定阶段确已到达，且未被包格式/目标已存在等检查提前阻断。
			tc.reached(t, m)

			// 目标目录不存在；父目录中只剩无关文件——本次暂存目录（含其下的
			// 部分资料副本与 *.tmp 临时文件）已被全部清除。
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("失败后目标目录应不存在，得到 err=%v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "无关文件.txt" {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("失败后父目录应只剩无关文件，得到 %v", names)
			}
			var tmpLeft []string
			_ = filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
				if err == nil && strings.HasPrefix(d.Name(), ".caretrack-restore-") {
					tmpLeft = append(tmpLeft, p)
				}
				return nil
			})
			if len(tmpLeft) != 0 {
				t.Fatalf("失败后不应残留暂存目录或临时文件: %v", tmpLeft)
			}
			if got, rerr := os.ReadFile(unrelated); rerr != nil || string(got) != unrelatedBytes {
				t.Fatalf("父目录无关文件应保持: %v %q", rerr, got)
			}

			// 输入包、源台账、原资料字节全部保持。
			if got, _ := os.ReadFile(pkg); !bytes.Equal(got, pkgBefore) {
				t.Fatal("失败不得修改输入迁移包字节")
			}
			if string(readFileBytes(t, fx.srcDir)) != string(srcBefore) {
				t.Fatal("失败不得修改源台账字节")
			}
			for p, want := range fx.files {
				got, rerr := os.ReadFile(p)
				if rerr != nil || !bytes.Equal(got, want) {
					t.Fatalf("失败不得修改原资料 %s: %v", p, rerr)
				}
			}

			// 移除故障后以原包、原目标路径重试：成功并输出成功摘要。
			var okOut, okErr bytes.Buffer
			if code := run([]string{"restore", "--package", pkg, "--target-dir", target}, &okOut, &okErr); code != 0 {
				t.Fatalf("移除故障后原包重试应成功，code=%d stderr=%s", code, okErr.String())
			}
			o := okOut.String()
			if !strings.Contains(o, "迁移包已还原: "+target) ||
				!strings.Contains(o, "资产: 2 项") || !strings.Contains(o, "附件索引: 4 条") {
				t.Fatalf("重试成功摘要不符: %s", o)
			}
			if okErr.Len() != 0 {
				t.Fatalf("重试成功不应有 stderr 输出: %q", okErr.String())
			}
			// 无关文件在重试成功后仍然保留。
			if got, rerr := os.ReadFile(unrelated); rerr != nil || string(got) != unrelatedBytes {
				t.Fatalf("重试后无关文件应保持: %v", rerr)
			}
			assertRestoredLedgerAndCopies(t, target, fx, wantStore)
		})
	}
}

// assertRestoredLedgerAndCopies 按固定夹具的业务数据独立核对一次成功还原：
// 重新加载台账、读取每份独立副本、核对撤销状态、附件记录与登记/撤销履历中的
// 目标绝对路径、编号计数器与履历序号、请求绑定，以及已有请求重放返回原工单
// 且不写台账、不新增业务履历。
func assertRestoredLedgerAndCopies(t *testing.T, target string, fx *migrateFixture, want *store) {
	t.Helper()
	// 目标目录恰含台账与资料目录；资料目录恰含四份副本。
	rootEntries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 2 {
		t.Fatalf("目标目录应恰有两项，得到 %v", rootEntries)
	}
	copyEntries, err := os.ReadDir(filepath.Join(target, restoreAttachDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(copyEntries) != 4 {
		t.Fatalf("资料目录应有四份独立副本，得到 %v", copyEntries)
	}

	s, err := openStore(target)
	if err != nil {
		t.Fatalf("重试后台账应可加载且自洽: %v", err)
	}
	// 未选择的资产/工单/请求不进入包内。
	if s.findAsset("EQ-C") != nil || s.findAsset("EQ-D") != nil {
		t.Fatal("还原台账不应包含未选择资产")
	}
	if s.findTicket("T0004") != nil || s.findRequest("req-c1") != nil {
		t.Fatal("还原台账不应包含未选择工单或请求绑定")
	}
	// 计数器原样保持。
	if s.data.NextTicketSeq != 5 || s.data.NextPartSeq != 2 || s.data.NextAttachSeq != 5 {
		t.Fatalf("计数器应保持，得到 ticket=%d part=%d attach=%d",
			s.data.NextTicketSeq, s.data.NextPartSeq, s.data.NextAttachSeq)
	}
	// 终态与未关闭工单及报修地点、负责人、关闭结果均为固定值。
	t1 := s.findTicket("T0001")
	if t1 == nil || t1.Status != ticketClosed || t1.Result != "已更换搓纸轮" ||
		t1.Assignee != "张三" || t1.ReportLocation != "一楼" {
		t.Fatalf("T0001 终态内容不符: %+v", t1)
	}
	for _, id := range []string{"T0002", "T0003"} {
		if tk := s.findTicket(id); tk == nil || tk.Status != ticketOpen {
			t.Fatalf("%s 应为未关闭工单: %+v", id, tk)
		}
	}
	if t2 := s.findTicket("T0002"); t2 != nil && t2.ReportLocation != "二楼" {
		t.Fatalf("T0002 报修地点应为二楼，得到 %s", t2.ReportLocation)
	}
	// 备件领用记录与保养计划段保持。
	var p1 *PartWithdrawal
	for i := range s.data.Parts {
		if s.data.Parts[i].ID == "P0001" {
			p1 = s.data.Parts[i]
		}
	}
	if p1 == nil || p1.TicketID != "T0001" || p1.PartID != "FILTER-01" || p1.Quantity != 2 ||
		p1.Note != "更换滤芯" || p1.Returned != 0 {
		t.Fatalf("P0001 领用记录应保持: %+v", p1)
	}
	plan := s.findPlan("EQ-A")
	if plan == nil || plan.Content != "更换滤芯" || plan.IntervalDays != 90 || plan.NextDue != "2027-01-30" {
		t.Fatalf("保养计划与下一到期日应保持: %+v", plan)
	}
	// 请求绑定逐项保持。
	wantReq := map[string][3]string{
		"req-a1": {"EQ-A", "卡纸", "T0001"},
		"req-a2": {"EQ-A", "无法开机", "T0002"},
		"req-b1": {"EQ-B", "不制冷", "T0003"},
	}
	if len(s.data.Requests) != len(wantReq) {
		t.Fatalf("请求绑定数量应为 %d，得到 %d", len(wantReq), len(s.data.Requests))
	}
	for id, w := range wantReq {
		r := s.findRequest(id)
		if r == nil || r.AssetID != w[0] || r.Description != w[1] || r.TicketID != w[2] {
			t.Fatalf("请求绑定 %s 应为 %v，得到 %+v", id, w, r)
		}
	}

	// 履历：序号集合与包内所选资产（EQ-A、EQ-B）的源履历完全一致（不重排、
	// 不新增业务履历），各类型数量固定。
	wantSeqs := make([]int, 0, len(want.data.Events))
	wantEventCount := 0
	for _, e := range want.data.Events {
		if e.AssetID != "EQ-A" && e.AssetID != "EQ-B" {
			continue // 未选择的 EQ-C 履历不进入迁移包
		}
		wantSeqs = append(wantSeqs, e.Seq)
		wantEventCount++
	}
	sort.Ints(wantSeqs)
	gotSeqs := make([]int, 0, len(s.data.Events))
	kindCount := map[string]int{}
	for _, e := range s.data.Events {
		gotSeqs = append(gotSeqs, e.Seq)
		kindCount[e.Kind]++
	}
	sort.Ints(gotSeqs)
	if len(gotSeqs) != wantEventCount {
		t.Fatalf("履历数量应保持为 %d，得到 %d（不得新增业务履历）", wantEventCount, len(gotSeqs))
	}
	for i := range wantSeqs {
		if gotSeqs[i] != wantSeqs[i] {
			t.Fatalf("履历序号集合应保持：源=%v 目标=%v", wantSeqs, gotSeqs)
		}
	}
	wantKindCount := map[string]int{
		eventReport: 3, eventAssign: 1, eventClose: 1, eventRelocate: 1,
		eventPlanCreate: 1, eventPlanDone: 1, eventPartWithdraw: 1,
		eventAttach: 4, eventAttachRevoke: 1,
	}
	for k, n := range wantKindCount {
		if kindCount[k] != n {
			t.Fatalf("履历类型 %s 应有 %d 条，得到 %d（全量 %v）", k, n, kindCount[k], kindCount)
		}
	}
	if len(kindCount) != len(wantKindCount) {
		t.Fatalf("出现了固定数据之外的履历类型: %v", kindCount)
	}

	// 附件：同路径的有效与已撤销索引各为目标内独立副本，内容与原资料逐字节
	// 一致；记录及登记/撤销履历中的路径均为副本的绝对路径。
	wantAtt := map[string]struct {
		content string
		base    string
		revoked bool
		reason  string
	}{
		"A0001": {"资料一内容", "f000001", false, ""},
		"A0002": {"资料一内容", "f000002", true, "照片拍错设备"},
		"A0003": {"资料三内容", "f000003", false, ""},
		"A0004": {"资料四内容", "f000004", false, ""},
	}
	paths := map[string]string{}
	for id, w := range wantAtt {
		a := s.findAttachment(id)
		if a == nil {
			t.Fatalf("附件 %s 应存在", id)
		}
		if a.Revoked != w.revoked || a.RevokeReason != w.reason {
			t.Fatalf("附件 %s 撤销状态/理由不符: %+v", id, a)
		}
		if !filepath.IsAbs(a.Path) || filepath.Dir(a.Path) != filepath.Join(target, restoreAttachDir) ||
			filepath.Base(a.Path) != w.base {
			t.Fatalf("附件 %s 路径应为目标内副本绝对路径 %s，得到 %s",
				id, filepath.Join(target, restoreAttachDir, w.base), a.Path)
		}
		raw, rerr := os.ReadFile(a.Path)
		if rerr != nil || string(raw) != w.content {
			t.Fatalf("附件 %s 副本内容应与原资料逐字节一致: %v %q", id, rerr, raw)
		}
		paths[id] = a.Path
		gotKinds := map[string]int{}
		for _, e := range s.data.Events {
			if (e.Kind == eventAttach || e.Kind == eventAttachRevoke) && e.AttachmentID == id {
				if e.Path != a.Path {
					t.Fatalf("附件 %s 的 %s 履历路径 %s 未同步为副本路径 %s", id, e.Kind, e.Path, a.Path)
				}
				gotKinds[e.Kind]++
			}
		}
		if w.revoked {
			if gotKinds[eventAttach] != 1 || gotKinds[eventAttachRevoke] != 1 {
				t.Fatalf("A0002 应恰有一条登记与一条撤销履历且路径均已替换，得到 %v", gotKinds)
			}
		} else if gotKinds[eventAttach] != 1 || gotKinds[eventAttachRevoke] != 0 {
			t.Fatalf("附件 %s 应只有一条登记履历，得到 %v", id, gotKinds)
		}
	}
	if paths["A0001"] == paths["A0002"] {
		t.Fatal("同路径的两条索引在目标内必须是各自独立的副本")
	}
	// 台账文本中不得残留源资料目录的绝对路径。
	rawLedger, err := os.ReadFile(filepath.Join(target, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawLedger, []byte(fx.fileDir)) {
		t.Fatalf("台账中不应残留源资料目录路径 %s", fx.fileDir)
	}

	// 已有报修请求重放返回原工单，且不写台账：重放前后台账字节一致，重新加载
	// 后履历与请求绑定数量不变。
	ledgerBefore, err := os.ReadFile(filepath.Join(target, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	replayS, err := openStore(target)
	if err != nil {
		t.Fatal(err)
	}
	tk, replayed, rerr := replayS.report("EQ-A", "卡纸", "req-a1")
	if rerr != nil || !replayed || tk == nil || tk.ID != "T0001" {
		t.Fatalf("已有请求重放应返回原工单 T0001: %v replayed=%v ticket=%+v", rerr, replayed, tk)
	}
	ledgerAfter, rerr := os.ReadFile(filepath.Join(target, dataFileName))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatal("已有请求重放不应写台账")
	}
	reloadS, err := openStore(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadS.data.Events) != wantEventCount || len(reloadS.data.Requests) != len(wantReq) {
		t.Fatal("重放后重新加载：履历与请求绑定数量应保持不变")
	}
}
