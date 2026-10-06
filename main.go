package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const appName = "caretrack"

const helpText = `caretrack — 本地设备资产登记与维修工单闭环工具

数据保存在本地目录，无外部服务依赖，仅支持单进程顺序调用。

用法:
  caretrack <命令> [参数]

命令:
  register   登记资产（企业资产编号、名称、位置）
  list       列出全部资产及当前状态
  detail     查看资产详情及其未关闭工单编号、保养计划
  report     对资产报修，创建工单，资产转为“维修中”
  assign     为未关闭工单派工或转派维修人员
  ticket     按工单编号查询工单状态、负责人及备件领用与净量
  close      关闭工单并填写维修结果，设备恢复“可用”
  cancel     取消误报或不再需要维修的未关闭工单，设备恢复“可用”
  issue      为未关闭工单领用备件，生成唯一领用编号
  return     按领用编号退回备件（允许多次部分退回，累计不超过原数量）
  plan       为资产建立唯一的周期保养计划（不可覆盖）
  maintain   登记当前周期的保养完成，推进下一到期日
  due        按指定日期列出到期的保养计划（只读）
  history    按资产查看履历（报修、派工、关闭、取消、保养建立与完成、备件领用与退回事件）
  downtime   查询时间窗口内的设备维修停机时长（单项或全部资产）
  import     从另一数据目录批量导入资产及其工单、履历、请求绑定、保养计划与备件记录（复制，源只读）
  help       显示本帮助

各命令参数:
  register --asset-id 编号 --name 名称 --location 位置 [--data-dir 目录]
  list                                                        [--data-dir 目录]
  detail   --asset-id 编号                                    [--data-dir 目录]
  report   --asset-id 编号 --description 故障描述 --request-id 请求标识
                                                              [--data-dir 目录]
  assign   --ticket-id 工单编号 --assignee 维修人员 --note 说明  [--data-dir 目录]
  ticket   --ticket-id 工单编号                                 [--data-dir 目录]
  close    --ticket-id 工单编号 --repair-result 维修结果       [--data-dir 目录]
  cancel   --ticket-id 工单编号 --reason 取消理由              [--data-dir 目录]
  issue    --ticket-id 工单编号 --part-no 备件编号 --qty 数量 --note 说明
                                                              [--data-dir 目录]
  return   --issue-id 领用编号 --qty 数量 --reason 理由        [--data-dir 目录]
  plan     --asset-id 编号 --content 保养内容 --first-due 首次到期日
           --interval-days 间隔天数                           [--data-dir 目录]
  maintain --asset-id 编号 --due 周期到期日 --done 实际完成日 --result 结果
                                                              [--data-dir 目录]
  due      --date 日期                                        [--data-dir 目录]
  history  --asset-id 编号                                    [--data-dir 目录]
  downtime --start 起点 --end 终点 [--asset-id 编号]          [--data-dir 目录]
  import   --source-dir 源目录 --asset-id 编号 [--asset-id 编号...]
                                                              [--data-dir 目录]

通用参数:
  --data-dir 目录   本地数据目录，默认 ".caretrack"；不同目录数据互不影响，
                    目录不存在时会在首次成功写入时自动初始化

业务规则:
  - 资产编号、名称、位置均不能为空；同一数据目录内资产编号唯一，新资产为“可用”。
  - 每项资产最多有一张未关闭工单；未知资产、空故障描述、资产已有未关闭工单时
    报修失败，不改变状态与履历。
  - --request-id 由调用方提供且非空，仅用于同一数据目录内的报修去重，不作为
    资产或工单编号：相同标识、资产、描述再次提交返回原工单及当前状态，不新增
    记录；同一标识搭配不同资产或描述将被拒绝；原工单关闭或取消后重放仍返回原工单，
    不重开旧单，也不影响新单。
  - 仅未关闭工单可派工：没有负责人时首次派工，已有负责人时转派给不同人员。
    维修人员标识只是本地文本记录（非人员账户），与派工说明均不能为空；
    成功后保存负责人、变更时间与说明，并追加一条含原负责人（首次派工为
    “未派工”）、新负责人及说明的派工履历。派工不创建工单、不消耗工单编号，
    不改变资产状态或报修请求绑定；未知工单、空人员或说明、重复派给当前
    人员、对已关闭或已取消工单派工均失败且不产生履历。工单关闭或取消后
    保留最后负责人与派工履历。
  - 仅未关闭工单可关闭，维修结果不能为空；未知工单、重复关闭均失败。
  - 仅未关闭工单可取消，取消理由不能为空；取消后工单进入“已取消”终态，保存取消
    理由与时间，资产恢复“可用”，可再次报修。取消不代表维修完成，不填写维修结果，
    不删除工单、履历或请求绑定，工单编号不回退也不复用；已取消工单不能关闭或再次
    取消，已关闭工单也不能取消。
  - 备件领用：仅未关闭工单可领用；工单编号、非空备件编号、正整数数量与非空说明
    缺一不可。每次成功生成同目录唯一且不复用的领用编号（形如 P0001）并输出；
    同一备件多次领用各自独立，领用编号用于定位记录，不用报修请求标识代替。
    退回按领用编号定位，作用于其所属工单：数量为正整数、理由非空，允许多次
    部分退回，累计不得超过该笔原数量，不能冲减另一笔；成功显示累计退回与净量
    （原数量减累计退回）。未知领用编号、超额退回均被拒绝。工单关闭或取消后
    保留领用记录与净量，不自动清零，但终结后拒绝再领用、再退回；旧单的领用
    记录不影响同一资产之后的新工单。ticket 查询按领用顺序显示各笔编号、备件、
    原数量、累计退回与净量，并按备件编号字典序汇总净量（零值仍显示，无记录
    明确提示）；history 按履历序号展示领用、退回的时间、工单、领用编号、备件、
    数量及说明或理由。领用与退回各为一次原子保存。
  - 每项资产最多一个周期保养计划，建立后不可覆盖：保养内容非空，首次到期日为
    0001 至 9999 年的有效公历日期（YYYY-MM-DD），间隔天数为正整数；日期按日历日
    运算，不受时区与运行时刻影响。未知资产或已有计划时拒绝。资产详情显示保养
    内容、间隔与下一到期日，无计划时明确提示。
  - maintain 登记当前周期的保养完成：--due 所填周期到期日必须等于当前下一到期日
    （旧周期不能重复登记，也不能登记尚未到期的新周期），--done 实际完成日不得早于
    周期到期日，--result 结果非空；无计划时拒绝。成功后记录一次实际保养，下一到期
    日为首次到期日加整数倍间隔所得日期中严格晚于完成日的最早日期；延期跨过的
    周期不生成完成记录，也不改用完成日加间隔。若下一到期日超出 9999-12-31，整次
    拒绝。输出完成周期与下一到期日。
  - due 为只读查询：列出下一到期日不晚于 --date 的保养计划，显示资产编号、名称、
    保养内容和到期日，按到期日再按资产编号排序；无匹配明确提示。不写文件、不
    初始化目录、不推进计划。
  - 每个保养计划恰有一条含初始计划的建立履历，完成履历含周期到期日、实际完成日
    和结果；保养履历与维修履历按全库唯一履历序号共同展示操作时间及内容。维修中
    资产也可建立计划和登记完成：保养不创建或终结工单、不消耗工单编号，不改变
    资产状态、请求绑定或停机统计。
  - downtime 为只读统计，不写文件、不初始化目录、不追加履历。--start/--end 为
    带时区的 RFC3339 时刻（可含小数秒），起点须早于终点；窗口包含起点、不包含
    终点，按实际时刻比较。停机自工单报修履历时间起，至关闭或取消履历时间止
    （取消前的占用同样计入）；派工、转派不另起区间；未关闭工单暂算到窗口终点，
    不读取运行时刻，也不改变状态。只统计工单区间与窗口的交集，窗口外及零长度
    不贡献；同一资产多张工单的重叠交集取并集，不直接相加。每项资产合计后向下
    取整为秒，并输出全部所选资产秒数之和；没有停机的资产显示零。不给 --asset-id
    时统计全部资产并按编号字典序排列，空库明确提示无记录并显示零合计。若所选
    资产任一终结工单的结束履历时间早于报修履历时间（即使该单在窗口外、结束
    等于开始合法），整次统计失败，指出资产与工单，不输出部分结果。
  - import 把源数据目录中所选资产连同全部工单、报修请求绑定、履历、保养计划
    与备件领用记录复制到目标目录（--data-dir）：源台账始终只读，同一台账不能
    导入自身；重复编号按一项处理。工单编号按源工单序号升序从目标下一序号重新
    分配，履历与请求绑定中的工单引用同步替换；备件领用编号按源领用顺序从目标
    下一领用序号重新分配并输出映射，退回履历的领用引用同步更新，数量、时间
    精度与操作顺序保持，导入的未关闭工单可继续退回。履历在目标最大履历序号
    之后按源序号顺序分配新序号，保留原操作顺序与原时间（含小数秒）。保养计划与保养履历原样复制，内容、日期及完成
    链接续，目标已有计划不变。资产编号在目标已存在、或所选工单的任一请求标识已在
    目标绑定时整批拒绝，不覆盖、不合并。导入后可用原请求标识、资产与描述重放报修，
    返回映射后的工单及其当前状态；再次导入同一批资产按编号冲突拒绝。任何失败
    （源不存在、资产不存在、台账损坏、编号或履历序号容量不足、读写失败）都整批
    失败，两边原文件不变，不消耗目标编号。

无参数、--help、-h 显示本帮助；参数错误或业务失败以非零退出码结束并说明原因。
参数错误退出码为 2；未知资产、上述时间异常及数据读取失败退出码为 1。
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
	case "assign":
		err = cmdAssign(args[1:], stdout)
	case "ticket":
		err = cmdTicket(args[1:], stdout)
	case "close":
		err = cmdClose(args[1:], stdout)
	case "cancel":
		err = cmdCancel(args[1:], stdout)
	case "issue":
		err = cmdIssue(args[1:], stdout)
	case "return":
		err = cmdReturn(args[1:], stdout)
	case "plan":
		err = cmdPlan(args[1:], stdout)
	case "maintain":
		err = cmdMaintain(args[1:], stdout)
	case "due":
		err = cmdDue(args[1:], stdout)
	case "history":
		err = cmdHistory(args[1:], stdout)
	case "downtime":
		err = cmdDowntime(args[1:], stdout)
	case "import":
		err = cmdImport(args[1:], stdout)
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
		fmt.Fprintf(w, "工单负责人: %s\n", assigneeDisplay(openTicket.Assignee))
	}
	plan := s.findPlan(asset.ID)
	if plan == nil {
		fmt.Fprintln(w, "保养计划: 无")
	} else {
		fmt.Fprintf(w, "保养内容: %s\n", plan.Content)
		fmt.Fprintf(w, "保养间隔: 每 %d 天\n", plan.IntervalDays)
		fmt.Fprintf(w, "下一到期日: %s\n", plan.NextDue)
	}
	return nil
}

// assigneeDisplay 把空负责人显示为“未派工”。
func assigneeDisplay(assignee string) string {
	if assignee == "" {
		return "未派工"
	}
	return assignee
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

func cmdAssign(args []string, w io.Writer) error {
	var opts cmdOptions
	var ticketID, assignee, note string
	fs := newFlagSet("assign", &opts)
	fs.StringVar(&ticketID, "ticket-id", "", "要派工的工单编号（必填）")
	fs.StringVar(&assignee, "assignee", "", "维修人员标识（必填，本地文本记录，非人员账户）")
	fs.StringVar(&note, "note", "", "派工说明（必填，非空）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, ticketID, "ticket-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, assignee, "assignee"); err != nil {
		return err
	}
	if err := requireFlag(fs, note, "note"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	ticket, err := s.assignTicket(ticketID, assignee, note)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "工单编号: %s\n", ticket.ID)
	fmt.Fprintf(w, "负责人: %s\n", ticket.Assignee)
	return nil
}

func cmdTicket(args []string, w io.Writer) error {
	var opts cmdOptions
	var ticketID string
	fs := newFlagSet("ticket", &opts)
	fs.StringVar(&ticketID, "ticket-id", "", "要查询的工单编号（必填）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, ticketID, "ticket-id"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	t := s.findTicket(ticketID)
	if t == nil {
		return fmt.Errorf("未知工单编号 %q", ticketID)
	}
	fmt.Fprintf(w, "工单编号: %s\n", t.ID)
	fmt.Fprintf(w, "资产编号: %s\n", t.AssetID)
	fmt.Fprintf(w, "工单状态: %s\n", t.Status)
	fmt.Fprintf(w, "负责人: %s\n", assigneeDisplay(t.Assignee))
	if t.Assignee != "" {
		fmt.Fprintf(w, "派工时间: %s\n", t.AssignedAt)
		fmt.Fprintf(w, "派工说明: %s\n", t.AssignNote)
	}
	// 备件台账：按领用顺序列出各笔，并按备件编号字典序汇总净量（零值仍显示）。
	rows := s.partIssuesOf(ticketID)
	if len(rows) == 0 {
		fmt.Fprintln(w, "备件领用: 无")
		return nil
	}
	fmt.Fprintf(w, "备件领用（共 %d 笔）:\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t原数量 %d\t累计退回 %d\t净量 %d\n",
			r.ID, r.PartNo, r.Qty, r.Returned, r.Net)
	}
	fmt.Fprintln(w, "按备件汇总净量:")
	for _, sn := range summarizePartNet(rows) {
		fmt.Fprintf(w, "%s\t净量 %d\n", sn.PartNo, sn.Net)
	}
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

func cmdCancel(args []string, w io.Writer) error {
	var opts cmdOptions
	var ticketID, reason string
	fs := newFlagSet("cancel", &opts)
	fs.StringVar(&ticketID, "ticket-id", "", "要取消的工单编号（必填）")
	fs.StringVar(&reason, "reason", "", "取消理由（必填，非空）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, ticketID, "ticket-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, reason, "reason"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	ticket, asset, err := s.cancelTicket(ticketID, reason)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "工单 %s 已取消。\n", ticket.ID)
	fmt.Fprintf(w, "工单状态: %s\n", ticket.Status)
	fmt.Fprintf(w, "资产 %s 当前状态: %s\n", asset.ID, asset.Status)
	return nil
}

// cmdIssue 为未关闭工单领用备件：生成唯一领用编号并输出，一次原子保存。
func cmdIssue(args []string, w io.Writer) error {
	var opts cmdOptions
	var ticketID, partNo, note string
	var qty int
	fs := newFlagSet("issue", &opts)
	fs.StringVar(&ticketID, "ticket-id", "", "要领用备件的工单编号（必填，仅未关闭工单）")
	fs.StringVar(&partNo, "part-no", "", "备件编号（必填，非空）")
	fs.IntVar(&qty, "qty", 0, "领用数量（必填，正整数）")
	fs.StringVar(&note, "note", "", "领用说明（必填，非空）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, ticketID, "ticket-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, partNo, "part-no"); err != nil {
		return err
	}
	if qty < 1 {
		return &usageError{msg: "--qty 须为正整数"}
	}
	if err := requireFlag(fs, note, "note"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	pi, err := s.issuePart(ticketID, partNo, qty, note)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "领用编号: %s\n", pi.ID)
	fmt.Fprintf(w, "工单编号: %s\n", pi.TicketID)
	fmt.Fprintf(w, "备件编号: %s\n", pi.PartNo)
	fmt.Fprintf(w, "领用数量: %d\n", pi.Qty)
	return nil
}

// cmdReturn 按领用编号退回备件：作用于该笔所属工单，允许多次部分退回，
// 累计不得超过该笔原数量。成功显示累计退回与净量，一次原子保存。
func cmdReturn(args []string, w io.Writer) error {
	var opts cmdOptions
	var issueID, reason string
	var qty int
	fs := newFlagSet("return", &opts)
	fs.StringVar(&issueID, "issue-id", "", "要退回的领用编号（必填）")
	fs.IntVar(&qty, "qty", 0, "退回数量（必填，正整数）")
	fs.StringVar(&reason, "reason", "", "退回理由（必填，非空）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, issueID, "issue-id"); err != nil {
		return err
	}
	if qty < 1 {
		return &usageError{msg: "--qty 须为正整数"}
	}
	if err := requireFlag(fs, reason, "reason"); err != nil {
		return err
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	pi, returned, err := s.returnPart(issueID, qty, reason)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "领用编号: %s\n", pi.ID)
	fmt.Fprintf(w, "工单编号: %s\n", pi.TicketID)
	fmt.Fprintf(w, "备件编号: %s\n", pi.PartNo)
	fmt.Fprintf(w, "原数量: %d\n", pi.Qty)
	fmt.Fprintf(w, "累计退回: %d\n", returned)
	fmt.Fprintf(w, "净量: %d\n", pi.Qty-returned)
	return nil
}

// cmdPlan 为资产建立唯一的周期保养计划。日期与间隔的参数错误为退出码 2；
// 未知资产、已有计划等业务失败为退出码 1。
func cmdPlan(args []string, w io.Writer) error {
	var opts cmdOptions
	var assetID, content, firstDue string
	var interval int
	fs := newFlagSet("plan", &opts)
	fs.StringVar(&assetID, "asset-id", "", "企业资产编号（必填）")
	fs.StringVar(&content, "content", "", "保养内容（必填，非空）")
	fs.StringVar(&firstDue, "first-due", "", "首次到期日（必填，YYYY-MM-DD，0001 至 9999 年的有效公历日期）")
	fs.IntVar(&interval, "interval-days", 0, "保养间隔天数（必填，正整数）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, assetID, "asset-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, content, "content"); err != nil {
		return err
	}
	if err := requireFlag(fs, firstDue, "first-due"); err != nil {
		return err
	}
	if _, err := parseDate(firstDue); err != nil {
		return &usageError{msg: fmt.Sprintf("--first-due 无效：%s", err)}
	}
	if interval < 1 {
		return &usageError{msg: "--interval-days 须为正整数"}
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	p, err := s.createPlan(assetID, content, firstDue, interval)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "已为资产 %s 建立保养计划。\n", p.AssetID)
	fmt.Fprintf(w, "保养内容: %s\n", p.Content)
	fmt.Fprintf(w, "保养间隔: 每 %d 天\n", p.IntervalDays)
	fmt.Fprintf(w, "首次到期日: %s\n", p.FirstDue)
	return nil
}

// cmdMaintain 登记当前周期的保养完成并推进下一到期日。
func cmdMaintain(args []string, w io.Writer) error {
	var opts cmdOptions
	var assetID, due, done, result string
	fs := newFlagSet("maintain", &opts)
	fs.StringVar(&assetID, "asset-id", "", "企业资产编号（必填）")
	fs.StringVar(&due, "due", "", "所完成周期的到期日（必填，YYYY-MM-DD，须等于当前下一到期日）")
	fs.StringVar(&done, "done", "", "实际完成日（必填，YYYY-MM-DD，不得早于周期到期日）")
	fs.StringVar(&result, "result", "", "保养结果（必填，非空）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, assetID, "asset-id"); err != nil {
		return err
	}
	if err := requireFlag(fs, due, "due"); err != nil {
		return err
	}
	if err := requireFlag(fs, done, "done"); err != nil {
		return err
	}
	if err := requireFlag(fs, result, "result"); err != nil {
		return err
	}
	if _, err := parseDate(due); err != nil {
		return &usageError{msg: fmt.Sprintf("--due 无效：%s", err)}
	}
	if _, err := parseDate(done); err != nil {
		return &usageError{msg: fmt.Sprintf("--done 无效：%s", err)}
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	p, next, err := s.completePlan(assetID, due, done, result)
	if err != nil {
		return err
	}
	if err := s.save(); err != nil {
		return err
	}
	fmt.Fprintf(w, "资产 %s 已完成周期 %s 的保养。\n", p.AssetID, due)
	fmt.Fprintf(w, "实际完成日: %s\n", done)
	fmt.Fprintf(w, "下一到期日: %s\n", next)
	return nil
}

// cmdDue 为只读的到期查询：列出下一到期日不晚于指定日期的保养计划。
// 不写文件、不初始化目录、不推进计划。
func cmdDue(args []string, w io.Writer) error {
	var opts cmdOptions
	var date string
	fs := newFlagSet("due", &opts)
	fs.StringVar(&date, "date", "", "查询截止日期（必填，YYYY-MM-DD；列出下一到期日不晚于该日的计划）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, date, "date"); err != nil {
		return err
	}
	if _, err := parseDate(date); err != nil {
		return &usageError{msg: fmt.Sprintf("--date 无效：%s", err)}
	}

	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}
	rows := s.duePlans(date)
	if len(rows) == 0 {
		fmt.Fprintf(w, "截至 %s 没有到期的保养计划。\n", date)
		return nil
	}
	fmt.Fprintf(w, "截至 %s 共 %d 项到期保养计划:\n", date, len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.AssetID, r.Name, r.Content, r.Due)
	}
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
		ts := e.Time.Format("2006-01-02T15:04:05Z07:00")
		switch e.Kind {
		case eventAssign:
			fmt.Fprintf(w, "[%s] %s 工单 %s: %s -> %s（%s）\n",
				ts, e.Kind, e.TicketID, assigneeDisplay(e.From), e.To, e.Content)
		case eventPlanCreate:
			fmt.Fprintf(w, "[%s] %s: %s（首次到期日 %s，每 %d 天）\n",
				ts, e.Kind, e.Content, e.Due, e.Interval)
		case eventPlanDone:
			fmt.Fprintf(w, "[%s] %s: 周期到期日 %s，实际完成日 %s，%s\n",
				ts, e.Kind, e.Due, e.Done, e.Content)
		case eventPartIssue, eventPartReturn:
			fmt.Fprintf(w, "[%s] %s 工单 %s 领用编号 %s: 备件 %s，数量 %d，%s\n",
				ts, e.Kind, e.TicketID, e.IssueID, e.PartNo, e.Qty, e.Content)
		default:
			fmt.Fprintf(w, "[%s] %s 工单 %s: %s\n", ts, e.Kind, e.TicketID, e.Content)
		}
	}
	return nil
}

// cmdDowntime 为只读的停机时长统计：先经 openStore 完成整库一致性检查，
// 再计算窗口内的停机秒数。无论成功或失败都不保存，因此不写文件、不初始化
// 目录、不追加履历、不消耗编号，也不改变报修请求绑定。
func cmdDowntime(args []string, w io.Writer) error {
	var opts cmdOptions
	var startText, endText, assetID string
	fs := newFlagSet("downtime", &opts)
	fs.StringVar(&startText, "start", "", "窗口起点（必填，带时区的 RFC3339 时刻，可含小数秒；包含）")
	fs.StringVar(&endText, "end", "", "窗口终点（必填，带时区的 RFC3339 时刻，可含小数秒；不包含）")
	fs.StringVar(&assetID, "asset-id", "", "企业资产编号（可选；缺省统计全部资产）")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag(fs, startText, "start"); err != nil {
		return err
	}
	if err := requireFlag(fs, endText, "end"); err != nil {
		return err
	}
	start, err := time.Parse(time.RFC3339, startText)
	if err != nil {
		return &usageError{msg: fmt.Sprintf("--start 不是带时区的 RFC3339 时刻 %q: %s", startText, err)}
	}
	end, err := time.Parse(time.RFC3339, endText)
	if err != nil {
		return &usageError{msg: fmt.Sprintf("--end 不是带时区的 RFC3339 时刻 %q: %s", endText, err)}
	}
	if !start.Before(end) {
		return &usageError{msg: fmt.Sprintf("--start（%s）须早于 --end（%s）", startText, endText)}
	}

	// openStore 已先做整库一致性检查：损坏或关联矛盾按原规则拒绝（退出码 1）。
	s, err := openStore(opts.dataDir)
	if err != nil {
		return err
	}

	var ids []string
	if assetID != "" {
		// 未知资产在统计前拒绝（退出码 1）。
		if s.findAsset(assetID) == nil {
			return fmt.Errorf("未知资产编号 %q", assetID)
		}
		ids = []string{assetID}
	} else {
		ids = s.allAssetIDs()
		if len(ids) == 0 {
			fmt.Fprintln(w, "没有记录。")
			fmt.Fprintln(w, "合计: 0 秒")
			return nil
		}
	}

	// 先算完全部结果再输出：任一资产存在时间异常时整体失败，不输出部分结果。
	results, err := s.downtimeForAssets(ids, start, end)
	if err != nil {
		return err
	}
	var total int64
	if len(results) == 1 {
		r := results[0]
		fmt.Fprintf(w, "资产编号: %s\n", r.AssetID)
		fmt.Fprintf(w, "名称: %s\n", r.Name)
		fmt.Fprintf(w, "停机秒数: %d\n", r.Seconds)
		total = r.Seconds
	} else {
		fmt.Fprintf(w, "共 %d 项资产:\n", len(results))
		for _, r := range results {
			fmt.Fprintf(w, "%s\t%s\t%d\n", r.AssetID, r.Name, r.Seconds)
			total += r.Seconds
		}
	}
	fmt.Fprintf(w, "合计: %d 秒\n", total)
	return nil
}
