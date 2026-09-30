package remoteconfig

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The products section's rules are a twin of emly-updater's
// internal/policy/products.go (validateProducts): same rules, same problem
// paths, kept equal by the shared fixtures under testdata/remoteconfig.
// Messages may differ between the two sides; paths may not.

// productSlugPattern is internal/productreg's slug rule. It is duplicated
// rather than imported because this package has no HTTP/DB dependency on
// purpose; TestProductSlugRulesMatchRegistry pins the two equal.
var productSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

// reservedProductSlugs is internal/productreg's reserved list plus "emly":
// EMLy is a registry product, but the agent has it built in and configures
// it elsewhere, so the document may not list it.
var reservedProductSlugs = []string{"emly", "updater", "all", "manifest", "releases", "download", "products"}

var windowsAbsPath = regexp.MustCompile(`^[A-Za-z]:\\`)

func hasDotDotSegment(path string) bool {
	return slices.Contains(strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }), "..")
}

func validateProducts(ps map[string]ProductSettings) []Problem {
	var problems []Problem
	add := func(path, msg string) { problems = append(problems, Problem{Path: path, Message: msg}) }

	slugs := make([]string, 0, len(ps))
	for slug := range ps {
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)

	for _, slug := range slugs {
		p, base := ps[slug], "/products/"+slug
		if !productSlugPattern.MatchString(slug) {
			add(base, "not a valid product slug (^[a-z0-9][a-z0-9-]{0,19}$)")
			continue
		}
		if slices.Contains(reservedProductSlugs, slug) {
			add(base, "reserved slug (emly is built into the agent and configured elsewhere)")
			continue
		}
		if name := strings.TrimSpace(p.Name); name == "" || len(name) > 64 {
			add(base+"/name", "is required, at most 64 characters")
		}
		switch p.Channel {
		case "", "stable", "beta":
		default:
			add(base+"/channel", "must be one of: stable, beta")
		}
		if !windowsAbsPath.MatchString(p.InstallDir) || hasDotDotSegment(p.InstallDir) {
			add(base+"/installDir", `must be an absolute Windows path (X:\...) without ".."`)
		}
		if p.ExeName == "" || strings.ContainsAny(p.ExeName, `\/:`) || !strings.HasSuffix(strings.ToLower(p.ExeName), ".exe") {
			add(base+"/exeName", "must be a file name ending in .exe")
		}
		if len(p.Detect) < 1 || len(p.Detect) > 5 {
			add(base+"/detect", "must list 1 to 5 sources")
		}
		for i, d := range p.Detect {
			dp := base + "/detect/" + strconv.Itoa(i)
			switch d.Type {
			case "ini", "file", "exe":
			default:
				add(dp+"/type", "must be one of: ini, file, exe")
			}
			// ':' covers drive-relative paths ("C:foo", which resolve
			// against that drive's current directory, not installDir) and
			// NTFS alternate data streams ("version.txt:x").
			if d.Path == "" || windowsAbsPath.MatchString(d.Path) || strings.HasPrefix(d.Path, `\`) ||
				strings.HasPrefix(d.Path, "/") || hasDotDotSegment(d.Path) || strings.Contains(d.Path, ":") {
				add(dp+"/path", `must be relative to installDir, without ".." or ':'`)
			}
			if d.Type == "ini" {
				if d.Section == "" {
					add(dp+"/section", "is required for ini")
				}
				if d.Key == "" {
					add(dp+"/key", "is required for ini")
				}
			}
		}
		switch p.Installer.Type {
		case "nsis", "inno":
		default:
			add(base+"/installer/type", "must be one of: nsis, inno")
		}
	}
	return problems
}
