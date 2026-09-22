// Package response holds the JSON response helpers every handler writes
// through, so that "what an API response looks like" is one decision in one
// place rather than a convention each feature package re-implements.
//
// It is the smallest kind of shared package: HTTP-only, no database, no
// dependency on any feature. Every feature package under internal/ imports it,
// and it imports none of them.
package response

import (
	"encoding/json"
	"net/http"
)

// Error writes a JSON error response with the given status code and message.
func Error(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// OK writes a 200 JSON response with the given payload.
func OK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Created writes a 201 JSON response with the given payload.
func Created(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}
