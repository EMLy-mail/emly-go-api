package remoteconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Canonical serializes doc into the exact bytes the API stores and serves
// (API design doc §5.3): fixed field order (Go's struct declaration order,
// which encoding/json preserves), no indentation, no trailing newline, map
// keys sorted (encoding/json does this for map[string]T automatically). Two
// documents with the same semantic content produce byte-identical output
// regardless of the key order or whitespace of whatever was originally
// posted, because both are decoded into the same typed struct first.
//
// The returned etag is the lowercase hex SHA-256 of those bytes, unquoted -
// callers that need the HTTP ETag wire form add the surrounding `"` (API
// design doc §5.2).
func Canonical(doc *Document) ([]byte, string) {
	b, err := json.Marshal(doc)
	if err != nil {
		// Document is a plain data struct with no cyclic references and no
		// types json.Marshal can choke on (no channels, funcs, complex) -
		// this is unreachable in practice. Return an empty, clearly-invalid
		// result rather than panicking a request handler.
		return nil, ""
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}
