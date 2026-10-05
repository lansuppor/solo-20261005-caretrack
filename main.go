package main

import (
	"fmt"
	"os"
	"strings"
)

const appName = "caretrack"

const helpText = appName + `

设备资产登记与维修工单闭环命令行工具，数据保存在本地目录。

Usage:
  caretrack [--dir <数据目录>] <命令> [参数]
  caretrack --help | -h

命令:
  register <资产编号> <名称> <位置>      登记新资产（初始状态为“可用”）
  list                                  列出全部资产及当前状态
  show <资产编号>                        查看资产详情及未关闭工单编号
  repair <资产编号> <故障描述> <请求标识>  提交报修，返回工单编号
  close <工单编号> <维修结果>             关闭工单，设备恢复“可用”
  history <资产编号>                     查看该资产的维修履历

选项:
  --dir <目录>   数据目录（默认 ./caretrack-data），不同目录互不影响
  --help, -h     显示本帮助
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(helpText)
		return 0
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(helpText)
		return 0
	}

	dir := "./caretrack-data"
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, appName+": --dir 需要一个目录参数")
				return 2
			}
			dir = args[i+1]
			i++
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
			if dir == "" {
				fmt.Fprintln(os.Stderr, appName+": --dir 需要一个目录参数")
				return 2
			}
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		fmt.Print(helpText)
		return 0
	}

	cmd, cmdArgs := rest[0], rest[1:]
	var err error
	switch cmd {
	case "register":
		err = cmdRegister(dir, cmdArgs)
	case "list":
		err = cmdList(dir, cmdArgs)
	case "show":
		err = cmdShow(dir, cmdArgs)
	case "repair":
		err = cmdRepair(dir, cmdArgs)
	case "close":
		err = cmdClose(dir, cmdArgs)
	case "history":
		err = cmdHistory(dir, cmdArgs)
	case "--help", "-h":
		fmt.Print(helpText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown arguments; use --help\n", appName)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, appName+": "+err.Error())
		if ue, ok := err.(*usageError); ok && ue != nil {
			return 2
		}
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}
