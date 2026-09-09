module github.com/caichengle666/TunFlow/desktop

go 1.26.3

require (
	github.com/getlantern/systray v1.2.2
	github.com/wailsapp/wails/v2 v2.15.0
	github.com/xjasonlyu/tun2socks/v2 v2.7.0
)

replace github.com/getlantern/systray => github.com/jimbertools/systray v0.0.0-20240321131420-7bf113ab6ac4
replace github.com/xjasonlyu/tun2socks/v2 => ..
