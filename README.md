# caretrack

本地设备资产登记与维修工单闭环命令行工具。数据保存在本地目录中的单个 JSON 文件，
无外部服务依赖，仅要求单进程顺序调用。

- Go 1.24 或更新版本，仅使用标准库。
- 不同 `--data-dir` 目录的数据互不影响；目录不存在时在首次成功写入自动初始化。
- 资产状态、工单、履历、请求去重信息在一次成功写操作中原子生效（临时文件 + rename）；
  业务校验或读写失败时非零退出并说明原因，业务数据保持操作前状态。
- 已有数据文件损坏（含空文件）时报错并保留原文件，不会当空库覆盖。

## 构建与帮助

```sh
go build -o caretrack .
./caretrack            # 无参数显示应用名与帮助
./caretrack --help     # 同 -h / help
```

参数错误退出码为 2；业务失败或数据读写失败退出码为 1。

## 命令

通用参数 `--data-dir 目录` 指定本地数据目录，默认 `.caretrack`。

### 登记资产

```sh
caretrack register --asset-id EQ-001 --name 打印机 --location 一楼大厅
```

资产编号（企业资产编号）、名称、位置均不能为空；编号在同一数据目录内唯一；
新资产状态为“可用”。

### 列表

```sh
caretrack list
```

显示编号、名称、位置和当前状态；没有记录时明确输出“没有记录。”。

### 详情

```sh
caretrack detail --asset-id EQ-001
```

显示资产信息、当前状态，以及未关闭工单编号（无则显示“无”）。

### 报修

```sh
caretrack report --asset-id EQ-001 --description 频繁卡纸 --request-id req-20261005-01
```

- 成功后返回独立工单编号（形如 `T0001`），资产变为“维修中”，并写入一条报修履历。
- 未知资产、空描述、资产已有未关闭工单时均失败，不改变状态与履历。
- `--request-id` 为调用方提供的非空标识，仅用于同一数据目录内的报修去重，
  既不是资产编号也不是工单编号：
  - 相同标识 + 相同资产 + 相同描述再次提交：返回原工单及当前状态，不新增记录。
  - 相同标识搭配不同资产或不同描述：拒绝。
  - 原工单关闭后重放：仍返回原工单（已关闭状态），不重开旧单，也不影响之后的新工单。
- 失败的新报修不会绑定请求标识；已有绑定保持不变。
- 每项资产最多有一张未关闭工单。

### 关闭工单

```sh
caretrack close --ticket-id T0001 --repair-result 已更换搓纸轮
```

仅未关闭工单可关闭，维修结果不能为空；成功后设备恢复“可用”，并写入一条关闭履历。
未知工单、空维修结果、重复关闭均失败且不改动记录。

### 维修履历

```sh
caretrack history --asset-id EQ-001
```

按操作发生顺序展示该资产的报修、关闭事件，包含所属工单编号、时间和内容；
失败操作不产生履历。无履历时明确提示。

## 完整示例

```sh
D=/tmp/caretrack-data
caretrack register --data-dir $D --asset-id EQ-001 --name 打印机 --location 一楼大厅
caretrack report   --data-dir $D --asset-id EQ-001 --description 频繁卡纸 --request-id req-1
caretrack detail   --data-dir $D --asset-id EQ-001
caretrack close    --data-dir $D --ticket-id T0001 --repair-result 已更换搓纸轮
caretrack history  --data-dir $D --asset-id EQ-001
```

## 测试

```sh
go test ./...
```
