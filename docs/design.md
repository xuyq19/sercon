# 设计说明

记录 sercon 为什么是现在这个样子。使用方式看 [`../README.md`](../README.md)。

## 要解决的问题

测试机不在开发机所在的网段，本地 minicom 够不到串口。串口挂在跳板机上：

```
你的机器  ──网络──▶  跳板机  ──USB串口──▶  目标机
```

一个硬约束：跳板机不能装 systemd 服务，只能靠 SSH 会话拉起进程。常驻 TCP 服务
那套做法直接排除掉了。

## 形态

单个 Go 二进制，按 argv 分三个角色：

| 命令 | 运行位置 | 作用 |
|---|---|---|
| `sercon` | 你的机器 | 客户端，终端交互、重连、本地日志 |
| `sercond session <ref>` | 跳板机，SSH 拉起 | 确保守护进程存在，把 stdio 接到 socket |
| `sercond capture` | 跳板机，分离运行 | 持有串口 fd 的守护进程 |

`sercond session` 是哑管道，不理解协议，只做双向字节搬运。协议在 `sercon` 和
`capture` 之间端到端跑，守护进程的复杂度不会泄漏到 SSH 这一层。

## 没有 systemd 怎么活

客户端执行 `ssh -T jump sercond session <ref>` 之后：

1. `session` 先探 `<runtime>/sercon/run/s.sock`，连得上就直接用
2. 连不上就抢 flock，拿到锁的进程负责拉起 `sercond capture`
3. `capture` 活在新会话里，SSH 断开时收不到 SIGHUP，继续存活
4. `session` 轮询 socket，连上后把自己变成哑管道

两个平台的脱离手段不一样。Linux 用 `setsid` 开新会话加无控制终端。Windows 用
`CREATE_BREAKAWAY_FROM_JOB`。

Windows 这条路是实测出来的：OpenSSH 把会话子进程放进 job object，断连时 kill
整棵树。`DETACHED_PROCESS` 出不了 job，只有 `CREATE_BREAKAWAY_FROM_JOB`
（`0x01000000`）可以，而且上游得设了 `JOB_OBJECT_LIMIT_BREAKAWAY_OK` 才允许这个
标志。

单实例不用锁文件，socket 绑定本身就保证——内核拒绝同一路径的第二次 bind。

两端都用 AF_UNIX socket（Windows 从 10 1803 起 Go 的 net 包支持）。不监听 TCP
端口，访问控制就是 socket 文件权限。

## 协议

```
1 byte 帧类型 | 4 bytes 长度(大端) | payload
```

`0x01` 是数据帧，装串口原始字节。`0x02` 是控制帧，JSON，用 `op` 字段区分。

终端控制字符全装在带长度前缀的 payload 里，协议对串口内容完全透明。串口上跑
什么都有可能，从 U-Boot 到 Linux console 到任意二进制，任何转义或加工都会在某处
出错。

控制 op：

```
hello  welcome  list   ports   open    opened  close  closed
ping   pong     break  status  stat    notice  error  bye
shutdown
```

## 设计取舍

**端口标识用 `/dev/serial/by-id/*`。** 内部保留 by-id 路径，每次打开前重新
`EvalSymlinks`。USB 重插导致 `ttyUSB` 编号漂移时，引用和日志连续性不受影响。

**不引入串口库，不套 net.Conn 抽象。** `open(O_RDWR|O_NOCTTY|O_NONBLOCK)` 加
termios ioctl 加 epoll 轮询，自己管 fd 生命周期，才能在设备消失时干净关闭并后台
重试。`net.Conn` 那种抽象没法表达串口需要的三件事：不消费数据的读超时、设备消失
的通知、break 信号。硬套接口只会把差异藏到后面。

**每个端口一个 reader goroutine，无条件落盘。** 日志写入不依赖有没有客户端连接。
守护进程存在的意义就在这儿。

**单写多读。** 一个 session 持写权限，其余只读。避免两个人同时敲键盘把目标机
console 搅乱。观察者数量可配（默认 4），也能整个关掉。

**每连接独立发送队列。** 慢客户端不会阻塞串口 reader。队列溢出就判定这个连接
死了并断开，不拖累其他会话。

**附带 backlog。** 新连接先收到最近 64 KB 历史输出，再切实时流。排障时「连上去
之前发生了什么」往往才是重点。

**端口注册后永不删除。** 设备消失只标 `offline`。这样重插后日志文件和 backlog
是连续的，否则拔一下线就多一个日志目录，看起来像换了个设备。

**锁序固定 manager → port。** 反向获取会死锁。

## 两个串口后端

两边故意长得不一样。两个系统对读超时、设备消失、break 的表达方式本来就不同。

| | Linux（termios） | Windows（Win32 通信 API） |
|---|---|---|
| 打开 | `open(O_RDWR\|O_NOCTTY\|O_NONBLOCK)` | `CreateFileW("\\.\COM3")` + `FILE_FLAG_OVERLAPPED` |
| 线路配置 | termios ioctl，波特率取 Bxxx 码 | DCB + `SetCommState`，波特率直接给数值 |
| 等待数据 | `epoll_wait` 带超时 | `WaitCommEvent(EV_RXCHAR)` + 重叠事件 |
| 读超时 | 内核对 tty 的原生支持 | 没有对应物，靠取消挂起的重叠操作实现 |
| 设备消失 | `EPOLLERR\|EPOLLHUP`，读返回 EIO | `ClearCommError` 返回错误 |
| break | `ioctl(TCSBRK)`，时长由内核定 | `SetCommBreak` 保持到 `ClearCommBreak` |
| 枚举 | `/dev/serial/by-id/*` + glob | 注册表 `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` |

Windows 侧两个地方值得单独说。

**全部用重叠 I/O，原因不是性能。** 这是唯一能把阻塞中的读从另一个 goroutine
唤醒的手段，`Close` 必须做到这件事。同步的 `ReadFile` 阻塞在串口句柄上之后没法
安全中断。

**超时是自己实现的。** 重叠操作要么完成要么一直挂着，所以超时 = 取消挂起的操作
加消费掉那个 aborted 完成事件，之后才能复用 `OVERLAPPED` 结构体。漏了这步会留下
一个指向「下次调用即将覆盖的结构体」的活动操作。

`\\.\` 前缀同理，少了它 COM10 及以上会被当成普通文件名，端口永远打不开，而且
只在机器串口够多的时候才暴露。

## Windows GUI

`sercon-gui.exe` 是同一个守护进程加一个 Win32 窗口。在一台有人坐着的实验室机器
上，不可见的后台进程不好用：看不出适配器活着没有、找不到日志、不知道怎么干净地
停掉。

窗口里是端口表，加三个按钮：打开日志目录、复制 attach 命令、立即重扫。复制按钮
把 `sercon attach -t user@host PORT` 放进剪贴板，选中哪行复制哪行。

窗口开着就在抓日志，关掉就停，没有单独的开关。`sercon stop` 会真的把窗口关掉。

状态列按颜色区分：

| 颜色 | 含义 |
|---|---|
| 绿色 | 端口在线 |
| 红色 | 端口不在（拔了、被占用、打不开） |
| 琥珀色 | 有人正持有写权限 |

界面上的几个选择：

- 不加网格线。满屏细线是报表型 ListView 显得老气的主要原因，整行选中加一点纵向
  留白读起来好得多
- 行高 26px。ListView 的行高等于它小图标列表的高度，所以塞一个空的小图标列表是
  唯一受支持的行内留白手段
- 窗口背景纯白，标题和状态栏走 `WM_CTLCOLORSTATIC` 单独设色。默认的
  `COLOR_BTNFACE` 灰底会让窗口看起来像 2001 年的设置对话框
- 标题固定。端口数量放在摘要行里，不然适配器一插一拔标题就跳，脚本也没法按名字
  找窗口

### 三个约束

**`-H=windowsgui` 是必须的。** 少了它链接器产出控制台子系统程序，Windows 会给它
分配控制台窗口，双击 GUI 就会在旁边冒出一个黑窗口。代价是没有控制台之后 stderr
无处可去，诊断信息写到 `<runtime>/gui.log`。窗口出得来但内容不对时先看这个文件。

**GUI 必须在交互桌面会话里启动。** SSH 会话和交互桌面属于不同的 window station，
SSH 拉起的进程画不出窗口，看得见进程看不见界面。

**主 goroutine 必须锁在同一个 OS 线程上。** Win32 把窗口消息投递给创建该窗口的
线程，而 goroutine 随时可能换线程。不锁的话 `WM_CREATE`（创建期间同步投递）正常
执行，之后所有消息——`WM_SIZE`、`WM_TIMER`、`WM_CLOSE`——都进了一个没人读的
队列。窗口能显示，但永不刷新，也关不掉。

`sercon-gui.exe.manifest` 要和 exe 放同目录，Windows 才会加载 comctl32 v6 用上
现代控件样式。缺了它程序照跑，控件退化成 XP 之前的画法。

### 改界面时怎么看效果

`hack/screenshot-window.py` 把窗口抓成 PNG：

```bash
./dist/sercon-gui.exe &
python hack/screenshot-window.py dist/shot.png
python hack/screenshot-window.py dist/shot.png --histogram   # 附带颜色分布
```

用 `PrintWindow` + `PW_RENDERFULLCONTENT`，只渲染目标窗口自己的内容，不会截到
桌面上的其他东西，窗口被遮挡时也能抓。纯标准库（ctypes + zlib）。

`--histogram` 是排查「颜色生效了没」时加的，小字号灰字在缩略图里看不出来，数颜色
比看眼睛可靠。

## 踩过的坑

每一条都花过时间，而且都不像是会出问题的地方。

**ListView 的 `CDDS_SUBITEM` 是 `0x00020000`**，不是 `0x00000002`（那是
`CDDS_POSTPAINT`）。写错不报错，颜色就是不变。
`CDDS_ITEMPREPAINT|CDDS_SUBITEM` = `0x00030001`。

**`Read` 的 timeout 被忽略。** 自己写坏了接口契约，`go test` 挂死被 SIGTERM。
修法是 `CancelIoEx` 之后必须消费那个 aborted 完成事件。

**`sercon run -t X COM1 --send ...` 里 flag 被忽略。** Go 的 `flag` 包遇到首个
非 flag 参数就停止解析。加一个 `reorderArgs()` 把 flag 提到前面。

**`mkdir COM1` 失败，日志静默不写。** `COM1`–`COM9` 是和 `CON`、`PRN`、`NUL`
同级的保留设备名。日志目录名转义成 `COM1_`。`COM10` 及以上不是保留名，所以这个
失败会随机器上用过的适配器数量时有时无，不能靠约定规避。

**错误被 `if err == nil` 吞掉。** `portlog` 的错误存进 `lastErr` 后又被
`goOnline()` 里的 `p.lastErr = ""` 抹掉，拆成独立字段才留住。

**全新机器上 `daemon.Ensure` 必然失败。** `os.MkdirAll(filepath.Dir(logPath))`
漏了，部署到第二台机器时才暴露。

**`--remote-bin '~/bin/sercond'` 被引号弄坏。** 远程 shell 是 zsh，`~` 在引号里
不展开，得让它留在引号外面。

**`git rebase --root` 把 `.git` 删进了回收站。** 改文档作者信息时踩的，和 sercon
本身无关：环境的安全删除层把 `.git` 整个移到了 `C:\$Recycle.Bin\...\$R0DIIEX.git`。
恢复之后改用 `git commit-tree` 重写作者，不碰 `.git` 结构。

## 传输方式

目前客户端只有 SSH 一条路。SSH 已经提供了认证、加密和穿网段的能力（`ProxyJump`
配一下就行），在它够用的地方再叠一层自定义传输，只会多一份要维护的密钥管理和多
一个能被绕过的边界。

「局域网直连」需要区分两件不同的事：

| | 前提 | 需要做什么 | 工作量 |
|---|---|---|---|
| 直连可达 | 同网段且路由互通 | 加 TCP 监听、强制 token、审计记录来源 IP | 小 |
| 真 P2P（打洞） | 双方都在 NAT 后 | 信令服务器 + STUN/TURN + 中继兜底 | 另一个数量级 |

「都是 WiFi 应该能连上」的场景基本都是第一种。P2P 只在隔着 NAT 的时候才有意义，
同一台交换机下面的两台机器之间没有 NAT 要穿。

企业 WiFi 上还有三个静默生效的拦路石：客户端隔离（很多 AP 默认开，同网段终端
之间完全不通）、不同 VLAN/SSID、主机防火墙。写代码之前先花 30 秒验证：

```powershell
Test-NetConnection 10.x.x.x -Port 22
```

`TcpTestSucceeded : True` 才说明这条路存在。

`internal/relay/` 里实现了中继传输，没接入 CLI，也没在真实 NAT 上验证过。办公网
实测能开 OpenSSH Server 之后，这条路暂时没必要。

真要加直连模式，安全模型得跟着变。现在的边界是 socket 文件属主加上 SSH 挡在前面；
一旦监听网络端口，串口控制台就等于挂在整个局域网上，而串口控制台能进 bootloader、
能改内核 cmdline、能 reset 机器。最低限度要做的：强制 token（每台机器一个）、
传输加密、源地址白名单、审计记录来源 IP、默认关闭。
