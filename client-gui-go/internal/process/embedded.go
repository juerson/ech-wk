package process

import (
	"fmt"
	"sync"

	"github.com/juerson/ech-wk/client-gui-go/core"
)

// EmbeddedRunner 内嵌代理运行器
type EmbeddedRunner struct {
	mu          sync.Mutex
	server      *core.ProxyServer
	isRunning   bool
	logCallback func(string)
}

// NewEmbeddedRunner 创建新的内嵌运行器
func NewEmbeddedRunner(logCallback func(string)) *EmbeddedRunner {
	return &EmbeddedRunner{
		logCallback: logCallback,
	}
}

// IsRunning 检查是否正在运行
func (r *EmbeddedRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isRunning
}

// Start 启动内嵌代理服务器
func (r *EmbeddedRunner) Start(c Config, onLog func(string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isRunning {
		return fmt.Errorf("server is already running")
	}

	// 转换配置
	proxyConfig := core.ProxyConfig{
		ListenAddr:  c.Listen,
		ServerAddr:  c.Server,
		ServerIP:    c.IP,
		Token:       c.Token,
		DNSServer:   c.DNS,
		ECHDomain:   c.ECH,
		RoutingMode: c.RoutingMode,
	}

	// 创建代理服务器
	r.server = core.NewProxyServer(proxyConfig, onLog)

	// 启动服务器
	if err := r.server.Start(); err != nil {
		return fmt.Errorf("failed to start embedded proxy: %w", err)
	}

	r.isRunning = true
	onLog("[系统] 内嵌代理服务器已启动\n")
	return nil
}

// Stop 停止内嵌代理服务器
func (r *EmbeddedRunner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isRunning || r.server == nil {
		return
	}

	r.server.Stop()
	r.server = nil
	r.isRunning = false

	if r.logCallback != nil {
		r.logCallback("[系统] 内嵌代理服务器已停止\n")
	}
}
