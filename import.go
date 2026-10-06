package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 资产批量导入：把源数据目录中所选资产连同其全部工单、报修请求绑定与
// 报修/派工/关闭/取消履历复制到目标数据目录。
//
// 关键规则：
//   - 导入是复制：源台账始终只读，不删除、不修改任何源记录；同一台账不能导入自身。
//   - 工单编号按源工单序号升序，从目标的下一工单序号重新分配，并同步替换履历与
//     请求绑定中的工单引用；目标原有记录不改编号、不改变业务含义。
//   - 履历按源履历序号排列，在目标已有最大履历序号之后依次分配新序号，保留原操作
//     顺序（不按时间重排）；履历时间保留原瞬间与小数秒精度。导入本身不追加报修
//     或其他业务事件。
//   - 资产编号在目标已存在，或所选工单的任一请求标识已在目标绑定时，整批拒绝：
//     不覆盖、不合并、不改请求标识。
//   - 操作前检查源、目标整库一致性，提交前由 save 再检查合并后的数据；整批变化
//     一次原子写入目标。任何失败都不改动两边原文件、不留下部分导入、不消耗目标编号。

// stringListFlag 收集可重复的 --asset-id 参数。
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// ticketRemap 记录一张工单的原编号与新编号。
type ticketRemap struct {
	OldID string
	NewID string
}

// importOutcome 为一次成功导入的结果摘要，用于输出。
type importOutcome struct {
	assetIDs []string
	tickets  []ticketRemap
}

func cmdImport(args []string, w io.Writer) error {
	var opts cmdOptions
	var sourceDir string
	var assetIDs stringListFlag
	fs := newFlagSet("import", &opts)
	fs.StringVar(&sourceDir, "source-dir", "", "源数据目录（必填，只读，不会被修改）")
	fs.Var(&assetIDs, "asset-id", "要导入的资产编号（必填，可重复；重复编号按一项处理）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, sourceDir, "source-dir"); err != nil {
		return err
	}
	if len(assetIDs) == 0 {
		return &usageError{msg: "命令 import 缺少必填参数 --asset-id（至少一项资产编号）"}
	}
	for _, id := range assetIDs {
		if id == "" {
			return &usageError{msg: "命令 import 的 --asset-id 不能为空"}
		}
	}

	outcome, err := importAssets(opts.dataDir, sourceDir, []string(assetIDs))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "已导入 %d 项资产（%s）。\n", len(outcome.assetIDs), strings.Join(outcome.assetIDs, ", "))
	if len(outcome.tickets) == 0 {
		fmt.Fprintln(w, "所选资产没有工单，未分配新工单编号。")
		return nil
	}
	fmt.Fprintln(w, "工单编号映射（原编号 -> 新编号）:")
	for _, m := range outcome.tickets {
		fmt.Fprintf(w, "%s -> %s\n", m.OldID, m.NewID)
	}
	return nil
}

// importAssets 把 sourceDir 中编号为 assetIDs 的资产导入 targetDir。
// 源目录只读；目标目录无台账时在成功导入时创建。任何一步失败都整批失败，
// 两边原文件字节保持不变。
func importAssets(targetDir, sourceDir string, assetIDs []string) (*importOutcome, error) {
	srcAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("无法解析源数据目录: %w", err)
	}
	dstAbs, err := filepath.Abs(targetDir)
	if err != nil {
		return nil, fmt.Errorf("无法解析目标数据目录: %w", err)
	}
	if srcAbs == dstAbs {
		return nil, errors.New("源数据目录与目标数据目录相同：同一台账不能导入自身")
	}
	// 路径不同也可能指向同一台账文件（符号链接等），按文件再判一次。
	srcPath := filepath.Join(srcAbs, dataFileName)
	dstPath := filepath.Join(dstAbs, dataFileName)
	if fi1, err1 := os.Stat(srcPath); err1 == nil {
		if fi2, err2 := os.Stat(dstPath); err2 == nil && os.SameFile(fi1, fi2) {
			return nil, errors.New("源与目标指向同一台账文件：同一台账不能导入自身")
		}
	}

	// 源台账必须已存在并通过整库一致性检查；源始终只读，绝不写入。
	src, err := openSourceStore(srcAbs)
	if err != nil {
		return nil, err
	}
	// 目标：无台账时视为空库（成功导入时创建）；已有台账同样先做整库一致性检查。
	dst, err := openStore(dstAbs)
	if err != nil {
		return nil, err
	}
	outcome, err := dst.mergeImport(src, assetIDs)
	if err != nil {
		return nil, err
	}
	// save 先校验合并后的整库一致性，再一次性原子写入；失败时目标原文件不变。
	if err := dst.save(); err != nil {
		return nil, err
	}
	return outcome, nil
}

// mergeImport 在内存中把源台账的所选资产合并进目标数据：先做全部冲突与容量
// 检查，再一次性应用，任何失败都不会留下部分变更。调用方负责随后的原子保存。
func (s *store) mergeImport(src *store, assetIDs []string) (*importOutcome, error) {
	// 重复编号按一项处理，保持首次出现的顺序。
	selected := map[string]bool{}
	ids := make([]string, 0, len(assetIDs))
	for _, id := range assetIDs {
		if !selected[id] {
			selected[id] = true
			ids = append(ids, id)
		}
	}
	// 所选资产必须存在于源，且编号在目标中不存在。
	assets := make([]*Asset, 0, len(ids))
	for _, id := range ids {
		a := src.findAsset(id)
		if a == nil {
			return nil, fmt.Errorf("%w: 源台账中不存在资产编号 %q", errNotFound, id)
		}
		if s.findAsset(id) != nil {
			return nil, fmt.Errorf("%w: 资产编号 %q 在目标台账中已存在，整批拒绝导入", errConflict, id)
		}
		assets = append(assets, a)
	}
	// 所选资产的全部工单，按源工单序号升序重新分配编号。
	tickets := make([]*Ticket, 0)
	for _, t := range src.data.Tickets {
		if selected[t.AssetID] {
			tickets = append(tickets, t)
		}
	}
	sort.SliceStable(tickets, func(i, j int) bool {
		ni, _ := parseTicketSeq(tickets[i].ID)
		nj, _ := parseTicketSeq(tickets[j].ID)
		return ni < nj
	})
	// 工单编号容量：与 report 同一约束，可分配的最大序号为 math.MaxInt-1。
	if len(tickets) > math.MaxInt-s.data.NextTicketSeq {
		return nil, fmt.Errorf(
			"%w: 目标工单编号容量不足：下一序号 %d 无法容纳 %d 张导入工单，整批拒绝导入",
			errConflict, s.data.NextTicketSeq, len(tickets))
	}
	// 所选工单的任一请求标识已在目标绑定时整批拒绝，不改请求标识。
	srcReqByTicket := map[string]*requestBinding{}
	for i := range src.data.Requests {
		r := &src.data.Requests[i]
		srcReqByTicket[r.TicketID] = r
	}
	for _, t := range tickets {
		r := srcReqByTicket[t.ID]
		if r == nil {
			return nil, fmt.Errorf("数据内部错误：源工单 %s 缺少报修请求绑定", t.ID)
		}
		if s.findRequest(r.RequestID) != nil {
			return nil, fmt.Errorf(
				"%w: 所选工单的请求标识 %q 已在目标台账绑定，整批拒绝导入", errConflict, r.RequestID)
		}
	}
	// 所选资产的全部履历，按源履历序号升序，在目标最大序号之后依次分配。
	events := make([]Event, 0)
	for _, e := range src.data.Events {
		if selected[e.AssetID] {
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	// 履历序号容量：与 nextEventSeq 同一约束，可分配的最大序号为 math.MaxInt。
	if len(events) > math.MaxInt-maxSeq {
		return nil, fmt.Errorf(
			"%w: 目标履历序号容量不足：当前最大序号 %d 无法容纳 %d 条导入履历，整批拒绝导入",
			errConflict, maxSeq, len(events))
	}

	// 检查全部通过，一次性应用合并。资产编号、名称、位置、状态与工单的描述、
	// 终态内容、负责人等业务信息原样保留；履历时间保留原瞬间与小数秒精度。
	for _, a := range assets {
		na := *a
		s.data.Assets = append(s.data.Assets, &na)
	}
	// 所选资产的保养计划原样复制（含首次到期日、保养内容与间隔）。资产编号
	// 本身是新导入的，目标不可能已有同资产的计划，故无冲突需要处理；完成
	// 履历随下方的履历复制一并迁入，与计划的完成链保持续接。
	for _, p := range src.data.Plans {
		if !selected[p.AssetID] {
			continue
		}
		np := *p
		s.data.Plans = append(s.data.Plans, &np)
	}
	outcome := &importOutcome{assetIDs: ids}
	remap := map[string]string{}
	next := s.data.NextTicketSeq
	for _, t := range tickets {
		nt := *t
		nt.ID = fmt.Sprintf("T%04d", next)
		remap[t.ID] = nt.ID
		s.data.Tickets = append(s.data.Tickets, &nt)
		r := srcReqByTicket[t.ID]
		s.data.Requests = append(s.data.Requests, requestBinding{
			RequestID:   r.RequestID,
			AssetID:     r.AssetID,
			Description: r.Description,
			TicketID:    nt.ID,
		})
		outcome.tickets = append(outcome.tickets, ticketRemap{OldID: t.ID, NewID: nt.ID})
		next++
	}
	s.data.NextTicketSeq = next
	seq := maxSeq
	for _, e := range events {
		seq++
		ne := e
		ne.Seq = seq
		ne.TicketID = remap[e.TicketID]
		s.data.Events = append(s.data.Events, ne)
	}
	return outcome, nil
}
