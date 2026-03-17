package main

import (
	"fmt"
	"time"
)

// RateLimitEngine 限流引擎
type RateLimitEngine struct {
	config *ConfigStore
}

// RateLimitResult 限流结果
type RateLimitResult struct {
	Allowed       bool
	BlockedBy     string
	BlockedReason string
	Details       map[string]*PeriodDetail
}

// PeriodDetail 周期详情
type PeriodDetail struct {
	Total     int64
	Remaining int64
	ResetAt   time.Time
	BlockedBy string `json:"-"` // 仅内部使用
}

// NewRateLimitEngine 创建限流引擎
func NewRateLimitEngine(config *ConfigStore) *RateLimitEngine {
	return &RateLimitEngine{
		config: config,
	}
}

// CheckAndIncrement 检查并增加计数
func (e *RateLimitEngine) CheckAndIncrement(appName string) *RateLimitResult {
	allowed, details := e.config.CheckAndInc(appName)

	result := &RateLimitResult{
		Allowed: allowed,
		Details: details,
	}

	if !allowed {
		if blockedDetail, ok := details["blocked_by"]; ok {
			result.BlockedBy = blockedDetail.BlockedBy
			result.BlockedReason = fmt.Sprintf("%s limit exceeded", blockedDetail.BlockedBy)
		}
	}

	return result
}
