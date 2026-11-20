// Small response helpers shared by every surface. Every reply is JSON (never
// HTML) and carries a non-empty X-GitHub-Request-Id, per the protocol checks.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type forgeResponse struct {
	status  int
	headers map[string]string
	body    []byte
}

var (
	requestMu      sync.Mutex
	requestCounter int
)

func nextRequestID() string {
	requestMu.Lock()
	defer requestMu.Unlock()
	requestCounter++
	return fmt.Sprintf("forge-%x", requestCounter)
}

func jsonResponse(status int, value any, extraHeaders ...map[string]string) forgeResponse {
	body, err := json.Marshal(value)
	if err != nil {
		body = []byte(`{"message":"Internal Server Error"}`)
		status = http.StatusInternalServerError
	}
	headers := map[string]string{
		"content-type":        "application/json",
		"x-github-request-id": nextRequestID(),
	}
	for _, extra := range extraHeaders {
		for k, v := range extra {
			headers[k] = v
		}
	}
	return forgeResponse{status: status, headers: headers, body: body}
}

func jsonError(status int, message string) forgeResponse {
	return jsonResponse(status, map[string]string{"message": message})
}

func rawResponse(status int, contentType string, body []byte) forgeResponse {
	return forgeResponse{
		status: status,
		headers: map[string]string{
			"content-type":        contentType,
			"x-github-request-id": nextRequestID(),
		},
		body: body,
	}
}

// writeResponse sends a forgeResponse on the Go http server.
func writeResponse(w http.ResponseWriter, resp forgeResponse) {
	for k, v := range resp.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.status)
	if len(resp.body) > 0 {
		_, _ = w.Write(resp.body)
	}
}
