# TunFlow

TunFlow 是基于 `xjasonlyu/tun2socks` 核心构建的桌面化 TUN 流量管理项目。

目标不是重写成熟的网络转发内核，而是在保持核心稳定性的前提下，增加普通用户需要的桌面控制能力：

```text
系统应用
   ↓
Windows TUN 虚拟网卡
   ↓
TunFlow / tun2socks 核心
   ↓
分流策略
   ├── DIRECT
   └── SOCKS5
        ↓
     Internet
```

## 当前进度

### 核心

底层仍然使用原项目的 gVisor TCP/IP 栈、TUN、TCP/UDP 和代理实现。

### 桌面版

`desktop/` 已加入 Wails 桌面控制层，目前支持：

- SOCKS5 配置
- TUN 设备配置
- 全局代理 / 规则直连 / 全部直连
- CIDR 直连分流
- Windows 自动设置 TUN IPv4 地址
- Windows 自动添加 TUN 默认路由
- 自动为远程 SOCKS5 地址添加防环路主机路由
- 配置持久化
- 中文桌面控制面板

Windows 桌面程序构建方式：

```powershell
cd desktop
.\\build.ps1
```

也可以：

```powershell
cd desktop
go mod tidy
go build -o TunFlow.exe .
```

## 分流设计

第一阶段采用稳定的目标 IP/CIDR 策略。例如：

```text
192.168.0.0/16  → DIRECT
10.0.0.0/8      → DIRECT
172.16.0.0/12   → DIRECT
其他            → SOCKS5
```

策略层实现为现有 `proxy.Proxy` 接口的包装器，因此不会改变原有 TCP/UDP 处理链路。

后续计划包括：

- 域名规则
- GeoIP / GeoSite
- DNS 防泄漏
- IPv6 系统路由自动化
- 网络切换自动恢复
- Kill Switch
- 应用/进程级分流
- 系统托盘
- 安装器与自动更新

## 上游项目

TunFlow 基于 MIT License 的 `xjasonlyu/tun2socks` Fork 开发，并尽量保持核心代码与上游兼容。

上游项目：
https://github.com/xjasonlyu/tun2socks

## 许可证

MIT License
