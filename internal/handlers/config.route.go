package handlers

import (
	"net/http"
	"time"

	"emly-api-go/internal/obfuscate"
)

// clientConfigValues returns the real client-config values keyed by the stable
// field IDs of obfuscate.ConfigSchema. Values stay in clear text; only the key
// names get scrambled before being written to the response.
//
// These are static for now; swap in a DB/config lookup here if they ever need
// to become dynamic.
func clientConfigValues() map[string]any {
	return map[string]any{
		"maintenance_mode":      false,
		"min_supported_version": "1.4.0",
		"telemetry_enabled":     true,
		"feature_flags": map[string]any{
			"new_dashboard": true,
			"dark_mode_v2":  false,
			"beta_uploads":  false,
		},
		"limits": map[string]any{
			"max_upload_mb":       25,
			"max_reports_per_day": 50,
		},
	}
}

// GetClientConfig serves the client config with obfuscated key names that
// rotate daily. The response carries shift_date (the date the client must use
// to recompute the daily shift) and data (values under obfuscated names).
func GetClientConfig() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftDate := time.Now().UTC().Format(obfuscate.DateLayout)

		shift, err := obfuscate.ShiftFromDateString(shiftDate)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to derive config shift")
			return
		}

		jsonOK(w, map[string]any{
			"updated_at_date": shiftDate,
			"data":            obfuscate.Build(obfuscate.ConfigSchema, clientConfigValues(), shift),
		})
	}
}
