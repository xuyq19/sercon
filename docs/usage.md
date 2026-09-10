# 使用手册

完整的参数说明。快速上手参见 [`../README.md`](../README.md)。

## 目录

- [前提](#前提)
- [连接参数](#连接参数)
- [ls](#ls)
- [attach](#attach)
- [run](#run)
- [status 和 stop](#status-和-stop)
- [端口引用匹配](#端口引用匹配)
- [日志与审计](#日志与审计)
- [配置](#配置)
- [排错](#排错)

## 前提

跳板机上需有 `sercond`，且执行用户已加入 `dialout` 组：

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond && sudo usermod -aG dialout $USER'
```

组变更需要新会话才能生效，因此完成后须重新登录。

本地主机上需有 `sercon`：

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

`sercond` 安装于 `~/bin` 时，该目录通常不在非交互式 SSH 的 PATH 中，客户端需指定
`--remote-bin`。也可以将 `~/bin` 加入远程 PATH，或配置别名。

## 连接参数

五个子命令共用的参数：

| 参数 | 说明 |
|---|---|
| `-t, --target` | SSH 目标，`user@host`，必填。支持 `~/.ssh/config` 的别名 |
| `--ssh-port N` | SSH 端口 |
| `--ssh-opt K=V` | 额外的 `ssh -o` 选项，可重复 |
| `--remote-bin PATH` | 跳板机上 `sercond` 的路径，默认为 `sercond` |
| `--remote-socket PATH` | 覆盖守护进程 socket 路径 |
| `--ssh-bin PATH` | 指定使用的 ssh 客户端，默认为 `ssh`。安装有多个 OpenSSH 时应使用完整路径 |

`-t` 支持 ssh 配置，因此按下列方式编写即可：

```
# ~/.ssh/config
Host jump
    HostName 10.245.39.92
    User lucas
    ProxyJump bastion
    LogLevel ERROR
```

```bash
sercon attach -t jump FT232R
```

需要穿越跳板机时，`ProxyJump` 由 SSH 处理，sercon 不介入。

## ls

```bash
sercon ls -t you@jump
```

```
REF                                           DEV           BAUD    STATE   OWNER  OBS  DESC
usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0  /dev/ttyUSB1  115200  online  -      -    WR5220-G5-bmc
usb-FTDI_USB__-__Serial-if00-port0            /dev/ttyUSB0  115200  online  -      -    -
```

| 列 | 含义 |
|---|---|
| `REF` | 端口引用，`attach` 时使用 |
| `DEV` | 设备路径 |
| `BAUD` | 当前波特率 |
| `STATE` | `online` 或 `offline` |
| `OWNER` | 持有写权限的用户，`-` 表示无人持有 |
| `OBS` | 观察者数量 |
| `DESC` | 配置中的标签 |

`--json` 输出机器可读的版本：

```bash
sercon ls -t jump --json | jq '.[] | select(.state=="offline") | .ref'
```

JSON 中包含 `last_err` 字段，记录端口无法打开的原因；表格输出中不包含该列。

## attach

```bash
sercon attach -t you@jump FT232R
```

连接建立后终端进入原始模式，按键被直接送往串口，Ctrl-C 等组合键不会中断 sercon。
退出使用 `Ctrl-A x`。

| 选项 | 说明 |
|---|---|
| `--baud N` | 本次连接的波特率，覆盖守护进程的默认值 |
| `--observe` | 只读旁观，不获取写权限 |
| `--log FILE` | 同时在本地保留一份日志 |
| `--no-reconnect` | 断线后直接退出 |

### 只读旁观

```bash
sercon attach -t jump FT232R --observe
```

旁观者可以接收串口输出，键盘输入不会被转发。数量上限由跳板机配置的
`max_observers` 控制，默认为 4。

### 断线重连

默认情况下每次断线都会自动重连，采用指数退避，重连后重新打开原端口，并在终端
输出一行恢复标记。

脚本中若需在断线时明确失败，应使用 `--no-reconnect`。

**stdin 不是终端时不会重连**，相当于隐式启用了 `--no-reconnect`。原因见下节。

### 管道

`attach` 不要求 stdin 是终端。当 stdin 不是 TTY 时，程序不进入 raw 模式、不解析
Ctrl-A、不向 stdout 写入任何装饰性输出，仅进行字节搬运，行为与直接访问本地设备
一致：

```bash
# 等价于 cat /dev/ttyUSB1，按 Ctrl-C 结束
sercon attach -t jump FT232R < /dev/null

# 采集 50 行
sercon attach -t jump FT232R < /dev/null | head -50

# 等待关键字出现，匹配后退出
sercon attach -t jump FT232R < /dev/null | grep -m1 panic

# 追加写入文件
sercon attach -t jump FT232R < /dev/null >> bench01.log
```

**stdin 关闭不等于读取结束。** 管道中 stdin 到达 EOF 仅表示不再有输入，链路仍在
接收数据，因此 `< /dev/null` 会持续读取直到链路断开或被中断，与
`cat /dev/ttyUSB1 </dev/null` 的行为相同。

**写入应使用 `run --send`**，而非 `attach`：

```bash
sercon run -t jump FT232R --send '\r' --quiet                     # 发送后退出
sercon run -t jump FT232R --send 'reboot\r' --expect 'login:' --timeout 60s
```

`attach` 不适合用于写入，原因是它在形态上无法区分「发送后退出」和「持续读取到
结束」这两种意图——两者都是一根管道。具体使用哪种由子命令来区分。

stdout 仅承载 console 数据。「已连接端口 X」「日志位于 Y」一类提示在非终端模式下
输出到 stderr，因此重定向和管道不会被污染。

### Ctrl-A

| 按键 | 动作 |
|---|---|
| `Ctrl-A x` / `Ctrl-A q` | 断开退出 |
| `Ctrl-A a` | 发送字面量 Ctrl-A |
| `Ctrl-A l` | 开关本地日志，没配 `--log` 时自动建一个 |
| `Ctrl-A r` | 立即重连 |
| `Ctrl-A b` | 在串口线上发 break |
| `Ctrl-A s` | 会话状态 |
| `Ctrl-A ?` | 帮助 |

向目标机发送真正的 `0x01` 需使用 `Ctrl-A a`，否则会被识别为转义前缀。

## run

### 直接传入参数

```bash
sercon run -t jump FT232R \
  --send '\r' \
  --expect 'login:' \
  --expect '#' \
  --timeout 30s \
  --out session.log
```

`--expect` 可重复，按顺序匹配，每个都有各自的 timeout。

### 使用脚本文件

```bash
sercon run -t jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

| 指令 | 说明 |
|---|---|
| `send <text>` | 写入文本，支持 `\r` `\n` `\t` `\0` `\\` `\xHH` |
| `sendln <text>` | 写入文本并追加 CRLF |
| `wait <regex>` | 阻塞等待正则匹配 |
| `sleep 2s` | 暂停 |

以 `#` 开头的行为注释。`wait` 的匹配起点接续上一次匹配的结尾，因此同一模式可以
连续等待两次（先等首次启动的 `login:`，再等重启后的 `login:`）。

`examples/reboot-capture.script` 是完整的例子。

### 选项

| 选项 | 说明 |
|---|---|
| `--script FILE` | 脚本文件 |
| `--send TEXT` | 在所有 wait 之前发送一次文本 |
| `--expect REGEX` | 等待正则匹配，可重复，按顺序 |
| `--timeout DUR` | 每个 wait 的超时，默认为 `60s` |
| `--out FILE` | 保存收到的所有数据 |
| `--quiet` | 不向 stdout 回显 |

`--expect` 未匹配到时会报错并以非零状态退出，不会静默返回空文件：

```
$ sercon run -t jump FT232R --send '\r' --expect 'ZZZZ_NOMATCH' --timeout 3s
sercond: attached to usb-FTDI_FT232R_... @ 115200 baud (writable)
sercond: console log /home/lucas/.local/state/sercon/ports/usb-FTDI_FT232R_.../2026-09-10.log
[177576.862609] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
sercon: --expect #1: pattern not seen before the timeout: ZZZZ_NOMATCH
```

## status 和 stop

```bash
$ sercon status -t jump
daemon   running
socket   /run/user/1000/sercon/run/s.sock
ports    2 (2 online, 0 held)
```

```bash
sercon stop -t jump
```

Windows GUI 版本收到 stop 后会实际关闭窗口。

## 端口引用匹配

```bash
# 完整的 by-id 引用
sercon attach -t jump usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0

# 配置中的 desc
sercon attach -t jump WR5220-G5-bmc

# 能唯一识别的前缀
sercon attach -t jump FT232R
```

匹配到多个端口时会报错并列出候选，不会自行选择：

```
sercon: "FTDI" matches 2 ports:
  usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0
  usb-FTDI_USB__-__Serial-if00-port0
```

Windows 上直接使用 COM 号：`sercon attach -t winjump COM3`。

## 日志与审计

### 位置

| 内容 | Linux | Windows |
|---|---|---|
| 串口输出 | `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log` | `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级 `audit/audit-YYYY-MM-DD.jsonl` | 同左 |
| 守护进程诊断 | `<runtime>/daemon.log` | `<runtime>\daemon.log` |
| socket | `$XDG_RUNTIME_DIR/sercon/run/s.sock` | `%LOCALAPPDATA%\sercon\run\s.sock` |

未设置 `$XDG_STATE_HOME` 和 `$XDG_RUNTIME_DIR` 时使用上表的默认值。

### 端口日志

```
2026-09-10 16:24:47.915 [177721.712505] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
```

按天分文件，只追加写入。文件开头有一行头部，记录当时的设备路径、端口引用和波特率，
便于事后确认文件与设备的对应关系。轮转由 logrotate 负责，sercon 不删除文件。

`stamp_logs: false` 可去除时间戳前缀，使日志能被终端模拟器原样回放。

### 审计流水

JSONL 格式，每行一个事件：

```json
{"ts":"2026-09-10T16:22:21.32+08:00","event":"port_online","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","detail":"baud=115200"}
{"ts":"2026-09-10T16:22:21.40+08:00","event":"session_open","user":"lucas","client":"DESKTOP-ROD9JT0","remote":"local","session":"516954b3"}
{"ts":"2026-09-10T16:22:21.43+08:00","event":"port_open","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","session":"516954b3","detail":"writable"}
{"ts":"2026-09-10T16:22:25.47+08:00","event":"port_close","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","session":"516954b3","detail":"disconnected"}
```

事件类型：`session_open` `session_close` `port_open` `port_close`
`port_online` `port_offline`。

`detail` 中的 `writable` / `observe` 用于区分写权限和旁观，写入字节数也记录在此。

查询当天对 console 的操作记录：

```bash
jq -r 'select(.event=="port_open") | "\(.ts) \(.user)@\(.client) -> \(.port)"' \
  ~/.local/state/sercon/audit/audit-$(date +%F).jsonl
```

## 配置

`~/.config/sercon/config.json`，Windows 下为 `%APPDATA%\sercon\config.json`。所有
字段均可省略，完整样例见 `examples/config.json`。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `baud` | int | `115200` | 默认波特率 |
| `auto_open` | bool | `true` | 启动时打开所有已发现的端口 |
| `stamp_logs` | bool | `true` | 日志行首添加时间戳 |
| `allow_observe` | bool | `true` | 允许只读旁观 |
| `max_observers` | int | `4` | 每端口旁观者上限，0 表示不限制 |
| `scan_globs` | []string | — | Linux 上额外扫描的路径 |
| `log_dir` | string | 见上 | 覆盖日志根目录 |
| `audit_dir` | string | 见上 | 覆盖审计根目录 |
| `ports` | []object | — | 逐端口设置 |

```json
{ "ref": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0",
  "desc": "WR5220-G5-bmc",
  "baud": 115200 }
```

`desc` 是供人阅读的标签，也可直接用作 `attach` 的端口引用。`baud` 可为单个端口
覆盖全局值。

修改配置后需重启守护进程：

```bash
sercon stop -t jump     # 下次连接时自动拉起
```

## 排错

### 端口全部为 offline

Linux 上通常由权限问题引起。串口设备的权限为 `crw-rw---- root:dialout`：

```bash
ls -l /dev/ttyUSB0
id | grep -o dialout
```

若不在组内，加入后重新登录：

```bash
sudo usermod -aG dialout $USER
```

具体原因可查看 `last_err`：

```bash
sercon ls -t jump --json | jq '.[] | {ref, state, last_err}'
```

### sercond: command not found

`sercond` 不在跳板机的 PATH 中：

```bash
sercon attach -t jump --remote-bin '~/bin/sercond' FT232R
```

`~/` 必须置于引号之外，否则远程 shell 不会展开。zsh 上尤其需要注意。

### 每条命令前被追加三行 SSH 警告

跳板机的 OpenSSH 版本较旧（Ubuntu 20.04 的 8.2、22.04 的 8.9 均不支持后量子密钥
交换），其 stderr 被转发至终端。

```
Host jump
    LogLevel ERROR
```

真实错误属于 ERROR 级别，仍会正常显示。

### stop 之后端口仍在采集

socket 按用户隔离，`lucas@jump` 和 `root@jump` 是两个独立的守护进程。确认连接的是
同一台主机上的同一用户。

### 日志目录为空

先确认守护进程正在运行：

```bash
sercon status -t jump
```

再确认端口状态为 `online`；`offline` 的端口不会产生新日志。

Windows 上目录名包含下划线（`COM1_`）属于正常现象。

### Windows GUI 已启动但窗口不可见

在 SSH 会话中启动的 GUI 无法绘制窗口，必须在交互式桌面上双击启动，或置于启动
文件夹。

若进程存在但窗口未出现，查看 `<runtime>/gui.log`。

### 查看更详细的诊断信息

守护进程自身的诊断日志位于 `<runtime>/daemon.log`。客户端可开启 SSH 层日志：

```bash
sercon attach -t jump --ssh-opt LogLevel=DEBUG FT232R
```
