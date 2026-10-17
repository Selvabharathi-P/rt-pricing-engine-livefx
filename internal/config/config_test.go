package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate clears every env var Load reads and points CONFIG_FILE at path.
func isolate(t *testing.T, yamlText string) {
	t.Helper()
	for _, k := range []string{"CONFIG_FILE", "LISTEN_ADDR", "MAX_CLIENTS", "MAX_FRAME_BYTES", "MAX_CONNS_PER_MERCHANT",
		"REDIS_DB", "WS_REQUIRE_AUTH", "COOKIE_SECURE", "REDIS_ADDR", "STORE_NAMESPACE", "FLUSH_INTERVAL", "SECRETS_DIR",
		"ADMIN_EMAIL", "LOG_LEVEL", "UPSTREAM_URL", "SYMBOLS", "AUTH_TIMEOUT", "READ_TIMEOUT", "API_KEY", "API_SECRET",
		"REDIS_PASSWORD", "ADMIN_PASSWORD", "B_KEY", "B_SECRET", "SOURCES_ENABLED", "SIM_API_KEY", "SIM_API_SECRET",
		"SIM_MODE", "SIM_LISTEN_ADDR", "SIM_TICK_INTERVAL", "SIM_DROP_EVERY"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir()) // no ./config.yaml unless written below
	if yamlText != "" {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(yamlText), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", p)
	}
	t.Setenv("API_KEY", "k")
	t.Setenv("API_SECRET", "s")
}

func TestDefaultsOnly(t *testing.T) {
	isolate(t, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := c.EnabledSources()
	if len(s) != 1 || s[0].Type != "livefxhub" || len(s[0].Symbols) != 61 || s[0].APIKey != "k" {
		t.Fatalf("default source: %+v", s)
	}
	if s[0].Retry.MaxBackoff.Duration != 30*time.Second || s[0].Retry.SessionInUseWait.Duration != 30*time.Second {
		t.Fatalf("default retry: %+v", s[0].Retry)
	}
	if c.Persistence.Namespace != "pe:v3-prod.livefxhub.com:" {
		t.Fatalf("namespace %q", c.Persistence.Namespace)
	}
}

func TestYAMLOverridesAndSecondsShorthand(t *testing.T) {
	isolate(t, `
server: {listen: ":9999", max_conns_per_merchant: 2}
logging: {level: debug}
sources:
  - name: primary
    type: livefxhub
    url: wss://a.example/ws
    symbols: [eurusd, GBPUSD]
    retry: {min_backoff: 250ms, max_backoff: 45, session_in_use_wait: 1.5}
  - name: backup
    type: livefxhub
    url: wss://b.example/ws
    api_key_env: B_KEY
    api_secret_env: B_SECRET
    symbols: [GBPUSD, XAUUSD]
`)
	t.Setenv("B_KEY", "bk")
	t.Setenv("B_SECRET", "bs")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":9999" || c.Server.MaxConnsPerMerchant != 2 || c.Server.MaxClients != 10000 {
		t.Fatalf("server merge: %+v", c.Server)
	}
	p := c.Sources[0]
	if p.Retry.MinBackoff.Duration != 250*time.Millisecond || p.Retry.MaxBackoff.Duration != 45*time.Second ||
		p.Retry.SessionInUseWait.Duration != 1500*time.Millisecond || p.Retry.StableAfter.Duration != 30*time.Second {
		t.Fatalf("retry: %+v", p.Retry)
	}
	if p.Symbols[0] != "EURUSD" {
		t.Fatalf("symbols not normalised: %v", p.Symbols)
	}
	if c.Sources[1].APIKey != "bk" {
		t.Fatalf("second source creds: %+v", c.Sources[1])
	}
	if got := strings.Join(c.AllSymbols(), ","); got != "EURUSD,GBPUSD,XAUUSD" {
		t.Fatalf("symbol union %s", got)
	}
	if c.Persistence.Namespace != "pe:a.example:" {
		t.Fatalf("namespace from first source: %q", c.Persistence.Namespace)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	isolate(t, "sources:\n  - {name: x, type: livefxhub, url: wss://file.example/ws, symbols: [EURUSD]}\nlogging: {level: warn}\n")
	t.Setenv("UPSTREAM_URL", "ws://env.example/ws")
	t.Setenv("SYMBOLS", "btcusd, ethusd")
	t.Setenv("LOG_LEVEL", "debug")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sources[0].URL != "ws://env.example/ws" || strings.Join(c.Sources[0].Symbols, ",") != "BTCUSD,ETHUSD" || c.Logging.Level != "debug" {
		t.Fatalf("env did not override: %+v %s", c.Sources[0], c.Logging.Level)
	}
}

func TestDisabledSourceSkipped(t *testing.T) {
	isolate(t, `
sources:
  - {name: on, type: livefxhub, url: wss://a/ws, symbols: [EURUSD]}
  - {name: off, type: livefxhub, enabled: false, url: "", symbols: []}
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EnabledSources()) != 1 || c.EnabledSources()[0].Name != "on" {
		t.Fatalf("enabled: %+v", c.EnabledSources())
	}
}

func TestValidationErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":       {"server: {lisen: \":1\"}\n", "field lisen not found"},
		"bad duration":      {"persistence: {flush_interval: soon}\n", "bad duration"},
		"bad level":         {"logging: {level: loud}\n", "logging.level"},
		"frame too big":     {"server: {max_frame_bytes: 20000}\n", "max_frame_bytes"},
		"no enabled source": {"sources:\n  - {name: a, type: livefxhub, enabled: false}\n", "at least one enabled source"},
		"duplicate names":   {"sources:\n  - {name: a, type: livefxhub, url: u, symbols: [X]}\n  - {name: a, type: livefxhub, url: u, symbols: [X]}\n", "duplicate name"},
		"missing type":      {"sources:\n  - {name: a, url: u, symbols: [X]}\n", "type is required"},
		"min above max":     {"sources:\n  - {name: a, type: livefxhub, url: u, symbols: [X], retry: {min_backoff: 10s, max_backoff: 1s}}\n", "min_backoff"},
		"ping >= read":      {"sources:\n  - {name: a, type: livefxhub, url: u, symbols: [X], ping_interval: 30s, read_timeout: 30s}\n", "ping_interval"},
		"missing creds env": {"sources:\n  - {name: a, type: livefxhub, url: u, symbols: [X], api_key_env: NOPE_KEY, api_secret_env: NOPE_SECRET}\n", "NOPE_KEY and NOPE_SECRET"},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t, tc.yaml)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMissingConfigFile(t *testing.T) {
	isolate(t, "")
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "nope.yaml"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONFIG_FILE") {
		t.Fatalf("want CONFIG_FILE error, got %v", err)
	}
}

// The config.yaml shipped in the repo must load and match the defaults.
func TestRepoConfigFile(t *testing.T) {
	repoCfg, err := filepath.Abs("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	isolate(t, "")
	t.Setenv("CONFIG_FILE", repoCfg)
	c, err := Load()
	if err != nil {
		t.Fatalf("repo config.yaml: %v", err)
	}
	if got := strings.Join(c.AllSymbols(), ","); got != strings.Join(DefaultSymbols, ",") {
		t.Fatalf("repo config symbols differ from the assignment list:\n%s", got)
	}
	if !strings.Contains(c.String(), "livefxhub") {
		t.Fatalf("String(): %s", c.String())
	}
}

// The logged config summary must never contain credential values.
func TestStringHidesSecrets(t *testing.T) {
	isolate(t, "")
	t.Setenv("API_KEY", "pk_SUPER_SECRET_KEY")
	t.Setenv("API_SECRET", "sk_SUPER_SECRET_VALUE")
	t.Setenv("REDIS_PASSWORD", "redis_SUPER_SECRET")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s := c.String(); strings.Contains(s, "SUPER_SECRET") {
		t.Fatalf("secret leaked into String(): %s", s)
	}
}

func TestSourcesEnabledEnv(t *testing.T) {
	repoCfg, _ := filepath.Abs("../../config.yaml")
	isolate(t, "")
	t.Setenv("CONFIG_FILE", repoCfg)
	t.Setenv("SOURCES_ENABLED", "simulator")
	t.Setenv("SIM_API_KEY", "sim-k")
	t.Setenv("SIM_API_SECRET", "sim-s")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := c.EnabledSources()
	if len(s) != 1 || s[0].Name != "simulator" || s[0].APIKey != "sim-k" || c.Persistence.Namespace != "pe:simulator:9001:" {
		t.Fatalf("enabled: %+v", s)
	}
	t.Setenv("SOURCES_ENABLED", "nope")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `no source named "nope"`) {
		t.Fatalf("want unknown-source error, got %v", err)
	}
}

func TestLoadSimulator(t *testing.T) {
	repoCfg, _ := filepath.Abs("../../config.yaml")
	isolate(t, "")
	t.Setenv("CONFIG_FILE", repoCfg)
	t.Setenv("SIM_API_KEY", "")
	s, err := LoadSimulator()
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != "livefxhub" || s.APIKey != "demo-key" || len(s.SilentSymbols) != 11 || s.TickInterval.Duration != 100*time.Millisecond {
		t.Fatalf("simulator config: %+v", s)
	}
	t.Setenv("SIM_MODE", "legacy")
	t.Setenv("SIM_API_KEY", "custom")
	if s, _ = LoadSimulator(); s.Mode != "legacy" || s.APIKey != "custom" {
		t.Fatalf("env override: %+v", s)
	}
	t.Setenv("SIM_MODE", "weird")
	if _, err := LoadSimulator(); err == nil {
		t.Fatal("bad mode accepted")
	}
}
