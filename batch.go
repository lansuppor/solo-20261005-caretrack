package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// 本地清单批量报修：从只读清单文件一次提交多项设备故障报修。
//
// 清单格式（JSON）：一个非空数组，每项为对象，含三个非空字符串字段——
// asset_id（资产编号）、description（故障描述）、request_id（报修请求标识）。
// 空清单、格式错误或缺少必填内容均拒绝（退出码 2）；清单文件只读，绝不写入。
//
// 关键规则：
//   - 请求标识仍仅用于同一数据目录内的报修去重，不引入批次标识。批内同标识、
//     同资产、同描述的重复项合并为一项；同标识搭配不同资产或描述，无论冲突
//     来自批内还是已有绑定，均整批拒绝。
//   - 已有相同绑定（标识、资产、描述一致）的项为重放：返回原工单及当前状态，
//     不新增履历；即使原单已关闭或取消、资产停用或已有后来工单，重放也不
//     重开原单、不改变资产或后来工单。
//   - 未绑定的请求按 report 的原有规则创建：未知资产、停用资产、已有未关闭
//     工单均拒绝；同一资产在本批出现两个不同的新请求同样整批拒绝（即使描述
//     相同）；旧请求重放与该资产的一项合法新报修可以共存。
//   - 各项新请求按清单中首次出现顺序分配连续工单编号与报修履历序号，重复项
//     与重放不消耗编号。含新请求时整批工单、状态、履历与请求绑定一次原子
//     保存后才输出成功结果；纯重放为只读，不写文件、不初始化目录，编号或
//     履历容量耗尽也不妨碍合法重放。
//   - 任一项不合法、容量不足或读写失败均整批失败并说明原因，涉及清单项时
//     指出其位置；不输出部分成功结果，已有台账与输入清单字节不变，不留下
//     部分记录或请求绑定、不消耗编号，恢复条件后可重试。

// batchItem 为清单中一项报修请求（已按首次出现合并重复项后仍保留首次位置）。
type batchItem struct {
	Pos         int // 在清单中的位置（从 1 开始）
	AssetID     string
	Description string
	RequestID   string
}

// batchResult 为批量报修成功后的单项结果，按请求标识首次出现顺序输出。
type batchResult struct {
	RequestID string
	AssetID   string
	TicketID  string
	Status    string
	Replayed  bool // true 为去重重放（既有工单），false 为本批新增
}

// manifestEntry 为清单 JSON 数组中的一项。
type manifestEntry struct {
	AssetID     string `json:"asset_id"`
	Description string `json:"description"`
	RequestID   string `json:"request_id"`
}

// parseBatchManifest 完整读取并校验清单内容：必须是一个非空 JSON 数组，
// 每项含非空的 asset_id、description、request_id。任何格式问题都返回
// 指明原因（涉及清单项时指出其位置）的错误；调用方据此以退出码 2 结束。
func parseBatchManifest(raw []byte) ([]batchItem, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("清单文件为空：应为 JSON 数组，每项含 asset_id、description、request_id")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries []manifestEntry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("清单格式错误：%v（应为 JSON 数组，每项含 asset_id、description、request_id）", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("清单格式错误：JSON 数组之后还有多余内容")
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("清单为空：至少需要一项报修请求")
	}
	items := make([]batchItem, len(entries))
	for i, e := range entries {
		pos := i + 1
		if e.AssetID == "" {
			return nil, fmt.Errorf("清单第 %d 项：资产编号 asset_id 不能为空", pos)
		}
		if e.Description == "" {
			return nil, fmt.Errorf("清单第 %d 项：故障描述 description 不能为空", pos)
		}
		if e.RequestID == "" {
			return nil, fmt.Errorf("清单第 %d 项：报修请求标识 request_id 不能为空", pos)
		}
		items[i] = batchItem{Pos: pos, AssetID: e.AssetID, Description: e.Description, RequestID: e.RequestID}
	}
	return items, nil
}

// batchReport 执行一批报修请求：先做全部去重、冲突、业务与容量检查，再一次
// 性应用全部新请求。任何失败都不修改内存中的业务数据（调用方亦不保存），
// 因此整批失败不会留下部分记录、请求绑定或消耗编号。
// 返回按请求标识首次出现顺序排列的结果（含重放项）。
func (s *store) batchReport(items []batchItem) ([]batchResult, error) {
	// 批内去重：同标识、同资产、同描述的重复项合并为一项（保留首次位置）；
	// 同标识搭配不同资产或描述整批拒绝。
	seen := map[string]batchItem{}
	unique := make([]batchItem, 0, len(items))
	for _, it := range items {
		if prev, ok := seen[it.RequestID]; ok {
			if prev.AssetID != it.AssetID || prev.Description != it.Description {
				return nil, fmt.Errorf(
					"%w: 清单第 %d 项与第 %d 项的请求标识 %q 相同，但资产编号或故障描述不同，整批拒绝",
					errConflict, it.Pos, prev.Pos, it.RequestID)
			}
			continue
		}
		seen[it.RequestID] = it
		unique = append(unique, it)
	}

	// 逐项分类：已有绑定为重放（须与绑定完全一致），否则为待创建的新请求。
	// 所有检查通过前不修改任何业务数据。
	type planned struct {
		item batchItem
	}
	replays := map[string]*Ticket{} // requestID -> 原工单
	news := make([]planned, 0)
	plannedNew := map[string]int{} // 本批新请求占用的资产 -> 首次清单位置
	for _, it := range unique {
		if r := s.findRequest(it.RequestID); r != nil {
			if r.AssetID != it.AssetID || r.Description != it.Description {
				return nil, fmt.Errorf(
					"%w: 清单第 %d 项：请求标识 %q 已用于资产 %q 的另一笔报修，不能搭配不同的资产编号或故障描述，整批拒绝",
					errConflict, it.Pos, it.RequestID, r.AssetID)
			}
			t := s.findTicket(r.TicketID)
			if t == nil {
				return nil, fmt.Errorf("数据内部错误：请求标识 %q 绑定的工单不存在", it.RequestID)
			}
			replays[it.RequestID] = t
			continue
		}
		asset := s.findAsset(it.AssetID)
		if asset == nil {
			return nil, fmt.Errorf("%w: 清单第 %d 项：未知资产编号 %q", errNotFound, it.Pos, it.AssetID)
		}
		if asset.Status == statusDeactivated {
			return nil, fmt.Errorf("%w: 清单第 %d 项：资产 %s 已停用，不能报修", errConflict, it.Pos, it.AssetID)
		}
		if t := s.openTicketOf(it.AssetID); t != nil {
			return nil, fmt.Errorf("%w: 清单第 %d 项：资产 %s 已有未关闭工单 %s", errConflict, it.Pos, it.AssetID, t.ID)
		}
		if firstPos, ok := plannedNew[it.AssetID]; ok {
			return nil, fmt.Errorf(
				"%w: 清单第 %d 项与第 %d 项都是对资产 %s 的新报修请求：同一资产在一批中只能有一项新请求，整批拒绝",
				errConflict, it.Pos, firstPos, it.AssetID)
		}
		plannedNew[it.AssetID] = it.Pos
		news = append(news, planned{item: it})
	}

	// 容量检查：与 report 同一约束。工单编号可分配的最大序号为 math.MaxInt-1，
	// 履历序号可分配的最大序号为 math.MaxInt；纯重放不做任何容量检查。
	if n := len(news); n > 0 {
		if n > math.MaxInt-s.data.NextTicketSeq {
			return nil, fmt.Errorf(
				"%w: 工单编号容量不足：下一序号 %d 无法容纳本批 %d 项新报修，整批拒绝",
				errConflict, s.data.NextTicketSeq, n)
		}
		maxSeq := 0
		for _, e := range s.data.Events {
			if e.Seq > maxSeq {
				maxSeq = e.Seq
			}
		}
		if n > math.MaxInt-maxSeq {
			return nil, fmt.Errorf(
				"%w: 履历序号容量不足：当前最大序号 %d 无法容纳本批 %d 条报修履历，整批拒绝",
				errConflict, maxSeq, n)
		}
		// 全部检查通过，按清单首次出现顺序一次性应用：连续分配工单编号与
		// 履历序号，每项新请求恰有一条报修履历，所属资产转为维修中。
		seq := s.data.NextTicketSeq
		eventSeq := maxSeq
		for _, p := range news {
			it := p.item
			eventSeq++
			t := &Ticket{
				ID:          fmt.Sprintf("T%04d", seq),
				AssetID:     it.AssetID,
				Description: it.Description,
				RequestID:   it.RequestID,
				Status:      ticketOpen,
				CreatedAt:   s.now().Format(time.RFC3339),
			}
			s.data.NextTicketSeq = seq + 1
			s.data.Tickets = append(s.data.Tickets, t)
			s.data.Requests = append(s.data.Requests, requestBinding{
				RequestID:   it.RequestID,
				AssetID:     it.AssetID,
				Description: it.Description,
				TicketID:    t.ID,
			})
			s.findAsset(it.AssetID).Status = statusRepairing
			s.appendEvent(eventSeq, it.AssetID, t.ID, eventReport, it.Description, "", "")
			seq++
		}
	}

	// 汇总结果：按请求标识首次出现顺序，每个标识只出现一次。
	results := make([]batchResult, 0, len(unique))
	for _, it := range unique {
		if t, ok := replays[it.RequestID]; ok {
			results = append(results, batchResult{
				RequestID: it.RequestID, AssetID: it.AssetID,
				TicketID: t.ID, Status: t.Status, Replayed: true,
			})
			continue
		}
		t := s.findTicketByRequest(it.RequestID)
		results = append(results, batchResult{
			RequestID: it.RequestID, AssetID: it.AssetID,
			TicketID: t.ID, Status: t.Status,
		})
	}
	return results, nil
}

// findTicketByRequest 返回指定请求标识绑定的工单；仅用于批量报修应用后汇总。
func (s *store) findTicketByRequest(requestID string) *Ticket {
	if r := s.findRequest(requestID); r != nil {
		return s.findTicket(r.TicketID)
	}
	return nil
}

// cmdBatchReport 从本地清单文件批量报修。清单只读；含新请求时整批一次原子
// 保存后才输出成功结果，纯重放不写文件。清单格式错误为退出码 2，业务或
// 读写失败为退出码 1。
func cmdBatchReport(args []string, w io.Writer) error {
	var opts cmdOptions
	var file string
	fs := newFlagSet("batch-report", &opts)
	fs.StringVar(&file, "file", "", "报修清单文件路径（必填，JSON 数组，每项含 asset_id、description、request_id；只读）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, file, "file"); err != nil {
		return err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("读取清单文件失败: %w", err)
	}
	items, err := parseBatchManifest(raw)
	if err != nil {
		return &usageError{msg: err.Error()}
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	results, err := s.batchReport(items)
	if err != nil {
		return err
	}
	created := 0
	for _, r := range results {
		if !r.Replayed {
			created++
		}
	}
	// 含新请求时整批一次原子保存；纯重放为只读，不写文件、不初始化目录。
	if created > 0 {
		if err := s.save(); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "批量报修完成：共 %d 项请求（新增 %d 项，重放 %d 项）。\n",
		len(results), created, len(results)-created)
	for _, r := range results {
		kind := "新增"
		if r.Replayed {
			kind = "重放"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.RequestID, r.AssetID, r.TicketID, r.Status, kind)
	}
	return nil
}
