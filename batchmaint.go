package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

// 本地清单批量登记保养完成：从 --file 指定的 JSON 清单一次登记多项保养完成，
// 整批原子生效。清单文件只读，不会被修改。
//
// 清单格式：JSON 数组，每项为一个对象，含非空的 asset_id（资产编号）、due
// （周期到期日，YYYY-MM-DD）、done（实际完成日，YYYY-MM-DD）、result（保养
// 结果）与正整数 segment_seq（方案段序号）。方案段以本目录建立或调整履历的
// 全库序号标识（history 可查）；只接受该资产当前方案段，同日期的旧段也拒绝。
// 空清单、JSON 格式错误、含未知字段、缺少必填内容、非正整数段序号或非法日期
// 均整批拒绝（退出码 2），并指出清单项位置。
//
// 按清单顺序逐项处理，允许资产交错与同一资产多次出现：每项的到期日须等于处理
// 该项时的下一到期日，后项接续前项推进后的日期；完成日不得早于到期日，推进
// 规则与 maintain 相同（按当前段首次日加整数倍间隔推进到严格晚于完成日的最早
// 日期，延期跳过的周期不补记录，超过 9999-12-31 整批拒绝）。未知资产、无计划、
// 停用资产均整批拒绝；维修中（含待验收）资产仍可登记，工单及资产状态不变。
// 清单项不合并、不按报修请求标识去重，同一周期重复出现整批失败。
//
// 每项追加一条普通完成履历，按清单顺序分配全库序号，不追加批次事件。整批一次
// 原子保存后才逐项输出项号、资产、周期、完成序号与推进后的下一到期日。任一项
// 失败、履历容量不足或读写失败均整批失败，不输出部分成功结果：已有台账与输入
// 清单字节不变，不推进计划、不留履历、不消耗序号，恢复条件后可用原清单重试。

// maintainItem 为保养清单中的一项保养完成登记。
type maintainItem struct {
	AssetID    string `json:"asset_id"`
	Due        string `json:"due"`
	Done       string `json:"done"`
	Result     string `json:"result"`
	SegmentSeq int    `json:"segment_seq"`
}

// readMaintainManifest 完整读取并校验保养清单。文件读取失败为读写失败（退出码 1）；
// 空清单、格式错误、缺少必填内容、非正整数段序号或非法日期为清单格式错误
// （*usageError，退出码 2）。清单文件只读，任何结果都不会修改它。
func readMaintainManifest(path string) ([]maintainItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取保养清单失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &usageError{msg: fmt.Sprintf(
			"保养清单 %s 为空：清单须为至少含一项保养完成的 JSON 数组", path)}
	}
	var items []maintainItem
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&items); err != nil {
		return nil, &usageError{msg: fmt.Sprintf(
			"保养清单格式错误: %s（清单须为 JSON 数组，每项含非空的 asset_id、due、done、result 与正整数 segment_seq）", err)}
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, &usageError{msg: "保养清单格式错误：JSON 数组之后存在多余内容"}
		}
		return nil, &usageError{msg: fmt.Sprintf("保养清单格式错误: %s", err)}
	}
	if len(items) == 0 {
		return nil, &usageError{msg: "保养清单为空：至少须包含一项保养完成"}
	}
	for i, it := range items {
		pos := i + 1
		switch {
		case it.AssetID == "":
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：资产编号（asset_id）不能为空", pos)}
		case it.Result == "":
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：保养结果（result）不能为空", pos)}
		case it.SegmentSeq < 1:
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：方案段序号（segment_seq）须为正整数", pos)}
		}
		if _, err := parseDate(it.Due); err != nil {
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：周期到期日（due）无效：%s", pos, err)}
		}
		if _, err := parseDate(it.Done); err != nil {
			return nil, &usageError{msg: fmt.Sprintf("清单第 %d 项：实际完成日（done）无效：%s", pos, err)}
		}
	}
	return items, nil
}

// maintainOutcome 为批量保养登记中一项的处理结果，用于输出。
type maintainOutcome struct {
	Pos     int    // 清单项号（从 1 开始）
	AssetID string // 资产编号
	Due     string // 完成的周期到期日
	Seq     int    // 完成履历的全库序号
	NextDue string // 该项推进后的下一到期日
}

// batchMaintain 按清单顺序登记全部保养完成项，返回每项（按清单顺序）的结果。
// 所有校验与规则判定任一失败都整批失败：内存中的部分变更随台账对象一并丢弃，
// 不保存、不推进计划、不留履历、不消耗序号。
func (s *store) batchMaintain(items []maintainItem) ([]maintainOutcome, error) {
	core, err := s.maintCore()
	if err != nil {
		return nil, err
	}
	// 每资产当前方案段的标识：最近一次建立或调整履历的全库序号（段边界按全库
	// 序号划分，不按日期或数组位置；数组乱序、序号间隔均不影响）。
	segmentSeq := map[string]int{}
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		if e.Kind == eventPlanCreate || e.Kind == eventPlanAdjust {
			if e.Seq > segmentSeq[e.AssetID] {
				segmentSeq[e.AssetID] = e.Seq
			}
		}
	}
	// 履历序号容量：本批每项各消耗一个连续序号（maxSeq+1 … maxSeq+len(items)），
	// 先整体确认容量，任何失败路径都不消耗序号。
	if len(items) > math.MaxInt-maxSeq {
		return nil, fmt.Errorf(
			"%w: 履历序号计数器容量不足，无法为本批 %d 项各分配一个履历序号", errConflict, len(items))
	}
	outcomes := make([]maintainOutcome, 0, len(items))
	for i, it := range items {
		pos := i + 1
		if s.findAsset(it.AssetID) == nil {
			return nil, fmt.Errorf("清单第 %d 项：未知资产编号 %q", pos, it.AssetID)
		}
		if s.findPlan(it.AssetID) == nil {
			return nil, fmt.Errorf("清单第 %d 项：资产 %s 没有保养计划", pos, it.AssetID)
		}
		if core.deactivatedNow(it.AssetID) {
			// 停用期间拒绝保养完成登记；停用不暂停或重算周期，保存的下一到期日不变。
			return nil, fmt.Errorf("清单第 %d 项：资产 %s 已停用，不能登记保养完成", pos, it.AssetID)
		}
		if it.SegmentSeq != segmentSeq[it.AssetID] {
			return nil, fmt.Errorf(
				"清单第 %d 项：方案段序号 %d 不是资产 %s 的当前方案段（当前段序号为 %d），旧方案段不能登记",
				pos, it.SegmentSeq, it.AssetID, segmentSeq[it.AssetID])
		}
		// 候选履历不含序号（核心判定不依赖序号）；接续、完成日不早于到期日与
		// 日期范围判定全部由共享核心完成，后项自然接续前项推进后的日期。
		cand := Event{AssetID: it.AssetID, Kind: eventPlanDone, Content: it.Result, Due: it.Due, Done: it.Done}
		if err := core.apply(cand); err != nil {
			return nil, fmt.Errorf("清单第 %d 项: %s", pos, friendlyErr(maintOpError(err).Error()))
		}
		_, _, _, next, _ := core.derived(it.AssetID)
		outcomes = append(outcomes, maintainOutcome{
			Pos: pos, AssetID: it.AssetID, Due: it.Due, Seq: maxSeq + i + 1, NextDue: next,
		})
	}
	// 全部判定通过后才修改业务数据：按清单顺序追加普通完成履历（不追加批次
	// 事件），并把每个涉及资产的计划推进到本批处理后的最终下一到期日。
	for i, it := range items {
		s.appendMaintEvent(maxSeq+i+1, it.AssetID, eventPlanDone, it.Result, it.Due, it.Done, 0)
	}
	advanced := map[string]bool{}
	for _, it := range items {
		if advanced[it.AssetID] {
			continue
		}
		advanced[it.AssetID] = true
		_, _, _, next, _ := core.derived(it.AssetID)
		s.findPlan(it.AssetID).NextDue = next
	}
	return outcomes, nil
}

// cmdBatchMaintain 批量保养完成登记命令入口。清单格式错误为退出码 2；业务冲突、
// 容量不足与读写失败为退出码 1。整批一次原子保存后才输出成功结果。
func cmdBatchMaintain(args []string, w io.Writer) error {
	var opts cmdOptions
	var file string
	fs := newFlagSet("batch-maintain", &opts)
	fs.StringVar(&file, "file", "", "保养清单文件路径（必填；JSON 数组，每项含非空的 asset_id、due、done、result 与正整数 segment_seq；清单只读）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, file, "file"); err != nil {
		return err
	}
	items, err := readMaintainManifest(file)
	if err != nil {
		return err
	}
	// openStore 已先做整库一致性检查：损坏或关联矛盾按原规则拒绝（退出码 1）。
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	outcomes, err := s.batchMaintain(items)
	if err != nil {
		return err
	}
	// 整批完成履历与计划推进一次原子保存（保存前再次校验整库一致性）；失败时
	// 原文件字节不变，不留履历、不消耗序号，恢复条件后可用原清单重试。
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "批量保养登记完成：共 %d 项。\n", len(outcomes))
	for _, o := range outcomes {
		fmt.Fprintf(w, "%d\t%s\t周期 %s\t完成序号 %d\t下一到期日 %s\n",
			o.Pos, o.AssetID, o.Due, o.Seq, o.NextDue)
	}
	return nil
}
