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

const version = "0.2.2"

func main() {
	cfg, _ := os.UserConfigDir()
	l := logx.New(filepath.Join(cfg, "FMTAssistant", "logs"))
	l.Log(logx.Info, "app", "FMT Assistant %s starting", version)

	app := NewApp(l)
	lock := &options.SingleInstanceLock{UniqueId: "va3mw-fmtassistant-1"}
	if os.Getenv("FMTASSISTANT_NO_LOCK") != "" { // development: run beside an installed copy
		lock = nil
	}
	err := wails.Run(&options.App{
		Title:     "FMT Assistant " + version,
		Width:     1280,
		Height:    900,
		MinWidth:  980,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour:   &options.RGBA{R: 245, G: 246, B: 248, A: 1},
		OnStartup:          app.startup,
		OnShutdown:         app.shutdown,
		Bind:               []interface{}{app},
		SingleInstanceLock: lock,
		Windows:            &windows.Options{},
	})
	if err != nil {
		l.Log(logx.Error, "app", "wails: %v", err)
	}
}
