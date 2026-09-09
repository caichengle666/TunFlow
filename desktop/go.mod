module github.com/caichengle666/TunFlow/desktop

go 1.26.3

require (
	fyne.io/systray v0.0.0-20250603113521-ca66a66d8b58
	github.com/wailsapp/wails/v2 v2.15.0
	github.com/xjasonlyu/tun2socks/v2 v2.7.0
)

replace github.com/xjasonlyu/tun2socks/v2 => ..
