// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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

type Config struct {
	UpstreamURL string
	APIKey      string
	APISecret   string
	Symbols     []string

	ListenAddr    string
	MaxClients    int
	MaxFrameBytes int // hard cap on every outgoing WebSocket frame

	RedisAddr     string
	RedisPassword string
	RedisDB       int
	StoreNS       string // Redis key namespace, one per upstream source
	FlushInterval time.Duration

	AuthTimeout time.Duration
	ReadTimeout time.Duration // reconnect if upstream is silent this long
	LogLevel    string

	// Merchant accounts / downstream access control
	SecretsDir          string // key material (signing, KYC encryption, API-key pepper)
	AdminEmail          string
	AdminPassword       string // bootstrap admin; generated and printed once if empty
	CookieSecure        bool   // set Secure on cookies (enable behind HTTPS)
	WSRequireAuth       bool   // downstream clients must authenticate
	MaxConnsPerMerchant int
}

// String never prints secrets, so the config is safe to log.
func (c Config) String() string {
	return fmt.Sprintf("upstream=%s symbols=%d listen=%s redis=%s ns=%s flush=%s api_key=%s",
		c.UpstreamURL, len(c.Symbols), c.ListenAddr, c.RedisAddr, c.StoreNS, c.FlushInterval, mask(c.APIKey))
}

func mask(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

func Load() (Config, error) {
	c := Config{
		UpstreamURL:   env("UPSTREAM_URL", "wss://v3-prod.livefxhub.com/ws/algo"),
		APIKey:        os.Getenv("API_KEY"),
		APISecret:     os.Getenv("API_SECRET"),
		Symbols:       DefaultSymbols,
		ListenAddr:    env("LISTEN_ADDR", ":8080"),
		RedisAddr:     env("REDIS_ADDR", "localhost:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		LogLevel:      env("LOG_LEVEL", "info"),
		SecretsDir:    env("SECRETS_DIR", ".secrets"),
		AdminEmail:    env("ADMIN_EMAIL", "admin@example.com"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		CookieSecure:  os.Getenv("COOKIE_SECURE") == "true",
		WSRequireAuth: os.Getenv("WS_REQUIRE_AUTH") != "false",
	}
	if s := os.Getenv("SYMBOLS"); s != "" {
		c.Symbols = nil
		for _, sym := range strings.Split(s, ",") {
			if sym = strings.ToUpper(strings.TrimSpace(sym)); sym != "" {
				c.Symbols = append(c.Symbols, sym)
			}
		}
	}
	var err error
	if c.MaxConnsPerMerchant, err = envInt("MAX_CONNS_PER_MERCHANT", 5); err != nil {
		return c, err
	}
	if c.MaxClients, err = envInt("MAX_CLIENTS", 10000); err != nil {
		return c, err
	}
	if c.MaxFrameBytes, err = envInt("MAX_FRAME_BYTES", 10*1024-256); err != nil {
		return c, err
	}
	if c.RedisDB, err = envInt("REDIS_DB", 0); err != nil {
		return c, err
	}
	if c.FlushInterval, err = envDur("FLUSH_INTERVAL", time.Second); err != nil {
		return c, err
	}
	if c.AuthTimeout, err = envDur("AUTH_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.ReadTimeout, err = envDur("READ_TIMEOUT", 30*time.Second); err != nil {
		return c, err
	}
	c.StoreNS = os.Getenv("STORE_NAMESPACE")
	if c.StoreNS == "" {
		c.StoreNS = "pe:" + upstreamID(c.UpstreamURL) + ":"
	}
	if c.APIKey == "" || c.APISecret == "" {
		return c, errors.New("API_KEY and API_SECRET must be set (see .env.example)")
	}
	if len(c.Symbols) == 0 {
		return c, errors.New("SYMBOLS is empty")
	}
	if c.MaxFrameBytes < 512 || c.MaxFrameBytes > 10*1024 {
		return c, errors.New("MAX_FRAME_BYTES must be between 512 and 10240")
	}
	return c, nil
}

// upstreamID is the host[:port] of the upstream URL, used to namespace state.
func upstreamID(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "default"
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envDur(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}
