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
	if err := validateConsistency(&d); err != nil {
		return nil, fmt.Errorf("数据文件 %s 内容矛盾：%w（原文件已保留，未做任何修改）", path, err)
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
		if t == nil || t.ID == "" || t.AssetID == "" || t.Description == "" || t.RequestID == "" || t.CreatedAt == "" {
			return errors.New("存在字段不完整的工单记录")
		}
		if t.Status != ticketOpen && t.Status != ticketClosed {
			return fmt.Errorf("工单 %s 状态无效", t.ID)
		}
		if !seenAssets[t.AssetID] {
			return fmt.Errorf("工单 %s 引用了不存在的资产", t.ID)
		}
		if _, err := time.Parse(time.RFC3339, t.CreatedAt); err != nil {
			return fmt.Errorf("工单 %s 的创建时间格式无效", t.ID)
		}
		if t.Status == ticketClosed {
			if t.Result == "" || t.ClosedAt == "" {
				return fmt.Errorf("工单 %s 缺少关闭信息", t.ID)
			}
			if _, err := time.Parse(time.RFC3339, t.ClosedAt); err != nil {
				return fmt.Errorf("工单 %s 的关闭时间格式无效", t.ID)
			}
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

// formatTicketID 生成工单编号：T 加补零正整数序号（至少 4 位）。
func formatTicketID(seq int) string {
	return fmt.Sprintf("T%04d", seq)
}

// parseTicketSeq 解析工单编号，仅接受 formatTicketID 的规范形式。
func parseTicketSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "T") {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil || n < 1 || formatTicketID(n) != id {
		return 0, false
	}
	return n, true
}

// validateConsistency 校验整份业务数据的内部一致性，发现矛盾时返回以
// “计数器矛盾 / 请求绑定矛盾 / 状态矛盾 / 履历矛盾”开头的错误。
// 调用前须先通过 validateData 的结构校验；本函数只报告矛盾，不修改任何数据。
func validateConsistency(d *storeData) error {
	ticketsByID := map[string]*Ticket{}
	maxUsed := 0
	for _, t := range d.Tickets {
		n, ok := parseTicketSeq(t.ID)
		if !ok {
			return fmt.Errorf("计数器矛盾：工单编号 %q 不是 T 加补零正整数序号的形式", t.ID)
		}
		ticketsByID[t.ID] = t
		if n > maxUsed {
			maxUsed = n
		}
	}
	if d.NextTicketSeq < 1 || d.NextTicketSeq <= maxUsed {
		return fmt.Errorf("计数器矛盾：下一工单序号 %d 必须为正并大于全部已用序号（当前最大已用 %d）",
			d.NextTicketSeq, maxUsed)
	}

	// 每张工单恰有一条请求绑定，且绑定内容与工单完全一致。
	bindingsOf := map[string]int{}
	for _, r := range d.Requests {
		t := ticketsByID[r.TicketID]
		if t == nil {
			continue // 结构校验已报告
		}
		bindingsOf[r.TicketID]++
		if r.RequestID != t.RequestID || r.AssetID != t.AssetID || r.Description != t.Description {
			return fmt.Errorf("请求绑定矛盾：请求标识 %q 的绑定与工单 %s 的请求标识、资产编号或故障描述不一致",
				r.RequestID, t.ID)
		}
	}
	for _, t := range d.Tickets {
		switch bindingsOf[t.ID] {
		case 0:
			return fmt.Errorf("请求绑定矛盾：工单 %s 缺少对应的报修请求绑定", t.ID)
		case 1:
		default:
			return fmt.Errorf("请求绑定矛盾：工单 %s 对应多条报修请求绑定", t.ID)
		}
	}

	// 每项资产最多一张未关闭工单；有未关闭工单时必须“维修中”，否则必须“可用”。
	openByAsset := map[string]string{}
	for _, t := range d.Tickets {
		if t.Status != ticketOpen {
			continue
		}
		if t.Result != "" || t.ClosedAt != "" {
			return fmt.Errorf("状态矛盾：未关闭工单 %s 不应带有维修结果或关闭时间", t.ID)
		}
		if prev, ok := openByAsset[t.AssetID]; ok {
			return fmt.Errorf("状态矛盾：资产 %s 同时存在多张未关闭工单（%s、%s）", t.AssetID, prev, t.ID)
		}
		openByAsset[t.AssetID] = t.ID
	}
	for _, a := range d.Assets {
		if _, hasOpen := openByAsset[a.ID]; hasOpen {
			if a.Status != statusRepairing {
				return fmt.Errorf("状态矛盾：资产 %s 有未关闭工单，状态应为“维修中”而非 %q", a.ID, a.Status)
			}
		} else if a.Status != statusAvailable {
			return fmt.Errorf("状态矛盾：资产 %s 没有未关闭工单，状态应为“可用”而非 %q", a.ID, a.Status)
		}
	}

	// 履历：序号全库唯一且为正；按序号推进重放，校验与工单、资产状态相符。
	events := make([]eventJSON, len(d.EventsJSON))
	copy(events, d.EventsJSON)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	seenSeq := map[int]bool{}
	reportCount := map[string]int{}
	closeCount := map[string]int{}
	openByReplay := map[string]string{}
	for _, e := range events {
		if e.Seq < 1 {
			return fmt.Errorf("履历矛盾：履历序号 %d 不是正整数", e.Seq)
		}
		if seenSeq[e.Seq] {
			return fmt.Errorf("履历矛盾：履历序号 %d 重复", e.Seq)
		}
		seenSeq[e.Seq] = true
		t := ticketsByID[e.TicketID]
		if t == nil {
			continue // 结构校验已报告
		}
		if e.AssetID != t.AssetID {
			return fmt.Errorf("履历矛盾：序号 %d 履历的资产 %s 与工单 %s 所属资产 %s 不一致",
				e.Seq, e.AssetID, t.ID, t.AssetID)
		}
		switch e.Kind {
		case eventReport:
			reportCount[t.ID]++
			if e.Content != t.Description {
				return fmt.Errorf("履历矛盾：工单 %s 的报修履历内容与故障描述不一致", t.ID)
			}
			if prev, ok := openByReplay[t.AssetID]; ok {
				if prev == t.ID {
					return fmt.Errorf("履历矛盾：工单 %s 的报修履历重复", t.ID)
				}
				return fmt.Errorf("履历矛盾：资产 %s 的上一张工单 %s 尚未关闭，就产生了工单 %s 的报修",
					t.AssetID, prev, t.ID)
			}
			openByReplay[t.AssetID] = t.ID
		case eventClose:
			closeCount[t.ID]++
			if t.Status != ticketClosed {
				return fmt.Errorf("履历矛盾：未关闭工单 %s 不应有关闭履历", t.ID)
			}
			if e.Content != t.Result {
				return fmt.Errorf("履历矛盾：工单 %s 的关闭履历内容与维修结果不一致", t.ID)
			}
			if openByReplay[t.AssetID] != t.ID {
				return fmt.Errorf("履历矛盾：工单 %s 的关闭履历出现在其报修之前", t.ID)
			}
			delete(openByReplay, t.AssetID)
		}
	}
	for _, t := range d.Tickets {
		if reportCount[t.ID] != 1 {
			return fmt.Errorf("履历矛盾：工单 %s 应恰有一条报修履历，实际 %d 条", t.ID, reportCount[t.ID])
		}
		if t.Status == ticketClosed && closeCount[t.ID] != 1 {
			return fmt.Errorf("履历矛盾：已关闭工单 %s 应恰有一条关闭履历，实际 %d 条", t.ID, closeCount[t.ID])
		}
	}
	// 按履历推进得到的未关闭工单须与保存的工单、资产状态相符。
	for assetID, tid := range openByReplay {
		if openByAsset[assetID] != tid {
			return fmt.Errorf("状态矛盾：按履历推进资产 %s 的未关闭工单为 %s，与保存的状态不符", assetID, tid)
		}
	}
	for assetID, tid := range openByAsset {
		if openByReplay[assetID] != tid {
			return fmt.Errorf("状态矛盾：保存的未关闭工单 %s 与资产 %s 的履历推进结果不符", tid, assetID)
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
		return fmt.Errorf("待保存数据未通过校验（%w），本次写入已放弃，原数据文件保持不变", err)
	}
	if err := validateConsistency(s.data); err != nil {
		return fmt.Errorf("待保存数据未通过一致性校验（%w），本次写入已放弃，原数据文件保持不变", err)
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
	if seq >= math.MaxInt {
		return nil, false, fmt.Errorf("%w: 工单编号已耗尽，无法接受新报修（已有工单仍可查询和关闭）", errConflict)
	}
	s.data.NextTicketSeq++
	t := &Ticket{
		ID:          formatTicketID(seq),
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
