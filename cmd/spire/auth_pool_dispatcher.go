package main

// auth_pool_dispatcher.go — concrete implementation of executor.AuthPool
// that drives pkg/auth/pool.Selector to reserve a credential slot per
// dispatch. Constructed lazily at the bridge layer so the executor
// package stays free of pkg/auth/pool — only this file links them.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
)

// poolHeartbeatInterval is the default cadence at which a dispatch's
// heartbeat goroutine refreshes the slot's HeartbeatAt. The steward sweep
// reaps claims older than its configured stale threshold, so any value
// noticeably below that threshold keeps an active dispatch's claim alive.
// 30s matches the design recommendation (spi-9hdpji §dispatch flow).
const poolHeartbeatInterval = 30 * time.Second

// authPoolAdapter is the executor.AuthPool implementation. It wraps a
// *pool.Selector + the policy's default/fallback pool names + the slot
// state directory, and translates pool errors into the executor's
// boundary types (notably *executor.RateLimitedError).
type authPoolAdapter struct {
	selector     *pool.Selector
	cfg          *pool.Config
	stateDir     string
	defaultPool  string
	fallbackPool string
}

// newAuthPoolAdapter constructs the adapter from an already-loaded config
// + state dir. Returns nil when cfg has no slots in either pool — there's
// nothing to dispatch against, so the executor should fall through to the
// legacy single-token path. Validation of cfg has already happened inside
// pool.LoadConfig.
func newAuthPoolAdapter(cfg *pool.Config, stateDir string) *authPoolAdapter {
	if cfg == nil {
		return nil
	}
	if len(cfg.Subscription) == 0 && len(cfg.APIKey) == 0 {
		return nil
	}
	defaultPool := cfg.DefaultPool
	if defaultPool == "" {
		// Mirror the legacy default: subscription first, api-key only when
		// there is no subscription slot. This keeps single-pool towers
		// dispatching without forcing them to set DefaultPool explicitly.
		if len(cfg.Subscription) > 0 {
			defaultPool = pool.PoolNameSubscription
		} else {
			defaultPool = pool.PoolNameAPIKey
		}
	}
	policy := pool.NewPolicy(cfg.Selection)
	wake := pool.NewPoolWake(stateDir)
	selector := pool.NewSelector(cfg, stateDir, policy, wake)
	return &authPoolAdapter{
		selector:     selector,
		cfg:          cfg,
		stateDir:     stateDir,
		defaultPool:  defaultPool,
		fallbackPool: cfg.FallbackPool,
	}
}

// Acquire implements executor.AuthPool. It picks a slot from the default
// pool, falls back to the configured fallback pool on
// *pool.ErrAllRateLimited, and translates a terminal pool exhaustion into
// *executor.RateLimitedError so the caller can park the dispatch.
//
// On success a heartbeat goroutine is launched against the dispatch ctx
// and the lease's Release closure cancels that goroutine before invoking
// Selector.Release.
func (a *authPoolAdapter) Acquire(ctx context.Context, dispatchID string) (executor.PoolLease, error) {
	if a == nil || a.selector == nil {
		return executor.PoolLease{}, errors.New("auth pool: not initialized")
	}
	if dispatchID == "" {
		return executor.PoolLease{}, errors.New("auth pool: empty dispatchID")
	}

	pickedPool := a.defaultPool
	slotName, err := a.selector.Pick(ctx, pickedPool, dispatchID)
	if err != nil {
		var rl *pool.ErrAllRateLimited
		if errors.As(err, &rl) && a.fallbackPool != "" && a.fallbackPool != pickedPool {
			fallbackSlot, fallbackErr := a.selector.Pick(ctx, a.fallbackPool, dispatchID)
			if fallbackErr == nil {
				pickedPool = a.fallbackPool
				slotName = fallbackSlot
				err = nil
			} else {
				var fallbackRL *pool.ErrAllRateLimited
				if errors.As(fallbackErr, &fallbackRL) {
					return executor.PoolLease{}, &executor.RateLimitedError{
						ResetsAt: fallbackRL.ResetsAt,
						Wrapped:  fallbackErr,
					}
				}
				return executor.PoolLease{}, fallbackErr
			}
		}
	}
	if err != nil {
		var rl *pool.ErrAllRateLimited
		if errors.As(err, &rl) {
			return executor.PoolLease{}, &executor.RateLimitedError{
				ResetsAt: rl.ResetsAt,
				Wrapped:  err,
			}
		}
		return executor.PoolLease{}, err
	}

	authEnv, slotErr := a.slotAuthEnv(pickedPool, slotName)
	if slotErr != nil {
		// Picked slot but cannot resolve its secret — release the claim
		// and return the error so the caller surfaces a real failure
		// rather than spawning with no auth.
		_ = a.selector.Release(pickedPool, slotName, dispatchID)
		return executor.PoolLease{}, slotErr
	}

	hbCtx, hbCancel := context.WithCancel(context.Background())
	go func() {
		if hbErr := pool.Heartbeat(hbCtx, a.stateDir, slotName, dispatchID, poolHeartbeatInterval); hbErr != nil && !errors.Is(hbErr, context.Canceled) {
			log.Printf("auth pool: heartbeat for slot=%q dispatch=%q exited: %v", slotName, dispatchID, hbErr)
		}
	}()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			hbCancel()
			if relErr := a.selector.Release(pickedPool, slotName, dispatchID); relErr != nil {
				log.Printf("auth pool: release slot=%q dispatch=%q: %v", slotName, dispatchID, relErr)
			}
		})
	}

	return executor.PoolLease{
		SlotName:     slotName,
		PoolName:     pickedPool,
		AuthEnv:      authEnv,
		PoolStateDir: a.stateDir,
		Release:      release,
	}, nil
}

// slotAuthEnv resolves the env-var entry the spawned subprocess should
// receive for the picked slot. The pool's two pools map to distinct env
// vars: subscription → CLAUDE_CODE_OAUTH_TOKEN, api-key → ANTHROPIC_API_KEY.
// Returns an error when the slot can't be found (config drift between
// the cached state and the in-memory cfg).
func (a *authPoolAdapter) slotAuthEnv(poolName, slotName string) ([]string, error) {
	switch poolName {
	case pool.PoolNameSubscription:
		for _, s := range a.cfg.Subscription {
			if s.Name == slotName {
				if s.Token == "" {
					return nil, fmt.Errorf("auth pool: subscription slot %q has empty token", slotName)
				}
				return []string{config.EnvClaudeCodeOAuthToken + "=" + s.Token}, nil
			}
		}
		return nil, fmt.Errorf("auth pool: subscription slot %q not found in config", slotName)
	case pool.PoolNameAPIKey:
		for _, s := range a.cfg.APIKey {
			if s.Name == slotName {
				if s.Key == "" {
					return nil, fmt.Errorf("auth pool: api-key slot %q has empty key", slotName)
				}
				return []string{config.EnvAnthropicAPIKey + "=" + s.Key}, nil
			}
		}
		return nil, fmt.Errorf("auth pool: api-key slot %q not found in config", slotName)
	default:
		return nil, fmt.Errorf("auth pool: unknown pool %q", poolName)
	}
}

// resolveAuthPoolForExecutor wraps buildAuthPool with the lossy logging
// the executor bridge wants: a malformed auth.toml is surfaced as a log
// line and the dep is left nil so the legacy per-bead AuthContext path
// keeps working. The executor is the wrong layer to fail closed on a
// missing pool config (single-token towers are a first-class supported
// shape); the CLI's `spire config auth probe` is the right surface for
// that diagnosis.
func resolveAuthPoolForExecutor() executor.AuthPool {
	pool, err := buildAuthPool()
	if err != nil {
		log.Printf("auth pool: disabled (config error): %v", err)
		return nil
	}
	return pool
}

// buildAuthPool wires the executor.AuthPool dep for the active tower. It
// reads <towerDir>/auth.toml (or the legacy credentials.toml fallback
// pool.LoadConfig provides) and constructs an adapter around the pool
// selector. Returns nil + a nil error when no auth.toml or
// credentials.toml exists — the executor falls through to the
// per-bead AuthContext path. Returns an error when the file is present
// but malformed; we surface the error loudly so a misconfigured tower
// fails summon rather than silently dispatching without pool gating.
func buildAuthPool() (executor.AuthPool, error) {
	towerDir, stateDir, err := authPoolDirs()
	if err != nil {
		return nil, fmt.Errorf("resolve auth pool dirs: %w", err)
	}
	cfg, err := pool.LoadConfig(towerDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load auth pool config: %w", err)
	}
	adapter := newAuthPoolAdapter(cfg, stateDir)
	if adapter == nil {
		return nil, nil
	}
	return adapter, nil
}
