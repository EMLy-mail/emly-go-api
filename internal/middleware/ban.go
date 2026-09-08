package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/config"
	"emly-api-go/internal/models"
)

// BanList is the permanent block list from the bans table, held in memory and
// consulted on every request.
//
// It is cached rather than queried per request for the obvious reason - this
// sits in front of endpoints a fleet of hundreds polls continuously - and
// refreshed two ways: immediately when an admin route changes it (Reload),
// and on a ticker so a second API instance picks up a ban somebody added
// through the first one. The ticker is what makes this correct on more than
// one instance, so the window between a ban and its effect elsewhere is
// bounded by refreshEvery rather than by a restart.
//
// A DB failure during refresh keeps the previous snapshot. Ban enforcement
// must not swing open because the database hiccuped, and it must not swing
// shut either - the last known list is the safest of the three options.
type BanList struct {
	db *sqlx.DB

	mu sync.RWMutex
	// Three maps rather than one scan: each request looks up at most three
	// exact keys, so matching costs the same whether the list holds ten
	// entries or ten thousand.
	ips       map[string]models.Ban
	hwids     map[string]models.Ban
	hostnames map[string]models.Ban // keys lower-cased
	loaded    bool

	refreshEvery time.Duration
	adminKey     string
}

// NewBanList builds a BanList and loads it once, synchronously, so the first
// request after start is already filtered. A load failure here is logged and
// left to the refresh loop: refusing to start the API because the bans table
// is briefly unreachable would be a worse outage than running unfiltered for
// a few seconds.
func NewBanList(db *sqlx.DB, cfg *config.Config) *BanList {
	b := &BanList{
		db:           db,
		ips:          map[string]models.Ban{},
		hwids:        map[string]models.Ban{},
		hostnames:    map[string]models.Ban{},
		refreshEvery: 30 * time.Second,
		adminKey:     cfg.AdminKey,
	}
	if err := b.Reload(context.Background()); err != nil {
		slog.Error("ban list: initial load failed, starting empty", "error", err)
	}
	return b
}

// Run refreshes the snapshot until ctx is cancelled.
func (b *BanList) Run(ctx context.Context) {
	t := time.NewTicker(b.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.Reload(ctx); err != nil {
				slog.WarnContext(ctx, "ban list: refresh failed, keeping previous snapshot", "error", err)
			}
		}
	}
}

// Reload replaces the snapshot from the database. Admin handlers call it
// right after a write so the change takes effect on the next request rather
// than at the next tick.
func (b *BanList) Reload(ctx context.Context) error {
	var rows []models.Ban
	if err := b.db.SelectContext(ctx, &rows,
		`SELECT id, ban_type, value, reason, created_by, created_at FROM bans`); err != nil {
		return err
	}

	ips := make(map[string]models.Ban, len(rows))
	hwids := make(map[string]models.Ban, len(rows))
	hostnames := make(map[string]models.Ban, len(rows))
	for _, row := range rows {
		switch row.BanType {
		case models.BanTypeIP:
			ips[row.Value] = row
		case models.BanTypeHWID:
			hwids[row.Value] = row
		case models.BanTypeHostname:
			hostnames[strings.ToLower(row.Value)] = row
		}
	}

	b.mu.Lock()
	b.ips, b.hwids, b.hostnames, b.loaded = ips, hwids, hostnames, true
	b.mu.Unlock()
	return nil
}

// match returns the ban that blocks this request, if any. IP is checked
// first because it is the only identifier present on every request; the
// header-based ones only exist on traffic from the EMLy Updater.
func (b *BanList) match(ip, hwid, hostname string) (models.Ban, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if ip != "" {
		if ban, ok := b.ips[ip]; ok {
			return ban, true
		}
	}
	if hwid != "" {
		if ban, ok := b.hwids[hwid]; ok {
			return ban, true
		}
	}
	if hostname != "" {
		if ban, ok := b.hostnames[strings.ToLower(hostname)]; ok {
			return ban, true
		}
	}
	return models.Ban{}, false
}

// Handler rejects banned clients with 403 before anything else runs.
//
// A valid X-Admin-Key is exempt, deliberately. The list exists to cut off
// fleet clients, and an operator who bans the office's public IP would
// otherwise lock the dashboard - and the route that removes the ban - out
// from behind that same IP. Anyone holding the admin key can already delete
// any ban, so exempting them removes no protection that was really there.
func (b *BanList) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.adminKey != "" && r.Header.Get("X-Admin-Key") == b.adminKey {
			next.ServeHTTP(w, r)
			return
		}

		ip := clientIP(r)
		ban, blocked := b.match(ip, r.Header.Get("X-EMLy-HWID"), r.Header.Get("X-EMLy-Hostname"))
		if !blocked {
			next.ServeHTTP(w, r)
			return
		}

		slog.WarnContext(r.Context(), "request rejected (banned)",
			"ban_id", ban.ID, "ban_type", string(ban.BanType), "ban_value", ban.Value,
			"ip", ip, "path", r.URL.Path)

		// A plain 403 with a body, not the rate limiter's connection drop:
		// this is a decision an operator made, and a client that is told
		// "forbidden" logs something an admin can recognise, while a dropped
		// connection looks like a network fault and gets retried forever.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "banned"})
	})
}

// clientIP mirrors RateLimiter.getIP: chiMiddleware.RealIP has usually
// already rewritten RemoteAddr, but the proxy headers are honoured directly
// too so both middlewares agree on what "the client's address" means.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		if i := strings.IndexByte(ip, ','); i >= 0 {
			ip = ip[:i]
		}
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
