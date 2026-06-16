package obfuscate

// ConfigSchema describes the client-config payload served at GET /v2/api/config.
// The IDs here are the stable contract between server and client; both sides
// must agree on this exact tree (and on the words/syllables tables) for the
// daily name regeneration to line up.
//
// Keep this in sync with the client copy.
var ConfigSchema = []Field{
	{ID: "maintenance_mode"},
	{ID: "min_supported_version"},
	{ID: "telemetry_enabled"},
	{
		ID: "feature_flags",
		Children: []Field{
			{ID: "new_dashboard"},
			{ID: "dark_mode_v2"},
			{ID: "beta_uploads"},
		},
	},
	{
		ID: "limits",
		Children: []Field{
			{ID: "max_upload_mb"},
			{ID: "max_reports_per_day"},
		},
	},
}
