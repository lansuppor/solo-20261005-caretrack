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
	statusAvailable = "可用"
	statusRepairing = "维修中"
	ticketOpen      = "未关闭"
	ticketClosed    = "已关闭"
	ticketCancelled = "已取消"

	eventReport = "报修"
	eventClose  = "关闭"
	eventCancel = "取消"
	eventAssign = "派工"

	eventPlanCreate = "保养建立"
	eventPlanDone   = "保养完成"

	eventPartIssue  = "备件领用"
	eventPartReturn = "备件退回"

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

// Event 为履历条目（报修/派工/关闭/取消/保养建立/保养完成/备件领用/备件退回），
// Seq 决定操作发生顺序。
// From/To 仅派工履历使用：原负责人（首次派工为空，展示为“未派工”）与新负责人。
// Due/Done/Interval 仅保养履历使用：建立履历含首次到期日（Due）与间隔天数
// （Interval），完成履历含周期到期日（Due）与实际完成日（Done）。
// IssueID/PartNo/Qty 仅备件履历使用：领用或退回的领用编号、备件编号与数量，
// Content 为领用说明或退回理由。
type Event struct {
	Seq      int       `json:"-"`
	AssetID  string    `json:"-"`
	TicketID string    `json:"-"`
	Kind     string    `json:"-"`
	Content  string    `json:"-"`
	From     string    `json:"-"`
	To       string    `json:"-"`
	Due      string    `json:"-"`
	Done     string    `json:"-"`
	Interval int       `json:"-"`
	IssueID  string    `json:"-"`
	PartNo   string    `json:"-"`
	Qty      int       `json:"-"`
	Time     time.Time `json:"-"`
}

// eventJSON 与 Event 对应，时间以 RFC3339 文本持久化。
type eventJSON struct {
	Seq      int    `json:"seq"`
	AssetID  string `json:"asset_id"`
	TicketID string `json:"ticket_id,omitempty"`
	Kind     string `json:"kind"`
	Content  string `json:"content"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Due      string `json:"due,omitempty"`
	Done     string `json:"done,omitempty"`
	Interval int    `json:"interval,omitempty"`
	IssueID  string `json:"issue_id,omitempty"`
	PartNo   string `json:"part_no,omitempty"`
	Qty      int    `json:"qty,omitempty"`
	Time     string `json:"time"`
}

func (e Event) toJSON() eventJSON {
	return eventJSON{
		Seq: e.Seq, AssetID: e.AssetID, TicketID: e.TicketID,
		Kind: e.Kind, Content: e.Content, From: e.From, To: e.To,
		Due: e.Due, Done: e.Done, Interval: e.Interval,
		IssueID: e.IssueID, PartNo: e.PartNo, Qty: e.Qty,
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
		Due: e.Due, Done: e.Done, Interval: e.Interval,
		IssueID: e.IssueID, PartNo: e.PartNo, Qty: e.Qty, Time: t,
	}, nil
}

// requestBinding 记录请求标识与具体报修的绑定，用于去重与冲突拒绝；
// 它既不是资产编号也不是工单编号。
type requestBinding struct {
	RequestID   string `json:"request_id"`
	AssetID     string `json:"asset_id"`
	Description string `json:"description"`
	TicketID    string `json:"ticket_id"`
}

// storeData 是一次成功写操作共同生效的完整业务数据。
type storeData struct {
	Version       int              `json:"version"`
	Assets        []*Asset         `json:"assets"`
	Tickets       []*Ticket        `json:"tickets"`
	Events        []Event          `json:"-"`
	EventsJSON    []eventJSON      `json:"events"`
	Requests      []requestBinding `json:"requests"`
	Plans         []*Plan          `json:"plans"`
	PartIssues    []*PartIssue     `json:"part_issues"`
	NextTicketSeq int              `json:"next_ticket_seq"`
	NextIssueSeq  int              `json:"next_issue_seq"`
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
		PartIssues:    []*PartIssue{},
		NextTicketSeq: 1,
		NextIssueSeq:  1,
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
	// 领用编号计数器从 1 开始；已有领用记录而计数器缺失按矛盾处理（见校验）。
	if d.PartIssues == nil {
		d.PartIssues = []*PartIssue{}
	}
	if d.NextIssueSeq == 0 && len(d.PartIssues) == 0 {
		d.NextIssueSeq = 1
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

// parseIssueSeq 解析 P 加补零正整数序号形式的领用编号，返回序号。
// 编号必须与 fmt.Sprintf("P%04d", n)（n ≥ 1）完全一致，否则视为非法。
func parseIssueSeq(id string) (int, bool) {
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

// validateData 校验整份业务数据的内部一致性。任何矛盾都会返回指明问题类别
// （计数器/请求绑定/状态/履历）的错误；调用方必须拒绝查询与写入并保留原文件。
// 不做任何修复：不补字段、不重编号、不删除记录。
func validateData(d *storeData) error {
	if d.Version != storeVersion {
		return fmt.Errorf("不支持的数据版本 %d", d.Version)
	}
	if d.Assets == nil || d.Tickets == nil || d.Requests == nil || d.EventsJSON == nil || d.Events == nil || d.Plans == nil || d.PartIssues == nil {
		return errors.New("缺少必要的数据段")
	}
	assets := map[string]*Asset{}
	for _, a := range d.Assets {
		if a == nil || a.ID == "" || a.Name == "" || a.Location == "" {
			return errors.New("数据矛盾：存在字段不完整的资产记录")
		}
		if a.Status != statusAvailable && a.Status != statusRepairing {
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
	// 备件领用记录：编号唯一且为 P 加补零正整数序号，数量为正，归属存在的工单，
	// 资产与工单归属一致；下一领用序号必须为正并大于全部已用序号（允许有间隔）。
	issues := map[string]*PartIssue{}
	maxIssueSeq := 0
	for _, pi := range d.PartIssues {
		if pi == nil || pi.ID == "" || pi.TicketID == "" || pi.AssetID == "" || pi.PartNo == "" || pi.Note == "" {
			return errors.New("数据矛盾：存在字段不完整的备件领用记录")
		}
		if pi.Qty < 1 {
			return fmt.Errorf("数据矛盾：领用记录 %s 的领用数量须为正整数", pi.ID)
		}
		n, ok := parseIssueSeq(pi.ID)
		if !ok {
			return fmt.Errorf("计数器矛盾：领用编号 %q 不是 P 加补零正整数序号的形式", pi.ID)
		}
		if n > maxIssueSeq {
			maxIssueSeq = n
		}
		if issues[pi.ID] != nil {
			return fmt.Errorf("数据矛盾：领用编号 %s 重复", pi.ID)
		}
		t := tickets[pi.TicketID]
		if t == nil {
			return fmt.Errorf("数据矛盾：领用记录 %s 引用了不存在的工单 %s", pi.ID, pi.TicketID)
		}
		if pi.AssetID != t.AssetID {
			return fmt.Errorf("数据矛盾：领用记录 %s 的资产 %s 与工单 %s 归属的资产 %s 不一致",
				pi.ID, pi.AssetID, t.ID, t.AssetID)
		}
		issues[pi.ID] = pi
	}
	if d.NextIssueSeq < 1 || d.NextIssueSeq <= maxIssueSeq {
		return fmt.Errorf("计数器矛盾：下一领用序号 %d 必须为正并大于全部已用序号（当前最大 %d）",
			d.NextIssueSeq, maxIssueSeq)
	}
	// 状态：每项资产最多一张未关闭工单，有则“维修中”，无则“可用”。
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
		want := statusAvailable
		if c == 1 {
			want = statusRepairing
		}
		if a.Status != want {
			return fmt.Errorf("状态矛盾：资产 %s 有 %d 张未关闭工单，状态应为 %s，实际为 %s",
				a.ID, c, want, a.Status)
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
	issueEventCount := map[string]int{}
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
		if e.Kind == eventPlanCreate || e.Kind == eventPlanDone {
			// 保养履历：不属于任何工单，不携带派工字段；日期与间隔在此核对，
			// 建立及完成链的接续在下方按序号重放时核对。
			if e.TicketID != "" || e.From != "" || e.To != "" {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有工单编号或派工人员字段", e.Seq, e.Kind)
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
		if e.Kind == eventPartIssue || e.Kind == eventPartReturn {
			// 备件履历：属于工单与一笔领用记录，不携带派工或保养字段；
			// 数量为正，备件、工单与资产归属须与领用记录一致。
			if e.TicketID == "" || e.IssueID == "" || e.PartNo == "" {
				return errors.New("履历矛盾：存在字段不完整的备件履历记录")
			}
			if e.Qty < 1 {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录数量须为正整数", e.Seq, e.Kind)
			}
			if e.From != "" || e.To != "" || e.Due != "" || e.Done != "" || e.Interval != 0 {
				return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有派工或保养字段", e.Seq, e.Kind)
			}
			t := tickets[e.TicketID]
			if t == nil || assets[e.AssetID] == nil {
				return fmt.Errorf("履历矛盾：履历序号 %d 引用了不存在的资产或工单", e.Seq)
			}
			if e.AssetID != t.AssetID {
				return fmt.Errorf("履历矛盾：履历序号 %d 的资产 %s 与工单 %s 归属的资产 %s 不一致",
					e.Seq, e.AssetID, t.ID, t.AssetID)
			}
			pi := issues[e.IssueID]
			if pi == nil {
				return fmt.Errorf("履历矛盾：履历序号 %d 引用了不存在的领用记录 %s", e.Seq, e.IssueID)
			}
			if pi.TicketID != e.TicketID || pi.AssetID != e.AssetID || pi.PartNo != e.PartNo {
				return fmt.Errorf("履历矛盾：履历序号 %d 的工单、资产或备件与领用记录 %s 不一致",
					e.Seq, e.IssueID)
			}
			if e.Kind == eventPartIssue {
				if e.Qty != pi.Qty || e.Content != pi.Note {
					return fmt.Errorf("履历矛盾：领用记录 %s 的领用履历数量或说明与记录不一致", e.IssueID)
				}
				issueEventCount[e.IssueID]++
			}
			continue
		}
		if e.IssueID != "" || e.PartNo != "" || e.Qty != 0 {
			return fmt.Errorf("履历矛盾：履历序号 %d 的%s记录不应带有备件字段", e.Seq, e.Kind)
		}
		if e.TicketID == "" {
			return errors.New("履历矛盾：存在字段不完整的履历记录")
		}
		if e.Due != "" || e.Done != "" || e.Interval != 0 {
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
	for _, pi := range d.PartIssues {
		if issueEventCount[pi.ID] != 1 {
			return fmt.Errorf("履历矛盾：领用记录 %s 应有恰一条领用履历，实际 %d 条",
				pi.ID, issueEventCount[pi.ID])
		}
	}
	// 按履历序号推进，重放得到的工单与资产状态须与保存的状态相符；
	// 同一资产的上一张工单必须先关闭或取消才可产生下一张报修。
	sorted := make([]Event, len(d.Events))
	copy(sorted, d.Events)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	derivedTicket := map[string]string{}
	derivedOpen := map[string]string{}
	derivedAssignee := map[string]string{}
	derivedNote := map[string]string{}
	derivedIssued := map[string]bool{}
	derivedReturned := map[string]int{}
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
			derivedTicket[e.TicketID] = ticketOpen
			derivedOpen[e.AssetID] = e.TicketID
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
		case eventPartIssue:
			// 领用须在报修之后、终结之前（即工单处于未关闭状态）。
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现备件领用履历", e.TicketID)
			}
			if derivedIssued[e.IssueID] {
				return fmt.Errorf("履历矛盾：领用编号 %s 出现多条领用履历", e.IssueID)
			}
			derivedIssued[e.IssueID] = true
		case eventPartReturn:
			// 退回须在报修之后、终结之前，且指向先前发生的领用，
			// 累计退回不得超过该笔原数量。
			if derivedTicket[e.TicketID] != ticketOpen {
				return fmt.Errorf("履历矛盾：工单 %s 在未处于未关闭状态时出现备件退回履历", e.TicketID)
			}
			if !derivedIssued[e.IssueID] {
				return fmt.Errorf("履历矛盾：退回履历（序号 %d）指向尚未发生的领用 %s", e.Seq, e.IssueID)
			}
			derivedReturned[e.IssueID] += e.Qty
			if derivedReturned[e.IssueID] > issues[e.IssueID].Qty {
				return fmt.Errorf("履历矛盾：领用编号 %s 的累计退回数量超过原数量 %d",
					e.IssueID, issues[e.IssueID].Qty)
			}
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
	for _, a := range d.Assets {
		want := statusAvailable
		if derivedOpen[a.ID] != "" {
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
	// 保养履历链：每个计划恰有一条建立履历（内容、首次到期日、间隔与计划一致），
	// 完成履历按序号接续（周期到期日须等于当前下一到期日，完成日不早于它），
	// 由履历推出的下一到期日须与计划保存的一致。没有计划的资产不得有保养履历。
	maintCreate := map[string]int{}
	derivedDue := map[string]string{}
	for _, e := range sorted {
		if e.Kind != eventPlanCreate && e.Kind != eventPlanDone {
			continue
		}
		p := plans[e.AssetID]
		if p == nil {
			return fmt.Errorf("履历矛盾：资产 %s 没有保养计划，却存在保养履历（序号 %d）", e.AssetID, e.Seq)
		}
		if e.Kind == eventPlanCreate {
			if maintCreate[e.AssetID] != 0 {
				return fmt.Errorf("履历矛盾：资产 %s 有多条保养建立履历", e.AssetID)
			}
			maintCreate[e.AssetID] = e.Seq
			if e.Content != p.Content || e.Due != p.FirstDue || e.Interval != p.IntervalDays {
				return fmt.Errorf("履历矛盾：资产 %s 的保养建立履历与计划内容、首次到期日或间隔不一致", e.AssetID)
			}
			derivedDue[e.AssetID] = e.Due
			continue
		}
		if maintCreate[e.AssetID] == 0 {
			return fmt.Errorf("履历矛盾：资产 %s 的保养完成履历（序号 %d）出现在建立履历之前", e.AssetID, e.Seq)
		}
		if e.Due != derivedDue[e.AssetID] {
			return fmt.Errorf("履历矛盾：资产 %s 的保养完成履历（序号 %d）周期到期日 %s 与当前下一到期日 %s 不接续",
				e.AssetID, e.Seq, e.Due, derivedDue[e.AssetID])
		}
		first, _ := parseDate(p.FirstDue)
		done, _ := parseDate(e.Done)
		next, ok := nextDueAfter(first, int64(p.IntervalDays), done)
		if !ok {
			return fmt.Errorf("履历矛盾：资产 %s 的保养完成履历（序号 %d）无法推出日期范围内的下一到期日",
				e.AssetID, e.Seq)
		}
		derivedDue[e.AssetID] = formatDate(next)
	}
	for _, p := range d.Plans {
		if maintCreate[p.AssetID] == 0 {
			return fmt.Errorf("履历矛盾：资产 %s 的保养计划应有恰一条建立履历，实际没有", p.AssetID)
		}
		if derivedDue[p.AssetID] != p.NextDue {
			return fmt.Errorf("履历矛盾：按履历推出资产 %s 的下一到期日为 %s，与保存的 %s 不符",
				p.AssetID, derivedDue[p.AssetID], p.NextDue)
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
