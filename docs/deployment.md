# 部署

两个文件的部署，但有几个地方容易漏。这份文档记录的是实际做过的步骤，不是理论上
应该怎么做。

---

## 清单

### 跳板机（插着串口那台）

**1. 放二进制**

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond'
```

不需要 root。`~/bin` 是最省事的位置，但注意它通常不在非交互式 SSH 的 PATH 上，
所以客户端要带 `--remote-bin '~/bin/sercond'`。

**2. 加 dialout 组**（Linux）

```bash
sudo usermod -aG dialout $USER
```

串口设备是 `crw-rw---- root:dialout`。**不加组的话 `sercond` 起得来、端口也
枚举得到，但每个端口都停在 `offline`** —— 这是最容易误判成「程序有 bug」的一种
失败。

组变更需要新会话才生效，所以改完要重新登录（或断开重连 SSH）。

**3. 验证**

```bash
~/bin/sercond version
~/bin/sercond list
```

### 你的机器

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

或者 Windows 上直接把 `sercon-windows-amd64.exe` 丢进 PATH 里某个目录。

### Windows 跳板机

两个都要放，它们分工不同：

| 文件 | 角色 | 谁来启动 |
|---|---|---|
| `sercon-gui.exe` | 守护进程本体，带窗口，持有串口 | 你双击，或放进启动文件夹 |
| `sercond-windows-amd64.exe`（改名 `sercond.exe`） | `session` / `list` / `status` / `stop` | SSH 自动拉起 |

**只放 GUI 的话 `sercon` 连不上**——SSH 需要的那几个子命令在 CLI 二进制里。
**只放 CLI 的话没有窗口**，得靠 `sercond capture` 手动起（前台，不脱离）。

`sercon-gui.exe.manifest` 和 exe 放同一目录。

Windows 跳板机还需要装 OpenSSH Server：

```powershell
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Start-Service sshd
Set-Service -Name sshd -StartupType Automatic
```

---

## 部署自检

`hack/verify-deployment.sh` 把上面这些检查串起来，9 步：

```bash
./hack/verify-deployment.sh you@jump
```

覆盖：二进制存在且可执行、版本能报出来、守护进程能拉起、端口枚举、dialout 权限、
日志目录可写、socket 绑定。

---

## 真实环境验证记录

**环境**

| | 机器 | 系统 | 内核 | OpenSSH | 串口 |
|---|---|---|---|---|---|
| 跳板机 / 目标 | `comp` | Ubuntu 20.04.6 | 5.15.0-139 | 8.2p1 | 两个 FTDI：ttyUSB0、ttyUSB1 |
| 客户端 | `flagship` | Ubuntu 24.04.4 | 6.8.0-139 | 9.6p1 | — |

`ttyUSB1` 接在一台 BMC 的串口控制台上，`ttyUSB0` 接一台 sync-agent。
客户端和跳板机之间是真 SSH，不是本地回环。

**验证结果**

| 项目 | 怎么验的 | 结果 |
|---|---|---|
| SSH 传输承载协议 | 客户端在 flagship、守护进程在 comp，真 ssh | 通 |
| 守护进程脱离 SSH 会话 | `sercond session < /dev/null` 立刻退出后查 `status` | 守护进程还活着 |
| termios + epoll 打开真实硬件 | 两个 FTDI 都进 `online` | 通 |
| 按 by-id 枚举与解析 | `list` 列出两个 by-id 名字并解析到 ttyUSB0/1 | 通 |
| 模糊匹配端口引用 | 只写 `usb-FTDI_FT232R` 前缀 | 命中唯一端口 |
| **读路径（真实数据）** | BMC 的 `ncsi-ioctl` 内核消息持续落盘 | 通 |
| **拔线检测 + 自动重连** | sysfs 解绑 USB 接口模拟拔线，再绑定 | `offline` → 7 秒后 `online` |
| **日志连续性** | 拔插前后检查同一个日志文件 | 一条没断，同一个文件 |
| 交互式 attach | 真 TTY 上把 BMC 串口实时输出打到屏幕 | 通 |
| 审计流水 | 会话/占用/释放/离线/上线全程记录 | 通 |
| 优雅停止 | `sercond stop` | 通 |
| Windows 串口后端 | 本机 COM1 上 open/read/close 反复 | 通 |
| Windows GUI | 窗口、着色、远端 stop 关窗 | 通 |

### 拔线是怎么模拟的

没有真的去拔 USB 线，用的是 sysfs 解绑接口——效果等价，而且可重复：

```bash
# 找到适配器对应的 USB 接口
ls /sys/bus/usb/drivers/ftdi_sio/

# 解绑（等效于拔线）
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/unbind

# 绑回来（等效于插回）
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/bind
```

解绑后 `sercond list` 立刻显示 `offline`，日志文件关闭；绑回后约 7 秒恢复
`online`，**写入的是同一个日志文件**——这是「端口一旦注册就永不删除」那条规则
的实际效果。

---

## 没验证的部分

诚实列出来，因为这几条都是会在生产里出问题的地方。

### Linux 写入路径没有确认送达

Windows 上验到「字节进了驱动」（审计流水里 `bytes=N`），Linux 上只验到**写调用
没报错**。

原因很实际：两个 FTDI 适配器都没接能回显的设备（BMC console 不会把你发的字符
回显回来），手上也没有回环插头。所以发送方向真正到达目标机这一条没验证。

要补的话，两个办法：

- 一根 USB 转串口的回环插头（TX 短接 RX），发什么收什么
- 一台会回话的设备接在另一端

**这不影响读路径**，而读路径（抓日志）是主要用途。但如果你的用法是「通过
sercon 往目标机敲命令」，目前只有「写调用成功」这个保证。

### Windows 拔线检测没实测

Linux 那条路径验过了（见上）。Windows 侧逻辑相同（`ClearCommError` 返回错误即
判离线），但没真拔过 USB 线，也没用设备管理器禁用过设备。

Windows 上没有 sysfs 那种干净的模拟手段，得真拔或者用
`pnputil /disable-device`。

### relay 传输没在真实 NAT 上验证

`internal/relay/` 实现了中继传输，但**没有接入 CLI**，也没在任何真实 NAT 环境
跑过。在办公网实测确认能开 OpenSSH Server 之后，这条路暂时没有必要——需求已经
被 SSH 满足了。设计取舍见 [`design.md`](design.md#传输方式为什么现在只有-ssh)。

---

## 卸载

```bash
# 跳板机
rm ~/bin/sercond
rm -rf ~/.local/state/sercon        # 日志和审计，想留就留着
rm -rf $XDG_RUNTIME_DIR/sercon      # socket（重启会自动清）

# 你的机器
rm ~/.local/bin/sercon
```

停守护进程：

```bash
sercon stop -t jump
```
