package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// 本地清单批量登记保养完成：从 --file 指定的 JSON 清单按顺序登记多项周期
// 保养完成，整批原子生效。清单文件只读，不会被修改。
//
// 清单格式：JSON 数组，每项为一个对象，含五个字段——
// asset_id（非空资产编号）、due（周期到期日）、done（实际完成日）、
// result（非空保养结果）与 segment_seq（方案段序号，正整数）。
// 方案段以本数据目录中该资产“保养建立/保养调整”履历的全库序号标识，
// history 可查；只接受资产的当前段——即使旧段的首次到期日与当前段相同
// （同日期的旧段）也拒绝。日期均为 0001 至 9999 年的有效公历 YYYY-MM-DD。
// 空清单、JSON 格式错误、含未知字段、缺项、段序号不是正整数或日期非法均
// 整批拒绝（退出码 2），并指出涉及的项号；文件读取失败为读写失败（退出码 1）。
//
// 业务处理严格按清单顺序进行，允许资产交错、同一资产多次出现：每项的周期
// 到期日须等于处理该项时该资产的下一到期日（后项接续前项推进后的日期），
// 实际完成日不得早于周期到期日；下一到期日按当前段首次日加整数倍间隔推进到
// 严格晚于完成日的最早日期，延期跳过的周期不补记录，推算结果超出
// 9999-12-31 整批拒绝。未知资产、无计划、停用资产拒绝；维修中（含待验收）
// 仍可登记，工单与资产状态不变。清单项不合并、不按任何报修请求标识去重，
// 同一周期重复出现会在后续项撞上已推进的下一到期日而整批失败。
//
// 每项追加一条普通“保养完成”履历，按清单顺序分配全库履历序号，不追加批次
// 事件。整批计划推进与履历一次原子保存后才逐项输出项号、资产、周期、完成
// 序号及该项推进后的下一到期日。任一项失败、容量不足或读写失败均无部分
// 成功输出：台账与清单字节保持，不推进计划、不留履历、不消耗序号；重载并
// 恢复条件后可用原清单重试。

// maintManifestItem 为保养完成清单中的一项登记。
type maintManifestItem struct {
	AssetID string `json:"asset_id"`
	Due     string `json:"due"`
	Done    string `json:"done"`
	Result  string `json:"result"`
	Segment int    `json:"segment_seq"`
}

// readMaintManifest 完整读取并校验保养完成清单。文件读取失败为读写失败
// （退出码 1）；空清单、格式错误、缺项、非正整数段序号或非法日期为清单
// 格式错误（*usageError，退出码 2），指出涉及的项号。清单文件只读。
func readMaintManifest(path string) ([]maintManifestItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取保养清单失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &usageError{msg: fmt.Sprintf(
			"保养清单 %s 为空：清单须为至少含一项登记的 JSON 数组", path)}
	}
	// 逐项解码，使字段级类型错误（如 segment_seq 不是整数、含未知字段）也能
	// 指出涉及的项号，而不是只报 JSON 偏移。
	dec := json.NewDecoder(bytes.NewReader(raw))
	open, err := dec.Token()
	if err != nil || open != json.Delim('[') {
		return nil, &usageError{msg: "保养清单格式错误：清单须为 JSON 数组，每项含 asset_id、due、done、result、segment_seq"}
	}
	items := make([]maintManifestItem, 0)
	pos := 0
	for dec.More() {
		pos++
		var rawItem json.RawMessage
		if err := dec.Decode(&rawItem); err != nil {
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项格式错误: %s", pos, err)}
		}
		if len(bytes.TrimSpace(rawItem)) == 0 || string(bytes.TrimSpace(rawItem)) == "null" {
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项不能为空", pos)}
		}
		var it maintManifestItem
		itemDec := json.NewDecoder(bytes.NewReader(rawItem))
		itemDec.DisallowUnknownFields()
		if err := itemDec.Decode(&it); err != nil {
			return nil, &usageError{msg: fmt.Sprintf(
				"保养清单第 %d 项格式错误: %s（每项须含非空 asset_id、result，YYYY-MM-DD 形式的 due、done 及正整数 segment_seq）",
				pos, err)}
		}
		switch {
		case it.AssetID == "":
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项：资产编号（asset_id）不能为空", pos)}
		case it.Result == "":
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项：保养结果（result）不能为空", pos)}
		}
		if _, err := parseDate(it.Due); err != nil {
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项：周期到期日（due）无效：%s", pos, err)}
		}
		if _, err := parseDate(it.Done); err != nil {
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项：实际完成日（done）无效：%s", pos, err)}
		}
		if it.Segment < 1 {
			return nil, &usageError{msg: fmt.Sprintf("保养清单第 %d 项：方案段序号（segment_seq）须为正整数", pos)}
		}
		items = append(items, it)
	}
	if _, err := dec.Token(); err != nil { // 数组结束括号 ']'
		return nil, &usageError{msg: fmt.Sprintf("保养清单格式错误: %s", err)}
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, &usageError{msg: "保养清单格式错误：JSON 数组之后存在多余内容"}
		}
		return nil, &usageError{msg: fmt.Sprintf("保养清单格式错误: %s", err)}
	}
	if len(items) == 0 {
		return nil, &usageError{msg: "保养清单为空：至少须包含一项保养完成登记"}
	}
	return items, nil
}

// currentSegmentSeq 返回资产当前方案段的全库序号：该资产序号最大的建立或
// 调整履历。方案段边界只按全库序号划分，不按日期；同日期的旧段序号更小。
func (s *store) currentSegmentSeq(assetID string) int {
	seg := 0
	for _, e := range s.data.Events {
		if e.AssetID != assetID {
			continue
		}
		if (e.Kind == eventPlanCreate || e.Kind == eventPlanAdjust) && e.Seq > seg {
			seg = e.Seq
		}
	}
	return seg
}

// maintBatchOutcome 为批量保养登记中一项的处理结果，用于保存后输出。
type maintBatchOutcome struct {
	pos     int
	assetID string
	due     string
	seq     int
	next    string
}

// batchCompletePlans 按清单顺序登记全部保养完成项，返回每项（按清单项号）
// 的完成序号与推进后的下一到期日。任一业务判定或容量失败都整批失败：内存
// 中的部分推进随台账对象丢弃，不保存、不消耗序号。接续、完成日不早于到期
// 日、越界与停用判定全部与单项 maintain 共用保养规则核心。
func (s *store) batchCompletePlans(items []maintManifestItem) ([]maintBatchOutcome, error) {
	core, err := s.maintCore()
	if err != nil {
		return nil, err
	}
	outcomes := make([]maintBatchOutcome, 0, len(items))
	for i, it := range items {
		pos := i + 1
		fail := func(msg string) error {
			return fmt.Errorf("保养清单第 %d 项（资产 %s）: %s", pos, it.AssetID, msg)
		}
		if s.findAsset(it.AssetID) == nil {
			return nil, fail(fmt.Sprintf("未知资产编号 %q", it.AssetID))
		}
		p := s.findPlan(it.AssetID)
		if p == nil {
			return nil, fail(fmt.Sprintf("资产 %s 没有保养计划", it.AssetID))
		}
		// 方案段只认当前段：即使旧段的首次到期日等日期与当前段相同也拒绝。
		if seg := s.currentSegmentSeq(it.AssetID); seg != it.Segment {
			return nil, fail(fmt.Sprintf(
				"方案段序号 %d 不是资产 %s 的当前方案段（当前段序号为 %d）；只接受当前段，同日期的旧段也拒绝",
				it.Segment, it.AssetID, seg))
		}
		if core.deactivatedNow(it.AssetID) {
			return nil, fail(fmt.Sprintf("资产 %s 已停用，不能登记保养完成", it.AssetID))
		}
		// 候选履历不含序号（核心判定不依赖序号）；接续、完成日不早于到期日与
		// 日期范围判定全部由共享核心完成，后项自然接续前项在同一核心上推进的
		// 下一到期日，重复周期在此整批失败。
		cand := Event{AssetID: it.AssetID, Kind: eventPlanDone, Content: it.Result, Due: it.Due, Done: it.Done}
		if err := core.apply(cand); err != nil {
			return nil, fail(friendlyErr(maintOpError(err).Error()))
		}
		eventSeq, err := s.nextEventSeq()
		if err != nil {
			return nil, fail(friendlyErr(err.Error()))
		}
		_, _, _, next, _ := core.derived(it.AssetID)
		p.NextDue = next
		s.appendMaintEvent(eventSeq, it.AssetID, eventPlanDone, it.Result, it.Due, it.Done, 0)
		outcomes = append(outcomes, maintBatchOutcome{
			pos: pos, assetID: it.AssetID, due: it.Due, seq: eventSeq, next: next,
		})
	}
	return outcomes, nil
}

// cmdBatchMaintain 批量保养完成登记命令入口。清单格式错误为退出码 2；
// 业务冲突、容量不足与读写失败为退出码 1。整批一次原子保存后才输出成功结果。
func cmdBatchMaintain(args []string, w io.Writer) error {
	var opts cmdOptions
	var file string
	fs := newFlagSet("batch-maintain", &opts)
	fs.StringVar(&file, "file", "", "保养完成清单文件路径（必填；JSON 数组，每项含非空 asset_id、result，YYYY-MM-DD 形式的 due、done 与正整数 segment_seq；清单只读）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, file, "file"); err != nil {
		return err
	}
	items, err := readMaintManifest(file)
	if err != nil {
		return err
	}
	// openStore 已先做整库一致性检查：损坏或关联矛盾按原规则拒绝（退出码 1）。
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	outcomes, err := s.batchCompletePlans(items)
	if err != nil {
		return err
	}
	// 整批计划推进与履历一次原子保存（保存前再次校验整库一致性）；失败时
	// 原文件字节不变，不推进计划、不留履历、不消耗序号，恢复后可用原清单重试。
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "批量保养完成登记完成：共 %d 项。\n", len(outcomes))
	fmt.Fprintln(w, "项号\t资产编号\t周期到期日\t完成序号\t下一到期日")
	for _, o := range outcomes {
		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\n", o.pos, o.assetID, o.due, o.seq, o.next)
	}
	return nil
}
