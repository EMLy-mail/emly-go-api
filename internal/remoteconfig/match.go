package remoteconfig

import (
	"net"
	"strings"
	"time"
)

// Host is the set of per-cycle facts an override's selector is matched
// against (client spec §7.10, §9.4). IPs are the host's local IPv4
// addresses in dotted-decimal form; Now is the clock used to evaluate
// "until" (defaults to time.Now() when the caller leaves it zero, see
// Effective).
type Host struct {
	HWID     string
	Hostname string
	DC       string
	Domain   string
	IPs      []string
	Now      time.Time
}

// Match reports whether sel selects h, per client spec §7.10: "all: true"
// alone matches everyone; otherwise every present key must match (AND
// across keys) with at least one value inside that key matching (OR within
// a list). A selector with no key present never matches - validation
// rejects that shape before Match ever sees it, but Match stays defensive
// rather than panicking on a hand-built Selector.
func Match(sel Selector, h Host) bool {
	if sel.All != nil && *sel.All {
		return true
	}

	matchedAnyKey := false

	if len(sel.HWIDs) > 0 {
		matchedAnyKey = true
		if !containsFold(sel.HWIDs, h.HWID) {
			return false
		}
	}
	if len(sel.Hostnames) > 0 {
		matchedAnyKey = true
		if !containsFold(sel.Hostnames, h.Hostname) {
			return false
		}
	}
	if len(sel.DCs) > 0 {
		matchedAnyKey = true
		if !dcNameMatchesAny(sel.DCs, h.DC) {
			return false
		}
	}
	if len(sel.Subnets) > 0 {
		matchedAnyKey = true
		if !subnetsContainAny(sel.Subnets, h.IPs) {
			return false
		}
	}
	if len(sel.Domains) > 0 {
		matchedAnyKey = true
		if !containsFold(sel.Domains, h.Domain) {
			return false
		}
	}

	return matchedAnyKey
}

func containsFold(list []string, s string) bool {
	if s == "" {
		return false
	}
	for _, item := range list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}

// dcShortName strips a DNS suffix the way sameDCName does client-side:
// "DC-RM2" and "dc-rm2.tregcc.local" are the same DC.
func dcShortName(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return name
}

func dcNameMatchesAny(list []string, dc string) bool {
	if dc == "" {
		return false
	}
	short := dcShortName(dc)
	for _, item := range list {
		if strings.EqualFold(dcShortName(item), short) {
			return true
		}
	}
	return false
}

func subnetsContainAny(cidrs []string, ips []string) bool {
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		for _, ipStr := range ips {
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}
			if network.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// untilExpired reports whether an RFC3339 "until" timestamp is in the past
// relative to now. An unparsable or absent until is treated as "not
// expired" (matching client spec: validation already guaranteed it parses;
// absent/nil means no expiry).
func untilExpired(until *string, now time.Time) bool {
	if until == nil || *until == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, *until)
	if err != nil {
		return false
	}
	return now.After(t)
}

// ResolveSite finds the dcLookupMap entry h belongs to, per client spec
// §7.5: the nearest DC matches the map key AND at least one of h's local
// IPv4 addresses is inside one of the site's subnets; a disabled site
// behaves as unmapped. Returns the site's label and resolver chain
// (baseServer then backupServer in order); when no site matches, nil and
// the single-entry defaultServer chain, mirroring what the client falls
// back to.
//
// If more than one site's subnets could match the same host - a
// misconfiguration overlapping CIDRs across sites, which validation does
// not forbid - which one wins is unspecified (map iteration order).
func ResolveSite(doc *Document, h Host) (site *string, chain []string) {
	for dc, s := range doc.DCLookupMap {
		if !s.Enabled {
			continue
		}
		if !dcNameMatchesAny([]string{dc}, h.DC) {
			continue
		}
		if !subnetsContainAny(s.InternalSubnets, h.IPs) {
			continue
		}
		name := dc
		return &name, append([]string{s.BaseServer}, s.BackupServer...)
	}
	return nil, []string{doc.DefaultServer}
}

// Effective computes the effective document for h: the global document with
// every non-expired, matching (and non-excepted) override's patch applied in
// list order, plus the ids of the overrides that applied. h.Now defaults to
// time.Now() when zero.
func Effective(doc *Document, h Host) (*Document, []string) {
	now := h.Now
	if now.IsZero() {
		now = time.Now()
	}

	effective := doc
	var applied []string

	for _, ov := range doc.Overrides {
		if untilExpired(ov.Until, now) {
			continue
		}
		if !Match(ov.Match, h) {
			continue
		}
		if ov.Except != nil && Match(*ov.Except, h) {
			continue
		}

		patched, err := applyPatchToDocument(effective, ov.Patch)
		if err != nil {
			// A patch that fails to apply here would have failed Parse's
			// dry-run already; skip defensively rather than propagating a
			// panic-worthy state into a preview response.
			continue
		}
		effective = patched
		applied = append(applied, ov.ID)
	}

	return effective, applied
}
