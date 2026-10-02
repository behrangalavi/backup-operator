package ui

import ()

type apiResponse struct {
	OK bool `json:"ok"`
	// Code is a stable machine-readable error tag — see errors_api.go for
	// the catalogue. Frontend uses it to pick toast severity and (later)
	// localised messages without parsing Message text.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Name    string `json:"name,omitempty"`
}
