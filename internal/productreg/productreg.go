// Package productreg is the in-memory registry of the products the updates
// surface distributes (the products table), plus the middleware that puts
// the product a request is about onto its context.
//
// It is a shared leaf package, not a feature: internal/updates needs it to
// scope releases, internal/stats to validate ?product=, internal/products to
// reload it after an admin write - and features must not import each other.
//
// The registry is cached for the same reason middleware.BanList is: the
// manifest route it guards is polled continuously by the whole fleet, and a
// product list changes a few times a year. It is refreshed immediately after
// an admin write (Reload) and on a ticker (Run), so a second API instance
// picks up a product created through the first; a failed refresh keeps the
// previous snapshot, so a DB hiccup never makes every product 404 at once.
package productreg

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
)

// EMLy is the slug of the product this API was built around. It is seeded by
// migration 22, cannot be deleted, and is what the legacy product-less routes
// (/v2/updates/manifest, /v2/updates/releases/...) serve.
const EMLy = "emly"

// slugPattern bounds a slug to what fits updater_events.product and
// updater_event_hourly.product (VARCHAR(20)) and to a safe URL/S3 segment.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

// reserved slugs collide with something that already answers for them:
// "manifest", "releases", "download" are static segments under /v2/updates,
// which chi prefers over {product}, so such a product would be unreachable;
// "updater" is both a static segment and the updater_events.product value of
// the Agent's own self-update traffic; "all" is the stats filter meaning
// "every product"; "products" is kept free for the registry's own surface.
var reserved = map[string]bool{
	"updater":  true,
	"all":      true,
	"manifest": true,
	"releases": true,
	"download": true,
	"products": true,
}

// ValidSlug reports whether s is well-formed. It does not check Reserved.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// Reserved reports whether s can never be a product slug.
func Reserved(s string) bool { return reserved[s] }

// defaultEMLy is what the registry answers for EMLy when it has no row for
// it - no database (tests) or an initial load that failed. The legacy routes
// must keep serving EMLy regardless: they did before the registry existed.
func defaultEMLy() models.Product {
	return models.Product{Slug: EMLy, Name: "EMLy", Enabled: true}
}

// S3Prefix is where p's installers live inside the updates bucket. An
// explicit s3_prefix wins; otherwise EMLy keeps base (S3_UPDATES_PREFIX, where
// its installers were before products existed) and every other product gets
// its own base/<slug> folder.
func S3Prefix(p models.Product, base string) string {
	if p.S3Prefix != nil && *p.S3Prefix != "" {
		return *p.S3Prefix
	}
	if p.Slug == EMLy {
		return base
	}
	if base == "" {
		return p.Slug
	}
	return path.Join(base, p.Slug)
}

// Registry is the cached products table. A nil *Registry is usable and knows
// only EMLy.
type Registry struct {
	db           *sqlx.DB
	refreshEvery time.Duration

	mu     sync.RWMutex
	bySlug map[string]models.Product
}

// New builds a Registry and loads it once, synchronously, so the first request
// after start already sees every product. A failed load is logged and left to
// Run: EMLy keeps working through defaultEMLy meanwhile. db may be nil (tests),
// in which case the registry knows only EMLy until Set is used.
func New(db *sqlx.DB) *Registry {
	r := &Registry{db: db, refreshEvery: time.Minute, bySlug: map[string]models.Product{}}
	if db != nil {
		if err := r.Reload(context.Background()); err != nil {
			slog.Error("product registry: initial load failed, serving emly only", "error", err)
		}
	}
	return r
}

// Run refreshes the snapshot until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	if r == nil || r.db == nil {
		return
	}
	t := time.NewTicker(r.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Reload(ctx); err != nil {
				slog.WarnContext(ctx, "product registry: refresh failed, keeping previous snapshot", "error", err)
			}
		}
	}
}

// Reload replaces the snapshot from the database. Admin handlers call it right
// after a write so the change is visible on the next request.
func (r *Registry) Reload(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	var rows []models.Product
	if err := r.db.SelectContext(ctx, &rows,
		`SELECT slug, name, s3_prefix, enabled, created_at, updated_at FROM products`); err != nil {
		return err
	}
	r.Set(rows)
	return nil
}

// Set replaces the snapshot with rows. Reload's second half, exported so
// tests can populate a registry without a database.
func (r *Registry) Set(rows []models.Product) {
	m := make(map[string]models.Product, len(rows))
	for _, p := range rows {
		m[p.Slug] = p
	}
	r.mu.Lock()
	r.bySlug = m
	r.mu.Unlock()
}

// Get returns the product with this slug, enabled or not.
func (r *Registry) Get(slug string) (models.Product, bool) {
	if r != nil {
		r.mu.RLock()
		p, ok := r.bySlug[slug]
		r.mu.RUnlock()
		if ok {
			return p, true
		}
	}
	if slug == EMLy {
		return defaultEMLy(), true
	}
	return models.Product{}, false
}

// Has reports whether slug is a known product, enabled or not. Used to
// validate stats filters, where a disabled product's history is still data.
func (r *Registry) Has(slug string) bool {
	_, ok := r.Get(slug)
	return ok
}

// List returns every product, sorted by slug.
func (r *Registry) List() []models.Product {
	out := []models.Product{}
	if r != nil {
		r.mu.RLock()
		for _, p := range r.bySlug {
			out = append(out, p)
		}
		r.mu.RUnlock()
	}
	if !containsSlug(out, EMLy) {
		out = append(out, defaultEMLy())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func containsSlug(ps []models.Product, slug string) bool {
	for _, p := range ps {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// WithProduct returns ctx carrying p.
func WithProduct(ctx context.Context, p models.Product) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the product Resolve or Fixed put on ctx. Without one it
// answers EMLy: every handler that reads it predates products and served EMLy
// unconditionally, so that is the only safe default.
func FromContext(ctx context.Context) models.Product {
	if p, ok := ctx.Value(ctxKey{}).(models.Product); ok {
		return p
	}
	return defaultEMLy()
}

// Resolve reads the {product} URL parameter and puts that product on the
// request context, or answers 404 when it is unknown. With includeDisabled
// false a disabled product is a 404 too: that is the public manifest and
// download, which must not advertise a product that is not live yet. Admin
// routes pass true so releases can be staged first.
func (r *Registry) Resolve(includeDisabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p, ok := r.Get(chi.URLParam(req, "product"))
			if !ok || (!p.Enabled && !includeDisabled) {
				response.Error(w, http.StatusNotFound, "product not found")
				return
			}
			next.ServeHTTP(w, req.WithContext(WithProduct(req.Context(), p)))
		})
	}
}

// Fixed puts the product with this slug on the request context. It serves the
// legacy product-less routes, which always mean EMLy. The product is looked up
// per request, so an s3_prefix edited from the dashboard applies to them too.
func (r *Registry) Fixed(slug string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p, ok := r.Get(slug)
			if !ok {
				response.Error(w, http.StatusNotFound, "product not found")
				return
			}
			next.ServeHTTP(w, req.WithContext(WithProduct(req.Context(), p)))
		})
	}
}

// RequireAssigned refuses, with 403, a request whose session scope
// (session.LoadScope, mounted before it) does not include the product
// Resolve or Fixed put on the context. The check comes after the admin key,
// so reaching it already proves the product exists to anyone who could ask:
// a 403 leaks nothing a 404 would hide.
func RequireAssigned(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !session.ScopeFrom(req.Context()).Allows(FromContext(req.Context()).Slug) {
			response.Error(w, http.StatusForbidden, "product not assigned to this user")
			return
		}
		next.ServeHTTP(w, req)
	})
}
