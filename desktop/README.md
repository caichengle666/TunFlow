# TunFlow Desktop

TunFlow Desktop 是基于 TunFlow 核心的桌面控制层，目标是把 `tun2socks` 的底层能力包装成“一键启动、自动接管系统流量、规则分流”的桌面应用。

## 当前版本

第一阶段已经包含：

- SOCKS5 地址配置
- TUN 设备配置
- 全局代理 / 规则直连 / 全部直连
- CIDR 直连规则
- Windows 自动配置 TUN IPv4 地址
- Windows 默认路由切换到 TUN
- 自动为远程 SOCKS5 服务增加物理网卡直连的防环路路由
- 路由配置失败自动回滚，停止时恢复原始 TUN IPv4/DHCP/网关状态
- 网络切换或 SOCKS5 DNS 地址变化时自动重建路由并刷新核心网卡绑定
- 配置文件持久化
- Wails 桌面 UI

## 构建

桌面模块是独立 Go module，不影响核心程序现有的 `CGO_ENABLED=0` 多平台构建设置。

Windows：

```powershell
cd desktop
go mod tidy
go build -o TunFlow.exe .
```

Wails v2 需要桌面 WebView 运行环境；Windows 使用系统 WebView2。

## 重要说明

当前分流第一阶段按目标 IP/CIDR 执行。域名规则、DNS 防泄漏和 IPv6 系统路由自动化仍不在当前版本范围内。

Windows 自动路由属于系统级网络操作，需要管理员权限或等效权限。
