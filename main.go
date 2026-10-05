package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const appName = "caretrack"

const helpText = `caretrack — 本地设备资产登记与维修工单闭环工具

数据保存在本地目录，无外部服务依赖，仅支持单进程顺序调用。

用法:
  caretrack <命令> [参数]

命令:
  register   登记资产（企业资产编号、名称、位置）
  list       列出全部资产及当前状态
  detail     查看资产详情及其未关闭工单编号
  report     对资产报修，创建工单，资产转为“维修中”
  close      关闭工单并填写维修结果，设备恢复“可用”
  history    按资产查看维修履历（报修、关闭事件）
  help       显示本帮助

各命令参数:
  register --asset-id 编号 --name 名称 --location 位置 [--data-dir 目录]
  list                                                        [--data-dir 目录]
  detail   --asset-id 编号                                    [--data-dir 目录]
  report   --asset-id 编号 --description 故障描述 --request-id 请求标识
                                                              [--data-dir 目录]
  close    --ticket-id 工单编号 --repair-result 维修结果       [--data-dir 目录]
  history  --asset-id 编号                                    [--data-dir 目录]

通用参数:
  --data-dir 目录   本地数据目录，默认 ".caretrack"；不同目录数据互不影响，
                    目录不存在时会在首次成功写入时自动初始化

业务规则:
  - 资产编号、名称、位置均不能为空；同一数据目录内资产编号唯一，新资产为“可用”。
  - 每项资产最多有一张未关闭工单；未知资产、空故障描述、资产已有未关闭工单时
    报修失败，不改变状态与履历。
  - --request-id 由调用方提供且非空，仅用于同一数据目录内的报修去重，不作为
    资产或工单编号：相同标识、资产、描述再次提交返回原工单及当前状态，不新增
    记录；同一标识搭配不同资产或描述将被拒绝；原工单关闭后重放仍返回原工单，
    不重开旧单，也不影响新单。
  - 仅未关闭工单可关闭，维修结果不能为空；未知工单、重复关闭均失败。

无参数、--help、-h 显示本帮助；参数错误或业务失败以非零退出码结束并说明原因。
`

type cmdOptions struct {
	dataDir string
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, helpText)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return 0
	}
	switch args[0] {
	case "--help", "-h", "help":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "%s: help 不接受额外参数\n", appName)
			return 2
		}
		printHelp(stdout)
		return 0
	}

	var err error
	switch args[0] {
	case "register":
		err = cmdRegister(args[1:], stdout)
	case "list":
		err = cmdList(args[1:], stdout)
	case "detail":
		err = cmdDetail(args[1:], stdout)
	case "report":
		err = cmdReport(args[1:], stdout)
	case "close":
		err = cmdClose(args[1:], stdout)
	case "history":
		err = cmdHistory(args[1:], stdout)
	default:
		fmt.Fprintf(stderr, "%s: 未知命令 %q，使用 --help 查看帮助\n", appName, args[0])
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "%s: %s\n", appName, ue.Error())
			return 2
		}
		fmt.Fprintf(stderr, "%s: %s\n", appName, friendlyErr(err.Error()))
		return 1
	}
	return 0
}

// friendlyErr 去掉内部哨兵错误（errNotFound/errConflict）的英文前缀。
func friendlyErr(msg string) string {
	for _, p := range []string{"not found: ", "conflict: "} {
		if strings.HasPrefix(msg, p) {
			return msg[len(p):]
		}
	}
	return msg
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// newFlagSet 为子命令准备参数解析器：--help/-h 输出帮助并返回 flag.ErrHelp，
// 其余参数错误转成 *usageError（退出码 2）。
func newFlagSet(name string, opts *cmdOptions) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() {
		fmt.Fprintf(os.Stdout, "用法: caretrack %s [参数]\n\n", name)
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.dataDir, "data-dir", ".caretrack", "本地数据目录")
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &usageError{msg: err.Error()}
	}
	if fs.NArg() > 0 {
		return &usageError{msg: fmt.Sprintf("命令 %s 不接受位置参数: %v", fs.Name(), fs.Args())}
	}
	return nil
}

func requireFlag(fs *flag.FlagSet, value, flagName string) error {
	if value == "" {
		return &usageError{msg: fmt.Sprintf("命令 %s 缺少必填参数 --%s", fs.Name(), flagName)}
	}
	return nil
}

func cmdRegister(args []string, w io.Writer) error {
	var opts cmdOptions
	var id, name, location string
	fs := newFlagSet("register", &opts)
	fs.StringVar(&id, "asset-id", "", "企业资产编号（必填，同一数据目录内唯一）")
	fs.StringVar(&name, "name", "", "资产名称（必填）")
	fs.StringVar(&location, "location", "", "资产位置（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, id, "asset-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, name, "name"); err != nil {
		return err
	}
	if err := requireFlag(fs, location, "location"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	asset, err := s.registerAsset(id, name, location)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "已登记资产 %s\n", asset.ID)
	fmt.Fprintf(w, "名称: %s\n", asset.Name)
	fmt.Fprintf(w, "位置: %s\n", asset.Location)
	fmt.Fprintf(w, "当前状态: %s\n", asset.Status)
	return nil
}

func cmdList(args []string, w io.Writer) error {
	var opts cmdOptions
	fs := newFlagSet("list", &opts)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	assets := s.data.Assets
	if len(assets) == 0 {
		fmt.Fprintln(w, "没有记录。")
		return nil
	}
	fmt.Fprintf(w, "共 %d 项资产:\n", len(assets))
	for _, a := range assets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Location, a.Status)
	}
	return nil
}

func cmdDetail(args []string, w io.Writer) error {
	var opts cmdOptions
	var id string
	fs := newFlagSet("detail", &opts)
	fs.StringVar(&id, "asset-id", "", "企业资产编号（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, id, "asset-id"); err != nil {
		return err
	}
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	asset := s.findAsset(id)
	if asset == nil {
		return fmt.Errorf("未知资产编号 %q", id)
	}
	openTicket := s.openTicketOf(asset.ID)
	fmt.Fprintf(w, "资产编号: %s\n", asset.ID)
	fmt.Fprintf(w, "名称: %s\n", asset.Name)
	fmt.Fprintf(w, "位置: %s\n", asset.Location)
	fmt.Fprintf(w, "当前状态: %s\n", asset.Status)
	if openTicket == nil {
		fmt.Fprintln(w, "未关闭工单: 无")
	} else {
		fmt.Fprintf(w, "未关闭工单: %s\n", openTicket.ID)
	}
	return nil
}

func cmdReport(args []string, w io.Writer) error {
	var opts cmdOptions
	var assetID, description, requestID string
	fs := newFlagSet("report", &opts)
	fs.StringVar(&assetID, "asset-id", "", "企业资产编号（必填）")
	fs.StringVar(&description, "description", "", "故障描述（必填）")
	fs.StringVar(&requestID, "request-id", "", "调用方提供的非空请求标识，用于报修去重（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, assetID, "asset-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, description, "description"); err != nil {
		return err
	}
	if err := requireFlag(fs, requestID, "request-id"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	ticket, replayed, err := s.report(assetID, description, requestID)
	if err != nil {
		return err
	}
	if !replayed {
		if err := s.save(); err != nil {
			return err
		}
		fmt.Fprintf(w, "工单编号: %s\n", ticket.ID)
	} else {
		fmt.Fprintf(w, "该请求标识已提交过，返回原工单，未重复创建。\n")
		fmt.Fprintf(w, "工单编号: %s（既有工单）\n", ticket.ID)
	}
	fmt.Fprintf(w, "资产编号: %s\n", ticket.AssetID)
	fmt.Fprintf(w, "工单状态: %s\n", ticket.Status)
	return nil
}

func cmdClose(args []string, w io.Writer) error {
	var opts cmdOptions
	var ticketID, result string
	fs := newFlagSet("close", &opts)
	fs.StringVar(&ticketID, "ticket-id", "", "要关闭的工单编号（必填）")
	fs.StringVar(&result, "repair-result", "", "维修结果（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, ticketID, "ticket-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, result, "repair-result"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	ticket, asset, err := s.closeTicket(ticketID, result)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "工单 %s 已关闭。\n", ticket.ID)
	fmt.Fprintf(w, "资产 %s 当前状态: %s\n", asset.ID, asset.Status)
	return nil
}

func cmdHistory(args []string, w io.Writer) error {
	var opts cmdOptions
	var id string
	fs := newFlagSet("history", &opts)
	fs.StringVar(&id, "asset-id", "", "企业资产编号（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, id, "asset-id"); err != nil {
		return err
	}
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	if s.findAsset(id) == nil {
		return fmt.Errorf("未知资产编号 %q", id)
	}
	events := s.eventsOf(id)
	if len(events) == 0 {
		fmt.Fprintf(w, "资产 %s 暂无维修履历。\n", id)
		return nil
	}
	fmt.Fprintf(w, "资产 %s 维修履历（共 %d 条）:\n", id, len(events))
	for _, e := range events {
		fmt.Fprintf(w, "[%s] %s 工单 %s: %s\n",
			e.Time.Format("2006-01-02T15:04:05Z07:00"), e.Kind, e.TicketID, e.Content)
	}
	return nil
}
