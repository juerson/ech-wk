package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ProxyConfig 代理配置
type ProxyConfig struct {
	ListenAddr  string
	ServerAddr  string
	ServerIP    string
	Token       string
	DNSServer   string
	ECHDomain   string
	RoutingMode string // 分流模式: "global", "bypass_cn", "none"
}

// ProxyServer 代理服务器
type ProxyServer struct {
	config    ProxyConfig
	listener  net.Listener
	stopChan  chan struct{}
	mu        sync.RWMutex
	isRunning bool

	// 活动连接跟踪
	activeConns map[net.Conn]bool
	connMu      sync.Mutex

	// ECH配置
	echListMu sync.RWMutex
	echList   []byte

	// 中国IP列表（IPv4）
	chinaIPRangesMu sync.RWMutex
	chinaIPRanges   []ipRange

	// 中国IP列表（IPv6）
	chinaIPV6RangesMu sync.RWMutex
	chinaIPV6Ranges   []ipRangeV6

	// 日志回调
	logCallback func(string)
}

// ipRange 表示一个IPv4 IP范围
type ipRange struct {
	start uint32
	end   uint32
}

// ipRangeV6 表示一个IPv6 IP范围
type ipRangeV6 struct {
	start [16]byte
	end   [16]byte
}

// NewProxyServer 创建新的代理服务器
func NewProxyServer(config ProxyConfig, logCallback func(string)) *ProxyServer {
	if config.ListenAddr == "" {
		config.ListenAddr = "127.0.0.1:30000"
	}
	if config.DNSServer == "" {
		config.DNSServer = "dns.alidns.com/dns-query"
	}
	if config.ECHDomain == "" {
		config.ECHDomain = "cloudflare-ech.com"
	}
	if config.RoutingMode == "" {
		config.RoutingMode = "global"
	}

	return &ProxyServer{
		config:      config,
		stopChan:    make(chan struct{}),
		activeConns: make(map[net.Conn]bool),
		logCallback: logCallback,
	}
}

// logf 内部日志函数
func (ps *ProxyServer) logf(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)

	// 在内核层面统一添加时间戳
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	timestampedMessage := fmt.Sprintf("[%s] %s", timestamp, message)

	if ps.logCallback != nil {
		ps.logCallback(timestampedMessage)
	} else {
		// 命令行模式也使用相同格式
		log.Printf("%s", timestampedMessage)
	}
}

// Start 启动代理服务器
func (ps *ProxyServer) Start() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.isRunning {
		return errors.New("server is already running")
	}

	if ps.config.ServerAddr == "" {
		return errors.New("server address is required")
	}

	ps.logf("[启动] 正在获取 ECH 配置...")
	if err := ps.prepareECH(); err != nil {
		return fmt.Errorf("获取 ECH 配置失败: %w", err)
	}

	// 加载中国IP列表（如果需要）
	if ps.config.RoutingMode == "bypass_cn" {
		ps.logf("[启动] 分流模式: 跳过中国大陆，正在加载中国IP列表...")
		ipv4Count := 0
		ipv6Count := 0

		if err := ps.loadChinaIPList(); err != nil {
			ps.logf("[警告] 加载中国IPv4列表失败: %v", err)
		} else {
			ps.chinaIPRangesMu.RLock()
			ipv4Count = len(ps.chinaIPRanges)
			ps.chinaIPRangesMu.RUnlock()
		}

		if err := ps.loadChinaIPV6List(); err != nil {
			ps.logf("[警告] 加载中国IPv6列表失败: %v", err)
		} else {
			ps.chinaIPV6RangesMu.RLock()
			ipv6Count = len(ps.chinaIPV6Ranges)
			ps.chinaIPV6RangesMu.RUnlock()
		}

		if ipv4Count > 0 || ipv6Count > 0 {
			ps.logf("[启动] 已加载 %d 个中国IPv4段, %d 个中国IPv6段", ipv4Count, ipv6Count)
		} else {
			ps.logf("[警告] 未加载到任何中国IP列表，将使用默认规则")
		}
	} else if ps.config.RoutingMode == "global" {
		ps.logf("[启动] 分流模式: 全局代理")
	} else if ps.config.RoutingMode == "none" {
		ps.logf("[启动] 分流模式: 不改变代理（直连模式）")
	} else {
		ps.logf("[警告] 未知的分流模式: %s，使用默认模式 global", ps.config.RoutingMode)
		ps.config.RoutingMode = "global"
	}

	return ps.runProxyServer()
}

// Stop 停止代理服务器
func (ps *ProxyServer) Stop() {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if !ps.isRunning {
		return
	}

	ps.logf("[系统] 正在停止代理服务器...")

	// 关闭停止通道
	close(ps.stopChan)

	// 关闭监听器
	if ps.listener != nil {
		ps.listener.Close()
		ps.logf("[系统] 已关闭监听器")
	}

	// 关闭所有活动连接
	ps.connMu.Lock()
	connCount := len(ps.activeConns)
	for conn := range ps.activeConns {
		if conn != nil {
			conn.Close()
		}
		delete(ps.activeConns, conn)
	}
	ps.connMu.Unlock()

	if connCount > 0 {
		ps.logf("[系统] 已关闭 %d 个活动连接", connCount)
	}

	ps.isRunning = false
	ps.logf("[系统] 代理服务器已完全停止")
}

// IsRunning 检查服务器是否正在运行
func (ps *ProxyServer) IsRunning() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.isRunning
}

// ======================== 工具函数 ========================

// addConnection 添加连接到跟踪列表
func (ps *ProxyServer) addConnection(conn net.Conn) {
	ps.connMu.Lock()
	defer ps.connMu.Unlock()
	if ps.activeConns != nil {
		ps.activeConns[conn] = true
	}
}

// removeConnection 从跟踪列表移除连接
func (ps *ProxyServer) removeConnection(conn net.Conn) {
	ps.connMu.Lock()
	defer ps.connMu.Unlock()
	if ps.activeConns != nil {
		delete(ps.activeConns, conn)
	}
}

// ======================== 工具函数 ========================

// ipToUint32 将IP地址转换为uint32
func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

// isChinaIP 检查IP是否在中国IP列表中（支持IPv4和IPv6）
func (ps *ProxyServer) isChinaIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	// 检查IPv4
	if ip.To4() != nil {
		ipUint32 := ipToUint32(ip)
		if ipUint32 == 0 {
			return false
		}

		ps.chinaIPRangesMu.RLock()
		defer ps.chinaIPRangesMu.RUnlock()

		// 二分查找
		left, right := 0, len(ps.chinaIPRanges)
		for left < right {
			mid := (left + right) / 2
			r := ps.chinaIPRanges[mid]
			if ipUint32 < r.start {
				right = mid
			} else if ipUint32 > r.end {
				left = mid + 1
			} else {
				return true
			}
		}
		return false
	}

	// 检查IPv6
	ipBytes := ip.To16()
	if ipBytes == nil {
		return false
	}

	var ipArray [16]byte
	copy(ipArray[:], ipBytes)

	ps.chinaIPV6RangesMu.RLock()
	defer ps.chinaIPV6RangesMu.RUnlock()

	// 二分查找IPv6
	left, right := 0, len(ps.chinaIPV6Ranges)
	for left < right {
		mid := (left + right) / 2
		r := ps.chinaIPV6Ranges[mid]

		// 比较起始IP
		cmpStart := compareIPv6(ipArray, r.start)
		if cmpStart < 0 {
			right = mid
			continue
		}

		// 比较结束IP
		cmpEnd := compareIPv6(ipArray, r.end)
		if cmpEnd > 0 {
			left = mid + 1
			continue
		}

		// 在范围内
		return true
	}
	return false
}

// compareIPv6 比较两个IPv6地址，返回 -1, 0, 或 1
func compareIPv6(a, b [16]byte) int {
	for i := 0; i < 16; i++ {
		if a[i] < b[i] {
			return -1
		} else if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// downloadIPList 下载IP列表文件
func (ps *ProxyServer) downloadIPList(url, filePath string) error {
	ps.logf("[下载] 正在下载 IP 列表: %s", url)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	// 读取内容
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取下载内容失败: %w", err)
	}

	// 保存到文件
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		return fmt.Errorf("保存文件失败: %w", err)
	}

	ps.logf("[下载] 已保存到: %s", filePath)
	return nil
}

// loadChinaIPList 从程序目录加载中国IP列表
func (ps *ProxyServer) loadChinaIPList() error {
	// 获取可执行文件所在目录
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	exeDir := filepath.Dir(exePath)
	ipListFile := filepath.Join(exeDir, "chn_ip.txt")

	// 如果文件不存在，尝试当前目录
	if _, err := os.Stat(ipListFile); os.IsNotExist(err) {
		ipListFile = "chn_ip.txt"
	}

	// 检查文件是否存在或为空
	needDownload := false
	if info, err := os.Stat(ipListFile); os.IsNotExist(err) {
		needDownload = true
		ps.logf("[加载] IPv4 列表文件不存在，将自动下载")
	} else if info.Size() == 0 {
		needDownload = true
		ps.logf("[加载] IPv4 列表文件为空，将自动下载")
	}

	// 如果需要下载，先下载文件
	if needDownload {
		url := "https://raw.githubusercontent.com/mayaxcn/china-ip-list/refs/heads/master/chn_ip.txt"
		if err := ps.downloadIPList(url, ipListFile); err != nil {
			return fmt.Errorf("自动下载 IPv4 列表失败: %w", err)
		}
	}

	file, err := os.Open(ipListFile)
	if err != nil {
		return fmt.Errorf("打开IP列表文件失败: %w", err)
	}
	defer file.Close()

	// 预分配容量以减少内存重分配
	var ranges []ipRange
	ranges = make([]ipRange, 0, 8000) // 预估中国IP段数量

	scanner := bufio.NewScanner(file)
	// 增大缓冲区以提高扫描性能
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		startIP := net.ParseIP(parts[0])
		endIP := net.ParseIP(parts[1])
		if startIP == nil || endIP == nil {
			continue
		}

		start := ipToUint32(startIP)
		end := ipToUint32(endIP)
		if start > 0 && end > 0 && start <= end {
			ranges = append(ranges, ipRange{start: start, end: end})
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取IP列表文件失败: %w", err)
	}

	if len(ranges) == 0 {
		return errors.New("IP列表为空")
	}

	ps.logf("[加载] 已加载 %d 个中国IPv4段，内存占用约 %d KB", len(ranges), len(ranges)*8/1024)

	// 按起始IP排序 - 使用更高效的排序
	for i := 0; i < len(ranges)-1; i++ {
		minIdx := i
		for j := i + 1; j < len(ranges); j++ {
			if ranges[j].start < ranges[minIdx].start {
				minIdx = j
			}
		}
		if minIdx != i {
			ranges[i], ranges[minIdx] = ranges[minIdx], ranges[i]
		}
	}

	ps.chinaIPRangesMu.Lock()
	ps.chinaIPRanges = ranges
	ps.chinaIPRangesMu.Unlock()

	return nil
}

// loadChinaIPV6List 从程序目录加载中国IPv6 IP列表
func (ps *ProxyServer) loadChinaIPV6List() error {
	// 获取可执行文件所在目录
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取可执行文件路径失败: %w", err)
	}
	exeDir := filepath.Dir(exePath)
	ipListFile := filepath.Join(exeDir, "chn_ip_v6.txt")

	// 如果文件不存在，尝试当前目录
	if _, err := os.Stat(ipListFile); os.IsNotExist(err) {
		ipListFile = "chn_ip_v6.txt"
	}

	// 检查文件是否存在或为空
	needDownload := false
	if info, err := os.Stat(ipListFile); os.IsNotExist(err) {
		needDownload = true
		ps.logf("[加载] IPv6 列表文件不存在，将自动下载")
	} else if info.Size() == 0 {
		needDownload = true
		ps.logf("[加载] IPv6 列表文件为空，将自动下载")
	}

	// 如果需要下载，先下载文件
	if needDownload {
		url := "https://raw.githubusercontent.com/mayaxcn/china-ip-list/refs/heads/master/chn_ip_v6.txt"
		if err := ps.downloadIPList(url, ipListFile); err != nil {
			ps.logf("[警告] 自动下载 IPv6 列表失败: %v，将跳过 IPv6 支持", err)
			return nil // IPv6 列表下载失败不算致命错误
		}
	}

	file, err := os.Open(ipListFile)
	if err != nil {
		// 文件打开失败，不算致命错误
		ps.logf("[警告] 打开 IPv6 IP列表文件失败: %v，将跳过 IPv6 支持", err)
		return nil
	}
	defer file.Close()

	// 预分配容量以减少内存重分配
	var ranges []ipRangeV6
	ranges = make([]ipRangeV6, 0, 1000) // 预估IPv6段数量

	scanner := bufio.NewScanner(file)
	// 增大缓冲区以提高扫描性能
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		startIP := net.ParseIP(parts[0])
		endIP := net.ParseIP(parts[1])
		if startIP == nil || endIP == nil {
			continue
		}

		// 转换为16字节数组
		startBytes := startIP.To16()
		endBytes := endIP.To16()
		if startBytes == nil || endBytes == nil {
			continue
		}

		var start, end [16]byte
		copy(start[:], startBytes)
		copy(end[:], endBytes)

		// 检查范围是否有效
		if compareIPv6(start, end) <= 0 {
			ranges = append(ranges, ipRangeV6{start: start, end: end})
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取IPv6 IP列表文件失败: %w", err)
	}

	if len(ranges) == 0 {
		// IPv6列表为空不算错误，可能文件不存在或为空
		return nil
	}

	ps.logf("[加载] 已加载 %d 个中国IPv6段，内存占用约 %d KB", len(ranges), len(ranges)*32/1024)

	// 按起始IP排序 - 使用更高效的排序
	for i := 0; i < len(ranges)-1; i++ {
		minIdx := i
		for j := i + 1; j < len(ranges); j++ {
			if compareIPv6(ranges[j].start, ranges[minIdx].start) < 0 {
				minIdx = j
			}
		}
		if minIdx != i {
			ranges[i], ranges[minIdx] = ranges[minIdx], ranges[i]
		}
	}

	ps.chinaIPV6RangesMu.Lock()
	ps.chinaIPV6Ranges = ranges
	ps.chinaIPV6RangesMu.Unlock()

	return nil
}

// shouldBypassProxy 根据分流模式判断是否应该绕过代理（直连）
func (ps *ProxyServer) shouldBypassProxy(targetHost string) bool {
	if ps.config.RoutingMode == "none" {
		// "不改变代理"模式：所有流量都直连
		return true
	}
	if ps.config.RoutingMode == "global" {
		// "全局代理"模式：所有流量都走代理
		return false
	}
	if ps.config.RoutingMode == "bypass_cn" {
		// "跳过中国大陆"模式：检查是否是中国IP
		// 先尝试解析为IP
		if ip := net.ParseIP(targetHost); ip != nil {
			return ps.isChinaIP(targetHost)
		}
		// 如果是域名，先解析IP
		ips, err := net.LookupIP(targetHost)
		if err != nil {
			// 解析失败，默认走代理
			return false
		}
		// 检查所有解析到的IP，如果有一个是中国IP，就直连
		for _, ip := range ips {
			if ps.isChinaIP(ip.String()) {
				return true
			}
		}
		// 都不是中国IP，走代理
		return false
	}
	// 未知模式，默认走代理
	return false
}

// ShouldBypassProxy 根据分流模式判断是否应该绕过代理（直连）- 公共方法
func (ps *ProxyServer) ShouldBypassProxy(targetHost string) bool {
	return ps.shouldBypassProxy(targetHost)
}

func isNormalCloseError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "normal closure")
}

// ======================== ECH 支持 ========================

const typeHTTPS = 65

// prepareECH 准备ECH配置
func (ps *ProxyServer) prepareECH() error {
	echBase64, err := ps.queryHTTPSRecord(ps.config.ECHDomain, ps.config.DNSServer)
	if err != nil {
		return fmt.Errorf("DNS 查询失败: %w", err)
	}
	if echBase64 == "" {
		return errors.New("未找到 ECH 参数")
	}
	raw, err := base64.StdEncoding.DecodeString(echBase64)
	if err != nil {
		return fmt.Errorf("ECH 解码失败: %w", err)
	}
	ps.echListMu.Lock()
	ps.echList = raw
	ps.echListMu.Unlock()
	ps.logf("[ECH] 配置已加载，长度: %d 字节", len(raw))
	return nil
}

// refreshECH 刷新ECH配置
func (ps *ProxyServer) refreshECH() error {
	ps.logf("[ECH] 刷新配置...")
	return ps.prepareECH()
}

// getECHList 获取ECH配置
func (ps *ProxyServer) getECHList() ([]byte, error) {
	ps.echListMu.RLock()
	defer ps.echListMu.RUnlock()
	if len(ps.echList) == 0 {
		return nil, errors.New("ECH 配置未加载")
	}
	return ps.echList, nil
}

func buildTLSConfigWithECH(serverName string, echList []byte) (*tls.Config, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("加载系统根证书失败: %w", err)
	}

	if len(echList) == 0 {
		return nil, errors.New("ECH 配置为空，这是必需功能")
	}

	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		RootCAs:    roots,
	}

	// 使用反射设置 ECH 字段（ECH 是核心功能，必须设置成功）
	if err := setECHConfig(config, echList); err != nil {
		return nil, fmt.Errorf("设置 ECH 配置失败（需要 Go 1.23+ 或支持 ECH 的版本）: %w", err)
	}

	return config, nil
}

// setECHConfig 使用反射设置 ECH 配置（ECH 是核心功能，必须成功）
func setECHConfig(config *tls.Config, echList []byte) error {
	configValue := reflect.ValueOf(config).Elem()

	// 设置 EncryptedClientHelloConfigList（必需）
	field1 := configValue.FieldByName("EncryptedClientHelloConfigList")
	if !field1.IsValid() || !field1.CanSet() {
		return fmt.Errorf("EncryptedClientHelloConfigList 字段不可用，需要 Go 1.23+ 版本")
	}
	field1.Set(reflect.ValueOf(echList))

	// 设置 EncryptedClientHelloRejectionVerify（必需）
	field2 := configValue.FieldByName("EncryptedClientHelloRejectionVerify")
	if !field2.IsValid() || !field2.CanSet() {
		return fmt.Errorf("EncryptedClientHelloRejectionVerify 字段不可用，需要 Go 1.23+ 版本")
	}
	rejectionFunc := func(cs tls.ConnectionState) error {
		return errors.New("服务器拒绝 ECH")
	}
	field2.Set(reflect.ValueOf(rejectionFunc))

	return nil
}

// queryHTTPSRecord 通过 DoH 查询 HTTPS 记录
func (ps *ProxyServer) queryHTTPSRecord(domain, dnsServer string) (string, error) {
	dohURL := dnsServer
	if !strings.HasPrefix(dohURL, "https://") && !strings.HasPrefix(dohURL, "http://") {
		dohURL = "https://" + dohURL
	}
	return ps.queryDoH(domain, dohURL)
}

// queryDoH 执行 DoH 查询（用于获取 ECH 配置）
func (ps *ProxyServer) queryDoH(domain, dohURL string) (string, error) {
	u, err := url.Parse(dohURL)
	if err != nil {
		return "", fmt.Errorf("无效的 DoH URL: %v", err)
	}

	dnsQuery := buildDNSQuery(domain, typeHTTPS)
	dnsBase64 := base64.RawURLEncoding.EncodeToString(dnsQuery)

	q := u.Query()
	q.Set("dns", dnsBase64)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %v", err)
	}
	req.Header.Set("Accept", "application/dns-message")
	req.Header.Set("Content-Type", "application/dns-message")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("DoH 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DoH 服务器返回错误: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 DoH 响应失败: %v", err)
	}

	return parseDNSResponse(body)
}

func buildDNSQuery(domain string, qtype uint16) []byte {
	query := make([]byte, 0, 512)
	query = append(query, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	for _, label := range strings.Split(domain, ".") {
		query = append(query, byte(len(label)))
		query = append(query, []byte(label)...)
	}
	query = append(query, 0x00, byte(qtype>>8), byte(qtype), 0x00, 0x01)
	return query
}

func parseDNSResponse(response []byte) (string, error) {
	if len(response) < 12 {
		return "", errors.New("响应过短")
	}
	ancount := binary.BigEndian.Uint16(response[6:8])
	if ancount == 0 {
		return "", errors.New("无应答记录")
	}

	offset := 12
	for offset < len(response) && response[offset] != 0 {
		offset += int(response[offset]) + 1
	}
	offset += 5

	for i := 0; i < int(ancount); i++ {
		if offset >= len(response) {
			break
		}
		if response[offset]&0xC0 == 0xC0 {
			offset += 2
		} else {
			for offset < len(response) && response[offset] != 0 {
				offset += int(response[offset]) + 1
			}
			offset++
		}
		if offset+10 > len(response) {
			break
		}
		rrType := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 8
		dataLen := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 2
		if offset+int(dataLen) > len(response) {
			break
		}
		data := response[offset : offset+int(dataLen)]
		offset += int(dataLen)

		if rrType == typeHTTPS {
			if ech := parseHTTPSRecord(data); ech != "" {
				return ech, nil
			}
		}
	}
	return "", nil
}

func parseHTTPSRecord(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	offset := 2
	if offset < len(data) && data[offset] == 0 {
		offset++
	} else {
		for offset < len(data) && data[offset] != 0 {
			offset += int(data[offset]) + 1
		}
		offset++
	}
	for offset+4 <= len(data) {
		key := binary.BigEndian.Uint16(data[offset : offset+2])
		length := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		offset += 4
		if offset+int(length) > len(data) {
			break
		}
		value := data[offset : offset+int(length)]
		offset += int(length)
		if key == 5 {
			return base64.StdEncoding.EncodeToString(value)
		}
	}
	return ""
}

// ======================== DoH 代理支持 ========================

// queryDoHForProxy 通过 ECH 转发 DNS 查询到 Cloudflare DoH
func (ps *ProxyServer) queryDoHForProxy(dnsQuery []byte) ([]byte, error) {
	_, port, _, err := ps.parseServerAddr(ps.config.ServerAddr)
	if err != nil {
		return nil, err
	}

	// 构建 DoH URL
	dohURL := fmt.Sprintf("https://cloudflare-dns.com:%s/dns-query", port)

	echBytes, err := ps.getECHList()
	if err != nil {
		return nil, fmt.Errorf("获取 ECH 配置失败: %w", err)
	}

	tlsCfg, err := buildTLSConfigWithECH("cloudflare-dns.com", echBytes)
	if err != nil {
		return nil, fmt.Errorf("构建 TLS 配置失败: %w", err)
	}

	// 创建 HTTP 客户端
	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
	}

	// 如果指定了 IP，使用自定义 Dialer
	if ps.config.ServerIP != "" {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			dialer := &net.Dialer{
				Timeout: 10 * time.Second,
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ps.config.ServerIP, port))
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	// 发送 DoH 请求
	req, err := http.NewRequest("POST", dohURL, bytes.NewReader(dnsQuery))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DoH 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH 响应错误: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// ======================== WebSocket 客户端 ========================

// parseServerAddr 解析服务器地址
func (ps *ProxyServer) parseServerAddr(addr string) (host, port, path string, err error) {
	path = "/"
	slashIdx := strings.Index(addr, "/")
	if slashIdx != -1 {
		path = addr[slashIdx:]
		addr = addr[:slashIdx]
	}

	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的服务器地址格式: %v", err)
	}

	return host, port, path, nil
}

// dialWebSocketWithECH 使用 ECH 连接 WebSocket
func (ps *ProxyServer) dialWebSocketWithECH(maxRetries int) (*websocket.Conn, error) {
	host, port, path, err := ps.parseServerAddr(ps.config.ServerAddr)
	if err != nil {
		return nil, err
	}

	wsURL := fmt.Sprintf("wss://%s:%s%s", host, port, path)
	ps.logf("[WebSocket] 连接地址: %s", wsURL)

	for attempt := 1; attempt <= maxRetries; attempt++ {
		var dialer websocket.Dialer

		// 尝试获取ECH配置
		echBytes, echErr := ps.getECHList()
		if echErr != nil {
			ps.logf("[ECH] ECH配置不可用，使用普通TLS连接: %v", echErr)
			// 使用普通TLS连接，但仍需包含Token
			dialer = websocket.Dialer{
				Subprotocols: func() []string {
					if ps.config.Token == "" {
						return nil
					}
					return []string{ps.config.Token}
				}(),
				HandshakeTimeout: 10 * time.Second, // 与原始文件保持一致
			}
		} else {
			// 使用ECH连接
			tlsCfg, tlsErr := buildTLSConfigWithECH(host, echBytes)
			if tlsErr != nil {
				return nil, tlsErr
			}

			dialer = websocket.Dialer{
				TLSClientConfig: tlsCfg,
				Subprotocols: func() []string {
					if ps.config.Token == "" {
						return nil
					}
					return []string{ps.config.Token}
				}(),
				HandshakeTimeout: 10 * time.Second, // 与原始文件保持一致
			}
		}

		if ps.config.ServerIP != "" {
			dialer.NetDial = func(network, address string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				return net.DialTimeout(network, net.JoinHostPort(ps.config.ServerIP, port), 10*time.Second)
			}
		}

		ps.logf("[WebSocket] 尝试连接 (尝试 %d/%d)", attempt, maxRetries)
		wsConn, _, dialErr := dialer.Dial(wsURL, nil)
		if dialErr != nil {
			ps.logf("[WebSocket] 连接失败: %v", dialErr)

			if websocket.IsUnexpectedCloseError(dialErr) {
				ps.logf("[WebSocket] 服务器主动关闭连接，可能原因:")
				ps.logf("[WebSocket] 1. Token认证失败")
				ps.logf("[WebSocket] 2. 服务器不支持WebSocket")
				ps.logf("[WebSocket] 3. 协议版本不匹配")
			}

			if strings.Contains(dialErr.Error(), "ECH") && attempt < maxRetries {
				ps.logf("[ECH] 连接失败，尝试刷新配置 (%d/%d)", attempt, maxRetries)
				ps.refreshECH()
				time.Sleep(time.Second)
				continue
			}
			return nil, dialErr
		}

		ps.logf("[WebSocket] 连接成功!")
		return wsConn, nil
	}

	return nil, errors.New("连接失败，已达最大重试次数")
}

// ======================== 统一代理服务器 ========================

// runProxyServer 运行代理服务器
func (ps *ProxyServer) runProxyServer() error {
	listener, err := net.Listen("tcp", ps.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("监听失败: %v", err)
	}
	ps.listener = listener
	ps.isRunning = true

	ps.logf("[代理] 服务器启动: %s (支持 SOCKS5 和 HTTP)", ps.config.ListenAddr)
	ps.logf("[代理] 后端服务器: %s", ps.config.ServerAddr)
	if ps.config.ServerIP != "" {
		ps.logf("[代理] 使用固定 IP: %s", ps.config.ServerIP)
	}

	go func() {
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				// 检查是否是因为停止导致的错误
				select {
				case <-ps.stopChan:
					return
				default:
					ps.logf("[代理] 接受连接失败: %v", err)
					continue
				}
			}

			go ps.handleConnection(conn)
		}
	}()

	return nil
}

// handleConnection 处理连接
func (ps *ProxyServer) handleConnection(conn net.Conn) {
	defer func() {
		conn.Close()
		ps.removeConnection(conn)
	}()

	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return
	}

	clientAddr := conn.RemoteAddr().String()
	conn.SetDeadline(time.Now().Add(300 * time.Second)) // 增加到5分钟

	// 添加到活动连接跟踪
	ps.addConnection(conn)
	ps.logf("[连接] %s 新连接，当前活动连接数: %d", clientAddr, len(ps.activeConns))

	// 读取第一个字节判断协议
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return
	}

	firstByte := buf[0]

	// 使用 switch 判断协议类型
	switch firstByte {
	case 0x05:
		// SOCKS5 协议
		ps.handleSOCKS5(conn, clientAddr, firstByte)
	case 'C', 'G', 'P', 'H', 'D', 'O', 'T':
		// HTTP 协议 (CONNECT, GET, POST, HEAD, DELETE, OPTIONS, TRACE, PUT, PATCH)
		ps.handleHTTP(conn, clientAddr, firstByte)
	default:
		ps.logf("[代理] %s 未知协议: 0x%02x", clientAddr, firstByte)
	}
}

// ======================== SOCKS5 处理 ========================

// handleSOCKS5 处理SOCKS5协议
func (ps *ProxyServer) handleSOCKS5(conn net.Conn, clientAddr string, firstByte byte) {
	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return
	}

	// 验证版本
	if firstByte != 0x05 {
		ps.logf("[SOCKS5] %s 版本错误: 0x%02x", clientAddr, firstByte)
		return
	}

	// 读取认证方法数量
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}

	nmethods := buf[0]
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}

	// 响应无需认证
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// 读取请求
	buf = make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}

	if buf[0] != 5 {
		return
	}

	command := buf[1]
	atyp := buf[3]

	var host string
	switch atyp {
	case 0x01: // IPv4
		buf = make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = net.IP(buf).String()

	case 0x03: // 域名
		buf = make([]byte, 1)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		domainBuf := make([]byte, buf[0])
		if _, err := io.ReadFull(conn, domainBuf); err != nil {
			return
		}
		host = string(domainBuf)

	case 0x04: // IPv6
		buf = make([]byte, 16)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		host = fmt.Sprintf("[%s]", net.IP(buf).String())

	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return
	}

	// 读取端口
	buf = make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	port := int(buf[0])<<8 | int(buf[1])

	switch command {
	case 0x01: // CONNECT
		var target string
		if atyp == 0x04 {
			target = fmt.Sprintf("[%s]:%d", host, port)
		} else {
			target = fmt.Sprintf("%s:%d", host, port)
		}

		ps.logf("[SOCKS5] %s -> %s", clientAddr, target)

		if err := ps.handleTunnel(conn, target, clientAddr, modeSOCKS5, ""); err != nil {
			if !isNormalCloseError(err) {
				ps.logf("[SOCKS5] %s 代理失败: %v", clientAddr, err)
			}
		}

	case 0x03: // UDP ASSOCIATE
		ps.handleUDPAssociate(conn, clientAddr)

	default:
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return
	}
}

// handleUDPAssociate 处理UDP ASSOCIATE
func (ps *ProxyServer) handleUDPAssociate(tcpConn net.Conn, clientAddr string) {
	// 创建 UDP 监听器
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		ps.logf("[UDP] %s 解析地址失败: %v", clientAddr, err)
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return
	}

	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		ps.logf("[UDP] %s 监听失败: %v", clientAddr, err)
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return
	}

	// 获取实际监听的端口
	localAddr := udpConn.LocalAddr().(*net.UDPAddr)
	port := localAddr.Port

	ps.logf("[UDP] %s UDP ASSOCIATE 监听端口: %d", clientAddr, port)

	// 发送成功响应
	response := []byte{0x05, 0x00, 0x00, 0x01}
	response = append(response, 127, 0, 0, 1) // 127.0.0.1
	response = append(response, byte(port>>8), byte(port&0xff))

	if _, err := tcpConn.Write(response); err != nil {
		udpConn.Close()
		return
	}

	// 启动 UDP 处理
	stopChan := make(chan struct{})
	go ps.handleUDPRelay(udpConn, clientAddr, stopChan)

	// 保持 TCP 连接，直到客户端关闭
	buf := make([]byte, 1)
	tcpConn.Read(buf)

	close(stopChan)
	udpConn.Close()
}

func (ps *ProxyServer) handleUDPRelay(udpConn *net.UDPConn, clientAddr string, stopChan chan struct{}) {
	// 减少UDP缓冲区大小从65535到8192字节以节省内存
	buf := make([]byte, 8192)
	for {
		select {
		case <-stopChan:
			return
		default:
			udpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				return
			}

			// 简化的UDP处理 - 目前只支持DNS查询
			if n < 10 {
				continue
			}

			data := buf[:n]
			if data[2] != 0x00 { // FRAG 必须为 0
				continue
			}

			atyp := data[3]
			var dstHost string
			var dstPort int

			switch atyp {
			case 0x01: // IPv4
				if n < 10 {
					continue
				}
				dstHost = net.IP(data[4:8]).String()
				dstPort = int(data[8])<<8 | int(data[9])

			case 0x03: // 域名
				if n < 5 {
					continue
				}
				domainLen := int(data[4])
				if n < 7+domainLen {
					continue
				}
				dstHost = string(data[5 : 5+domainLen])
				dstPort = int(data[5+domainLen])<<8 | int(data[6+domainLen])

			case 0x04: // IPv6
				if n < 22 {
					continue
				}
				dstHost = net.IP(data[4:20]).String()
				dstPort = int(data[20])<<8 | int(data[21])

			default:
				continue
			}

			var headerLen int
			switch data[3] {
			case 0x01: // IPv4
				headerLen = 10
			case 0x03: // 域名
				domainLen := int(data[4])
				headerLen = 7 + domainLen
			case 0x04: // IPv6
				headerLen = 22
			default:
				continue
			}

			udpData := data[headerLen:]
			var target string
			if data[3] == 0x04 { // IPv6
				target = fmt.Sprintf("[%s]:%d", dstHost, dstPort)
			} else {
				target = fmt.Sprintf("%s:%d", dstHost, dstPort)
			}

			// 检查是否是 DNS 查询（端口 53）
			if dstPort == 53 {
				ps.logf("[UDP-DNS] %s -> %s (DoH 查询)", clientAddr, target)
				go ps.handleDNSQuery(udpConn, addr, udpData, data[:headerLen])
			} else {
				ps.logf("[UDP] %s -> %s (暂不支持非 DNS UDP)", clientAddr, target)
			}
		}
	}
}

// handleDNSQuery 处理DNS查询
func (ps *ProxyServer) handleDNSQuery(udpConn *net.UDPConn, clientAddr *net.UDPAddr, dnsQuery []byte, socks5Header []byte) {
	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return
	}

	// 通过 DoH 查询
	dnsResponse, err := ps.queryDoHForProxy(dnsQuery)
	if err != nil {
		ps.logf("[UDP-DNS] DoH 查询失败: %v", err)
		return
	}

	// 构建 SOCKS5 UDP 响应
	response := make([]byte, 0, len(socks5Header)+len(dnsResponse))
	response = append(response, socks5Header...)
	response = append(response, dnsResponse...)

	// 发送响应
	_, err = udpConn.WriteToUDP(response, clientAddr)
	if err != nil {
		ps.logf("[UDP-DNS] 发送响应失败: %v", err)
		return
	}

	ps.logf("[UDP-DNS] DoH 查询成功，响应 %d 字节", len(dnsResponse))
}

// ======================== HTTP 处理 ========================

// handleHTTP 处理HTTP协议
func (ps *ProxyServer) handleHTTP(conn net.Conn, clientAddr string, firstByte byte) {
	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return
	}

	// 将第一个字节放回缓冲区
	reader := bufio.NewReader(io.MultiReader(
		strings.NewReader(string(firstByte)),
		conn,
	))

	// 读取 HTTP 请求行
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	parts := strings.Fields(requestLine)
	if len(parts) < 3 {
		return
	}

	method := parts[0]
	requestURL := parts[1]
	httpVersion := parts[2]

	// 读取所有 headers
	headers := make(map[string]string)
	var headerLines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		headerLines = append(headerLines, line)
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			headers[strings.ToLower(key)] = value
		}
	}

	switch method {
	case "CONNECT":
		// HTTPS 隧道代理
		ps.logf("[HTTP-CONNECT] %s -> %s", clientAddr, requestURL)
		if err := ps.handleTunnel(conn, requestURL, clientAddr, modeHTTPConnect, ""); err != nil {
			if !isNormalCloseError(err) {
				ps.logf("[HTTP-CONNECT] %s 代理失败: %v", clientAddr, err)
			}
		}

	case "GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH", "TRACE":
		// HTTP 代理 - 直接转发
		ps.logf("[HTTP-%s] %s -> %s", method, clientAddr, requestURL)

		var target string
		var path string

		if strings.HasPrefix(requestURL, "http://") {
			// 解析完整 URL
			urlWithoutScheme := strings.TrimPrefix(requestURL, "http://")
			idx := strings.Index(urlWithoutScheme, "/")
			if idx > 0 {
				target = urlWithoutScheme[:idx]
				path = urlWithoutScheme[idx:]
			} else {
				target = urlWithoutScheme
				path = "/"
			}
		} else {
			// 相对路径，从 Host header 获取
			target = headers["host"]
			path = requestURL
		}

		if target == "" {
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			return
		}

		// 添加默认端口
		if !strings.Contains(target, ":") {
			target += ":80"
		}

		// 重构 HTTP 请求（去掉完整 URL，使用相对路径）
		var requestBuilder strings.Builder
		requestBuilder.WriteString(fmt.Sprintf("%s %s %s\r\n", method, path, httpVersion))

		// 写入 headers（过滤掉 Proxy-Connection）
		for _, line := range headerLines {
			key := strings.Split(line, ":")[0]
			keyLower := strings.ToLower(strings.TrimSpace(key))
			if keyLower != "proxy-connection" && keyLower != "proxy-authorization" {
				requestBuilder.WriteString(line)
				requestBuilder.WriteString("\r\n")
			}
		}
		requestBuilder.WriteString("\r\n")

		// 如果有请求体，需要读取并附加
		if contentLength := headers["content-length"]; contentLength != "" {
			var length int
			fmt.Sscanf(contentLength, "%d", &length)
			if length > 0 && length < 10*1024*1024 { // 限制 10MB
				body := make([]byte, length)
				if _, err := io.ReadFull(reader, body); err == nil {
					requestBuilder.Write(body)
				}
			}
		}

		firstFrame := requestBuilder.String()

		// 使用 modeHTTPProxy 模式
		if err := ps.handleTunnel(conn, target, clientAddr, modeHTTPProxy, firstFrame); err != nil {
			if !isNormalCloseError(err) {
				ps.logf("[HTTP-%s] %s 代理失败: %v", method, clientAddr, err)
			}
		}

	default:
		// 不支持的HTTP方法
		ps.logf("[HTTP] %s 不支持的方法: %s", clientAddr, method)
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
	}
}

// ======================== 通用隧道处理 ========================

// 代理模式常量
const (
	modeSOCKS5      = 1 // SOCKS5 代理
	modeHTTPConnect = 2 // HTTP CONNECT 隧道
	modeHTTPProxy   = 3 // HTTP 代理 (GET/POST等)
)

// handleTunnel 处理隧道连接
func (ps *ProxyServer) handleTunnel(conn net.Conn, target, clientAddr string, mode int, firstFrame string) error {
	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return errors.New("server is stopping")
	}

	// 解析目标地址
	targetHost, _, err := net.SplitHostPort(target)
	if err != nil {
		targetHost = target
	}

	// 检查是否应该绕过代理（直连）
	if ps.shouldBypassProxy(targetHost) {
		ps.logf("[分流] %s -> %s (直连，绕过代理)", clientAddr, target)
		return ps.handleDirectConnection(conn, target, clientAddr, mode, firstFrame)
	}

	// 走代理
	ps.logf("[分流] %s -> %s (通过代理)", clientAddr, target)
	wsConn, err := ps.dialWebSocketWithECH(2)
	if err != nil {
		ps.logf("[代理] 无法连接到后端服务器 %s: %v", ps.config.ServerAddr, err)
		ps.logf("[代理] 请检查服务器地址是否正确，或使用有效的Cloudflare Workers地址")
		sendErrorResponse(conn, mode)
		return fmt.Errorf("后端服务器连接失败: %w", err)
	}

	// 确保WebSocket连接被关闭
	defer func() {
		ps.logf("[WebSocket] 正在关闭连接: %s -> %s", clientAddr, target)
		wsConn.Close()
		ps.logf("[WebSocket] 连接已关闭: %s -> %s", clientAddr, target)
	}()

	var mu sync.Mutex

	// 设置最长连接时间为5分钟
	conn.SetDeadline(time.Now().Add(5 * time.Minute))

	// 保活
	stopPing := make(chan bool)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				// 延长连接时间（每次ping时重置deadline）
				conn.SetDeadline(time.Now().Add(5 * time.Minute))
				wsConn.WriteMessage(websocket.PingMessage, nil)
				mu.Unlock()
			case <-stopPing:
				return
			}
		}
	}()
	defer close(stopPing)

	conn.SetDeadline(time.Time{})

	// 如果没有预设的 firstFrame，尝试读取第一帧数据（仅 SOCKS5）
	if firstFrame == "" && mode == modeSOCKS5 {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		buffer := make([]byte, 8192) // 减少从32768到8192字节
		n, _ := conn.Read(buffer)
		_ = conn.SetReadDeadline(time.Time{})
		if n > 0 {
			firstFrame = string(buffer[:n])
		}
	}

	// 发送连接请求
	connectMsg := fmt.Sprintf("CONNECT:%s|%s", target, firstFrame)
	mu.Lock()
	err = wsConn.WriteMessage(websocket.TextMessage, []byte(connectMsg))
	mu.Unlock()
	if err != nil {
		sendErrorResponse(conn, mode)
		return err
	}

	// 等待响应
	_, msg, err := wsConn.ReadMessage()
	if err != nil {
		sendErrorResponse(conn, mode)
		return err
	}

	response := string(msg)
	if strings.HasPrefix(response, "ERROR:") {
		sendErrorResponse(conn, mode)
		return errors.New(response)
	}
	if response != "CONNECTED" {
		sendErrorResponse(conn, mode)
		return fmt.Errorf("意外响应: %s", response)
	}

	// 发送成功响应
	if err := sendSuccessResponse(conn, mode); err != nil {
		return err
	}

	ps.logf("[代理] %s 已连接: %s", clientAddr, target)

	// 双向转发
	done := make(chan bool, 2)

	// Client -> Server
	go func() {
		buf := make([]byte, 8192) // 减少从32768到8192字节
		for {
			n, err := conn.Read(buf)
			if err != nil {
				mu.Lock()
				wsConn.WriteMessage(websocket.TextMessage, []byte("CLOSE"))
				mu.Unlock()
				done <- true
				return
			}

			mu.Lock()
			err = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
			mu.Unlock()
			if err != nil {
				done <- true
				return
			}
		}
	}()

	// Server -> Client
	go func() {
		for {
			mt, msg, err := wsConn.ReadMessage()
			if err != nil {
				done <- true
				return
			}

			if mt == websocket.TextMessage {
				if string(msg) == "CLOSE" {
					done <- true
					return
				}
			}

			if _, err := conn.Write(msg); err != nil {
				done <- true
				return
			}
		}
	}()

	<-done
	ps.logf("[代理] %s 已断开: %s", clientAddr, target)
	return nil
}

// ======================== 直连处理 ========================

// handleDirectConnection 处理直连（绕过代理）
func (ps *ProxyServer) handleDirectConnection(conn net.Conn, target, clientAddr string, mode int, firstFrame string) error {
	// 检查服务器是否已经停止
	ps.mu.RLock()
	isRunning := ps.isRunning
	ps.mu.RUnlock()

	if !isRunning {
		return errors.New("server is stopping")
	}

	// 解析目标地址
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// 如果没有端口，根据模式添加默认端口
		host = target
		if mode == modeHTTPConnect {
			port = "443" // HTTPS
		} else if mode == modeHTTPProxy {
			port = "80" // HTTP
		} else {
			port = "80" // SOCKS5 默认
		}
		target = net.JoinHostPort(host, port)
	}

	ps.logf("[直连] %s -> %s (正在连接...)", clientAddr, target)

	// 直接连接到目标
	targetConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		ps.logf("[直连] %s -> %s 连接失败: %v", clientAddr, target, err)
		sendErrorResponse(conn, mode)
		return fmt.Errorf("直连失败: %w", err)
	}
	defer targetConn.Close()

	ps.logf("[直连] %s -> %s 连接成功", clientAddr, target)

	// 发送成功响应
	if err := sendSuccessResponse(conn, mode); err != nil {
		return err
	}

	// 如果有预设的第一帧数据，先发送
	if firstFrame != "" {
		if _, err := targetConn.Write([]byte(firstFrame)); err != nil {
			ps.logf("[直连] %s -> %s 发送第一帧失败: %v", clientAddr, target, err)
			return err
		}
	}

	// 改进的双向转发 - 等待连接完成
	done := make(chan struct{}, 2)

	// Client -> Target
	go func() {
		defer func() { done <- struct{}{} }()
		_, err := io.Copy(targetConn, conn)
		if err != nil && !isNormalCloseError(err) {
			ps.logf("[直连] %s -> %s 转发数据失败: %v", clientAddr, target, err)
		}
	}()

	// Target -> Client
	go func() {
		defer func() { done <- struct{}{} }()
		_, err := io.Copy(conn, targetConn)
		if err != nil && !isNormalCloseError(err) {
			ps.logf("[直连] %s -> %s 接收数据失败: %v", clientAddr, target, err)
		}
	}()

	// 等待任一方向完成
	<-done
	ps.logf("[直连] %s -> %s 连接已断开", clientAddr, target)

	return nil
}

// ======================== 响应辅助函数 ========================

func sendErrorResponse(conn net.Conn, mode int) {
	switch mode {
	case modeSOCKS5:
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	case modeHTTPConnect, modeHTTPProxy:
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
	}
}

func sendSuccessResponse(conn net.Conn, mode int) error {
	switch mode {
	case modeSOCKS5:
		// SOCKS5 成功响应
		_, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return err
	case modeHTTPConnect:
		// HTTP CONNECT 需要发送 200 响应
		_, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		return err
	case modeHTTPProxy:
		// HTTP GET/POST 等不需要发送响应，直接转发目标服务器的响应
		return nil
	}
	return nil
}
