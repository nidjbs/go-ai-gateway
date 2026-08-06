package apierr

import (
	"encoding/json"
	"net/http"
)

type Body struct {
	Error Detail `json:"error"`
}
type Detail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func Write(w http.ResponseWriter, status int, code, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Body{Error: Detail{Message: message, Type: kind, Code: code}})
}
