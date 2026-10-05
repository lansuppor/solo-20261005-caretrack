package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func cmdRegister(dir string, args []string) error {
	if len(args) != 3 {
		return usagef("register 需要 3 个参数: <资产编号> <名称> <位置>")
	}
	id, name, location := args[0], args[1], args[2]
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(location) == "" {
		return fmt.Errorf("资产编号、名称和位置均不能为空")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Assets[id]; ok {
		return fmt.Errorf("资产编号 %q 已存在", id)
	}
	s.Assets[id] = &Asset{ID: id, Name: name, Location: location, Status: statusAvailable}
	if err := s.save(dir); err != nil {
		return err
	}
	fmt.Printf("已登记资产 %s（%s，%s），状态：%s\n", id, name, location, statusAvailable)
	return nil
}

func cmdList(dir string, args []string) error {
	if len(args) != 0 {
		return usagef("list 不接受参数")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if len(s.Assets) == 0 {
		fmt.Println("没有记录")
		return nil
	}
	ids := make([]string, 0, len(s.Assets))
	for id := range s.Assets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := s.Assets[id]
		fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Location, a.Status)
	}
	return nil
}

func cmdShow(dir string, args []string) error {
	if len(args) != 1 {
		return usagef("show 需要 1 个参数: <资产编号>")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	a, ok := s.Assets[args[0]]
	if !ok {
		return fmt.Errorf("未知资产 %q", args[0])
	}
	fmt.Printf("资产编号: %s\n名称: %s\n位置: %s\n状态: %s\n", a.ID, a.Name, a.Location, a.Status)
	if t := s.openTicket(a.ID); t != nil {
		fmt.Printf("未关闭工单: %s\n", t.ID)
	} else {
		fmt.Println("未关闭工单: 无")
	}
	return nil
}

func cmdRepair(dir string, args []string) error {
	if len(args) != 3 {
		return usagef("repair 需要 3 个参数: <资产编号> <故障描述> <请求标识>")
	}
	assetID, desc, reqID := args[0], args[1], args[2]
	if strings.TrimSpace(desc) == "" {
		return fmt.Errorf("故障描述不能为空")
	}
	if strings.TrimSpace(reqID) == "" {
		return fmt.Errorf("请求标识不能为空")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	// 请求标识去重：相同标识+资产+描述的重放返回原工单及其当前状态，
	// 即使原工单已关闭或该资产已有新工单，也不重开旧单、不影响新单。
	if b, ok := s.Requests[reqID]; ok {
		if b.AssetID != assetID || b.Description != desc {
			return fmt.Errorf("请求标识 %q 已用于其他报修（资产或描述不一致）", reqID)
		}
		t := s.Tickets[b.TicketID]
		status := "未关闭"
		if t.Closed {
			status = "已关闭"
		}
		fmt.Printf("重复请求，返回原工单 %s（%s）\n", t.ID, status)
		return nil
	}
	if _, ok := s.Assets[assetID]; !ok {
		return fmt.Errorf("未知资产 %q", assetID)
	}
	if t := s.openTicket(assetID); t != nil {
		return fmt.Errorf("资产 %q 已有未关闭工单 %s，不能再报修", assetID, t.ID)
	}
	now := time.Now()
	t := &Ticket{
		ID:          s.newTicketID(),
		AssetID:     assetID,
		Description: desc,
		ReportedAt:  now,
	}
	s.Tickets[t.ID] = t
	s.Assets[assetID].Status = statusRepairing
	s.Requests[reqID] = &RequestBinding{RequestID: reqID, AssetID: assetID, Description: desc, TicketID: t.ID}
	s.appendEvent("报修", t.ID, assetID, desc, now)
	if err := s.save(dir); err != nil {
		return err
	}
	fmt.Printf("报修成功，工单编号: %s，资产 %s 状态：%s\n", t.ID, assetID, statusRepairing)
	return nil
}

func cmdClose(dir string, args []string) error {
	if len(args) != 2 {
		return usagef("close 需要 2 个参数: <工单编号> <维修结果>")
	}
	ticketID, result := args[0], args[1]
	if strings.TrimSpace(result) == "" {
		return fmt.Errorf("维修结果不能为空")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	t, ok := s.Tickets[ticketID]
	if !ok {
		return fmt.Errorf("未知工单 %q", ticketID)
	}
	if t.Closed {
		return fmt.Errorf("工单 %s 已关闭，不能重复关闭", ticketID)
	}
	now := time.Now()
	t.Closed = true
	t.Result = result
	t.ClosedAt = &now
	s.Assets[t.AssetID].Status = statusAvailable
	s.appendEvent("关闭", t.ID, t.AssetID, result, now)
	if err := s.save(dir); err != nil {
		return err
	}
	fmt.Printf("工单 %s 已关闭，资产 %s 状态：%s\n", t.ID, t.AssetID, statusAvailable)
	return nil
}

func cmdHistory(dir string, args []string) error {
	if len(args) != 1 {
		return usagef("history 需要 1 个参数: <资产编号>")
	}
	s, err := loadStore(dir)
	if err != nil {
		return err
	}
	if _, ok := s.Assets[args[0]]; !ok {
		return fmt.Errorf("未知资产 %q", args[0])
	}
	var events []*Event
	for _, e := range s.Events {
		if e.AssetID == args[0] {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		fmt.Println("没有记录")
		return nil
	}
	for _, e := range events {
		fmt.Printf("%s 工单 %s %s: %s\n", e.Time.Format("2006-01-02 15:04:05"), e.TicketID, e.Type, e.Detail)
	}
	return nil
}
