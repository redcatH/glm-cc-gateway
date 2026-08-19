package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateGeneratesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "config.json")
	cfg, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if cfg.Listen != "0.0.0.0:8080" || cfg.UpstreamBaseURL == "" || cfg.Behavior.SessionPoolSize == 0 {
		t.Fatalf("default config incomplete: %+v", cfg)
	}
	// 文件已写盘,且再次加载得到相同值。
	if _, err := os.Stat(path); err != nil {
		t.Fatal("default config file not written")
	}
	cfg2, created2, err := LoadOrCreate(path)
	if err != nil || created2 {
		t.Fatalf("second load should not create, created=%v err=%v", created2, err)
	}
	if cfg2.Listen != cfg.Listen || cfg2.UpstreamBaseURL != cfg.UpstreamBaseURL {
		t.Fatal("roundtrip mismatch")
	}
}

func TestDefaultConfigCoversAllFields(t *testing.T) {
	c := DefaultConfig()
	if c.CountTokensPath == "" || c.IdentityFile == "" || c.MaxBodyBytes <= 0 ||
		c.Behavior.QueueTimeoutSeconds <= 0 || c.Behavior.SessionRotateMaxMinutes <= c.Behavior.SessionRotateMinMinutes {
		t.Fatalf("default config field missing: %+v", c)
	}
}
