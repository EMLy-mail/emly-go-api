package obfuscate

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// stream is a deterministic, platform-independent PRNG seeded by a string.
// It draws bytes from SHA-256(seed#counter) so that the same seed yields the
// same sequence on every OS and Go version (math/rand makes no such promise).
type stream struct {
	seed    string
	counter uint64
	buf     []byte
}

func (s *stream) next() uint32 {
	if len(s.buf) < 4 {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", s.seed, s.counter)))
		s.counter++
		s.buf = append(s.buf, h[:]...)
	}
	v := binary.BigEndian.Uint32(s.buf[:4])
	s.buf = s.buf[4:]
	return v
}

func (s *stream) intn(n int) int { return int(s.next() % uint32(n)) }

// ObfuscatedName maps (fieldID, shift) to a stable, human-readable but
// semantically meaningless name. The same pair always yields the same name.
// It mixes three styles: camelCase words, snake_case words, and an invented
// pronounceable word built from syllables.
func ObfuscatedName(fieldID string, shift int) string {
	s := &stream{seed: fmt.Sprintf("%s|%d", fieldID, shift)}
	switch s.intn(3) {
	case 0: // camelCase, e.g. seedBuildJsonMutex
		n := 2 + s.intn(3)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			w := words[s.intn(len(words))]
			if i == 0 {
				sb.WriteString(w)
			} else {
				sb.WriteString(capitalize(w))
			}
		}
		return sb.String()
	case 1: // snake_case, e.g. use_void_requiem_core
		n := 2 + s.intn(3)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = words[s.intn(len(words))]
		}
		return strings.Join(parts, "_")
	default: // invented word, e.g. ninkilim
		n := 3 + s.intn(2)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteString(syllables[s.intn(len(syllables))])
		}
		return sb.String()
	}
}

func capitalize(w string) string {
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// words is a generic, tech-sounding wordlist. Order matters: it is part of the
// shared contract with the client and must never be reordered or trimmed
// (appending is also a breaking change because indices shift via modulo).
var words = []string{
	"seed", "build", "json", "mutex", "void", "requiem", "core", "use",
	"flux", "node", "quartz", "ember", "cipher", "vector", "raster", "delta",
	"prism", "shard", "atlas", "vertex", "kernel", "buffer", "stream", "anchor",
	"forge", "relay", "beacon", "cobalt", "onyx", "saffron", "ivory", "umber",
	"glyph", "rune", "token", "lattice", "matrix", "pulse", "drift", "spark",
	"cascade", "harbor", "meadow", "canyon", "summit", "tundra", "zephyr", "willow",
	"orbit", "comet", "nebula", "quasar", "photon", "lumen", "facet", "helix",
}

// syllables is a list of pronounceable fragments used to build invented words.
// Same contract as words: never reorder or trim.
var syllables = []string{
	"nin", "ki", "lim", "ta", "ro", "su", "va", "mo",
	"len", "dor", "fa", "sho", "bel", "nor", "tu", "wen",
	"qua", "zir", "pol", "mar", "cel", "vio", "ran", "tos",
	"mel", "dra", "fen", "lor", "vex", "nim", "kor", "sal",
	"thu", "bra", "gon", "rel", "phi", "ano", "tir", "uss",
	"oba", "yel", "wim", "hax", "jor", "lus", "pim", "rho",
	"sed", "vun", "cae", "dol",
}
