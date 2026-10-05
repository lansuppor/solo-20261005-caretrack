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
// 报修、派工、关闭、取消履历一起复制到目标数据目录。导入是复制：
// 源台账始终只读，不删除原记录；同一台账不能导入自身；导入本身不追加
// 任何报修或其他业务事件。

// stringListFlag 收集可重复出现的 --asset-id 参数。
type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// ticketIDMapping 记录一张工单从源编号到目标新编号的映射。
type ticketIDMapping struct {
	OldID string
	NewID string
}

// importResult 汇总一次成功导入：资产数量与每张工单的原编号、新编号。
type importResult struct {
	AssetCount int
	Mappings   []ticketIDMapping
}

// importAssets 把 src 中所选资产（重复编号按一项处理）及其工单、请求绑定、
// 履历复制进 dst 的内存数据。所有检查都在修改之前完成：任何错误都保证 dst
// 数据完全未被修改——不留下部分资产、履历或请求绑定，也不消耗目标编号。
// 调用方负责随后 save，由 save 在提交前再次校验合并后的数据并原子写入。
func importAssets(dst, src *store, assetIDs []string) (*importResult, error) {
	// 去重并保持首次出现的顺序。
	ids := make([]string, 0, len(assetIDs))
	selected := map[string]bool{}
	for _, id := range assetIDs {
		if !selected[id] {
			selected[id] = true
			ids = append(ids, id)
		}
	}
	// 所选资产必须都存在于源、都不存在于目标；任一冲突即整批拒绝，
	// 不覆盖、不合并。
	assets := make([]*Asset, 0, len(ids))
	for _, id := range ids {
		a := src.findAsset(id)
		if a == nil {
			return nil, fmt.Errorf("%w: 源数据目录中不存在资产编号 %q", errNotFound, id)
		}
		if dst.findAsset(id) != nil {
			return nil, fmt.Errorf("%w: 资产编号 %q 在目标中已存在，整批拒绝导入", errConflict, id)
		}
		assets = append(assets, a)
	}

	// 所选资产的全部工单，按源工单序号升序重新编号。
	tickets := make([]*Ticket, 0)
	for _, t := range src.data.Tickets {
		if selected[t.AssetID] {
			tickets = append(tickets, t)
		}
	}
	sort.SliceStable(tickets, func(i, j int) bool {
		si, _ := parseTicketSeq(tickets[i].ID)
		sj, _ := parseTicketSeq(tickets[j].ID)
		return si < sj
	})
	// 所选工单的任一请求标识已在目标绑定时整批拒绝：不覆盖、不合并、
	// 不改请求标识。
	for _, t := range tickets {
		if dst.findRequest(t.RequestID) != nil {
			return nil, fmt.Errorf("%w: 请求标识 %q 已在目标中绑定，整批拒绝导入", errConflict, t.RequestID)
		}
	}

	// 所选资产的全部履历，按源履历序号排列：保留原操作顺序，不按时间排序。
	events := make([]Event, 0)
	for _, e := range src.data.Events {
		if selected[e.AssetID] {
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	// 容量检查必须先于任何修改：编号或序号容量不足时整批失败，不消耗编号。
	if n := len(tickets); n > 0 && n > math.MaxInt-dst.data.NextTicketSeq {
		return nil, fmt.Errorf("%w: 目标工单编号容量不足，无法为 %d 张工单分配新编号", errConflict, n)
	}
	maxEventSeq := 0
	for _, e := range dst.data.Events {
		if e.Seq > maxEventSeq {
			maxEventSeq = e.Seq
		}
	}
	if m := len(events); m > 0 && m > math.MaxInt-maxEventSeq {
		return nil, fmt.Errorf("%w: 目标履历序号容量不足，无法为 %d 条履历分配新序号", errConflict, m)
	}

	// 以下开始修改目标内存数据：此后不会再失败。
	// 工单编号从目标下一工单序号重新分配，并同步替换履历与请求绑定中的
	// 工单引用；资产编号、名称、位置、状态与工单业务信息原样保留。
	newID := map[string]string{}
	res := &importResult{AssetCount: len(assets)}
	for i, t := range tickets {
		seq := dst.data.NextTicketSeq + i
		id := fmt.Sprintf("T%04d", seq)
		newID[t.ID] = id
		res.Mappings = append(res.Mappings, ticketIDMapping{OldID: t.ID, NewID: id})
		cp := *t
		cp.ID = id
		dst.data.Tickets = append(dst.data.Tickets, &cp)
		dst.data.Requests = append(dst.data.Requests, requestBinding{
			RequestID:   t.RequestID,
			AssetID:     t.AssetID,
			Description: t.Description,
			TicketID:    id,
		})
	}
	dst.data.NextTicketSeq += len(tickets)
	for _, a := range assets {
		cp := *a
		dst.data.Assets = append(dst.data.Assets, &cp)
	}
	// 履历在目标已有最大序号之后依次分配新序号；时间保留原瞬间与小数秒
	// 精度，不替换为导入时间。
	for i, e := range events {
		e.Seq = maxEventSeq + 1 + i
		e.TicketID = newID[e.TicketID]
		dst.data.Events = append(dst.data.Events, e)
	}
	return res, nil
}

// sameLedger 判断两个数据目录是否指向同一台账。
func sameLedger(srcDir, dstDir string) (bool, error) {
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return false, fmt.Errorf("解析源数据目录路径失败: %w", err)
	}
	absDst, err := filepath.Abs(dstDir)
	if err != nil {
		return false, fmt.Errorf("解析目标数据目录路径失败: %w", err)
	}
	if absSrc == absDst {
		return true, nil
	}
	// 路径不同也可能指向同一文件（如符号链接）：两边台账都存在时比较文件身份。
	fiSrc, errSrc := os.Stat(filepath.Join(absSrc, dataFileName))
	fiDst, errDst := os.Stat(filepath.Join(absDst, dataFileName))
	if errSrc == nil && errDst == nil && os.SameFile(fiSrc, fiDst) {
		return true, nil
	}
	return false, nil
}

// cmdImport 批量导入：caretrack import --source-dir 源目录 --asset-id 编号
// [--asset-id 编号 ...] [--data-dir 目标目录]。
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
		return &usageError{msg: "命令 import 缺少必填参数 --asset-id（至少一个资产编号）"}
	}
	for _, id := range assetIDs {
		if id == "" {
			return &usageError{msg: "命令 import 的 --asset-id 不能为空"}
		}
	}

	// 同一台账不能导入自身。
	same, err := sameLedger(sourceDir, opts.dataDir)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("%w: 源数据目录与目标数据目录是同一台账，不能导入自身", errConflict)
	}

	// 源台账必须已存在：源不存在时不能当空库初始化。
	srcPath := filepath.Join(sourceDir, dataFileName)
	if _, err := os.Stat(srcPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("源数据文件 %s 不存在：源台账不存在时不能当空库导入", srcPath)
		}
		return fmt.Errorf("读取源数据目录失败: %w", err)
	}
	// 操作前检查源整库一致性（openStore 含损坏与关联矛盾检查，只读）。
	src, err := openStore(sourceDir)
	if err != nil {
		return err
	}
	// 操作前检查目标整库一致性；目标无台账时视为空库，成功导入时创建。
	dst, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}

	res, err := importAssets(dst, src, assetIDs)
	if err != nil {
		return err
	}
	// 提交前校验合并后的数据，整批变化一次原子写入目标。
	if err := dst.save(); err != nil {
		return err
	}

	fmt.Fprintf(w, "已导入资产数量: %d\n", res.AssetCount)
	for _, m := range res.Mappings {
		fmt.Fprintf(w, "工单 %s -> %s\n", m.OldID, m.NewID)
	}
	return nil
}
