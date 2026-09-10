# 使用手册

完整参数说明。快速上手看 [`../README.md`](../README.md)。

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

跳板机上有 `sercond`，你的用户在 `dialout` 组里：

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond && sudo usermod -aG dialout $USER'
```

组变更要新会话生效，所以改完重新登录一次。

自己机器上有 `sercon`：

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

`sercond` 装在 `~/bin` 时它通常不在非交互式 SSH 的 PATH 上，客户端要带
`--remote-bin`。嫌烦的话把 `~/bin` 加进远程 PATH，或者配个别名。

## 连接参数

四个子命令共享：

| 参数 | 说明 |
|---|---|
| `-t, --target` | SSH 目标，`user@host`，必填。吃 `~/.ssh/config` 的别名 |
| `--ssh-port N` | SSH 端口 |
| `--ssh-opt K=V` | 额外的 `ssh -o` 选项，可重复 |
| `--remote-bin PATH` | 跳板机上 `sercond` 的路径，默认 `sercond` |
| `--remote-socket PATH` | 覆盖守护进程 socket 路径 |
| `--ssh-bin PATH` | 用哪个 ssh 客户端，默认 `ssh`。装了多个 OpenSSH 时用全路径 |

`-t` 吃 ssh 配置，所以这样写就够了：

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

要穿跳板的话 `ProxyJump` 交给 SSH，sercon 不关心。

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
| `REF` | 端口引用，`attach` 时用它 |
| `DEV` | 设备路径 |
| `BAUD` | 当前波特率 |
| `STATE` | `online` 或 `offline` |
| `OWNER` | 持有写权限的用户，`-` 表示没人持有 |
| `OBS` | 观察者数量 |
| `DESC` | 配置里的标签 |

`--json` 输出机器可读版本：

```bash
sercon ls -t jump --json | jq '.[] | select(.state=="offline") | .ref'
```

JSON 里有 `last_err` 字段，端口打不开的原因写在那儿，表格输出里没这列。

## attach

```bash
sercon attach -t you@jump FT232R
```

连上后终端进原始模式，按键直接进串口，Ctrl-C 之类不会打断 sercon。退出用
`Ctrl-A x`。

| 选项 | 说明 |
|---|---|
| `--baud N` | 本次连接的波特率，覆盖守护进程默认值 |
| `--observe` | 只读旁观，不抢写权限 |
| `--log FILE` | 本地也写一份 |
| `--no-reconnect` | 断线直接退出 |

### 只读旁观

```bash
sercon attach -t jump FT232R --observe
```

旁观者收得到串口输出，键盘输入不转发。上限由跳板机配置的 `max_observers`
控制，默认 4。

### 断线重连

默认每次都自动重连，指数退避，重连后重新 open 原端口，终端上打一行恢复标记。

脚本里想要明确失败就用 `--no-reconnect`。

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

往目标机发真正的 `0x01` 要用 `Ctrl-A a`，否则会被当成转义前缀。

## run

### 直接给参数

```bash
sercon run -t jump FT232R \
  --send '\r' \
  --expect 'login:' \
  --expect '#' \
  --timeout 30s \
  --out session.log
```

`--expect` 可重复，按顺序匹配，每个有自己的 timeout。

### 用脚本文件

```bash
sercon run -t jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

| 指令 | 说明 |
|---|---|
| `send <text>` | 写文本，支持 `\r` `\n` `\t` `\0` `\\` `\xHH` |
| `sendln <text>` | 写文本并追加 CRLF |
| `wait <regex>` | 阻塞等正则出现 |
| `sleep 2s` | 暂停 |

`#` 开头是注释。`wait` 的匹配起点接着上一次匹配的结尾，所以同一个模式能连等
两次（等第一次启动的 `login:`，再等重启后的 `login:`）。

`examples/reboot-capture.script` 是完整例子。

### 选项

| 选项 | 说明 |
|---|---|
| `--script FILE` | 脚本文件 |
| `--send TEXT` | 在所有 wait 之前发一次文本 |
| `--expect REGEX` | 等正则，可重复，按顺序 |
| `--timeout DUR` | 每个 wait 的超时，默认 `60s` |
| `--out FILE` | 存下收到的所有数据 |
| `--quiet` | 不回显到 stdout |

`--expect` 没等到会报错并以非零退出，不会静默返回空文件：

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

Windows GUI 版本收到 stop 会真的关窗口。

## 端口引用匹配

```bash
# 完整 by-id 引用
sercon attach -t jump usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0

# 配置里的 desc
sercon attach -t jump WR5220-G5-bmc

# 能唯一识别的前缀
sercon attach -t jump FT232R
```

匹配到多个会报错并列出候选，不会替你猜：

```
sercon: "FTDI" matches 2 ports:
  usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0
  usb-FTDI_USB__-__Serial-if00-port0
```

Windows 上直接用 COM 号：`sercon attach -t winjump COM3`。

## 日志与审计

### 位置

| 内容 | Linux | Windows |
|---|---|---|
| 串口输出 | `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log` | `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级 `audit/audit-YYYY-MM-DD.jsonl` | 同左 |
| 守护进程诊断 | `<runtime>/daemon.log` | `<runtime>\daemon.log` |
| socket | `$XDG_RUNTIME_DIR/sercon/run/s.sock` | `%LOCALAPPDATA%\sercon\run\s.sock` |

`$XDG_STATE_HOME` 和 `$XDG_RUNTIME_DIR` 没设的话走上面的默认值。

### 端口日志

```
2026-09-10 16:24:47.915 [177721.712505] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
```

按天换文件，只追加。文件开头有一行头部，记下当时的设备路径、端口引用和波特率，
事后能确认这个文件对应哪根线。轮转交给 logrotate，sercon 不删文件。

`stamp_logs: false` 可以去掉时间戳前缀，这样日志能被终端模拟器原样回放。

### 审计流水

JSONL，一行一个事件：

```json
{"ts":"2026-09-10T16:22:21.32+08:00","event":"port_online","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","detail":"baud=115200"}
{"ts":"2026-09-10T16:22:21.40+08:00","event":"session_open","user":"lucas","client":"DESKTOP-ROD9JT0","remote":"local","session":"516954b3"}
{"ts":"2026-09-10T16:22:21.43+08:00","event":"port_open","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","session":"516954b3","detail":"writable"}
{"ts":"2026-09-10T16:22:25.47+08:00","event":"port_close","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","session":"516954b3","detail":"disconnected"}
```

事件类型：`session_open` `session_close` `port_open` `port_close`
`port_online` `port_offline`。

`detail` 里 `writable` / `observe` 区分写权限和旁观，写入字节数也记在这儿。

查询今天谁动过 console：

```bash
jq -r 'select(.event=="port_open") | "\(.ts) \(.user)@\(.client) -> \(.port)"' \
  ~/.local/state/sercon/audit/audit-$(date +%F).jsonl
```

## 配置

`~/.config/sercon/config.json`，Windows 是 `%APPDATA%\sercon\config.json`。全部
可省略，完整样例 `examples/config.json`。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `baud` | int | `115200` | 默认波特率 |
| `auto_open` | bool | `true` | 启动就打开所有发现的端口 |
| `stamp_logs` | bool | `true` | 日志行首加时间戳 |
| `allow_observe` | bool | `true` | 允许只读旁观 |
| `max_observers` | int | `4` | 每端口旁观者上限，0 不限 |
| `scan_globs` | []string | — | Linux 上额外扫描的路径 |
| `log_dir` | string | 见上 | 覆盖日志根目录 |
| `audit_dir` | string | 见上 | 覆盖审计根目录 |
| `ports` | []object | — | 逐端口设置 |

```json
{ "ref": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0",
  "desc": "WR5220-G5-bmc",
  "baud": 115200 }
```

`desc` 是给人看的标签，也能直接当 `attach` 的引用。`baud` 为单个口覆盖全局值。

改完配置要重启守护进程：

```bash
sercon stop -t jump     # 下次连接自动拉起
```

## 排错

### 端口全是 offline

Linux 上基本是权限问题。串口设备是 `crw-rw---- root:dialout`：

```bash
ls -l /dev/ttyUSB0
id | grep -o dialout
```

不在组里就加上，然后重新登录：

```bash
sudo usermod -aG dialout $USER
```

具体原因看 `last_err`：

```bash
sercon ls -t jump --json | jq '.[] | {ref, state, last_err}'
```

### sercond: command not found

不在跳板机的 PATH 上：

```bash
sercon attach -t jump --remote-bin '~/bin/sercond' FT232R
```

`~/` 要留在引号外面，否则远程 shell 不展开它。zsh 上尤其明显。

### 每条命令前面被塞了三行 SSH 警告

跳板机 OpenSSH 太老（Ubuntu 20.04 的 8.2、22.04 的 8.9 都没有后量子密钥交换），
stderr 被转发到终端了。

```
Host jump
    LogLevel ERROR
```

真实错误是 ERROR 级别的，照常显示。

### stop 之后端口还在被抓

socket 按用户隔离，`lucas@jump` 和 `root@jump` 是两个独立的守护进程。确认连的是
同一台机器上的同一个用户。

### 日志目录是空的

先确认守护进程在跑：

```bash
sercon status -t jump
```

再确认端口是 `online`，`offline` 的端口没有新日志。

Windows 上目录名带下划线（`COM1_`）是正常的。

### Windows GUI 打开了但没有窗口

SSH 会话里启动的 GUI 画不出窗口，必须在交互桌面上双击，或者放进启动文件夹。

窗口没起来但进程在的话看 `<runtime>/gui.log`。

### 想看更多诊断

守护进程自己的诊断在 `<runtime>/daemon.log`。客户端可以打开 SSH 层日志：

```bash
sercon attach -t jump --ssh-opt LogLevel=DEBUG FT232R
```
