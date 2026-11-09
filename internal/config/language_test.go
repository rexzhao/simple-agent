package config_test

import (
	"testing"

	"simple-agent/internal/config"
)

// 界面语言：环境变量 > 配置文件 > auto。
func TestLanguageEnvOverride(t *testing.T) {
	t.Setenv("SIMPLE_AGENT_LANG", "en")

	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Load 内部会 applyEnv：没有配置文件时，环境变量应生效
	if cfg.Language != "en" {
		t.Fatalf("SIMPLE_AGENT_LANG 应覆盖配置：%q", cfg.Language)
	}
}
