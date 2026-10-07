package main

import (
	"fmt"
)

// 设备停用与恢复使用。
//
// 停用：仅“可用”（无未关闭工单、未停用）的资产可以停用，输入资产编号与非空
// 理由；维修中（存在未关闭工单）的资产不能停用。成功后资产状态变为“停用”，
// 追加一条资产级履历，记录操作时间、原状态、新状态与理由。停用不创建工单、
// 不消耗工单编号，不删除工单、负责人、备件或附件记录，也不改变保养内容、
// 间隔或下一到期日，不暂停或重算保养周期。
//
// 恢复使用：仅停用资产可恢复，输入资产编号与非空理由。成功后资产状态恢复
// “可用”，同样追加一条含原状态、新状态、理由与操作时间的资产级履历。历史
// 记录不因恢复而删除；恢复后按保存的下一到期日参与到期查询。
//
// 停用期间拒绝新报修与保养完成登记，但已有报修请求的相同重放仍返回原工单及
// 当前状态，不开单、不改资产状态；停用资产仍可建立保养计划、撤销已登记完成，
// 终态工单仍可补充或撤销附件。停用、恢复各为一次原子保存：校验、履历容量或
// 读写失败时保留原文件字节，不留下部分状态或履历，不消耗序号，恢复后可重试。

// appendStatusEvent 以给定序号追加停用/恢复使用履历。该履历为资产级履历，
// 不属于任何工单；from/to 为原资产状态与新资产状态，content 为理由。
func (s *store) appendStatusEvent(seq int, assetID, kind, from, to, reason string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:     seq,
		AssetID: assetID,
		Kind:    kind,
		Content: reason,
		From:    from,
		To:      to,
		Time:    s.now(),
	})
}

// deactivateAsset 停用资产：仅“可用”且无未关闭工单的资产可停用。
// 未知资产、空理由、重复停用、维修中停用均拒绝。失败路径不修改任何业务数据。
func (s *store) deactivateAsset(assetID, reason string) (*Asset, error) {
	a := s.findAsset(assetID)
	if a == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: 停用理由不能为空", errConflict)
	}
	if a.Status == statusDeactivated {
		return nil, fmt.Errorf("%w: 资产 %s 已停用，不能重复停用", errConflict, assetID)
	}
	if t := s.openTicketOf(assetID); t != nil {
		return nil, fmt.Errorf("%w: 资产 %s 有未关闭工单 %s，维修中不能停用", errConflict, assetID, t.ID)
	}
	if a.Status != statusAvailable {
		return nil, fmt.Errorf("%w: 资产 %s 当前状态为 %s，不能停用", errConflict, assetID, a.Status)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	from := a.Status
	a.Status = statusDeactivated
	s.appendStatusEvent(eventSeq, assetID, eventDeactivate, from, statusDeactivated, reason)
	return a, nil
}

// reactivateAsset 恢复使用停用资产：仅停用资产可恢复，理由非空。
// 未知资产、空理由、对可用或维修中资产恢复均拒绝。失败路径不修改任何业务数据。
func (s *store) reactivateAsset(assetID, reason string) (*Asset, error) {
	a := s.findAsset(assetID)
	if a == nil {
		return nil, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: 恢复理由不能为空", errConflict)
	}
	if a.Status == statusAvailable {
		return nil, fmt.Errorf("%w: 资产 %s 未停用，不能恢复使用", errConflict, assetID)
	}
	if t := s.openTicketOf(assetID); t != nil {
		return nil, fmt.Errorf("%w: 资产 %s 有未关闭工单 %s，不能恢复使用", errConflict, assetID, t.ID)
	}
	if a.Status != statusDeactivated {
		return nil, fmt.Errorf("%w: 资产 %s 当前状态为 %s，不能恢复使用", errConflict, assetID, a.Status)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	from := a.Status
	a.Status = statusAvailable
	s.appendStatusEvent(eventSeq, assetID, eventReactivate, from, statusAvailable, reason)
	return a, nil
}
