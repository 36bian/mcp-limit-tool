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
	logFile *os.File
	logDir  string
	mu      sync.Mutex
}

func (l *fileLogger) SetLogDir(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logDir = dir
}

func (l *fileLogger) init(appName string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.logFile != nil {
		l.logFile.Close()
		l.logFile = nil
	}

	var logDir string
	if l.logDir != "" {
		logDir = l.logDir
	} else {
		logDir = "tmp"
	}
	os.MkdirAll(logDir, 0755)

	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", appName))

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

func (l *fileLogger) Warn(msg string) {
	l.log("[WARN] " + msg)
}

func (l *fileLogger) Error(msg string) {
	l.log("[ERROR] " + msg)
}

func (l *fileLogger) log(msg string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	line := fmt.Sprintf("[%s] %s\n", timestamp, msg)

	fmt.Fprint(os.Stderr, line)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logFile != nil {
		l.logFile.WriteString(line)
		l.logFile.Sync()
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
