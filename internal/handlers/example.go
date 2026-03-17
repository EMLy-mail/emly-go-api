package handlers

import (
	"encoding/json"
	"io"
	"net/http"
)

var ExampleGet http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "example GET"})
}

var ExamplePost http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"message":  "example POST",
		"received": string(body),
	})
}
