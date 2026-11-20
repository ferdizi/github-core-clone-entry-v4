// The HTTP server: one process, one store, four surfaces. Routing order:
// /healthz (no auth), git smart HTTP, REST v3, GraphQL, MCP, then 404 JSON.
// Mirrors src/app.ts. The binary binds an ephemeral port on 127.0.0.1 and
// prints `READY <port>` on stdout; the main.ts launcher reads that line and
// runs the MAKEFUN register/done protocol.
//
// The router is transport-independent (dispatch): both the net/http server
// and the raw TCP server feed it the same parsed request fields.

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

const maxBodyBytes = 64 * 1024 * 1024

// tryIssueCacheHit is the fast path for the repeated single-issue GET: when
// the serialized body is already cached at the repo's current version, answer
// it directly without URL parsing, routing or body parsing. Anything uncached,
// unauthenticated or unmatched returns ok=false and falls through to the
// normal router, which produces the exact same 401/404/200 as before.
func tryIssueCacheHit(s *store, method, escapedPath, rawQuery, host string, headers http.Header) (forgeResponse, bool) {
	if rawQuery != "" {
		return forgeResponse{}, false
	}
	parts := strings.Split(escapedPath, "/")
	if len(parts) != 8 || parts[1] != "api" || parts[2] != "v3" || parts[3] != "repos" || parts[6] != "issues" {
		return forgeResponse{}, false
	}
	if parts[4] == "" || parts[5] == "" {
		return forgeResponse{}, false
	}
	numStr := parts[7]
	for i := 0; i < len(numStr); i++ {
		if numStr[i] < '0' || numStr[i] > '9' {
			return forgeResponse{}, false
		}
	}
	if !checkAuth(headers.Get("Authorization")) {
		return forgeResponse{}, false
	}
	owner, err1 := url.PathUnescape(parts[4])
	repoName, err2 := url.PathUnescape(parts[5])
	if err1 != nil || err2 != nil {
		return forgeResponse{}, false
	}
	if owner != viewerLogin {
		return forgeResponse{}, false
	}
	rep := s.getRepo(owner, repoName)
	if rep == nil {
		return forgeResponse{}, false
	}
	number, err := strconv.Atoi(numStr)
	if err != nil {
		return forgeResponse{}, false
	}
	rep.mu.RLock()
	defer rep.mu.RUnlock()
	var found *issue
	for _, iss := range rep.issues {
		if iss.number == number {
			found = iss
			break
		}
	}
	if found == nil {
		return forgeResponse{}, false
	}
	if host == "" {
		host = "127.0.0.1"
	}
	cached, ok := rep.issueBodyCache["http://"+host+":"+numStr]
	if !ok || cached.version != rep.version {
		return forgeResponse{}, false
	}
	return rawResponse(http.StatusOK, "application/json", cached.body), true
}

// dispatch routes one parsed request to the four surfaces and returns the
// response. It is transport-independent: the net/http handler and the raw
// TCP server both feed it the same fields.
func dispatch(s *store, method, escapedPath, rawQuery, host string, headers http.Header, body []byte) (resp forgeResponse) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[entry] request failed: %v\n", rec)
			resp = jsonError(http.StatusInternalServerError, "Internal Server Error")
		}
	}()
	if method == http.MethodGet {
		if fast, ok := tryIssueCacheHit(s, method, escapedPath, rawQuery, host, headers); ok {
			return fast
		}
	}
	if method == http.MethodGet && escapedPath == "/healthz" {
		return jsonResponse(http.StatusOK, map[string]bool{"ok": true}, nil)
	}
	query, _ := url.ParseQuery(rawQuery)
	if git := handleGit(s, method, escapedPath, query, headers, body); git != nil {
		return *git
	}
	if escapedPath == "/api/graphql" || strings.HasPrefix(escapedPath, "/api/graphql/") {
		return handleGraphql(s, method, escapedPath, headers, body)
	}
	if escapedPath == "/mcp" || strings.HasPrefix(escapedPath, "/mcp/") {
		return handleMcp(s, method, escapedPath, headers, body)
	}
	if escapedPath == "/api/v3" || strings.HasPrefix(escapedPath, "/api/v3/") {
		return handleRest(s, method, escapedPath, rawQuery, host, headers, body)
	}
	return jsonError(http.StatusNotFound, "Not Found")
}

func serve(s *store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		path := r.URL.EscapedPath()
		var body []byte
		if method != http.MethodGet && method != http.MethodHead {
			limited := io.LimitReader(r.Body, maxBodyBytes+1)
			b, err := io.ReadAll(limited)
			if err != nil {
				writeResponse(w, jsonError(http.StatusInternalServerError, "Internal Server Error"))
				return
			}
			if len(b) > maxBodyBytes {
				writeResponse(w, jsonError(http.StatusInternalServerError, "Internal Server Error"))
				return
			}
			body = b
		} else {
			body = []byte{}
		}
		writeResponse(w, dispatch(s, method, path, r.URL.RawQuery, r.Host, r.Header, body))
	})
}

func main() {
	s := newStore()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen failed: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("READY %d\n", port)
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-done
		_ = ln.Close()
	}()
	// Raw TCP server (see rawhttp.go); the net/http serve() above stays as
	// the fallback transport.
	if err := serveRaw(s, ln); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] serve failed: %v\n", err)
		os.Exit(1)
	}
}
