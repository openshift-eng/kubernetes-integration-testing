package bundle

import (
	"context"
	"time"
)

type GCConfig struct {
	Interval  time.Duration
	Retention time.Duration
}

func DefaultGCConfig() GCConfig {
	return GCConfig{
		Interval:  5 * time.Minute,
		Retention: 30 * time.Minute,
	}
}

type ClusterSnapshot struct {
	ActiveAgents     map[string]struct{}
	ProtectedDigests map[string]struct{}
}

type ProtectedState func(ctx context.Context) (ClusterSnapshot, error)

// GCRunner holds state for the bundle garbage collector across ticks.
// Not safe for concurrent use.
type GCRunner struct {
	store     *BundleStore
	state     ProtectedState
	lastSeen  map[string]time.Time
	firstTick bool
}

// NewGCRunner creates a GC runner that uses state to determine which
// blobs and agent mappings are still live.
func NewGCRunner(store *BundleStore, state ProtectedState) *GCRunner {
	return &GCRunner{
		store:     store,
		state:     state,
		lastSeen:  make(map[string]time.Time),
		firstTick: true,
	}
}

// RunGC runs the garbage collector in a loop until ctx is cancelled.
func (s *BundleStore) RunGC(ctx context.Context, cfg GCConfig, state ProtectedState) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultGCConfig().Interval
	}
	if cfg.Retention <= 0 {
		cfg.Retention = DefaultGCConfig().Retention
	}

	gc := NewGCRunner(s, state)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		gc.Tick(ctx, time.Now(), cfg.Retention)
	}
}

// Tick runs one deterministic GC pass. Not safe for concurrent use.
func (g *GCRunner) Tick(ctx context.Context, now time.Time, retention time.Duration) {
	blobs, active, err := g.store.snapshotForGC()
	if err != nil {
		g.store.log.Error("bundle GC: listing blobs", "error", err)
		return
	}

	snap, err := g.state(ctx)
	if err != nil {
		g.store.log.Error("bundle GC: fetching cluster state, skipping tick", "error", err)
		return
	}

	for d := range snap.ProtectedDigests {
		active[d] = struct{}{}
	}

	for _, d := range blobs {
		if _, ok := active[d]; ok {
			g.lastSeen[d] = now
			continue
		}
		if _, ok := g.lastSeen[d]; !ok {
			g.lastSeen[d] = now
			continue
		}
		if g.firstTick {
			continue
		}
		if now.Sub(g.lastSeen[d]) <= retention {
			continue
		}
		deleted, err := g.store.deleteIfUnreferenced(d, snap.ProtectedDigests)
		if err != nil {
			g.store.log.Error("bundle GC: removing blob", "digest", d, "error", err)
			continue
		}
		if deleted {
			delete(g.lastSeen, d)
			g.store.log.Info("bundle GC: removed stale blob", "digest", d[:12])
		} else {
			g.lastSeen[d] = now
		}
	}

	if err := g.store.purgeStaleAgents(snap.ActiveAgents); err != nil {
		g.store.log.Error("bundle GC: purging stale agents", "error", err)
	}

	for d := range g.lastSeen {
		if _, ok := active[d]; ok {
			continue
		}
		found := false
		for _, b := range blobs {
			if b == d {
				found = true
				break
			}
		}
		if !found {
			delete(g.lastSeen, d)
		}
	}

	g.firstTick = false
}
