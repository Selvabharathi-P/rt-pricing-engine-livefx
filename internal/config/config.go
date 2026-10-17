// Package config loads runtime configuration.
//
// Precedence (later wins): built-in defaults → YAML file → environment.
// The YAML file is found via CONFIG_FILE, else ./config.yaml, else
// /etc/pricing-engine/config.yaml. Secrets never live in the file: it names
// the environment variables that hold them (api_key_env, password_env, ...).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultSymbols is the subscription list from the assignment, in order.
var DefaultSymbols = []string{
	"AUDUSD", "AUDCAD", "AUDCHF", "AUDJPY", "AUDNZD",
	"AUDSGD", "CADCHF", "CADJPY", "CHFJPY", "CHFSGD",
	"EURAUD", "EURCAD", "EURCHF", "EURGBP", "EURJPY",
	"EURNZD", "EURUSD", "GBPAUD", "GBPCAD", "GBPCHF",
	"GBPJPY", "GBPNZD", "GBPUSD", "NZDCAD", "NZDCHF",
	"NZDJPY", "NZDUSD", "USDCAD", "USDCHF", "USDCNH",
	"USDJPY", "USDMXN", "USDNOK", "USDPLN", "USDSEK",
	"USDSGD", "USDTRY", "USDZAR", "EURTRY", "GBPTRY",
	"NOKJPY", "SEKJPY", "SGDJPY", "ZARJPY", "XAUUSD",
	"XAGUSD", "BTCUSD", "ETHUSD", "BNBUSD", "SOLUSD",
	"XRPUSD", "ADAUSD", "DOGUSD", "DOTUSD", "LTCUSD",
	"BCHUSD", "XLMUSD", "TRXUSD", "UNIUSD", "FILUSD",
	"AVXUSD",
}

// Duration accepts Go duration strings ("500ms", "30s", "2m") or a bare
// number, which means seconds (so `max_backoff: 30` is 30 seconds).
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a value like 30s or 500ms", n.Line)
	}
	if n.Tag == "!!int" || n.Tag == "!!float" {
		secs, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		d.Duration = time.Duration(secs * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: bad duration %q (use e.g. 30s, 500ms, 2m, or a number of seconds)", n.Line, n.Value)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func dur(d time.Duration) Duration { return Duration{d} }

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Redis       RedisConfig       `yaml:"redis"`
	Persistence PersistenceConfig `yaml:"persistence"`
	Accounts    AccountsConfig    `yaml:"accounts"`
	Logging     LoggingConfig     `yaml:"logging"`
	Sources     []SourceConfig    `yaml:"sources"`
	Simulator   SimulatorConfig   `yaml:"simulator"` // read by cmd/simulator; ignored by the engine

	File string `yaml:"-"` // file the config was loaded from ("" = defaults only)
}

type ServerConfig struct {
	Listen              string `yaml:"listen"`
	MaxClients          int    `yaml:"max_clients"`
	MaxFrameBytes       int    `yaml:"max_frame_bytes"`
	MaxConnsPerMerchant int    `yaml:"max_conns_per_merchant"`
	RequireAuth         bool   `yaml:"require_auth"`
	CookieSecure        bool   `yaml:"cookie_secure"`
}

type RedisConfig struct {
	Addr        string `yaml:"addr"`
	DB          int    `yaml:"db"`
	PasswordEnv string `yaml:"password_env"`
	Password    string `yaml:"-"` // resolved from PasswordEnv
}

type PersistenceConfig struct {
	FlushInterval Duration `yaml:"flush_interval"`
	Namespace     string   `yaml:"namespace"` // default: pe:<host of the first enabled source>:
}

type AccountsConfig struct {
	SecretsDir       string `yaml:"secrets_dir"`
	AdminEmail       string `yaml:"admin_email"`
	AdminPasswordEnv string `yaml:"admin_password_env"`
	AdminPassword    string `yaml:"-"` // resolved; empty -> generated at first start
}

type LoggingConfig struct {
	Level string `yaml:"level"` // debug | info | warn | error
}

// SourceConfig is one market-data source in the registry. Type selects the
// implementation (see feed.Types()); add new sources by adding entries.
type SourceConfig struct {
	Name         string      `yaml:"name"`
	Type         string      `yaml:"type"`
	Enabled      *bool       `yaml:"enabled"` // default true
	URL          string      `yaml:"url"`
	APIKeyEnv    string      `yaml:"api_key_env"`
	APISecretEnv string      `yaml:"api_secret_env"`
	Symbols      []string    `yaml:"symbols"`
	AuthTimeout  Duration    `yaml:"auth_timeout"`
	ReadTimeout  Duration    `yaml:"read_timeout"`
	PingInterval Duration    `yaml:"ping_interval"`
	Retry        RetryConfig `yaml:"retry"`

	APIKey    string `yaml:"-"` // resolved from APIKeyEnv
	APISecret string `yaml:"-"` // resolved from APISecretEnv
}

// RetryConfig controls reconnects: exponential backoff with full jitter
// between MinBackoff and MaxBackoff; a session that stayed up StableAfter
// resets it. "Session in use" waits SessionInUseWait plus up to
// SessionInUseJitter, because retrying a busy session fast keeps it locked.
type RetryConfig struct {
	MinBackoff         Duration `yaml:"min_backoff"`
	MaxBackoff         Duration `yaml:"max_backoff"`
	StableAfter        Duration `yaml:"stable_after"`
	SessionInUseWait   Duration `yaml:"session_in_use_wait"`
	SessionInUseJitter Duration `yaml:"session_in_use_jitter"`
}

func (s SourceConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// SimulatorConfig drives cmd/simulator, a stand-in for the LiveFXHub feed.
type SimulatorConfig struct {
	Listen string `yaml:"listen"`
	Path   string `yaml:"path"`
	// Mode "livefxhub" reproduces the real feed's protocol and messages;
	// "legacy" sends the assignment brief's format (arrays with buy/sell/
	// timestamp and bogus 24h fields), padded to large frames.
	Mode             string   `yaml:"mode"`
	APIKeyEnv        string   `yaml:"api_key_env"`    // credentials the simulator accepts
	APISecretEnv     string   `yaml:"api_secret_env"` //   (default demo-key / demo-secret)
	OneSessionPerKey bool     `yaml:"one_session_per_key"`
	TickInterval     Duration `yaml:"tick_interval"`
	UpdateRatio      float64  `yaml:"update_ratio"`   // share of symbols updated each tick (livefxhub mode)
	Volatility       float64  `yaml:"volatility"`     // per-tick relative std-dev of the random walk
	SilentSymbols    []string `yaml:"silent_symbols"` // accepted in subscribe but never streamed (like live)
	DuplicateRate    float64  `yaml:"duplicate_rate"`
	OutOfOrderRate   float64  `yaml:"out_of_order_rate"` // legacy mode only (live has no timestamps)
	PadBytes         int      `yaml:"pad_bytes"`         // legacy mode: pad frames to about this size
	DropEvery        Duration `yaml:"drop_every"`        // 0 = never; else close each session after this long

	APIKey    string `yaml:"-"`
	APISecret string `yaml:"-"`
}

// LiveSilentSymbols are the subscribed symbols the live feed never sent.
var LiveSilentSymbols = []string{"GBPCAD", "GBPCHF", "GBPJPY", "USDPLN", "DOGUSD", "DOTUSD", "LTCUSD", "BCHUSD", "XLMUSD", "UNIUSD", "FILUSD"}

// Defaults returns the built-in configuration (one LiveFXHub source).
func Defaults() Config {
	return Config{
		Server: ServerConfig{Listen: ":8080", MaxClients: 10000, MaxFrameBytes: 10*1024 - 256,
			MaxConnsPerMerchant: 5, RequireAuth: true},
		Redis:       RedisConfig{Addr: "localhost:6379", PasswordEnv: "REDIS_PASSWORD"},
		Persistence: PersistenceConfig{FlushInterval: dur(time.Second)},
		Accounts:    AccountsConfig{SecretsDir: ".secrets", AdminEmail: "admin@example.com", AdminPasswordEnv: "ADMIN_PASSWORD"},
		Logging:     LoggingConfig{Level: "info"},
		Sources: []SourceConfig{{
			Name: "livefxhub", Type: "livefxhub", URL: "wss://v3-prod.livefxhub.com/ws/algo",
			APIKeyEnv: "API_KEY", APISecretEnv: "API_SECRET", Symbols: DefaultSymbols,
		}},
		Simulator: SimulatorConfig{
			Listen: ":9001", Path: "/ws/algo", Mode: "livefxhub", APIKeyEnv: "SIM_API_KEY", APISecretEnv: "SIM_API_SECRET",
			OneSessionPerKey: true, TickInterval: dur(100 * time.Millisecond), UpdateRatio: 0.2, Volatility: 0.00015,
			SilentSymbols: LiveSilentSymbols, DuplicateRate: 0.01, OutOfOrderRate: 0.02, PadBytes: 250000,
		},
	}
}

// LoadSimulator reads only what cmd/simulator needs (defaults → file →
// SIM_* env); the engine's own settings are not validated here.
func LoadSimulator() (SimulatorConfig, error) {
	c := Defaults()
	path, err := findFile()
	if err != nil {
		return c.Simulator, err
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c.Simulator, fmt.Errorf("config %s: %w", path, err)
		}
		if err := decode(raw, &c); err != nil {
			return c.Simulator, fmt.Errorf("config %s: %w", path, err)
		}
	}
	s := &c.Simulator
	if v := os.Getenv("SIM_LISTEN_ADDR"); v != "" {
		s.Listen = v
	}
	if v := os.Getenv("SIM_MODE"); v != "" {
		s.Mode = v
	}
	if v := os.Getenv("SIM_TICK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return *s, fmt.Errorf("SIM_TICK_INTERVAL: %w", err)
		}
		s.TickInterval.Duration = d
	}
	if v := os.Getenv("SIM_DROP_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return *s, fmt.Errorf("SIM_DROP_EVERY: %w", err)
		}
		s.DropEvery.Duration = d
	}
	s.APIKey, s.APISecret = os.Getenv(s.APIKeyEnv), os.Getenv(s.APISecretEnv)
	if s.APIKey == "" {
		s.APIKey = "demo-key"
	}
	if s.APISecret == "" {
		s.APISecret = "demo-secret"
	}
	for i, sym := range s.SilentSymbols {
		s.SilentSymbols[i] = strings.ToUpper(strings.TrimSpace(sym))
	}
	switch {
	case s.Mode != "livefxhub" && s.Mode != "legacy":
		return *s, fmt.Errorf("simulator.mode %q: use livefxhub or legacy", s.Mode)
	case s.TickInterval.Duration < time.Millisecond:
		return *s, errors.New("simulator.tick_interval must be at least 1ms")
	case s.UpdateRatio <= 0 || s.UpdateRatio > 1:
		return *s, errors.New("simulator.update_ratio must be in (0, 1]")
	case s.Volatility < 0 || s.Volatility > 0.05:
		return *s, errors.New("simulator.volatility must be between 0 and 0.05")
	}
	return *s, nil
}

// sourceDefaults fills unset per-source values.
func sourceDefaults(s *SourceConfig) {
	set := func(d *Duration, v time.Duration) {
		if d.Duration == 0 {
			d.Duration = v
		}
	}
	set(&s.AuthTimeout, 10*time.Second)
	set(&s.ReadTimeout, 30*time.Second)
	set(&s.PingInterval, 15*time.Second)
	set(&s.Retry.MinBackoff, 500*time.Millisecond)
	set(&s.Retry.MaxBackoff, 30*time.Second)
	set(&s.Retry.StableAfter, 30*time.Second)
	set(&s.Retry.SessionInUseWait, 30*time.Second)
	set(&s.Retry.SessionInUseJitter, 10*time.Second)
	if s.APIKeyEnv == "" {
		s.APIKeyEnv = "API_KEY"
	}
	if s.APISecretEnv == "" {
		s.APISecretEnv = "API_SECRET"
	}
	for i, sym := range s.Symbols {
		s.Symbols[i] = strings.ToUpper(strings.TrimSpace(sym))
	}
}

// Load builds the configuration from defaults, the YAML file and the
// environment, then validates it.
func Load() (Config, error) {
	c := Defaults()
	path, err := findFile()
	if err != nil {
		return c, err
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		if err := decode(raw, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		c.File = path
	}
	for i := range c.Sources {
		sourceDefaults(&c.Sources[i])
	}
	if err := applyEnv(&c); err != nil {
		return c, err
	}
	resolveSecrets(&c)
	if c.Persistence.Namespace == "" {
		if s := c.firstEnabled(); s != nil {
			c.Persistence.Namespace = "pe:" + hostOf(s.URL) + ":"
		}
	}
	return c, c.Validate()
}

// decode reads YAML strictly: unknown keys are errors, so typos surface.
func decode(raw []byte, c *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) { // empty file = no overrides
		return err
	}
	return nil
}

func findFile() (string, error) {
	if p := os.Getenv("CONFIG_FILE"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("CONFIG_FILE=%s: %w", p, err)
		}
		return p, nil
	}
	for _, p := range []string{"config.yaml", "/etc/pricing-engine/config.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// applyEnv keeps the original environment variables working (.env,
// docker-compose). Empty variables are ignored. UPSTREAM_URL, SYMBOLS,
// AUTH_TIMEOUT and READ_TIMEOUT apply to the first source.
func applyEnv(c *Config) error {
	str := func(k string, dst *string) {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	num := func(k string, dst *int) error {
		if v := os.Getenv(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			*dst = n
		}
		return nil
	}
	boolean := func(k string, dst *bool) {
		if v := os.Getenv(k); v != "" {
			*dst = v == "true" || v == "1"
		}
	}
	durEnv := func(k string, dst *Duration) error {
		if v := os.Getenv(k); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			dst.Duration = d
		}
		return nil
	}
	str("LISTEN_ADDR", &c.Server.Listen)
	for _, e := range []struct {
		k string
		p *int
	}{{"MAX_CLIENTS", &c.Server.MaxClients}, {"MAX_FRAME_BYTES", &c.Server.MaxFrameBytes},
		{"MAX_CONNS_PER_MERCHANT", &c.Server.MaxConnsPerMerchant}, {"REDIS_DB", &c.Redis.DB}} {
		if err := num(e.k, e.p); err != nil {
			return err
		}
	}
	boolean("WS_REQUIRE_AUTH", &c.Server.RequireAuth)
	boolean("COOKIE_SECURE", &c.Server.CookieSecure)
	str("REDIS_ADDR", &c.Redis.Addr)
	str("STORE_NAMESPACE", &c.Persistence.Namespace)
	if err := durEnv("FLUSH_INTERVAL", &c.Persistence.FlushInterval); err != nil {
		return err
	}
	str("SECRETS_DIR", &c.Accounts.SecretsDir)
	str("ADMIN_EMAIL", &c.Accounts.AdminEmail)
	str("LOG_LEVEL", &c.Logging.Level)
	if v := os.Getenv("SOURCES_ENABLED"); v != "" { // e.g. "simulator" or "livefxhub,backup"
		want := map[string]bool{}
		for _, n := range strings.Split(v, ",") {
			want[strings.TrimSpace(n)] = true
		}
		known := map[string]bool{}
		for i := range c.Sources {
			on := want[c.Sources[i].Name]
			c.Sources[i].Enabled = &on
			known[c.Sources[i].Name] = true
		}
		for n := range want {
			if !known[n] {
				return fmt.Errorf("SOURCES_ENABLED: no source named %q in config", n)
			}
		}
	}
	if len(c.Sources) > 0 {
		s := &c.Sources[0]
		str("UPSTREAM_URL", &s.URL)
		if v := os.Getenv("SYMBOLS"); v != "" {
			s.Symbols = nil
			for _, sym := range strings.Split(v, ",") {
				if sym = strings.ToUpper(strings.TrimSpace(sym)); sym != "" {
					s.Symbols = append(s.Symbols, sym)
				}
			}
		}
		if err := durEnv("AUTH_TIMEOUT", &s.AuthTimeout); err != nil {
			return err
		}
		if err := durEnv("READ_TIMEOUT", &s.ReadTimeout); err != nil {
			return err
		}
	}
	return nil
}

func resolveSecrets(c *Config) {
	if c.Redis.PasswordEnv != "" {
		c.Redis.Password = os.Getenv(c.Redis.PasswordEnv)
	}
	if c.Accounts.AdminPasswordEnv != "" {
		c.Accounts.AdminPassword = os.Getenv(c.Accounts.AdminPasswordEnv)
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		s.APIKey, s.APISecret = os.Getenv(s.APIKeyEnv), os.Getenv(s.APISecretEnv)
	}
}

// Validate reports the first configuration problem, naming the setting.
func (c Config) Validate() error {
	if c.Server.MaxFrameBytes < 512 || c.Server.MaxFrameBytes > 10*1024 {
		return errors.New("server.max_frame_bytes must be between 512 and 10240")
	}
	if c.Server.MaxClients < 1 || c.Server.MaxConnsPerMerchant < 1 {
		return errors.New("server.max_clients and server.max_conns_per_merchant must be >= 1")
	}
	if c.Persistence.FlushInterval.Duration < 100*time.Millisecond {
		return errors.New("persistence.flush_interval must be at least 100ms")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level %q: use debug, info, warn or error", c.Logging.Level)
	}
	names := map[string]bool{}
	enabled := 0
	for i, s := range c.Sources {
		where := fmt.Sprintf("sources[%d]", i)
		if s.Name != "" {
			where = fmt.Sprintf("source %q", s.Name)
		}
		switch {
		case s.Name == "":
			return fmt.Errorf("%s: name is required", where)
		case names[s.Name]:
			return fmt.Errorf("%s: duplicate name", where)
		case s.Type == "":
			return fmt.Errorf("%s: type is required", where)
		}
		names[s.Name] = true
		if !s.IsEnabled() {
			continue
		}
		enabled++
		switch {
		case s.URL == "":
			return fmt.Errorf("%s: url is required", where)
		case len(s.Symbols) == 0:
			return fmt.Errorf("%s: symbols is empty", where)
		case s.APIKey == "" || s.APISecret == "":
			return fmt.Errorf("%s: credentials missing: set environment variables %s and %s (see .env.example)", where, s.APIKeyEnv, s.APISecretEnv)
		case s.Retry.MinBackoff.Duration <= 0 || s.Retry.MaxBackoff.Duration < s.Retry.MinBackoff.Duration:
			return fmt.Errorf("%s: retry.min_backoff must be > 0 and <= retry.max_backoff", where)
		case s.AuthTimeout.Duration <= 0 || s.ReadTimeout.Duration <= 0 || s.PingInterval.Duration <= 0:
			return fmt.Errorf("%s: auth_timeout, read_timeout and ping_interval must be > 0", where)
		case s.PingInterval.Duration >= s.ReadTimeout.Duration:
			return fmt.Errorf("%s: ping_interval must be shorter than read_timeout", where)
		}
	}
	if enabled == 0 {
		return errors.New("sources: at least one enabled source is required")
	}
	return nil
}

func (c Config) firstEnabled() *SourceConfig {
	for i := range c.Sources {
		if c.Sources[i].IsEnabled() {
			return &c.Sources[i]
		}
	}
	return nil
}

// EnabledSources returns the sources to run, in file order.
func (c Config) EnabledSources() []SourceConfig {
	var out []SourceConfig
	for _, s := range c.Sources {
		if s.IsEnabled() {
			out = append(out, s)
		}
	}
	return out
}

// AllSymbols is the union of every enabled source's symbols, in first-seen order.
func (c Config) AllSymbols() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range c.EnabledSources() {
		for _, sym := range s.Symbols {
			if !seen[sym] {
				seen[sym] = true
				out = append(out, sym)
			}
		}
	}
	return out
}

// String never prints secrets, so the config is safe to log.
func (c Config) String() string {
	var src []string
	for _, s := range c.EnabledSources() {
		src = append(src, fmt.Sprintf("%s(%s %s, %d symbols, retry %s-%s)", s.Name, s.Type, s.URL, len(s.Symbols),
			s.Retry.MinBackoff, s.Retry.MaxBackoff))
	}
	file := c.File
	if file == "" {
		file = "<defaults>"
	}
	return fmt.Sprintf("file=%s sources=[%s] listen=%s redis=%s ns=%s flush=%s", file, strings.Join(src, "; "),
		c.Server.Listen, c.Redis.Addr, c.Persistence.Namespace, c.Persistence.FlushInterval)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "default"
}
