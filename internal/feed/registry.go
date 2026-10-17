package feed

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"pricing-engine/internal/metrics"
	"pricing-engine/internal/model"
)

// Source is a market-data source the engine runs. Every source writes
// normalised ticks to Deps.Out and reports its connection state through
// Deps.OnStatus.
type Source interface {
	Name() string
	Symbols() []string
	Run(ctx context.Context) // until ctx is cancelled, reconnecting as needed
}

// Deps are what the service gives every source.
type Deps struct {
	Out      chan<- []model.Tick
	Log      *slog.Logger
	Metrics  *metrics.Set
	OnStatus func(up bool, reason string) // StatusConnected, StatusSessionInUse, ...
}

// Factory builds a source of one type from its configuration.
type Factory func(cfg Config, d Deps) (Source, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a source type. Call it from an init function in the file
// that implements the type; the name is what `type:` means in config.yaml.
func Register(typ string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[typ]; dup {
		panic("feed: source type registered twice: " + typ)
	}
	registry[typ] = f
}

// New builds a source of the given registered type.
func New(typ string, cfg Config, d Deps) (Source, error) {
	regMu.RLock()
	f, ok := registry[typ]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown source type %q (registered: %s)", typ, strings.Join(Types(), ", "))
	}
	return f(cfg, d)
}

// Types lists the registered source types.
func Types() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// The LiveFXHub WebSocket protocol (auth -> subscribeMarketPrice -> prices).
// The bundled simulator speaks the same protocol, so it uses this type too.
func init() {
	Register("livefxhub", func(cfg Config, d Deps) (Source, error) {
		if cfg.URL == "" {
			return nil, fmt.Errorf("source %q: url is required", cfg.Name)
		}
		c := NewClient(cfg, d.Out, d.Log.With("source", cfg.Name), d.Metrics)
		c.OnStatus = d.OnStatus
		return c, nil
	})
}
