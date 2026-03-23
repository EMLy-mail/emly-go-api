// middleware/ratelimit.go
package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
	failures int
}

type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	banned   sync.Map // ip -> unban time

	// config
	rps        rate.Limit // richieste/sec normali
	burst      int
	maxFails   int           // quanti 429 prima del ban
	banDur     time.Duration // durata ban
	cleanEvery time.Duration
}

func NewRateLimiter(rps float64, burst, maxFails int, banDur time.Duration) *RateLimiter {
	rl := &RateLimiter{
		visitors:   make(map[string]*visitor),
		rps:        rate.Limit(rps),
		burst:      burst,
		maxFails:   maxFails,
		banDur:     banDur,
		cleanEvery: 5 * time.Minute,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) getIP(r *http.Request) string {
	// Rispetta X-Forwarded-For se dietro Traefik/proxy
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		// Prendi il primo IP (quello del client originale)
		if h, _, err := net.SplitHostPort(ip); err == nil {
			return h
		}
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (rl *RateLimiter) getVisitor(ip string) *visitor {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, ok := rl.visitors[ip]
	if !ok {
		v = &visitor{
			limiter: rate.NewLimiter(rl.rps, rl.burst),
		}
		rl.visitors[ip] = v
	}
	v.lastSeen = time.Now()
	return v
}

func (rl *RateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.getIP(r)

		// Controlla ban attivo
		if unbanAt, banned := rl.banned.Load(ip); banned {
			if time.Now().Before(unbanAt.(time.Time)) {
				w.Header().Set("Retry-After", unbanAt.(time.Time).Format(time.RFC1123))
				http.Error(w, "too many requests - temporarily banned", http.StatusForbidden)
				return
			}
			// Ban scaduto
			rl.banned.Delete(ip)
		}

		v := rl.getVisitor(ip)

		if !v.limiter.Allow() {
			rl.mu.Lock()
			v.failures++
			fails := v.failures
			rl.mu.Unlock()

			if fails >= rl.maxFails {
				unbanAt := time.Now().Add(rl.banDur)
				rl.banned.Store(ip, unbanAt)
				// Opzionale: loga il ban
				w.Header().Set("Retry-After", unbanAt.Format(time.RFC1123))
				http.Error(w, "banned", http.StatusForbidden)
				return
			}

			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// Reset failures su richiesta legittima
		rl.mu.Lock()
		v.failures = 0
		rl.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanEvery)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		for ip, v := range rl.visitors {
			if time.Since(v.lastSeen) > 10*time.Minute {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
		// Pulisci anche i ban scaduti
		rl.banned.Range(func(k, v any) bool {
			if time.Now().After(v.(time.Time)) {
				rl.banned.Delete(k)
			}
			return true
		})
	}
}
