# 部署

按顺序执行的部署步骤，以及实际的验证记录。

## 清单

### 跳板机（连接串口的主机）

**部署二进制文件**

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond'
```

不需要 root 权限。`~/bin` 通常不在非交互式 SSH 的 PATH 中，因此客户端需指定
`--remote-bin`。

**加入 dialout 组**

```bash
sudo usermod -aG dialout $USER
```

串口设备权限为 `crw-rw---- root:dialout`。未加入该组时，`sercond` 可以启动、端口也
能被枚举，但所有端口都会停留在 `offline` 状态——很容易被误判为程序缺陷。

组变更需要新会话才能生效，完成后须重新登录。

**验证**

```bash
~/bin/sercond version
~/bin/sercond list
```

### 本地主机

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

Windows 上将 `sercon-windows-amd64.exe` 放入 PATH 中的某个目录。

### Windows 跳板机

需要部署两个二进制文件：

| 文件 | 角色 | 启动方式 |
|---|---|---|
| `sercon-gui.exe` | 守护进程本体，带窗口，持有串口 | 双击，或置于启动文件夹 |
| `sercond-windows-amd64.exe`（重命名为 `sercond.exe`） | `session` / `list` / `status` / `stop` | 由 SSH 拉起 |

只部署 GUI 时客户端无法连接，因为 SSH 所需的子命令位于 CLI 中。只部署 CLI 时没有
窗口，需手动执行 `sercond capture`（前台运行，不脱离会话）。`sercon-gui.exe.manifest`
须与 exe 置于同一目录。

安装 OpenSSH Server：

```powershell
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Start-Service sshd
Set-Service -Name sshd -StartupType Automatic
```

防火墙放行 22 端口：

```powershell
New-NetFirewallRule -Name sshd -DisplayName 'OpenSSH Server' `
  -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
```

**客户端公钥的位置取决于账号是否为管理员。** 默认 `sshd_config` 末尾包含：

```
Match Group administrators
       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys
```

管理员账号的公钥必须放入 `%ProgramData%\ssh\administrators_authorized_keys`。放入
`%USERPROFILE%\.ssh\authorized_keys` 不会生效，且报错信息仍为
`Permission denied (publickey)`，指向密钥而非路径，难以排查。该目录无法以普通权限
写入，修改文件需使用管理员权限的命令行。

写入后 ACL 也须正确，sshd 才会读取：

```cmd
icacls "%ProgramData%\ssh\administrators_authorized_keys" ^
  /inheritance:r /grant "*S-1-5-18:(F)" /grant "*S-1-5-32-544:(F)"
```

此处使用 SID 而非组名，因为组名是本地化的。

修改后无需重启 sshd，下一次连接时会重新读取。

GUI 的 `SSH keys` 面板将上述步骤整合为两步操作，参见
[README](../README.md#ssh-keys)。

## 自检

`hack/verify-deployment.sh` 将上述检查串联起来：

```bash
./hack/verify-deployment.sh you@jump
```

覆盖内容包括：二进制文件存在且可执行、版本可正常输出、守护进程可拉起、端口枚举、
dialout 权限、日志目录可写、socket 绑定。

## 验证记录

### 环境

| | 机器 | 系统 | 内核 | OpenSSH | 串口 |
|---|---|---|---|---|---|
| 跳板机 | `comp` | Ubuntu 20.04.6 | 5.15.0-139 | 8.2p1 | 两个 FTDI：ttyUSB0、ttyUSB1 |
| 客户端 | `flagship` | Ubuntu 24.04.4 | 6.8.0-139 | 9.6p1 | — |

`ttyUSB1` 连接在一台 BMC 的串口控制台上，`ttyUSB0` 连接一台 sync-agent。

### 结果

| 项目 | 验证方式 | 结果 |
|---|---|---|
| SSH 传输承载协议 | 客户端在 flagship、守护进程在 comp，使用真实 ssh | 通过 |
| 守护进程脱离 SSH 会话 | `sercond session < /dev/null` 退出后查询 `status` | 仍在运行 |
| termios + epoll 打开真实硬件 | 两个 FTDI 均进入 `online` | 通过 |
| by-id 枚举与解析 | `list` 列出两个 by-id 名称并解析到 ttyUSB0/1 | 通过 |
| 模糊匹配端口引用 | 仅写 `usb-FTDI_FT232R` 前缀 | 命中唯一端口 |
| 读路径 | BMC 的 `ncsi-ioctl` 内核消息持续落盘 | 通过 |
| 拔线检测与重连 | sysfs 解绑 USB 接口后重新绑定 | `offline` → 7 秒后 `online` |
| 日志连续性 | 检查拔插前后的同一个日志文件 | 无中断 |
| 交互式 attach | 在真实 TTY 上将 BMC 串口输出显示到屏幕 | 通过 |
| 审计流水 | 会话、占用、释放、离线、上线全程均有记录 | 通过 |
| 优雅停止 | `sercond stop` | 通过 |
| Windows 串口后端 | 本机 COM1 上反复执行 open/read/close | 通过 |
| Windows GUI | 窗口、着色、远端 stop 关闭窗口 | 通过 |

### Windows 作为服务端

`comp`（Linux）作为客户端，本机（Windows）作为服务端。本机有 COM1、COM3 两个串口，
均为主板自带，非 USB 适配器。

| 项目 | 验证方式 | 结果 |
|---|---|---|
| comp → Windows 公钥认证 | 公钥写入 `administrators_authorized_keys` | 通过 |
| 修改密钥文件是否需重启 sshd | 修改后不重启，直接连接 | 通过（立即生效） |
| ACL 收紧后仍可认证 | `icacls /inheritance:r` + 两个 SID | 通过 |
| GUI 面板定位密钥文件 | 面板顶部显示 ProgramData 路径 | 通过 |
| GUI 面板提权读取 | 「Reload」→ UAC → 列出两个 key | 通过 |
| GUI 面板写入 | 粘贴新 key → UAC → 文件 676 字节 | 通过（+96 = 95 字符 + 换行） |
| 指纹与 ssh-keygen 一致 | 面板显示的指纹 vs `ssh-keygen -lf` | 通过（逐字节相同） |
| GUI 面板删除 | 选中 → 确认 → UAC → 文件恢复至 581 字节 | 通过 |
| 删除后仍可登录 | 删除测试 key 后重新连接 | 通过 |
| 重复添加幂等 | 同一个 key 添加两次 | 第二次提示「已安装」，文件未变 |

指纹一项需要单独说明：面板显示的 `SHA256:X8A69RzhE4rAXgGiWCy5f8wyid/bTFOjPwK7tUQoVC0`
与 `ssh-keygen -lf` 的输出完全一致。这一点在运维需要用该值进行比对时才有实际意义。

### 拔线的模拟方式

未实际拔除 USB 线，而是通过 sysfs 解绑接口，效果等价且可重复：

```bash
# 查找适配器对应的 USB 接口
ls /sys/bus/usb/drivers/ftdi_sio/

# 解绑，等同于拔线
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/unbind

# 重新绑定，等同于插回
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/bind
```

解绑后 `sercond list` 立即显示 `offline`，日志文件关闭。重新绑定后约 7 秒恢复
`online`，写入的是同一个日志文件。

## 未验证项

### Linux 写路径未确认送达

Windows 上已验证字节进入驱动（审计中记录 `bytes=N`），Linux 上仅验证到写调用未报错。

两个 FTDI 均未连接可回显的设备（BMC console 不回显），也没有回环插头。补齐验证需要：

- 一个 USB 转串口回环插头（TX 短接 RX），发送什么即收到什么
- 或一台会响应的设备连接在另一端

读路径不受影响，而采集日志是主要用途。但如果通过 sercon 向目标机输入命令，目前仅有
「写调用成功」这一层面的保证。

### Windows 拔线检测未实测

Linux 侧已验证，Windows 侧逻辑相同（`ClearCommError` 返回错误即判定离线），但未进行
真实的拔线操作，也未通过设备管理器禁用过设备。

Windows 上没有 sysfs 这类干净的模拟手段，需要真实拔线或使用
`pnputil /disable-device`。

### relay 传输未在真实 NAT 环境验证

`internal/relay/` 已实现中继传输，但未接入 CLI，也未在真实 NAT 环境中运行过。设计
取舍参见 [`design.md`](design.md#传输方式)。

### Windows 服务端仅验证到 SSH 与密钥

上述表格验证的是「comp 能够免密登录该 Windows 主机」和「GUI 能正确安装密钥」。更
完整的链路尚未走通：

- **`sercond.exe` 经 SSH 拉起后，通过 `CREATE_BREAKAWAY_FROM_JOB` 脱离 OpenSSH 的
  job object** —— 代码和回退路径均已实现，但未在真实 SSH 会话中实测。这是整条链路中
  唯一缺少实证的环节，也是最可能出现问题的环节：如果 sshd 不再为 job 设置
  `JOB_OBJECT_LIMIT_BREAKAWAY_OK`，守护进程将无法存活至会话结束，`Ensure` 会报错。
- **从 comp 使用 `sercon attach` 读取 Windows 的 COM 口** —— 未执行过。本机的
  COM1/COM3 未连接能产生输出的设备，读取结果为空。

继续验证的第一步是：

```bash
ssh lucas@10.240.205.92 'sercond session' < /dev/null
ssh lucas@10.240.205.92 'sercond status'      # 检查是否仍在运行
```

第二条命令能够返回，即说明 breakaway 成功。

## 卸载

```bash
# 跳板机
sercon stop -t jump
rm ~/bin/sercond
rm -rf ~/.local/state/sercon        # 日志与审计
rm -rf $XDG_RUNTIME_DIR/sercon      # socket，重启后会自动清理

# 本地主机
rm ~/.local/bin/sercon
```
