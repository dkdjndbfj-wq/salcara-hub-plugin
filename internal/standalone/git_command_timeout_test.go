package standalone

import (
	"testing"
	"time"
)

func TestCommandTimeoutEnvironmentPreservesDefaultAndExplicitOverrides(t *testing.T) {
	dir := t.TempDir()
	for _, setting := range []string{"", "1s", "45s", "120s"} {
		cfg, err := ConfigFromEnv(func(key string) string {
			switch key {
			case "SALCARA_HUB_DATA_DIR":
				return dir
			case "SALCARA_HUB_COMMAND_TIMEOUT":
				return setting
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		want := time.Duration(0)
		if setting != "" {
			want, _ = time.ParseDuration(setting)
		}
		if got := cfg.hubCommandTimeout(); got != want {
			t.Errorf("environment=%q forwarded=%v want=%v", setting, got, want)
		}
		// Programmatic changes after loading still override the default.
		cfg.CommandTimeout = 2 * time.Second
		if got := cfg.hubCommandTimeout(); got != 2*time.Second {
			t.Errorf("programmatic override was lost: %v", got)
		}
	}
	if got := (Config{CommandTimeout: 45 * time.Second}).hubCommandTimeout(); got != 45*time.Second {
		t.Errorf("programmatic explicit default was lost: %v", got)
	}
}
