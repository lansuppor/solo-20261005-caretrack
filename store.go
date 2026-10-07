package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 资产状态、工单状态与履历类型。
const (
	statusAvailable   = "可用"
	statusRepairing   = "维修中"
	statusDeactivated = "停用"
	ticketOpen        = "未关闭"
	ticketClosed      = "已关闭"
	ticketCancelled   = "已取消"

	eventReport = "报修"
	eventClose  = "关闭"
	eventCancel = "取消"
	eventAssign = "派工"

	eventDeactivate = "停用"
	eventReactivate = "恢复使用"

	eventPlanCreate = "保养建立"
	eventPlanDone   = "保养完成"
	eventPlanRevoke = "保养撤销"
	eventPlanAdjust = "保养调整"

	eventPartWithdraw = "领用"
	eventPartReturn   = "退回"

	eventAttach       = "附件登记"
	eventAttachRevoke = "附件撤销"

	storeVersion = 1
	dataFileName = "caretrack.json"
)

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
)

// Asset 为登记的设备资产，ID 即企业资产编号。
type Asset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Status   string `json:"status"`
}

// Ticket 为维修工单。Assignee 为当前（或终结前最后）负责人，空表示未派工；
// AssignedAt/AssignNote 为最近一次派工的变更时间与说明。
type Ticket struct {
	ID           string `json:"id"`
	AssetID      string `json:"asset_id"`
	Description  string `json:"description"`
	RequestID    string `json:"request_id"`
	Status       string `json:"status"`
	Result       string `json:"result,omitempty"`
	CreatedAt    string `json:"created_at"`
	ClosedAt     string `json:"closed_at,omitempty"`
	CancelReason string `json:"cancel_reason,omitempty"`
	CancelledAt  string `json:"cancelled_at,omitempty"`
	Assignee     string `json:"assignee,omitempty"`
	AssignedAt   string `json:"assigned_at,omitempty"`
	AssignNote   string `json:"assign_note,omitempty"`
}

// Event 为履历条目（报修/派工/关闭/取消/停用/恢复使用/保养建立/保养完成/保养撤销/保养调整/领用/退回/附件登记/附件撤销），Seq 决定操作发生顺序。
// From/To 在派工履历中为原负责人（首次派工为空，展示为“未派工”）与新负责人；
// 在停用、恢复使用履历中为原资产状态与新资产状态。
// Due/Done/Interval 仅保养履历使用：建立履历含首次到期日（Due）与间隔天数
// （Interval），完成履历含周期到期日（Due）与实际完成日（Done）。
// TargetSeq 仅保养撤销履历使用：被撤销的完成履历的全库序号；完成身份以此序号
// 标识，不能用到期日或报修请求标识代替。
// 保养调整履历中 Content 为调整理由，Due/Interval 为新方案的首次到期日与间隔
// 天数，NewContent 为新保养内容，OldContent/OldDue/OldInterval 为原方案的内容、
// 首次到期日与间隔天数，OldNextDue 为调整前的下一到期日；调整以该履历的全库
// 序号划分方案段，不按日期或数组位置。
// PartID/Quantity/WithdrawalID 仅备件履历使用：领用与退回履历都携带备件编号、
// 数量与所属领用编号；Content 分别为领用说明与退回理由。
// AttachmentID/Path 仅附件履历使用：登记与撤销履历都携带附件编号与保存的绝对
// 路径；Content 分别为附件说明与撤销理由。
type Event struct {
	Seq          int       `json:"-"`
	AssetID      string    `json:"-"`
	TicketID     string    `json:"-"`
	Kind         string    `json:"-"`
	Content      string    `json:"-"`
	From         string    `json:"-"`
	To           string    `json:"-"`
	Due          string    `json:"-"`
	Done         string    `json:"-"`
	Interval     int       `json:"-"`
	TargetSeq    int       `json:"-"`
	NewContent   string    `json:"-"`
	OldContent   string    `json:"-"`
	OldDue       string    `json:"-"`
	OldInterval  int       `json:"-"`
	OldNextDue   string    `json:"-"`
	PartID       string    `json:"-"`
	Quantity     int       `json:"-"`
	WithdrawalID string    `json:"-"`
	AttachmentID string    `json:"-"`
	Path         string    `json:"-"`
	Time         time.Time `json:"-"`
}

// eventJSON 与 Event 对应，时间以 RFC3339 文本持久化。
type eventJSON struct {
	Seq          int    `json:"seq"`
	AssetID      string `json:"asset_id"`
	TicketID     string `json:"ticket_id,omitempty"`
	Kind         string `json:"kind"`
	Content      string `json:"content"`
	From         string `json:"from,omitempty"`
	To           string `json:"to,omitempty"`
	Due          string `json:"due,omitempty"`
	Done         string `json:"done,omitempty"`
	Interval     int    `json:"interval,omitempty"`
	TargetSeq    int    `json:"target_seq,omitempty"`
	NewContent   string `json:"new_content,omitempty"`
	OldContent   string `json:"old_content,omitempty"`
	OldDue       string `json:"old_due,omitempty"`
	OldInterval  int    `json:"old_interval,omitempty"`
	OldNextDue   string `json:"old_next_due,omitempty"`
	PartID       string `json:"part_id,omitempty"`
	Quantity     int    `json:"quantity,omitempty"`
	WithdrawalID string `json:"withdrawal_id,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
	Path         string `json:"path,omitempty"`
	Time         string `json:"time"`
}

func (e Event) toJSON() eventJSON {
	return eventJSON{
		Seq: e.Seq, AssetID: e.AssetID, TicketID: e.TicketID,
		Kind: e.Kind, Content: e.Content, From: e.From, To: e.To,
		Due: e.Due, Done: e.Done, Interval: e.Interval, TargetSeq: e.TargetSeq,
		NewContent: e.NewContent, OldContent: e.OldContent,
		OldDue: e.OldDue, OldInterval: e.OldInterval, OldNextDue: e.OldNextDue,
		PartID: e.PartID, Quantity: e.Quantity, WithdrawalID: e.WithdrawalID,
		AttachmentID: e.AttachmentID, Path: e.Path,
		// RFC3339Nano 保留小数秒精度；整秒时输出与 RFC3339 完全一致，
		// 因此既有整秒台账的字节表示不变，而旧库中带小数秒的履历时间
		// 在重新保存（含导入合并后的提交）时也不会被截断。
		Time: e.Time.Format(time.RFC3339Nano),
	}
}

func (e eventJSON) toEvent() (Event, error) {
	t, err := time.Parse(time.RFC3339, e.Time)
	if err != nil {
		return Event{}, fmt.Errorf("履历时间格式无效 %q: %w", e.Time, err)
	}
	return Event{
		Seq: e.Seq, AssetID: e.AssetID, TicketID: e.TicketID,
		Kind: e.Kind, Content: e.Content, From: e.From, To: e.To,
		Due: e.Due, Done: e.Done, Interval: e.Interval, TargetSeq: e.TargetSeq,
		NewContent: e.NewContent, OldContent: e.OldContent,
		OldDue: e.OldDue, OldInterval: e.OldInterval, OldNextDue: e.OldNextDue,
		PartID: e.PartID, Quantity: e.Quantity, WithdrawalID: e.WithdrawalID,
		AttachmentID: e.AttachmentID, Path: e.Path,
		Time: t,
	}, nil
}

// PartWithdrawal 为一笔备件领用记录，ID 即领用编号（同目录唯一、不复用）。
// 数量表示工单用量；Returned 为累计退回数量，净量 = Quantity - Returned。
// 工单关闭或取消后记录与净量保留，不清零。
type PartWithdrawal struct {
	ID       string `json:"id"`
	TicketID string `json:"ticket_id"`
	AssetID  string `json:"asset_id"`
	PartID   string `json:"part_id"`
	Quantity int    `json:"quantity"`
	Note     string `json:"note"`
	Returned int    `json:"returned"`
}

// requestBinding 记录请求标识与具体报修的绑定，用于去重与冲突拒绝；
// 它既不是资产编号也不是工单编号。
type requestBinding struct {
	RequestID   string `json:"request_id"`
	AssetID     string `json:"asset_id"`
	Description string `json:"description"`
	TicketID    string `json:"ticket_id"`
}

// Attachment 为工单本地资料附件的索引记录，ID 即附件编号（同目录唯一、不复用）。
// 只保存引用：Path 为登记时解析得到的绝对路径，工具不复制、不修改、不删除
// 资料文件本身。撤销后保留记录、路径、说明（Note）与撤销理由（RevokeReason），
// 已撤销编号不能恢复；工单终结不自动撤销附件。
type Attachment struct {
	ID           string `json:"id"`
	TicketID     string `json:"ticket_id"`
	AssetID      string `json:"asset_id"`
	Path         string `json:"path"`
	Note         string `json:"note"`
	Revoked      bool   `json:"revoked"`
	RevokeReason string `json:"revoke_reason,omitempty"`
}

// storeData 是一次成功写操作共同生效的完整业务数据。
type storeData struct {
	Version       int               `json:"version"`
	Assets        []*Asset          `json:"assets"`
	Tickets       []*Ticket         `json:"tickets"`
	Events        []Event           `json:"-"`
	EventsJSON    []eventJSON       `json:"events"`
	Requests      []requestBinding  `json:"requests"`
	Plans         []*Plan           `json:"plans"`
	Parts         []*PartWithdrawal `json:"parts"`
	Attachments   []*Attachment     `json:"attachments"`
	NextTicketSeq int               `json:"next_ticket_seq"`
	NextPartSeq   int               `json:"next_part_seq"`
	NextAttachSeq int               `json:"next_attach_seq"`
}

type store struct {
	dir  string
	data *storeData
	now  func() time.Time
}

func newStoreData() *storeData {
	return &storeData{
		Version:       storeVersion,
		Assets:        []*Asset{},
		Tickets:       []*Ticket{},
		Events:        []Event{},
		EventsJSON:    []eventJSON{},
		Requests:      []requestBinding{},
		Plans:         []*Plan{},
		Parts:         []*PartWithdrawal{},
		Attachments:   []*Attachment{},
		NextTicketSeq: 1,
		NextPartSeq:   1,
		NextAttachSeq: 1,
	}
}

// openStore 打开数据目录：目录/文件不存在时视为空库（首次保存时初始化）；
// 已有文件为空或无法解析时报告错误并保留原文件，绝不当空库覆盖。
func openStore(dir string) (*store, error) {
	return loadStore(dir, true)
}

// openSourceStore 以只读用途打开导入源数据目录：与 openStore 做同样的整库
// 一致性检查，但源台账必须已存在——不存在时报错，绝不能当空库初始化。
func openSourceStore(dir string) (*store, error) {
	return loadStore(dir, false)
}

func loadStore(dir string, allowMissing bool) (*store, error) {
	path := filepath.Join(dir, dataFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if allowMissing {
				return &store{dir: dir, data: newStoreData(), now: time.Now}, nil
			}
			return nil, fmt.Errorf("源数据文件 %s 不存在：源台账必须已存在，不能当空库处理", path)
		}
		return nil, fmt.Errorf("读取数据文件失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("数据文件 %s 已损坏：文件为空（原文件已保留，未做任何修改）", path)
	}
	var d storeData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("数据文件 %s 已损坏：%w（原文件已保留，未做任何修改）", path, err)
	}
	if d.EventsJSON != nil {
		d.Events = make([]Event, len(d.EventsJSON))
		for i, ej := range d.EventsJSON {
			ev, err := ej.toEvent()
			if err != nil {
				return nil, fmt.Errorf("数据文件 %s 已损坏：%w（原文件已保留，未做任何修改）", path, err)
			}
			d.Events[i] = ev
		}
	}
	// 无保养字段的有效旧库直接使用：缺省视为没有任何保养计划。
	if d.Plans == nil {
		d.Plans = []*Plan{}
	}
	// 无备件字段的有效旧库直接使用：缺省视为没有任何备件领用记录，
	// 领用编号计数器从 1 开始。
	if d.Parts == nil {
		d.Parts = []*PartWithdrawal{}
	}
	if d.NextPartSeq == 0 && len(d.Parts) == 0 {
		d.NextPartSeq = 1
	}
	// 无附件字段的有效旧库直接使用：缺省视为没有任何附件索引记录，
	// 附件编号计数器从 1 开始。
	if d.Attachments == nil {
		d.Attachments = []*Attachment{}
	}
	if d.NextAttachSeq == 0 && len(d.Attachments) == 0 {
		d.NextAttachSeq = 1
	}
	if err := validateData(&d); err != nil {
		return nil, fmt.Errorf("数据文件 %s 内容相互矛盾：%w（原文件已保留，未做任何修改）", path, err)
	}
	return &store{dir: dir, data: &d, now: time.Now}, nil
}

// parseTicketSeq 解析 T 加补零正整数序号形式的工单编号，返回序号。
// 编号必须与 fmt.Sprintf("T%04d", n)（n ≥ 1）完全一致，否则视为非法。
func parseTicketSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "T") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	if fmt.Sprintf("T%04d", n) != id {
		return 0, false
	}
	return n, true
}

// parsePartSeq 解析 P 加补零正整数序号形式的领用编号，返回序号。
// 编号必须与 fmt.Sprintf("P%04d", n)（n ≥ 1）完全一致，否则视为非法。
func parsePartSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "P") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	if fmt.Sprintf("P%04d", n) != id {
		return 0, false
	}
	return n, true
}

// parseAttachSeq 解析 A 加补零正整数序号形式的附件编号，返回序号。
// 编号必须与 fmt.Sprintf("A%04d", n)（n ≥ 1）完全一致，否则视为非法。
func parseAttachSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "A") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	if fmt.Sprintf("A%04d", n) != id {
		return 0, false
	}
	return n, true
}

// validateData 校验整份业务数据的内部一致性。任何矛盾都会返回指明问题类别
// （计数器/请求绑定/状态/履历）的错误；调用方必须拒绝查询与写入并保留原文件。
// 不做任何修复：不补字段、不重编号、不删除记录。
func validateData(d *storeData) error {
	if d.Version != storeVersion {
		return fmt.Errorf("不支持的数据版本 %d", d.Version)
	}
	if d.Assets == nil || d.Tickets == nil || d.Requests == nil || d.EventsJSON == nil || d.Events == nil || d.Plans == nil || d.Parts == nil || d.Attachments == nil {
		return errors.New("缺少必要的数据段")
	}
	assets := map[string]*Asset{}
	for _, a := range d.Assets {
		if a == nil || a.ID == "" || a.Name == "" || a.Location == "" {
			return errors.New("数据矛盾：存在字段不完整的资产记录")
		}
		if a.Status != statusAvailable && a.Status != statusRepairing && a.Status != statusDeactivated {
			return fmt.Errorf("状态矛盾：资产 %s 状态无效 %q", a.ID, a.Status)
		}
		if assets[a.ID] != nil {
			return fmt.Errorf("数据矛盾：资产编号 %s 重复", a.ID)
		}
		assets[a.ID] = a
	}
	tickets := map[string]*Ticket{}
	maxTicketSeq := 0
	for _, t := range d.Tickets {
		if t == nil || t.ID == "" || t.AssetID == "" || t.RequestID == "" || t.Description == "" || t.CreatedAt == "" {
			return errors.New("数据矛盾：存在字段不完整的工单记录（工单描述不能为空）")
		}
		n, ok := parseTicketSeq(t.ID)
		if !ok {
			return fmt.Errorf("计数器矛盾：工单编号 %q 不是 T 加补零正整数序号的形式", t.ID)
		}
		if n > maxTicketSeq {
			maxTicketSeq = n
		}
		if t.Status != ticketOpen && t.Status != ticketClosed && t.Status != ticketCancelled {
			return fmt.Errorf("状态矛盾：工单 %s 状态无效 %q", t.ID, t.Status)
		}
		if assets[t.AssetID] == nil {
			return fmt.Errorf("数据矛盾：工单 %s 引用了不存在的资产 %s", t.ID, t.AssetID)
		}
		if t.Status == ticketClosed && (t.Result == "" || t.ClosedAt == "") {
			return fmt.Errorf("状态矛盾：已关闭工单 %s 缺少维修结果或关闭时间", t.ID)
		}
		if t.Status == ticketCancelled && (t.CancelReason == "" || t.CancelledAt == "") {
			return fmt.Errorf("状态矛盾：已取消工单 %s 缺少取消理由或取消时间", t.ID)
		}
		if t.Status != ticketClosed && (t.Result != "" || t.ClosedAt != "") {
			return fmt.Errorf("状态矛盾：非已关闭工单 %s 不应带有维修结果或关闭时间", t.ID)
		}
		if t.Status != ticketCancelled && (t.CancelReason != "" || t.CancelledAt != "") {
			return fmt.Errorf("状态矛盾：非已取消工单 %s 不应带有取消理由或取消时间", t.ID)
		}
		if t.Assignee == "" {
			if t.AssignedAt != "" || t.AssignNote != "" {
				return fmt.Errorf("状态矛盾：未派工工单 %s 不应带有派工时间或派工说明", t.ID)
			}
		} else if t.AssignedAt == "" || t.AssignNote == "" {
			return fmt.Errorf("状态矛盾：已派工工单 %s 缺少派工时间或派工说明", t.ID)
		}
		if tickets[t.ID] != nil {
			return fmt.Errorf("数据矛盾：工单编号 %s 重复", t.ID)
		}
		tickets[t.ID] = t
	}
	// 下一工单序号必须为正并大于全部已用序号（允许有间隔）。
	if d.NextTicketSeq < 1 || d.NextTicketSeq <= maxTicketSeq {
		return fmt.Errorf("计数器矛盾：下一工单序号 %d 必须为正并大于全部已用序号（当前最大 %d）",
			d.NextTicketSeq, maxTicketSeq)
	}
	// 备件领用记录：编号唯一且为 P 加补零正整数序号，归属存在的工单且资产归属
	// 与工单一致，数量为正，累计退回在 [0, 原数量] 内；与履历的接续在下方按
	// 履历序号重放时核对。
	parts := map[string]*PartWithdrawal{}
	maxPartSeq := 0
	for _, p := range d.Parts {
		if p == nil || p.ID == "" || p.TicketID == "" || p.AssetID == "" || p.PartID == "" || p.Note == "" {
			return errors.New("数据矛盾：存在字段不完整的备件领用记录")
		}
		n, ok := parsePartSeq(p.ID)
		if !ok {
			return fmt.Errorf("计数器矛盾：领用编号 %q 不是 P 加补零正整数序号的形式", p.ID)
		}
		if n > maxPartSeq {
			maxPartSeq = n
		}
		if p.Quantity < 1 {
			return fmt.Errorf("数据矛盾：领用记录 %s 的数量 %d 不是正整数", p.ID, p.Quantity)
		}
		if p.Returned < 0 || p.Returned > p.Quantity {
			return fmt.Errorf("备件数量矛盾：领用记录 %s 的累计退回 %d 超出原数量 %d", p.ID, p.Returned, p.Quantity)
		}
		t := tickets[p.TicketID]
		if t == nil {
			return fmt.Errorf("数据矛盾：领用记录 %s 引用了不存在的工单 %s", p.ID, p.TicketID)
		}
		if t.AssetID != p.AssetID {
			return fmt.Errorf("数据矛盾：领用记录 %s 的资产 %s 与工单 %s 归属的资产 %s 不一致",
				p.ID, p.AssetID, t.ID, t.AssetID)
		}
		if parts[p.ID] != nil {
			return fmt.Errorf("数据矛盾：领用编号 %s 重复", p.ID)
		}
		parts[p.ID] = p
	}
	// 下一领用序号必须为正并大于全部已用序号（允许有间隔）。
	if d.NextPartSeq < 1 || d.NextPartSeq <= maxPartSeq {
		return fmt.Errorf("计数器矛盾：下一领用序号 %d 必须为正并大于全部已用序号（当前最大 %d）",
			d.NextPartSeq, maxPartSeq)
	}
	// 附件索引记录：编号唯一且为 A 加补零正整数序号，归属存在的工单且资产归属
	// 与工单一致，路径为绝对路径，说明非空；撤销状态与理由相互呼应。文件当前
	// 是否可用不参与整库一致性校验；与履历的接续在下方按履历序号重放时核对。
	attachments := map[string]*Attachment{}
	maxAttachSeq := 0
	for _, a := range d.Attachments {
		if a == nil || a.ID == "" || a.TicketID == "" || a.AssetID == "" || a.Path == "" || a.Note == "" {
			return errors.New("数据矛盾：存在字段不完整的附件记录")
		}
		n, ok := parseAttachSeq(a.ID)
		if !ok {
			return fmt.Errorf("计数器矛盾：附件编号 %q 不是 A 加补零正整数序号的形式", a.ID)
		}
		if n > maxAttachSeq {
			maxAttachSeq = n
		}
		if !filepath.IsAbs(a.Path) {
			return fmt.Errorf("数据矛盾：附件记录 %s 的路径 %q 不是绝对路径", a.ID, a.Path)
		}
		t := tickets[a.TicketID]
		if t == nil {
			return fmt.Errorf("数据矛盾：附件记录 %s 引用了不存在的工单 %s", a.ID, a.TicketID)
		}
		if t.AssetID != a.AssetID {
			return fmt.Errorf("数据矛盾：附件记录 %s 的资产 %s 与工单 %s 归属的资产 %s 不一致",
				a.ID, a.AssetID, t.ID, t.AssetID)
		}
		if a.Revoked && a.RevokeReason == "" {
			return fmt.Errorf("状态矛盾：已撤销附件 %s 缺少撤销理由", a.ID)
		}
		if !a.Revoked && a.RevokeReason != "" {
			return fmt.Errorf("状态矛盾：有效附件 %s 不应带有撤销理由", a.ID)
		}
		if attachments[a.ID] != nil {
			return fmt.Errorf("数据矛盾：附件编号 %s 重复", a.ID)
		}
		attachments[a.ID] = a
	}
	// 下一附件序号必须为正并大于全部已用序号（允许有间隔）。
	if d.NextAttachSeq < 1 || d.NextAttachSeq <= maxAttachSeq {
		return fmt.Errorf("计数器矛盾：下一附件序号 %d 必须为正并大于全部已用序号（当前最大 %d）",
			d.NextAttachSeq, maxAttachSeq)
	}
	// 请求绑定：每张工单恰有一条绑定，且请求标识、资产、描述、工单编号完全一致。
	boundReq := map[string]bool{}
	boundTicket := map[string]bool{}
	for _, r := range d.Requests {
		if r.RequestID == "" || r.AssetID == "" || r.Description == "" || r.TicketID == "" {
			return errors.New("请求绑定矛盾：存在字段不完整的请求绑定记录")
		}
		if boundReq[r.RequestID] {
			return fmt.Errorf("请求绑定矛盾：请求标识 %s 被绑定多次", r.RequestID)
		}
		boundReq[r.RequestID] = true
		t := tickets[r.TicketID]
		if t == nil {
			return fmt.Errorf("请求绑定矛盾：请求标识 %s 绑定的工单 %s 不存在", r.RequestID, r.TicketID)
		}
		if t.RequestID != r.RequestID {
			return fmt.Errorf("请求绑定矛盾：工单 %s 的请求标识为 %s，绑定记录却来自请求 %s",
				r.TicketID, t.RequestID, r.RequestID)
		}
		if r.AssetID != t.AssetID || r.Description != t.Description {
			return fmt.Errorf("请求绑定矛盾：请求标识 %s 绑定的资产编号或故障描述与工单 %s 不一致",
				r.RequestID, t.ID)
		}
		boundTicket[r.TicketID] = true
	}
	for _, t := range d.Tickets {
		if !boundTicket[t.ID] {
			return fmt.Errorf("请求绑定矛盾：工单 %s 缺少对应的报修请求绑定", t.ID)
		}
	}
	// 状态：每项资产最多一张未关闭工单，有则必须为“维修中”；无未关闭工单时
	// 资产可为“可用”或“停用”，停用与否由停用、恢复使用履历链决定，在下方
	// 按履历序号重放时核对。
	openCount := map[string]int{}
	for _, t := range d.Tickets {
		if t.Status == ticketOpen {
			openCount[t.AssetID]++
		}
	}
	for _, a := range d.Assets {
		c := openCount[a.ID]
		if c > 1 {
			return fmt.Errorf("状态矛盾：资产 %s 有 %d 张未关闭工单，最多允许一张", a.ID, c)
		}
		if c == 1 {
			if a.Status != statusRepairing {
				return fmt.Errorf("状态矛盾：资产 %s 有 %d 张未关闭工单，状态应为 %s，实际为 %s",
					a.ID, c, statusRepairing, a.Status)
			}
			continue
		}
		if a.Status != statusAvailable && a.Status != statusDeactivated {
			return fmt.Errorf("状态矛盾：资产 %s 没有未关闭工单，状态应为 %s 或 %s，实际为 %s",
				a.ID, statusAvailable, statusDeactivated, a.Status)
		}
	}
	// 履历：序号全库唯一且为正整数；归属、内容与工单一致。
	seenSeq := map[int]bool{}
	reportSeq := map[string]int{}
	closeSeq := map[string]int{}
	cancelSeq := map[string]int{}
	reportCount := map[string]int{}
	closeCount := map[string]int{}
	cancelCount := map[string]int{}
	for i := range d.Events {
		e := &d.Events[i]
		if e.Seq < 1 {
			return fmt.Errorf("履历矛盾：履历序号 %d 不是正整数", e.Seq)
		}
		if seenSeq[e.Seq] {
			return fmt.Errorf("履历矛盾：履历序号 %d 重复", e.Seq)
		}
		seenSeq[e.Seq] = true
		if e.AssetID == "" || e.Kind == "" || e.Content == "" {
			return errors.New("履历矛盾：存在字段不完整的履历记录")
		}
		if e.Kind == eventPlanCreate || e.Kind == eventPlanDone || e.Kind == eventPlanRevoke || e.Kind == eventPlanAdjust {
			// 保养履历：不属于任何工单，不携带派工、备件或附件字段；日期与间隔在此
			// 核对，建立、完成、撤销与调整链的接续在下方按序号重放时核对。
			if e.TicketID != "" || e.From != "" || e.To != "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有工单编号或派工人员字段", e.Seq, e.Kind)
			}
			if e.PartID != "" || e.Quantity != 0 || e.WithdrawalID != "" ||
				e.AttachmentID != "" || e.Path != "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有备件或附件字段", e.Seq, e.Kind)
			}
			if e.Kind != eventPlanAdjust &&
				(e.NewContent != "" || e.OldContent != "" || e.OldDue != "" || e.OldInterval != 0 || e.OldNextDue != "") {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有方案调整字段", e.Seq, e.Kind)
			}
			if e.Kind == eventPlanAdjust {
				// 调整履历：内容即调整理由（非空已检查）；Due/Interval 为新方案的
				// 首次到期日与间隔天数，另携带原方案与调整前的下一到期日。
				if e.Done != "" || e.TargetSeq != 0 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录不应带有完成日或目标完成履历序号", e.Seq)
				}
				if e.NewContent == "" {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录缺少新保养内容", e.Seq)
				}
				if _, err := parseDate(e.Due); err != nil {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录新首次到期日无效", e.Seq)
				}
				if e.Interval < 1 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录新间隔天数须为正整数", e.Seq)
				}
				if e.OldContent == "" {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录缺少原保养内容", e.Seq)
				}
				if _, err := parseDate(e.OldDue); err != nil {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录原首次到期日无效", e.Seq)
				}
				if e.OldInterval < 1 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录原间隔天数须为正整数", e.Seq)
				}
				if _, err := parseDate(e.OldNextDue); err != nil {
					return fmt.Errorf("履历矛盾：履历序号 %d 的调整记录原下一到期日无效", e.Seq)
				}
				continue
			}
			if e.Kind == eventPlanRevoke {
				// 撤销履历：内容即撤销理由（非空已检查），仅携带目标完成履历序号。
				if e.Due != "" || e.Done != "" || e.Interval != 0 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的撤销记录不应带有到期日、完成日或间隔天数", e.Seq)
				}
				if e.TargetSeq < 1 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的撤销记录目标完成履历序号须为正整数", e.Seq)
				}
				continue
			}
			if e.TargetSeq != 0 {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有目标完成履历序号", e.Seq, e.Kind)
			}
			due, err := parseDate(e.Due)
			if err != nil {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录到期日无效", e.Seq, e.Kind)
			}
			if e.Kind == eventPlanCreate {
				if e.Done != "" {
					return fmt.Errorf("履历矛盾：履历序号 %d 的建立记录不应带有完成日", e.Seq)
				}
				if e.Interval < 1 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的建立记录间隔天数须为正整数", e.Seq)
				}
			} else {
				if e.Interval != 0 {
					return fmt.Errorf("履历矛盾：履历序号 %d 的完成记录不应带有间隔天数", e.Seq)
				}
				done, err := parseDate(e.Done)
				if err != nil {
					return fmt.Errorf("履历矛盾：履历序号 %d 的完成记录完成日无效", e.Seq)
				}
				if done < due {
					return fmt.Errorf("履历矛盾：履历序号 %d 的完成日早于周期到期日", e.Seq)
				}
			}
			continue
		}
		if e.Kind == eventDeactivate || e.Kind == eventReactivate {
			// 停用/恢复使用为资产级履历：不属于任何工单，From/To 为原状态与新
			// 状态，不携带派工、保养、备件或附件字段。转换是否接续（可用->停用、
			// 停用->可用）在下方按履历序号重放时核对。
			if e.TicketID != "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有工单编号", e.Seq, e.Kind)
			}
			if e.From == "" || e.To == "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录缺少原状态或新状态", e.Seq, e.Kind)
			}
			if e.Due != "" || e.Done != "" || e.Interval != 0 || e.TargetSeq != 0 ||
				e.NewContent != "" || e.OldContent != "" || e.OldDue != "" || e.OldInterval != 0 || e.OldNextDue != "" ||
				e.PartID != "" || e.Quantity != 0 || e.WithdrawalID != "" ||
				e.AttachmentID != "" || e.Path != "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有保养、备件或附件字段", e.Seq, e.Kind)
			}
			if assets[e.AssetID] == nil {
				return fmt.Errorf("履历矛盾：履历序号 %d 引用了不存在的资产", e.Seq)
			}
			continue
		}
		if e.TicketID == "" {
			return errors.New("履历矛盾：存在字段不完整的履历记录")
		}
		if e.Due != "" || e.Done != "" || e.Interval != 0 || e.TargetSeq != 0 ||
			e.NewContent != "" || e.OldContent != "" || e.OldDue != "" || e.OldInterval != 0 || e.OldNextDue != "" {
			return fmt.Errorf("履历矛盾：履历序号 %d 的维修履历不应带有保养字段", e.Seq)
		}
		if e.Kind != eventAssign && (e.From != "" || e.To != "") {
			return fmt.Errorf("履历矛盾：履历序号 %d 的 %s 记录不应带有派工人员字段", e.Seq, e.Kind)
		}
		if e.Kind == eventAssign {
			if e.To == "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的派工记录缺少新负责人", e.Seq)
			}
			if e.From == e.To {
				return fmt.Errorf("履历矛盾：履历序号 %d 的派工记录新负责人与原负责人相同", e.Seq)
			}
		}
		// 附件字段仅附件履历携带；附件履历不携带备件字段。
		if e.Kind != eventAttach && e.Kind != eventAttachRevoke && (e.AttachmentID != "" || e.Path != "") {
			return fmt.Errorf("履历矛盾：履历序号 %d 的 %s 记录不应带有附件编号或路径", e.Seq, e.Kind)
		}
		if (e.Kind == eventAttach || e.Kind == eventAttachRevoke) &&
			(e.PartID != "" || e.Quantity != 0 || e.WithdrawalID != "") {
			return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有备件字段", e.Seq, e.Kind)
		}
		t := tickets[e.TicketID]
		if t == nil || assets[e.AssetID] == nil {
			return fmt.Errorf("履历矛盾：履历序号 %d 引用了不存在的资产或工单", e.Seq)
		}
		if e.AssetID != t.AssetID {
			return fmt.Errorf("履历矛盾：履历序号 %d 的资产 %s 与工单 %s 归属的资产 %s 不一致",
				e.Seq, e.AssetID, t.ID, t.AssetID)
		}
		switch e.Kind {
		case eventReport:
			if e.Content != t.Description {
				return fmt.Errorf("履历矛盾：工单 %s 的报修履历内容与故障描述不一致", t.ID)
			}
			reportCount[t.ID]++
			reportSeq[t.ID] = e.Seq
		case eventClose:
			if e.Content != t.Result {
				return fmt.Errorf("履历矛盾：工单 %s 的关闭履历内容与维修结果不一致", t.ID)
			}
			closeCount[t.ID]++
			closeSeq[t.ID] = e.Seq
		case eventCancel:
			if e.Content != t.CancelReason {
				return fmt.Errorf("履历矛盾：工单 %s 的取消履历内容与取消理由不一致", t.ID)
			}
			cancelCount[t.ID]++
			cancelSeq[t.ID] = e.Seq
		case eventAssign:
			// 内容即派工说明（非空已检查）；派工链的接续与工单负责人一致性
			// 在下方按序号重放时核对。
		case eventPartWithdraw, eventPartReturn:
			// 内容即领用说明或退回理由（非空已检查）；备件字段在此核对，
			// 与领用记录的接续及累计退回在下方按序号重放时核对。
			if e.PartID == "" || e.WithdrawalID == "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录缺少备件编号或领用编号", e.Seq, e.Kind)
			}
			if e.Quantity < 1 {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录数量 %d 不是正整数", e.Seq, e.Kind, e.Quantity)
			}
			if _, ok := parsePartSeq(e.WithdrawalID); !ok {
				return fmt.Errorf("履历矛盾：履历序号 %d 的领用编号 %q 不是 P 加补零正整数序号的形式",
					e.Seq, e.WithdrawalID)
			}
		case eventAttach, eventAttachRevoke:
			// 内容即附件说明或撤销理由（非空已检查）；附件字段在此核对，
			// 与附件记录的接续及登记、撤销顺序在下方按序号重放时核对。
			if e.AttachmentID == "" || e.Path == "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录缺少附件编号或路径", e.Seq, e.Kind)
			}
			if _, ok := parseAttachSeq(e.AttachmentID); !ok {
				return fmt.Errorf("履历矛盾：履历序号 %d 的附件编号 %q 不是 A 加补零正整数序号的形式",
					e.Seq, e.AttachmentID)
			}
			if !filepath.IsAbs(e.Path) {
				return fmt.Errorf("履历矛盾：履历序号 %d 的附件路径 %q 不是绝对路径", e.Seq, e.Path)
			}
		default:
			return fmt.Errorf("履历矛盾：履历序号 %d 的事件类型无效 %q", e.Seq, e.Kind)
		}
	}
	for _, t := range d.Tickets {
		if reportCount[t.ID] != 1 {
			return fmt.Errorf("履历矛盾：工单 %s 应有恰一条报修履历，实际 %d 条", t.ID, reportCount[t.ID])
		}
		if t.Status == ticketClosed {
			if closeCount[t.ID] != 1 {
				return fmt.Errorf("履历矛盾：已关闭工单 %s 应有恰一条关闭履历，实际 %d 条", t.ID, closeCount[t.ID])
			}
			if closeSeq[t.ID] <= reportSeq[t.ID] {
				return fmt.Errorf("履历矛盾：工单 %s 的关闭履历（序号 %d）不在报修履历（序号 %d）之后",
					t.ID, closeSeq[t.ID], reportSeq[t.ID])
			}
		} else if closeCount[t.ID] != 0 {
			return fmt.Errorf("履历矛盾：非已关闭工单 %s 不得有关闭履历", t.ID)
		}
		if t.Status == ticketCancelled {
			if cancelCount[t.ID] != 1 {
				return fmt.Errorf("履历矛盾：已取消工单 %s 应有恰一条取消履历，实际 %d 条", t.ID, cancelCount[t.ID])
			}
			if cancelSeq[t.ID] <= reportSeq[t.ID] {
				return fmt.Errorf("履历矛盾：工单 %s 的取消履历（序号 %d）不在报修履历（序号 %d）之后",
					t.ID, cancelSeq[t.ID], reportSeq[t.ID])
			}
		} else if cancelCount[t.ID] != 0 {
			return fmt.Errorf("履历矛盾：非已取消工单 %s 不得有取消履历", t.ID)
		}
	}
	// 按履历序号推进，重放得到的工单与资产状态须与保存的状态相符；
	// 同一资产的上一张工单必须先关闭或取消才可产生下一张报修。
	sorted := make([]Event, len(d.Events))
	copy(sorted, d.Events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	derivedTicket := map[string]string{}
	derivedOpen := map[string]string{}
	derivedDeactivated := map[string]bool{}
	derivedAssignee := map[string]string{}
	derivedNote := map[string]string{}
	withdrawSeq := map[string]int{}
	derivedReturned := map[string]int{}
	attachRegSeq := map[string]int{}
	attachRevokeSeq := map[string]int{}
	for _, e := range sorted {
		switch e.Kind {
		case eventReport:
			if derivedTicket[e.TicketID] != "" {
				return fmt.Errorf("履历矛盾：工单 %s 被重复报修", e.TicketID)
			}
			if prev := derivedOpen[e.AssetID]; prev != "" {
				return fmt.Errorf("履历矛盾：资产 %s 的上一张工单 %s 尚未结束就产生了工单 %s 的报修",
					e.AssetID, prev, e.TicketID)
			}
			if derivedDeactivated[e.AssetID] {
				return fmt.Errorf("履历矛盾：资产 %s 在停用期间产生了工单 %s 的报修",
					e.AssetID, e.TicketID)
			}
			derivedTicket[e.TicketID] = ticketOpen
			derivedOpen[e.AssetID] = e.TicketID
		case eventDeactivate:
			// 停用要求当时“可用”（无未关闭工单且未停用）；维修中（有未关闭
			// 工单）的资产不能停用。From/To 须与实际转换一致。
			if derivedOpen[e.AssetID] != "" {
				return fmt.Errorf("履历矛盾：资产 %s 在存在未关闭工单时出现停用履历（序号 %d）",
					e.AssetID, e.Seq)
			}
			if derivedDeactivated[e.AssetID] {
				return fmt.Errorf("履历矛盾：资产 %s 被重复停用（序号 %d）", e.AssetID, e.Seq)
			}
			if e.From != statusAvailable || e.To != statusDeactivated {
				return fmt.Errorf("履历矛盾：停用履历（序号 %d）原状态应为 %s、新状态应为 %s，实际为 %s -> %s",
					e.Seq, statusAvailable, statusDeactivated, e.From, e.To)
			}
			derivedDeactivated[e.AssetID] = true
		case eventReactivate:
			// 恢复使用只作用于停用资产；From/To 须与实际转换一致。
			if !derivedDeactivated[e.AssetID] {
				return fmt.Errorf("履历矛盾：资产 %s 未停用却出现恢复使用履历（序号 %d）", e.AssetID, e.Seq)
			}
			if derivedOpen[e.AssetID] != "" {
				return fmt.Errorf("履历矛盾：资产 %s 恢复使用（序号 %d）时仍存在未关闭工单",
					e.AssetID, e.Seq)
			}
			if e.From != statusDeactivated || e.To != statusAvailable {
				return fmt.Errorf("履历矛盾：恢复使用履历（序号 %d）原状态应为 %s、新状态应为 %s，实际为 %s -> %s",
					e.Seq, statusDeactivated, statusAvailable, e.From, e.To)
			}
			delete(derivedDeactivated, e.AssetID)
		case eventAssign:
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现派工履历", e.TicketID)
			}
			if e.From != derivedAssignee[e.TicketID] {
				return fmt.Errorf("履历矛盾：工单 %s 的派工履历（序号 %d）原负责人 %q 与上次记录不接续",
					e.TicketID, e.Seq, e.From)
			}
			derivedAssignee[e.TicketID] = e.To
			derivedNote[e.TicketID] = e.Content
		case eventClose:
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现关闭履历", e.TicketID)
			}
			derivedTicket[e.TicketID] = ticketClosed
			delete(derivedOpen, e.AssetID)
		case eventCancel:
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现取消履历", e.TicketID)
			}
			derivedTicket[e.TicketID] = ticketCancelled
			delete(derivedOpen, e.AssetID)
		case eventPartWithdraw:
			// 领用须在报修之后、终结之前；履历与领用记录的业务字段必须一致。
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现领用履历（序号 %d）",
					e.TicketID, e.Seq)
			}
			p := parts[e.WithdrawalID]
			if p == nil {
				return fmt.Errorf("履历矛盾：领用履历（序号 %d）引用了不存在的领用记录 %s",
					e.Seq, e.WithdrawalID)
			}
			if withdrawSeq[p.ID] != 0 {
				return fmt.Errorf("履历矛盾：领用记录 %s 有多条领用履历", p.ID)
			}
			if p.TicketID != e.TicketID || p.AssetID != e.AssetID || p.PartID != e.PartID ||
				p.Quantity != e.Quantity || p.Note != e.Content {
				return fmt.Errorf("履历矛盾：领用履历（序号 %d）与领用记录 %s 的工单、资产、备件、数量或说明不一致",
					e.Seq, p.ID)
			}
			withdrawSeq[p.ID] = e.Seq
		case eventPartReturn:
			// 退回须在报修之后、终结之前，且指向先前已领用的记录；
			// 累计退回不得超过该笔原数量。
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现退回履历（序号 %d）",
					e.TicketID, e.Seq)
			}
			p := parts[e.WithdrawalID]
			if p == nil {
				return fmt.Errorf("履历矛盾：退回履历（序号 %d）引用了不存在的领用记录 %s",
					e.Seq, e.WithdrawalID)
			}
			if withdrawSeq[p.ID] == 0 {
				return fmt.Errorf("履历矛盾：退回履历（序号 %d）出现在领用记录 %s 的领用履历之前",
					e.Seq, p.ID)
			}
			if p.TicketID != e.TicketID || p.AssetID != e.AssetID || p.PartID != e.PartID {
				return fmt.Errorf("履历矛盾：退回履历（序号 %d）与领用记录 %s 的工单、资产或备件不一致",
					e.Seq, p.ID)
			}
			// 逐步核对累计退回不超过原数量：先做不会溢出的减法比较再累加。
			// 直接相加可能整数溢出回绕，即使回绕后的累计恰好等于保存值，
			// 实际超额的履历也必须拒绝。
			if e.Quantity > p.Quantity-derivedReturned[p.ID] {
				return fmt.Errorf("备件数量矛盾：领用记录 %s 的累计退回在履历序号 %d 处超过原数量 %d",
					p.ID, e.Seq, p.Quantity)
			}
			derivedReturned[p.ID] += e.Quantity
		case eventAttach:
			// 登记须在报修之后；未关闭、已关闭或已取消工单均可补充资料。
			// 履历与附件记录的业务字段必须一致。
			if derivedTicket[e.TicketID] == "" {
				return fmt.Errorf("履历矛盾：工单 %s 在报修之前出现附件登记履历（序号 %d）",
					e.TicketID, e.Seq)
			}
			a := attachments[e.AttachmentID]
			if a == nil {
				return fmt.Errorf("履历矛盾：附件登记履历（序号 %d）引用了不存在的附件记录 %s",
					e.Seq, e.AttachmentID)
			}
			if attachRegSeq[a.ID] != 0 {
				return fmt.Errorf("履历矛盾：附件记录 %s 有多条登记履历", a.ID)
			}
			if a.TicketID != e.TicketID || a.AssetID != e.AssetID || a.Path != e.Path || a.Note != e.Content {
				return fmt.Errorf("履历矛盾：附件登记履历（序号 %d）与附件记录 %s 的工单、资产、路径或说明不一致",
					e.Seq, a.ID)
			}
			attachRegSeq[a.ID] = e.Seq
		case eventAttachRevoke:
			// 撤销须在登记之后，且每个附件至多撤销一次；
			// 履历与附件记录的业务字段必须一致。
			if derivedTicket[e.TicketID] == "" {
				return fmt.Errorf("履历矛盾：工单 %s 在报修之前出现附件撤销履历（序号 %d）",
					e.TicketID, e.Seq)
			}
			a := attachments[e.AttachmentID]
			if a == nil {
				return fmt.Errorf("履历矛盾：附件撤销履历（序号 %d）引用了不存在的附件记录 %s",
					e.Seq, e.AttachmentID)
			}
			if attachRegSeq[a.ID] == 0 {
				return fmt.Errorf("履历矛盾：附件撤销履历（序号 %d）出现在附件记录 %s 的登记履历之前",
					e.Seq, a.ID)
			}
			if attachRevokeSeq[a.ID] != 0 {
				return fmt.Errorf("履历矛盾：附件记录 %s 有多条撤销履历", a.ID)
			}
			if a.TicketID != e.TicketID || a.AssetID != e.AssetID || a.Path != e.Path || a.RevokeReason != e.Content {
				return fmt.Errorf("履历矛盾：附件撤销履历（序号 %d）与附件记录 %s 的工单、资产、路径或撤销理由不一致",
					e.Seq, a.ID)
			}
			attachRevokeSeq[a.ID] = e.Seq
		}
	}
	for _, t := range d.Tickets {
		if derivedTicket[t.ID] != t.Status {
			return fmt.Errorf("状态矛盾：按履历推进得到工单 %s 状态为 %s，与保存的 %s 不符",
				t.ID, derivedTicket[t.ID], t.Status)
		}
		// 由履历推出的最后负责人须与工单记录一致；无派工履历则必须未派工。
		if derivedAssignee[t.ID] != t.Assignee {
			return fmt.Errorf("状态矛盾：按履历推出工单 %s 最后负责人为 %q，与保存的 %q 不符",
				t.ID, derivedAssignee[t.ID], t.Assignee)
		}
		if t.Assignee != "" && derivedNote[t.ID] != t.AssignNote {
			return fmt.Errorf("状态矛盾：工单 %s 保存的派工说明与最后一次派工履历不一致", t.ID)
		}
	}
	// 每笔领用记录恰有一条领用履历；按履历重放得到的累计退回须与保存值一致。
	for _, p := range d.Parts {
		if withdrawSeq[p.ID] == 0 {
			return fmt.Errorf("履历矛盾：领用记录 %s 缺少对应的领用履历", p.ID)
		}
		if derivedReturned[p.ID] != p.Returned {
			return fmt.Errorf("备件数量矛盾：按履历推出领用记录 %s 的累计退回为 %d，与保存的 %d 不符",
				p.ID, derivedReturned[p.ID], p.Returned)
		}
	}
	// 每条附件记录恰有一条登记履历、至多一条撤销履历；记录的撤销状态须与
	// 是否存在撤销履历一致。
	for _, a := range d.Attachments {
		if attachRegSeq[a.ID] == 0 {
			return fmt.Errorf("履历矛盾：附件记录 %s 缺少对应的登记履历", a.ID)
		}
		if revoked := attachRevokeSeq[a.ID] != 0; revoked != a.Revoked {
			return fmt.Errorf("状态矛盾：附件记录 %s 保存的撤销状态与履历不符", a.ID)
		}
	}
	for _, a := range d.Assets {
		want := statusAvailable
		switch {
		case derivedDeactivated[a.ID]:
			want = statusDeactivated
		case derivedOpen[a.ID] != "":
			want = statusRepairing
		}
		if a.Status != want {
			return fmt.Errorf("状态矛盾：按履历推进得到资产 %s 状态为 %s，与保存的 %s 不符",
				a.ID, want, a.Status)
		}
	}
	// 保养计划：归属存在的资产、每项资产最多一个、日期有效、间隔为正整数。
	plans := map[string]*Plan{}
	for _, p := range d.Plans {
		if p == nil || p.AssetID == "" || p.Content == "" {
			return errors.New("数据矛盾：存在字段不完整的保养计划")
		}
		if assets[p.AssetID] == nil {
			return fmt.Errorf("数据矛盾：保养计划引用了不存在的资产 %s", p.AssetID)
		}
		if plans[p.AssetID] != nil {
			return fmt.Errorf("数据矛盾：资产 %s 有多个保养计划，最多允许一个", p.AssetID)
		}
		if _, err := parseDate(p.FirstDue); err != nil {
			return fmt.Errorf("数据矛盾：资产 %s 保养计划的首次到期日无效", p.AssetID)
		}
		if p.IntervalDays < 1 {
			return fmt.Errorf("数据矛盾：资产 %s 保养计划的间隔天数须为正整数", p.AssetID)
		}
		if _, err := parseDate(p.NextDue); err != nil {
			return fmt.Errorf("数据矛盾：资产 %s 保养计划的下一到期日无效", p.AssetID)
		}
		plans[p.AssetID] = p
	}
	// 保养履历链的接续核对收敛到共用核心：按全库履历序号逐步重放建立、
	// 完成、撤销与调整，逐步采用当时的方案、有效完成与停用状态（不能用最终
	// 状态代替历史状态）。数组乱序、序号有间隔、时间不递增都不影响——顺序
	// 只由序号决定。纯重放不修改 d 中的任何记录。
	rr, err := replayAllMaintenance(d)
	if err != nil {
		return fmt.Errorf("履历矛盾：%w", err)
	}
	// 有保养履历的资产必须有计划；没有计划的资产不得有保养履历。
	for assetID := range rr.states {
		if plans[assetID] == nil {
			return fmt.Errorf("履历矛盾：资产 %s 没有保养计划，却存在保养履历", assetID)
		}
	}
	// 每个计划恰有一条建立履历；由履历推出的当前方案段方案与下一到期日须与
	// 计划保存值一致，最终保存方案也在核对之列。
	for _, p := range d.Plans {
		st := rr.states[p.AssetID]
		if st == nil || !st.created {
			return fmt.Errorf("履历矛盾：资产 %s 的保养计划应有恰一条建立履历，实际没有", p.AssetID)
		}
		if st.spec.content != p.Content || st.spec.firstDue != p.FirstDue || st.spec.interval != p.IntervalDays {
			return fmt.Errorf("履历矛盾：按履历推出资产 %s 的当前方案（内容 %q、首次到期日 %s、每 %d 天）与保存的方案（内容 %q、首次到期日 %s、每 %d 天）不符",
				p.AssetID, st.spec.content, st.spec.firstDue, st.spec.interval,
				p.Content, p.FirstDue, p.IntervalDays)
		}
		if st.nextDue != p.NextDue {
			return fmt.Errorf("履历矛盾：按履历推出资产 %s 的下一到期日为 %s，与保存的 %s 不符",
				p.AssetID, st.nextDue, p.NextDue)
		}
	}
	return nil
}

// save 将全部业务数据一次性原子写入：先写同目录临时文件，fsync 后 rename
// 覆盖正式文件。写入前先校验待提交数据的一致性；校验或读写失败时原文件
// 保持不变，不留下部分业务变化。
func (s *store) save() error {
	s.data.EventsJSON = make([]eventJSON, len(s.data.Events))
	for i, e := range s.data.Events {
		s.data.EventsJSON[i] = e.toJSON()
	}
	if err := validateData(s.data); err != nil {
		return fmt.Errorf("待保存数据未通过一致性检查：%w（未写入任何数据）", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.data); err != nil {
		return fmt.Errorf("编码数据失败: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	path := filepath.Join(s.dir, dataFileName)
	tmp, err := os.CreateTemp(s.dir, ".caretrack-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时数据文件失败: %w", err)
	}
	tmpName := tmp.Name()
	abort := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		abort(nil)
		return fmt.Errorf("写入数据失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		abort(nil)
		return fmt.Errorf("写入数据失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("写入数据失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("保存数据失败: %w", err)
	}
	return nil
}

func (s *store) findAsset(id string) *Asset {
	for _, a := range s.data.Assets {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (s *store) findTicket(id string) *Ticket {
	for _, t := range s.data.Tickets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *store) findRequest(requestID string) *requestBinding {
	for i := range s.data.Requests {
		if s.data.Requests[i].RequestID == requestID {
			return &s.data.Requests[i]
		}
	}
	return nil
}

func (s *store) openTicketOf(assetID string) *Ticket {
	for _, t := range s.data.Tickets {
		if t.AssetID == assetID && t.Status == ticketOpen {
			return t
		}
	}
	return nil
}

func (s *store) eventsOf(assetID string) []Event {
	out := make([]Event, 0)
	for _, e := range s.data.Events {
		if e.AssetID == assetID {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// nextEventSeq 返回下一条履历应使用的全库唯一正整数序号；计数器耗尽时报错。
func (s *store) nextEventSeq() (int, error) {
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	if maxSeq == math.MaxInt {
		return 0, fmt.Errorf("%w: 履历序号计数器已耗尽，无法追加履历", errConflict)
	}
	return maxSeq + 1, nil
}

// appendEvent 以给定序号追加履历。调用方须先通过 nextEventSeq 取得序号，
// 确保任何失败路径都不会在部分变更后才报错。from/to 仅派工履历使用。
func (s *store) appendEvent(seq int, assetID, ticketID, kind, content, from, to string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:      seq,
		AssetID:  assetID,
		TicketID: ticketID,
		Kind:     kind,
		Content:  content,
		From:     from,
		To:       to,
		Time:     s.now(),
	})
}

func (s *store) registerAsset(id, name, location string) (*Asset, error) {
	if s.findAsset(id) != nil {
		return nil, fmt.Errorf("%w: 资产编号 %q 已存在", errConflict, id)
	}
	a := &Asset{ID: id, Name: name, Location: location, Status: statusAvailable}
	s.data.Assets = append(s.data.Assets, a)
	return a, nil
}

// report 创建报修工单。返回 (工单, 是否为去重重放, 错误)。
// 所有失败路径都不修改任何业务数据，也不会绑定新的请求标识。
func (s *store) report(assetID, description, requestID string) (*Ticket, bool, error) {
	// 已有绑定：相同资产+描述 => 返回原工单（无论其是否已关闭、是否已有新工单）；
	// 资产或描述不同 => 拒绝。
	if r := s.findRequest(requestID); r != nil {
		if r.AssetID != assetID || r.Description != description {
			return nil, false, fmt.Errorf(
				"%w: 请求标识 %q 已用于资产 %q 的另一笔报修，不能搭配不同的资产编号或故障描述",
				errConflict, requestID, r.AssetID)
		}
		t := s.findTicket(r.TicketID)
		if t == nil {
			return nil, false, fmt.Errorf("数据内部错误：请求标识 %q 绑定的工单不存在", requestID)
		}
		return t, true, nil
	}

	asset := s.findAsset(assetID)
	if asset == nil {
		return nil, false, fmt.Errorf("%w: 未知资产编号 %q", errNotFound, assetID)
	}
	if asset.Status == statusDeactivated {
		// 停用期间拒绝新报修，也不绑定新请求标识；恢复使用后可用同一标识重试。
		return nil, false, fmt.Errorf("%w: 资产 %s 已停用，不能报修", errConflict, assetID)
	}
	if t := s.openTicketOf(assetID); t != nil {
		return nil, false, fmt.Errorf("%w: 资产 %s 已有未关闭工单 %s", errConflict, assetID, t.ID)
	}
	// 先确认编号与履历计数器都能推进，再修改任何业务数据：
	// 编号耗尽时拒绝新报修，不消耗编号，也不绑定请求标识。
	seq := s.data.NextTicketSeq
	if seq < 1 || seq == math.MaxInt {
		return nil, false, fmt.Errorf("%w: 工单编号计数器已耗尽，无法分配新工单编号", errConflict)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, false, err
	}
	t := &Ticket{
		ID:          fmt.Sprintf("T%04d", seq),
		AssetID:     assetID,
		Description: description,
		RequestID:   requestID,
		Status:      ticketOpen,
		CreatedAt:   s.now().Format(time.RFC3339),
	}
	s.data.NextTicketSeq = seq + 1
	s.data.Tickets = append(s.data.Tickets, t)
	s.data.Requests = append(s.data.Requests, requestBinding{
		RequestID:   requestID,
		AssetID:     assetID,
		Description: description,
		TicketID:    t.ID,
	})
	asset.Status = statusRepairing
	s.appendEvent(eventSeq, assetID, t.ID, eventReport, description, "", "")
	return t, false, nil
}

// closeTicket 关闭未关闭工单并把资产恢复为可用。
// 失败路径不修改任何业务数据。
func (s *store) closeTicket(ticketID, result string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已取消，不能关闭", errConflict, ticketID)
	}
	if t.Status != ticketOpen {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能重复关闭", errConflict, ticketID)
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	t.Status = ticketClosed
	t.Result = result
	t.ClosedAt = s.now().Format(time.RFC3339)
	asset.Status = statusAvailable
	s.appendEvent(eventSeq, asset.ID, t.ID, eventClose, result, "", "")
	return t, asset, nil
}

// cancelTicket 取消未关闭工单并把资产恢复为可用。取消是终态，但不代表维修完成：
// 不填写维修结果，不删除工单、履历或请求绑定，工单编号不回退也不复用。
// 失败路径不修改任何业务数据。
func (s *store) cancelTicket(ticketID, reason string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已取消，不能再次取消", errConflict, ticketID)
	}
	if t.Status == ticketClosed {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能取消", errConflict, ticketID)
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, nil, err
	}
	t.Status = ticketCancelled
	t.CancelReason = reason
	t.CancelledAt = s.now().Format(time.RFC3339)
	asset.Status = statusAvailable
	s.appendEvent(eventSeq, asset.ID, t.ID, eventCancel, reason, "", "")
	return t, asset, nil
}

// assignTicket 为未关闭工单派工或转派：没有负责人时首次派工，已有负责人时
// 转派给不同人员。人员标识只是本地文本记录，不对应任何人员账户。
// 成功后保存负责人、变更时间与说明，并追加一条含原负责人（首次派工为空，
// 展示为“未派工”）、新负责人及说明的派工履历。派工不创建工单、不消耗工单
// 编号，也不改变资产状态或报修请求绑定。失败路径不修改任何业务数据。
func (s *store) assignTicket(ticketID, assignee, note string) (*Ticket, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status == ticketClosed {
		return nil, fmt.Errorf("%w: 工单 %s 已关闭，不能派工", errConflict, ticketID)
	}
	if t.Status == ticketCancelled {
		return nil, fmt.Errorf("%w: 工单 %s 已取消，不能派工", errConflict, ticketID)
	}
	if assignee == "" {
		return nil, fmt.Errorf("%w: 维修人员标识不能为空", errConflict)
	}
	if note == "" {
		return nil, fmt.Errorf("%w: 派工说明不能为空", errConflict)
	}
	if t.Assignee == assignee {
		return nil, fmt.Errorf("%w: 工单 %s 当前负责人已是 %s，不能重复派给同一人",
			errConflict, ticketID, assignee)
	}
	eventSeq, err := s.nextEventSeq()
	if err != nil {
		return nil, err
	}
	s.appendEvent(eventSeq, t.AssetID, t.ID, eventAssign, note, t.Assignee, assignee)
	t.Assignee = assignee
	t.AssignedAt = s.now().Format(time.RFC3339)
	t.AssignNote = note
	return t, nil
}
