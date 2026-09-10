# 跨网段串口控制台工具 — 设计与实施计划

> **历史文档。** 这是动手之前写的实施计划，阶段划分和当时的判断都保留原样。
> 实际实现里有些地方和它不一致（比如模块名、二进制名、以及几处踩坑后改掉的
> 做法）。要看最终设计请读 [`design.md`](design.md)。

## 问题

测试机与开发机不在同一网段，本地 minicom 够不到串口。串口物理上挂在跳板机，
实际链路是：本机终端 → 网络 → 跳板机 → USB 串口 → 目标机。

## 约束（已确认）

- 跳板机**不能**装 systemd 服务，只能靠 SSH 会话拉起
- 客户端要能在 Windows 原生终端、WSL/Git Bash、Linux 三种环境下工作

约束推翻了"常驻 TCP 服务"的常规做法，改用下面的形态。

## 形态

单个 Go 二进制、零第三方依赖（纯 stdlib）。同一份代码三个角色：

| 命令 | 运行位置 | 作用 |
|---|---|---|
| `sercon` | 本机 | 客户端，负责终端交互、重连、本地日志 |
| `sercond session <ref>` | 跳板机（SSH 拉起） | 确保守护进程存在，然后把 stdin/stdout 与 Unix socket 对接 |
| `sercond capture` | 跳板机（分离运行） | 真正持有串口 fd 的守护进程 |

关键点：`sercond session` 是个**哑管道**，不理解协议，只做双向字节搬运。
协议在 `sercon` 和 `capture` 之间端到端跑，这样守护进程的复杂性不会泄漏到 SSH 这一层。

## 守护进程怎么在无 systemd 的情况下活下来

1. 客户端执行 `ssh -T jump sercond session <ref>`
2. `session` 先探 `<runtime>/sercond.sock`；连得上就直接用
3. 连不上：取 `<runtime>/sercond.lock` 的 `flock`，拿到锁的进程负责
   以 `setsid` + 重定向 stdio 的方式拉起 `sercond capture`
4. `capture` 新会话 + 无控制终端，SSH 断开时收不到 SIGHUP，继续存活
5. `session` 轮询 dial socket，成功后把自己变成哑管道

因为 socket 只对本用户可读（runtime 目录 0700），认证边界就是文件权限，
不需要 token，也不需要 TCP 端口。

## 协议

```
1 byte type | 4 bytes length (big-endian) | payload
```

- `0x01` 数据帧 — 串口原始字节，不做任何加工
- `0x02` 控制帧 — JSON，`op` 字段区分

控制 op：
`hello` / `welcome` / `list` / `ports` / `open` / `opened` / `close` / `closed` /
`ping` / `pong` / `break` / `status` / `stat` / `notice` / `error` / `bye`

传输控制字符以外的一切都由数据帧承载，所以协议对串口内容完全透明。

## 关键设计决策

1. **端口标识以 `/dev/serial/by-id/*` 为准**
   内部保留 by-id 路径，每次打开前重新 `EvalSymlinks`。
   USB 重插导致 `ttyUSB` 编号漂移时，引用和日志连续性都不受影响。

2. **不引入串口库，也不套 net.Conn 抽象**
   `open(O_RDWR|O_NOCTTY|O_NONBLOCK)` + termios ioctl + epoll 轮询。
   自己管 fd 生命周期，才能在设备消失时干净关闭并后台重试。

3. **每个端口一个 reader goroutine，无条件落盘**
   日志写入不依赖是否有客户端连接。这是守护进程存在的全部意义。

4. **单写多读**
   一个 session 持写权限（owner），其余只读观察（observer）。
   避免两人同时敲键盘把目标机 console 搅乱。

5. **每连接独立发送队列**
   慢客户端不会阻塞串口 reader。队列溢出即判定该连接死亡并断开，
   不会拖垮其他会话。

6. **附带 backlog**
   新连接先收到最近 64 KB 的历史输出，再切入实时流。
   排障时"我连上去之前发生了什么"往往才是重点。

7. **客户端原始终端模式**
   Linux 走 termios ioctl，Windows 走 `SetConsoleMode` + VT 输入。
   转义键 `Ctrl-A`，对齐 minicom 肌肉记忆。

## 四大功能落点

1. **自动留日志**
   `<logdir>/<port>/YYYY-MM-DD.log`，行首时间戳，跨天自动换文件，
   纯 append，不删旧文件（交给 logrotate）。

2. **多会话并发**
   守护进程内维护 session 表，串口数据向所有已连接 session 广播。

3. **断线自动重连**
   客户端侧：EOF 后指数退避重连 + 自动 re-open 原端口，终端打印恢复标记。
   服务端侧：设备消失后关闭 fd，后台退避重试 open，设备回来即恢复。

4. **权限与审计**
   认证由 SSH 承担，审计落 JSONL：
   会话建立/断开、端口占用/释放、写字节数、来源标识、认证失败。

## 默认参数

- 串口：115200 8N1，无流控，raw
- runtime：`$XDG_RUNTIME_DIR/sercon`，未设置则 `/tmp/sercond-<uid>`
- 日志：`$XDG_STATE_HOME/sercon/ports`，未设置则 `~/.local/state/sercon/ports`
- 审计：同级的 `audit/`
- 配置：`~/.config/sercon/config.json`

## 分阶段实施

| 阶段 | 内容 | 验证 |
|---|---|---|
| 1 | proto 帧 + serialport(termios/epoll) + terminal(raw 模式) | 编译通过 |
| 2 | portlog 轮转日志 + audit JSONL | 编译通过 |
| 3 | hub：端口注册/广播/端口锁/backlog | 编译通过 |
| 4 | sercond（session/capture/list/status/stop）+ sercon（ls/attach/run） | 编译通过 |
| 5 | 交叉编译 linux+windows，端到端冒烟 | 跑通 |
| 6 | Makefile + config 样例 + README | 可部署 |
