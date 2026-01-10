package ui

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	autostart "github.com/juerson/ech-wk/client-gui-go/internal/autostart"
	"github.com/juerson/ech-wk/client-gui-go/internal/config"
	"github.com/juerson/ech-wk/client-gui-go/internal/process"
	sysproxy "github.com/juerson/ech-wk/client-gui-go/internal/sysproxy"
)

//go:embed app.png
var appIconData []byte

func trayIconResource() fyne.Resource {
	if len(appIconData) == 0 {
		return nil
	}
	return fyne.NewStaticResource("app.png", appIconData)
}

func WindowIconResource() fyne.Resource {
	if len(appIconData) == 0 {
		return nil
	}
	return fyne.NewStaticResource("app.png", appIconData)
}

// LogEntry represents a single log entry with metadata
type LogEntry struct {
	Timestamp time.Time
	Level     string // "INFO", "WARN", "ERROR", "SYSTEM"
	Message   string
}

// LogBuffer manages log entries with intelligent limiting
type LogBuffer struct {
	entries     []LogEntry
	maxEntries  int
	maxSize     int // maximum total size in bytes
	mu          sync.RWMutex
	currentSize int
}

// NewLogBuffer creates a new log buffer with intelligent limits
func NewLogBuffer(maxEntries int, maxSize int) *LogBuffer {
	return &LogBuffer{
		entries:    make([]LogEntry, 0, maxEntries),
		maxEntries: maxEntries,
		maxSize:    maxSize,
	}
}

// Add adds a new log entry with intelligent truncation
func (lb *LogBuffer) Add(entry LogEntry) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	entrySize := len(entry.Message) + 20 // approximate size including metadata

	// Remove oldest entries if we exceed limits
	for (len(lb.entries) >= lb.maxEntries || lb.currentSize+entrySize > lb.maxSize) && len(lb.entries) > 0 {
		oldSize := len(lb.entries[0].Message) + 20
		lb.entries = lb.entries[1:]
		lb.currentSize -= oldSize
	}

	lb.entries = append(lb.entries, entry)
	lb.currentSize += entrySize
}

// GetText returns all log entries as formatted text, optionally filtered by level
func (lb *LogBuffer) GetText(filterLevel string) string {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var sb strings.Builder
	for _, entry := range lb.entries {
		// Apply filter if not "ALL"
		if filterLevel != "ALL" && entry.Level != filterLevel {
			continue
		}
		sb.WriteString(entry.Message)
		if !strings.HasSuffix(entry.Message, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Clear removes all log entries
func (lb *LogBuffer) Clear() {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.entries = lb.entries[:0]
	lb.currentSize = 0
}

type MainWindow struct {
	w fyne.Window

	cfg *config.Manager
	run *process.Runner

	servers      *widget.Select
	serverNameTo map[string]string

	addBtn   *widget.Button
	saveBtn  *widget.Button
	delBtn   *widget.Button
	clearBtn *widget.Button

	name   *widget.Entry
	server *MaskedEntry
	listen *widget.Entry
	token  *widget.Entry
	ip     *widget.Entry
	dns    *widget.Entry
	ech    *widget.Entry

	routing *widget.Select

	proxyCheck     *widget.Check
	autoStartCheck *widget.Check

	systemProxyEnabled bool

	startBtn *widget.Button
	stopBtn  *widget.Button

	logBox    *widget.Entry
	logScroll *container.Scroll
	logBuffer *LogBuffer
	logFilter *widget.Select

	trayEnabled bool
	isHidden    bool

	running         bool
	lastServer      string
	lastRouting     string
	lastSystemProxy bool
	lastAutoStart   bool

	suppressChanges bool

	lastName   string
	lastSrv    string
	lastListen string
	lastToken  string
	lastIP     string
	lastDNS    string
	lastECH    string
	lastLog    string

	mu sync.Mutex

	// Log optimization fields
	logUpdateTimer    *time.Timer
	logUpdateInterval time.Duration
	pendingLogCount   int32
	lastUIUpdate      time.Time
	logLevelFilter    string // Current log level filter
}

var mainWindowInstance *MainWindow

func InitTray(a fyne.App, w fyne.Window) {
	if mainWindowInstance != nil {
		mainWindowInstance.initTray(a)
		mainWindowInstance.installCloseHandler(a)
	}
}

func NewMainWindow(a fyne.App) (fyne.Window, error) {
	log.Printf("初始化配置管理器...")
	cfg, err := config.NewManager()
	if err != nil {
		log.Printf("创建配置管理器失败: %v", err)
		return nil, err
	}

	log.Printf("加载配置文件...")
	if err := cfg.Load(); err != nil {
		log.Printf("加载配置失败: %v", err)
		// 配置加载失败不应该阻止程序启动
	}

	log.Printf("创建主窗口实例...")
	mw := &MainWindow{
		cfg: cfg,
		run: process.NewRunner(),
		// Initialize optimized log buffer
		logBuffer:         NewLogBuffer(1000, 50*1024), // 1000 entries, 50KB max
		logUpdateInterval: 100 * time.Millisecond,      // Update UI every 100ms max
		lastUIUpdate:      time.Now(),
		logLevelFilter:    "ALL", // Show all logs by default
	}

	// 设置全局实例
	mainWindowInstance = mw

	log.Printf("创建Fyne窗口...")
	mw.w = a.NewWindow("ECH Workers Client")
	mw.w.Resize(fyne.NewSize(760, 650))
	mw.w.CenterOnScreen()

	log.Printf("设置窗口图标...")
	if icon := WindowIconResource(); icon != nil {
		mw.w.SetIcon(icon)
		a.SetIcon(icon)
		log.Printf("窗口图标已设置")
	} else {
		log.Printf("警告：无法加载窗口图标")
	}

	log.Printf("初始化控件...")
	mw.initControls()

	log.Printf("构建布局...")
	content := mw.buildLayout()
	mw.w.SetContent(content)

	mw.ensureAtLeastOneServer()
	mw.refreshServerSelect()
	mw.loadCurrentToForm()

	// Restore checkboxes (do not apply proxy immediately unless we are running)
	mw.setProxyCheckSilently(mw.cfg.Model.LastState.SystemProxyEnabled)

	if mw.cfg.Model.LastState.AutoStartChecked {
		mw.setAutoStartCheckSilently(true)
	} else {
		if en, err := autostart.IsEnabled(); err == nil {
			mw.setAutoStartCheckSilently(en)
		}
	}

	// 恢复上次状态并检查是否需要自动启动代理
	mw.restoreLastState()

	// 初始化日志显示 - 确保初始状态正确
	mw.updateLogUI()

	log.Printf("主窗口创建完成")
	return mw.w, nil
}

func (mw *MainWindow) initControls() {
	mw.name = widget.NewEntry()
	mw.name.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.name, mw.lastName)
			return
		}
		mw.lastName = s
	}
	mw.server = NewMaskedEntry(8)
	mw.server.OnChanged = func(_ string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setMaskedEntryRealSilently(mw.server, mw.lastSrv)
			return
		}
		mw.lastSrv = mw.server.Real()
	}
	mw.listen = widget.NewEntry()
	mw.listen.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.listen, mw.lastListen)
			return
		}
		mw.lastListen = s
	}
	mw.token = widget.NewPasswordEntry()
	mw.token.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.token, mw.lastToken)
			return
		}
		mw.lastToken = s
	}
	mw.ip = widget.NewPasswordEntry()
	mw.ip.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.ip, mw.lastIP)
			return
		}
		mw.lastIP = s
	}
	mw.dns = widget.NewEntry()
	mw.dns.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.dns, mw.lastDNS)
			return
		}
		mw.lastDNS = s
	}
	mw.ech = widget.NewEntry()
	mw.ech.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.ech, mw.lastECH)
			return
		}
		mw.lastECH = s
	}

	mw.routing = widget.NewSelect([]string{"global", "bypass_cn", "none"}, func(string) {})
	mw.routing.OnChanged = func(v string) {
		if mw.running {
			if mw.routing.Selected != mw.lastRouting {
				mw.routing.SetSelected(mw.lastRouting)
			}
			return
		}
		mw.lastRouting = v
		mw.onRoutingChanged()
	}

	mw.proxyCheck = widget.NewCheck("系统代理", func(v bool) {
		if mw.running {
			if mw.proxyCheck.Checked != mw.lastSystemProxy {
				mw.proxyCheck.SetChecked(mw.lastSystemProxy)
			}
			return
		}
		mw.lastSystemProxy = v
		mw.onProxyChanged()
	})
	mw.autoStartCheck = widget.NewCheck("开机启动", func(v bool) {
		if mw.running {
			if mw.autoStartCheck.Checked != mw.lastAutoStart {
				mw.autoStartCheck.SetChecked(mw.lastAutoStart)
			}
			return
		}
		mw.lastAutoStart = v
		mw.onAutoStartChanged()
	})

	mw.logBox = widget.NewMultiLineEntry()
	mw.logBox.Wrapping = fyne.TextWrapWord
	mw.logBox.OnChanged = func(s string) {
		if mw.suppressChanges {
			return
		}
		if mw.running {
			mw.setEntryTextSilently(mw.logBox, mw.lastLog)
			return
		}
		mw.lastLog = s
	}
	mw.logBox.Enable()
	mw.logScroll = container.NewVScroll(mw.logBox)

	// Initialize log level filter
	mw.logFilter = widget.NewSelect([]string{"ALL", "ERROR", "WARN", "SYSTEM", "INFO"}, func(selected string) {
		mw.mu.Lock()
		mw.logLevelFilter = selected
		mw.mu.Unlock()
		// Update UI with new filter
		mw.updateLogUI()
	})
	mw.logFilter.SetSelected("ALL")
	mw.logFilter.Resize(fyne.NewSize(80, 25))

	mw.servers = widget.NewSelect([]string{}, func(selected string) {
		if mw.running {
			if mw.servers.Selected != mw.lastServer {
				mw.servers.SetSelected(mw.lastServer)
			}
			return
		}
		mw.lastServer = selected
		mw.onServerSelected(selected)
	})

	mw.addBtn = widget.NewButton("新增", func() { mw.addServer() })
	mw.saveBtn = widget.NewButton("保存", func() { mw.saveFormToCurrent(true) })
	mw.delBtn = widget.NewButton("删除", func() { mw.deleteCurrentServer() })
	mw.clearBtn = widget.NewButton("清空日志", func() {
		// Asynchronous clear with debouncing
		go func() {
			mw.mu.Lock()
			mw.lastLog = ""
			mw.logBuffer.Clear()
			mw.mu.Unlock()

			// Immediate UI update for clear operation
			fyne.Do(func() {
				mw.mu.Lock()
				mw.suppressChanges = true
				mw.logBox.SetText("")
				mw.lastLog = ""
				mw.suppressChanges = false
				mw.mu.Unlock()
			})
		}()
	})
	mw.clearBtn.Importance = widget.MediumImportance

	mw.startBtn = widget.NewButton("启动代理", func() {
		mw.onStart()
	})
	mw.startBtn.Importance = widget.MediumImportance
	mw.stopBtn = widget.NewButton("停止", func() {
		mw.shutdown()
	})
	mw.stopBtn.Importance = widget.DangerImportance
	mw.stopBtn.Disable()
}

func (mw *MainWindow) installCloseHandler(a fyne.App) {
	closing := false
	mw.w.SetCloseIntercept(func() {
		if closing {
			mw.w.Close()
			return
		}

		if mw.trayEnabled {
			// 隐藏到托盘而不是退出
			mw.isHidden = true
			mw.w.Hide()
			return
		}
		// 如果没有托盘，则正常退出
		closing = true
		mw.shutdown()
		mw.w.Close()
		if a != nil {
			a.Quit()
		}
	})
}

func (mw *MainWindow) initTray(a fyne.App) {
	da, ok := a.(desktop.App)
	if !ok {
		return
	}

	res := trayIconResource()
	if res == nil {
		res = theme.FyneLogo()
		log.Printf("使用默认图标")
	} else {
		log.Printf("使用自定义托盘图标")
	}

	// Always set a tray icon; avoids "Failed to convert systray icon" from missing/invalid resources.
	da.SetSystemTrayIcon(res)
	// 不要覆盖应用图标，保持窗口图标设置
	// a.SetIcon(res)
	// mw.w.SetIcon(res)

	showHide := fyne.NewMenuItem("显示/隐藏", func() {
		if !mw.isHidden {
			mw.isHidden = true
			mw.w.Hide()
		} else {
			mw.isHidden = false
			// 确保窗口在任务栏中正确显示
			mw.w.Show()
			mw.w.RequestFocus()
			// 强制刷新窗口内容以确保任务栏图标显示
			currentContent := mw.w.Content()
			mw.w.SetContent(currentContent)
		}
	})

	quit := fyne.NewMenuItem("退出", func() {
		mw.shutdown()
		a.Quit()
	})

	menu := fyne.NewMenu("ECH Workers", showHide, fyne.NewMenuItemSeparator(), quit)
	da.SetSystemTrayMenu(menu)
	mw.trayEnabled = true
	log.Printf("系统托盘初始化完成")
}

func (mw *MainWindow) shutdown() {
	// Clean up log timer
	mw.cleanupLogTimer()

	// Best-effort stop child process
	if mw.run != nil && mw.run.IsRunning() {
		mw.run.Stop()
	}
	// Ensure UI state and last_state are updated and proxy is cleared
	mw.onStopped()
	// Persist checkbox preference even if not running
	mw.cfg.Model.LastState.SystemProxyEnabled = mw.proxyCheck.Checked
	mw.cfg.Model.LastState.AutoStartChecked = mw.autoStartCheck.Checked
	_ = mw.cfg.Save()
}

func (mw *MainWindow) buildLayout() fyne.CanvasObject {
	serverBar := container.NewBorder(nil, nil,
		widget.NewLabel("选择服务器"),
		container.NewHBox(
			mw.servers,
			mw.addBtn,
			mw.saveBtn,
			mw.delBtn,
		),
	)

	form := widget.NewForm(
		widget.NewFormItem("名称", mw.name),
		widget.NewFormItem("服务地址(*)", mw.server),
		widget.NewFormItem("监听地址(*)", mw.listen),
		widget.NewFormItem("身份令牌", mw.token),
		widget.NewFormItem("优选地址(*)", mw.ip),
		widget.NewFormItem("ECH域名", mw.ech),
		widget.NewFormItem("DOH服务", mw.dns),
		widget.NewFormItem("代理模式", mw.routing),
	)

	startWrap := container.NewGridWrap(fyne.NewSize(140, 44), mw.startBtn)
	stopWrap := container.NewGridWrap(fyne.NewSize(140, 44), mw.stopBtn)
	clearWrap := container.NewGridWrap(fyne.NewSize(120, 40), mw.clearBtn)

	leftControls := container.NewVBox(
		mw.proxyCheck,
		layout.NewSpacer(),
		mw.autoStartCheck,
	)

	buttons := container.NewHBox(
		container.NewCenter(leftControls),
		container.NewGridWrap(fyne.NewSize(4, 1), widget.NewLabel("")),
		container.NewCenter(startWrap),
		container.NewCenter(stopWrap),
		layout.NewSpacer(),
		container.NewCenter(clearWrap),
	)

	logHeader := container.NewBorder(nil, nil,
		widget.NewLabel("运行日志"),
		container.NewHBox(
			widget.NewLabel("日志级别:"),
			mw.logFilter,
		),
		widget.NewLabel(""))

	logGroup := container.NewBorder(logHeader, nil, nil, nil, mw.logScroll)

	contentTop := container.NewVBox(serverBar, form, buttons)
	return container.NewBorder(contentTop, nil, nil, nil, logGroup)
}

func (mw *MainWindow) ensureAtLeastOneServer() {
	if len(mw.cfg.Model.Servers) > 0 {
		return
	}
	def := config.Server{
		ID:          newID(),
		Name:        "默认服务器",
		Server:      "example.com:443",
		Listen:      "127.0.0.1:30000",
		Token:       "",
		IP:          "saas.sin.fan",
		DNS:         "dns.alidns.com/dns-query",
		ECH:         "cloudflare-ech.com",
		RoutingMode: "bypass_cn",
	}
	mw.cfg.Model.Servers = []config.Server{def}
	mw.cfg.Model.CurrentServerID = def.ID
	_ = mw.cfg.Save()
}

func (mw *MainWindow) refreshServerSelect() {
	mw.serverNameTo = map[string]string{}
	names := make([]string, 0, len(mw.cfg.Model.Servers))
	for _, s := range mw.cfg.Model.Servers {
		names = append(names, s.Name)
		mw.serverNameTo[s.Name] = s.ID
	}
	mw.servers.Options = names
	mw.servers.Refresh()

	cur, ok := mw.cfg.GetCurrentServer()
	if ok {
		mw.setServerSelectSilently(cur.Name)
	}
}

func (mw *MainWindow) setServerSelectSilently(name string) {
	on := mw.servers.OnChanged
	mw.servers.OnChanged = nil
	mw.servers.SetSelected(name)
	mw.servers.OnChanged = on
}

func (mw *MainWindow) onServerSelected(selected string) {
	id := mw.serverNameTo[selected]
	if id == "" {
		return
	}
	mw.saveFormToCurrent(false)
	mw.cfg.SetCurrentServer(id)
	_ = mw.cfg.Save()
	mw.loadCurrentToForm()
}

func (mw *MainWindow) loadCurrentToForm() {
	s, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}
	mw.suppressChanges = true
	mw.name.SetText(s.Name)
	mw.server.SetReal(s.Server)
	mw.listen.SetText(s.Listen)
	mw.token.SetText(s.Token)
	mw.ip.SetText(s.IP)
	mw.dns.SetText(s.DNS)
	mw.ech.SetText(s.ECH)
	if s.RoutingMode == "" {
		s.RoutingMode = "bypass_cn"
	}
	mw.routing.SetSelected(s.RoutingMode)
	mw.setServerSelectSilently(s.Name)
	mw.suppressChanges = false

	// update snapshots used for "lock while running"
	mw.lastName = s.Name
	mw.lastSrv = s.Server
	mw.lastListen = s.Listen
	mw.lastToken = s.Token
	mw.lastIP = s.IP
	mw.lastDNS = s.DNS
	mw.lastECH = s.ECH
	mw.lastRouting = s.RoutingMode
	mw.lastServer = s.Name
}

func (mw *MainWindow) saveFormToCurrent(showToast bool) {
	s, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}

	s.Name = strings.TrimSpace(mw.name.Text)
	s.Server = strings.TrimSpace(mw.server.Real())
	s.Listen = strings.TrimSpace(mw.listen.Text)
	s.Token = mw.token.Text
	s.IP = mw.ip.Text
	s.DNS = strings.TrimSpace(mw.dns.Text)
	s.ECH = strings.TrimSpace(mw.ech.Text)
	s.RoutingMode = mw.routing.Selected
	mw.cfg.UpsertServer(s)
	mw.cfg.SetCurrentServer(s.ID)
	_ = mw.cfg.Save()
	mw.refreshServerSelect()

	if showToast {
		dialog.ShowInformation("提示", fmt.Sprintf("服务器 \"%s\" 配置已保存", s.Name), mw.w)
	}
}

func (mw *MainWindow) addServer() {
	nameEntry := widget.NewEntry()
	nameEntry.SetText("新服务器")
	form := widget.NewForm(widget.NewFormItem("服务器名称", nameEntry))
	d := dialog.NewForm("新增服务器", "确定", "取消", form.Items, func(ok bool) {
		if !ok {
			return
		}
		name := strings.TrimSpace(nameEntry.Text)
		if name == "" {
			dialog.ShowError(errors.New("服务器名称不能为空"), mw.w)
			return
		}
		for _, s := range mw.cfg.Model.Servers {
			if s.Name == name {
				dialog.ShowError(errors.New("服务器名称已存在"), mw.w)
				return
			}
		}

		cur, _ := mw.cfg.GetCurrentServer()
		newS := config.Server{
			ID:          newID(),
			Name:        name,
			Server:      cur.Server,
			Listen:      cur.Listen,
			Token:       cur.Token,
			IP:          cur.IP,
			DNS:         cur.DNS,
			ECH:         cur.ECH,
			RoutingMode: cur.RoutingMode,
		}
		mw.cfg.UpsertServer(newS)
		mw.cfg.SetCurrentServer(newS.ID)
		_ = mw.cfg.Save()
		mw.refreshServerSelect()
		mw.loadCurrentToForm()
	}, mw.w)
	d.Resize(fyne.NewSize(420, 180))
	d.Show()
}

func (mw *MainWindow) deleteCurrentServer() {
	if len(mw.cfg.Model.Servers) <= 1 {
		dialog.ShowInformation("提示", "至少需要保留一个服务器配置", mw.w)
		return
	}
	cur, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}
	dialog.ShowConfirm("确认删除", fmt.Sprintf("确定要删除服务器 \"%s\" 吗？", cur.Name), func(ok bool) {
		if !ok {
			return
		}
		mw.cfg.DeleteServer(cur.ID)
		_ = mw.cfg.Save()
		mw.refreshServerSelect()
		mw.loadCurrentToForm()
	}, mw.w)
}

func (mw *MainWindow) onStart() {
	mw.saveFormToCurrent(false)
	cur, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}
	if strings.TrimSpace(cur.Server) == "" {
		dialog.ShowInformation("提示", "请输入服务地址", mw.w)
		return
	}
	if strings.TrimSpace(cur.Listen) == "" {
		dialog.ShowInformation("提示", "请输入监听地址", mw.w)
		return
	}

	mw.appendLog(fmt.Sprintf("[系统] 正在启动服务器: %s\n", cur.Name))

	// 检测运行模式：根据用户偏好和外部程序可用性决定
	mode := process.ModeEmbedded // 默认内嵌模式

	preferredMode := mw.cfg.Model.LastState.PreferredMode
	if preferredMode == 0 {
		// 自动检测模式
		if _, err := process.FindEchWorkersExe(); err == nil {
			// 外部exe存在，使用外部模式
			mode = process.ModeExternal
			mw.appendLog("[系统] 检测到外部程序，使用外部进程模式\n")
		} else {
			// 外部exe不存在，使用内嵌模式
			mw.appendLog("[系统] 未检测到外部程序，使用内嵌模式\n")
		}
	} else if preferredMode == 2 {
		// 强制外部模式
		mode = process.ModeExternal
		mw.appendLog("[系统] 用户指定使用外部进程模式\n")
		if _, err := process.FindEchWorkersExe(); err != nil {
			dialog.ShowError(errors.New("外部程序文件不存在，请将ech-workers.exe放在同一目录下"), mw.w)
			return
		}
	} else {
		// 强制内嵌模式
		mw.appendLog("[系统] 用户指定使用内嵌模式\n")
	}

	err := mw.run.Start(process.Config{
		Server:      cur.Server,
		Listen:      cur.Listen,
		Token:       cur.Token,
		IP:          cur.IP,
		DNS:         cur.DNS,
		ECH:         cur.ECH,
		RoutingMode: cur.RoutingMode,
		Mode:        mode,
	}, func(line string) {
		mw.appendLog(line)
	})
	if err != nil {
		dialog.ShowError(err, mw.w)
		mw.appendLog("[系统] 错误: 启动失败\n")
		return
	}
	mw.setRunningState(true)

	// 记录 last_state
	mw.cfg.Model.LastState.WasRunning = true
	_ = mw.cfg.Save()

	// 如果勾选了系统代理，尝试设置
	if mw.proxyCheck.Checked {
		mw.tryEnableProxyFromUI()
	}
}

func (mw *MainWindow) onStopped() {
	// 停止时自动清理系统代理
	if mw.systemProxyEnabled {
		if err := sysproxy.Set(false, ""); err == nil {
			mw.appendLog("[系统] 已自动清理系统代理\n")
		}
		mw.systemProxyEnabled = false
		mw.cfg.Model.LastState.SystemProxyEnabled = false
		_ = mw.cfg.Save()
		mw.setProxyCheckSilently(false)
	}

	mw.setRunningState(false)
	mw.cfg.Model.LastState.WasRunning = false
	_ = mw.cfg.Save()
}

func (mw *MainWindow) setRunningState(running bool) {
	mw.running = running
	if running {
		mw.lastServer = mw.servers.Selected
		mw.lastRouting = mw.routing.Selected
		mw.lastSystemProxy = mw.proxyCheck.Checked
		mw.lastAutoStart = mw.autoStartCheck.Checked
		mw.lastName = mw.name.Text
		mw.lastSrv = mw.server.Real()
		mw.lastListen = mw.listen.Text
		mw.lastToken = mw.token.Text
		mw.lastIP = mw.ip.Text
		mw.lastDNS = mw.dns.Text
		mw.lastECH = mw.ech.Text
		mw.lastLog = mw.logBox.Text
	}
	if mw.addBtn != nil {
		if running {
			mw.addBtn.Disable()
			mw.saveBtn.Disable()
			mw.delBtn.Disable()
		} else {
			mw.addBtn.Enable()
			mw.saveBtn.Enable()
			mw.delBtn.Enable()
		}
	}

	if running {
		mw.startBtn.Disable()
		mw.stopBtn.Enable()
		return
	}
	mw.startBtn.Enable()
	mw.stopBtn.Disable()
}

func (mw *MainWindow) setEntryTextSilently(e *widget.Entry, s string) {
	if e == nil {
		return
	}
	mw.suppressChanges = true
	e.SetText(s)
	mw.suppressChanges = false
}

func (mw *MainWindow) setMaskedEntryRealSilently(e *MaskedEntry, real string) {
	if e == nil {
		return
	}
	mw.suppressChanges = true
	e.SetReal(real)
	mw.suppressChanges = false
}

type MaskedEntry struct {
	widget.Entry

	real     string
	focused  bool
	suppress bool
}

func NewMaskedEntry(maskTail int) *MaskedEntry {
	me := &MaskedEntry{}
	me.ExtendBaseWidget(me)
	return me
}

func (m *MaskedEntry) Real() string {
	return m.real
}

func (m *MaskedEntry) SetReal(s string) {
	m.real = s
	m.applyDisplay()
}

func (m *MaskedEntry) FocusGained() {
	m.focused = true
	m.applyDisplay()
	m.Entry.FocusGained()
}

func (m *MaskedEntry) FocusLost() {
	m.focused = false
	m.applyDisplay()
	m.Entry.FocusLost()
}

func (m *MaskedEntry) applyDisplay() {
	if m.suppress {
		return
	}
	m.suppress = true
	if m.focused {
		m.Entry.SetText(m.real)
		m.suppress = false
		return
	}
	m.Entry.SetText(maskServerAddress(m.real))
	m.suppress = false
}

func (m *MaskedEntry) TypedRune(r rune) {
	if !m.focused {
		return
	}
	m.Entry.TypedRune(r)
	if !m.suppress {
		m.real = m.Entry.Text
	}
}

func (m *MaskedEntry) TypedKey(e *fyne.KeyEvent) {
	if !m.focused {
		return
	}
	m.Entry.TypedKey(e)
	if !m.suppress {
		m.real = m.Entry.Text
	}
}

func maskServerAddress(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// If it looks like a URL, parse it as URL.
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			host := u.Host
			port := ""
			if h, p, err := net.SplitHostPort(host); err == nil {
				host, port = h, p
			}
			u.Host = joinHostPort(maskHost(host), port)
			return u.String()
		}
	}

	// Otherwise treat it as host[:port][/path...]
	hostport, rest := splitHostRest(s)
	host := hostport
	port := ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	return joinHostPort(maskHost(host), port) + rest
}

func splitHostRest(s string) (hostport, rest string) {
	idx := strings.Index(s, "/")
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx:]
}

func joinHostPort(host, port string) string {
	if port == "" {
		return host
	}
	// Keep IPv6 formatting if needed.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

func maskHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "" {
		return host
	}

	// IPv4: keep first+last octet, mask middle.
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return fmt.Sprintf("%d.***.%d", v4[0], v4[3])
		}
		// IPv6: keep prefix/suffix chunks
		if len(host) > 8 {
			return host[:4] + "****" + host[len(host)-4:]
		}
		return "****"
	}

	lower := strings.ToLower(host)
	if strings.HasSuffix(lower, ".workers.dev") {
		return "***.workers.dev"
	}
	if strings.HasSuffix(lower, ".pages.dev") {
		return "***.pages.dev"
	}

	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		// keep the last 2 labels, mask the rest
		keep := strings.Join(parts[len(parts)-2:], ".")
		if len(parts) == 2 {
			// only one label + tld: mask that label partially
			label := parts[0]
			if len(label) <= 2 {
				return "*." + keep
			}
			return strings.Repeat("*", len(label)-2) + label[len(label)-2:] + "." + parts[1]
		}
		return "***." + keep
	}

	// fallback
	if len(host) <= 3 {
		return "***"
	}
	return strings.Repeat("*", len(host)-3) + host[len(host)-3:]
}

// parseLogLevel extracts log level from message
func parseLogLevel(message string) string {
	message = strings.TrimSpace(message)
	if strings.HasPrefix(message, "[错误]") || strings.Contains(message, "ERROR") || strings.Contains(message, "error") {
		return "ERROR"
	}
	if strings.HasPrefix(message, "[警告]") || strings.Contains(message, "WARN") || strings.Contains(message, "warn") {
		return "WARN"
	}
	if strings.HasPrefix(message, "[系统]") || strings.Contains(message, "SYSTEM") {
		return "SYSTEM"
	}
	return "INFO"
}

func (mw *MainWindow) scheduleLogUpdate() {
	if mw.logUpdateTimer != nil {
		mw.logUpdateTimer.Stop()
	}
	delay := mw.logUpdateInterval
	if time.Since(mw.lastUIUpdate) > 500*time.Millisecond {
		delay = 50 * time.Millisecond
	}
	mw.logUpdateTimer = time.AfterFunc(delay, func() {
		mw.updateLogUI()
	})
}

// updateLogUI updates the log UI with buffered content
func (mw *MainWindow) updateLogUI() {
	fyne.Do(func() {
		mw.mu.Lock()
		defer mw.mu.Unlock()

		if mw.logBuffer == nil {
			return
		}

		// Get current filter level safely - default to "ALL" if not set
		filterLevel := mw.logLevelFilter
		if filterLevel == "" {
			filterLevel = "ALL"
		}

		mw.suppressChanges = true
		currentText := mw.logBuffer.GetText(filterLevel)
		mw.logBox.SetText(currentText)
		mw.lastLog = currentText

		if mw.logScroll != nil {
			mw.logScroll.ScrollToBottom()
		}
		// Ensure cursor is at end to help auto-scroll
		mw.logBox.CursorRow = strings.Count(currentText, "\n")
		mw.logBox.Refresh()

		mw.suppressChanges = false
		mw.lastUIUpdate = time.Now()
	})
}

func (mw *MainWindow) setLogText(s string) {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	if mw.logBuffer != nil {
		mw.logBuffer.Clear()
		if s != "" {
			mw.logBuffer.Add(LogEntry{
				Timestamp: time.Now(),
				Level:     "SYSTEM",
				Message:   s,
			})
		}
		// Get current filter level safely - default to "ALL" if not set
		filterLevel := mw.logLevelFilter
		if filterLevel == "" {
			filterLevel = "ALL"
		}
		currentText := mw.logBuffer.GetText(filterLevel)
		fyne.Do(func() {
			mw.suppressChanges = true
			mw.logBox.SetText(currentText)
			mw.lastLog = currentText
			if mw.logScroll != nil {
				mw.logScroll.ScrollToBottom()
			}
			mw.suppressChanges = false
		})
	} else {
		// Fallback to immediate update
		fyne.Do(func() {
			mw.suppressChanges = true
			mw.logBox.SetText(s)
			mw.lastLog = s
			if mw.logScroll != nil {
				mw.logScroll.ScrollToBottom()
			}
			mw.suppressChanges = false
		})
	}
}

func (mw *MainWindow) appendLog(s string) {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	// If log was cleared, reset suppressChanges
	if mw.lastLog == "" && mw.logBox.Text == "" {
		mw.suppressChanges = false
	}

	// Ensure log ends with newline
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}

	// Add to buffer with intelligent limiting
	if mw.logBuffer != nil {
		logLevel := parseLogLevel(s)
		mw.logBuffer.Add(LogEntry{
			Timestamp: time.Now(),
			Level:     logLevel,
			Message:   s,
		})

		// Schedule debounced UI update
		mw.scheduleLogUpdate()
	} else {
		// Fallback to immediate update
		fyne.Do(func() {
			mw.suppressChanges = true
			currentText := mw.logBox.Text
			currentText += s

			// Intelligent log size limiting
			const maxLogSize = 20 * 1024
			if len(currentText) > maxLogSize {
				cutPos := len(currentText) - maxLogSize
				if idx := strings.Index(currentText[cutPos:], "\n"); idx != -1 {
					currentText = currentText[cutPos+idx+1:]
				} else {
					currentText = currentText[cutPos:]
				}
			}

			mw.logBox.SetText(currentText)
			mw.lastLog = mw.logBox.Text

			if mw.logScroll != nil {
				mw.logScroll.ScrollToBottom()
			}
			mw.logBox.CursorRow = strings.Count(currentText, "\n")
			mw.logBox.Refresh()

			mw.suppressChanges = false
		})
	}
}

// cleanupLogTimer cleans up the log update timer
func (mw *MainWindow) cleanupLogTimer() {
	mw.mu.Lock()
	defer mw.mu.Unlock()

	if mw.logUpdateTimer != nil {
		mw.logUpdateTimer.Stop()
		mw.logUpdateTimer = nil
	}
}

func (mw *MainWindow) restoreLastState() {
	// Restore checkboxes (do not apply proxy immediately unless we are running)
	mw.setProxyCheckSilently(mw.cfg.Model.LastState.SystemProxyEnabled)

	if mw.cfg.Model.LastState.AutoStartChecked {
		mw.setAutoStartCheckSilently(true)
	} else {
		if en, err := autostart.IsEnabled(); err == nil {
			mw.setAutoStartCheckSilently(en)
		}
	}

	// 只在开机启动时执行延迟逻辑
	if mw.cfg.Model.LastState.AutoStartChecked {
		if mw.cfg.Model.LastState.WasRunning {
			// 延迟2秒后自动启动，确保UI完全准备好
			go func() {
				time.Sleep(2 * time.Second)
				fyne.Do(func() {
					// 再次检查是否还在运行状态，避免重复启动
					if !mw.running {
						log.Printf("[系统] 检测到上次运行状态，自动启动代理")
						mw.onStart()
					}

					// 确保开机启动时窗口可见
					if mw.isHidden {
						mw.isHidden = false
						mw.w.Show()
						mw.w.RequestFocus()
						log.Printf("[系统] 开机启动，显示窗口")
					}
				})
			}()
		} else {
			// 仅开机启动，不启动代理，也要确保窗口可见
			go func() {
				time.Sleep(5 * time.Second)
				fyne.Do(func() {
					if mw.isHidden {
						mw.isHidden = false
						mw.w.Show()
						mw.w.RequestFocus()
						log.Printf("[系统] 开机启动，显示窗口")
					}
				})
			}()
		}
	}
}

func (mw *MainWindow) onAutoStartChanged() {
	enabled := mw.autoStartCheck.Checked
	var err error
	if enabled {
		err = autostart.Enable()
	} else {
		err = autostart.Disable()
	}
	if err != nil {
		mw.setAutoStartCheckSilently(!enabled)
		dialog.ShowError(err, mw.w)
		return
	}
	mw.cfg.Model.LastState.AutoStartChecked = enabled
	_ = mw.cfg.Save()
	if enabled {
		mw.appendLog("[系统] 已设置开机启动\n")
	} else {
		mw.appendLog("[系统] 已取消开机启动\n")
	}
}

func (mw *MainWindow) onProxyChanged() {
	// If not running, only persist preference.
	if !mw.run.IsRunning() {
		mw.cfg.Model.LastState.SystemProxyEnabled = mw.proxyCheck.Checked
		_ = mw.cfg.Save()
		return
	}

	if mw.proxyCheck.Checked {
		mw.tryEnableProxyFromUI()
		return
	}
	if err := sysproxy.Set(false, ""); err != nil {
		mw.setProxyCheckSilently(true)
		dialog.ShowError(err, mw.w)
		return
	}
	mw.systemProxyEnabled = false
	mw.cfg.Model.LastState.SystemProxyEnabled = false
	_ = mw.cfg.Save()
	mw.appendLog("[系统] 已关闭系统代理\n")
}

func (mw *MainWindow) tryEnableProxyFromUI() {
	cur, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}
	if mw.routing.Selected == "none" {
		mw.setProxyCheckSilently(false)
		dialog.ShowInformation("提示", "当前分流模式为\"none\"，不设置系统代理", mw.w)
		return
	}
	if err := sysproxy.Set(true, cur.Listen); err != nil {
		mw.setProxyCheckSilently(false)
		dialog.ShowError(err, mw.w)
		return
	}
	mw.systemProxyEnabled = true
	mw.cfg.Model.LastState.SystemProxyEnabled = true
	_ = mw.cfg.Save()
	mw.appendLog("[系统] 已设置系统代理\n")
}

func (mw *MainWindow) onRoutingChanged() {
	if !mw.run.IsRunning() {
		return
	}
	if !mw.systemProxyEnabled {
		return
	}
	if mw.routing.Selected == "none" {
		_ = sysproxy.Set(false, "")
		mw.systemProxyEnabled = false
		mw.cfg.Model.LastState.SystemProxyEnabled = false
		_ = mw.cfg.Save()
		mw.setProxyCheckSilently(false)
		mw.appendLog("[系统] 分流模式切换为 none，已关闭系统代理\n")
		return
	}
	cur, ok := mw.cfg.GetCurrentServer()
	if !ok {
		return
	}
	_ = sysproxy.Set(true, cur.Listen)
	mw.appendLog("[系统] 分流模式已变更，已更新系统代理设置\n")
}

func (mw *MainWindow) setProxyCheckSilently(v bool) {
	old := mw.proxyCheck.OnChanged
	mw.proxyCheck.OnChanged = nil
	mw.proxyCheck.SetChecked(v)
	mw.proxyCheck.OnChanged = old
}

func (mw *MainWindow) setAutoStartCheckSilently(v bool) {
	old := mw.autoStartCheck.OnChanged
	mw.autoStartCheck.OnChanged = nil
	mw.autoStartCheck.SetChecked(v)
	mw.autoStartCheck.OnChanged = old
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// onWindowClosing 处理窗口关闭事件
func (mw *MainWindow) onWindowClosing() {
	// 停止代理服务器
	if mw.running {
		mw.run.Stop()
		mw.onStopped()
	}

	// 保存配置
	mw.saveFormToCurrent(false)
	_ = mw.cfg.Save()

	// 关闭窗口
	mw.w.Close()
}
