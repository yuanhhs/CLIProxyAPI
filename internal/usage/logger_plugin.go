package usage

import (
	"context"
	"sync"
	"sync/atomic"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

var statisticsEnabled atomic.Bool
var persistentLogger = &LoggerPlugin{}

func init() {
	statisticsEnabled.Store(true)
	coreusage.RegisterPlugin(persistentLogger)
}

// LoggerPlugin persists the official usage.Record stream in SQL.
type LoggerPlugin struct {
	mu    sync.RWMutex
	store *Store
}

func (p *LoggerPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !statisticsEnabled.Load() {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.store == nil {
		return
	}
	if err := p.store.Record(ctx, record); err != nil {
		log.WithError(err).Warn("failed to persist usage record")
	}
}

func SetStore(store *Store) {
	persistentLogger.mu.Lock()
	persistentLogger.store = store
	persistentLogger.mu.Unlock()
}

func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

func StatisticsEnabled() bool { return statisticsEnabled.Load() }
