package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// PoolClient 包装客户端，记录最后使用时间
type PoolClient struct {
	client     *client.Client
	lastUsedAt time.Time
}

// MCPClientPool MCP客户端连接池 - 由DaemonServer统一管理协程
type MCPClientPool struct {
	registry     *AppConfig
	clients      []*PoolClient
	clientChan   chan *PoolClient
	maxSize      int
	currentSize  int32 // 原子计数，当前总客户端数（包括已借出）
	mutex        sync.Mutex
	expandMutex  sync.Mutex // 扩容互斥锁，避免并发过度创建
	initialized  bool
	initOnce     sync.Once // 确保初始化只执行一次
	idleTimeout  time.Duration // 空闲超时时间
	needExpand   bool          // 标记是否需要扩容（由DaemonServer检查）
	poolMutex    sync.RWMutex  // 保护needExpand字段
}

// NewMCPClientPool 创建新的客户端连接池（不再启动独立协程，由DaemonServer统一管理）
func NewMCPClientPool(registry *AppConfig, maxSize int) *MCPClientPool {
	pool := &MCPClientPool{
		registry:    registry,
		maxSize:     maxSize,
		clients:     make([]*PoolClient, 0),
		clientChan:  make(chan *PoolClient, maxSize),
		idleTimeout: 5 * time.Minute,
		needExpand:  false,
	}
	return pool
}

// Initialize 初始化连接池（预创建1个客户端，其余动态扩展）
func (p *MCPClientPool) Initialize() error {
	p.initOnce.Do(func() {
		logger.Info(fmt.Sprintf("MCP client pool initializing (pre-create 1, max: %d)...", p.maxSize))

		start := time.Now()
		pc, err := p.createClient()
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to pre-create first client: %v", err))
			return
		}

		select {
		case p.clientChan <- pc:
		default:
		}
		atomic.AddInt32(&p.currentSize, 1)

		logger.Info(fmt.Sprintf("Pre-created first client in %v", time.Since(start)))

		p.mutex.Lock()
		p.initialized = true
		p.mutex.Unlock()

		// 标记需要扩容，由DaemonServer统一处理
		p.MarkExpand()
	})

	p.mutex.Lock()
	initialized := p.initialized
	p.mutex.Unlock()

	if !initialized {
		return fmt.Errorf("failed to initialize client pool")
	}
	return nil
}

// MarkExpand 标记需要扩容（DaemonServer会统一处理）
func (p *MCPClientPool) MarkExpand() {
	p.poolMutex.Lock()
	p.needExpand = true
	p.poolMutex.Unlock()
}

// ShouldExpand 检查是否需要扩容（由DaemonServer调用）
// 注意：返回true后，DaemonServer应该调用DoExpandAndClear，而不是单独调用DoExpand
func (p *MCPClientPool) ShouldExpand() bool {
	p.poolMutex.RLock()
	defer p.poolMutex.RUnlock()
	return p.needExpand
}

// ClearExpandFlag 清除扩容标记（在扩容完成后调用）
func (p *MCPClientPool) ClearExpandFlag() {
	p.poolMutex.Lock()
	p.needExpand = false
	p.poolMutex.Unlock()
}

// createClient 创建一个新的客户端
func (p *MCPClientPool) createClient() (*PoolClient, error) {
	start := time.Now()
	mcpClient, err := createMCPClient(p.registry)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	logger.Info(fmt.Sprintf("Created new MCP client in %v", time.Since(start)))
	return &PoolClient{
		client:     mcpClient,
		lastUsedAt: time.Now(),
	}, nil
}

// Acquire 获取一个客户端（使用 channel 阻塞获取，消除 CPU 空转）
func (p *MCPClientPool) Acquire(ctx context.Context) (*PoolClient, error) {
	p.mutex.Lock()
	if !p.initialized {
		p.mutex.Unlock()
		return nil, fmt.Errorf("client pool not initialized")
	}
	p.mutex.Unlock()

	currentTotal := int(atomic.LoadInt32(&p.currentSize))
	if currentTotal < p.maxSize && len(p.clientChan) == 0 {
		p.MarkExpand()
	}

	for {
		select {
		case pc := <-p.clientChan:
			if pc.client != nil {
				pc.lastUsedAt = time.Now()

				// 获取连接后，如果空闲连接不足 targetIdle，标记需要扩容
				currentIdle := len(p.clientChan)
				if currentIdle < 1 && int(atomic.LoadInt32(&p.currentSize)) < p.maxSize {
					p.MarkExpand()
				}
				return pc, nil
			}
			atomic.AddInt32(&p.currentSize, -1)
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for available client in pool (max: %d): %v", p.maxSize, ctx.Err())
		}
	}
}

// AcquireNoExpand 获取一个客户端，但不触发扩容（用于 tools/list 等轻量级操作）
func (p *MCPClientPool) AcquireNoExpand(ctx context.Context) (*PoolClient, error) {
	p.mutex.Lock()
	if !p.initialized {
		p.mutex.Unlock()
		return nil, fmt.Errorf("client pool not initialized")
	}
	p.mutex.Unlock()

	for {
		select {
		case pc := <-p.clientChan:
			if pc.client != nil {
				pc.lastUsedAt = time.Now()
				return pc, nil
			}
			atomic.AddInt32(&p.currentSize, -1)
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for available client in pool (max: %d): %v", p.maxSize, ctx.Err())
		}
	}
}

// DoExpand 执行扩容（由DaemonServer统一调用）
func (p *MCPClientPool) DoExpand() {
	p.expandMutex.Lock()
	defer p.expandMutex.Unlock()

	// 重新计算实际需要创建的数量，避免并发时过度创建
	currentTotal := int(atomic.LoadInt32(&p.currentSize))
	chanLen := len(p.clientChan)

	targetIdle := 1
	needCreate := targetIdle - chanLen

	if needCreate <= 0 {
		return
	}

	if currentTotal+needCreate > p.maxSize {
		needCreate = p.maxSize - currentTotal
	}

	if needCreate <= 0 {
		return
	}

	logger.Info(fmt.Sprintf("Expanding pool: current=%d, inChannel=%d, creating=%d", currentTotal, chanLen, needCreate))

	// 同步创建，避免并发时过度创建
	for i := 0; i < needCreate; i++ {
		// 再次检查，避免重复创建
		currentTotal = int(atomic.LoadInt32(&p.currentSize))
		chanLen = len(p.clientChan)
		if chanLen >= targetIdle || currentTotal >= p.maxSize {
			break
		}

		if atomic.AddInt32(&p.currentSize, 1) > int32(p.maxSize) {
			atomic.AddInt32(&p.currentSize, -1)
			break
		}

		newPc, err := p.createClient()
		if err != nil {
			logger.Error(fmt.Sprintf("Failed to create client during expand: %v", err))
			atomic.AddInt32(&p.currentSize, -1)
			continue
		}

		select {
		case p.clientChan <- newPc:
		default:
			if newPc.client != nil {
				newPc.client.Close()
			}
			atomic.AddInt32(&p.currentSize, -1)
		}
	}
}

// Release 释放客户端回连接池
func (p *MCPClientPool) Release(pc *PoolClient) {
	if pc == nil || pc.client == nil {
		return
	}

	// 仅更新时间戳，不创建新对象
	pc.lastUsedAt = time.Now()

	select {
	case p.clientChan <- pc:
		// 成功归还
	default:
		// 缓冲池满，执行底层连接关闭
		pc.client.Close()
		atomic.AddInt32(&p.currentSize, -1)
	}
}

// Discard 丢弃坏死连接并触发扩容
func (p *MCPClientPool) Discard(pc *PoolClient) {
	if pc == nil || pc.client == nil {
		return
	}

	pc.client.Close()
	atomic.AddInt32(&p.currentSize, -1)

	if int(atomic.LoadInt32(&p.currentSize)) < p.maxSize {
		p.MarkExpand()
	}
}

// CleanupIdleClients 清理空闲超时的客户端（由DaemonServer统一调用）
func (p *MCPClientPool) CleanupIdleClients() int {
	if !p.initialized {
		return 0
	}

	now := time.Now()
	closedCount := 0

	itemsToCheck := len(p.clientChan)
	for i := 0; i < itemsToCheck; i++ {
		select {
		case pc := <-p.clientChan:
			if now.Sub(pc.lastUsedAt) > p.idleTimeout {
				// 关闭客户端（即使是nil也要减少计数）
				if pc.client != nil {
					go pc.client.Close()
				}
				closedCount++
				atomic.AddInt32(&p.currentSize, -1)
			} else {
				select {
				case p.clientChan <- pc:
				default:
				}
			}
		default:
			break
		}
	}

	return closedCount
}

// CallTool 使用连接池执行工具调用
func (p *MCPClientPool) CallTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}

	res, err := pc.client.CallTool(ctx, request)
	if err != nil {
		p.Discard(pc)
		return res, err
	}

	p.Release(pc)
	return res, nil
}

// ListTools 使用连接池获取工具列表（不触发扩容）
func (p *MCPClientPool) ListTools(ctx context.Context, request mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	// 使用 AcquireNoExpand，避免 tools/list 触发连接池扩容
	pc, err := p.AcquireNoExpand(ctx)
	if err != nil {
		return nil, err
	}

	res, err := pc.client.ListTools(ctx, request)
	if err != nil {
		p.Discard(pc)
		return res, err
	}

	p.Release(pc)
	return res, nil
}

// Close 关闭连接池中的所有客户端
func (p *MCPClientPool) Close() {
	// 关闭 channel 中的客户端
	for {
		select {
		case pc := <-p.clientChan:
			if pc.client != nil {
				pc.client.Close()
			}
		default:
			// channel 已空，退出循环
			goto done
		}
	}
done:
	atomic.StoreInt32(&p.currentSize, 0)
	p.initialized = false
	logger.Info("MCP client pool closed")
}

// Size 返回当前空闲连接数
func (p *MCPClientPool) Size() int {
	return len(p.clientChan)
}

// TotalSize 返回当前总连接数
func (p *MCPClientPool) TotalSize() int {
	return int(atomic.LoadInt32(&p.currentSize))
}
