package tray

import (
	"os"
	"runtime"

	"github.com/ProtocolScience/AstralGocq/icon"
	"github.com/getlantern/systray"
)

var (
	Icon          []byte
	QuitItem      *systray.MenuItem
	interruptChan chan os.Signal
)

func OnReady() {
	if runtime.GOOS == "windows" {
		Icon = icon.IconWindows
	} else {
		Icon = icon.IconUnix
	}
	systray.SetIcon(Icon)
	systray.SetTitle("gocq运行中")
	systray.SetTooltip("gocq is running in the background")

	// quit button
	QuitItem = systray.AddMenuItem("退出(Ctrl+C)", "退出程序")
	go func() {
		for {
			select {
			case <-QuitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func OnExit() {
	os.Exit(0)
}
