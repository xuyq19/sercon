# 设计说明

本文说明 sercon 各项设计决策的由来。使用方式参见 [`../README.md`](../README.md)。

## 要解决的问题

测试机与开发机不在同一网段，本地的 minicom 无法访问串口。串口设备连接在跳板机上，
链路如下：

```
开发机  ──网络──▶  跳板机  ──USB串口──▶  目标机
```

一项硬性约束：跳板机上不能安装 systemd 服务，只能通过 SSH 会话拉起进程。因此常驻
TCP 服务这一常规做法被直接排除。

## 进程形态

单个 Go 二进制，按 argv 区分三种角色：

| 命令 | 运行位置 | 作用 |
|---|---|---|
| `sercon` | 开发机 | 客户端，负责终端交互、重连、本地日志 |
| `sercond session <ref>` | 跳板机，由 SSH 拉起 | 确保守护进程存在，并将 stdio 接入 socket |
| `sercond capture` | 跳板机，分离运行 | 持有串口 fd 的守护进程 |

`sercond session` 是哑管道，不解析协议，仅做双向字节搬运。协议在 `sercon` 与
`capture` 之间端到端运行，因此守护进程的复杂度不会泄漏到 SSH 这一层。

## 没有 systemd 时如何存活

客户端执行 `ssh -T jump sercond session <ref>` 之后：

1. `session` 先探测 `<runtime>/sercon/run/s.sock`，可连接则直接使用
2. 无法连接则竞争 flock，获得锁的进程负责拉起 `sercond capture`
3. `capture` 运行在新的会话中，SSH 断开时不会收到 SIGHUP，因此继续存活
4. `session` 轮询 socket，连接建立后转为哑管道

两个平台的脱离机制不同。Linux 使用 `setsid` 创建新会话并脱离控制终端；Windows 使用
`CREATE_BREAKAWAY_FROM_JOB`。

Windows 上的做法来自实测：OpenSSH 将会话的子进程放入 job object，断连时会终止整棵
进程树。`DETACHED_PROCESS` 无法脱离 job，只有 `CREATE_BREAKAWAY_FROM_JOB`
（`0x01000000`）可以，且要求上游设置了 `JOB_OBJECT_LIMIT_BREAKAWAY_OK` 才允许使用
该标志。

单实例不依赖锁文件，由 socket 绑定本身保证——内核会拒绝同一路径的第二次 bind。

两端均使用 AF_UNIX socket（Windows 自 10 1803 起 Go 的 net 包支持）。不监听 TCP
端口，访问控制由 socket 文件的权限承担。

## 协议

```
1 byte 帧类型 | 4 bytes 长度(大端) | payload
```

`0x01` 为数据帧，承载串口原始字节。`0x02` 为控制帧，JSON 格式，通过 `op` 字段区分。

终端控制字符全部封装在带长度前缀的 payload 中，协议对串口内容完全透明。串口上传输
的内容没有限制，从 U-Boot 到 Linux console 再到任意二进制数据，任何转义或加工都会在
某处出错。

控制 op：

```
hello  welcome  list   ports   open    opened  close  closed
ping   pong     break  status  stat    notice  error  bye
shutdown
```

## 设计取舍

**端口标识使用 `/dev/serial/by-id/*`。** 内部保留 by-id 路径，每次打开前重新执行
`EvalSymlinks`。USB 重插导致 `ttyUSB` 编号漂移时，端口引用和日志连续性不受影响。

**不引入串口库，也不封装为 net.Conn 抽象。** 使用 `open(O_RDWR|O_NOCTTY|O_NONBLOCK)`
配合 termios ioctl 和 epoll 轮询，自行管理 fd 生命周期，才能在设备消失时干净地关闭
并后台重试。`net.Conn` 这类抽象无法表达串口需要的三项能力：不消费数据的读超时、
设备消失的通知、break 信号。强行套用接口只会把差异推迟到后面暴露。

**每个端口一个 reader goroutine，无条件落盘。** 日志写入不依赖是否存在客户端连接，
这正是守护进程存在的意义。

**单写多读。** 同一时刻一个 session 持有写权限，其余为只读。避免多人同时输入而扰乱
目标机的 console。观察者数量可配置（默认 4），也可完全关闭。

**每连接独立发送队列。** 慢客户端不会阻塞串口 reader。队列溢出时判定该连接已失效并
断开，不影响其他会话。

**附带 backlog。** 新连接先收到最近 64 KB 的历史输出，再切换到实时流。排障时「连接
建立之前发生了什么」往往才是关键。

**端口注册后永不删除。** 设备消失时仅标记为 `offline`。这样重插后日志文件和 backlog
保持连续，否则每次拔线都会新增一个日志目录，看起来像是更换了设备。

**锁序固定为 manager → port。** 反向获取会导致死锁。

## 两个串口后端

两侧的实现刻意保持不同。两个系统在读超时、设备消失、break 上的表达方式本就不同。

| | Linux（termios） | Windows（Win32 通信 API） |
|---|---|---|
| 打开 | `open(O_RDWR\|O_NOCTTY\|O_NONBLOCK)` | `CreateFileW("\\.\COM3")` + `FILE_FLAG_OVERLAPPED` |
| 线路配置 | termios ioctl，波特率取 Bxxx 码 | DCB + `SetCommState`，波特率直接给数值 |
| 等待数据 | `epoll_wait` 带超时 | `WaitCommEvent(EV_RXCHAR)` + 重叠事件 |
| 读超时 | 内核对 tty 的原生支持 | 没有对应物，通过取消挂起的重叠操作实现 |
| 设备消失 | `EPOLLERR\|EPOLLHUP`，读返回 EIO | `ClearCommError` 返回错误 |
| break | `ioctl(TCSBRK)`，时长由内核决定 | `SetCommBreak` 保持到 `ClearCommBreak` |
| 枚举 | `/dev/serial/by-id/*` + glob | 注册表 `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` |

Windows 侧有两点需要单独说明。

**全部使用重叠 I/O，原因不是性能。** 这是唯一能将阻塞中的读从另一个 goroutine 唤醒
的手段，而 `Close` 必须做到这一点。同步的 `ReadFile` 阻塞在串口句柄上之后无法安全
中断。

**超时为自行实现。** 重叠操作要么完成要么一直挂起，因此超时等于取消挂起的操作，并
消费掉对应的 aborted 完成事件，之后才能复用 `OVERLAPPED` 结构体。遗漏这一步会留下
一个指向「下次调用即将覆盖的结构体」的活动操作。

`\\.\` 前缀同理：缺少它时 COM10 及以上会被当作普通文件名，端口永远无法打开，而且
只有在机器串口数量较多时才会暴露。

## Windows GUI

`sercon-gui.exe` 是同一守护进程加上一个 Win32 窗口。在有人使用的实验室机器上，不可见
的后台进程不便操作：无法判断适配器是否存活、找不到日志、也不知道如何干净地停止。

主窗口显示端口列表和端口状态，底部提供四个按钮：打开日志目录、复制 attach 命令、
SSH keys、立即重扫。复制按钮将 `sercon attach -t user@host PORT` 写入剪贴板，选中
哪一行即复制哪一行。

窗口打开期间持续采集日志，关闭即停止，没有独立的开关。`sercon stop` 会实际关闭窗口。

状态列通过颜色区分：

| 颜色 | 含义 |
|---|---|
| 绿色 | 端口在线 |
| 红色 | 端口不在（拔了、被占用、打不开） |
| 琥珀色 | 有人正持有写权限 |

界面上的几项设计选择：

- 不使用网格线。满屏细线是报表型列表显得陈旧的主要原因；整行选中配合适当的纵向留白，
  可读性明显更好
- 日志列只显示文件名。目录对每个端口都相同，完整路径会在该列重复占据大部分宽度
- 侧边栏在窗口宽度不足 620px 时整体隐去，而不是压缩。压缩后的表格无法容纳各列
- 标题固定。端口数量放在统计区中，否则适配器插拔会导致标题跳动，脚本也无法按名称
  查找窗口

### 自绘客户区

客户区不使用 Static、Button 或 ListView 控件，全部通过 `WM_PAINT` 与 GDI 绘制。上一版
使用 `SysListView32`；内容为纯文本时这是合理选择，滚动、选中与键盘导航都由控件提供。
此处不再适用有两个原因：状态列需要圆角胶囊而非文字，行淡入淡出需要知道每一行的位置，
而控件不暴露这些信息。代价是选中、悬停与滚动改由本模块实现。

每次重绘先组合到内存 DC，最后一次 `BitBlt` 上屏；同时 `WM_ERASEBKGND` 返回 1 阻止系统
擦除背景。两者缺一都会在重绘时闪出上一帧的内容。

几何计算集中在 `layout_windows.go` 的 `buildLayout`，每次尺寸变化时重新计算一次，绘制与
命中测试读取同一份结果。两处各自计算同一个矩形，是控件画在一个位置而被点在另一个位置
的常见来源。

### 动画时钟

一个共享定时器驱动窗口中所有动画，而非每个元素一个。空闲时不调度任何帧：动画结束即
`KillTimer`，这是窗口挂机数天也不出现在任务管理器里的原因。

补间按墙钟计算进度（`elapsed / duration`），不累加每帧增量。累加会漂移：某一帧延迟、
或窗口被拖动导致定时器暂停时误差会累积，动画要么提前结束，要么始终差一点到位。

`anim_windows.go` 的 `animator` 每帧由调用方重建元素列表，被销毁的元素不再被询问，
不需要显式的注销步骤。显式注销的遗漏正是动画悬垂的常见来源。

配色与字体角色集中在 `theme_windows.go`。界面使用 Segoe UI，数字与路径使用 Consolas。
比例字体意味着列宽必须用 `GetTextExtentPoint32W` 实测，不能按字符数估算。

### 四个约束

**`-H=windowsgui` 是必需的。** 缺少它时链接器会产出控制台子系统程序，Windows 会为其
分配控制台窗口，双击 GUI 时会在旁边弹出一个黑色窗口。代价是失去控制台后 stderr 无处
输出，诊断信息改为写入 `<runtime>/gui.log`。窗口能出现但内容不正确时，应先查看该文件。

**GUI 必须在交互式桌面会话中启动。** SSH 会话与交互式桌面属于不同的 window station，
由 SSH 拉起的进程无法绘制窗口，表现为进程存在但界面不可见。

**主 goroutine 必须锁定在同一个 OS 线程上。** Win32 将窗口消息投递给创建该窗口的线程，
而 goroutine 随时可能切换线程。未加锁时，`WM_CREATE`（创建期间同步投递）能正常执行，
但其后的所有消息——`WM_SIZE`、`WM_TIMER`、`WM_CLOSE`——都会进入一个无人读取的队列。
窗口能显示，但永远不会刷新，也无法关闭。

**`sercon-gui.exe.manifest` 须与 exe 置于同一目录**，Windows 才会加载 comctl32 v6。
自绘部分不依赖它，但消息框和密钥面板仍使用真实控件；缺少它程序仍可运行，控件会退化为
XP 之前的绘制方式。

### 修改界面后如何查看效果

`hack/screenshot-window.py` 将窗口截取为 PNG。它使用 `PrintWindow` +
`PW_RENDERFULLCONTENT`，只渲染目标窗口自身的内容，不截取桌面上的其他内容，窗口被遮挡
时也能截取。仅依赖标准库。

```bash
./dist/sercon-gui.exe &
python hack/screenshot-window.py dist/shot.png
python hack/screenshot-window.py dist/shot.png --histogram   # 附带颜色分布
```

`--histogram` 是为排查「颜色是否生效」而添加的：小字号灰字在缩略图中难以辨认，统计颜色
比目视更可靠。

动画无法用单次截图验证，动画窗口只有 110–320ms。设置 `SERCON_ANIM_DEMO=1` 后窗口会按
定时器自行走一遍全部动画（侧边栏悬停、行悬停、按钮悬停、统计数字滚动、行淡出），每步
停留 2200ms。`hack/shoot-demo.py` 启动该模式并逐步截图，`hack/shoot-burst.py` 对单步
高频连拍以捕捉补间中间态：

```bash
python hack/shoot-demo.py dist/demo          # 每步两张，覆盖整轮
python hack/shoot-burst.py dist/burst 5 12 50  # 第 5 步连拍 12 张，间隔 50ms
```

这两个脚本在单次进程内完成启动、驱动与截图。由 shell 命令启动的窗口会在该命令的作业对象
关闭时被一并终止，拆成两条命令执行时没有窗口可供截取。

`SERCON_ANIM_TRACE=1` 会让每一帧把各动画元素的当前值写入 `gui.log`，用于区分「动画在跑
但截图恰好落在静止值上」与「动画根本没有运行」。`hack/win-probe.py` 报告窗口句柄、窗口
矩形与客户区原点；这些调用都需要显式声明返回类型，未声明时 ctypes 默认按 32 位整数读取，
在 64 位构建上会把句柄截断为零。

### 性能

窗口自身的开销按「动画期间」与「静止期间」分别衡量。`hack/cpu-slices.py` 按固定时间片
读取进程 CPU 时间，输出其分布而非单一平均值——平均值会掩盖启动时的突发与持续不断的
细流之间的差别：

```bash
python hack/cpu-slices.py 36 3                          # 静止
python hack/cpu-slices.py 36 3 dist/sercon-gui.exe --demo  # 动画演示
```

静止时窗口不调度任何帧。按 3 秒分片测量，绝大多数分片为 0.00ms。残余的活动来自守护进程
的串口轮询（`rediscoverInterval`，以及每端口 250ms 超时的读循环），在自绘界面之前就存在：
同一台机器上旧版二进制测得 234ms/36s，新版 47ms/36s。新版更省，原因是旧版每秒无条件整窗
重绘，新版仅在端口表、行集合或底部状态实际变化时重绘。

## 实现陷阱

以下问题都曾实际发生，且失败时均没有明确的报错信息。

**绘制文字前必须重设文字颜色。** GDI 的 `SetTextColor` 是设备上下文状态，`TextOutW` 使用
当前值。内容区先绘制并在 DC 上留下近黑色，侧边栏随后绘制时未重设颜色，于是在深色底上
画出了近黑色的文字。症状是侧边栏标签全部不可见，而非绘制错误——排查时容易误判为标签
没有画出来。

**`FillRect` 在 user32.dll，`TextOutW` 在 gdi32.dll。** 两者的归属与直觉相反，且写反时
直到运行到该调用才 panic，错误信息分别为
`failed to find FillRect procedure in gdi32.dll` 与
`failed to find TextOutW procedure in user32.dll`。

**`setTarget` 会把补间重置到调用时的值。** 行淡出原本从 `syncRows` 启动，而 `syncRows`
既在动画帧上运行，也在每秒一次的刷新 tick 上运行：每次 tick 都把淡出重置回整行高度，
淡出因此永远不会完成。启动动作改到绘制路径（`beginRowFade`，幂等），只有动画帧会走到。
判断一个动画是否真的会结束，要看它的启动点是否会被周期性路径重复进入。

**`SetCursorPos` 不产生 `WM_MOUSEMOVE`。** 它直接移动指针，不经过鼠标输入路径，窗口
因此收不到任何消息。用脚本扫过窗口来验证悬停动画时，测得 0.00% CPU，一度被判定为消息
未送达；加探针后确认链路本身正常。合成鼠标移动须用
`mouse_event(MOUSEEVENTF_MOVE, dx, dy, ...)`，且它是相对移动。

**由 shell 命令启动的窗口会随该命令的作业对象一同终止。** 即使传入 `DETACHED_PROCESS`
与 `CREATE_NEW_PROCESS_GROUP` 也会被回收，`CREATE_BREAKAWAY_FROM_JOB` 在本机返回
「拒绝访问」。涉及多步操作的窗口验证脚本须在单次进程内完成启动、驱动与截图。

**64 位构建下 ctypes 的默认返回类型为 32 位整数。** `FindWindowW`、`OpenProcess`、
`CreateToolhelp32Snapshot` 等返回句柄的函数若不显式声明 `restype`，句柄会被截断为零，
表现为「进程存在但查找不到窗口」或「CPU 时间恒为 0.00ms」。后者尤其危险：一个失败的
`OpenProcess` 会产出看起来完全合理的零值。

**淡出行的高度必须与实际裁剪区一致。** 行淡出时高度收缩到不足一行的位置，若不做裁剪，
行内容会画到按钮行与侧边栏底栏上。裁剪区用 `clipGuard` 成对建立与释放，漏掉释放会让
该帧后续的绘制全部被吞掉。

**`Read` 的 timeout 被忽略。** 违反了自身定义的接口契约，导致 `go test` 挂起并被
SIGTERM 终止。修法是在 `CancelIoEx` 之后必须消费对应的 aborted 完成事件。

**`sercon run -t X COM1 --send ...` 中的 flag 被忽略。** Go 的 `flag` 包在遇到
首个非 flag 参数后即停止解析。通过新增 `reorderArgs()` 将 flag 提前解决。

**`mkdir COM1` 失败，日志静默不写入。** `COM1`–`COM9` 是与 `CON`、`PRN`、`NUL`
同级的保留设备名。日志目录名转义为 `COM1_`。`COM10` 及以上不是保留名，因此该问题
是否出现取决于机器上曾使用过的适配器数量，无法通过约定规避。

**错误被 `if err == nil` 吞掉。** `portlog` 的错误存入 `lastErr` 后，又被
`goOnline()` 中的 `p.lastErr = ""` 清除；拆分为独立字段后才得以保留。

**全新机器上 `daemon.Ensure` 必然失败。** 遗漏了
`os.MkdirAll(filepath.Dir(logPath))`，直到部署到第二台机器时才暴露。

**`--remote-bin '~/bin/sercond'` 被引号破坏。** 远程 shell 为 zsh，`~` 在引号内不会
展开，必须置于引号之外。

**`git rebase --root` 将 `.git` 移入回收站。** 修改文档作者信息时触发，与 sercon
本身无关：环境的安全删除层将 `.git` 整个移动到了
`C:\$Recycle.Bin\...\$R0DIIEX.git`。恢复后改用 `git commit-tree` 重写作者，不再触碰
`.git` 结构。

## 传输方式

客户端目前只支持 SSH 一种传输方式。SSH 已经提供了认证、加密和跨网段的能力
（`ProxyJump` 稍作配置即可），在它足够使用的场景下再叠加一层自定义传输，只会增加
一份需要维护的密钥管理，以及一个可能被绕过的边界。

「局域网直连」需要区分两种不同的情况：

| | 前提 | 需要实现的内容 | 工作量 |
|---|---|---|---|
| 直连可达 | 同网段且路由互通 | TCP 监听、强制 token、审计记录来源 IP | 小 |
| 真 P2P（打洞） | 双方均位于 NAT 之后 | 信令服务器 + STUN/TURN + 中继兜底 | 高一个数量级 |

「同处 WiFi 环境应该可以连通」的场景基本属于前者。P2P 仅在隔着 NAT 时才有意义，
连接在同一台交换机下的两台机器之间没有 NAT 需要穿透。

企业 WiFi 环境中还有三项静默生效的障碍：客户端隔离（许多 AP 默认开启，导致同网段
终端之间完全不通）、处于不同的 VLAN/SSID、主机防火墙。实现之前应先花 30 秒验证：

```powershell
Test-NetConnection 10.x.x.x -Port 22
```

输出 `TcpTestSucceeded : True` 才说明该路径可用。

`internal/relay/` 中已实现中继传输，但未接入 CLI，也未在真实 NAT 环境中验证过。
在办公网中实测可以启用 OpenSSH Server 之后，这条路径暂时没有必要。

若要加入直连模式，安全模型需要随之调整。当前的边界由 socket 文件属主和前置的 SSH
共同构成；一旦监听网络端口，串口控制台即等同于暴露在整个局域网中，而串口控制台可以
进入 bootloader、修改内核 cmdline、复位机器。最低限度需要实现：强制 token（每台机器
一个）、传输加密、源地址白名单、审计记录来源 IP、默认关闭。
