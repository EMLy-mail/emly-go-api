package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"emly-api-go/internal/config"
)

type limitConfig struct {
	maxReqs  int
	window   time.Duration
	maxFails int
	banDur   time.Duration
}

type ipState struct {
	count       int
	windowStart time.Time
	failures    int
	lastSeen    time.Time
}

type RateLimiter struct {
	mu             sync.Mutex
	unauthVisitors map[string]*ipState
	authVisitors   map[string]*ipState
	banned         sync.Map // ip -> unban time (shared)

	unauthCfg  limitConfig
	authCfg    limitConfig
	cleanEvery time.Duration
}

// NewRateLimiter creates a two-tier rate limiter configured from cfg:
//   - Unauthenticated (no X-API-Key / X-Admin-Key): RL_UNAUTH_* env vars
//   - Authenticated (X-API-Key or X-Admin-Key present): RL_AUTH_* env vars
func NewRateLimiter(cfg *config.Config) *RateLimiter {
	rl := &RateLimiter{
		unauthVisitors: make(map[string]*ipState),
		authVisitors:   make(map[string]*ipState),
		unauthCfg: limitConfig{
			maxReqs:  cfg.RateLimit.UnauthMaxReqs,
			window:   cfg.RateLimit.UnauthWindow,
			maxFails: cfg.RateLimit.UnauthMaxFails,
			banDur:   cfg.RateLimit.UnauthBanDur,
		},
		authCfg: limitConfig{
			maxReqs:  cfg.RateLimit.AuthMaxReqs,
			window:   cfg.RateLimit.AuthWindow,
			maxFails: cfg.RateLimit.AuthMaxFails,
			banDur:   cfg.RateLimit.AuthBanDur,
		},
		cleanEvery: 10 * time.Minute,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) getIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		if h, _, err := net.SplitHostPort(ip); err == nil {
			return h
		}
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (rl *RateLimiter) isAuthenticated(r *http.Request) bool {
	return r.Header.Get("X-API-Key") != "" || r.Header.Get("X-Admin-Key") != ""
}

// record increments the counter for the IP and returns whether the limit was
// exceeded, the current failure count, and whether the IP should be banned.
func (rl *RateLimiter) record(ip string, auth bool) (exceeded bool, failures int, shouldBan bool, banDur time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	var visitors map[string]*ipState
	var cfg limitConfig
	if auth {
		visitors = rl.authVisitors
		cfg = rl.authCfg
	} else {
		visitors = rl.unauthVisitors
		cfg = rl.unauthCfg
	}

	v, ok := visitors[ip]
	if !ok {
		v = &ipState{windowStart: time.Now()}
		visitors[ip] = v
	}

	now := time.Now()
	v.lastSeen = now

	if now.Sub(v.windowStart) >= cfg.window {
		v.count = 0
		v.windowStart = now
	}

	v.count++

	if v.count > cfg.maxReqs {
		v.failures++
		return true, v.failures, v.failures >= cfg.maxFails, cfg.banDur
	}

	v.failures = 0
	return false, 0, false, 0
}

func (rl *RateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)

		if unbanAt, banned := rl.banned.Load(ip); banned {
			if time.Now().Before(unbanAt.(time.Time)) {
				slog.WarnContext(r.Context(), "ip dropped (banned)", "ip", ip, "ban_until", unbanAt.(time.Time), "path", r.URL.Path)
				panic(http.ErrAbortHandler)
			}
			rl.banned.Delete(ip)
		}

		auth := rl.isAuthenticated(r)
		exceeded, failures, shouldBan, banDur := rl.record(ip, auth)

		if exceeded {
			if shouldBan {
				unbanAt := time.Now().Add(banDur)
				rl.banned.Store(ip, unbanAt)
				slog.WarnContext(r.Context(), "ip banned", "ip", ip, "ban_until", unbanAt, "path", r.URL.Path, "auth", auth)
				w.Header().Set("Retry-After", unbanAt.Format(time.RFC1123))
				http.Error(w, "banned", http.StatusForbidden)
				return
			}
			slog.WarnContext(r.Context(), "rate limit exceeded", "ip", ip, "violations", failures, "path", r.URL.Path, "auth", auth)
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanEvery)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		for ip, v := range rl.unauthVisitors {
			if time.Since(v.lastSeen) > rl.unauthCfg.window*2 {
				delete(rl.unauthVisitors, ip)
			}
		}
		for ip, v := range rl.authVisitors {
			if time.Since(v.lastSeen) > rl.authCfg.window*2 {
				delete(rl.authVisitors, ip)
			}
		}
		rl.mu.Unlock()
		rl.banned.Range(func(k, v any) bool {
			if time.Now().After(v.(time.Time)) {
				rl.banned.Delete(k)
			}
			return true
		})
	}
}
