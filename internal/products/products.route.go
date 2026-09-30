// Package products is the /v2/products admin surface over the products
// table. The registry it edits lives in internal/productreg, which the
// updates and stats features read; this package only writes and reloads it.
package products

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/models"
	"emly-api-go/internal/productreg"
	"emly-api-go/internal/response"
	"emly-api-go/internal/session"
)

const productSelectCols = `slug, name, s3_prefix, enabled, created_at, updated_at`

// ListProducts handles GET /v2/products: every product, or only the assigned
// ones for a dashboard user.
func ListProducts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all := []models.Product{}
		if err := db.SelectContext(r.Context(), &all,
			`SELECT `+productSelectCols+` FROM products ORDER BY slug`); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to list products")
			return
		}
		scope := session.ScopeFrom(r.Context())
		products := []models.Product{}
		for _, p := range all {
			if scope.Allows(p.Slug) {
				products = append(products, p)
			}
		}
		response.OK(w, products)
	}
}

// assigned answers 404 for a product outside the caller's scope - the same
// answer as a product that does not exist, so a user cannot learn which
// products they were not given. Returns false when it wrote a response.
func assigned(w http.ResponseWriter, r *http.Request, slug string) bool {
	if !session.ScopeFrom(r.Context()).Allows(slug) {
		response.Error(w, http.StatusNotFound, "product not found")
		return false
	}
	return true
}

// GetProduct handles GET /v2/products/{slug}.
func GetProduct(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if !assigned(w, r, slug) {
			return
		}
		p, err := fetchProduct(r.Context(), db, slug)
		if errors.Is(err, sql.ErrNoRows) {
			response.Error(w, http.StatusNotFound, "product not found")
			return
		}
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch product")
			return
		}
		response.OK(w, p)
	}
}

// CreateProduct handles POST /v2/products. The slug is permanent: it is
// written into every release, event and installed-version row of the
// product, so it cannot be renamed later - only name, s3_prefix and enabled
// can (PatchProduct).
func CreateProduct(db *sqlx.DB, reg *productreg.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Slug     string  `json:"slug"`
			Name     string  `json:"name"`
			S3Prefix *string `json:"s3_prefix"`
			Enabled  *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		slug := strings.TrimSpace(body.Slug)
		if !productreg.ValidSlug(slug) {
			response.Error(w, http.StatusBadRequest, "slug must match ^[a-z0-9][a-z0-9-]{0,19}$")
			return
		}
		if productreg.Reserved(slug) {
			response.Error(w, http.StatusBadRequest, "slug '"+slug+"' is reserved")
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" || len([]rune(name)) > 100 {
			response.Error(w, http.StatusBadRequest, "name is required (max 100 characters)")
			return
		}
		prefix, err := normalizeS3Prefix(body.S3Prefix)
		if err != nil {
			response.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}

		tx, err := db.BeginTxx(r.Context(), nil)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to begin transaction")
			return
		}
		defer tx.Rollback()

		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO products (slug, name, s3_prefix, enabled) VALUES (?, ?, ?, ?)`,
			slug, name, prefix, enabled)
		if isDuplicate(err) {
			response.Error(w, http.StatusConflict, "product already exists")
			return
		}
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to create product")
			return
		}

		// The creator is assigned the product in the same transaction:
		// otherwise a dashboard user would create a product they
		// immediately cannot see.
		if userID := session.ScopeFrom(r.Context()).UserID; userID != "" {
			if _, err := tx.ExecContext(r.Context(),
				`INSERT IGNORE INTO user_products (user_id, product) VALUES (?, ?)`, userID, slug); err != nil {
				response.Error(w, http.StatusInternalServerError, "failed to assign product to its creator")
				return
			}
		}

		if err := tx.Commit(); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to commit")
			return
		}

		reload(r.Context(), reg)
		p, err := fetchProduct(r.Context(), db, slug)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch created product")
			return
		}
		response.Created(w, p)
	}
}

// PatchProduct handles PATCH /v2/products/{slug}. An empty s3_prefix clears
// it back to the derived default (productreg.S3Prefix). Changing it does not
// move files already in the bucket: existing releases' installers must be
// moved by hand, or their downloads 404.
func PatchProduct(db *sqlx.DB, reg *productreg.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if !assigned(w, r, slug) {
			return
		}

		var body struct {
			Name     *string `json:"name"`
			S3Prefix *string `json:"s3_prefix"`
			Enabled  *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			response.Error(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		var sets []string
		var args []any
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" || len([]rune(name)) > 100 {
				response.Error(w, http.StatusBadRequest, "name must be 1-100 characters")
				return
			}
			sets = append(sets, "name = ?")
			args = append(args, name)
		}
		if body.S3Prefix != nil {
			prefix, err := normalizeS3Prefix(body.S3Prefix)
			if err != nil {
				response.Error(w, http.StatusBadRequest, err.Error())
				return
			}
			sets = append(sets, "s3_prefix = ?")
			args = append(args, prefix)
		}
		if body.Enabled != nil {
			sets = append(sets, "enabled = ?")
			args = append(args, *body.Enabled)
		}
		if len(sets) == 0 {
			response.Error(w, http.StatusBadRequest, "no fields to update")
			return
		}

		if _, err := fetchProduct(r.Context(), db, slug); errors.Is(err, sql.ErrNoRows) {
			response.Error(w, http.StatusNotFound, "product not found")
			return
		} else if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch product")
			return
		}

		args = append(args, slug)
		if _, err := db.ExecContext(r.Context(),
			`UPDATE products SET `+strings.Join(sets, ", ")+` WHERE slug = ?`, args...); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to update product")
			return
		}

		reload(r.Context(), reg)
		p, err := fetchProduct(r.Context(), db, slug)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to fetch updated product")
			return
		}
		response.OK(w, p)
	}
}

// DeleteProduct handles DELETE /v2/products/{slug}. A product that still has
// releases is a 409: deleting it would orphan their rows and installers.
// Disable it instead (PATCH enabled=false) to take it off the public routes.
// EMLy cannot be deleted at all - the legacy routes serve it unconditionally.
// Telemetry (updater_events, the hourly rollup, installed versions) is kept:
// it is the fleet's history, not the product's configuration.
func DeleteProduct(db *sqlx.DB, reg *productreg.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		if !assigned(w, r, slug) {
			return
		}
		if slug == productreg.EMLy {
			response.Error(w, http.StatusConflict, "the emly product cannot be deleted")
			return
		}

		var releases int
		if err := db.GetContext(r.Context(), &releases,
			`SELECT COUNT(*) FROM update_releases WHERE product = ?`, slug); err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to count releases")
			return
		}
		if releases > 0 {
			response.Error(w, http.StatusConflict, "product still has releases; delete them first or disable the product")
			return
		}

		res, err := db.ExecContext(r.Context(), `DELETE FROM products WHERE slug = ?`, slug)
		if err != nil {
			response.Error(w, http.StatusInternalServerError, "failed to delete product")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			response.Error(w, http.StatusNotFound, "product not found")
			return
		}

		reload(r.Context(), reg)
		response.OK(w, map[string]bool{"deleted": true})
	}
}

func fetchProduct(ctx context.Context, db *sqlx.DB, slug string) (models.Product, error) {
	var p models.Product
	err := db.GetContext(ctx, &p, `SELECT `+productSelectCols+` FROM products WHERE slug = ?`, slug)
	return p, err
}

// normalizeS3Prefix trims surrounding slashes and turns "" into NULL (derive
// the default). A ".." segment is refused: the prefix is joined into object
// keys and must stay inside the bucket's own layout.
func normalizeS3Prefix(p *string) (*string, error) {
	if p == nil {
		return nil, nil
	}
	v := strings.Trim(strings.TrimSpace(*p), "/")
	if v == "" {
		return nil, nil
	}
	if len(v) > 255 {
		return nil, errors.New("s3_prefix must be at most 255 characters")
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, errors.New("s3_prefix must not contain empty, '.' or '..' segments")
		}
	}
	return &v, nil
}

// reload refreshes the registry after a write. A failure is logged, not
// returned: the row is committed, and the ticker picks it up within a minute.
func reload(ctx context.Context, reg *productreg.Registry) {
	if err := reg.Reload(ctx); err != nil {
		slog.WarnContext(ctx, "product registry: reload after write failed", "error", err)
	}
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
