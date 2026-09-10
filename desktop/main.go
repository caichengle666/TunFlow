package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed frontend
var frontend embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:  "TunFlow",
		Width:  980,
		Height: 720,
		MinWidth: 860,
		MinHeight: 620,
		Frameless:       true,
		HideWindowOnClose: true,
		AssetServer: &assetserver.Options{
			Assets: frontend,
		},
		BackgroundColour: &options.RGBA{R: 13, G: 14, B: 21, A: 255},
		OnStartup:        app.startupFixed,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
