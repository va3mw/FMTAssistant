package main

import (
	"embed"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/va3mw/FMTAssistant/internal/logx"
)

//go:embed all:frontend/dist
var assets embed.FS

const version = "0.1.0"

func main() {
	cfg, _ := os.UserConfigDir()
	l := logx.New(filepath.Join(cfg, "FMTAssistant", "logs"))
	l.Log(logx.Info, "app", "FMT Assistant %s starting", version)

	app := NewApp(l)
	err := wails.Run(&options.App{
		Title:     "FMT Assistant " + version,
		Width:     1280,
		Height:    900,
		MinWidth:  980,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 246, B: 248, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "va3mw-fmtassistant-1",
		},
		Windows: &windows.Options{},
	})
	if err != nil {
		l.Log(logx.Error, "app", "wails: %v", err)
	}
}
