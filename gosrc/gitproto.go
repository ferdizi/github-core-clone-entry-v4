// git smart HTTP: info/refs, git-upload-pack (protocol v0 and v2) and
// git-receive-pack (v0 only), over the shared store. Plain-object packs only;
// the suite's client writes plain packs, and the server answers with plain
// packs. Every update is checked for fast-forward before it is applied.
//
// Locking: upload-pack and info/refs take RLock for the read + compute, then
// Lock briefly to record a cache entry (only when the repo version is still
// the one the body was computed from). Receive-pack takes Lock throughout.

package main

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	uploadAd      = "application/x-git-upload-pack-advertisement"
	uploadResult  = "application/x-git-upload-pack-result"
	receiveAd     = "application/x-git-receive-pack-advertisement"
	receiveResult = "application/x-git-receive-pack-result"
)

type refLine struct {
	oid       string
	name      string
	peeled    string
	hasPeeled bool
}

// advertisedRefs returns all advertised refs, sorted: HEAD first, then
// branches, then tags. Caller must hold at least RLock.
func advertisedRefs(r *repo) []refLine {
	var refs []refLine
	if defaultTip, ok := r.refs["refs/heads/"+r.defaultBranch]; ok {
		refs = append(refs, refLine{oid: defaultTip, name: "HEAD"})
	} else {
		refs = append(refs, refLine{oid: zeroOID, name: "HEAD"})
	}
	var names []string
	for name := range r.refs {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		oid := r.refs[name]
		ref := refLine{oid: oid, name: name}
		if strings.HasPrefix(name, "refs/tags/") {
			if object, ok := r.objects[oid]; ok && object.typ == objTag {
				ref.peeled = parseTag(object.data).object
				ref.hasPeeled = true
			}
		}
		refs = append(refs, ref)
	}
	return refs
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func v0Capabilities(service string, r *repo) string {
	common := "agent=" + gitAgent
	if service == "git-upload-pack" {
		return fmt.Sprintf("side-band-64k symref=HEAD:refs/heads/%s shallow thin-pack ofs-delta %s", r.defaultBranch, common)
	}
	return "report-status side-band-64k delete-refs ofs-delta " + common
}

// v0Advertisement builds the v0 advertisement body, with the "# service="
// preamble. Caller must hold at least RLock.
func v0Advertisement(service string, r *repo) []byte {
	refs := advertisedRefs(r)
	caps := v0Capabilities(service, r)
	var parts [][]byte
	parts = append(parts, encodePktLines([]pktLineItem{
		{payload: []byte("# service=" + service + "\n")},
		{flush: true},
	}))
	var lines []pktLineItem
	if len(refs) == 0 {
		lines = append(lines, pktLineItem{payload: []byte(zeroOID + " capabilities^{}\x00" + caps + "\n")})
	} else {
		for i, ref := range refs {
			first := ""
			if i == 0 {
				first = "\x00" + caps
			}
			lines = append(lines, pktLineItem{payload: []byte(ref.oid + " " + ref.name + first + "\n")})
			if ref.hasPeeled {
				lines = append(lines, pktLineItem{payload: []byte(ref.peeled + " " + ref.name + "^{}\n")})
			}
		}
	}
	lines = append(lines, pktLineItem{flush: true})
	parts = append(parts, encodePktLines(lines))
	return bytes.Join(parts, nil)
}

// v2Advertisement builds the v2 capability advertisement (no service
// preamble).
func v2Advertisement() []byte {
	return encodePktLines([]pktLineItem{
		{payload: []byte("version 2\n")},
		{payload: []byte("ls-refs=unborn\n")},
		{payload: []byte("fetch=shallow\n")},
		{payload: []byte("object-format=sha1\n")},
		{payload: []byte("agent=" + gitAgent + "\n")},
		{flush: true},
	})
}

// sidebandPack splits packfile bytes into sideband band-1 packets.
func sidebandPack(pack []byte) [][]byte {
	const chunk = 65500
	var packets [][]byte
	for pos := 0; pos < len(pack); pos += chunk {
		end := pos + chunk
		if end > len(pack) {
			end = len(pack)
		}
		p := make([]byte, 0, 1+end-pos)
		p = append(p, 1)
		p = append(p, pack[pos:end]...)
		packets = append(packets, p)
	}
	if len(packets) == 0 {
		packets = append(packets, []byte{1})
	}
	return packets
}

// fetchKeyV2 builds the cache key for a v2 ls-refs/fetch argument list
// without JSON encoding: the lines are joined with U+001F, a byte that
// never appears in pkt-line payloads or the \x01/\x00 control markers, so
// the join is unambiguous. jsonStringify did the same job but allocated a
// buffer and ran the JSON encoder on every fetch; this is the hot path for
// the clone workload (ls-refs + fetch per clone).
func fetchKeyV2(lines []string) string {
	return strings.Join(lines, "\x1f")
}

// gitAd returns a cached advertisement buffer for this repo version, computed
// once. It takes RLock for the read + compute and Lock briefly to record.
func gitAd(r *repo, key string, compute func() []byte) []byte {
	r.mu.RLock()
	if entry, ok := r.fetchCache[key]; ok && entry.version == r.version {
		body := entry.body
		r.mu.RUnlock()
		return body
	}
	body := compute()
	ver := r.version
	r.mu.RUnlock()
	r.mu.Lock()
	if r.version == ver {
		if len(r.fetchCache) > 64 {
			r.fetchCache = make(map[string]cachedBody)
		}
		r.fetchCache[key] = cachedBody{version: ver, body: body}
	}
	r.mu.Unlock()
	return body
}

type inflightCall struct {
	done chan struct{}
	resp forgeResponse
}

// fetchCached serves a cached git response, computing it once per key even
// under concurrent identical requests (singleflight): the first caller
// computes, the rest wait and share its result. compute runs under RLock and
// returns the repo version it observed; only a 200 at an unchanged version
// is stored in fetchCache.
func fetchCached(r *repo, key string, compute func() (forgeResponse, int)) forgeResponse {
	r.mu.RLock()
	if entry, ok := r.fetchCache[key]; ok && entry.version == r.version {
		b := entry.body
		r.mu.RUnlock()
		return rawResponse(200, uploadResult, b)
	}
	r.mu.RUnlock()

	r.sfMu.Lock()
	if call, ok := r.inflight[key]; ok {
		r.sfMu.Unlock()
		<-call.done
		r.mu.RLock()
		if entry, ok := r.fetchCache[key]; ok && entry.version == r.version {
			b := entry.body
			r.mu.RUnlock()
			return rawResponse(200, uploadResult, b)
		}
		r.mu.RUnlock()
		return call.resp
	}
	call := &inflightCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.sfMu.Unlock()

	resp, ver := compute()
	if resp.status == 200 {
		r.mu.Lock()
		if r.version == ver {
			if len(r.fetchCache) > 64 {
				r.fetchCache = make(map[string]cachedBody)
			}
			r.fetchCache[key] = cachedBody{version: ver, body: resp.body}
		}
		r.mu.Unlock()
	}
	call.resp = resp
	close(call.done)
	r.sfMu.Lock()
	delete(r.inflight, key)
	r.sfMu.Unlock()
	return resp
}

type fetchRequest struct {
	wants   []string
	haves   []string
	shallow []string
	depth   *int
}

// ackLines builds the ACK/NAK + ready lines for a fetch. Caller must hold at
// least RLock.
func ackLines(r *repo, wants, haves []string) []string {
	var lines []string
	if len(haves) == 0 {
		lines = append(lines, "NAK\n")
	} else {
		acked := false
		for _, have := range haves {
			if have == zeroOID {
				continue
			}
			hit := false
			for _, w := range wants {
				if w == have {
					hit = true
					break
				}
			}
			if !hit {
				for _, want := range wants {
					if isAncestor(r, have, want) {
						hit = true
						break
					}
				}
			}
			if hit {
				lines = append(lines, "ACK "+have+"\n")
				acked = true
			}
		}
		if !acked {
			lines = append(lines, "NAK\n")
		}
	}
	lines = append(lines, "ready\n")
	return lines
}

func handleUploadPackV2(r *repo, body []byte) forgeResponse {
	packets, err := decodePktLines(body)
	if err != nil {
		return jsonError(400, "malformed pkt-line stream")
	}
	var lines []string
	for _, p := range packets {
		switch p.kind {
		case pktData:
			lines = append(lines, string(p.payload))
		case pktDelimKind:
			lines = append(lines, "\x01")
		default:
			lines = append(lines, "\x00")
		}
	}
	command := ""
	if len(lines) > 0 {
		command = strings.TrimSuffix(lines[0], "\n")
	}
	if command == "command=ls-refs" {
		key := "v2:ls-refs:" + fetchKeyV2(lines[1:])
		return fetchCached(r, key, func() (forgeResponse, int) {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return lsRefsV2(r, lines[1:]), r.version
		})
	}
	if command == "command=fetch" {
		key := "v2:fetch:" + fetchKeyV2(lines[1:])
		return fetchCached(r, key, func() (forgeResponse, int) {
			r.mu.RLock()
			defer r.mu.RUnlock()
			return fetchV2(r, lines[1:]), r.version
		})
	}
	return jsonError(400, "unknown v2 command "+strconv.Quote(command))
}

func lsRefsV2(r *repo, lines []string) forgeResponse {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	pastDelim := false
	for _, raw := range lines {
		if raw == "\x01" {
			pastDelim = true
			continue
		}
		if raw == "\x00" {
			break
		}
		line := strings.TrimSuffix(raw, "\n")
		if !pastDelim {
			continue
		}
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	var out []pktLineItem
	refs := advertisedRefs(r)
	empty := len(r.refs) == 0
	if empty {
		if unborn {
			out = append(out, pktLineItem{payload: []byte("unborn HEAD symref-target:refs/heads/" + r.defaultBranch + "\n")})
		}
	} else {
		for _, ref := range refs {
			if len(prefixes) > 0 {
				if ref.name == "HEAD" {
					continue
				}
				matched := false
				for _, p := range prefixes {
					if strings.HasPrefix(ref.name, p) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
				peeled := ""
				if peel && ref.hasPeeled {
					peeled = " peeled:" + ref.peeled
				}
				out = append(out, pktLineItem{payload: []byte(ref.oid + " " + ref.name + peeled + "\n")})
				continue
			}
			if ref.name == "HEAD" {
				sym := ""
				if symrefs {
					sym = " symref-target:refs/heads/" + r.defaultBranch
				}
				out = append(out, pktLineItem{payload: []byte(ref.oid + " HEAD" + sym + "\n")})
			} else {
				peeled := ""
				if peel && ref.hasPeeled {
					peeled = " peeled:" + ref.peeled
				}
				out = append(out, pktLineItem{payload: []byte(ref.oid + " " + ref.name + peeled + "\n")})
			}
		}
	}
	out = append(out, pktLineItem{flush: true})
	return rawResponse(200, uploadResult, encodePktLines(out))
}

func fetchV2(r *repo, lines []string) forgeResponse {
	req := fetchRequest{}
	pastDelim := false
	for _, raw := range lines {
		if raw == "\x01" {
			pastDelim = true
			continue
		}
		if raw == "\x00" {
			break
		}
		line := strings.TrimSuffix(raw, "\n")
		if !pastDelim {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			req.wants = append(req.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			req.haves = append(req.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			req.shallow = append(req.shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.ParseFloat(strings.TrimSpace(line[7:]), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
				d := int(n)
				req.depth = &d
			}
		}
	}
	depth := -1
	if req.depth != nil {
		depth = *req.depth
	}
	plan := planFetch(r, req.wants, req.haves, depth, req.shallow)
	pack := writePack(plan.objects)
	var parts [][]byte
	ackItems := []pktLineItem{{payload: []byte("acknowledgments\n")}}
	for _, l := range ackLines(r, req.wants, req.haves) {
		ackItems = append(ackItems, pktLineItem{payload: []byte(l)})
	}
	ackItems = append(ackItems, pktLineItem{delim: true})
	parts = append(parts, encodePktLines(ackItems))
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		var shallowItems []pktLineItem
		shallowItems = append(shallowItems, pktLineItem{payload: []byte("shallow-info\n")})
		for _, oid := range plan.shallow {
			shallowItems = append(shallowItems, pktLineItem{payload: []byte("shallow " + oid + "\n")})
		}
		for _, oid := range plan.unshallow {
			shallowItems = append(shallowItems, pktLineItem{payload: []byte("unshallow " + oid + "\n")})
		}
		shallowItems = append(shallowItems, pktLineItem{delim: true})
		parts = append(parts, encodePktLines(shallowItems))
	}
	packItems := []pktLineItem{{payload: []byte("packfile\n")}}
	for _, packet := range sidebandPack(pack) {
		packItems = append(packItems, pktLineItem{payload: packet})
	}
	packItems = append(packItems, pktLineItem{flush: true})
	parts = append(parts, encodePktLines(packItems))
	return rawResponse(200, uploadResult, bytes.Join(parts, nil))
}

func handleUploadPackV0(r *repo, body []byte) forgeResponse {
	packets, err := decodePktLines(body)
	if err != nil {
		return jsonError(400, "malformed pkt-line stream")
	}
	req := fetchRequest{}
	section := 0
	for _, packet := range packets {
		if packet.kind == pktFlushKind {
			section++
			continue
		}
		if packet.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.payload), "\n")
		if section == 0 {
			switch {
			case strings.HasPrefix(line, "want "):
				rest := line[5:]
				if nul := strings.IndexByte(rest, 0); nul >= 0 {
					rest = rest[:nul]
				}
				req.wants = append(req.wants, strings.TrimSpace(rest))
			case strings.HasPrefix(line, "shallow "):
				req.shallow = append(req.shallow, strings.TrimSpace(line[8:]))
			case strings.HasPrefix(line, "deepen "):
				if n, err := strconv.ParseFloat(strings.TrimSpace(line[7:]), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
					d := int(n)
					req.depth = &d
				}
			}
		} else if strings.HasPrefix(line, "have ") {
			req.haves = append(req.haves, strings.TrimSpace(line[5:]))
		}
	}
	if len(req.wants) == 0 {
		return jsonError(400, "no want lines")
	}
	depthStr := ""
	if req.depth != nil {
		depthStr = strconv.Itoa(*req.depth)
	}
	cacheKey := "v0:fetch:" + strings.Join(req.wants, ",") + "|" + strings.Join(req.haves, ",") + "|" + strings.Join(req.shallow, ",") + "|" + depthStr
	return fetchCached(r, cacheKey, func() (forgeResponse, int) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		depth := -1
		if req.depth != nil {
			depth = *req.depth
		}
		plan := planFetch(r, req.wants, req.haves, depth, req.shallow)
		pack := writePack(plan.objects)
		var items []pktLineItem
		for _, l := range ackLines(r, req.wants, req.haves) {
			items = append(items, pktLineItem{payload: []byte(l)})
		}
		for _, oid := range plan.shallow {
			items = append(items, pktLineItem{payload: []byte("shallow " + oid + "\n")})
		}
		for _, oid := range plan.unshallow {
			items = append(items, pktLineItem{payload: []byte("unshallow " + oid + "\n")})
		}
		for _, packet := range sidebandPack(pack) {
			items = append(items, pktLineItem{payload: packet})
		}
		items = append(items, pktLineItem{flush: true})
		return rawResponse(200, uploadResult, encodePktLines(items)), r.version
	})
}

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

func handleReceivePack(r *repo, body []byte) forgeResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	packets, packStart, err := decodePktPrefix(body)
	if err != nil {
		return jsonError(400, "malformed pkt-line stream")
	}
	var commands []pushCommand
	for _, packet := range packets {
		if packet.kind == pktFlushKind {
			break
		}
		if packet.kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(packet.payload), "\n")
		head := text
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			head = text[:nul]
		}
		parts := strings.Split(head, " ")
		if len(parts) < 3 {
			return jsonError(400, "malformed push command")
		}
		commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: strings.Join(parts[2:], " ")})
	}
	if len(commands) == 0 {
		return jsonError(400, "no push commands")
	}
	packBytes := body[packStart:]
	var report []string
	unpackOk := true
	unpackDetail := "ok"
	var incoming *packContents
	if len(packBytes) > 0 {
		checksumOk, _, _ := verifyPackChecksum(packBytes)
		if !checksumOk {
			unpackOk = false
			unpackDetail = "pack checksum mismatch"
		} else {
			pc, err := readPack(packBytes)
			if err != nil {
				unpackOk = false
				unpackDetail = err.Error()
			} else {
				incoming = pc
			}
		}
	}
	if unpackOk && incoming != nil {
		for oid, object := range incoming.objects {
			r.objects[oid] = object
		}
		for _, command := range commands {
			if command.newOid == zeroOID {
				continue
			}
			object, ok := r.objects[command.newOid]
			if !ok {
				unpackOk = false
				unpackDetail = "object " + command.newOid + " missing"
				break
			}
			if object.typ == objCommit {
				if missing := missingDirectRefs(r, incoming); missing != "" {
					unpackOk = false
					unpackDetail = "object " + missing + " missing"
					break
				}
			}
		}
	}
	if unpackOk {
		for _, command := range commands {
			current, exists := r.refs[command.ref]
			if command.newOid == zeroOID {
				if !exists {
					report = append(report, "ng "+command.ref+" remote ref does not exist\n")
				} else if command.oldOid != zeroOID && current != command.oldOid {
					report = append(report, "ng "+command.ref+" non-fast-forward\n")
				} else {
					delete(r.refs, command.ref)
					r.version++
					report = append(report, "ok "+command.ref+"\n")
				}
				continue
			}
			if command.oldOid == zeroOID {
				if exists {
					report = append(report, "ng "+command.ref+" already exists\n")
				} else {
					r.refs[command.ref] = command.newOid
					r.version++
					report = append(report, "ok "+command.ref+"\n")
				}
				continue
			}
			if !exists {
				report = append(report, "ng "+command.ref+" remote ref does not exist\n")
			} else if current != command.oldOid {
				report = append(report, "ng "+command.ref+" non-fast-forward\n")
			} else if !isAncestor(r, command.oldOid, command.newOid) {
				report = append(report, "ng "+command.ref+" non-fast-forward\n")
			} else {
				r.refs[command.ref] = command.newOid
				r.version++
				report = append(report, "ok "+command.ref+"\n")
			}
		}
	} else {
		for _, command := range commands {
			report = append(report, "ng "+command.ref+" unpack failed\n")
		}
	}
	unpackLine := "unpack ok\n"
	if !unpackOk {
		unpackLine = "unpack " + unpackDetail + "\n"
	}
	lines := append([]string{unpackLine}, report...)
	var banded []pktLineItem
	for _, line := range lines {
		inner := pktLine([]byte(line))
		framed := make([]byte, 0, 1+len(inner))
		framed = append(framed, 1)
		framed = append(framed, inner...)
		banded = append(banded, pktLineItem{payload: framed})
	}
	banded = append(banded, pktLineItem{flush: true})
	return rawResponse(200, receiveResult, encodePktLines(banded))
}

// missingDirectRefs returns the first direct reference of the pack's new
// objects that the repo does not hold, or "". Only the pack's own objects
// are checked. Caller must hold Lock.
func missingDirectRefs(r *repo, incoming *packContents) string {
	for _, oid := range incoming.order {
		object := incoming.objects[oid]
		switch object.typ {
		case objCommit:
			commit := parseCommit(object.data)
			if _, ok := r.objects[commit.tree]; !ok {
				return commit.tree
			}
			for _, parent := range commit.parents {
				if _, ok := r.objects[parent]; !ok {
					return parent
				}
			}
		case objTree:
			entries, err := parseTree(object.data)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if _, ok := r.objects[entry.oid]; !ok {
					return entry.oid
				}
			}
		case objTag:
			target := parseTag(object.data).object
			if _, ok := r.objects[target]; !ok {
				return target
			}
		}
	}
	return ""
}

// handleGit routes one git smart-HTTP request. Returns nil when the path is
// not a git route.
func handleGit(s *store, method, path string, query url.Values, headers http.Header, body []byte) *forgeResponse {
	parts := strings.Split(path, "/")
	if len(parts) != 4 && len(parts) != 5 {
		return nil
	}
	ownerRaw, nameGit := parts[1], parts[2]
	if ownerRaw == "" || !strings.HasSuffix(nameGit, ".git") || len(nameGit) <= 4 {
		return nil
	}
	repoName := nameGit[:len(nameGit)-4]
	var service string
	if len(parts) == 5 {
		if parts[3] != "info" || parts[4] != "refs" {
			return nil
		}
		service = "info/refs"
	} else {
		if parts[3] != "git-upload-pack" && parts[3] != "git-receive-pack" {
			return nil
		}
		service = parts[3]
	}
	owner, err1 := url.PathUnescape(ownerRaw)
	decodedName, err2 := url.PathUnescape(repoName)
	if err1 != nil || err2 != nil {
		resp := jsonError(500, "Internal Server Error")
		return &resp
	}
	if !checkAuth(headers.Get("Authorization")) {
		resp := jsonError(401, "Requires authentication")
		return &resp
	}
	if owner != viewerLogin {
		resp := jsonError(404, "Not Found")
		return &resp
	}
	repo := s.getRepo(owner, decodedName)
	if method == "GET" && service == "info/refs" {
		if repo == nil {
			resp := jsonError(404, "Not Found")
			return &resp
		}
		wanted := query.Get("service")
		if wanted == "git-upload-pack" {
			if headers.Get("Git-Protocol") == "version=2" {
				resp := rawResponse(200, uploadAd, gitAd(repo, "ad:v2", v2Advertisement))
				return &resp
			}
			resp := rawResponse(200, uploadAd, gitAd(repo, "ad:v0:upload:"+repo.defaultBranch, func() []byte {
				return v0Advertisement("git-upload-pack", repo)
			}))
			return &resp
		}
		if wanted == "git-receive-pack" {
			resp := rawResponse(200, receiveAd, gitAd(repo, "ad:v0:receive:"+repo.defaultBranch, func() []byte {
				return v0Advertisement("git-receive-pack", repo)
			}))
			return &resp
		}
		resp := jsonError(400, "unknown service")
		return &resp
	}
	if method == "POST" && service == "git-upload-pack" {
		if repo == nil {
			resp := jsonError(404, "Not Found")
			return &resp
		}
		var resp forgeResponse
		if headers.Get("Git-Protocol") == "version=2" {
			resp = handleUploadPackV2(repo, body)
		} else {
			resp = handleUploadPackV0(repo, body)
		}
		return &resp
	}
	if method == "POST" && service == "git-receive-pack" {
		if repo == nil {
			resp := jsonError(404, "Not Found")
			return &resp
		}
		resp := handleReceivePack(repo, body)
		return &resp
	}
	resp := jsonError(404, "Not Found")
	return &resp
}
