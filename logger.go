package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var logger = &fileLogger{}

type fileLogger struct {
	logFile     *os.File
	logDir      string
	appName     string
	mu          sync.RWMutex
	enableStderr bool
	enableFile   bool
	debugMode    bool
}

type LogConfig struct {
	AppName      string
	LogDir       string
	EnableStderr bool
	EnableFile   bool
	DebugMode    bool
}

func (l *fileLogger) Configure(cfg LogConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.appName = cfg.AppName
	l.enableStderr = cfg.EnableStderr
	l.enableFile = cfg.EnableFile
	l.debugMode = cfg.DebugMode

	if cfg.LogDir != "" {
		l.logDir = cfg.LogDir
	} else {
		execPath, _ := os.Executable()
		l.logDir = filepath.Join(filepath.Dir(execPath), "logs")
	}

	if l.enableFile {
		l.initLocked()
	} else {
		if l.logFile != nil {
			l.logFile.Close()
			l.logFile = nil
		}
	}
}

func (l *fileLogger) initLocked() {
	if l.logFile != nil {
		l.logFile.Close()
		l.logFile = nil
	}

	os.MkdirAll(l.logDir, 0755)

	logPath := filepath.Join(l.logDir, fmt.Sprintf("%s.log", l.appName))

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
		return
	}
	l.logFile = f
}

func (l *fileLogger) Info(msg string) {
	l.log(msg)
}

func (l *fileLogger) Debug(msg string) {
	l.mu.RLock()
	if l.debugMode {
		l.mu.RUnlock()
		l.log("[DEBUG] " + msg)
	} else {
		l.mu.RUnlock()
	}
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

	l.mu.RLock()
	enableStderr := l.enableStderr
	enableFile := l.enableFile
	logFile := l.logFile
	l.mu.RUnlock()

	if enableStderr {
		fmt.Fprint(os.Stderr, line)
	}

	if enableFile && logFile != nil {
		l.mu.Lock()
		logFile.WriteString(line)
		logFile.Sync()
		l.mu.Unlock()
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
