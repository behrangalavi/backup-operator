package ui

import (
	"context"
	"io"
	"net/http"
)

// httpGetCtx / httpPostCtx are the context-carrying stand-ins for http.Get /
// http.Post in tests (noctx): same call shape, same return values.
func httpGetCtx(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func httpPostCtx(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}
