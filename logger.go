package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var logger = &fileLogger{}

type fileLogger struct {
	logFile *os.File
	mu      sync.Mutex
}

func (l *fileLogger) init(appName string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.logFile != nil {
		return
	}

	// 避免写死 /tmp，使用标准库获取临时目录
	logDir := filepath.Join(os.TempDir(), "mcp-limit-tool-logs")

	// 严格检查目录创建错误，权限收紧为 0700
	if err := os.MkdirAll(logDir, 0700); err != nil {
		// 目录创建失败时，直接阻断启动，避免日志静默丢失
		log.Fatalf("Fatal: failed to create log directory '%s': %v", logDir, err)
	}

	// 打开日志文件（追加模式）
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", appName))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// 如果文件打开失败，使用 stderr
		return
	}
	l.logFile = f
}

func (l *fileLogger) Info(msg string) {
	l.log(msg)
}

func (l *fileLogger) Warn(msg string) {
	l.log("[WARN] " + msg)
}

func (l *fileLogger) Error(msg string) {
	l.log("[ERROR] " + msg)
}

func (l *fileLogger) log(msg string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	line := fmt.Sprintf("[%s] %s\n", timestamp, msg)

	// 同时输出到 stderr 和文件
	fmt.Fprint(os.Stderr, line)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logFile != nil {
		l.logFile.WriteString(line)
		l.logFile.Sync() // 立即刷新到磁盘
	}
}

func (l *fileLogger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logFile != nil {
		l.logFile.Close()
		l.logFile = nil
	}
}
