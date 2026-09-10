# sercon

跨网段串口控制台工具。串口物理上挂在跳板机，你从自己的机器上通过 SSH 访问它。

替代「跑过去插串口线开 minicom」——尤其是当目标机和你的开发机不在同一网段、
而跳板机又不允许装常驻服务的时候。

## 为什么不是简单的 socat 转发

`socat` / `ser2net` 能做到把串口暴露成 TCP，但它们解决不了这个场景里真正麻烦的四件事：

| 需求 | 纯转发的问题 | 这里的做法 |
|---|---|---|
| 无人值守抓日志 | 没人连就不抓 | 守护进程无条件落盘，目标机半夜重启的 boot log 早上还在 |
| 断线自动重连 | 断了要手动重开，目标机重启后 `ttyUSB` 编号还会漂 | 服务端按 `by-id` 重开，客户端指数退避重连 |
| 多人不互相踩 | 谁都能往同一条线上敲字 | 单写多读，一个 owner 持有写权限，其余只读观察 |
| 事后追溯 | 什么都没有 | JSONL 审计：谁、什么时候、连了哪个口、敲了多少字节 |

## 架构

```
你的开发机                      跳板机                        目标机
┌──────────────┐  SSH stdio  ┌──────────────┐   USB 串口  ┌──────────┐
│ sercon         │────────────▶│ sercond      │───────────▶│ 串口控制台│
│  · 原始终端   │  TCP+token  │  · 会话 Hub  │            │          │
│  · 断线重连   │             │  · 端口日志   │            │          │
│  · 本地日志   │             │  · 审计流水   │            │          │
└──────────────┘             └──────────────┘            └──────────┘
```

一个 Go 二进制三个角色，按 argv 区分：

- `sercond capture` — 真正持有串口 fd 的守护进程
- `sercond session` — SSH 拉起的哑管道，只搬运字节，不理解协议
- `sercon` — 客户端，协议在它和守护进程之间端到端跑

Windows 上还有第四个：`sercon-gui.exe`，同一个守护进程加上一个原生
Win32 窗口（见下）。

协议不泄漏到 SSH 那一层，所以守护进程的复杂度不会影响传输。

### 没有 systemd 怎么活

守护进程由第一个需要它的客户端拉起，并且脱离 SSH 会话存活：

- **Linux**：`setsid` 新会话 + 无控制终端 → 收不到 SIGHUP
- **Windows**：`CREATE_BREAKAWAY_FROM_JOB`。Windows OpenSSH 把会话放进 job object，
  断连时整棵树 kill；`DETACHED_PROCESS` 救不了你，只有 breakaway 能出 job。
  上游专门设了 `JOB_OBJECT_LIMIT_BREAKAWAY_OK` 允许这个标志。

单实例靠 socket 绑定本身保证：内核拒绝同一路径的第二次 bind，不需要额外的锁文件。

### 会话通道

两端都用 AF_UNIX socket（Windows 从 10 1803 起支持，Go 的 net 包直接驱动）。
不监听任何 TCP 端口；访问控制就是 socket 的文件权限 / 目录 ACL。

## Windows：带窗口的守护进程

`sercon-gui.exe` 做的是同一件事，但把状态显示出来。在一台有人坐着的实验室
机器上，一个不可见的后台进程是错的形状：看不出适配器是不是活着、找不到日志、
也不知道怎么干净地停掉它。

窗口里是一张端口表（引用 / 状态 / 持有者 / 设备 / 波特率 / 观察者 / 日志路径 /
最近错误），下面三个按钮：

- **Open log folder** — 在资源管理器里打开日志目录
- **Copy attach command** — 把 `sercon attach -t user@host PORT` 复制到剪贴板，
  选中的是哪一行就复制哪一行。这是这个窗口能给出的最有用的一样东西
- **Refresh** — 立刻重扫一次，不用等 5 秒的自动扫描周期

窗口开着就在抓日志，关掉就停。没有单独的开关，因为「窗口可见 = 在抓」比一个
可能和实际状态不一致的复选框更不容易误判。

客户端执行 `sercond stop` 也会真的关掉这个窗口，不会只回一个「shutting down」
然后继续跑。

### 界面

顶部一行大号标题加一行灰色摘要（`2 ports · 2 online · 0 writable · 0 observing`），
中间是端口表，底部三个按钮加一行灰色状态栏（显示 socket 路径，或刚执行完的操作用
来反馈）。

状态列按颜色区分，这是扫一眼就能得到答案的三个问题：

| 颜色 | 含义 |
|---|---|
| 绿色 | 端口在线 |
| 红色 | 端口不在（拔了、被占用、打不开） |
| 琥珀色 | 有人正持有写权限 |

几个刻意的选择：

- **不加网格线。** 满屏细线是报表型 ListView 显得老气最主要的原因。整行选中加
  一点纵向留白读起来好得多，也不花成本
- **行高 26px。** ListView 的行高等于它小图标列表的高度，所以塞一个空的小图标
  列表是唯一受支持的行内留白手段
- **窗口背景纯白**，标题和状态栏走 `WM_CTLCOLORSTATIC` 单独设色。默认的
  `COLOR_BTNFACE` 灰底会让整个窗口像个 2001 年的设置对话框
- **固定标题。** 端口数量放在摘要行里，不放在标题栏——否则适配器一插一拔窗口
  标题就跳，脚本也没法按名字找到窗口

### 编译时的一个必须项

`-H=windowsgui` 不是可选的美化选项。少了它链接器产出的是**控制台子系统**程序，
Windows 会给它分配一个控制台窗口——双击 GUI 就会在旁边冒出一个黑窗口。

```bash
CGO_ENABLED=0 GOOS=windows go build -ldflags="-s -w -H=windowsgui" -o sercon-gui.exe ./cmd/sercon-gui
```

副作用是没有控制台之后 stderr 无处可去，所以诊断信息写到
`<runtime>/gui.log`。窗口出得来但内容不对时先看这个文件。

### 两个必须知道的约束

**GUI 必须在交互桌面会话里启动。** SSH 会话和交互桌面属于不同的 window
station，SSH 拉起的进程即使带着这段代码也画不出窗口——看得见进程，看不见界面。
所以它要么手动开，要么丢进启动文件夹：

```
shell:startup
```

**主 goroutine 必须锁在同一个 OS 线程上。** Win32 把窗口消息投递给创建该窗口的
线程，而 Go 的 goroutine 随时可能换一个 OS 线程继续跑。不锁的话会出现一种很像
「程序坏了」的现象：`WM_CREATE`（创建期间同步投递）正常执行，而之后所有消息
——`WM_SIZE`、`WM_TIMER`、`WM_CLOSE`——都进了一个没人读的队列。窗口能显示，
但里面永远不刷新，也关不掉。代码里的 `runtime.LockOSThread()` 就是为这个。

`sercon-gui.exe.manifest` 要和 exe 放在同一目录，Windows 才会加载 comctl32 v6
并用上现代控件样式；缺了它程序照跑，只是控件退化成 XP 之前的画法。

### 改界面时怎么看效果

`hack/screenshot-window.py` 把窗口内容抓成 PNG，用于迭代界面。

```bash
./dist/sercon-gui.exe &
python hack/screenshot-window.py dist/shot.png            # 出图
python hack/screenshot-window.py dist/shot.png --histogram  # 附加颜色分布
```

它用 `PrintWindow` + `PW_RENDERFULLCONTENT`，只把目标窗口自己的内容渲染进位图：
不会截到桌面上的其他东西，窗口被遮挡时也能正确抓取。纯标准库（ctypes + zlib）。

`--histogram` 那个模式是在排查"颜色到底生效了没"时加的——小字号灰字在缩略图里
看不出来，数颜色比看眼睛可靠。

## 协议

```
1 byte 帧类型 | 4 bytes 长度(大端) | payload
```

- `0x01` 数据帧 — 串口原始字节，不做任何加工
- `0x02` 控制帧 — JSON

终端控制字符全部装在带长度前缀的 payload 里，所以协议对串口内容完全透明。
控制 op：`hello` `welcome` `list` `ports` `open` `opened` `close` `closed`
`ping` `pong` `break` `status` `stat` `notice` `error` `bye` `shutdown`

## 部署

跳板机只需要一个二进制，不需要 root，不需要配置文件也能跑：

```bash
# Linux 跳板机
scp dist/sercond-linux-amd64 lucas@jump:~/bin/sercond && ssh lucas@jump 'chmod +x ~/bin/sercond'

# Windows 跳板机（装 OpenSSH Server 后，用于 sercon 远程接入）
scp dist/sercond-windows-amd64.exe lucas@winjump:sercond.exe

# Windows 跳板机、想直接在机器上用：带窗口的版本
#   sercon-gui.exe 和 sercon-gui.exe.manifest 放同一个目录，然后双击
scp dist/sercon-gui.exe dist/sercon-gui.exe.manifest lucas@winjump:
```

**Linux 上还要把用户加进 `dialout` 组**，否则串口设备打不开：

```bash
sudo usermod -aG dialout lucas
```

串口设备是 `crw-rw---- root:dialout`，不加组的话 `sercond` 起得来、端口也枚举得
到，但每个端口都会停在 `offline`。组变更需要新会话才生效，所以改完要重新登录
（或重新开一个 SSH 连接）。

排查这类问题看 `sercond list --json` 里的 `last_err` 字段——端口打不开的原因会
写在那里，表格输出里没有这一列。

如果 `sercond` 装在 `~/bin` 而那个目录不在 PATH 上（常见），用 `--remote-bin`
指过去即可，`~` 会被远程 shell 正确展开：

```bash
sercon attach -t lucas@jump --remote-bin '~/bin/sercond' FT232R
```

**如果跳板机的 OpenSSH 比较老**（Ubuntu 22.04 的 8.9 就没有后量子密钥交换），
每次连接会往 stderr 打三行 "store now, decrypt later" 警告，而 sercon 会把 stderr
转发到终端，于是每条命令都被污染。在 `~/.ssh/config` 里给这台机器加一行就好：

```
Host jump
    LogLevel ERROR
```

真实错误是 ERROR 级别的，照常显示。

Windows 跳板机上**两个都要放**，它们分工不同：

| 文件 | 角色 | 谁来启动 |
|---|---|---|
| `sercon-gui.exe` | 守护进程本体，带窗口，持有串口 | 你双击，或放进启动文件夹 |
| `sercond-windows-amd64.exe` | `session` / `list` / `status` / `stop` | SSH 自动拉起 |

只放 GUI 的话 `sercon` 连不上，因为 SSH 需要的那几个子命令在 CLI 那个二进制里；
只放 CLI 的话就没有窗口，得靠 `sercond capture` 手动起。

`sercond stop` 对两者都有效——GUI 收到请求会真的把窗口关掉。

本机放客户端：

```bash
install -m755 dist/sercon-linux-amd64 ~/.local/bin/sercon
```

## 使用

```bash
# 看有哪些口、谁占着
sercon ls -t lucas@jump

# 连上去
sercon attach -t lucas@jump usb-FT232R

# 只读旁观同事的会话，不抢写权限
sercon attach -t lucas@jump usb-FT232R --observe

# 顺手在本地也留一份
sercon attach -t lucas@jump usb-FT232R --log bench01.log

# 守护进程状态 / 停掉它
sercon status -t lucas@jump
sercon stop   -t lucas@jump
```

端口引用支持模糊匹配：`usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0` 可以直接写
`FT232R`，只要唯一就行。匹配到多个会报错并列出候选。

### Ctrl-A 快捷键

对齐 minicom 的肌肉记忆：

| 按键 | 动作 |
|---|---|
| `Ctrl-A x` / `Ctrl-A q` | 断开并退出 |
| `Ctrl-A a` | 发送一个字面量 Ctrl-A |
| `Ctrl-A l` | 开关本地日志（没配 `--log` 时自动建一个） |
| `Ctrl-A r` | 立即重连 |
| `Ctrl-A b` | 在串口线上发 break |
| `Ctrl-A s` | 查看当前会话状态 |
| `Ctrl-A ?` | 帮助 |

### 脚本化会话

用于「重启目标机并抓完整 boot log」这类活，比 shell 管道可靠：

```bash
sercon run -t lucas@jump FT232R \
  --script examples/reboot-capture.script \
  --out bench01-boot.log --timeout 90s
```

脚本语法就四条：

```
send <text>      写文本，支持 \r \n \t \0 \\ \xHH 转义
sendln <text>    写文本并追加 CRLF
wait <regex>     阻塞等正则出现（匹配起点接着上一次匹配的结尾）
sleep 2s         暂停
```

也可以不用脚本：

```bash
sercon run -t jump FT232R --send '\r' --expect 'login:' --expect '#' --timeout 30s
```

## 日志在哪

| 内容 | 位置 |
|---|---|
| 串口输出 | Linux `~/.local/state/sercon/ports/<port>/YYYY-MM-DD.log`<br>Windows `%LOCALAPPDATA%\sercon\ports\<port>\YYYY-MM-DD.log` |
| 审计流水 | 同级的 `audit/audit-YYYY-MM-DD.jsonl` |
| 守护进程自身诊断 | `<runtime>/daemon.log` |
| socket | Linux `$XDG_RUNTIME_DIR/sercon/run/s.sock`<br>Windows `%LOCALAPPDATA%\sercon\run\s.sock` |

端口日志行首带时间戳，跨天自动换文件，纯 append 不删旧文件——轮转交给 logrotate。
日期文件开头有一行自描述头部，记录了当时的设备路径、端口引用和波特率。

### Windows 上目录名会带下划线，这不是 bug

```
%LOCALAPPDATA%\sercon\ports\COM1_\2026-09-10.log
                              ^^^ 注意这个下划线
```

Windows 把 `COM1` 到 `COM9` 当作**保留设备名**，和 `CON`、`PRN`、`NUL` 一样。
它们是设备而不是普通名字，所以 `mkdir COM1` 会直接失败，报的还是
"The directory name is invalid" 或者更难懂的 "The system cannot find the path
specified"。而串口在 Windows 上几乎总是叫 COM1 到 COM9 之间的某个名字。

所以端口引用会被转义：`COM1` → `COM1_`。日志文件**内容**里的头部仍然写原始的
`port=COM1`，转义只影响目录名。

顺带一个容易踩的陷阱：**`COM10` 及以上不是保留名**。所以这个失败会随着机器上
出现过的适配器数量时有时无——这也是不能靠约定来规避、必须在代码里处理的原因。

## 配置

`~/.config/sercon/config.json`（Windows 是 `%APPDATA%\sercon\config.json`），
全部可省略。参考 `examples/config.json`：

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

`desc` 只是给人看的标签；`baud` 可以为单个口覆盖全局值。
用 `sercond list --json` 可以看到实际的 `ref`。

## 两个串口后端

两者故意长得不一样，因为两个系统对"读超时""设备消失""break"的表达方式本就
不同。硬套一个 `io.ReadWriter` 抽象只会把差异藏到后面去。

| | Linux（termios） | Windows（Win32 通信 API） |
|---|---|---|
| 打开 | `open(O_RDWR\|O_NOCTTY\|O_NONBLOCK)` | `CreateFileW("\\.\COM3")` + `FILE_FLAG_OVERLAPPED` |
| 线路配置 | termios ioctl（TCGETS/TCSETS），波特率取固定表里的 Bxxx 码 | DCB + `SetCommState`，波特率直接给数值，任意值都行 |
| 等待数据 | `epoll_wait` 带超时 | `WaitCommEvent(EV_RXCHAR)` + 重叠事件 |
| 读超时 | 内核对 tty 的原生支持 | **没有对应物**：超时要靠取消挂起的重叠操作实现 |
| 设备消失 | `EPOLLERR\|EPOLLHUP`，读返回 EIO | `ClearCommError` 返回错误 |
| break | `ioctl(TCSBRK)`，时长由内核定 | `SetCommBreak` 保持到 `ClearCommBreak`，时长自己定 |
| 枚举 | `/dev/serial/by-id/*` + glob | 注册表 `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` |

Windows 侧值得一提的两点：

**全部用重叠 I/O，但不是为了性能。** 这是唯一能把阻塞中的读从另一个 goroutine
唤醒的手段——`Close` 必须做到这件事。同步的 `ReadFile` 一旦阻塞在串口句柄上就
没法安全中断。

**超时是自己实现的。** 重叠操作要么完成、要么一直挂着。所以超时 = 取消挂起的
操作 + 消费掉那个 aborted 完成事件，然后才能复用 `OVERLAPPED` 结构体。少这一步
就会留下一个指向"下一次调用即将覆盖的结构体"的活动操作。

`\\.\` 前缀也不是装饰：少了它，COM10 及以上会被当成普通文件名，端口永远打不开，
而且只在机器串口够多的时候才暴露出来。

### 测试

```bash
go test ./...

# 在真硬件上跑（会占用端口并拉高 DTR/RTS，所以要显式开启）
SERCON_TEST_HARDWARE=1 SERCON_TEST_PORT=COM1 go test ./internal/serialport/ -v
```

硬件测试默认跳过。在 COM1 上接了重要东西的机器上，测试套件自作主张去抢串口
是个很不好的惊喜。

## 传输方式：为什么现在只有 SSH

现阶段客户端只有一条路：SSH 到跳板机（或到插着串口的那台机器）。
这不是偷懒，是因为 SSH 已经免费给了三样东西——认证、加密、以及穿过网段的
能力（`~/.ssh/config` 里配好 ProxyJump 就行）。在它已经够用的地方再叠一层
自定义传输，只会多一份要维护的密钥管理和多一个可以被绕过的边界。

### 「局域网直连」需要区分两件不同的事

| | 前提 | 需要做什么 | 工作量 |
|---|---|---|---|
| **直连可达** | 两台机器在同一网段且路由互通 | 加一个 TCP 监听 + 强制 token + 审计记录来源 IP | 小 |
| **真 P2P（打洞）** | 双方都在 NAT 后面 | 信令服务器 + STUN/TURN + 打洞失败的中继兜底 | 另一个数量级 |

绝大多数「都是 WiFi 应该能连上」的场景其实是第一种。P2P 那套东西只有在中间
隔着 NAT 的时候才有意义——同一台交换机下面的两台机器之间没有 NAT 要穿。

### 但「都在 WiFi 上」不等于能直连

企业 WiFi 上最常见的三个拦路石，而且都是静默生效：

1. **客户端隔离（AP isolation）** — 很多企业 AP 默认打开，同网段终端之间
   完全不通。ping 也好、TCP 也好，全被 AP 丢掉。这是最常见的那个
2. **不同 VLAN / SSID** — 办公 SSID 和测试 SSID 分开是常态
3. **主机防火墙** — Windows 默认拦掉入站的未放行端口

所以先花 30 秒验证，再决定要不要写代码。在办公室那台机器上：

```powershell
# 从你笔记本打过去，看目标机器上有没有能开的端口
Test-NetConnection 10.x.x.x -Port 22
```

`TcpTestSucceeded : True` 才说明这条路存在。如果连 IP 都 ping 不通，先找网管
问客户端隔离的事，别急着写 P2P。

### 真要加直连模式，安全模型必须跟着变

现在的权限边界是「socket 文件属主 + SSH 在前面挡着」。一旦监听网络端口，
串口控制台就等于挂在整个局域网上——而串口控制台能进 bootloader、能改内核
cmdline、能 reset 机器。最低限度：

- 强制 token，每台机器一个，不要所有机器共用一个
- 传输加密（TLS 或 Noise），否则同网段任何人抓包就能看到 console 内容
- 源地址白名单
- 审计里记下来源 IP 而不是只记用户名
- 默认关闭，必须显式打开

## 验证状态

以下都在真实环境里跑过，不是"编译通过所以应该没问题"。

在一台 Linux 机器上（Ubuntu 22.04 / kernel 5.15，两个 FTDI USB 转串口，
其中一个接在一台 BMC 的串口控制台上）：

| 项目 | 怎么验的 | 结果 |
|---|---|---|
| SSH 传输承载协议 | 客户端在 A 机、守护进程在 B 机，真 ssh | 通 |
| 守护进程脱离 SSH 会话 | `sercond session < /dev/null` 立刻退出后查 `status` | 守护进程还活着 |
| Linux termios + epoll 打开真实硬件 | 两个 FTDI 适配器都进 `online` | 通 |
| by-id 枚举与解析 | `list` 列出两个 by-id 名字并解析到 ttyUSB0/1 | 通 |
| 模糊匹配端口引用 | `sercon attach` 只写 `usb-FTDI_FT232R` 前缀 | 命中唯一端口 |
| **读路径（真实数据）** | BMC 的 `ncsi-ioctl` 内核消息持续落盘 | 通 |
| **拔线检测 + 自动重连** | sysfs 解绑 USB 接口模拟拔线，再绑定 | `offline` → 7 秒后 `online` |
| **日志连续性** | 上面的拔插前后检查同一个文件 | 一条没断，同一个文件 |
| 交互式 attach | 真 TTY 上把 BMC 串口实时输出打到屏幕 | 通 |
| 审计流水 | 会话/占用/释放/离线/上线全程记录 | 通 |
| 优雅停止 | `sercond stop` | 通 |
| Windows 串口后端 | 本机 COM1 上 open/read/close 反复 | 通 |
| Windows GUI | 窗口、着色、远端 stop 关窗 | 通 |

没验过的：

**写入路径在 Linux 上没确认送达。** Windows 上验到"字节进了驱动"（审计
`bytes=N`），Linux 上只验到写调用没报错——两个 FTDI 适配器都没接能回显的设备，
也没有回环插头。发送方向真正到达目标机这一条，需要一根交叉线或一台会回话的设备。

**Windows 上的拔线检测没实测。** Linux 那条路径验过了，Windows 侧是同样的逻辑
（`ClearCommError` 返回错误即判离线），但没真拔过 USB 线。

## 已知限制

**Windows 上串口没有稳定标识。** 按当前选择直接用 COM 号：USB 重插后 COM3 变成
COM5 时，旧引用会标 offline 留在列表里，新号自动开新日志。不会静默丢数据，
但日志连续性会断一段。想避免的话用 `examples/config.json` 里的 `desc` 做人工映射。

**日志目录名在 Windows 上带下划线。** 见上文「两个串口后端」下的说明：`COM1` 是
保留设备名，所以要转义成 `COM1_`。

## 构建

零第三方依赖，纯标准库。

**版本号只有一个来源：`internal/version`**，链接时用 ldflags 注入，三个二进制都从
那里读。代码里任何 `const version = "..."` 都已删除——那正是打 tag 时会漏掉的地方。

```bash
# 全平台 + 版本注入（Linux / CI 上最省事）
make build

# 只要本机的两个二进制
go build -o sercond ./cmd/sercond
go build -o sercon ./cmd/sercon

# 手动注入版本（等价于 make build 做的事）
MODULE=$(go list -m)
go build -ldflags "\
  -X $MODULE/internal/version.Version=$(git describe --tags --always --dirty) \
  -X $MODULE/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X $MODULE/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o sercond ./cmd/sercond
```

`make build` 构建 5 个平台 × 2 个二进制，外加 `sercon-gui.exe`（Windows x64，并把
manifest 一起复制到 `dist/`）。

注入过版本的二进制会自报家门：

```
$ ./sercond version
sercond v0.1.0+db5b9b7 (protocol v1, go1.27.1, linux/amd64)
```

裸 `go build` 出来的显示 `devel`——一眼就能看出它不是发布流程产出的东西。

### 这台 Windows 机器上的注意事项

`make` 和 `go` 都不在 PATH 上（Go 装在隔离目录里，`make` 根本没装）。本地构建要么
用完整的 go 路径，要么先补一个 make，比如 `winget install GnuWin32.Make`。

## 版本与发布

版本走 SemVer，tag 形如 `v0.1.0`。**打 tag 就是发布**：

```bash
git tag -a v0.1.0 -m "first release"
git push origin v0.1.0
```

`release.yml` 在 `v*` tag 上构建全部平台、生成校验和、创建 Release。它还会校验
二进制上报的版本号和 tag 一致，不一致就直接失败——不会发出一个说不出自己是谁的二进制。

Release 里会有：

| 内容 | 来源 |
|---|---|
| `sercond-{linux,windows,darwin}-{amd64,arm64}` | 交叉编译，5 个平台 |
| `sercon-*` 同上 | 交叉编译 |
| `sercon-gui.exe` + `sercon-gui.exe.manifest` | Windows GUI |
| `SHA256SUMS.txt` | `sha256sum` |
| Source code (zip / tar.gz) | **GitHub 自动附带**，不需要额外步骤 |

`ci.yml` 在 push 到 main 和 PR 上跑：gofmt、`go vet`、`GOOS=windows go vet`、`go test
-race`、全平台构建、shell 脚本语法检查。

其中 **`GOOS=windows go vet` 那步不能省**：linux 上的 vet 会静默跳过所有带 windows
构建约束的文件，而这棵树里差不多三分之一是 windows-only 的，最容易出问题的那部分
正好在里面。

## 代码结构

```
cmd/sercond/          跳板机端：capture / session / list / status / stop
cmd/sercon-gui/      Windows 带窗口的守护进程（Win32 绑定 + 窗口逻辑）
cmd/sercon/             客户端：ls / attach / run / status / stop
internal/proto/       帧格式与控制消息
internal/serialport/  串口后端（linux 已实现，windows 待补）
internal/terminal/    客户端原始终端模式（linux / windows）
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
