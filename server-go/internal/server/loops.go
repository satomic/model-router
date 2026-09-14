package server

import (
	"context"
	"time"

	"github.com/satomic/model-router/server-go/internal/aicredits"
	"github.com/satomic/model-router/server-go/internal/ghcache"
	"github.com/satomic/model-router/server-go/internal/release"
	"github.com/satomic/model-router/server-go/internal/usagestats"
)

// The background refresh waits this long before its first run: startup must never block on
// GitHub, and a service that cannot start because github.com is slow is worse than one whose
// cache is a minute stale.
const cacheWarmupDelay = 10 * time.Second

// After a failed refresh, wait this long instead of the full interval -- a GitHub outage is
// usually short, and an empty cache means every request pays a live probe meanwhile.
const cacheRetryDelay = 300 * time.Second

// sleep waits for d, or returns false as soon as the context is cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// StartLoops runs the background tasks until ctx is cancelled.
func (a *App) StartLoops(ctx context.Context) {
	go a.cacheRefreshLoop(ctx)
	go a.usageRollupLoop(ctx)
	go a.aiCreditsLoop(ctx)
	go release.Loop(ctx)
}

// cacheRefreshLoop keeps data/github/ warm.
//
// Every iteration is independent: a GitHub outage must not kill the loop, because a background
// task that dies silently is worse than no background task at all -- the cache would simply stop
// ageing forward and nobody would be told.
//
// The lease keeps N workers from all refreshing the shared data/ directory; losing it is not an
// error, it just means another worker is doing the work.
func (a *App) cacheRefreshLoop(ctx context.Context) {
	if !sleep(ctx, cacheWarmupDelay) {
		return
	}
	for {
		cfg := a.Config()
		delay := time.Duration(ghcache.RefreshSeconds(cfg)) * time.Second
		// Nothing to cache while access control is off: the policy is what decides which member
		// lists are worth having.
		if cfg.KeyPolicy.Bool("enabled", false) && cfg.GHAdminToken() != "" {
			if ghcache.AcquireLease() {
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							logWarn("GitHub cache refresh failed: %v", rec)
							if delay > cacheRetryDelay {
								delay = cacheRetryDelay
							}
						}
						ghcache.ReleaseLease()
					}()
					ghcache.Refresh(ctx, cfg)
				}()
			}
		}
		if !sleep(ctx, delay) {
			return
		}
	}
}

// Long enough that a restart does not spend its first seconds scanning the trace directory while
// the first requests are arriving.
const usageWarmupDelay = 5 * time.Second

// The loop wakes at least this often regardless of the configured interval, so a rollup that
// went stale while this worker held no lease is picked up promptly.
const usagePollDelay = 300 * time.Second

// usageRollupLoop keeps data/usage_rollup.json fresh, so the Usage page never scans trace files
// itself.
func (a *App) usageRollupLoop(ctx context.Context) {
	if !sleep(ctx, usageWarmupDelay) {
		return
	}
	for {
		cfg := a.Config()
		interval := time.Duration(cfg.UsageRollupSeconds(usagestats.DefaultInterval)) * time.Second
		if float64(time.Now().UnixNano())/1e9-usagestats.BuiltAt() >= interval.Seconds() {
			if _, err := usagestats.Refresh(a.Traces, cfg.UsageRollupDays(usagestats.DefaultDays)); err != nil {
				logWarn("usage rollup refresh failed: %v", err)
			}
		}
		delay := interval
		if delay > usagePollDelay {
			delay = usagePollDelay
		}
		if !sleep(ctx, delay) {
			return
		}
	}
}

// How often the scheduler checks whether the cron schedule has come due. The schedule itself has
// minute resolution, so a poll this frequent fires within a minute of the intended time.
const (
	creditsPollDelay   = 30 * time.Second
	creditsWarmupDelay = 15 * time.Second
)

// aiCreditsLoop polls GitHub for the AI-credit pool state on the configured cron schedule.
//
// Same shape as the other two loops: every iteration independent so one GitHub failure cannot
// kill it, and a lease so N workers do not all poll at once. When the lease is lost the snapshot
// another worker wrote is what this one reads, which is the point of the file.
func (a *App) aiCreditsLoop(ctx context.Context) {
	if !sleep(ctx, creditsWarmupDelay) {
		return
	}
	for {
		cfg := a.Config()
		if aicredits.Config(cfg).Enabled && cfg.GHAdminToken() != "" && aicredits.Due(cfg) {
			if aicredits.AcquireLease() {
				if _, err := aicredits.Refresh(ctx, cfg); err != nil {
					logWarn("AI credits refresh failed: %v", err)
				}
				aicredits.ReleaseLease()
			}
		}
		if !sleep(ctx, creditsPollDelay) {
			return
		}
	}
}
