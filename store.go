package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 资产状态、工单状态与履历类型。
const (
	statusAvailable = "可用"
	statusRepairing = "维修中"
	ticketOpen      = "未关闭"
	ticketClosed    = "已关闭"

	eventReport = "报修"
	eventClose  = "关闭"

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

// Ticket 为维修工单。
type Ticket struct {
	ID          string `json:"id"`
	AssetID     string `json:"asset_id"`
	Description string `json:"description"`
	RequestID   string `json:"request_id"`
	Status      string `json:"status"`
	Result      string `json:"result,omitempty"`
	CreatedAt   string `json:"created_at"`
	ClosedAt    string `json:"closed_at,omitempty"`
}

// Event 为履历条目（报修/关闭），Seq 决定操作发生顺序。
type Event struct {
	Seq      int       `json:"-"`
	AssetID  string    `json:"-"`
	TicketID string    `json:"-"`
	Kind     string    `json:"-"`
	Content  string    `json:"-"`
	Time     time.Time `json:"-"`
}

// eventJSON 与 Event 对应，时间以 RFC3339 文本持久化。
type eventJSON struct {
	Seq      int    `json:"seq"`
	AssetID  string `json:"asset_id"`
	TicketID string `json:"ticket_id"`
	Kind     string `json:"kind"`
	Content  string `json:"content"`
	Time     string `json:"time"`
}

func (e Event) toJSON() eventJSON {
	return eventJSON{
		Seq: e.Seq, AssetID: e.AssetID, TicketID: e.TicketID,
		Kind: e.Kind, Content: e.Content, Time: e.Time.Format(time.RFC3339),
	}
}

func (e eventJSON) toEvent() (Event, error) {
	t, err := time.Parse(time.RFC3339, e.Time)
	if err != nil {
		return Event{}, fmt.Errorf("履历时间格式无效 %q: %w", e.Time, err)
	}
	return Event{
		Seq: e.Seq, AssetID: e.AssetID, TicketID: e.TicketID,
		Kind: e.Kind, Content: e.Content, Time: t,
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
	NextTicketSeq int              `json:"next_ticket_seq"`
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
		NextTicketSeq: 1,
	}
}

// openStore 打开数据目录：目录/文件不存在时视为空库（首次保存时初始化）；
// 已有文件为空或无法解析时报告错误并保留原文件，绝不当空库覆盖。
func openStore(dir string) (*store, error) {
	path := filepath.Join(dir, dataFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &store{dir: dir, data: newStoreData(), now: time.Now}, nil
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
	if err := validateData(&d); err != nil {
		return nil, fmt.Errorf("数据文件 %s 已损坏：%w（原文件已保留，未做任何修改）", path, err)
	}
	d.Events = make([]Event, len(d.EventsJSON))
	for i, ej := range d.EventsJSON {
		ev, err := ej.toEvent()
		if err != nil {
			return nil, fmt.Errorf("数据文件 %s 已损坏：%w（原文件已保留，未做任何修改）", path, err)
		}
		d.Events[i] = ev
	}
	return &store{dir: dir, data: &d, now: time.Now}, nil
}

func validateData(d *storeData) error {
	if d.Version != storeVersion {
		return fmt.Errorf("不支持的数据版本 %d", d.Version)
	}
	if d.Assets == nil || d.Tickets == nil || d.Requests == nil || d.EventsJSON == nil {
		return errors.New("缺少必要的数据段")
	}
	seenAssets := map[string]bool{}
	for _, a := range d.Assets {
		if a == nil || a.ID == "" || a.Name == "" || a.Location == "" {
			return errors.New("存在字段不完整的资产记录")
		}
		if a.Status != statusAvailable && a.Status != statusRepairing {
			return fmt.Errorf("资产 %s 状态无效", a.ID)
		}
		if seenAssets[a.ID] {
			return fmt.Errorf("资产编号 %s 重复", a.ID)
		}
		seenAssets[a.ID] = true
	}
	seenTickets := map[string]bool{}
	for _, t := range d.Tickets {
		if t == nil || t.ID == "" || t.AssetID == "" || t.RequestID == "" || t.CreatedAt == "" {
			return errors.New("存在字段不完整的工单记录")
		}
		if t.Status != ticketOpen && t.Status != ticketClosed {
			return fmt.Errorf("工单 %s 状态无效", t.ID)
		}
		if !seenAssets[t.AssetID] {
			return fmt.Errorf("工单 %s 引用了不存在的资产", t.ID)
		}
		if t.Status == ticketClosed && (t.Result == "" || t.ClosedAt == "") {
			return fmt.Errorf("工单 %s 缺少关闭信息", t.ID)
		}
		if seenTickets[t.ID] {
			return fmt.Errorf("工单编号 %s 重复", t.ID)
		}
		seenTickets[t.ID] = true
	}
	seenReq := map[string]bool{}
	for _, r := range d.Requests {
		if r.RequestID == "" || r.AssetID == "" || r.Description == "" || r.TicketID == "" {
			return errors.New("存在字段不完整的请求去重记录")
		}
		if !seenAssets[r.AssetID] || !seenTickets[r.TicketID] {
			return fmt.Errorf("请求标识 %s 的绑定记录不完整", r.RequestID)
		}
		if seenReq[r.RequestID] {
			return fmt.Errorf("请求标识 %s 重复", r.RequestID)
		}
		seenReq[r.RequestID] = true
	}
	for _, ej := range d.EventsJSON {
		if ej.AssetID == "" || ej.TicketID == "" || ej.Kind == "" || ej.Time == "" {
			return errors.New("存在字段不完整的履历记录")
		}
		if !seenAssets[ej.AssetID] || !seenTickets[ej.TicketID] {
			return errors.New("履历记录引用了不存在的资产或工单")
		}
		if ej.Kind != eventReport && ej.Kind != eventClose {
			return fmt.Errorf("履历事件类型无效: %q", ej.Kind)
		}
	}
	return nil
}

// save 将全部业务数据一次性原子写入：先写同目录临时文件，fsync 后 rename
// 覆盖正式文件。业务校验均在调用 save 之前完成；写入失败时原文件保持不变。
func (s *store) save() error {
	s.data.EventsJSON = make([]eventJSON, len(s.data.Events))
	for i, e := range s.data.Events {
		s.data.EventsJSON[i] = e.toJSON()
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

func (s *store) nextEventSeq() int {
	maxSeq := 0
	for _, e := range s.data.Events {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	return maxSeq + 1
}

func (s *store) addEvent(assetID, ticketID, kind, content string) {
	s.data.Events = append(s.data.Events, Event{
		Seq:      s.nextEventSeq(),
		AssetID:  assetID,
		TicketID: ticketID,
		Kind:     kind,
		Content:  content,
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

	seq := s.data.NextTicketSeq
	s.data.NextTicketSeq++
	t := &Ticket{
		ID:          fmt.Sprintf("T%04d", seq),
		AssetID:     assetID,
		Description: description,
		RequestID:   requestID,
		Status:      ticketOpen,
		CreatedAt:   s.now().Format(time.RFC3339),
	}
	s.data.Tickets = append(s.data.Tickets, t)
	s.data.Requests = append(s.data.Requests, requestBinding{
		RequestID:   requestID,
		AssetID:     assetID,
		Description: description,
		TicketID:    t.ID,
	})
	asset.Status = statusRepairing
	s.addEvent(assetID, t.ID, eventReport, description)
	return t, false, nil
}

// closeTicket 关闭未关闭工单并把资产恢复为可用。
func (s *store) closeTicket(ticketID, result string) (*Ticket, *Asset, error) {
	t := s.findTicket(ticketID)
	if t == nil {
		return nil, nil, fmt.Errorf("%w: 未知工单编号 %q", errNotFound, ticketID)
	}
	if t.Status != ticketOpen {
		return nil, nil, fmt.Errorf("%w: 工单 %s 已关闭，不能重复关闭", errConflict, ticketID)
	}
	asset := s.findAsset(t.AssetID)
	if asset == nil {
		return nil, nil, fmt.Errorf("数据内部错误：工单 %s 引用的资产不存在", ticketID)
	}
	t.Status = ticketClosed
	t.Result = result
	t.ClosedAt = s.now().Format(time.RFC3339)
	asset.Status = statusAvailable
	s.addEvent(asset.ID, t.ID, eventClose, result)
	return t, asset, nil
}
