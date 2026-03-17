package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ToolsCache 工具列表缓存
type ToolsCache struct {
	tools    []mcp.Tool
	hasValue bool
	mutex    sync.RWMutex
}

func (tc *ToolsCache) IsValid() bool {
	tc.mutex.RLock()
	defer tc.mutex.RUnlock()
	return tc.hasValue && tc.tools != nil
}

func (tc *ToolsCache) Get() []mcp.Tool {
	tc.mutex.RLock()
	defer tc.mutex.RUnlock()
	return tc.tools
}

func (tc *ToolsCache) Set(tools []mcp.Tool) {
	tc.mutex.Lock()
	defer tc.mutex.Unlock()
	tc.tools = tools
	tc.hasValue = true
}

func (tc *ToolsCache) Invalidate() {
	tc.mutex.Lock()
	defer tc.mutex.Unlock()
	tc.tools = nil
	tc.hasValue = false
}

// StdioMCPProxy stdio MCP 代理服务器
type StdioMCPProxy struct {
	appName     string
	registryMu  sync.RWMutex
	registry    *AppConfig
	clientPool  *MCPClientPool
	rateEngine  *RateLimitEngine
	toolsCache  *ToolsCache
}

func (s *StdioMCPProxy) GetRegistry() *AppConfig {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	return s.registry
}

func (s *StdioMCPProxy) SetRegistry(registry *AppConfig) {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	s.registry = registry
}

func (s *StdioMCPProxy) Start() error {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		response := s.handleRequest(line)
		if response != "" {
			fmt.Println(response)
		}
	}

	return scanner.Err()
}

func (s *StdioMCPProxy) handleRequest(requestLine string) string {
	reqStart := time.Now()
	var reqBody map[string]interface{}
	if err := sonic.UnmarshalString(requestLine, &reqBody); err != nil {
		return s.createErrorResponse(nil, -32700, "Parse error")
	}

	method, _ := reqBody["method"].(string)
	id, hasID := reqBody["id"]

	if !hasID {
		if method == "notifications/initialized" {
			logger.Info(fmt.Sprintf("[%s] received notifications/initialized", s.appName))
			return s.handleNotification(method, reqBody)
		}
		return ""
	}

	logger.Info(fmt.Sprintf("[%s] handling method: %s", s.appName, method))

	switch method {
	case "initialize":
		return s.handleInitialize(id, reqBody)
	case "tools/list":
		return s.handleToolsList(id)
	case "tools/call":
		var rateResult *RateLimitResult
		if s.rateEngine != nil {
			rateStart := time.Now()
			rateResult = s.rateEngine.CheckAndIncrement(s.appName)
			logger.Info(fmt.Sprintf("[%s] rate limit check took %v", s.appName, time.Since(rateStart)))

			if !rateResult.Allowed {
				logger.Info(fmt.Sprintf("[%s] request blocked by rate limit: %s", s.appName, rateResult.BlockedReason))
				return s.createRateLimitErrorResponse(id, rateResult)
			}
		}

		callStart := time.Now()
		result := s.handleToolsCall(id, reqBody, rateResult)
		logger.Info(fmt.Sprintf("[%s] tool call took %v, total request took %v", s.appName, time.Since(callStart), time.Since(reqStart)))
		return result
	default:
		return s.createErrorResponse(id, -32601, fmt.Sprintf("unsupported method: %s", method))
	}
}

func (s *StdioMCPProxy) handleInitialize(id interface{}, reqBody map[string]interface{}) string {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{},
			},
			"serverInfo": map[string]interface{}{
				"name":    "mcp-limit-tool",
				"version": "1.0.0",
			},
		},
	}
	result, _ := sonic.MarshalString(resp)
	return result
}

func (s *StdioMCPProxy) handleToolsList(id interface{}) string {
	if s.toolsCache != nil && s.toolsCache.IsValid() {
		logger.Info(fmt.Sprintf("[%s] tools/list cache hit", s.appName))
		tools := s.toolsCache.Get()

		result := &mcp.ListToolsResult{Tools: tools}

		registry := s.GetRegistry()
		if len(registry.AllowedTools) > 0 {
			allowedSet := make(map[string]bool)
			for _, tool := range registry.AllowedTools {
				allowedSet[tool] = true
			}

			filteredTools := make([]mcp.Tool, 0)
			for _, tool := range result.Tools {
				if allowedSet[tool.Name] {
					filteredTools = append(filteredTools, tool)
				}
			}
			result.Tools = filteredTools
		}

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		}
		resultBytes, _ := sonic.Marshal(resp)
		return string(resultBytes)
	}

	logger.Info(fmt.Sprintf("[%s] tools/list cache miss, fetching from MCP server...", s.appName))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	listRequest := mcp.ListToolsRequest{
		PaginatedRequest: mcp.PaginatedRequest{
			Request: mcp.Request{
				Method: string(mcp.MethodToolsList),
			},
		},
	}

	result, err := s.clientPool.ListTools(ctx, listRequest)
	if err != nil {
		if s.toolsCache != nil {
			s.toolsCache.Invalidate()
		}
		return s.createErrorResponse(id, -32603, fmt.Sprintf("failed to list tools: %v", err))
	}

	if s.toolsCache != nil {
		s.toolsCache.Set(result.Tools)
		logger.Info(fmt.Sprintf("[%s] tools/list cached, %d tools", s.appName, len(result.Tools)))
	}

	registry := s.GetRegistry()
	if len(registry.AllowedTools) > 0 {
		allowedSet := make(map[string]bool)
		for _, tool := range registry.AllowedTools {
			allowedSet[tool] = true
		}

		filteredTools := make([]mcp.Tool, 0)
		for _, tool := range result.Tools {
			if allowedSet[tool.Name] {
				filteredTools = append(filteredTools, tool)
			}
		}
		result.Tools = filteredTools
	}

	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	resultBytes, _ := sonic.Marshal(resp)
	return string(resultBytes)
}

func (s *StdioMCPProxy) handleNotification(method string, reqBody map[string]interface{}) string {
	switch method {
	case "notifications/initialized":
		logger.Info(fmt.Sprintf("[%s] MCP client initialized", s.appName))
		return ""
	default:
		return ""
	}
}

func (s *StdioMCPProxy) handleToolsCall(id interface{}, reqBody map[string]interface{}, rateResult *RateLimitResult) string {
	params, _ := reqBody["params"].(map[string]interface{})
	toolName, _ := params["name"].(string)
	toolArgs, _ := params["arguments"].(map[string]interface{})

	registry := s.GetRegistry()
	if len(registry.AllowedTools) > 0 {
		allowed := false
		for _, allowedTool := range registry.AllowedTools {
			if allowedTool == toolName {
				allowed = true
				break
			}
		}
		if !allowed {
			return s.createErrorResponse(id, -32601, fmt.Sprintf("tool '%s' is not allowed. allowed tools: %v", toolName, registry.AllowedTools))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	callRequest := mcp.CallToolRequest{
		Request: mcp.Request{
			Method: string(mcp.MethodToolsCall),
		},
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: toolArgs,
		},
	}
	result, err := s.clientPool.CallTool(ctx, callRequest)
	if err != nil {
		return s.createErrorResponse(id, -32603, fmt.Sprintf("failed to call tool: %v", err))
	}

	var resultMap map[string]interface{}
	resultBytes, _ := sonic.Marshal(result)
	sonic.Unmarshal(resultBytes, &resultMap)

	if rateResult != nil && len(rateResult.Details) > 0 {
		rateLimitInfo := "\n\n[mcp_limit_tool]:\n"
		periodOrder := []string{"hourly", "daily", "weekly", "monthly"}

		for _, period := range periodOrder {
			if detail, ok := rateResult.Details[period]; ok {
				usage := detail.Total - detail.Remaining
				rateLimitInfo += fmt.Sprintf("%s %d/%d\n", period, usage, detail.Total)
			}
		}

		if contents, ok := resultMap["content"].([]interface{}); ok && len(contents) > 0 {
			for i, content := range contents {
				if contentMap, ok := content.(map[string]interface{}); ok {
					if text, ok := contentMap["text"].(string); ok {
						contentMap["text"] = text + rateLimitInfo
						contents[i] = contentMap
						break
					}
				}
			}
		}
	}

	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  resultMap,
	}
	respBytes, _ := sonic.Marshal(resp)
	return string(respBytes)
}

func (s *StdioMCPProxy) createErrorResponse(id interface{}, code int, message string) string {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	result, _ := sonic.MarshalString(resp)
	return result
}

func (s *StdioMCPProxy) createRateLimitErrorResponse(id interface{}, result *RateLimitResult) string {
	limitsData := make(map[string]interface{})
	periodOrder := []string{"hourly", "daily", "weekly", "monthly"}

	for _, period := range periodOrder {
		if detail, ok := result.Details[period]; ok {
			limitsData[period] = map[string]interface{}{
				"usage":     detail.Total,
				"remaining": detail.Remaining,
			}
		}
	}

	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    -32002,
			"message": fmt.Sprintf("[mcp_limit_tool] Rate limit exceeded for '%s': %s", s.appName, result.BlockedReason),
			"data": map[string]interface{}{
				"limits":     limitsData,
				"blocked_by": result.BlockedBy,
				"source":     "mcp_limit_tool",
			},
		},
	}
	resultBytes, _ := sonic.Marshal(resp)
	return string(resultBytes)
}

func createMCPClient(registry *AppConfig) (*client.Client, error) {
	envVars := os.Environ()
	hasNodeOptions := false

	for k, v := range registry.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
		if k == "NODE_OPTIONS" {
			hasNodeOptions = true
		}
	}

	if !hasNodeOptions {
		envVars = append(envVars, "NODE_OPTIONS=--dns-result-order=ipv4first")
	}

	mcpClient, err := client.NewStdioMCPClient(registry.Command, envVars, registry.Args...)
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP client: %v", err)
	}

	if err := mcpClient.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to start MCP client: %v", err)
	}

	ctxInit, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelInit()

	initRequest := mcp.InitializeRequest{
		Request: mcp.Request{
			Method: string(mcp.MethodInitialize),
		},
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "mcp-limit-tool",
				Version: "1.0.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	}

	_, err = mcpClient.Initialize(ctxInit, initRequest)
	if err != nil {
		mcpClient.Close()
		return nil, fmt.Errorf("failed to initialize MCP client: %v", err)
	}

	return mcpClient, nil
}
