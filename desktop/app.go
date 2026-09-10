package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/engine"
)

type App struct {
	ctx context.Context
	mu sync.Mutex
	cfg Config
	err error
	up routeState
	runtimeInterface string
	networkSignature string
	trayEnd func()
	configWatchStop chan struct{}
	configWatchWG sync.WaitGroup
	configDiskChecksum string
	configLoaded bool
}

type Config struct {
	Proxy string `json:"proxy"`
	Device string `json:"device"`
	Interface string `json:"interface"`
	Mode string `json:"mode"`
	DirectCIDRs []string `json:"directCIDRs,omitempty"`
	DirectRules []string `json:"directRules,omitempty"`
	ProxyRules []string `json:"proxyRules,omitempty"`
	DefaultRoute string `json:"defaultRoute"`
	GeoIPFile string `json:"geoIPFile,omitempty"`
	AutoRoute bool `json:"autoRoute"`
	StartWithWindows bool `json:"startWithWindows"`
}

type Status struct {
	Running bool `json:"running"`
	Proxy string `json:"proxy"`
	Device string `json:"device"`
	Mode string `json:"mode"`
	AutoRoute bool `json:"autoRoute"`
	RouteReady bool `json:"routeReady"`
	LastError string `json:"lastError,omitempty"`
	StartWithWindows bool `json:"startWithWindows"`
}

type NetworkInterface struct {
	Name string `json:"name"`
	Addresses []string `json:"addresses"`
}

const (
	geoIPDownloadURL = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
	geoSiteDownloadURL = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
)

func NewApp() *App {
	return &App{cfg: Config{Device:"tun://TunFlow", Mode:"global", DirectCIDRs:[]string{}, DirectRules:[]string{}, ProxyRules:[]string{}, DefaultRoute:"proxy", GeoIPFile:"geoip.dat", AutoRoute:true}}
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	if err := a.loadConfigLocked(); err != nil {
		if os.IsNotExist(err) {
			if err := normalizeConfig(&a.cfg); err != nil { a.err = err; a.configLoaded = false } else if err := a.writeConfigLocked(); err != nil { a.err = err; a.configLoaded = false } else { a.err = nil; a.configLoaded = true }
		} else { a.err = err; a.configLoaded = false }
	}
	a.refreshConfigChecksumLocked()
	a.startConfigWatcherLocked()
	a.mu.Unlock()
	a.startSystemTray()
}

func (a *App) shutdownNoPersist(ctx context.Context) {
	_ = ctx
	a.mu.Lock()
	watchStop := a.configWatchStop
	a.configWatchStop = nil
	if watchStop != nil { close(watchStop) }
	trayEnd := a.trayEnd
	a.trayEnd = nil
	if engine.Running() || a.up.active { _ = a.stopLocked() }
	a.mu.Unlock()
	if watchStop != nil { a.configWatchWG.Wait() }
	if trayEnd != nil { trayEnd() }
}

func (a *App) GetConfig() Config { a.mu.Lock(); defer a.mu.Unlock(); return cloneConfig(a.cfg) }
func (a *App) GetConfigPath() string { return a.configPath() }
func (a *App) GetStatus() Status { a.mu.Lock(); defer a.mu.Unlock(); return Status{Running:engine.Running(), Proxy:a.cfg.Proxy, Device:a.cfg.Device, Mode:a.cfg.Mode, AutoRoute:a.cfg.AutoRoute, RouteReady:a.up.active, LastError:errString(a.err), StartWithWindows:a.cfg.StartWithWindows} }
func (a *App) GetTrafficStats() engine.TrafficStats { return engine.GetTrafficStats() }

func (a *App) GetNetworkInterfaces() ([]NetworkInterface, error) {
	interfaces, err := net.Interfaces(); if err != nil { return nil, fmt.Errorf("读取网络接口失败: %w", err) }
	tunName := strings.TrimPrefix(strings.TrimPrefix(a.GetConfig().Device, "tun://"), "tun:")
	result := make([]NetworkInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || strings.EqualFold(iface.Name, tunName) { continue }
		addrs, err := iface.Addrs(); if err != nil { continue }
		addresses := make([]string, 0, len(addrs)); for _, addr := range addrs { addresses = append(addresses, addr.String()) }
		result = append(result, NetworkInterface{Name:iface.Name, Addresses:addresses})
	}
	return result, nil
}

// SaveConfigJSON is the frontend-safe configuration write path. The UI passes
// the exact JSON payload it built from the input controls, avoiding any
// ambiguity in Wails' struct argument marshalling.
func (a *App) SaveConfigJSON(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("保存配置失败: 配置内容为空")
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return fmt.Errorf("保存配置失败: JSON 配置格式无效: %w", err)
	}
	if strings.TrimSpace(cfg.Proxy) == "" {
		return errors.New("保存配置失败: proxy 字段为空，请检查上游 SOCKS5 输入")
	}
	return a.SaveConfig(cfg)
}

func (a *App) SaveConfig(cfg Config) error {
	cfg.Proxy = normalizeProxy(cfg.Proxy)
	cfg.Device = strings.TrimSpace(cfg.Device); cfg.Interface = strings.TrimSpace(cfg.Interface); cfg.GeoIPFile = strings.TrimSpace(cfg.GeoIPFile)
	if err := normalizeConfig(&cfg); err != nil { return err }
	a.mu.Lock(); old := cloneConfig(a.cfg); a.cfg = cloneConfig(cfg)
	if err := a.writeConfigLocked(); err != nil { a.cfg = old; a.mu.Unlock(); return err }
	a.configLoaded = true; a.err = nil; a.mu.Unlock()
	if err := setStartWithWindows(cfg.StartWithWindows); err != nil { a.mu.Lock(); a.err = err; a.mu.Unlock(); return err }
	if engine.Running() {
		a.mu.Lock(); err := a.applyRunningConfigLocked(old); a.err = err; a.mu.Unlock()
		if err != nil { return fmt.Errorf("配置已保存，但运行中的核心未能完全应用: %w", err) }
	}
	return nil
}

func (a *App) Start() error { a.mu.Lock(); defer a.mu.Unlock(); err:=a.startLocked(); a.err=err; return err }
func (a *App) Stop() error { a.mu.Lock(); defer a.mu.Unlock(); err:=a.stopLocked(); a.err=err; return err }

func (a *App) startLocked() error {
	if engine.Running() { return nil }
	if err:=normalizeConfig(&a.cfg); err!=nil { return err }
	if strings.TrimSpace(a.cfg.Proxy)=="" { return errors.New("SOCKS5 地址不能为空") }
	if err:=checkProxyEndpoint(a.cfg.Proxy); err!=nil { return err }
	if a.cfg.AutoRoute { if err:=a.selectRuntimeInterfaceLocked(); err!=nil { return err } }
	engine.Insert(a.buildEngineKeyLocked(a.cfg))
	if err:=engine.StartE(); err!=nil { return fmt.Errorf("启动核心失败: %w", err) }
	if a.cfg.AutoRoute { if err:=a.setupRoutesLocked(); err!=nil { _=engine.StopE(); return err } }
	return nil
}

func (a *App) stopLocked() error { if a.up.active { if err:=a.teardownRoutesLocked(); err!=nil { return fmt.Errorf("清理 Windows 路由失败: %w", err) } }; if err:=engine.StopE(); err!=nil { return err }; return nil }

func (a *App) applyRunningConfigLocked(old Config) error {
	if !engine.Running() { return nil }
	newCfg:=a.cfg
	if strings.TrimSpace(newCfg.Proxy)=="" { return errors.New("运行中的 TunFlow 不能清空 SOCKS5 地址") }
	if old.Device!=newCfg.Device || old.Interface!=newCfg.Interface || old.AutoRoute!=newCfg.AutoRoute || old.Proxy!=newCfg.Proxy {
		if err:=a.stopLocked(); err!=nil { return err }
		if err:=a.startLocked(); err!=nil { a.cfg=old; _=a.startLocked(); return fmt.Errorf("重启核心应用新配置失败: %w", err) }
		return nil
	}
	if err:=engine.Reload(a.buildEngineKeyLocked(newCfg)); err!=nil { a.cfg=old; return fmt.Errorf("热更新核心失败: %w", err) }
	return nil
}

func (a *App) buildEngineKeyLocked(cfg Config) *engine.Key {
	iface:=strings.TrimSpace(cfg.Interface); if iface=="" { iface=a.runtimeInterface }
	geo:=cfg.GeoIPFile; if !filepath.IsAbs(geo) { if p,ok:=bundledDataPath(geo); ok { geo=p } }
	return &engine.Key{Proxy:normalizeProxy(cfg.Proxy), Device:strings.TrimSpace(cfg.Device), Interface:iface, RoutingMode:strings.TrimSpace(cfg.Mode), DirectCIDRs:append([]string(nil),cfg.DirectCIDRs...), DirectRules:append([]string(nil),cfg.DirectRules...), ProxyRules:append([]string(nil),cfg.ProxyRules...), DefaultRoute:cfg.DefaultRoute, GeoIPFile:geo, LogLevel:"info"}
}

func normalizeProxy(raw string) string { proxy:=strings.TrimSpace(raw); lower:=strings.ToLower(proxy); if strings.HasPrefix(lower,"s5://") { return "socks5://"+proxy[5:] }; if proxy!=""&&!strings.Contains(proxy,"://") { return "socks5://"+proxy }; return proxy }

func normalizeConfig(cfg *Config) error {
	if strings.TrimSpace(cfg.Device)=="" { return errors.New("TUN 设备不能为空") }
	switch cfg.Mode { case "", "global", "rules", "bypass", "direct": default: return fmt.Errorf("不支持的分流模式: %s",cfg.Mode) }
	if cfg.Mode=="" { cfg.Mode="global" }; if cfg.Mode=="bypass" { cfg.Mode="rules" }
	if cfg.DefaultRoute!="direct"&&cfg.DefaultRoute!="proxy" { cfg.DefaultRoute="proxy" }
	if cfg.DirectRules==nil { cfg.DirectRules=[]string{} }; if cfg.ProxyRules==nil { cfg.ProxyRules=[]string{} }; if cfg.GeoIPFile=="" { cfg.GeoIPFile="geoip.dat" }
	return nil
}

func cloneConfig(cfg Config) Config { out:=cfg; out.DirectCIDRs=append([]string(nil),cfg.DirectCIDRs...); out.DirectRules=append([]string(nil),cfg.DirectRules...); out.ProxyRules=append([]string(nil),cfg.ProxyRules...); return out }
func executableDir() (string,error) { exe,err:=os.Executable(); if err!=nil { return "",fmt.Errorf("获取程序目录失败: %w",err) }; return filepath.Dir(exe),nil }
func (a *App) configPath() string { dir,err:=executableDir(); if err!=nil { return "config.json" }; return filepath.Join(dir,"config.json") }
func bundledDataPath(name string)(string,bool){ if name=="" { return "",false }; if filepath.IsAbs(name){if _,err:=os.Stat(name);err==nil{return name,true};return name,false};dir,err:=executableDir();if err!=nil{return name,false};p:=filepath.Join(dir,name);if _,err:=os.Stat(p);err==nil{return p,true};return name,false }

func (a *App) loadConfigLocked() error { data,err:=os.ReadFile(a.configPath());if err!=nil{return err};data=bytesTrimBOMSpace(data);if len(data)==0{return errors.New("配置文件为空")};var cfg Config;if err:=json.Unmarshal(data,&cfg);err!=nil{return fmt.Errorf("JSON 配置格式无效: %w",err)};cfg.Proxy=normalizeProxy(cfg.Proxy);if err:=normalizeConfig(&cfg);err!=nil{return err};a.cfg=cfg;return nil }
func bytesTrimBOMSpace(data []byte) []byte { for len(data)>0&&(data[0]==' '||data[0]=='\t'||data[0]=='\r'||data[0]=='\n'){data=data[1:]};if len(data)>=3&&data[0]==0xEF&&data[1]==0xBB&&data[2]==0xBF{data=data[3:]};for len(data)>0&&(data[len(data)-1]==' '||data[len(data)-1]=='\t'||data[len(data)-1]=='\r'||data[len(data)-1]=='\n'){data=data[:len(data)-1]};return data }

func (a *App) writeConfigLocked() error {
	data,err:=json.MarshalIndent(a.cfg,"","  ");if err!=nil{return err};path:=a.configPath();dir:=filepath.Dir(path);tmp,err:=os.CreateTemp(dir,".config.json.tmp-*");if err!=nil{return fmt.Errorf("创建配置临时文件失败: %w",err)};tmpName:=tmp.Name();defer os.Remove(tmpName)
	if _,err=tmp.Write(data);err!=nil{_ = tmp.Close();return fmt.Errorf("写入配置临时文件失败: %w",err)};if err=tmp.Sync();err!=nil{_ = tmp.Close();return fmt.Errorf("刷新配置临时文件失败: %w",err)};if err=tmp.Close();err!=nil{return err};if err=replaceFileAtomic(tmpName,path);err!=nil{return fmt.Errorf("替换配置文件失败: %w",err)}
stored,err:=os.ReadFile(path);if err!=nil{return fmt.Errorf("保存后读取配置失败: %w",err)};if string(stored)!=string(data){return errors.New("配置保存校验失败")};a.configDiskChecksum=checksumBytes(data);return nil
}
func replaceFileAtomic(source,target string) error { if err:=os.Rename(source,target);err==nil{return nil};backup:=target+".bak";_ = os.Remove(backup);if err:=os.Rename(target,backup);err!=nil&& !os.IsNotExist(err){return err};if err:=os.Rename(source,target);err!=nil{_ = os.Rename(backup,target);return err};_ = os.Remove(backup);return nil }
func (a *App) refreshConfigChecksumLocked(){if data,err:=os.ReadFile(a.configPath());err==nil{a.configDiskChecksum=checksumBytes(data)}}
func checksumBytes(data []byte) string { sum:=sha256.Sum256(data);return hex.EncodeToString(sum[:]) }
func errString(err error)string{if err==nil{return ""};return err.Error()}

func (a *App) UpdateGeoIP() error { return a.updateRuleFile(geoIPDownloadURL,"geoip.dat","GeoIP") }
func (a *App) UpdateGeoSite() error { return a.updateRuleFile(geoSiteDownloadURL,"geosite.dat","GeoSite") }
func (a *App) updateRuleFile(source,name,label string)error{dir,err:=executableDir();if err!=nil{return err};client:=&http.Client{Timeout:2*time.Minute};tmp,err:=downloadRuleFile(client,source,dir,name);if err!=nil{return fmt.Errorf("更新 %s 失败: %w",label,err)};defer os.Remove(tmp);if err:=replaceRuleFile(tmp,filepath.Join(dir,name));err!=nil{return fmt.Errorf("替换 %s 文件失败: %w",label,err)};return nil}
func downloadRuleFile(client *http.Client,source,dir,name string)(string,error){resp,err:=client.Get(source);if err!=nil{return "",err};defer resp.Body.Close();if resp.StatusCode!=http.StatusOK{return "",fmt.Errorf("HTTP 状态码 %d",resp.StatusCode)};tmp,err:=os.CreateTemp(dir,"."+name+".tmp-*");if err!=nil{return "",err};path:=tmp.Name();if _,err=io.Copy(tmp,resp.Body);err!=nil{_ = tmp.Close();_ = os.Remove(path);return "",err};if err=tmp.Close();err!=nil{_ = os.Remove(path);return "",err};info,err:=os.Stat(path);if err!=nil{_ = os.Remove(path);return "",err};if info.Size()==0{_ = os.Remove(path);return "",errors.New("下载文件为空")};return path,nil}
func replaceRuleFile(source,target string)error{return replaceFileAtomic(source,target)}

func checkProxyEndpoint(raw string)error{raw=normalizeProxy(raw);u,err:=url.Parse(raw);if err!=nil{return fmt.Errorf("SOCKS5 地址无效: %w",err)};host,port:=u.Hostname(),u.Port();if host==""||port==""{return errors.New("SOCKS5 地址必须包含主机和端口")};conn,err:=net.DialTimeout("tcp",net.JoinHostPort(host,port),3*time.Second);if err!=nil{return fmt.Errorf("SOCKS5 入口不可达: %w",err)};_ = conn.Close();return nil}
