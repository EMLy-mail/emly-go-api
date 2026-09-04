package remoteconfig

import "fmt"

// Problem is one validation failure. Path is a JSON-pointer-style location
// ("/servers/srv-x", "/overrides/1/patch") so a dashboard can show every
// problem next to the field that caused it; Message is human-readable.
//
// Parse returns every Problem it finds, not just the first, so an operator
// (or the client's log line for event 902) sees the whole picture in one
// pass rather than fixing one error at a time.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func problemf(path, format string, args ...interface{}) Problem {
	return Problem{Path: path, Message: fmt.Sprintf(format, args...)}
}
