package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// 本地清单批量报修：从 --file 指定的 JSON 清单一次提交多项设备故障报修，
// 整批原子生效。清单文件只读，不会被修改。
//
// 清单格式：JSON 数组，每项为一个对象，含三个非空字符串字段——
// asset_id（资产编号）、description（故障描述）、request_id（报修请求标识）。
// 空清单、JSON 格式错误、含未知字段或缺少必填内容均整批拒绝（退出码 2）。
//
// 请求标识仍仅用于同一数据目录内的报修去重，不引入批次标识：
//   - 批内同一请求标识搭配相同资产与描述的重复项合并为一项（按首次出现处理，
//     不消耗编号）；同一标识搭配不同资产或描述——无论冲突来自批内还是已有
//     绑定——均整批拒绝。
//   - 已有相同绑定的项为重放：返回原工单及当前状态，不新增履历；即使原单已
//     关闭或取消、资产停用或已有后来工单，重放也不重开原单、不改变资产或
//     后来工单。纯重放为只读：不写文件、不初始化目录，编号或履历容量耗尽也
//     不妨碍合法重放。
//   - 其余项按 report 规则创建工单：未知资产、停用资产、已有未关闭工单（含
//     本批前项刚开出的工单）均整批拒绝；旧请求重放与该资产的一项合法新报修
//     可以共存。新工单按清单首次出现顺序分配连续工单编号与报修履历序号，
//     每张新单恰有一条报修履历，所属资产变为“维修中”，不追加批次业务事件。
//
// 含新请求时，整批工单、状态、履历与请求绑定一次原子保存后才输出成功结果。
// 任一项不合法、容量不足或读写失败均整批失败并指出清单项位置，不输出部分
// 成功结果：已有台账与输入清单字节不变，不留下部分记录或请求绑定、不消耗
// 编号，恢复条件后可重试。

// manifestItem 为报修清单中的一项报修请求。
type manifestItem struct {
	AssetID     string `json:"asset_id"`
	Description string `json:"description"`
	RequestID   string `json:"request_id"`
}

// readBatchManifest 完整读取并校验报修清单。文件读取失败为读写失败（退出码 1）；
// 空清单、格式错误、缺少必填内容或含未知字段为清单格式错误（*usageError，退出码 2）。
// 清单文件只读，任何结果都不会修改它。
func readBatchManifest(path string) ([]manifestItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取报修清单失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &usageError{msg: fmt.Sprintf(
			"报修清单 %s 为空：清单须为至少含一项报修的 JSON 数组", path)}
	}
	var items []manifestItem
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&items); err != nil {
		return nil, &usageError{msg: fmt.Sprintf(
			"报修清单格式错误: %s（清单须为 JSON 数组，每项含非空的 asset_id、description、request_id）", err)}
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, &usageError{msg: "报修清单格式错误：JSON 数组之后存在多余内容"}
		}
		return nil, &usageError{msg: fmt.Sprintf("报修清单格式错误: %s", err)}
	}
	if len(items) == 0 {
		return nil, &usageError{msg: "报修清单为空：至少须包含一项报修"}
	}
	for i, it := range items {
		pos := i + 1
		switch {
		case it.AssetID == "":
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：资产编号（asset_id）不能为空", pos)}
		case it.Description == "":
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：故障描述（description）不能为空", pos)}
		case it.RequestID == "":
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：报修请求标识（request_id）不能为空", pos)}
		}
	}
	return items, nil
}

// batchOutcome 为批量报修中一个唯一请求标识的处理结果，用于输出。
type batchOutcome struct {
	RequestID string
	AssetID   string
	TicketID  string
	Status    string
	Replayed  bool
}

// batchReport 按清单顺序处理全部报修项，返回每个唯一请求标识（按首次出现
// 顺序）的结果与新增工单数。所有校验与冲突检查任一失败都整批失败：内存中的
// 部分变更随台账对象一并丢弃，不保存、不消耗编号、不绑定请求标识。
func (s *store) batchReport(items []manifestItem) ([]batchOutcome, int, error) {
	// 批内去重：同一请求标识首次出现时记录；相同资产与描述的重复项合并为一项，
	// 搭配不同资产或描述的整批拒绝。
	type firstSeen struct {
		pos         int
		assetID     string
		description string
	}
	seen := map[string]firstSeen{}
	unique := make([]manifestItem, 0, len(items))
	positions := make([]int, 0, len(items))
	for i, it := range items {
		pos := i + 1
		if prev, ok := seen[it.RequestID]; ok {
			if prev.assetID != it.AssetID || prev.description != it.Description {
				return nil, 0, fmt.Errorf(
					"清单第 %d 项与第 %d 项的请求标识 %q 相同，但资产编号或故障描述不同，整批拒绝",
					pos, prev.pos, it.RequestID)
			}
			continue // 重复项合并，不消耗编号
		}
		seen[it.RequestID] = firstSeen{pos: pos, assetID: it.AssetID, description: it.Description}
		unique = append(unique, it)
		positions = append(positions, pos)
	}

	outcomes := make([]batchOutcome, 0, len(unique))
	newCount := 0
	for i, it := range unique {
		// 与单项 report 共享同一套规则与请求绑定：已有相同绑定返回原工单
		// （重放），否则按原报修规则开单；同资产第二张新单会撞上本批前项
		// 刚开出的未关闭工单而整批拒绝。
		t, replay, err := s.report(it.AssetID, it.Description, it.RequestID)
		if err != nil {
			return nil, 0, fmt.Errorf("清单第 %d 项（请求标识 %q）: %s",
				positions[i], it.RequestID, friendlyErr(err.Error()))
		}
		if !replay {
			newCount++
		}
		outcomes = append(outcomes, batchOutcome{
			RequestID: it.RequestID,
			AssetID:   it.AssetID,
			TicketID:  t.ID,
			Status:    t.Status,
			Replayed:  replay,
		})
	}
	return outcomes, newCount, nil
}

// cmdBatchReport 批量报修命令入口。清单格式错误为退出码 2；业务冲突、容量
// 不足与读写失败为退出码 1。含新请求时整批一次原子保存后才输出成功结果；
// 纯重放为只读，不写文件、不初始化目录。
func cmdBatchReport(args []string, w io.Writer) error {
	var opts cmdOptions
	var file string
	fs := newFlagSet("batch-report", &opts)
	fs.StringVar(&file, "file", "", "报修清单文件路径（必填；JSON 数组，每项含非空的 asset_id、description、request_id；清单只读）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, file, "file"); err != nil {
		return err
	}
	items, err := readBatchManifest(file)
	if err != nil {
		return err
	}
	// openStore 已先做整库一致性检查：损坏或关联矛盾按原规则拒绝（退出码 1）。
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	outcomes, newCount, err := s.batchReport(items)
	if err != nil {
		return err
	}
	if newCount > 0 {
		// 整批工单、状态、履历与请求绑定一次原子保存（保存前再次校验整库
		// 一致性）；失败时原文件字节不变，不留下部分记录，恢复后可重试。
		if err := s.save(); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "批量报修完成：共 %d 项请求（新增 %d 项，重放 %d 项）。\n",
		len(outcomes), newCount, len(outcomes)-newCount)
	for _, o := range outcomes {
		kind := "新增"
		if o.Replayed {
			kind = "重放"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", o.RequestID, o.AssetID, o.TicketID, o.Status, kind)
	}
	if newCount == 0 {
		fmt.Fprintln(w, "全部为既有请求的相同重放，未创建工单、未写入任何记录。")
	}
	return nil
}
