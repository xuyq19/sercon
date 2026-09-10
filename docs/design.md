# 设计说明

这份文档解释 sercon 为什么长成现在这样。想直接用的话看
[`../README.md`](../README.md)，那是入口。

---

## 要解决的问题

测试机和开发机不在同一网段，本地 minicom 够不到串口。串口物理上挂在跳板机：

```
你的机器  ──网络──▶  跳板机  ──USB串口──▶  目标机
```

约束是硬的：**跳板机不能装 systemd 服务**，只能靠 SSH 会话拉起。这一条推翻了
「常驻 TCP 服务」的常规做法。

---

## 为什么不是 socat 转发

`socat` / `ser2net` 能把串口暴露成 TCP，但它们解决不了这个场景里真正麻烦的
四件事：

| 需求 | 纯转发的问题 | sercon 的做法 |
|---|---|---|
| 无人值守抓日志 | 没人连就不抓 | 守护进程无条件落盘，目标机半夜重启的 boot log 早上还在 |
| 断线自动重连 | 断了要手动重开，目标机重启后 `ttyUSB` 编号还会漂 | 服务端按 `by-id` 重开，客户端指数退避重连 |
| 多人不互相踩 | 谁都能往同一条线上敲字 | 单写多读，一个 owner 持有写权限，其余只读观察 |
| 事后追溯 | 什么都没有 | JSONL 审计：谁、什么时候、连了哪个口、敲了多少字节 |

---

## 形态：一个二进制三个角色

按 argv 区分：

| 命令 | 运行位置 | 作用 |
|---|---|---|
| `sercon` | 你的机器 | 客户端，终端交互、重连、本地日志 |
| `sercond session <ref>` | 跳板机（SSH 拉起） | 确保守护进程存在，把 stdio 接到 socket |
| `sercond capture` | 跳板机（分离运行） | 真正持有串口 fd 的守护进程 |

关键点：**`sercond session` 是个哑管道**，不理解协议，只做双向字节搬运。协议在
`sercon` 和 `capture` 之间端到端跑，所以守护进程的复杂度不会泄漏到 SSH 这一层。

Windows 上还有第四个：`sercon-gui.exe`，同一个守护进程加一个原生 Win32 窗口。

---

## 没有 systemd 怎么活下来

1. 客户端执行 `ssh -T jump sercond session <ref>`
2. `session` 先探 `<runtime>/sercon/run/s.sock`；连得上就直接用
3. 连不上：拿到 flock 的那个进程负责以脱离会话的方式拉起 `sercond capture`
4. `capture` 活在新会话里，SSH 断开时收不到 SIGHUP，继续存活
5. `session` 轮询 dial socket，成功后把自己变成哑管道

脱离手段两个平台不同：

- **Linux**：`setsid` 新会话 + 无控制终端
- **Windows**：`CREATE_BREAKAWAY_FROM_JOB`

Windows 那条值得展开：Windows OpenSSH 把会话子进程放进一个 job object，断连时
整棵树 kill。`DETACHED_PROCESS` 救不了你，只有 `CREATE_BREAKAWAY_FROM_JOB`
（`0x01000000`）能出 job——而且上游得专门设了 `JOB_OBJECT_LIMIT_BREAKAWAY_OK`
才允许这个标志。这是实测出来的，不是读文档读出来的。

**单实例靠 socket 绑定本身保证**：内核拒绝同一路径的第二次 bind，不需要额外的
锁文件。

### 会话通道

两端都用 AF_UNIX socket（Windows 从 10 1803 起支持 `net.Listen("unix", ...)`，
Go 的 net 包直接驱动）。不监听任何 TCP 端口；访问控制就是 socket 的文件权限 /
目录 ACL。

---

## 协议

```
1 byte 帧类型 | 4 bytes 长度(大端) | payload
```

- `0x01` 数据帧 — 串口原始字节，不做任何加工
- `0x02` 控制帧 — JSON，`op` 字段区分

终端控制字符全部装在带长度前缀的 payload 里，**所以协议对串口内容完全透明**。
这是有意的：串口上跑的东西从 U-Boot 到 Linux console 到任意二进制，任何转义或
加工都会在某处出错。

控制 op：

```
hello  welcome  list   ports   open    opened  close  closed
ping   pong     break  status  stat    notice  error  bye
shutdown
```

---

## 关键设计决策

### 端口标识以 `/dev/serial/by-id/*` 为准

内部保留 by-id 路径，每次打开前重新 `EvalSymlinks`。USB 重插导致 `ttyUSB` 编号
漂移时，引用和日志连续性都不受影响。

### 不引入串口库，也不套 net.Conn 抽象

`open(O_RDWR|O_NOCTTY|O_NONBLOCK)` + termios ioctl + epoll 轮询。自己管 fd 生命
周期，才能在设备消失时干净关闭并后台重试。

`net.Conn` 那种抽象表达不了串口真正需要的三件事：**不消费数据的读超时**、
**设备消失的通知**、**break 信号**。硬套一个接口只会把差异藏到后面。

### 每个端口一个 reader goroutine，无条件落盘

日志写入不依赖是否有客户端连接。**这是守护进程存在的全部理由。**

### 单写多读

一个 session 持写权限（owner），其余只读观察（observer）。避免两人同时敲键盘
把目标机 console 搅乱。观察者数量可配（`max_observers`，默认 4），也可以整个
关掉（`allow_observe: false`）。

### 每连接独立发送队列

慢客户端不会阻塞串口 reader。**队列溢出即判定该连接死亡并断开**，不会拖垮其他
会话。

### 附带 backlog

新连接先收到最近 64 KB 的历史输出，再切入实时流。排障时「我连上去之前发生了
什么」往往才是重点。

### 端口一旦注册就永不删除

设备消失只标 `offline`。这样重插后日志文件和 backlog 是连续的——否则「拔了一下
线」会造成两个日志目录，看起来像换了个设备。

### 锁序固定为 manager → port

任何反向获取都会死锁。

---

## 两个串口后端

两者**故意长得不一样**，因为两个系统对「读超时」「设备消失」「break」的表达
方式本就不同。硬套一个 `io.ReadWriter` 抽象只会把差异藏到后面去。

| | Linux（termios） | Windows（Win32 通信 API） |
|---|---|---|
| 打开 | `open(O_RDWR\|O_NOCTTY\|O_NONBLOCK)` | `CreateFileW("\\.\COM3")` + `FILE_FLAG_OVERLAPPED` |
| 线路配置 | termios ioctl（TCGETS/TCSETS），波特率取固定表里的 Bxxx 码 | DCB + `SetCommState`，波特率直接给数值，任意值都行 |
| 等待数据 | `epoll_wait` 带超时 | `WaitCommEvent(EV_RXCHAR)` + 重叠事件 |
| 读超时 | 内核对 tty 的原生支持 | **没有对应物**：超时要靠取消挂起的重叠操作实现 |
| 设备消失 | `EPOLLERR\|EPOLLHUP`，读返回 EIO | `ClearCommError` 返回错误 |
| break | `ioctl(TCSBRK)`，时长由内核定 | `SetCommBreak` 保持到 `ClearCommBreak`，时长自己定 |
| 枚举 | `/dev/serial/by-id/*` + glob | 注册表 `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` |

### Windows 侧两个必须知道的坑

**全部用重叠 I/O，但不是为了性能。** 这是唯一能把阻塞中的读从另一个 goroutine
唤醒的手段——`Close` 必须做到这件事。同步的 `ReadFile` 一旦阻塞在串口句柄上就
没法安全中断。

**超时是自己实现的。** 重叠操作要么完成、要么一直挂着。所以超时 = 取消挂起的
操作 + **消费掉那个 aborted 完成事件**，然后才能复用 `OVERLAPPED` 结构体。少这
一步就会留下一个指向「下一次调用即将覆盖的结构体」的活动操作。

这块踩过一个具体的坑：写坏接口契约后 `Read` 的 timeout 被忽略，`go test` 挂死
被 SIGTERM。修法是 `CancelIoEx` 之后必须消费那个 aborted 完成事件，否则
`OVERLAPPED` 被复用时会指向已释放的内存。

**`\\.\` 前缀不是装饰。** 少了它，COM10 及以上会被当成普通文件名，端口永远打不
开——而且只在机器串口够多的时候才暴露出来。

---

## Windows GUI

`sercon-gui.exe` 做的是同一件事，但把状态显示出来。在一台有人坐着的实验室机器
上，一个不可见的后台进程是错的形状：看不出适配器是不是活着、找不到日志、也不
知道怎么干净地停掉它。

窗口里是一张端口表（引用 / 状态 / 持有者 / 设备 / 波特率 / 观察者 / 日志路径 /
最近错误），下面三个按钮：

- **Open log folder** — 在资源管理器里打开日志目录
- **Copy attach command** — 把 `sercon attach -t user@host PORT` 复制到剪贴板，
  选中的是哪一行就复制哪一行。这是这个窗口能给出的最有用的一样东西
- **Refresh** — 立刻重扫一次，不用等自动扫描周期

窗口开着就在抓日志，关掉就停。没有单独的开关，因为「窗口可见 = 在抓」比一个
可能和实际状态不一致的复选框更不容易误判。

`sercond stop` 也会真的关掉这个窗口，不会只回一句「shutting down」然后继续跑。

### 界面上的几个刻意选择

状态列按颜色区分，这是扫一眼就能得到答案的三个问题：

| 颜色 | 含义 |
|---|---|
| 绿色 | 端口在线 |
| 红色 | 端口不在（拔了、被占用、打不开） |
| 琥珀色 | 有人正持有写权限 |

- **不加网格线。** 满屏细线是报表型 ListView 显得老气最主要的原因。整行选中加
  一点纵向留白读起来好得多，也不花成本
- **行高 26px。** ListView 的行高等于它小图标列表的高度，所以塞一个空的小图标
  列表是唯一受支持的行内留白手段
- **窗口背景纯白**，标题和状态栏走 `WM_CTLCOLORSTATIC` 单独设色。默认的
  `COLOR_BTNFACE` 灰底会让整个窗口像个 2001 年的设置对话框
- **固定标题。** 端口数量放在摘要行里，不放在标题栏——否则适配器一插一拔窗口
  标题就跳，脚本也没法按名字找到窗口

### 三个必须遵守的约束

**`-H=windowsgui` 不是可选的美化选项。** 少了它链接器产出的是**控制台子系统**
程序（subsystem 3），Windows 会给它分配一个控制台窗口——双击 GUI 就会在旁边
冒出一个黑窗口。副作用是没有控制台之后 stderr 无处可去，所以诊断信息写到
`<runtime>/gui.log`。窗口出得来但内容不对时先看这个文件。

**GUI 必须在交互桌面会话里启动。** SSH 会话和交互桌面属于不同的 window
station，SSH 拉起的进程即使带着这段代码也画不出窗口——看得见进程，看不见界面。

**主 goroutine 必须锁在同一个 OS 线程上。** Win32 把窗口消息投递给创建该窗口的
线程，而 Go 的 goroutine 随时可能换一个 OS 线程继续跑。不锁的话会出现一种很像
「程序坏了」的现象：`WM_CREATE`（创建期间同步投递）正常执行，而之后所有消息
——`WM_SIZE`、`WM_TIMER`、`WM_CLOSE`——都进了一个没人读的队列。窗口能显示，
但里面永远不刷新，也关不掉。代码里的 `runtime.LockOSThread()` 就是为这个。

`sercon-gui.exe.manifest` 要和 exe 放在同一目录，Windows 才会加载 comctl32 v6
并用上现代控件样式；缺了它程序照跑，只是控件退化成 XP 之前的画法。

### 改界面时怎么看效果

`hack/screenshot-window.py` 把窗口内容抓成 PNG，用于迭代界面：

```bash
./dist/sercon-gui.exe &
python hack/screenshot-window.py dist/shot.png            # 出图
python hack/screenshot-window.py dist/shot.png --histogram # 附加颜色分布
```

它用 `PrintWindow` + `PW_RENDERFULLCONTENT`，只把目标窗口自己的内容渲染进位图：
不会截到桌面上的其他东西，窗口被遮挡时也能正确抓取。纯标准库（ctypes + zlib）。

`--histogram` 那个模式是在排查「颜色到底生效了没」时加的——小字号灰字在缩略图里
看不出来，数颜色比看眼睛可靠。

---

## 踩过的实现坑

留在这里，因为每一条都花过时间，而且都不像是会出问题的地方。

### ListView 的 `CDDS_SUBITEM`

想给某一列单独上色时，`CDDS_SUBITEM` 的值是 **`0x00020000`**，不是
`0x00000002`。后者是 `CDDS_POSTPAINT`。写错不会报错，颜色就是不变。

`CDDS_ITEMPREPAINT|CDDS_SUBITEM` = `0x00030001`。

### `Read` 的 timeout 被忽略

自己写坏了接口契约，`go test` 挂死被 SIGTERM。见上面 Windows 侧那段。

### `sctl run -t X COM1 --send ...` 里 flag 被忽略

Go 的 `flag` 包遇到首个非 flag 参数就停止解析。所以 `-t X COM1 --send ...` 里
`--send` 根本不被解析。修法是加一个 `reorderArgs()` 把 flag 提到前面。

### Windows 保留设备名

`mkdir COM1` 直接失败，日志静默不写。`COM1`–`COM9` 是和 `CON`、`PRN`、`NUL`
同级的保留设备名。修法是日志目录名转义成 `COM1_`（见 `internal/portlog/safeName`）。

`COM10` 及以上**不是**保留名，所以这个失败会随机器上出现过的适配器数量时有时无
——这也是不能靠约定规避、必须在代码里处理的原因。

### 错误被 `if err == nil` 吞掉

`portlog` 的错误存进 `lastErr` 后，又被 `goOnline()` 里的 `p.lastErr = ""` 抹掉。
拆成独立字段才留住。

### 全新机器上 `daemon.Ensure` 必然失败

`os.MkdirAll(filepath.Dir(logPath))` 漏了，在 comp 上第一次部署时暴露。

### `--remote-bin '~/bin/sercond'` 被引号弄坏

远程 shell 是 zsh，`~` 在引号里不展开。修法是让 `~/` 留在引号外面。

### `git rebase --root` 把 `.git` 删进了回收站

重写作者信息时踩的，与 sercon 本身无关，但值得记：环境的安全删除层把 `.git`
整个移到 `C:\$Recycle.Bin\...\$R0DIIEX.git`，含一个截断的 `rebase-merge` 目录。
恢复之后改用 `git commit-tree` 重写作者，不碰 `.git` 结构。

---

## 传输方式：为什么现在只有 SSH

现阶段客户端只有一条路：SSH 到跳板机（或到插着串口的那台机器）。

这不是偷懒。SSH 已经免费给了三样东西——认证、加密、以及穿过网段的能力
（`~/.ssh/config` 里配好 ProxyJump 就行）。在它已经够用的地方再叠一层自定义
传输，只会多一份要维护的密钥管理和多一个可以被绕过的边界。

### 「局域网直连」需要区分两件不同的事

| | 前提 | 需要做什么 | 工作量 |
|---|---|---|---|
| **直连可达** | 两台机器在同一网段且路由互通 | 加一个 TCP 监听 + 强制 token + 审计记录来源 IP | 小 |
| **真 P2P（打洞）** | 双方都在 NAT 后面 | 信令服务器 + STUN/TURN + 打洞失败的中继兜底 | 另一个数量级 |

绝大多数「都是 WiFi 应该能连上」的场景其实是第一种。P2P 那套东西只有在中间
隔着 NAT 的时候才有意义——同一台交换机下面的两台机器之间没有 NAT 要穿。

`internal/relay/` 里已经实现了中继传输，但**没有接入 CLI，也没在真实 NAT 上
验证过**。在办公网实测确认能开 OpenSSH Server 之后，直连/打洞这条路暂时没有
必要——需求已经被 SSH 满足了。

### 但「都在 WiFi 上」不等于能直连

企业 WiFi 上最常见的三个拦路石，而且都是静默生效：

1. **客户端隔离（AP isolation）** — 很多企业 AP 默认打开，同网段终端之间完全
   不通。ping 也好、TCP 也好，全被 AP 丢掉
2. **不同 VLAN / SSID** — 办公 SSID 和测试 SSID 分开是常态
3. **主机防火墙** — Windows 默认拦掉入站的未放行端口

先花 30 秒验证，再决定要不要写代码：

```powershell
Test-NetConnection 10.x.x.x -Port 22
```

`TcpTestSucceeded : True` 才说明这条路存在。

### 真要加直连模式，安全模型必须跟着变

现在的权限边界是「socket 文件属主 + SSH 在前面挡着」。一旦监听网络端口，串口
控制台就等于挂在整个局域网上——而串口控制台能进 bootloader、能改内核 cmdline、
能 reset 机器。最低限度：

- 强制 token，每台机器一个，不要所有机器共用一个
- 传输加密（TLS 或 Noise），否则同网段任何人抓包就能看到 console 内容
- 源地址白名单
- 审计里记下来源 IP 而不是只记用户名
- 默认关闭，必须显式打开
