# TunFlow Desktop 0.1.3

TunFlow Desktop 是基于 TunFlow 核心的桌面控制层，目标是把 `tun2socks` 的底层能力包装成“一键启动、自动接管系统流量、规则分流”的桌面应用。

## 当前版本

第一阶段已经包含：

- SOCKS5 地址配置
- TUN 设备配置
- 全局代理 / 规则直连 / 全部直连
- CIDR 直连规则
- Windows 自动配置 TUN IPv4 地址
- Windows 默认路由切换到 TUN
- 自动为远程 SOCKS5 服务增加物理网卡直连的防环路由（支持多 IPv4）
- Windows 路由事务失败自动回滚，停止时先清理路由再停止核心
- 网络切换 / SOCKS5 DNS 地址变化自动检测并重建路由
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

当前分流第一阶段按目标 IP/CIDR 执行。完整的域名规则、GeoIP/GeoSite、DNS 防泄漏和 IPv6 路由自动化仍会在后续阶段加入。Windows 网络切换自动恢复已加入当前版本。

Windows 自动路由属于系统级网络操作，需要管理员权限或等效权限。
