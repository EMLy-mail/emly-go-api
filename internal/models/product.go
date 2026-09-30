package models

import "time"

// Product is one piece of software the updates surface distributes (the
// products table). Slug is its identity everywhere: the {product} segment of
// /v2/updates/{product}/..., update_releases.product, updater_events.product
// and updater_client_products.product.
//
// S3Prefix nil means "derive it" - see productreg.S3Prefix. Enabled false
// hides the product's public manifest and downloads (404) while leaving its
// admin routes usable, so releases can be staged before it goes live.
type Product struct {
	Slug      string    `db:"slug"       json:"slug"`
	Name      string    `db:"name"       json:"name"`
	S3Prefix  *string   `db:"s3_prefix"  json:"s3_prefix,omitempty"`
	Enabled   bool      `db:"enabled"    json:"enabled"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// ClientProduct is one row of updater_client_products: the version of a
// product installed on a client at its last report.
type ClientProduct struct {
	Product   string    `db:"product"    json:"product"`
	Version   string    `db:"version"    json:"version"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}
