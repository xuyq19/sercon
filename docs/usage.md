# 使用手册

完整用法。想快速上手看 [`../README.md`](../README.md)。

---

## 目录

- [前提](#前提)
- [连接参数](#连接参数)
- [`ls` — 看有哪些端口](#ls--看有哪些端口)
- [`attach` — 交互式连接](#attach--交互式连接)
- [`run` — 脚本化会话](#run--脚本化会话)
- [`status` / `stop`](#status--stop)
- [端口引用匹配](#端口引用匹配)
- [日志与审计](#日志与审计)
- [配置](#配置)
- [排错](#排错)

---

## 前提

**跳板机**上要有 `sercond`，并且你的用户在 `dialout` 组里（Linux）。

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond && sudo usermod -aG dialout $USER'
```

组变更要新会话才生效，改完重新登录一次。

**你的机器**上要有 `sercon`：

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

如果 `sercond` 不在跳板机的 PATH 上（装在 `~/bin` 时很常见），每次都要带
`--remote-bin`。嫌烦的话在 `~/.ssh/config` 里配跳板机的别名，或者把 `~/bin`
加进远程 PATH。

---

## 连接参数

四个子命令共享这一组：

| 参数 | 说明 |
|---|---|
| `-t`, `--target` | SSH 目标，`user@host`。**必填。** 也吃 `~/.ssh/config` 里的别名 |
| `--ssh-port N` | SSH 端口 |
| `--ssh-opt K=V` | 额外的 `ssh -o` 选项，可重复 |
| `--remote-bin PATH` | 跳板机上 `sercond` 的路径，默认 `sercond` |
| `--remote-socket PATH` | 覆盖守护进程 socket 路径（一般不用） |
| `--ssh-bin PATH` | 用哪个 ssh 客户端，默认 `ssh`。有多个 OpenSSH 安装时用全路径指定 |

因为 `-t` 吃 ssh 配置，所以下面这种写法是成立的：

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

要穿跳板的时候，`ProxyJump` 交给 SSH 就好，sercon 不关心。

---

## `ls` — 看有哪些端口

```bash
sercon ls -t you@jump
```

```
REF                                           DEV           BAUD    STATE   OWNER  OBS  DESC
usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0  /dev/ttyUSB1  115200  online  -      -    WR5220-G5-bmc
usb-FTDI_USB__-__Serial-if00-port0            /dev/ttyUSB0  115200  online  -      -    -

attach with: sercon attach -t <user@jump>
```

| 列 | 含义 |
|---|---|
| `REF` | 端口引用，`attach` 时用它 |
| `DEV` | 实际设备路径 |
| `BAUD` | 当前波特率 |
| `STATE` | `online` / `offline` |
| `OWNER` | 持有写权限的用户，`-` 表示没人持有 |
| `OBS` | 观察者数量 |
| `DESC` | 配置文件里的标签 |

加 `--json` 输出机器可读版本，适合脚本：

```bash
sercon ls -t jump --json | jq '.[] | select(.state=="offline") | .ref'
```

---

## `attach` — 交互式连接

```bash
sercon attach -t you@jump FT232R
```

连上后终端进入原始模式，按键直接进串口，Ctrl-C 之类不会打断 sercon 自己。
退出用 `Ctrl-A x`。

### 选项

| 选项 | 说明 |
|---|---|
| `--baud N` | 本次连接的波特率，覆盖守护进程默认值 |
| `--observe` | 只读旁观，不抢写权限 |
| `--log FILE` | 同时写一份到本地文件 |
| `--no-reconnect` | 断线直接退出，不重试 |

### 只读旁观

同事正在调 BMC，你只想看：

```bash
sercon attach -t jump FT232R --observe
```

旁观者收得到串口输出，但键盘输入不会被转发——所以不会两个人一起往 console 里
敲字。观测者数量由跳板机配置的 `max_observers` 限制（默认 4）。

### 断线重连

默认每次都自动重连，指数退避。客户端重连后会重新 `open` 原端口，终端上打一行
恢复标记，所以你能看出中间断过。

`--no-reconnect` 关掉它——脚本里想要明确失败的时候用。

### Ctrl-A 快捷键

| 按键 | 动作 |
|---|---|
| `Ctrl-A x` / `Ctrl-A q` | 断开并退出 |
| `Ctrl-A a` | 发送一个字面量 Ctrl-A |
| `Ctrl-A l` | 开关本地日志（没配 `--log` 时自动建一个） |
| `Ctrl-A r` | 立即重连 |
| `Ctrl-A b` | 在串口线上发 break |
| `Ctrl-A s` | 查看当前会话状态 |
| `Ctrl-A ?` | 帮助 |

要往目标机发一个真正的 `0x01` 时用 `Ctrl-A a`——否则会被 sercon 当成转义前缀吃掉。

---

## `run` — 脚本化会话

「重启目标机并抓完整 boot log」这类活，比 shell 管道可靠：不用猜要睡多久，
也不会因为一次 read 超时就截断。

### 直接用 `--send` / `--expect`

```bash
sercon run -t jump FT232R \
  --send '\r' \
  --expect 'login:' \
  --expect '#' \
  --timeout 30s \
  --out session.log
```

`--expect` 可重复，**按顺序匹配**。每个 `--expect` 有自己的 timeout。

### 用脚本文件

```bash
sercon run -t jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

脚本语法四条：

| 指令 | 说明 |
|---|---|
| `send <text>` | 写文本，支持 `\r` `\n` `\t` `\0` `\\` `\xHH` |
| `sendln <text>` | 写文本并追加 CRLF |
| `wait <regex>` | 阻塞等正则出现 |
| `sleep 2s` | 暂停 |

`#` 开头是注释。`wait` 的匹配起点接着上一次匹配的结尾，所以同一个模式可以连续
等两次（等第一次启动的 `login:`，再等重启后的 `login:`）。

看 `examples/reboot-capture.script` 是个完整例子。

### 选项

| 选项 | 说明 |
|---|---|
| `--script FILE` | 脚本文件 |
| `--send TEXT` | 发送一次文本，在所有 wait 之前 |
| `--expect REGEX` | 等正则出现，可重复，按顺序匹配 |
| `--timeout DUR` | 每个 wait 的超时，默认 `60s` |
| `--out FILE` | 把收到的所有数据存下来 |
| `--quiet` | 不回显到 stdout |

### 失败是有信号的

`--expect` 没等到会明确报错并**以非零退出**，不会静默返回一个空文件：

```
$ sercon run -t jump FT232R --send '\r' --expect 'ZZZZ_NOMATCH' --timeout 3s
sercond: attached to usb-FTDI_FT232R_... @ 115200 baud (writable)
sercond: console log /home/lucas/.local/state/sercon/ports/usb-FTDI_FT232R_.../2026-09-10.log
[177576.862609] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
sercon: --expect #1: pattern not seen before the timeout: ZZZZ_NOMATCH
```

这条对 CI 很重要——「重启没成功」和「脚本写错了」必须能区分开。

---

## `status` / `stop`

```bash
$ sercon status -t jump
daemon   running
socket   /run/user/1000/sercon/run/s.sock
ports    2 (2 online, 0 held)
```

```bash
sercon stop -t jump
```

`stop` 对 Windows GUI 版本也有效——窗口会真的关掉，不会只回一句就继续跑。

---

## 端口引用匹配

三种写法，从精确到宽松：

```bash
# 1. 完整 by-id 引用
sercon attach -t jump usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0

# 2. 配置里的 desc 标签
sercon attach -t jump WR5220-G5-bmc

# 3. 模糊前缀/子串
sercon attach -t jump FT232R
```

模糊匹配**只要唯一就行**。匹配到多个会报错并把候选列出来，不会替你猜：

```
sercon: "FTDI" matches 2 ports:
  usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0
  usb-FTDI_USB__-__Serial-if00-port0
```

Windows 上直接用 COM 号：`sercon attach -t winjump COM3`。

---

## 日志与审计

### 在哪

| 内容 | Linux | Windows |
|---|---|---|
| 串口输出 | `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log` | `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级的 `audit/audit-YYYY-MM-DD.jsonl` | 同左 |
| 守护进程诊断 | `<runtime>/daemon.log` | `<runtime>\daemon.log` |
| socket | `$XDG_RUNTIME_DIR/sercon/run/s.sock` | `%LOCALAPPDATA%\sercon\run\s.sock` |

`$XDG_STATE_HOME` / `$XDG_RUNTIME_DIR` 没设的话走上面列出的默认值。

### 端口日志长什么样

行首带时间戳，跨天自动换文件，纯 append 从不删旧的：

```
2026-09-10 16:24:47.915 [177721.712505] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
2026-09-10 16:24:57.576 [177731.372507] ncsi-ioctl: NCSI_SEND_CMD_GET_RESPONSE failed
```

文件开头有一行自描述头部，记下当时的设备路径、端口引用和波特率——事后翻日志时
能确认「这个文件对应哪根线」。

不想要时间戳前缀就配 `"stamp_logs": false`，这样日志可以被终端模拟器原样回放。

轮转交给 logrotate，sercon 自己不删文件。

### 审计流水

JSONL，一行一个事件：

```json
{"ts":"2026-09-10T16:22:21.32+08:00","event":"port_online","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","detail":"baud=115200"}
{"ts":"2026-09-10T16:22:21.40+08:00","event":"session_open","user":"lucas","client":"DESKTOP-ROD9JT0","remote":"local","session":"516954b3"}
{"ts":"2026-09-10T16:22:21.43+08:00","event":"port_open","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","dev":"/dev/ttyUSB1","session":"516954b3","detail":"writable"}
{"ts":"2026-09-10T16:22:25.47+08:00","event":"port_close","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...","session":"516954b3","detail":"disconnected"}
{"ts":"2026-09-10T16:22:25.47+08:00","event":"session_close","user":"lucas","client":"DESKTOP-ROD9JT0","remote":"local","session":"516954b3"}
```

事件类型：`session_open` `session_close` `port_open` `port_close`
`port_online` `port_offline`。

`detail` 里 `writable` / `observe` 区分写权限和旁观；写入的字节数也记在这里。

查询举例——今天谁动过这台机器的 console：

```bash
jq -r 'select(.event=="port_open") | "\(.ts) \(.user)@\(.client) -> \(.port)"' \
  ~/.local/state/sercon/audit/audit-$(date +%F).jsonl
```

---

## 配置

`~/.config/sercon/config.json`（Windows `%APPDATA%\sercon\config.json`），
全部可省略。完整样例 `examples/config.json`。

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `baud` | int | `115200` | 默认波特率 |
| `auto_open` | bool | `true` | 启动时就打开所有发现的端口 |
| `stamp_logs` | bool | `true` | 日志行首加时间戳 |
| `allow_observe` | bool | `true` | 允许只读旁观 |
| `max_observers` | int | `4` | 每端口旁观者上限，0 不限 |
| `scan_globs` | []string | — | Linux 上额外扫描的路径，默认走 `/dev/serial/by-id` |
| `log_dir` | string | 见上 | 覆盖日志根目录 |
| `audit_dir` | string | 见上 | 覆盖审计根目录 |
| `ports` | []object | — | 逐端口固定设置 |

`ports[]` 每项：

```json
{ "ref": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0",
  "desc": "WR5220-G5-bmc",
  "baud": 115200 }
```

`desc` 只是给人看的标签，也可以直接拿来做 `attach` 的引用（见
[端口引用匹配](#端口引用匹配)）。`baud` 为单个口覆盖全局值。

改完配置要重启守护进程才生效：

```bash
sercon stop -t jump     # 下次连接会自动拉起
```

---

## 排错

### 端口都停在 `offline`

Linux 上九成是权限。串口设备是 `crw-rw---- root:dialout`：

```bash
ls -l /dev/ttyUSB0
id | grep -o dialout
```

不在组里就加上，然后**重新登录**（组变更要新会话才生效）：

```bash
sudo usermod -aG dialout $USER
```

具体原因看 `--json` 里的 `last_err` 字段，表格输出里没有这列：

```bash
sercon ls -t jump --json | jq '.[] | {ref, state, last_err}'
```

### 连不上 / `sercond: command not found`

`sercond` 不在跳板机的 PATH 上。指路径：

```bash
sercon attach -t jump --remote-bin '~/bin/sercond' FT232R
```

`~/` 必须在引号外面，否则远程 shell 不展开它。zsh 上尤其明显。

### 每条命令前面都被塞了三行 SSH 警告

跳板机的 OpenSSH 太老（Ubuntu 20.04 的 8.2、22.04 的 8.9 都没有后量子密钥交换），
会往 stderr 打 "store now, decrypt later" 警告，而 sercon 把 stderr 转发到终端。

```
Host jump
    LogLevel ERROR
```

真实错误是 ERROR 级别的，照常显示。

### `sercon stop` 之后端口还在被抓

确认你连的是同一台机器、同一个用户。socket 路径按用户隔离，`lucas@jump` 和
`root@jump` 是两个独立的守护进程。

### 日志目录是空的

确认守护进程真的在跑：

```bash
sercon status -t jump
```

然后确认端口是 `online` —— `offline` 的端口不会有新日志。

Windows 上还有一层：如果目录名带下划线（`COM1_`），那是对的，不是 bug，见
[`../README.md`](../README.md#windows)。

### Windows GUI 打开了但没有窗口

SSH 会话里启动的 GUI 画不出窗口（window station 不同）。必须在交互桌面上双击，
或者放进启动文件夹。

窗口没起来但进程在的话，看 `<runtime>/gui.log`。

### 想看清楚到底出了什么事

守护进程自己的诊断在 `<runtime>/daemon.log`。客户端加 `--ssh-opt
LogLevel=DEBUG` 可以看到 SSH 层的动静。

```bash
sercon attach -t jump --ssh-opt LogLevel=DEBUG FT232R
```
