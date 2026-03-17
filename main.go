package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// loadConfig 加载配置文件
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := sonic.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	var rawConfig map[string]json.RawMessage
	if err := sonic.Unmarshal(data, &rawConfig); err != nil {
		return nil, err
	}

	reservedKeys := map[string]bool{
		"host": true,
		"port": true,
	}

	config.ClientRegistry = make(map[string]*AppConfig)
	for key, value := range rawConfig {
		if reservedKeys[key] {
			continue
		}

		var appConfig AppConfig
		if err := sonic.Unmarshal(value, &appConfig); err == nil && appConfig.Command != "" {
			appConfig.AppName = key
			config.ClientRegistry[key] = &appConfig
		} else {
			logger.Warn(fmt.Sprintf("Ignored unmapped root key or invalid app config: %s", key))
		}
	}

	return &config, nil
}

func main() {
	// 打印启动信息
	fmt.Fprintln(os.Stderr, "╔═══════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║                                                           ║")
	fmt.Fprintln(os.Stderr, "║   Bifrost MCP Proxy - Lightweight MCP Gateway            ║")
	fmt.Fprintln(os.Stderr, "║                                                           ║")
	fmt.Fprintln(os.Stderr, "╚═══════════════════════════════════════════════════════════╝")

	// 获取可执行文件所在目录
	execPath, err := os.Executable()
	if err != nil {
		logger.Error(fmt.Sprintf("failed to get executable path: %v", err))
		os.Exit(1)
	}
	execDir := filepath.Dir(execPath)

	var configPath string
	isDaemon := false

	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-daemon" {
			isDaemon = true
		} else if os.Args[i] == "-config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
			i++
		}
	}

	if configPath == "" {
		if envConfig := os.Getenv("BIFROST_CONFIG"); envConfig != "" {
			configPath = envConfig
		} else {
			configPath = filepath.Join(execDir, "config", "config.json")
		}
	}

	config, err := loadConfig(configPath)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to load config from %s: %v", configPath, err))
		os.Exit(1)
	}
	logger.Info(fmt.Sprintf("Loaded config from: %s", configPath))

	if len(config.ClientRegistry) == 0 {
		logger.Error("no valid app configurations found in config")
		os.Exit(1)
	}

	// 从环境变量读取 AppName
	appName := os.Getenv("AppName")
	if !isDaemon && appName == "" {
		logger.Error("AppName environment variable is required")
		os.Exit(1)
	}

	// 如果是守护进程模式，直接运行
	if isDaemon {
		logger.init("daemon")
		runDaemonMode(config, configPath)
		return
	}

	// 初始化日志文件
	logger.init(appName)

	// 查找注册表
	registry, ok := config.ClientRegistry[appName]
	if !ok {
		logger.Error(fmt.Sprintf("app_name '%s' not found in registry", appName))
		os.Exit(1)
	}

	logger.Info(fmt.Sprintf("starting MCP proxy for app: %s", appName))
	logger.Info(fmt.Sprintf("target: %s %s", registry.Command, strings.Join(registry.Args, " ")))

	// 检查守护进程是否已在运行
	if isDaemonRunning() {
		logger.Info(fmt.Sprintf("[%s] Connecting to existing daemon...", appName))
		if err := runClientMode(appName, config); err != nil {
			logger.Error(fmt.Sprintf("failed to connect to daemon: %v", err))
			os.Exit(1)
		}
		return
	}

	// 启动守护进程
	logger.Info(fmt.Sprintf("[%s] Starting daemon process...", appName))
	if err := startDaemonProcess(configPath); err != nil {
		logger.Error(fmt.Sprintf("failed to start daemon: %v", err))
		os.Exit(1)
	}

	// 等待守护进程启动（最多等待10秒）
	logger.Info(fmt.Sprintf("[%s] Waiting for daemon to start...", appName))
	started := false
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		if isDaemonRunning() {
			started = true
			break
		}
	}
	if !started {
		logger.Error("daemon failed to start within 10 seconds")
		os.Exit(1)
	}

	// 连接到守护进程
	logger.Info(fmt.Sprintf("[%s] Connecting to daemon...", appName))
	if err := runClientMode(appName, config); err != nil {
		logger.Error(fmt.Sprintf("failed to connect to daemon: %v", err))
		os.Exit(1)
	}
}
