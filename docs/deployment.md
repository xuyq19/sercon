# 部署

按顺序做下来的部署步骤，以及实际的验证记录。

## 清单

### 跳板机（插着串口那台）

**放二进制**

```bash
scp sercond-linux-amd64 you@jump:~/bin/sercond
ssh you@jump 'chmod +x ~/bin/sercond'
```

不需要 root。`~/bin` 通常不在非交互式 SSH 的 PATH 上，所以客户端要带
`--remote-bin`。

**加 dialout 组**

```bash
sudo usermod -aG dialout $USER
```

串口设备是 `crw-rw---- root:dialout`。不加组的话 `sercond` 起得来、端口也枚举
得到，但每个端口都停在 `offline`——很容易误判成程序有 bug。

组变更要新会话生效，改完重新登录。

**验证**

```bash
~/bin/sercond version
~/bin/sercond list
```

### 自己的机器

```bash
install -m755 sercon-linux-amd64 ~/.local/bin/sercon
```

Windows 上把 `sercon-windows-amd64.exe` 放进 PATH 里某个目录。

### Windows 跳板机

两个二进制都要放：

| 文件 | 角色 | 谁启动 |
|---|---|---|
| `sercon-gui.exe` | 守护进程本体，带窗口，持有串口 | 双击，或放启动文件夹 |
| `sercond-windows-amd64.exe`（改名 `sercond.exe`） | `session` / `list` / `status` / `stop` | SSH 拉起 |

只放 GUI 的话客户端连不上，SSH 需要的那几个子命令在 CLI 里。只放 CLI 的话没有
窗口，得手动 `sercond capture`（前台，不脱离）。`sercon-gui.exe.manifest` 和 exe
放同一目录。

装 OpenSSH Server：

```powershell
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Start-Service sshd
Set-Service -Name sshd -StartupType Automatic
```

防火墙放行 22：

```powershell
New-NetFirewallRule -Name sshd -DisplayName 'OpenSSH Server' `
  -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
```

**客户端公钥放哪儿，取决于账号是不是管理员。** 默认 `sshd_config` 末尾有：

```
Match Group administrators
       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys
```

管理员账号的公钥必须放 `%ProgramData%\ssh\administrators_authorized_keys`，
放 `%USERPROFILE%\.ssh\authorized_keys` 不生效，而且报错还是
`Permission denied (publickey)`，指向密钥而不是路径，很难查。这个目录普通权限
写不进去，改文件要用管理员 cmd。

写进去之后 ACL 还得对，sshd 才肯读：

```cmd
icacls "%ProgramData%\ssh\administrators_authorized_keys" ^
  /inheritance:r /grant "*S-1-5-18:(F)" /grant "*S-1-5-32-544:(F)"
```

用 SID 而不是组名，因为组名是本地化的。

改完不用重启 sshd，下一次连接就会重新读。

GUI 的 `SSH keys` 面板把上面这些包成两步，见
[README](../README.md#ssh-keys)。

## 自检

`hack/verify-deployment.sh` 把上面的检查串起来：

```bash
./hack/verify-deployment.sh you@jump
```

覆盖二进制存在且可执行、版本能报出来、守护进程能拉起、端口枚举、dialout 权限、
日志目录可写、socket 绑定。

## 验证记录

### 环境

| | 机器 | 系统 | 内核 | OpenSSH | 串口 |
|---|---|---|---|---|---|
| 跳板机 | `comp` | Ubuntu 20.04.6 | 5.15.0-139 | 8.2p1 | 两个 FTDI：ttyUSB0、ttyUSB1 |
| 客户端 | `flagship` | Ubuntu 24.04.4 | 6.8.0-139 | 9.6p1 | — |

`ttyUSB1` 接在一台 BMC 的串口控制台上，`ttyUSB0` 接一台 sync-agent。

### 结果

| 项目 | 怎么验的 | 结果 |
|---|---|---|
| SSH 传输承载协议 | 客户端在 flagship、守护进程在 comp，真 ssh | 通 |
| 守护进程脱离 SSH 会话 | `sercond session < /dev/null` 退出后查 `status` | 还活着 |
| termios + epoll 打开真实硬件 | 两个 FTDI 都进 `online` | 通 |
| by-id 枚举与解析 | `list` 列出两个 by-id 名字并解析到 ttyUSB0/1 | 通 |
| 模糊匹配端口引用 | 只写 `usb-FTDI_FT232R` 前缀 | 命中唯一端口 |
| 读路径 | BMC 的 `ncsi-ioctl` 内核消息持续落盘 | 通 |
| 拔线检测与重连 | sysfs 解绑 USB 接口，再绑回 | `offline` → 7 秒后 `online` |
| 日志连续性 | 拔插前后检查同一个日志文件 | 一条没断 |
| 交互式 attach | 真 TTY 上把 BMC 串口输出打到屏幕 | 通 |
| 审计流水 | 会话、占用、释放、离线、上线全程记录 | 通 |
| 优雅停止 | `sercond stop` | 通 |
| Windows 串口后端 | 本机 COM1 上 open/read/close 反复 | 通 |
| Windows GUI | 窗口、着色、远端 stop 关窗 | 通 |

### Windows 当服务端

`comp`（Linux）作客户端，本机（Windows）作服务端。本机有 COM1、COM3 两个串口，
是主板自带的，不是 USB 适配器。

| 项目 | 怎么验的 | 结果 |
|---|---|---|
| comp → Windows 公钥认证 | 公钥装进 `administrators_authorized_keys` | 通 |
| sshd 重新读密钥要否重启 | 改完文件不重启，直接连 | 通（立即生效） |
| ACL 收紧后仍可认证 | `icacls /inheritance:r` + 两个 SID | 通 |
| GUI 面板定位密钥文件 | 面板顶部显示 ProgramData 路径 | 通 |
| GUI 面板提权读 | 「Reload」→ UAC → 列出两个 key | 通 |
| GUI 面板写入 | 粘贴新 key → UAC → 文件 676 字节 | 通（+96 = 95 字符 + 换行） |
| 指纹与 ssh-keygen 一致 | 面板显示的指纹 vs `ssh-keygen -lf` | 通（逐字节相同） |
| GUI 面板删除 | 选中 → 确认 → UAC → 文件回到 581 字节 | 通 |
| 删除后仍能登录 | 删掉测试 key 后再连 | 通 |
| 重复添加幂等 | 同一个 key 加两次 | 第二次报「已安装」，文件不变 |

指纹这一项值得单独说：面板显示的 `SHA256:X8A69RzhE4rAXgGiWCy5f8wyid/bTFOjPwK7tUQoVC0`
和 `ssh-keygen -lf` 输出的完全一致。运维要拿它比对时才有意义。

### 拔线是怎么模拟的

没真拔 USB 线，用 sysfs 解绑接口，效果等价而且可重复：

```bash
# 找适配器对应的 USB 接口
ls /sys/bus/usb/drivers/ftdi_sio/

# 解绑，相当于拔线
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/unbind

# 绑回，相当于插回
echo '<bus>-<port>:1.0' | sudo tee /sys/bus/usb/drivers/ftdi_sio/bind
```

解绑后 `sercond list` 立刻显示 `offline`，日志文件关闭。绑回后约 7 秒恢复
`online`，写的是同一个日志文件。

## 没验证的

### Linux 写路径没有确认送达

Windows 上验到字节进了驱动（审计里 `bytes=N`），Linux 上只验到写调用没报错。

两个 FTDI 都没接能回显的设备（BMC console 不回显），也没有回环插头。要补的话：

- 一根 USB 转串口的回环插头（TX 短接 RX），发什么收什么
- 或者一台会回话的设备接在另一端

读路径不受影响，而抓日志是主要用途。但如果你的用法是通过 sercon 往目标机敲
命令，目前只有「写调用成功」这个保证。

### Windows 拔线检测没实测

Linux 侧验过了，Windows 侧逻辑一样（`ClearCommError` 返回错误即判离线），但没
真拔过线，也没用设备管理器禁用过设备。

Windows 上没有 sysfs 那种干净的模拟手段，得真拔或者用 `pnputil /disable-device`。

### relay 传输没在真实 NAT 上验证

`internal/relay/` 实现了中继传输，没接入 CLI，也没在真实 NAT 环境跑过。设计取舍
见 [`design.md`](design.md#传输方式)。

### Windows 服务端只验到 SSH 和密钥

上面那张表验的是「comp 能免密登进这台 Windows」和「GUI 能把密钥装对」。再往下
的完整链路还没走：

- **`sercond.exe` 经 SSH 拉起，靠 `CREATE_BREAKAWAY_FROM_JOB` 逃出 OpenSSH 的
  job object** —— 代码写了，回退路径也写了，但在真实 SSH 会话里没实测过。
  这是整条链路里唯一没有实证的环节，也是最可能翻车的：如果 sshd 不再给 job 设
  `JOB_OBJECT_LIMIT_BREAKAWAY_OK`，守护进程就活不过会话，`Ensure` 会报错。
- **从 comp 用 `sercon attach` 读 Windows 的 COM 口** —— 没跑过。本机的 COM1/COM3
  没接能产生输出的设备，读到的是空。

要接着验的话，第一步是：

```bash
ssh lucas@10.240.205.92 'sercond session' < /dev/null
ssh lucas@10.240.205.92 'sercond status'      # 看是否还活着
```

第二条能返回就说明 breakaway 成功了。

## 卸载

```bash
# 跳板机
sercon stop -t jump
rm ~/bin/sercond
rm -rf ~/.local/state/sercon        # 日志和审计
rm -rf $XDG_RUNTIME_DIR/sercon      # socket，重启会自动清

# 自己的机器
rm ~/.local/bin/sercon
```
