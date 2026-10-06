package main

import (
	"fmt"
	"math"
	"os"
	"sort"
)

// 维修工单本地资料附件索引。
//
// 只保存资料文件的引用（登记时解析得到的绝对路径），不复制、不修改、不删除
// 资料文件本身。登记输入工单编号、本地文件路径与非空说明；未关闭、已关闭或
// 已取消工单均可补充资料。登记时文件须为存在且可读的普通文件；文件日后消失、
// 成为目录或不可读仅使引用不可用，不阻止撤销及其他业务，也不参与整库一致性
// 校验。每次登记独立生成同目录唯一且不复用的附件编号（A 加补零序号），同路径
// 不合并，也不使用报修请求标识去重。
//
// 撤销输入附件编号与非空理由，保留记录、路径、说明与理由；未知编号、重复撤销
// 拒绝，不影响同路径其他索引或后来工单。工单终结不自动撤销附件；已撤销编号
// 不能恢复，可重新登记为新索引。登记、撤销各为一次原子保存：校验、编号或履历
// 容量不足、读写失败时保留原文件字节，不留下部分记录、不消耗编号，恢复后可重试。

// findAttachment 按附件编号查找附件记录，不存在时返回 nil。
func (s *store) findAttachment(id string) *Attachment {
	for _, a := range s.data.Attachments {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// attachRegSeqs 返回每条附件编号对应的登记履历全库序号。附件编号只用于定位
// 记录：各条索引的先后由该条唯一登记履历的序号决定，与记录数组位置、编号
// 大小及履历时间无关（有效台账允许数组乱序、序号间隔、时间不递增）。
func (s *store) attachRegSeqs() map[string]int {
	seqs := make(map[string]int, len(s.data.Attachments))
	for _, e := range s.data.Events {
		if e.Kind == eventAttach {
			seqs[e.AttachmentID] = e.Seq
		}
	}
	return seqs
}

// attachmentsOf 按登记先后返回工单的全部附件记录（含已撤销）：顺序由每条
// 唯一登记履历的全库序号决定，不使用数组位置或编号大小。
func (s *store) attachmentsOf(ticketID string) []*Attachment {
	seqs := s.attachRegSeqs()
	out := make([]*Attachment, 0)
	for _, a := range s.data.Attachments {
		if a.TicketID == ticketID {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return seqs[out[i].ID] < seqs[out[j].ID] })
	return out
}

// checkAttachFile 校验登记时的资料文件：须为存在且可读的普通文件。
func checkAttachFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("附件文件 %s 不存在或不可访问: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("附件路径 %s 不是普通文件", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("附件文件 %s 不可读: %v", path, err)
	}
	return f.Close()
}

// attachmentReadable 报告资料文件当前是否仍可作为普通文件读取。文件日后
// 消失、成为目录或不可读仅使引用不可用，不影响台账一致性与任何业务操作。
func attachmentReadable(path string) bool {
	return checkAttachFile(path) == nil
}

// appendAttachEvent 以给定序号追加附件履历（登记或撤销）。附件履历属于附件
// 记录所在的工单，from/to、保养与备件字段恒为空；content 为附件说明或撤销
// 理由，Path 为记录保存的绝对路径。
func (s *store) appendAttachEvent(seq int, a *Attachment, kind, content string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:          seq,
		AssetID:      a.AssetID,
		TicketID:     a.TicketID,
		Kind:         kind,
		Content:      content,
		AttachmentID: a.ID,
		Path:         a.Path,
		Time:         s.now(),
	})
}

// attach 为工单登记本地资料附件索引：未关闭、已关闭或已取消工单均可补充。
// 生成同目录唯一且不复用的附件编号，追加一条登记履历。path 须为调用方已
// 解析得到的绝对路径，且登记时须为存在且可读的普通文件。每次登记独立：
// 同路径不合并，不使用报修请求标识去重。失败路径不修改任何业务数据，
// 也不消耗附件编号。
func (s *store) attach(ticketID, path, note string) (*Attachment, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if note == "" {
		return nil, fmt.Errorf("%w: 附件说明不能为空", errConflict)
	}
	if err := checkAttachFile(path); err != nil {
		return nil, err
	}
	// 先确认附件编号与履历计数器都能推进，再修改任何业务数据：
	// 编号耗尽时拒绝登记，不消耗编号，也不产生履历。
	seq := s.data.NextAttachSeq
	if seq < 1 || seq == math.MaxInt {
		return nil, fmt.Errorf("%w: 附件编号计数器已耗尽，无法分配新附件编号", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	a := &Attachment{
		ID:       fmt.Sprintf("A%04d", seq),
		TicketID: t.ID,
		AssetID:  t.AssetID,
		Path:     path,
		Note:     note,
	}
	s.data.NextAttachSeq = seq + 1
	s.data.Attachments = append(s.data.Attachments, a)
	s.appendAttachEvent(eventSeq, a, eventAttach, note)
	return a, nil
}

// revokeAttachment 按附件编号撤销附件索引：保留记录、路径、说明与撤销理由，
// 追加一条撤销履历。已撤销编号不能恢复（可重新登记为新索引）；工单终结不
// 自动撤销附件，撤销也不受工单状态限制。失败路径不修改任何业务数据。
func (s *store) revokeAttachment(id, reason string) (*Attachment, error) {
	a := s.findAttachment(id)
	if a == nil {
		return nil, fmt.Errorf("%w: 未知附件编号 %q", errNotFound, id)
	}
	if a.Revoked {
		return nil, fmt.Errorf("%w: 附件 %s 已撤销，不能重复撤销", errConflict, id)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: 撤销理由不能为空", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	a.Revoked = true
	a.RevokeReason = reason
	s.appendAttachEvent(eventSeq, a, eventAttachRevoke, reason)
	return a, nil
}
