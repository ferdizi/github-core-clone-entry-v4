// Raw TCP HTTP/1.1 server: a minimal, low-overhead transport for the same
// dispatch the net/http server uses. net/http allocates a Request, a header
// map, a ResponseWriter wrapper and a bufio pair per request; on this VM
// that costs ~100us per request even for a cached GET, and the four
// workloads are dominated by per-request overhead. The raw server parses
// the request line + headers in place, keeps one bufio pair per connection,
// and writes the response in a single flush.
//
// It speaks only what the kit's clients send: origin-form targets,
// Content-Length or chunked bodies, HTTP/1.1 keep-alive. Anything it
// cannot parse gets a 400 and the connection closed; the dispatch itself
// is unchanged, so conformance is identical to the net/http path.

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
)

// rawRequest is one parsed HTTP request.
type rawRequest struct {
	method    string
	target    string // raw request-target (path + optional query)
	path      string // escaped path (no query)
	rawQuery  string
	host      string
	headers   http.Header
	body      []byte
	keepAlive bool
}

// readRawRequest parses one HTTP/1.1 request from br. It returns io.EOF
// when the peer closed the connection cleanly before a request line.
func readRawRequest(br *bufio.Reader) (*rawRequest, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		// A stray empty line before the request line: tolerate one, then
		// read the real line.
		line, err = br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad request line")
	}
	req := &rawRequest{
		method:  parts[0],
		target:  parts[1],
		headers: make(http.Header),
	}
	version := parts[2]
	// Origin-form target; tolerate an absolute-form target by stripping
	// the scheme and authority.
	target := req.target
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			target = rest[slash:]
		} else {
			target = "/"
		}
	}
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.path = target[:q]
		req.rawQuery = target[q+1:]
	} else {
		req.path = target
	}
	// Headers.
	var lastKey string
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		if hline[0] == ' ' || hline[0] == '\t' {
			// Continuation line: fold into the previous header.
			if lastKey != "" {
				vals := req.headers[lastKey]
				if len(vals) > 0 {
					vals[len(vals)-1] += " " + strings.TrimSpace(hline)
					req.headers[lastKey] = vals
				}
			}
			continue
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			return nil, fmt.Errorf("bad header line")
		}
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(hline[:colon]))
		value := strings.TrimSpace(hline[colon+1:])
		req.headers[key] = append(req.headers[key], value)
		lastKey = key
	}
	req.host = req.headers.Get("Host")
	// Keep-alive: HTTP/1.1 defaults to keep-alive unless Connection:
	// close; HTTP/1.0 defaults to close unless Connection: keep-alive.
	connHeader := strings.ToLower(req.headers.Get("Connection"))
	if version == "HTTP/1.1" {
		req.keepAlive = !strings.Contains(connHeader, "close")
	} else {
		req.keepAlive = strings.Contains(connHeader, "keep-alive")
	}
	// Body.
	if te := strings.ToLower(req.headers.Get("Transfer-Encoding")); strings.Contains(te, "chunked") {
		body, err := readChunkedBody(br)
		if err != nil {
			return nil, err
		}
		req.body = body
	} else if cl := req.headers.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad Content-Length")
		}
		if n > maxBodyBytes {
			return nil, fmt.Errorf("body too large")
		}
		if n > 0 {
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, err
			}
			req.body = body
		} else {
			req.body = []byte{}
		}
	} else {
		req.body = []byte{}
	}
	return req, nil
}

// readChunkedBody reads a chunked transfer-encoded body.
func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimSpace(sizeLine)
		if semi := strings.IndexByte(sizeLine, ';'); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		size, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad chunk size")
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				trailer, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if strings.TrimRight(trailer, "\r\n") == "" {
					break
				}
			}
			return body, nil
		}
		if int64(len(body))+size > maxBodyBytes {
			return nil, fmt.Errorf("body too large")
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		// Trailing CRLF after the chunk.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}

var rawStatusText = map[int]string{
	200: "OK",
	201: "Created",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// writeRawResponse writes one response on bw and flushes. keepAlive selects
// the Connection header.
func writeRawResponse(bw *bufio.Writer, resp forgeResponse, keepAlive bool) error {
	text, ok := rawStatusText[resp.status]
	if !ok {
		text = http.StatusText(resp.status)
		if text == "" {
			text = "OK"
		}
	}
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(resp.status))
	sb.WriteByte(' ')
	sb.WriteString(text)
	sb.WriteString("\r\n")
	// Headers from the response, in a stable order.
	if ct := resp.headers["content-type"]; ct != "" {
		sb.WriteString("Content-Type: ")
		sb.WriteString(ct)
		sb.WriteString("\r\n")
	}
	if rid := resp.headers["x-github-request-id"]; rid != "" {
		sb.WriteString("X-GitHub-Request-Id: ")
		sb.WriteString(rid)
		sb.WriteString("\r\n")
	}
	for k, v := range resp.headers {
		if k == "content-type" || k == "x-github-request-id" {
			continue
		}
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\r\n")
	}
	sb.WriteString("Content-Length: ")
	sb.WriteString(strconv.Itoa(len(resp.body)))
	sb.WriteString("\r\n")
	if keepAlive {
		sb.WriteString("Connection: keep-alive\r\n")
	} else {
		sb.WriteString("Connection: close\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := bw.WriteString(sb.String()); err != nil {
		return err
	}
	if len(resp.body) > 0 {
		if _, err := bw.Write(resp.body); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// handleRawConn serves one TCP connection: read requests, dispatch, write
// responses, until the peer closes or asks to close.
func handleRawConn(s *store, conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 64*1024)
	bw := bufio.NewWriterSize(conn, 64*1024)
	for {
		req, err := readRawRequest(br)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			// A malformed request or an oversized body: answer 400 if we
			// can still write, then close.
			if err.Error() == "body too large" {
				_ = writeRawResponse(bw, jsonError(http.StatusInternalServerError, "Internal Server Error"), false)
			} else {
				_ = writeRawResponse(bw, jsonError(http.StatusBadRequest, "Bad Request"), false)
			}
			return
		}
		resp := dispatch(s, req.method, req.path, req.rawQuery, req.host, req.headers, req.body)
		if err := writeRawResponse(bw, resp, req.keepAlive); err != nil {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

// serveRaw accepts connections on ln and serves them with the raw server.
func serveRaw(s *store, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go handleRawConn(s, conn)
	}
}

var _ = os.Stderr
