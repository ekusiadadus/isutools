package isutools

import (
	"log"
	"os"
	"strings"
	"sync"
)

const envGlobalMode = "ISUTOOLS"

// GlobalConfig is the immutable process-wide enablement decision.
type GlobalConfig struct {
	Off     bool
	Code    string
	Message string
}

func resolveGlobalConfig(getenv func(string) string) GlobalConfig {
	if getenv == nil {
		return GlobalConfig{Code: "enabled", Message: "measurement enabled"}
	}
	switch strings.ToLower(strings.TrimSpace(getenv(envGlobalMode))) {
	case "off", "0", "false", "no", "disabled":
		return GlobalConfig{Off: true, Code: "hard-off", Message: "measurement hard-disabled at startup"}
	case "", "on", "1", "true", "yes", "enabled":
		return GlobalConfig{Code: "enabled", Message: "measurement enabled at startup"}
	default:
		return GlobalConfig{Code: "unknown-value", Message: "unknown ISUTOOLS value; measurement enabled fail-open"}
	}
}

type processConfig struct {
	once   sync.Once
	getenv func(string) string
	value  GlobalConfig
}

func newProcessConfig(getenv func(string) string) *processConfig {
	return &processConfig{getenv: getenv}
}

func (p *processConfig) Resolve() GlobalConfig {
	p.once.Do(func() {
		p.value = resolveGlobalConfig(p.getenv)
		logResolvedProcessConfig(p.value)
	})
	return p.value
}

var (
	processConfigMu   sync.RWMutex
	globalProcessMode = newProcessConfig(os.Getenv)
)

func resolvedProcessConfig() GlobalConfig {
	processConfigMu.RLock()
	state := globalProcessMode
	processConfigMu.RUnlock()
	return state.Resolve()
}

func replaceProcessConfigForTest(getenv func(string) string) func() {
	processConfigMu.Lock()
	old := globalProcessMode
	globalProcessMode = newProcessConfig(getenv)
	processConfigMu.Unlock()
	return func() {
		processConfigMu.Lock()
		globalProcessMode = old
		processConfigMu.Unlock()
	}
}

func logResolvedProcessConfig(cfg GlobalConfig) {
	status := "on"
	if cfg.Off {
		status = "off"
	}
	log.Printf("isutools: mode=%s reason=%s", status, cfg.Code)
}
