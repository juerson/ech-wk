package main

import (
	"log"
	"os"
	"path/filepath"

	"fyne.io/fyne/v2/app"
	"github.com/juerson/ech-wk/client-gui-go/internal/ui"
)

func main() {
	// Ensure working directory is set to executable directory
	// This is crucial for relative paths (config, resources) to work
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		log.Printf("切换到工作目录: %s", dir)
		if err := os.Chdir(dir); err != nil {
			log.Printf("警告: 无法切换到工作目录: %v", err)
		}
	}

	log.Printf("创建Fyne应用...")
	a := app.New()

	// 设置应用图标
	if icon := ui.WindowIconResource(); icon != nil {
		a.SetIcon(icon)
		log.Printf("应用图标已设置")
	}

	log.Printf("创建主窗口...")
	w, err := ui.NewMainWindow(a)
	if err != nil {
		log.Fatalf("创建主窗口失败: %v", err)
	}

	log.Printf("初始化系统托盘...")
	ui.InitTray(a, w)

	log.Printf("显示并运行应用...")
	w.ShowAndRun()
}
