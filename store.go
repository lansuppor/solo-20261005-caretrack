package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	statusAvailable = "可用"
	statusRepairing = "维修中"

	storeFileName = "caretrack.json"
)

// Asset 设备资产。
type Asset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Status   string `json:"status"`
}

// Ticket 维修工单。Closed 为 false 表示未关闭。
type Ticket struct {
	ID          string     `json:"id"`
	AssetID     string     `json:"assetId"`
	Description string     `json:"description"`
	Result      string     `json:"result,omitempty"`
	Closed      bool       `json:"closed"`
	ReportedAt  time.Time  `json:"reportedAt"`
	ClosedAt    *time.Time `json:"closedAt,omitempty"`
}

// Event 履历事件，按发生顺序追加。
type Event struct {
	Seq      int       `json:"seq"`
	Type     string    `json:"type"` // "报修" 或 "关闭"
	TicketID string    `json:"ticketId"`
	AssetID  string    `json:"assetId"`
	Time     time.Time `json:"time"`
	Detail   string    `json:"detail"` // 报修为故障描述，关闭为维修结果
}

// RequestBinding 报修请求标识去重绑定。
type RequestBinding struct {
	RequestID   string `json:"requestId"`
	AssetID     string `json:"assetId"`
	Description string `json:"description"`
	TicketID    string `json:"ticketId"`
}

// Store 单个数据目录内的全部业务数据，一次写操作共同生效。
type Store struct {
	Assets     map[string]*Asset          `json:"assets"`
	Tickets    map[string]*Ticket         `json:"tickets"`
	Events     []*Event                   `json:"events"`
	Requests   map[string]*RequestBinding `json:"requests"`
	NextTicket int                        `json:"nextTicket"`
	nextEvent  int
}

func newStore() *Store {
	return &Store{
		Assets:     map[string]*Asset{},
		Tickets:    map[string]*Ticket{},
		Requests:   map[string]*RequestBinding{},
		NextTicket: 1,
		nextEvent:  1,
	}
}

// loadStore 从目录读取数据；目录或文件不存在时初始化为空库。
// 已有数据损坏时报错并保留原文件。
func loadStore(dir string) (*Store, error) {
	path := filepath.Join(dir, storeFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newStore(), nil
		}
		return nil, fmt.Errorf("读取数据文件失败: %w", err)
	}
	var s Store
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("数据文件 %s 已损坏，未做改动: %w", path, err)
	}
	if s.Assets == nil {
		s.Assets = map[string]*Asset{}
	}
	if s.Tickets == nil {
		s.Tickets = map[string]*Ticket{}
	}
	if s.Requests == nil {
		s.Requests = map[string]*RequestBinding{}
	}
	if s.NextTicket < 1 {
		s.NextTicket = 1
	}
	maxSeq := 0
	for _, e := range s.Events {
		if e != nil && e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	s.nextEvent = maxSeq + 1
	return &s, nil
}

// save 将数据原子写入目录：先写临时文件再重命名，避免半截文件。
func (s *Store) save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化数据失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".caretrack-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("写入数据失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("写入数据失败: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, storeFileName)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("保存数据失败: %w", err)
	}
	return nil
}

func (s *Store) newTicketID() string {
	id := fmt.Sprintf("T%06d", s.NextTicket)
	s.NextTicket++
	return id
}

func (s *Store) appendEvent(typ, ticketID, assetID, detail string, t time.Time) {
	s.Events = append(s.Events, &Event{
		Seq:      s.nextEvent,
		Type:     typ,
		TicketID: ticketID,
		AssetID:  assetID,
		Time:     t,
		Detail:   detail,
	})
	s.nextEvent++
}

// openTicket 返回资产当前未关闭的工单，没有则为 nil。
func (s *Store) openTicket(assetID string) *Ticket {
	for _, t := range s.Tickets {
		if t.AssetID == assetID && !t.Closed {
			return t
		}
	}
	return nil
}
