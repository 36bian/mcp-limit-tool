package main

type RateLimitConfig struct {
	Total    int64  `json:"total"`
	Duration string `json:"duration,omitempty"`
}

type AppConfig struct {
	AppName      string                     `json:"app_name"`
	Command      string                     `json:"command"`
	Args         []string                   `json:"args,omitempty"`
	Env          map[string]string          `json:"env,omitempty"`
	AllowedTools []string                   `json:"allowed_tools,omitempty"`
	RateLimits   map[string]*RateLimitConfig `json:"rate_limits,omitempty"`
}

type Config struct {
	Host           string               `json:"host"`
	Port           int                  `json:"port"`
	ClientRegistry map[string]*AppConfig `json:"-"`
}
