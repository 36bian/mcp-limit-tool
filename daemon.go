package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
)

// daemonSocketPath 守护进程 Socket 路径
var daemonSocketPath string = func() string {
	if u, err := user.Current(); err == nil {
		return filepath.Join(os.TempDir(), fmt.Sprintf("mcp-limit-tool-%s.sock", u.Uid))
	}
	return filepath.Join(os.TempDir(), "mcp-limit-tool-daemon.sock")
}()

const (
	idleTimeout = 5 * time.Minute
)

// DaemonProxy 守护进程中的代理实例
type DaemonProxy struct {
	appName      string
	registry     *AppConfig
	proxy        *StdioMCPProxy
	mutex        sync.Mutex
	lastUsedAt   time.Time // 最后使用时间
	idleTimer    *time.Timer // 空闲定时器
}



// DaemonServer 守护进程服务器
type DaemonServer struct {
	listener       net.Listener
	config         *Config
	configPath     string                      // 配置文件路径
	proxies        map[string]*DaemonProxy
	mutex          sync.RWMutex
	configStore    *ConfigStore               // 配置存储（包含限流计数）
	rateEngine     *RateLimitEngine           // 限流引擎
	sem            chan struct{}              // 全局并发控制信号量

	// 生命周期与自毁控制
	activeConns    int32         // 当前活跃连接数
	idleTimer      *time.Timer   // 空闲自毁定时器
	lifecycleMu    sync.Mutex    // 生命周期锁
	shutdownChan   chan struct{} // 触发关闭的信号通道

	// 统一管理所有连接池的协程控制
	poolManagerStop chan struct{} // 停止连接池管理协程
}

// isDaemonRunning 检查守护进程是否正在运行
func isDaemonRunning() bool {
	socketPath := daemonSocketPath
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// addConnection 增加活跃连接，并取消自毁倒计时
func (ds *DaemonServer) addConnection() {
	ds.lifecycleMu.Lock()
	defer ds.lifecycleMu.Unlock()

	ds.activeConns++
	if ds.idleTimer != nil {
		ds.idleTimer.Stop()
		ds.idleTimer = nil
		logger.Info(fmt.Sprintf("New connection established. Active connections: %d. Cancelled auto-shutdown timer.", ds.activeConns))
	}
}

// removeConnection 减少活跃连接，若归零则启动自毁倒计时
func (ds *DaemonServer) removeConnection() {
	ds.lifecycleMu.Lock()
	defer ds.lifecycleMu.Unlock()

	ds.activeConns--
	if ds.activeConns <= 0 {
		ds.activeConns = 0
		logger.Info("All clients disconnected. Starting 10-minute auto-shutdown countdown...")
		ds.idleTimer = time.AfterFunc(10*time.Minute, func() {
			logger.Info("Daemon has been absolutely idle for 10 minutes. Triggering auto-shutdown.")
			close(ds.shutdownChan)
		})
	} else {
		logger.Info(fmt.Sprintf("Client disconnected. Remaining active connections: %d", ds.activeConns))
	}
}

// startDaemonProcess 启动守护进程
func startDaemonProcess(configPath string, debugMode bool) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	args := []string{"-daemon"}
	if debugMode {
		args = append(args, "-debug")
	}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}

	cmd := exec.Command(execPath, args...)

	if debugMode {
		logDir := filepath.Join(filepath.Dir(execPath), "logs")
		os.MkdirAll(logDir, 0755)
		logPath := filepath.Join(logDir, "daemon-sys.log")

		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
		} else {
			cmd.Stdout = nil
			cmd.Stderr = nil
		}
	} else {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	return cmd.Start()
}

// runDaemonMode 运行守护进程模式
func runDaemonMode(config *Config, configPath string) {
	logger.Info("Starting daemon mode...")

	// 设置信号处理：忽略 SIGHUP，捕获 SIGTERM/SIGINT 进行优雅退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	// 初始化配置存储（包含限流计数）
	configDir := getConfigDir()
	configStore, err := NewConfigStore(configDir, config)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to create config store: %v", err))
		os.Exit(1)
	}
	defer configStore.Stop()

	// 初始化限流引擎
	rateEngine := NewRateLimitEngine(configStore)

	shutdownChan := make(chan struct{})
	poolManagerStop := make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-sigChan:
				if sig == syscall.SIGHUP {
					logger.Info("Received SIGHUP, ignoring (daemon continues running)")
					continue
				}
				logger.Info(fmt.Sprintf("Received %v, shutting down gracefully...", sig))
				close(poolManagerStop)
				configStore.Stop()
				os.Remove(daemonSocketPath)
				os.Exit(0)
			case <-shutdownChan:
				logger.Info("Auto-shutdown executed. Cleaning up resources...")
				close(poolManagerStop)
				configStore.Stop()
				os.Remove(daemonSocketPath)
				os.Exit(0)
			}
		}
	}()

	logger.Info("Daemon started")

	socketPath := daemonSocketPath
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to create socket: %v", err))
		os.Exit(1)
	}
	defer listener.Close()

	// 强制设定 Socket 文件权限为仅所有者可读写 (0600)
	if err := os.Chmod(socketPath, 0600); err != nil {
		logger.Error(fmt.Sprintf("Failed to secure socket permissions: %v", err))
		os.Exit(1)
	}

	server := &DaemonServer{
		listener:        listener,
		config:          config,
		configPath:      configPath,
		proxies:         make(map[string]*DaemonProxy),
		configStore:     configStore,
		rateEngine:      rateEngine,
		sem:             make(chan struct{}, 100),
		shutdownChan:    shutdownChan,
		poolManagerStop: poolManagerStop,
	}

	// 启动统一管理协程（2个协程管理所有连接池）
	server.startPoolManager()

	server.lifecycleMu.Lock()
	server.idleTimer = time.AfterFunc(10*time.Minute, func() {
		logger.Info("Daemon started but no clients connected for 10 minutes. Auto-shutting down.")
		close(shutdownChan)
	})
	server.lifecycleMu.Unlock()

	logger.Info("Daemon started (lazy pool init, dynamic expand, 5min idle timeout, 10min auto-destruct)")
	logger.Info(fmt.Sprintf("Daemon listening on %s", socketPath))

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error(fmt.Sprintf("Accept error: %v", err))
			continue
		}
		go server.handleConnection(conn)
	}
}

// getOrCreateProxy 获取或创建代理实例
func (ds *DaemonServer) getOrCreateProxy(appName string) (*DaemonProxy, error) {
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	registry, ok := ds.configStore.GetAppRegistry(appName)
	if !ok {
		if proxy, exists := ds.proxies[appName]; exists {
			if proxy.proxy != nil && proxy.proxy.clientPool != nil {
				proxy.proxy.clientPool.Close()
			}
			delete(ds.proxies, appName)
		}
		return nil, fmt.Errorf("app '%s' not found in registry", appName)
	}

	if proxy, exists := ds.proxies[appName]; exists {
		if proxy.registry != registry {
			logger.Info(fmt.Sprintf("Config changed for %s, tearing down old pool and recreating...", appName))
			if proxy.proxy != nil && proxy.proxy.clientPool != nil {
				proxy.proxy.clientPool.Close()
			}
			delete(ds.proxies, appName)
		} else {
			proxy.lastUsedAt = time.Now()
			return proxy, nil
		}
	}

	clientPool := NewMCPClientPool(registry, 30)
	if err := clientPool.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize client pool: %v", err)
	}

	proxy := &DaemonProxy{
		appName:    appName,
		registry:   registry,
		lastUsedAt: time.Now(),
		proxy: &StdioMCPProxy{
			appName:    appName,
			registry:   registry,
			clientPool: clientPool,
			toolsCache: &ToolsCache{},
		},
	}

	proxy.proxy.rateEngine = ds.rateEngine
	proxy.startIdleTimer(ds)

	ds.proxies[appName] = proxy
	logger.Info(fmt.Sprintf("Created proxy for %s with multiplexed client (single connection)", appName))
	return proxy, nil
}

// startPoolManager 启动统一管理所有连接池的协程
// 已废除原有的协程池管理，现采用单一长连接多路复用
// JSON-RPC 协议原生支持在单一连接上并发处理多个请求，无需连接池
func (ds *DaemonServer) startPoolManager() {
	// 已废除原有的协程池管理。
	// 现采用单一长连接多路复用，彻底消除定期轮询扩容/清理带来的 CPU 唤醒开销。
	logger.Info("Pool manager disabled (using zero-overhead multiplexed connection)")
}

// cleanupAllIdleClients 清理所有连接池的空闲客户端
func (ds *DaemonServer) cleanupAllIdleClients() {
	ds.mutex.RLock()
	proxies := make([]*DaemonProxy, 0, len(ds.proxies))
	for _, proxy := range ds.proxies {
		proxies = append(proxies, proxy)
	}
	ds.mutex.RUnlock()

	totalClosed := 0
	for _, proxy := range proxies {
		if proxy.proxy != nil && proxy.proxy.clientPool != nil {
			closed := proxy.proxy.clientPool.CleanupIdleClients()
			totalClosed += closed
		}
	}

	if totalClosed > 0 {
		logger.Info(fmt.Sprintf("Cleaned up %d idle clients from all pools", totalClosed))
	}
}

// expandAllPools 检查并扩容所有需要扩容的连接池
func (ds *DaemonServer) expandAllPools() {
	ds.mutex.RLock()
	proxies := make([]*DaemonProxy, 0, len(ds.proxies))
	for _, proxy := range ds.proxies {
		proxies = append(proxies, proxy)
	}
	ds.mutex.RUnlock()

	for _, proxy := range proxies {
		if proxy.proxy != nil && proxy.proxy.clientPool != nil {
			if proxy.proxy.clientPool.ShouldExpand() {
				// 在协程中执行扩容，并在完成后清除标记
				go func(pool *MCPClientPool) {
					pool.DoExpand()
					pool.ClearExpandFlag()
				}(proxy.proxy.clientPool)
			}
		}
	}
}

// startIdleTimer 启动空闲超时定时器
func (dp *DaemonProxy) startIdleTimer(ds *DaemonServer) {
	dp.idleTimer = time.AfterFunc(5*time.Minute, func() {
		ds.mutex.Lock()
		defer ds.mutex.Unlock()

		// 检查是否超过5分钟没有使用
		if time.Since(dp.lastUsedAt) >= 5*time.Minute {
			// 销毁连接池
			if dp.proxy != nil && dp.proxy.clientPool != nil {
				dp.proxy.clientPool.Close()
			}
			// 从map中移除
			delete(ds.proxies, dp.appName)
			logger.Info(fmt.Sprintf("Destroyed idle proxy for %s (idle > 5min)", dp.appName))
		} else {
			// 重新启动定时器
			dp.startIdleTimer(ds)
		}
	})
}

// updateLastUsed 更新最后使用时间
func (dp *DaemonProxy) updateLastUsed() {
	dp.mutex.Lock()
	dp.lastUsedAt = time.Now()
	dp.mutex.Unlock()

	if dp.idleTimer != nil {
		dp.idleTimer.Stop()
		dp.idleTimer.Reset(5 * time.Minute)
	}
}

// handleConnection 处理客户端连接
// 逻辑：
// 1. appname匹配 -> 马上分配连接池并异步创建更多备用
// 2. appname不匹配 -> 创建错误连接，回复错误，维持5分钟活性
// 3. 其他错误 -> 复用通用错误连接
func (ds *DaemonServer) handleConnection(conn net.Conn) {
	ds.addConnection()
	defer ds.removeConnection()
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	var writeMu sync.Mutex

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 解析请求获取 appName 和 id
		var reqData map[string]interface{}
		if err := sonic.Unmarshal([]byte(line), &reqData); err != nil {
			ds.writeErrorResponse(writer, &writeMu, -32700, "Parse error", nil)
			continue
		}

		appName := ""
		if params, ok := reqData["params"].(map[string]interface{}); ok {
			if meta, ok := params["_meta"].(map[string]interface{}); ok {
				if name, ok := meta["appName"].(string); ok {
					appName = name
				}
			}
		}

		// 提取 id 字段用于响应
		var reqId interface{}
		if id, ok := reqData["id"]; ok {
			reqId = id
		}

		if appName == "" {
			ds.writeErrorResponse(writer, &writeMu, -32602, "Missing appName", reqId)
			continue
		}

		// 检查 appName 是否在配置中
		_, exists := ds.configStore.GetAppRegistry(appName)

		if exists {
			// 情况1: appname匹配 -> 使用正常代理流程
			ds.handleValidApp(conn, reader, writer, &writeMu, line, appName, reqId)
			return // 处理完成后退出此连接的循环
		} else {
			// 情况2: appname不匹配 -> 创建/复用错误连接，维持5分钟
			ds.handleInvalidApp(conn, reader, writer, &writeMu, line, appName, reqId)
			return
		}
	}
}

// writeErrorResponse 写入错误响应
func (ds *DaemonServer) writeErrorResponse(writer *bufio.Writer, writeMu *sync.Mutex, code int, message string, id interface{}) {
	writeMu.Lock()
	if id != nil {
		writer.WriteString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":%d,"message":"%s"},"id":%v}`, code, message, id) + "\n")
	} else {
		writer.WriteString(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":%d,"message":"%s"}}`, code, message) + "\n")
	}
	writer.Flush()
	writeMu.Unlock()
}

// handleValidApp 处理有效的app请求
func (ds *DaemonServer) handleValidApp(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, writeMu *sync.Mutex, line, appName string, reqId interface{}) {
	// 获取或创建代理（马上分配连接池）
	proxy, err := ds.getOrCreateProxy(appName)
	if err != nil {
		ds.writeErrorResponse(writer, writeMu, -32603, fmt.Sprintf("Failed to create proxy: %v", err), reqId)
		return
	}

	// 更新最后使用时间
	proxy.updateLastUsed()

	// 标记需要扩容（由DaemonServer统一管理）
	if proxy.proxy != nil && proxy.proxy.clientPool != nil {
		proxy.proxy.clientPool.MarkExpand()
	}

	// 处理请求
	ds.processRequest(conn, reader, writer, writeMu, line, appName, proxy.proxy, reqId)
}

// handleInvalidApp 处理无效的app请求
func (ds *DaemonServer) handleInvalidApp(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, writeMu *sync.Mutex, line, appName string, reqId interface{}) {
	logger.Warn(fmt.Sprintf("[InvalidApp] %s not found in registry, returning error", appName))

	ds.writeErrorResponse(writer, writeMu, -32602, fmt.Sprintf("App '%s' not found in registry", appName), reqId)
}

// processRequest 处理请求
func (ds *DaemonServer) processRequest(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer, writeMu *sync.Mutex, line, appName string, proxy *StdioMCPProxy, reqId interface{}) {
	handleWithTimeout := func(reqLine string, currentReqId interface{}) {
		select {
		case ds.sem <- struct{}{}:
		case <-time.After(10 * time.Second):
			ds.writeErrorResponse(writer, writeMu, -32000, "Server too busy", currentReqId)
			return
		}

		defer func() { <-ds.sem }()

		activeProxy, err := ds.getOrCreateProxy(appName)
		if err != nil {
			ds.writeErrorResponse(writer, writeMu, -32603, fmt.Sprintf("Proxy unavailable: %v", err), currentReqId)
			return
		}

		done := make(chan string, 1)
		go func() {
			done <- activeProxy.proxy.handleRequest(reqLine)
		}()

		select {
		case resp := <-done:
			if resp != "" {
				writeMu.Lock()
				writer.WriteString(resp + "\n")
				writer.Flush()
				writeMu.Unlock()
			}
		case <-time.After(90 * time.Second):
			ds.writeErrorResponse(writer, writeMu, -32603, "Internal request processing timeout", currentReqId)
		}
	}

	go handleWithTimeout(line, reqId)

	for {
		nextLine, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		nextLine = strings.TrimSpace(nextLine)
		if nextLine == "" {
			continue
		}

		var nextReqData map[string]interface{}
		var nextReqId interface{}
		if err := sonic.Unmarshal([]byte(nextLine), &nextReqData); err == nil {
			nextReqId = nextReqData["id"]
		}

		go handleWithTimeout(nextLine, nextReqId)
	}
}

// runClientMode 运行客户端模式
func runClientMode(appName string, config *Config) error {
	socketPath := daemonSocketPath
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	go func() {
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fmt.Print(line)
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var reqData map[string]interface{}
		if err := sonic.Unmarshal([]byte(line), &reqData); err == nil {
			params, ok := reqData["params"].(map[string]interface{})
			if !ok || params == nil {
				params = make(map[string]interface{})
				reqData["params"] = params
			}

			if meta, ok := params["_meta"].(map[string]interface{}); !ok || meta == nil {
				params["_meta"] = make(map[string]interface{})
			}
			meta, _ := params["_meta"].(map[string]interface{})
			meta["appName"] = appName

			if modifiedLine, err := sonic.Marshal(reqData); err == nil {
				line = string(modifiedLine)
			}
		}

		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			return err
		}
	}

	return nil
}


