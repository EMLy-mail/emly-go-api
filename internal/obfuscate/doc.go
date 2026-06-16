// Package obfuscate produces JSON responses whose *key names* are scrambled
// daily while the *values* stay in clear text. The server writes each value
// under an obfuscated name; the client regenerates the same name locally and
// uses it as a lookup key. No translation map is ever sent over the wire.
//
// SECURITY NOTE: this is obfuscation, NOT encryption. Anyone with access to
// this source or to the client binary can regenerate the name map. It is good
// enough to discourage casual traffic inspection (devtools, proxies); it does
// NOT protect sensitive data from a motivated attacker who controls the client.
//
// SHARED CODE: the Wails client must carry a byte-for-byte identical copy of
// this package (Schema, ObfuscatedName, ShiftFromDateString, Build/Parse and
// the words/syllables tables). This repo is server-only, so the package is
// duplicated rather than imported. If you change anything here that affects
// generated names or the schema, you MUST mirror it in the client or the two
// sides will silently disagree.
package obfuscate
