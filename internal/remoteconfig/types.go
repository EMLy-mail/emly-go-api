// Package remoteconfig implements the document type, validation, override
// evaluation and canonical serialization shared by the /v2/config API and
// (as a copy, see the design doc) the EMLy Updater client. It has no HTTP
// and no DB dependency on purpose: internal/handlers wires it to storage,
// the client wires the equivalent Go package to its own fetch/cache loop.
//
// The rules implemented here follow
// emly-updater/docs/superpowers/specs/2026-09-04-remote-config-design.md §7-8
// (the normative reference for the wire format) and
// docs/superpowers/specs/2026-09-04-remote-config-api-design.md §6 (this
// package's contract).
package remoteconfig

// Document is the whole /v2/config policy document: the unit of storage,
// validation and the wire format served to clients.
//
// Field order below is the order encoding/json emits them in when the
// struct is marshaled directly (Go preserves declared field order), and
// Canonical (canonical.go) depends on that being stable - this order IS the
// canonical field order and must not be reordered casually once documents
// exist in the wild with an etag computed over it.
//
// Optional sub-objects are pointers with no `omitempty`, so they marshal as
// JSON `null` when unset rather than being omitted - the canonical form (§5.3
// of the API design doc) is "null for unset optionals", which keeps two
// documents that differ only in whether a key was present at all producing
// the same bytes once round-tripped through this struct.
type Document struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      int64  `json:"revision"`
	GeneratedAt   string `json:"generatedAt"`

	Refresh *Refresh `json:"refresh"`

	Servers       map[string]string `json:"servers"`
	DefaultServer string            `json:"defaultServer"`

	IPCProtocol *IPCProtocol `json:"ipcProtocol"`

	DCLookupMap map[string]Site `json:"dcLookupMap"`

	HostIntegrity *HostIntegrity `json:"hostIntegrity"`

	Control *Control `json:"control"`

	Updater *UpdaterTuning `json:"updater"`

	Logging *Logging `json:"logging"`

	Overrides []Override `json:"overrides"`
}

// Refresh controls how often the client re-fetches the document.
type Refresh struct {
	IntervalMinutes int  `json:"intervalMinutes"`
	StaleAfterDays  *int `json:"staleAfterDays"`
}

// IPCProtocol narrows the updater<->EMLy IPC compatibility. Versions is
// keyed by the protocol version number as a decimal string ("1", "2", ...).
type IPCProtocol struct {
	Versions       map[string]IPCVersion `json:"versions"`
	DefaultVersion int                   `json:"defaultVersion"`
}

type IPCVersion struct {
	Updater VersionRange `json:"updater"`
	EMLy    VersionRange `json:"emly"`
	Enabled bool         `json:"enabled"`
}

// VersionRange bounds a semver range; either end may be null (no bound).
type VersionRange struct {
	Min *string `json:"min"`
	Max *string `json:"max"`
}

// Site is one entry of dcLookupMap: the resolver chain for hosts whose
// nearest DC is the map key.
type Site struct {
	InternalSubnets []string `json:"internalSubnets"`
	BaseServer      string   `json:"baseServer"`
	BackupServer    []string `json:"backupServer"`
	Enabled         bool     `json:"enabled"`
}

type HostIntegrity struct {
	Enabled   bool      `json:"enabled"`
	Whitelist Whitelist `json:"whitelist"`
}

type Whitelist struct {
	Hostnames []string `json:"hostnames"`
	HWIDs     []string `json:"hwids"`
}

type Control struct {
	Updater ControlGate `json:"updater"`
	App     AppGate     `json:"app"`
}

// ControlGate is a plain kill switch: enabled/reason/until.
type ControlGate struct {
	Enabled bool    `json:"enabled"`
	Reason  *string `json:"reason"`
	Until   *string `json:"until"`
}

// AppGate is the same shape as ControlGate plus a mode the updater does not
// enforce itself, only relays to EMLy over IPC.
type AppGate struct {
	Enabled bool    `json:"enabled"`
	Mode    string  `json:"mode"`
	Reason  *string `json:"reason"`
	Until   *string `json:"until"`
}

// UpdaterTuning is the remote equivalent of the updater's own config.ini
// runtime keys. Every field is meaningful at its zero value only via the
// pointer's nilness - a missing field means "use config.ini", not "use 0".
type UpdaterTuning struct {
	PollIntervalMinutes *int             `json:"pollIntervalMinutes"`
	ChannelOverride     *string          `json:"channelOverride"`
	CriticalWarning     *CriticalWarning `json:"criticalWarning"`
	DCLookupRetry       *DCLookupRetry   `json:"dcLookupRetry"`
	Resolver            *ResolverTuning  `json:"resolver"`
	SelfUpdate          *ToggleOnly      `json:"selfUpdate"`
	InstallCertificate  *ToggleOnly      `json:"installCertificate"`
}

type CriticalWarning struct {
	Enabled bool `json:"enabled"`
	Seconds int  `json:"seconds"`
}

type DCLookupRetry struct {
	Attempts     int `json:"attempts"`
	DelaySeconds int `json:"delaySeconds"`
}

type ResolverTuning struct {
	Attempts           int `json:"attempts"`
	BaseBackoffSeconds int `json:"baseBackoffSeconds"`
}

// ToggleOnly is the shape shared by selfUpdate and installCertificate: just
// an enabled flag.
type ToggleOnly struct {
	Enabled bool `json:"enabled"`
}

type Logging struct {
	Level     string `json:"level"`
	MaxSizeMB int    `json:"maxSizeMB"`
	Backups   int    `json:"backups"`
	Compress  bool   `json:"compress"`
	EventLog  bool   `json:"eventLog"`
}

// Override is one per-host/per-site exception, applied in list order.
type Override struct {
	ID     string    `json:"id"`
	Match  Selector  `json:"match"`
	Except *Selector `json:"except"`
	Until  *string   `json:"until"`
	Patch  PatchDoc  `json:"patch"`
}

// Selector is the "match"/"except" object. All is a pointer so "absent" and
// "false" can be told apart - both "all: false" and "all" combined with any
// list are validation errors, only "all: true" alone is meaningful.
type Selector struct {
	All       *bool    `json:"all"`
	HWIDs     []string `json:"hwids"`
	Hostnames []string `json:"hostnames"`
	DCs       []string `json:"dcs"`
	Subnets   []string `json:"subnets"`
	Domains   []string `json:"domains"`
}

// PatchDoc is a JSON Merge Patch (RFC 7386) restricted, by validation, to the
// top-level keys "control", "updater", "logging" and "defaultServer". It is
// kept as a generic map rather than a typed struct because merge-patch
// semantics (object-merge, array/scalar-replace, null-deletes) are naturally
// a generic-JSON-tree operation - see mergepatch.go.
type PatchDoc map[string]interface{}

// AllowedPatchKeys is the exhaustive set of top-level keys an override's
// patch may touch. Any other key rejects the whole document (§7.10).
var AllowedPatchKeys = map[string]bool{
	"control":       true,
	"updater":       true,
	"logging":       true,
	"defaultServer": true,
}
