package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fsnotify/fsnotify"
)

// getConfigDir 获取配置目录（与可执行文件同目录）
func getConfigDir() string {
	execPath, _ := os.Executable()
	execDir := filepath.Dir(execPath)
	return filepath.Join(execDir, "config")
}

// ConfigStore 管理配置热更新和限流计数
type ConfigStore struct {
	basePath    string
	config      *Config
	quotas      map[string]*QuotaPeriods
	mu          sync.RWMutex
	watcher     *fsnotify.Watcher
	stop        chan struct{}
	saveChan    chan struct{}
	lastSaveMux sync.Mutex
	lastSaveTime time.Time
}



// QuotaConfig 单个限流配置
type QuotaConfig struct {
	Usage     int64     `json:"usage"`
	Remaining int64     `json:"remain"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
}

// QuotaPeriods 所有周期配额
type QuotaPeriods struct {
	Hourly  *QuotaConfig `json:"hourly,omitempty"`
	Daily   *QuotaConfig `json:"daily,omitempty"`
	Weekly  *QuotaConfig `json:"weekly,omitempty"`
	Monthly *QuotaConfig `json:"monthly,omitempty"`
}

// NewConfigStore 创建配置存储
func NewConfigStore(basePath string, config *Config) (*ConfigStore, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	cs := &ConfigStore{
		basePath: basePath,
		config:   config,
		quotas:   make(map[string]*QuotaPeriods),
		watcher:  watcher,
		stop:     make(chan struct{}),
		saveChan: make(chan struct{}, 1),
	}

	// 确保目录存在
	os.MkdirAll(basePath, 0755)

	// 加载配置
	cs.loadAll()

	// 保存初始化后的 usage（根据 config 重新计算 remain）
	cs.saveQuotas()

	// 启动监听
	go cs.watch()

	// 启动自动保存
	go cs.autoSave()

	configPath := filepath.Join(cs.basePath, "config.json")
	usagePath := filepath.Join(cs.basePath, "auto_usage.json")
	watcher.Add(filepath.Dir(configPath))
	watcher.Add(filepath.Dir(usagePath))

	return cs, nil
}

// loadAll 加载所有配置并做首次对齐
func (cs *ConfigStore) loadAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	now := time.Now()

	if data, err := os.ReadFile(filepath.Join(cs.basePath, "auto_usage.json")); err == nil {
		sonic.Unmarshal(data, &cs.quotas)
	}

	if cs.quotas == nil {
		cs.quotas = make(map[string]*QuotaPeriods)
	}

	if cs.config != nil {
		for appName := range cs.quotas {
			if _, exists := cs.config.ClientRegistry[appName]; !exists {
				delete(cs.quotas, appName)
			}
		}

		for appName, appConfig := range cs.config.ClientRegistry {
			if _, exists := cs.quotas[appName]; !exists {
				cs.quotas[appName] = &QuotaPeriods{}
			}
			cs.reconcileQuota(cs.quotas[appName], appConfig.RateLimits, now)
		}
	}
}

// reconcileQuota 对齐单个 App 的限流周期
func (cs *ConfigStore) reconcileQuota(quota *QuotaPeriods, rateLimits map[string]*RateLimitConfig, now time.Time) {
	if rateLimits == nil {
		quota.Hourly = nil
		quota.Daily = nil
		quota.Weekly = nil
		quota.Monthly = nil
		return
	}

	if _, ok := rateLimits["hourly"]; !ok {
		quota.Hourly = nil
	}
	if _, ok := rateLimits["daily"]; !ok {
		quota.Daily = nil
	}
	if _, ok := rateLimits["weekly"]; !ok {
		quota.Weekly = nil
	}
	if _, ok := rateLimits["monthly"]; !ok {
		quota.Monthly = nil
	}

	for period, limitConfig := range rateLimits {
		if limitConfig == nil || limitConfig.Total <= 0 {
			continue
		}

		var quotaConfig *QuotaConfig
		switch period {
		case "hourly":
			if quota.Hourly == nil {
				quota.Hourly = &QuotaConfig{}
			}
			quotaConfig = quota.Hourly
		case "daily":
			if quota.Daily == nil {
				quota.Daily = &QuotaConfig{}
			}
			quotaConfig = quota.Daily
		case "weekly":
			if quota.Weekly == nil {
				quota.Weekly = &QuotaConfig{}
			}
			quotaConfig = quota.Weekly
		case "monthly":
			if quota.Monthly == nil {
				quota.Monthly = &QuotaConfig{}
			}
			quotaConfig = quota.Monthly
		}

		if quotaConfig == nil {
			continue
		}

		if quotaConfig.ResetAt.IsZero() {
			quotaConfig.Usage = 0
			quotaConfig.Remaining = limitConfig.Total
			quotaConfig.ResetAt = getNextPeriodStart(period, now)
		} else if now.After(quotaConfig.ResetAt) {
			quotaConfig.Usage = 0
			quotaConfig.Remaining = limitConfig.Total
			quotaConfig.ResetAt = getNextPeriodStart(period, now)
		} else {
			quotaConfig.Remaining = limitConfig.Total - quotaConfig.Usage
		}
	}
}

// reloadConfig 重新加载配置文件
func (cs *ConfigStore) reloadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var config Config
	if err := sonic.Unmarshal(data, &config); err != nil {
		return err
	}

	var rawConfig map[string]json.RawMessage
	if err := sonic.Unmarshal(data, &rawConfig); err != nil {
		return err
	}

	config.ClientRegistry = make(map[string]*AppConfig)
	for key, value := range rawConfig {
		if key == "host" || key == "port" {
			continue
		}
		var appConfig AppConfig
		if err := sonic.Unmarshal(value, &appConfig); err == nil {
			appConfig.AppName = key
			config.ClientRegistry[key] = &appConfig
		}
	}

	cs.mu.Lock()
	cs.config = &config

	now := time.Now()
	for appName := range cs.quotas {
		if _, exists := config.ClientRegistry[appName]; !exists {
			delete(cs.quotas, appName)
		}
	}

	for appName, appConfig := range config.ClientRegistry {
		if _, exists := cs.quotas[appName]; !exists {
			cs.quotas[appName] = &QuotaPeriods{}
		}
		cs.reconcileQuota(cs.quotas[appName], appConfig.RateLimits, now)
	}
	cs.mu.Unlock()

	logger.Info("Config changed, saving usage immediately...")
	cs.saveQuotas()

	return nil
}

func (cs *ConfigStore) GetAppRegistry(appName string) (*AppConfig, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if cs.config == nil || cs.config.ClientRegistry == nil {
		return nil, false
	}
	registry, exists := cs.config.ClientRegistry[appName]
	return registry, exists
}

// watch 监听文件变更
func (cs *ConfigStore) watch() {
	for {
		select {
		case event, ok := <-cs.watcher.Events:
			if !ok {
				return
			}
			logger.Info(fmt.Sprintf("FS event: %v, path: %s", event.Op, event.Name))
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) != 0 {
				time.Sleep(100 * time.Millisecond)
				basePath := cs.basePath
				configPath := filepath.Join(basePath, "config.json")
				usagePath := filepath.Join(basePath, "auto_usage.json")

				logger.Info(fmt.Sprintf("BasePath: %s, configPath: %s, usagePath: %s", basePath, configPath, usagePath))

				if filepath.Base(event.Name) == "config.json" {
					logger.Info("Calling reloadConfig for config.json")
					if err := cs.reloadConfig(configPath); err == nil {
						logger.Info("Reloaded config.json")
					} else {
						logger.Error("Failed to reload config.json: " + err.Error())
					}
				} else if filepath.Base(event.Name) == "auto_usage.json" {
					logger.Info("Calling reloadQuotas for auto_usage.json")
					if err := cs.reloadQuotas(usagePath); err == nil {
						logger.Info("Reloaded auto_usage.json from external modification")
					} else {
						logger.Error("Failed to reload auto_usage.json: " + err.Error())
					}
				} else {
					logger.Info(fmt.Sprintf("Ignoring event for: %s", filepath.Base(event.Name)))
				}
			}
		case err, ok := <-cs.watcher.Errors:
			if !ok {
				return
			}
			logger.Error("Config watcher error: " + err.Error())
		case <-cs.stop:
			return
		}
	}
}

// autoSave 包含防抖机制的自动保存协程
func (cs *ConfigStore) autoSave() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cs.saveQuotas()
		case <-cs.saveChan:
			time.Sleep(100 * time.Millisecond)
		drainChan:
			for {
				select {
				case <-cs.saveChan:
				default:
					break drainChan
				}
			}
			cs.saveQuotas()
		case <-cs.stop:
			cs.saveQuotas()
			return
		}
	}
}

// saveQuotas 保存 quotas 到文件
func (cs *ConfigStore) saveQuotas() {
	cs.mu.RLock()
	keys := make([]string, 0, len(cs.quotas))
	for k := range cs.quotas {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	periodOrder := []string{"hourly", "daily", "weekly", "monthly"}

	var buf strings.Builder
	buf.WriteString("{\n")
	for i, app := range keys {
		if i > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(fmt.Sprintf(`  "%s": {`, app))
		periods := cs.quotas[app]
		first := true
		for _, p := range periodOrder {
			var cfg *QuotaConfig
			switch p {
			case "hourly":
				cfg = periods.Hourly
			case "daily":
				cfg = periods.Daily
			case "weekly":
				cfg = periods.Weekly
			case "monthly":
				cfg = periods.Monthly
			}
			if cfg == nil {
				continue
			}
			if first {
				buf.WriteString("\n")
			} else {
				buf.WriteString(",\n")
			}
			first = false
			if !cfg.ResetAt.IsZero() {
				buf.WriteString(fmt.Sprintf(`    "%s": {"usage": %d, "remain": %d, "reset_at": "%s"}`,
					p, cfg.Usage, cfg.Remaining, cfg.ResetAt.Format(time.RFC3339)))
			} else {
				buf.WriteString(fmt.Sprintf(`    "%s": {"usage": %d, "remain": %d}`,
					p, cfg.Usage, cfg.Remaining))
			}
		}
		if first {
			buf.WriteString("}")
		} else {
			buf.WriteString("\n  }")
		}
	}
	buf.WriteString("\n}")

	cs.mu.RUnlock()

	path := filepath.Join(cs.basePath, "auto_usage.json")
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, []byte(buf.String()), 0600); err != nil {
		logger.Error("Failed to write usage temp file: " + err.Error())
		return
	}

	if err := os.Rename(tmpPath, path); err != nil {
		logger.Error("Failed to rename usage file: " + err.Error())
		return
	}

	cs.lastSaveMux.Lock()
	cs.lastSaveTime = time.Now()
	cs.lastSaveMux.Unlock()
}

// TriggerSave 触发保存
func (cs *ConfigStore) TriggerSave() {
	select {
	case cs.saveChan <- struct{}{}:
	default:
	}
}

// GetQuota 获取配额配置
func (cs *ConfigStore) GetQuota(name string) *QuotaPeriods {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.quotas[name]
}

// CheckAndInc 动态计算 remaining 并增加 usage
func (cs *ConfigStore) CheckAndInc(appName string) (allowed bool, details map[string]*PeriodDetail) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	appConfig, exists := cs.config.ClientRegistry[appName]
	if !exists || appConfig.RateLimits == nil {
		return true, nil
	}

	quota := cs.quotas[appName]
	if quota == nil {
		quota = &QuotaPeriods{}
		cs.quotas[appName] = quota
		cs.reconcileQuota(quota, appConfig.RateLimits, time.Now())
	}

	now := time.Now()
	details = make(map[string]*PeriodDetail)
	allowed = true
	var blockedBy string

	periods := []struct {
		name     string
		limit    *RateLimitConfig
		state    **QuotaConfig
	}{
		{"hourly", appConfig.RateLimits["hourly"], &quota.Hourly},
		{"daily", appConfig.RateLimits["daily"], &quota.Daily},
		{"weekly", appConfig.RateLimits["weekly"], &quota.Weekly},
		{"monthly", appConfig.RateLimits["monthly"], &quota.Monthly},
	}

	for _, p := range periods {
		if p.limit == nil || p.limit.Total <= 0 || *p.state == nil {
			continue
		}
		qState := *p.state

		if now.After(qState.ResetAt) {
			qState.Usage = 0
			qState.Remaining = p.limit.Total
			qState.ResetAt = getNextPeriodStart(p.name, now)
		}

		remaining := p.limit.Total - qState.Usage
		if remaining < 0 {
			remaining = 0
		}

		details[p.name] = &PeriodDetail{
			Total:     p.limit.Total,
			Remaining: remaining,
			ResetAt:   qState.ResetAt,
		}

		if qState.Usage >= p.limit.Total && allowed {
			allowed = false
			blockedBy = p.name
		}
	}

	if !allowed {
		details["blocked_by"] = &PeriodDetail{BlockedBy: blockedBy}
		return false, details
	}

	for _, p := range periods {
		if p.limit != nil && p.limit.Total > 0 && *p.state != nil {
			(*p.state).Usage++
			(*p.state).Remaining--

			if detail, exists := details[p.name]; exists && detail.Remaining > 0 {
				detail.Remaining--
			}
		}
	}

	cs.TriggerSave()

	return true, details
}

// Stop 停止配置监听
func (cs *ConfigStore) Stop() {
	close(cs.stop)
	cs.watcher.Close()
}

// getNextPeriodStart 获取下一个周期开始时间
func getNextPeriodStart(period string, now time.Time) time.Time {
	switch period {
	case "hourly":
		return now.Truncate(time.Hour).Add(time.Hour)
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	case "weekly":
		daysUntilNext := int(7 - now.Weekday())
		if daysUntilNext <= 0 {
			daysUntilNext = 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()+daysUntilNext, 0, 0, 0, 0, now.Location())
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	default:
		return now.Add(time.Hour)
	}
}

// reloadQuotas 专门用于处理外部对 auto_usage.json 的修改
func (cs *ConfigStore) reloadQuotas(path string) error {
	cs.lastSaveMux.Lock()
	recentSave := time.Since(cs.lastSaveTime) < 500*time.Millisecond
	cs.lastSaveMux.Unlock()

	if recentSave {
		logger.Info("Skipping reloadQuotas - file was recently saved by us")
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var externalQuotas map[string]*QuotaPeriods
	if err := sonic.Unmarshal(data, &externalQuotas); err != nil {
		return err
	}

	cs.mu.Lock()
	now := time.Now()

	for appName, extQuota := range externalQuotas {
		target, exists := cs.quotas[appName]
		appConfig, configExists := cs.config.ClientRegistry[appName]

		if !exists || !configExists || appConfig.RateLimits == nil {
			continue
		}

		rateLimits := appConfig.RateLimits

		if extQuota.Hourly != nil && target.Hourly != nil && rateLimits["hourly"] != nil {
			target.Hourly.Usage = extQuota.Hourly.Usage
			if now.After(extQuota.Hourly.ResetAt) {
				target.Hourly.Remaining = rateLimits["hourly"].Total
			} else {
				target.Hourly.Remaining = rateLimits["hourly"].Total - extQuota.Hourly.Usage
			}
			target.Hourly.ResetAt = extQuota.Hourly.ResetAt
		}
		if extQuota.Daily != nil && target.Daily != nil && rateLimits["daily"] != nil {
			target.Daily.Usage = extQuota.Daily.Usage
			if now.After(extQuota.Daily.ResetAt) {
				target.Daily.Remaining = rateLimits["daily"].Total
			} else {
				target.Daily.Remaining = rateLimits["daily"].Total - extQuota.Daily.Usage
			}
			target.Daily.ResetAt = extQuota.Daily.ResetAt
		}
		if extQuota.Weekly != nil && target.Weekly != nil && rateLimits["weekly"] != nil {
			target.Weekly.Usage = extQuota.Weekly.Usage
			if now.After(extQuota.Weekly.ResetAt) {
				target.Weekly.Remaining = rateLimits["weekly"].Total
			} else {
				target.Weekly.Remaining = rateLimits["weekly"].Total - extQuota.Weekly.Usage
			}
			target.Weekly.ResetAt = extQuota.Weekly.ResetAt
		}
		if extQuota.Monthly != nil && target.Monthly != nil && rateLimits["monthly"] != nil {
			target.Monthly.Usage = extQuota.Monthly.Usage
			if now.After(extQuota.Monthly.ResetAt) {
				target.Monthly.Remaining = rateLimits["monthly"].Total
			} else {
				target.Monthly.Remaining = rateLimits["monthly"].Total - extQuota.Monthly.Usage
			}
			target.Monthly.ResetAt = extQuota.Monthly.ResetAt
		}
	}
	cs.mu.Unlock()

	cs.saveQuotas()
	logger.Info("Reloaded auto_usage.json from user edit, saved updated remain values")

	return nil
}
