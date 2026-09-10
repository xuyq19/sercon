# sercon

**串口在别人那台机器上，你在自己这台机器上，中间隔着网段和防火墙。
sercon 让你像插了根线一样用它。**

进程自己脱离 SSH 会话活下来，没人连的时候也在抓日志，所以目标机半夜重启的
boot log，第二天早上还在。

![sercon-gui](docs/images/sercon-gui.png)

零依赖。一个静态二进制丢到跳板机就能跑，不需要 root、不需要 systemd、不需要
配置文件。

---

## 30 秒跑起来

跳板机（插着串口那台）：

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond && ssh you@jump 'chmod +x ~/bin/sercond'
```

你的机器：

```bash
sercon ls -t you@jump                              # 看有哪些口
sercon attach -t you@jump --remote-bin '~/bin/sercond' FT232R   # 连上去
```

就这两条。守护进程会在你第一次连接时被拉起来，然后一直活着。

> `--remote-bin` 只在 `sercond` 不在跳板机 PATH 上时才需要。`~` 会被远程
> shell 正确展开。

---

## 命令

### `sercon` — 你这边

| 命令 | 作用 |
|---|---|
| `sercon ls -t HOST` | 列出端口：引用、设备、波特率、状态、谁占着 |
| `sercon attach -t HOST PORT` | 连上去，进原始终端 |
| `sercon run -t HOST PORT` | 脚本化会话，用来抓 boot log |
| `sercon status -t HOST` | 守护进程状态 |
| `sercon stop -t HOST` | 停掉守护进程 |
| `sercon version` | 版本 |

`PORT` 支持模糊匹配，唯一就行：

```bash
# 完整引用
sercon attach -t jump usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0
# 一样的效果
sercon attach -t jump FT232R
```

匹配到多个会报错并把候选列出来，不会替你猜。

### `attach` 常用参数

| 参数 | 作用 |
|---|---|
| `--observe` | 只读旁观，不抢写权限 |
| `--log FILE` | 本地也留一份 |
| `--no-reconnect` | 断了不自动重连 |
| `--remote-bin PATH` | 跳板机上 `sercond` 的位置 |

### Ctrl-A 快捷键

对齐 minicom 的肌肉记忆：

| 按键 | 动作 |
|---|---|
| `Ctrl-A x` / `Ctrl-A q` | 断开并退出 |
| `Ctrl-A a` | 发送一个字面量 Ctrl-A |
| `Ctrl-A l` | 开关本地日志 |
| `Ctrl-A r` | 立即重连 |
| `Ctrl-A b` | 在线上发 break |
| `Ctrl-A s` | 当前会话状态 |
| `Ctrl-A ?` | 帮助 |

### `sercond` — 跳板机那边

`sercon` 会自己调用它，一般不用手敲。手动调的场景：

| 命令 | 作用 |
|---|---|
| `sercond list [--json]` | 列出端口。`--json` 带 `last_err`，排查用 |
| `sercond status` | 守护进程状态 |
| `sercond stop` | 停掉守护进程 |
| `sercond session PORT` | 客户端调用的入口，把 stdio 接到 socket |
| `sercond capture` | 手动起守护进程（前台，不脱离） |

---

## 脚本化会话

「重启目标机并抓完整 boot log」这类活，比 shell 管道可靠——不用猜要睡多久。

```bash
sercon run -t jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

脚本语法四条：

```
send <text>      写文本，支持 \r \n \t \0 \\ \xHH
sendln <text>    写文本并追加 CRLF
wait <regex>     阻塞等正则出现
sleep 2s         暂停
```

不写脚本也行：

```bash
sercon run -t jump FT232R --send '\r' --expect 'login:' --expect '#' --timeout 30s
```

`--expect` 没等到会明确报错并退出非零，不会静默返回一个空文件：

```
sercon: --expect #1: pattern not seen before the timeout: ZZZZ_NOMATCH
```

---

## 日志在哪

守护进程无条件落盘，不看有没有人连着——这是它存在的全部理由。

| 内容 | 位置 |
|---|---|
| 串口输出 | Linux `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log`<br>Windows `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级的 `audit/audit-YYYY-MM-DD.jsonl` |
| 守护进程诊断 | `<runtime>/daemon.log` |
| socket | Linux `$XDG_RUNTIME_DIR/sercon/run/s.sock`<br>Windows `%LOCALAPPDATA%\sercon\run\s.sock` |

按天自动换文件，纯 append 从不删旧的，轮转交给 logrotate。文件开头有一行自描述
头部，记下当时的设备路径、端口引用和波特率。

审计流水长这样：

```json
{"ts":"2026-09-10T16:22:21.40+08:00","event":"session_open","user":"lucas","client":"DESKTOP-ROD9JT0","remote":"local","session":"516954b3"}
{"ts":"2026-09-10T16:22:21.43+08:00","event":"port_open","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...-port0","dev":"/dev/ttyUSB1","session":"516954b3","detail":"writable"}
{"ts":"2026-09-10T16:22:25.47+08:00","event":"port_close","user":"lucas","client":"DESKTOP-ROD9JT0","port":"usb-FTDI_FT232R_...-port0","session":"516954b3","detail":"disconnected"}
```

---

## 配置

`~/.config/sercon/config.json`（Windows `%APPDATA%\sercon\config.json`），
全部可省略。完整样例见 `examples/config.json`：

```json
{
  "baud": 115200,
  "auto_open": true,
  "ports": [
    { "ref": "usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0", "desc": "WR5220-G5-bmc" },
    { "ref": "COM3", "desc": "lab-bench-01" }
  ]
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `baud` | `115200` | 默认波特率 |
| `auto_open` | `true` | 启动时就打开所有端口，没人连也抓日志 |
| `stamp_logs` | `true` | 日志行首加时间戳 |
| `allow_observe` | `true` | 允许只读旁观 |
| `max_observers` | `4` | 每个端口的旁观者上限，0 不限 |
| `scan_globs` | — | Linux 上额外扫描的设备路径（默认走 `/dev/serial/by-id`） |
| `log_dir` / `audit_dir` | — | 覆盖默认日志位置 |
| `ports[]` | — | 逐端口固定 `ref` / `desc` / `baud` |

实际可用的 `ref` 用 `sercond list --json` 看。

---

## Windows

### 两个二进制，分工不同

| 文件 | 角色 | 谁来启动 |
|---|---|---|
| `sercon-gui.exe` | 守护进程本体，带窗口，持有串口 | 你双击，或放进启动文件夹 |
| `sercond-windows-amd64.exe` | `session` / `list` / `status` / `stop` | SSH 自动拉起 |

**两个都要放。** 只放 GUI 的话 `sercon` 连不上——SSH 需要的那几个子命令在 CLI
二进制里；只放 CLI 的话就没有窗口，得靠 `sercond capture` 手动起。
`sercon-gui.exe.manifest` 和 exe 放同一目录，缺了它控件会退化成 XP 之前的画法。

### 带窗口的意义

在一台有人坐着的实验室机器上，一个不可见的后台进程是错的形状：看不出适配器
是不是活着、找不到日志、不知道怎么干净地停掉。

窗口里是一张端口表加三个按钮：

- **Open log folder** — 资源管理器打开日志目录
- **Copy attach command** — 把 `sercon attach -t user@host PORT` 复制到剪贴板，
  选中哪行复制哪行
- **Refresh** — 立刻重扫，不等 5 秒的自动周期

窗口开着就在抓日志，关掉就停。没有单独的开关，因为「窗口可见 = 在抓」比一个
可能和实际状态不一致的复选框更不容易误判。

### GUI 必须在交互桌面会话里启动

SSH 会话与交互桌面属于不同 window station，SSH 拉起的进程即使带着这段代码也
画不出窗口——看得见进程，看不见界面。所以要么手动开，要么丢进启动文件夹
（`Win+R` → `shell:startup`）。

### 目录名会带下划线，这不是 bug

```
%LOCALAPPDATA%\sercon\ports\COM1_\2026-09-10.log
                              ^^^ 注意下划线
```

`COM1`–`COM9` 在 Windows 上是**保留设备名**，和 `CON`、`PRN`、`NUL` 一样。
`mkdir COM1` 会直接失败，报 "The directory name is invalid" 这类看不懂的错。
而串口在 Windows 上几乎总是叫 COM1–COM9 之间的名字，所以引用会被转义成
`COM1_`。日志**内容**里的头部仍写原始的 `port=COM1`，转义只影响目录名。

`COM10` 及以上不是保留名——所以这个失败会随机器上出现过的适配器数量时有时无。

### Windows 串口没有稳定标识

按当前选择直接用 COM 号。USB 重插后 COM3 变 COM5 时，旧引用标 offline 留在列表
里，新号自动开新日志。不会静默丢数据，但日志连续性会断一段。想避免的话用
配置里的 `desc` 做人工映射。

---

## Linux 部署注意

**把用户加进 `dialout` 组**，否则串口打不开：

```bash
sudo usermod -aG dialout $USER
```

串口设备是 `crw-rw---- root:dialout`。不加组的话 `sercond` 起得来、端口也枚举得
到，但每个端口都停在 `offline`。组变更要新会话才生效，改完重新登录。

排查这类问题看 `sercond list --json` 里的 `last_err`——打不开的原因写在那里，
表格输出里没有这列。

**跳板机 OpenSSH 太老的话**（Ubuntu 20.04 的 8.2、22.04 的 8.9 都没有后量子密钥
交换），每次连接会往 stderr 打三行 "store now, decrypt later" 警告，而 sercon 会
把 stderr 转发到终端，于是每条命令都被污染。在 `~/.ssh/config` 里加一行：

```
Host jump
    LogLevel ERROR
```

真实错误是 ERROR 级别的，照常显示。

---

## 构建

零第三方依赖，纯标准库。

```bash
make build          # 全平台 + 版本注入 + GUI
```

本机只要两个：

```bash
go build -o sercond ./cmd/sercond
go build -o sercon  ./cmd/sercon
```

版本号只有一个来源 `internal/version`，链接时注入，三个二进制都读它。裸
`go build` 出来显示 `devel`，一眼看出不是发布产物：

```
$ ./sercond version
sercond devel (protocol v1, go1.27.1, linux/amd64)
```

### 测试

```bash
go test ./...

# 真硬件（会占用端口并拉高 DTR/RTS，所以要显式开启）
SERCON_TEST_HARDWARE=1 SERCON_TEST_PORT=COM1 go test ./internal/serialport/ -v
```

硬件测试默认跳过。在 COM1 上接了重要东西的机器上，测试套件自作主张去抢串口
是个很坏的惊喜。

---

## 发布

**打 tag 就是发布。** `release.yml` 会构建全平台、生成校验和、创建 Release，
并校验二进制上报的版本与 tag 一致——不一致直接失败，不会发出一个说不出自己是
谁的东西。

```bash
git tag -a v0.1.0 -m "first release"
git push origin v0.1.0
```

Release 内容是 10 个二进制 + `sercon-gui.exe` + manifest + `SHA256SUMS.txt`；
源码包由 GitHub 自动附带。

`ci.yml` 在 push 到 main 和 PR 上跑：gofmt、`go vet`、`GOOS=windows go vet`、
`go test -race`、全平台构建、shell 语法检查。

---

## 文档

| 文档 | 内容 |
|---|---|
| [`docs/usage.md`](docs/usage.md) | 完整用法：每个参数、脚本语法、排错 |
| [`docs/design.md`](docs/design.md) | 为什么这么设计——协议、守护进程存活、串口后端差异 |
| [`docs/deployment.md`](docs/deployment.md) | 部署清单与真实环境验证记录 |
| [`CHANGELOG.md`](CHANGELOG.md) | 版本变更 |

---

## 验证状态

下面这些在真实环境跑过，不是「编译通过所以应该没问题」。

Ubuntu 20.04 / kernel 5.15，两个 FTDI USB 转串口，其中一个接在 BMC 串口控制台上：

| 项目 | 结果 |
|---|---|
| SSH 传输承载协议 | 通 |
| 守护进程脱离 SSH 会话 | 通 |
| termios + epoll 打开真实硬件 | 通 |
| by-id 枚举与解析 | 通 |
| 模糊匹配端口引用 | 通 |
| 读路径（BMC `ncsi-ioctl` 消息持续落盘） | 通 |
| 拔线检测 + 自动重连 | `offline` → 7 秒后 `online` |
| 拔插前后日志连续性 | 一条没断 |
| 交互式 attach | 通 |
| 审计流水 | 通 |
| Windows 串口后端（COM1） | 通 |
| Windows GUI | 通 |

**没验过的：**

- **Linux 写入路径没确认送达。** Windows 上验到「字节进了驱动」，Linux 上只验到
  写调用没报错——两个适配器都没接能回显的设备，也没有回环插头。发送方向真正
  到达目标机这一条，需要一根交叉线或一台会回话的设备。
- **Windows 拔线检测没实测。** Linux 那条验过了，Windows 侧是同样逻辑
  （`ClearCommError` 报错即判离线），但没真拔过 USB 线。

细节见 [`docs/deployment.md`](docs/deployment.md)。

## 代码结构

```
cmd/sercond/          跳板机端：capture / session / list / status / stop
cmd/sercon/           客户端：ls / attach / run / status / stop
cmd/sercon-gui/       Windows 带窗口的守护进程
internal/proto/       帧格式与控制消息
internal/serialport/  串口后端（linux / windows / other）
internal/terminal/    客户端原始终端模式
internal/winconsole/  双击启动时的窗口保活
internal/ipc/         socket 端点解析与单实例绑定
internal/daemon/      守护进程的分离启动
internal/hub/         端口状态机、广播、端口锁、backlog
internal/portlog/     按天轮转的串口日志
internal/audit/       JSONL 审计流水
internal/version/     版本号的唯一来源（链接时注入）
internal/relay/       中继传输（已实现，未接入 CLI，未在真实 NAT 上验证）
internal/config/      配置加载与默认值
```

## License

MIT
