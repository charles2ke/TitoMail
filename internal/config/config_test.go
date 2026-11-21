package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadEnvironment(t *testing.T) {
	t.Setenv("TITOMAIL_RELAY_PORT", "2525")
	t.Setenv("TITOMAIL_QUEUE_MAX_AGE", "48h")
	t.Setenv("TITOMAIL_TRUST_PROXY", "true")
	t.Setenv("TITOMAIL_LOG_LEVEL", "debug")
	cfg, err := Load([]string{"-log-level", "warn"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelayPort != 2525 || cfg.QueueMaxAge != 48*time.Hour || !cfg.TrustProxy {
		t.Errorf("environment not applied: relay port %d, queue max age %v, trust proxy %v",
			cfg.RelayPort, cfg.QueueMaxAge, cfg.TrustProxy)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("log level %q: the flag should take precedence over the environment", cfg.LogLevel)
	}
}

func TestLoadInvalidEnvironment(t *testing.T) {
	invalid := map[string]string{
		"TITOMAIL_ACME":              "yes",
		"TITOMAIL_RELAY_PORT":        "587/tcp",
		"TITOMAIL_MAX_MESSAGE_BYTES": "50MB",
		"TITOMAIL_QUEUE_MAX_AGE":     "5d",
	}
	for env, v := range invalid {
		t.Setenv(env, v)
	}
	_, err := Load(nil)
	if err == nil {
		t.Fatal("Load accepted invalid environment variables")
	}
	for env := range invalid {
		if !strings.Contains(err.Error(), env) {
			t.Errorf("the error does not mention %s: %v", env, err)
		}
	}
}
