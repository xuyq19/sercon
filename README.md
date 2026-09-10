# sercon

sercon 是一套通过 SSH 访问远程串口的工具。串口设备物理连接在跳板机上，当跳板机与
开发机不在同一网段、本地终端程序无法直接访问该串口时，由 sercon 提供等效的访问能力。

sercon 由两个进程组成：客户端 `sercon` 运行在本地主机上，提供终端交互；守护进程
`sercond` 运行在跳板机上，持有串口设备。两者之间的通信通过 SSH 承载，因此无需在
跳板机上部署常驻服务或开放额外端口。

![sercon-gui](docs/images/sercon-gui.png)

## 安装

跳板机（连接串口的主机）：

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond && sudo usermod -aG dialout $USER'
```

`dialout` 组的添加是必需的，串口设备权限为 `crw-rw---- root:dialout`。组变更需要
新会话才能生效，因此完成后须重新登录；否则端口虽能被枚举，但状态均为 `offline`。

本地主机：

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

Windows 跳板机见 [Windows](#windows)。

## 用法

```bash
sercon ls -t you@jump                  # 列出端口及其占用情况
sercon attach -t you@jump FT232R       # 建立交互式连接
```

当 `sercond` 安装于 `~/bin` 而该目录不在远程 PATH 中时，需指定 `--remote-bin`：

```bash
sercon attach -t you@jump --remote-bin '~/bin/sercond' FT232R
```

`~` 必须置于引号之外，否则远程 shell 不会展开。

### 命令

| 命令 | 作用 |
|---|---|
| `sercon ls -t HOST` | 列出端口：引用、设备、波特率、状态、持有者 |
| `sercon attach -t HOST PORT` | 交互式连接 |
| `sercon run -t HOST PORT` | 脚本化会话 |
| `sercon status -t HOST` | 守护进程状态 |
| `sercon stop -t HOST` | 停止守护进程 |

五个命令均支持 `~/.ssh/config` 中定义的别名，因此 `-t jump` 即可，ProxyJump 由
SSH 处理。

`PORT` 可以只写能唯一识别的前缀。例如端口引用为
`usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0` 时，写 `FT232R` 即可。若匹配到多个
端口，程序会报错并列出候选，不会自行选择。

### attach

| 选项 | 作用 |
|---|---|
| `--observe` | 只读旁观，不获取写权限 |
| `--log FILE` | 同时在本地保留一份日志 |
| `--no-reconnect` | 断线后不重连 |
| `--baud N` | 覆盖波特率 |

默认在断线后自动重连。旁观者可以接收输出，但键盘输入不会被转发，因此不会出现
两人同时向 console 输入的情况。

`Ctrl-A` 是转义键，键位与 minicom 对齐：`Ctrl-A x` 退出，`Ctrl-A a` 发送字面量
Ctrl-A，`Ctrl-A l` 开关本地日志，`Ctrl-A r` 重连，`Ctrl-A b` 发送 break，
`Ctrl-A ?` 显示帮助。

### 管道

`attach` 不要求 stdin 是终端。当 stdin 不是 TTY 时，程序不进入 raw 模式、不解析
转义键、不向 stdout 写入任何装饰性输出，行为与直接访问本地设备一致：

```bash
# 采集一段输出，等价于 cat /dev/ttyUSB1，按 Ctrl-C 结束
sercon attach -t jump FT232R < /dev/null | head -50

# 等待指定关键字出现
sercon attach -t jump FT232R < /dev/null | grep -m1 panic

# 写入文件
sercon attach -t jump FT232R < /dev/null >> bench01.log

# 写入数据，等价于 echo -e '\r' > /dev/ttyUSB1，发送后即退出
sercon run -t jump FT232R --send '\r' --quiet

# 写入并等待响应
sercon run -t jump FT232R --send 'reboot\r' --expect 'Restarting system' --timeout 60s
```

与本地设备相比有两点差异：

- **`attach` 持续读取直到链路断开。** `cat /dev/ttyUSB1 </dev/null` 的行为相同：
  stdin 关闭仅表示不再有输入，不代表应当停止读取。管道模式下不自动重连，链路断开
  即结束
- **写入使用 `run --send`。** 配合 `--expect` 可以等待响应后再退出，比直接写入更
  便于脚本使用

stdout 仅承载 console 数据。端口名、日志路径等信息输出到 stderr，因此重定向和管道
不会被污染。

### run

重启目标机并采集完整的 boot log：

```bash
sercon run -t jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

也可以不使用脚本文件：

```bash
sercon run -t jump FT232R --send '\r' --expect 'login:' --expect '#' --timeout 30s
```

脚本语法共四条：

```
send <text>      写文本，支持 \r \n \t \0 \\ \xHH
sendln <text>    写文本并追加 CRLF
wait <regex>     阻塞等待正则匹配
sleep 2s         暂停
```

`--expect` 未匹配到时会报错并以非零状态退出：

```
sercon: --expect #1: pattern not seen before the timeout: ZZZZ_NOMATCH
```

## 日志

日志由守护进程写入，与是否存在客户端连接无关。

| 内容 | 位置 |
|---|---|
| 串口输出 | Linux `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log` |
| | Windows `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级的 `audit/audit-YYYY-MM-DD.jsonl` |
| 守护进程诊断 | `<runtime>/daemon.log` |
| socket | Linux `$XDG_RUNTIME_DIR/sercon/run/s.sock` |
| | Windows `%LOCALAPPDATA%\sercon\run\s.sock` |

日志按天分文件，只追加不删除，轮转由 logrotate 负责。

```
2026-09-10 16:24:47.915 [177721.712505] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
```

审计流水为 JSONL 格式，每行一个事件：

```json
{"ts":"2026-09-10T16:22:21.40+08:00","event":"session_open","user":"lucas","client":"DESKTOP-ROD9JT0","session":"516954b3"}
{"ts":"2026-09-10T16:22:21.43+08:00","event":"port_open","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...-port0","dev":"/dev/ttyUSB1","session":"516954b3","detail":"writable"}
```

## 配置

`~/.config/sercon/config.json`，Windows 下为 `%APPDATA%\sercon\config.json`。
所有字段均可省略。

```json
{
  "baud": 115200,
  "ports": [
    { "ref": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0", "desc": "WR5220-G5-bmc" },
    { "ref": "COM3", "desc": "lab-bench-01" }
  ]
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `baud` | 115200 | 默认波特率 |
| `auto_open` | true | 启动时打开所有已发现的端口 |
| `stamp_logs` | true | 日志行首添加时间戳；关闭后可被终端模拟器原样回放 |
| `allow_observe` | true | 允许只读旁观 |
| `max_observers` | 4 | 每端口旁观者上限，0 表示不限制 |
| `scan_globs` | — | Linux 上额外扫描的路径 |
| `log_dir` / `audit_dir` | — | 覆盖默认位置 |
| `ports[]` | — | 逐端口固定 `ref` / `desc` / `baud` |

`desc` 可直接用作 `attach` 的端口引用。实际可用的 `ref` 可通过
`sercon ls -t jump --json` 查看。

修改配置后需执行 `sercon stop -t jump`，重启守护进程后方生效。

## Windows

Windows 跳板机上需要部署两个二进制文件，职责不同：

| 文件 | 角色 | 启动方式 |
|---|---|---|
| `sercon-gui.exe` | 带窗口的守护进程，持有串口 | 双击，或置于启动文件夹 |
| `sercond.exe`（发布包中为 `sercond-windows-amd64.exe`） | `session` / `list` / `status` / `stop` | 由 SSH 拉起 |

两者缺一不可。只有 GUI 时，SSH 侧缺少可调用的子命令；只有 CLI 时没有窗口，需通过
`sercond capture` 以前台方式手动启动守护进程。`sercon-gui.exe.manifest` 须与 exe
置于同一目录。

主窗口显示端口列表，底部提供四个按钮：打开日志目录、复制 attach 命令、SSH keys、
立即重扫。窗口打开期间持续采集日志，关闭即停止，没有独立的启停开关。
`sercon stop -t winjump` 会关闭该窗口，而不仅是返回确认。

GUI 必须在交互式桌面会话中启动。SSH 会话启动的进程无法绘制窗口，表现为进程存在但
界面不可见。应通过双击或置于启动文件夹（`Win+R` → `shell:startup`）启动。

### SSH keys

`SSH keys` 按钮用于安装客户端公钥，避免手工编辑 `authorized_keys`。

Windows 上有两个容易出错的点，且都表现为同一条 `Permission denied (publickey)`：
密钥文件的位置取决于账号是否为管理员；管理员所用的文件还必须收紧 ACL。该面板同时
处理这两项。

管理员账号应写入的文件是：

```
C:\ProgramData\ssh\administrators_authorized_keys
```

而非 `%USERPROFILE%\.ssh\authorized_keys`。默认 `sshd_config` 末尾包含以下配置：

```
Match Group administrators
       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys
```

因此写入 `~/.ssh/authorized_keys` 看似正确，但 sshd 不会读取该文件。面板会根据账号
自动选择正确的文件，并在顶部显示其路径。

该文件的 ACL 必须仅包含 `SYSTEM` 和 `Administrators`，sshd 才会读取——能够修改此
文件的人即可以任意身份登录。面板在写入后会自动收紧 ACL。

两个操作路径都需要管理员权限，因此「Add key」和「Reload」会触发 UAC 提示。GUI 本身
不提权：串口守护进程不应以管理员身份运行，提权仅发生在写入该文件时。

改动立即生效，无需重启 sshd。

跳板机侧还需安装 OpenSSH Server 并放行防火墙，参见
[`deployment.md`](docs/deployment.md#windows-跳板机)。

Windows 上直接以 COM 号引用端口：`sercon attach -t winjump COM3`。

日志目录名中包含下划线：

```
%LOCALAPPDATA%\sercon\ports\COM1_\2026-09-10.log
                              ^^^
```

`COM1` 至 `COM9` 是 Windows 保留设备名，`mkdir COM1` 会直接失败，因此转义为
`COM1_`。`COM10` 及以上不是保留名，因此该问题是否出现取决于机器上曾经使用过的
适配器数量，并不稳定。日志内容中的头部仍写 `port=COM1`。

## 排错

**端口状态全部为 `offline`。** Linux 上通常由 dialout 权限引起，用
`id | grep dialout` 确认，不在组内则执行 `sudo usermod -aG dialout $USER` 并重新
登录。具体原因可查看 `sercon ls -t jump --json` 输出的 `last_err` 字段，表格输出
中不包含该列。

**每条命令前被追加了三行 SSH 警告。** 跳板机的 OpenSSH 版本较旧（Ubuntu 20.04 的
8.2 与 22.04 的 8.9 均不支持后量子密钥交换），其 stderr 被转发至终端。添加以下配置
即可消除：

```
Host jump
    LogLevel ERROR
```

**`sercond: command not found`。** `sercond` 不在远程 PATH 中，使用 `--remote-bin`
指定其路径。

**`sercon stop` 之后端口仍在采集。** socket 按用户隔离，确认连接的是同一台主机上的
同一用户。

## 构建

```bash
make build                          # 全平台 + 版本注入 + GUI
go build -o sercond ./cmd/sercond   # 仅当前平台
```

版本号以 `internal/version` 为唯一来源，在链接时注入。直接使用 `go build` 会显示
`devel`：

```
$ ./sercond version
sercond devel (protocol v1, go1.27.1, linux/amd64)
```

测试：

```bash
go test ./...

# 真实硬件测试，会占用端口并拉高 DTR/RTS
SERCON_TEST_HARDWARE=1 SERCON_TEST_PORT=COM1 go test ./internal/serialport/ -v
```

硬件测试默认跳过，以避免在接有重要设备的机器上占用串口。

## 发布

打 tag 即触发发布：

```bash
git tag -a v0.1.0 -m "first release"
git push origin v0.1.0
```

CI 会构建全平台产物、生成校验和、创建 Release，并校验二进制上报的版本与 tag 一致。

## 状态

已在 Ubuntu 20.04 / kernel 5.15 上通过真实硬件验证（两个 FTDI 适配器，其中一个
连接 BMC 串口）：SSH 传输、脱离会话、termios + epoll 打开硬件、by-id 枚举、模糊
匹配、读路径落盘、拔线检测与自动重连、日志连续性、交互式 attach、审计流水。

尚未验证的两项：

- Linux 写路径仅验证到调用未报错。两个适配器均未连接可回显的设备，也没有回环插头，
  因此「字节是否真正送达目标机」未得到确认。读路径不受影响。
- Windows 拔线检测未实测。逻辑与 Linux 侧一致，但未进行真实的拔线操作。

详细记录见 [`docs/deployment.md`](docs/deployment.md)。

## 文档

| | |
|---|---|
| [docs/usage.md](docs/usage.md) | 每个参数、脚本语法、排错 |
| [docs/design.md](docs/design.md) | 设计取舍、串口后端差异、实现陷阱 |
| [docs/deployment.md](docs/deployment.md) | 部署清单、验证记录 |
| [CHANGELOG.md](CHANGELOG.md) | 版本变更 |

## 结构

```
cmd/sercond/          跳板机端：capture / session / list / status / stop
cmd/sercon/           客户端：ls / attach / run / status / stop
cmd/sercon-gui/       Windows 带窗口的守护进程
internal/proto/       帧格式与控制消息
internal/serialport/  串口后端（linux / windows / other）
internal/hub/         端口状态机、广播、端口锁、backlog
internal/portlog/     按天轮转的串口日志
internal/audit/       JSONL 审计流水
internal/daemon/      守护进程的分离启动
internal/ipc/         socket 端点解析与单实例绑定
internal/terminal/    客户端原始终端模式
internal/winconsole/  Windows 控制台探测与双击保活
internal/sshauth/      sshd 密钥文件定位、解析与写入
internal/version/     版本号唯一来源
internal/relay/       中继传输（已实现，未接入 CLI）
internal/config/      配置
```

## License

MIT
