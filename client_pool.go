package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPClientPool 单例管理器，利用 JSON-RPC 协议原生的并发多路复用能力
// 一个 stdio 连接即可处理所有并发请求，无需进程池
type MCPClientPool struct {
	registry    *AppConfig
	client      *client.Client
	mutex       sync.RWMutex
	initialized bool
	initOnce    sync.Once
}

// NewMCPClientPool 创建新的客户端连接池（单例模式）
// maxSize 参数被忽略，强制采用单进程复用模式
func NewMCPClientPool(registry *AppConfig, maxSize int) *MCPClientPool {
	return &MCPClientPool{
		registry: registry,
	}
}

// Initialize 初始化 MCP 客户端（单例）
func (p *MCPClientPool) Initialize() error {
	var err error
	p.initOnce.Do(func() {
		start := time.Now()
		p.client, err = createMCPClient(p.registry)
		if err == nil {
			p.initialized = true
			logger.Info(fmt.Sprintf("Created single multiplexed MCP client in %v", time.Since(start)))
		} else {
			logger.Error(fmt.Sprintf("Failed to create client: %v", err))
		}
	})
	if !p.initialized {
		return fmt.Errorf("failed to initialize client pool")
	}
	return nil
}

// CallTool 使用单一客户端执行工具调用
// 利用 JSON-RPC 多路复用支持并发调用
func (p *MCPClientPool) CallTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p.mutex.RLock()
	if !p.initialized || p.client == nil {
		p.mutex.RUnlock()
		return nil, fmt.Errorf("client not initialized")
	}
	c := p.client
	p.mutex.RUnlock()

	return c.CallTool(ctx, request)
}

// ListTools 使用单一客户端获取工具列表
// 利用 JSON-RPC 多路复用支持并发调用
func (p *MCPClientPool) ListTools(ctx context.Context, request mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	p.mutex.RLock()
	if !p.initialized || p.client == nil {
		p.mutex.RUnlock()
		return nil, fmt.Errorf("client not initialized")
	}
	c := p.client
	p.mutex.RUnlock()

	return c.ListTools(ctx, request)
}

// Close 关闭 MCP 客户端
func (p *MCPClientPool) Close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.client != nil {
		p.client.Close()
		p.client = nil
	}
	p.initialized = false
	logger.Info("Multiplexed MCP client closed")
}
