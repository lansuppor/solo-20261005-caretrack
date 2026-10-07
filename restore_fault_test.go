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

// restoreReach 记录一次故障还原实际到达的阶段与现场，供断言“故障确实在
// 真实还原流程对应的读写点触发”，而不是入口直接返回或另写模拟流程。
type restoreReach struct {
	reached bool
	stage   string // 本次暂存目录绝对路径
	target  string // 发布后的目标目录（仅发布后复核用例）

	// 台账/副本原子写入现场。
	writes       []string // 经 stagedWrite 接缝的最终路径（按调用顺序）
	partialTmp   string   // 故障前写入部分真实字节的临时文件
	partialExist bool     // 故障返回时该临时文件确实存在
	partialHead  []byte   // 临时文件内已写入的真实字节前缀

	// 副本写入失败用例：故障发生时已完整写好的前序副本路径与内容。
	copiedBefore map[string][]byte
}

// resetRestoreFaults 清空全部还原故障接缝，保证后续重试与其他测试执行真实
// 操作。测试在每个用例结束时（含 t.Cleanup）调用，防止污染。
func resetRestoreFaults() {
	restoreFaults.mkdirAttach = nil
	restoreFaults.stagedWrite = nil
	restoreFaults.precheckLoad = nil
	restoreFaults.publishRename = nil
	restoreFaults.postcheckOpen = nil
}

// readRestoreManifest 从已导出的包条目解析清单，得到附件编号到资料成员名
// 的映射（测试据此独立确定期望路径与内容，不依赖还原内部状态）。
func readRestoreManifest(t *testing.T, entries []pkgEntry) migrateManifest {
	t.Helper()
	var m migrateManifest
	if err := json.Unmarshal(findEntry(entries, migrateManifestName).data, &m); err != nil {
		t.Fatalf("解析清单: %v", err)
	}
	return m
}

// assertParentClean 断言目标父目录内只剩预先放置的无关文件，没有暂存目录
// 或临时文件残留。
func assertParentClean(t *testing.T, parent, unrelatedName string) {
	t.Helper()
	le, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("读取目标父目录: %v", err)
	}
	if len(le) != 1 || le[0].Name() != unrelatedName {
		t.Fatalf("失败后父目录应仅剩无关文件 %s，得到 %v", unrelatedName, le)
	}
	var leaked []string
	_ = filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".caretrack-restore") || strings.HasSuffix(name, ".tmp") {
			leaked = append(leaked, p)
		}
		return nil
	})
	if len(leaked) != 0 {
		t.Fatalf("失败后不应残留暂存目录或临时文件，发现 %v", leaked)
	}
}

// assertInputsUntouched 断言输入包、源台账与原资料字节全部保持。
func assertInputsUntouched(t *testing.T, pkg string, pkgBefore, srcBefore []byte, fx *migrateFixture) {
	t.Helper()
	if got, err := os.ReadFile(pkg); err != nil || !bytes.Equal(got, pkgBefore) {
		t.Fatalf("失败不应修改输入包字节: %v", err)
	}
	if got := readFileBytes(t, fx.srcDir); !bytes.Equal(got, srcBefore) {
		t.Fatal("失败不应修改源台账字节")
	}
	for p, want := range fx.files {
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("原资料 %s 字节应保持: %v", p, err)
		}
	}
}

// verifyRestoredContent 按固定业务数据独立核对一次成功还原的结果：计数器、
// 工单终态、各附件副本的绝对路径与逐字节内容、撤销状态、登记/撤销履历中的
// 路径、履历集合（还原不新增业务履历）以及请求重放（返回原工单且不写台账）。
func verifyRestoredContent(t *testing.T, target string, entries []pkgEntry) {
	t.Helper()
	manifest := readRestoreManifest(t, entries)
	memberData := map[string][]byte{}
	for _, e := range entries {
		memberData[e.name] = e.data
	}

	// 目标目录结构：仅台账与资料目录；资料目录内恰为四份副本。
	le, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("重试后读取目标目录: %v", err)
	}
	if len(le) != 2 {
		t.Fatalf("目标目录应只有台账与资料目录，得到 %v", le)
	}
	attachLe, err := os.ReadDir(filepath.Join(target, restoreAttachDir))
	if err != nil {
		t.Fatalf("读取资料目录: %v", err)
	}
	if len(attachLe) != len(manifest.Files) {
		t.Fatalf("资料目录应有 %d 份副本，得到 %v", len(manifest.Files), attachLe)
	}

	s, err := openStore(target)
	if err != nil {
		t.Fatalf("重试还原后台账应可加载: %v", err)
	}
	// 计数器原样保持。
	if s.data.NextTicketSeq != 5 || s.data.NextPartSeq != 2 || s.data.NextAttachSeq != 5 {
		t.Fatalf("计数器应保持，得到 ticket=%d part=%d attach=%d",
			s.data.NextTicketSeq, s.data.NextPartSeq, s.data.NextAttachSeq)
	}
	// 未关闭与终态工单均在，状态保持。
	if t1 := s.findTicket("T0001"); t1 == nil || t1.Status != ticketClosed || t1.Result != "已更换搓纸轮" {
		t.Fatalf("T0001 应为已关闭且结果保持: %+v", t1)
	}
	if t2 := s.findTicket("T0002"); t2 == nil || t2.Status != ticketOpen {
		t.Fatalf("T0002 应为未关闭: %+v", t2)
	}
	if t3 := s.findTicket("T0003"); t3 == nil || t3.Status != ticketOpen {
		t.Fatalf("T0003 应为未关闭: %+v", t3)
	}
	// 还原不新增业务履历：固定夹具导出部分恰有 14 条履历，最大序号 14。
	if len(s.data.Events) != 14 {
		t.Fatalf("还原不应新增履历，应有 14 条，得到 %d", len(s.data.Events))
	}
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if maxSeq != 14 {
		t.Fatalf("履历最大序号应为 14，得到 %d", maxSeq)
	}

	// 固定业务期望（独立于还原内部状态）：
	wantContent := map[string]string{
		"A0001": "资料一内容", "A0002": "资料一内容",
		"A0003": "资料三内容", "A0004": "资料四内容",
	}
	revoked := map[string]bool{"A0002": true}
	memberByID := map[string]string{}
	for _, fm := range manifest.Files {
		memberByID[fm.AttachmentID] = fm.Member
	}
	paths := map[string]string{}
	for id, want := range wantContent {
		att := s.findAttachment(id)
		if att == nil {
			t.Fatalf("附件 %s 应存在", id)
		}
		member := memberByID[id]
		wantPath := filepath.Join(target, restoreAttachDir, filepath.Base(member))
		if att.Path != wantPath || !filepath.IsAbs(att.Path) {
			t.Fatalf("附件 %s 路径应为副本绝对路径 %s，得到 %s", id, wantPath, att.Path)
		}
		got, rerr := os.ReadFile(att.Path)
		if rerr != nil {
			t.Fatalf("读取附件 %s 副本: %v", id, rerr)
		}
		// 与固定期望文本核对，也与包内对应成员逐字节核对。
		if string(got) != want {
			t.Fatalf("附件 %s 副本内容应为 %q，得到 %q", id, want, got)
		}
		if !bytes.Equal(got, memberData[member]) {
			t.Fatalf("附件 %s 副本字节与包内成员 %s 不一致", id, member)
		}
		if att.Revoked != revoked[id] {
			t.Fatalf("附件 %s 撤销状态应为 %v，得到 %v", id, revoked[id], att.Revoked)
		}
		if id == "A0002" && att.RevokeReason != "照片拍错设备" {
			t.Fatalf("A0002 撤销理由应保持，得到 %q", att.RevokeReason)
		}
		paths[id] = att.Path

		// 登记履历（A0002 另含撤销履历）中的路径须同步为副本绝对路径。
		var regs, revokes int
		for _, e := range s.data.Events {
			if e.AttachmentID != id {
				continue
			}
			if e.Kind != eventAttach && e.Kind != eventAttachRevoke {
				t.Fatalf("附件 %s 出现意外履历类型 %s", id, e.Kind)
			}
			if e.Path != att.Path {
				t.Fatalf("附件 %s 的%s履历路径 %s 应为 %s", id, e.Kind, e.Path, att.Path)
			}
			switch e.Kind {
			case eventAttach:
				regs++
				if e.Content != att.Note {
					t.Fatalf("附件 %s 登记履历说明 %q 与记录 %q 不符", id, e.Content, att.Note)
				}
			case eventAttachRevoke:
				revokes++
				if e.Content != att.RevokeReason {
					t.Fatalf("附件 %s 撤销履历理由 %q 与记录 %q 不符", id, e.Content, att.RevokeReason)
				}
			}
		}
		if regs != 1 {
			t.Fatalf("附件 %s 应恰有一条登记履历，得到 %d", id, regs)
		}
		wantRevokes := 0
		if revoked[id] {
			wantRevokes = 1
		}
		if revokes != wantRevokes {
			t.Fatalf("附件 %s 撤销履历应为 %d 条，得到 %d", id, wantRevokes, revokes)
		}
	}
	// 同路径的两条索引（A0001/A0002）为相互独立的副本文件。
	if paths["A0001"] == paths["A0002"] {
		t.Fatal("同路径的有效与已撤销索引在目标内应保持各自独立副本")
	}

	// 请求绑定保持：既有报修请求重放返回原工单，且不写台账。
	ledgerBefore, err := os.ReadFile(filepath.Join(target, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	tk, replayed, err := s.report("EQ-A", "卡纸", "req-a1")
	if err != nil || !replayed || tk.ID != "T0001" {
		t.Fatalf("原请求 req-a1 重放应返回原工单 T0001: %v replayed=%v ticket=%+v", err, replayed, tk)
	}
	ledgerAfter, err := os.ReadFile(filepath.Join(target, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatal("既有请求重放不应写台账")
	}
}

// TestMigrateRestoreFailureRollbackAndRetry 在真实还原流程的六个读写/文件
// 系统操作处逐次确定性注入可恢复故障，验证完整回滚；每个故障随后移除并以
// 原包、原目标路径重试成功。故障经最小内部接缝注入，接缝默认执行真实操作，
// 清理路径不经接缝、始终真实执行。
func TestMigrateRestoreFailureRollbackAndRetry(t *testing.T) {
	t.Cleanup(resetRestoreFaults)

	fx := newMigrateFixture(t)
	pkg := filepath.Join(t.TempDir(), "pkg.zip")
	if _, err := exportPackage(fx.srcDir, pkg, []string{"EQ-A", "EQ-B", "EQ-D"}); err != nil {
		t.Fatalf("导出合法迁移包失败: %v", err)
	}
	pkgBefore, err := os.ReadFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	srcBefore := readFileBytes(t, fx.srcDir)
	entries := readPkgEntries(t, pkg)
	memberData := map[string][]byte{}
	for _, e := range entries {
		memberData[e.name] = e.data
	}

	const unrelatedName = "无关文件.txt"
	unrelatedContent := []byte("父目录内预先存在，不应受影响")

	type faultCase struct {
		name       string
		wantReason string // 错误说明必须包含的阶段原因
		inject     func(t *testing.T, rec *restoreReach)
	}
	cases := []faultCase{
		{
			name:       "资料目录创建失败",
			wantReason: "创建资料目录失败",
			inject: func(t *testing.T, rec *restoreReach) {
				restoreFaults.mkdirAttach = func(path string, perm os.FileMode) error {
					rec.reached = true
					rec.stage = filepath.Dir(path)
					if fi, err := os.Stat(rec.stage); err != nil || !fi.IsDir() {
						t.Errorf("故障时暂存目录应已建成: %v", err)
					}
					return errors.New("测试注入：资料目录无法创建")
				}
			},
		},
		{
			name:       "台账写入失败",
			wantReason: "写入台账失败",
			inject: func(t *testing.T, rec *restoreReach) {
				restoreFaults.stagedWrite = func(path string, data []byte) error {
					rec.writes = append(rec.writes, path)
					if filepath.Base(path) != dataFileName {
						return writeAtomicFileReal(path, data)
					}
					// 先在暂存目录的临时文件中写入部分真实台账字节，再失败。
					rec.reached = true
					rec.stage = filepath.Dir(path)
					tmp, err := os.CreateTemp(rec.stage, ".caretrack-restore-*.tmp")
					if err != nil {
						t.Fatalf("建立部分字节临时文件: %v", err)
					}
					head := data[:min(64, len(data))]
					if _, err := tmp.Write(head); err != nil {
						t.Fatalf("写入部分台账字节: %v", err)
					}
					rec.partialTmp = tmp.Name()
					rec.partialHead = head
					if _, err := os.Lstat(rec.partialTmp); err == nil {
						rec.partialExist = true
					}
					_ = tmp.Close()
					return errors.New("测试注入：台账临时文件写入中断")
				}
			},
		},
		{
			name:       "后续资料副本写入失败",
			wantReason: "写入资料副本失败（附件编号 A0003）",
			inject: func(t *testing.T, rec *restoreReach) {
				rec.copiedBefore = map[string][]byte{}
				restoreFaults.stagedWrite = func(path string, data []byte) error {
					rec.writes = append(rec.writes, path)
					base := filepath.Base(path)
					// 台账及前两份副本（A0001→f000001、A0002→f000002）走真实写入。
					if base == dataFileName || base == "f000001" || base == "f000002" {
						if err := writeAtomicFileReal(path, data); err != nil {
							return err
						}
						if base != dataFileName {
							got, rerr := os.ReadFile(path)
							if rerr != nil {
								t.Fatalf("前序副本应已真实落盘: %v", rerr)
							}
							rec.copiedBefore[base] = got
						}
						return nil
					}
					// 第三份副本（A0003→f000003）：先在其临时文件写入部分真实字节再失败。
					if base != "f000003" {
						t.Fatalf("故障应在 A0003 的第三份副本处触发，实际为 %s", base)
					}
					rec.reached = true
					rec.stage = filepath.Dir(filepath.Dir(path))
					tmp, err := os.CreateTemp(filepath.Dir(path), ".caretrack-restore-*.tmp")
					if err != nil {
						t.Fatalf("建立副本临时文件: %v", err)
					}
					head := data[:min(5, len(data))]
					if _, err := tmp.Write(head); err != nil {
						t.Fatalf("写入部分资料字节: %v", err)
					}
					rec.partialTmp = tmp.Name()
					rec.partialHead = head
					if _, err := os.Lstat(rec.partialTmp); err == nil {
						rec.partialExist = true
					}
					_ = tmp.Close()
					return errors.New("测试注入：资料副本写入中断")
				}
			},
		},
		{
			name:       "发布前复核失败",
			wantReason: "暂存台账复核失败",
			inject: func(t *testing.T, rec *restoreReach) {
				restoreFaults.precheckLoad = func(dir string, allowMissing bool) (*store, error) {
					// 到达此处时台账与全部四份副本都应已真实写好且内容正确，
					// 证明故障发生在发布前复核而非更早的写入阶段。
					rec.reached = true
					rec.stage = dir
					if _, err := loadStore(dir, false); err != nil {
						t.Fatalf("发布前暂存台账本应可加载: %v", err)
					}
					for _, name := range []string{"f000001", "f000002", "f000003", "f000004"} {
						p := filepath.Join(dir, restoreAttachDir, name)
						got, err := os.ReadFile(p)
						if err != nil {
							t.Fatalf("发布前副本 %s 应已写好: %v", name, err)
						}
						if !bytes.Equal(got, memberData[filepath.Join(migrateFilesDir, name)]) {
							t.Fatalf("发布前副本 %s 内容与包内成员不符", name)
						}
					}
					return nil, errors.New("测试注入：发布前复核未通过")
				}
			},
		},
		{
			name:       "目标目录发布失败",
			wantReason: "发布目标目录失败",
			inject: func(t *testing.T, rec *restoreReach) {
				restoreFaults.publishRename = func(oldpath, newpath string) error {
					// 到达此处时暂存目录完整、目标尚不存在。
					rec.reached = true
					rec.stage = oldpath
					if _, err := os.Lstat(newpath); !os.IsNotExist(err) {
						t.Fatalf("发布前目标不应存在: %v", err)
					}
					if le, err := os.ReadDir(filepath.Join(oldpath, restoreAttachDir)); err != nil || len(le) != 4 {
						t.Fatalf("发布前应有四份副本，得到 %v %v", le, err)
					}
					return errors.New("测试注入：暂存目录改名发布失败")
				}
			},
		},
		{
			name:       "发布后复核失败",
			wantReason: "还原后复核失败",
			inject: func(t *testing.T, rec *restoreReach) {
				restoreFaults.postcheckOpen = func(dir string) (*store, error) {
					// 到达此处时目标目录确已发布且内容真实可用（否则属于更早阻断）。
					rec.reached = true
					rec.stage = ""
					rec.target = dir
					if _, err := os.Lstat(dir); err != nil {
						t.Fatalf("发布后复核时目标目录应已存在: %v", err)
					}
					published, err := openSourceStore(dir)
					if err != nil {
						t.Fatalf("发布后台账本应可加载: %v", err)
					}
					if len(published.data.Attachments) != 4 {
						t.Fatalf("发布后应能看到四份附件索引，得到 %d", len(published.data.Attachments))
					}
					for _, a := range published.data.Attachments {
						if !attachmentReadable(a.Path) {
							t.Fatalf("发布后复核时附件 %s 的副本本应可读", a.ID)
						}
					}
					return nil, errors.New("测试注入：发布后复核未通过")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRestoreFaults()

			// 每次用例使用独立父目录，预先放入无关文件；目标尚不存在。
			parent := filepath.Join(t.TempDir(), "parent")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(parent, unrelatedName), unrelatedContent, 0o644); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(parent, "restored")

			rec := &restoreReach{}
			tc.inject(t, rec)

			// 经真实命令入口执行；业务失败必须退出 1、说明原因且无成功摘要。
			var stdout, stderr bytes.Buffer
			code := run([]string{"restore", "--package", pkg, "--target-dir", target}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("故障还原应退出 1，得到 %d；stderr=%s", code, stderr.String())
			}
			if stdout.Len() != 0 || strings.Contains(stdout.String(), "迁移包已还原") {
				t.Fatalf("失败时不应输出成功摘要，stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantReason) ||
				!strings.Contains(stderr.String(), "测试注入") {
				t.Fatalf("错误应说明阶段与注入原因（含 %q），stderr=%q", tc.wantReason, stderr.String())
			}
			if !rec.reached {
				t.Fatal("故障未在指定的真实还原阶段触发")
			}

			// 目标目录必须不存在；已发布的目标（发布后复核用例）也须清除。
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("失败后目标目录必须不存在，得到 err=%v", err)
			}
			// 暂存目录、已写好的部分副本与临时文件全部清除。
			if rec.stage != "" {
				if _, err := os.Lstat(rec.stage); !os.IsNotExist(err) {
					t.Fatalf("失败后暂存目录必须清除: %s", rec.stage)
				}
			}
			if rec.partialTmp != "" {
				if !rec.partialExist {
					t.Fatal("故障返回前部分字节临时文件应真实存在过")
				}
				if len(rec.partialHead) == 0 {
					t.Fatal("临时文件应先写入部分真实字节")
				}
				if _, err := os.Lstat(rec.partialTmp); !os.IsNotExist(err) {
					t.Fatalf("部分字节临时文件必须清除: %s", rec.partialTmp)
				}
			}
			if rec.copiedBefore != nil {
				if len(rec.copiedBefore) != 2 {
					t.Fatalf("故障前应至少完整写好两份副本，得到 %d", len(rec.copiedBefore))
				}
				for name, got := range rec.copiedBefore {
					want := memberData[filepath.Join(migrateFilesDir, name)]
					if !bytes.Equal(got, want) {
						t.Fatalf("已写好的副本 %s 内容应与包内成员一致", name)
					}
				}
			}
			assertParentClean(t, parent, unrelatedName)
			if got, err := os.ReadFile(filepath.Join(parent, unrelatedName)); err != nil ||
				!bytes.Equal(got, unrelatedContent) {
				t.Fatalf("无关文件应保持原样: %v", err)
			}
			assertInputsUntouched(t, pkg, pkgBefore, srcBefore, fx)

			// 移除故障，以原包与原目标路径重试：必须成功并输出成功摘要。
			resetRestoreFaults()
			var okOut, okErr bytes.Buffer
			code = run([]string{"restore", "--package", pkg, "--target-dir", target}, &okOut, &okErr)
			if code != 0 {
				t.Fatalf("移除故障后原包重试应成功，code=%d stderr=%s", code, okErr.String())
			}
			if !strings.Contains(okOut.String(), "迁移包已还原: "+target) ||
				!strings.Contains(okOut.String(), "资产: 3 项") ||
				!strings.Contains(okOut.String(), "附件索引: 4 条") {
				t.Fatalf("重试成功摘要不符: %q", okOut.String())
			}
			// 无关文件在成功还原后仍保持。
			if got, err := os.ReadFile(filepath.Join(parent, unrelatedName)); err != nil ||
				!bytes.Equal(got, unrelatedContent) {
				t.Fatalf("重试后无关文件应保持: %v", err)
			}
			// 重新加载台账并逐字节读取资料，按固定业务数据核对。
			verifyRestoredContent(t, target, entries)

			resetRestoreFaults()
		})
	}
}
