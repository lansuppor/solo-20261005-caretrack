package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// 维修工单附件的本地索引。
//
// 索引只保存资料文件的引用（绝对路径），不复制、修改或删除文件本身。
// 登记：输入工单编号、本地文件路径与非空说明；未关闭、已关闭或已取消工单均可
// 补充资料。相对路径按调用时工作目录解析并保存绝对路径；登记时须为存在且可读
// 的普通文件。每次登记独立生成同目录唯一且不复用的附件编号（A 加补零序号），
// 同路径不合并，也不使用报修请求标识去重。
//
// 撤销：输入附件编号与非空理由；成功后记录保留（路径、说明、理由都在），状态
// 变为已撤销。未知编号、重复撤销拒绝；不影响同路径的其他索引或后来的工单。
// 工单关闭或取消不自动撤销附件；已撤销编号不能恢复，可重新登记为新索引。
//
// 文件日后消失、成为目录或不可读，仅使该引用在查询时显示“不可用”，不阻止
// 撤销及其他任何业务，也不参与整库一致性校验。登记、撤销各为一次原子保存：
// 校验、编号或履历容量不足、读写失败时保留原文件字节，不留下部分记录、
// 不消耗编号，恢复后可重试。

// findAttachment 按附件编号查找附件记录，不存在时返回 nil。
func (s *store) findAttachment(id string) *Attachment {
	for _, a := range s.data.Attachments {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// attachSeqs 返回每条附件编号对应的登记履历全库序号。附件编号只用于定位
// 记录：各条索引的先后由该条唯一登记履历的序号决定，与记录数组位置、编号
// 大小及履历时间无关（有效台账允许数组乱序、序号间隔、时间不递增）。
func (s *store) attachSeqs() map[string]int {
	seqs := make(map[string]int, len(s.data.Attachments))
	for _, e := range s.data.Events {
		if e.Kind == eventAttach {
			seqs[e.AttachmentID] = e.Seq
		}
	}
	return seqs
}

// attachmentsOf 按登记先后返回工单的全部附件索引（含已撤销）：顺序由每条
// 唯一登记履历的全库序号决定，不使用数组位置或编号大小。
func (s *store) attachmentsOf(ticketID string) []*Attachment {
	seqs := s.attachSeqs()
	out := make([]*Attachment, 0)
	for _, a := range s.data.Attachments {
		if a.TicketID == ticketID {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return seqs[out[i].ID] < seqs[out[j].ID] })
	return out
}

// regularFileReadable 报告路径当前是否指向一个存在且可读的普通文件。
// 目录、不存在或不可读的路径都返回 false。只读检查，不修改任何文件。
func regularFileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// appendAttachEvent 以给定序号追加附件履历（登记或撤销）。附件履历属于附件
// 记录所在的工单，from/to、保养与备件字段恒为空；content 为登记说明或撤销理由。
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

// attachFile 为工单登记一条附件索引：相对路径按调用时工作目录解析并保存
// 绝对路径，登记时文件须为存在且可读的普通文件。未关闭、已关闭或已取消工单
// 均可登记。每次登记独立生成同目录唯一且不复用的附件编号，同路径不合并，
// 不使用报修请求标识去重。失败路径不修改任何业务数据，也不消耗附件编号。
func (s *store) attachFile(ticketID, path, note string) (*Attachment, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if note == "" {
		return nil, fmt.Errorf("%w: 附件说明不能为空", errConflict)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("无法解析文件路径 %q: %w", path, err)
	}
	if !regularFileReadable(abs) {
		return nil, fmt.Errorf("%w: 文件 %s 不存在、不是普通文件或不可读，不能登记为附件", errConflict, abs)
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
		Path:     abs,
		Note:     note,
	}
	s.data.NextAttachSeq = seq + 1
	s.data.Attachments = append(s.data.Attachments, a)
	s.appendAttachEvent(eventSeq, a, eventAttach, note)
	return a, nil
}

// revokeAttachment 按附件编号撤销一条索引：记录、路径、说明与理由全部保留，
// 状态变为已撤销。撤销与工单状态、文件当前可用性无关；不影响同路径的其他
// 索引。已撤销编号不能恢复，也不能再次撤销。失败路径不修改任何业务数据。
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
	s.appendAttachEvent(eventSeq, a, eventRevoke, reason)
	return a, nil
}
