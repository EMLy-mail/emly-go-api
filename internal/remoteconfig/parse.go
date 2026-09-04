package remoteconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxDocumentBytes is the size cap enforced before parsing, matching the
// client (client spec §8 step 1).
const MaxDocumentBytes = 1 << 20 // 1 MiB

var semverPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}[A-Za-z0-9.\-]*$`)

var validLoggingLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
var validAppModes = map[string]bool{"normal": true, "readonly": true, "maintenance": true}
var validChannelOverride = map[string]bool{"stable": true, "beta": true}

// Parse decodes and validates a document per client spec §7-8: structural
// typing, referential integrity, override dry-run. It returns every Problem
// found (possibly none), plus the decoded Document for the caller's
// convenience even when problems are present - a caller that only wants the
// bytes when valid must still check len(problems) == 0 itself.
//
// revision and generatedAt are validated for shape only when present/non-
// zero: an operator-submitted document is expected to omit both (the server
// assigns them, see the API design doc §7.2), while a fully-formed document
// (mirror replication, a stored revision) carries both. Requiring "must be
// present" here would reject the former; the caller enforces presence where
// its own contract requires it.
func Parse(data []byte) (*Document, []Problem) {
	if len(data) > MaxDocumentBytes {
		return nil, []Problem{problemf("", "document exceeds the %d byte size cap", MaxDocumentBytes)}
	}

	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, []Problem{problemf("", "invalid JSON: %s", err.Error())}
	}

	var problems []Problem
	problems = append(problems, validateTop(&doc)...)
	problems = append(problems, validateServers(&doc)...)
	problems = append(problems, validateIPCProtocol(doc.IPCProtocol)...)
	problems = append(problems, validateDCLookupMap(&doc)...)
	problems = append(problems, validateHostIntegrity(doc.HostIntegrity)...)
	problems = append(problems, validateControl(doc.Control)...)
	problems = append(problems, validateUpdater(doc.Updater)...)
	problems = append(problems, validateLogging(doc.Logging)...)
	problems = append(problems, validateOverridesShape(&doc)...)

	// Dry-run every override against an all-matching synthetic host, so a
	// patch that would produce an invalid value is caught here rather than
	// on the fleet. Only meaningful once the shape checks above passed -
	// merge-patching against a document we already know is malformed would
	// just produce confusing secondary errors.
	if len(problems) == 0 {
		problems = append(problems, dryRunOverrides(&doc)...)
	}

	return &doc, problems
}

func validateTop(doc *Document) []Problem {
	var problems []Problem
	if doc.SchemaVersion != 1 {
		problems = append(problems, problemf("/schemaVersion", "must equal 1, got %d", doc.SchemaVersion))
	}
	if doc.Revision < 0 {
		problems = append(problems, problemf("/revision", "must be >= 0"))
	}
	if doc.GeneratedAt != "" {
		if _, err := time.Parse(time.RFC3339, doc.GeneratedAt); err != nil {
			problems = append(problems, problemf("/generatedAt", "must be RFC3339: %s", err.Error()))
		}
	}
	return problems
}

func validateServers(doc *Document) []Problem {
	var problems []Problem
	if len(doc.Servers) == 0 {
		problems = append(problems, problemf("/servers", "must have at least one entry"))
	}
	for name, base := range doc.Servers {
		if err := validateServerURL(base); err != nil {
			problems = append(problems, problemf("/servers/"+name, "%s", err.Error()))
		}
	}
	if doc.DefaultServer == "" {
		problems = append(problems, problemf("/defaultServer", "is required"))
	} else if _, ok := doc.Servers[doc.DefaultServer]; !ok {
		problems = append(problems, problemf("/defaultServer", "references unknown server %q", doc.DefaultServer))
	}
	return problems
}

// validateServerURL enforces: absolute http(s) URL, no query string, no
// trailing slash (client spec §7.3 - the client appends its own paths).
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %s", err.Error())
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must be an absolute URL")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("must not carry a query string")
	}
	if strings.HasSuffix(raw, "/") {
		return fmt.Errorf("must not have a trailing slash")
	}
	return nil
}

func validateSemverPtr(path string, v *string) []Problem {
	if v == nil || *v == "" {
		return nil
	}
	if !semverPattern.MatchString(*v) {
		return []Problem{problemf(path, "must be a semver string, got %q", *v)}
	}
	return nil
}

func validateIPCProtocol(ipc *IPCProtocol) []Problem {
	if ipc == nil {
		return nil
	}
	var problems []Problem
	for key, v := range ipc.Versions {
		if _, err := strconv.Atoi(key); err != nil {
			problems = append(problems, problemf("/ipcProtocol/versions/"+key, "key must be a decimal integer"))
		}
		problems = append(problems, validateSemverPtr("/ipcProtocol/versions/"+key+"/updater/min", v.Updater.Min)...)
		problems = append(problems, validateSemverPtr("/ipcProtocol/versions/"+key+"/updater/max", v.Updater.Max)...)
		problems = append(problems, validateSemverPtr("/ipcProtocol/versions/"+key+"/emly/min", v.EMLy.Min)...)
		problems = append(problems, validateSemverPtr("/ipcProtocol/versions/"+key+"/emly/max", v.EMLy.Max)...)
	}
	defaultKey := strconv.Itoa(ipc.DefaultVersion)
	v, ok := ipc.Versions[defaultKey]
	if !ok {
		problems = append(problems, problemf("/ipcProtocol/defaultVersion", "references unknown protocol version %d", ipc.DefaultVersion))
	} else if !v.Enabled {
		problems = append(problems, problemf("/ipcProtocol/defaultVersion", "protocol version %d is disabled", ipc.DefaultVersion))
	}
	return problems
}

func validateDCLookupMap(doc *Document) []Problem {
	var problems []Problem
	for dc, site := range doc.DCLookupMap {
		path := "/dcLookupMap/" + dc
		if len(site.InternalSubnets) == 0 {
			problems = append(problems, problemf(path+"/internalSubnets", "must have at least one entry"))
		}
		for _, cidr := range site.InternalSubnets {
			if err := validateCIDR(cidr); err != nil {
				problems = append(problems, problemf(path+"/internalSubnets", "%q: %s", cidr, err.Error()))
			}
		}
		if site.BaseServer == "" {
			problems = append(problems, problemf(path+"/baseServer", "is required"))
		} else if _, ok := doc.Servers[site.BaseServer]; !ok {
			problems = append(problems, problemf(path+"/baseServer", "references unknown server %q", site.BaseServer))
		}
		for i, ref := range site.BackupServer {
			if _, ok := doc.Servers[ref]; !ok {
				problems = append(problems, problemf(fmt.Sprintf("%s/backupServer/%d", path, i), "references unknown server %q", ref))
			}
		}
	}
	return problems
}

// validateCIDR enforces IPv4-only, per client spec §7.5.
func validateCIDR(cidr string) error {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	if ip.To4() == nil {
		return fmt.Errorf("IPv6 CIDRs are not supported")
	}
	return nil
}

func validateHostIntegrity(hi *HostIntegrity) []Problem {
	// Free-form string lists; nothing to validate beyond the shape the
	// struct already enforces at decode time.
	return nil
}

func validateGateUntil(path string, until *string) []Problem {
	if until == nil || *until == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, *until); err != nil {
		return []Problem{problemf(path, "must be RFC3339: %s", err.Error())}
	}
	return nil
}

func validateControl(c *Control) []Problem {
	if c == nil {
		return nil
	}
	var problems []Problem
	problems = append(problems, validateGateUntil("/control/updater/until", c.Updater.Until)...)
	problems = append(problems, validateGateUntil("/control/app/until", c.App.Until)...)
	if c.App.Mode != "" && !validAppModes[c.App.Mode] {
		problems = append(problems, problemf("/control/app/mode", "must be one of: normal, readonly, maintenance, got %q", c.App.Mode))
	}
	return problems
}

func validateUpdater(u *UpdaterTuning) []Problem {
	if u == nil {
		return nil
	}
	var problems []Problem
	if u.PollIntervalMinutes != nil && *u.PollIntervalMinutes < 1 {
		problems = append(problems, problemf("/updater/pollIntervalMinutes", "must be >= 1"))
	}
	if u.ChannelOverride != nil && *u.ChannelOverride != "" && !validChannelOverride[*u.ChannelOverride] {
		problems = append(problems, problemf("/updater/channelOverride", "must be one of: stable, beta, or null"))
	}
	if u.CriticalWarning != nil && u.CriticalWarning.Seconds < 0 {
		problems = append(problems, problemf("/updater/criticalWarning/seconds", "must be >= 0"))
	}
	if u.DCLookupRetry != nil {
		if u.DCLookupRetry.Attempts < 0 {
			problems = append(problems, problemf("/updater/dcLookupRetry/attempts", "must be >= 0"))
		}
		if u.DCLookupRetry.DelaySeconds < 0 {
			problems = append(problems, problemf("/updater/dcLookupRetry/delaySeconds", "must be >= 0"))
		}
	}
	if u.Resolver != nil {
		if u.Resolver.Attempts < 1 {
			problems = append(problems, problemf("/updater/resolver/attempts", "must be >= 1"))
		}
		if u.Resolver.BaseBackoffSeconds < 0 {
			problems = append(problems, problemf("/updater/resolver/baseBackoffSeconds", "must be >= 0"))
		}
	}
	return problems
}

func validateLogging(l *Logging) []Problem {
	if l == nil {
		return nil
	}
	var problems []Problem
	if l.Level != "" && !validLoggingLevels[l.Level] {
		problems = append(problems, problemf("/logging/level", "must be one of: debug, info, warn, error, got %q", l.Level))
	}
	if l.MaxSizeMB != 0 && (l.MaxSizeMB < 1 || l.MaxSizeMB > 100) {
		problems = append(problems, problemf("/logging/maxSizeMB", "must be in [1, 100]"))
	}
	if l.Backups < 0 || l.Backups > 50 {
		problems = append(problems, problemf("/logging/backups", "must be in [0, 50]"))
	}
	return problems
}

func validateSelector(path string, sel Selector, allowAll bool) []Problem {
	nonEmptyLists := 0
	if len(sel.HWIDs) > 0 {
		nonEmptyLists++
	}
	if len(sel.Hostnames) > 0 {
		nonEmptyLists++
	}
	if len(sel.DCs) > 0 {
		nonEmptyLists++
	}
	if len(sel.Domains) > 0 {
		nonEmptyLists++
	}
	if len(sel.Subnets) > 0 {
		nonEmptyLists++
	}

	var problems []Problem
	for _, cidr := range sel.Subnets {
		if err := validateCIDR(cidr); err != nil {
			problems = append(problems, problemf(path+"/subnets", "%q: %s", cidr, err.Error()))
		}
	}

	if sel.All != nil {
		if !allowAll {
			problems = append(problems, problemf(path+"/all", "\"all\" is not allowed here"))
			return problems
		}
		if !*sel.All {
			problems = append(problems, problemf(path+"/all", "must be true if present"))
		}
		if nonEmptyLists > 0 {
			problems = append(problems, problemf(path, "\"all\" must be the only key"))
		}
		return problems
	}

	if nonEmptyLists == 0 {
		problems = append(problems, problemf(path, "must be either {\"all\": true} or have at least one non-empty list"))
	}
	return problems
}

func validateOverridesShape(doc *Document) []Problem {
	var problems []Problem
	seen := make(map[string]bool, len(doc.Overrides))
	for i, ov := range doc.Overrides {
		path := fmt.Sprintf("/overrides/%d", i)
		if ov.ID == "" {
			problems = append(problems, problemf(path+"/id", "is required"))
		} else if seen[ov.ID] {
			problems = append(problems, problemf(path+"/id", "duplicate override id %q", ov.ID))
		} else {
			seen[ov.ID] = true
		}

		problems = append(problems, validateSelector(path+"/match", ov.Match, true)...)
		if ov.Except != nil {
			problems = append(problems, validateSelector(path+"/except", *ov.Except, false)...)
		}
		problems = append(problems, validateGateUntil(path+"/until", ov.Until)...)

		for key := range ov.Patch {
			if !AllowedPatchKeys[key] {
				problems = append(problems, problemf(path+"/patch/"+key, "patch may only touch control, updater, logging, defaultServer"))
			}
		}
	}
	return problems
}

// dryRunOverrides applies every override's patch to a copy of the global
// document and re-validates the patched sub-sections, catching a patch that
// would produce an invalid effective value (client spec §7.10, §8 step 5).
func dryRunOverrides(doc *Document) []Problem {
	var problems []Problem
	for i, ov := range doc.Overrides {
		path := fmt.Sprintf("/overrides/%d/patch", i)

		patched, err := applyPatchToDocument(doc, ov.Patch)
		if err != nil {
			problems = append(problems, problemf(path, "failed to apply: %s", err.Error()))
			continue
		}

		if patched.DefaultServer != "" {
			if _, ok := doc.Servers[patched.DefaultServer]; !ok {
				problems = append(problems, problemf(path+"/defaultServer", "references unknown server %q", patched.DefaultServer))
			}
		}
		problems = append(problems, reprefix(path+"/control", validateControl(patched.Control))...)
		problems = append(problems, reprefix(path+"/updater", validateUpdater(patched.Updater))...)
		problems = append(problems, reprefix(path+"/logging", validateLogging(patched.Logging))...)
	}
	return problems
}

// reprefix rewrites the paths produced by re-running a global-section
// validator over a patched sub-document so the caller can tell "this problem
// came from override 2's patch" apart from "this problem is in the global
// document" (both would otherwise report e.g. "/updater/pollIntervalMinutes").
func reprefix(prefix string, problems []Problem) []Problem {
	if len(problems) == 0 {
		return nil
	}
	out := make([]Problem, len(problems))
	for i, p := range problems {
		out[i] = Problem{Path: prefix + strings.TrimPrefix(p.Path, sectionOf(p.Path)), Message: p.Message}
	}
	return out
}

// sectionOf returns the leading "/control", "/updater" or "/logging" segment
// of a path produced by validateControl/validateUpdater/validateLogging, so
// reprefix can strip it before substituting the override-scoped prefix.
func sectionOf(path string) string {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return "/" + parts[1]
}
