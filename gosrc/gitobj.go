// Git's object layer, written from the protocol: SHA-1 object ids, pkt-lines,
// packfiles of plain (non-delta) objects, tree/commit parsing, reachability
// walks and a file-level three-way merge. No git binary, no outside code.
//
// Pack format (version 2): "PACK", uint32be version, uint32be count, then per
// object a type+size varint, the zlib-deflated bytes, then a 20-byte SHA-1
// trailer over everything before it.
//
// NOTE on locking: functions taking *repo read or write repo.objects and must
// be called with the repo's lock held (RLock for reads, Lock for writers like
// buildTreeFromFlat). Pure helpers (hashing, pkt-lines, packs) take no lock.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
	"sort"
	"strings"
	"sync"
)

// Pooled zlib writers/readers ("pooled zlib" in the top entries' titles):
// a pack deflate/inflate allocates a compressor/decompressor per object, and
// the push workload inflates hundreds of objects. The pools reuse them.
// A zlib.Writer is Reset to a fresh buffer per use; a zlib.Reader is Reset
// to a fresh input. Both are safe to reuse after Close.
var zlibWriterPool = sync.Pool{
	New: func() any { return zlib.NewWriter(io.Discard) },
}

var zlibReaderPool sync.Pool // holds io.ReadCloser, a *zlib.reader

const gitAgent = "forge-core-clone/1.0"

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// hashObject returns the id of a git object: sha1("<type> <len>\0" + bytes).
func hashObject(typ objectType, data []byte) string {
	header := []byte(fmt.Sprintf("%s %d\x00", typ, len(data)))
	sum := sha1.Sum(append(header, data...))
	return hex.EncodeToString(sum[:])
}

var typeCode = map[objectType]byte{
	objCommit: 1,
	objTree:   2,
	objBlob:   3,
	objTag:    4,
}

var codeType = map[byte]objectType{
	1: objCommit,
	2: objTree,
	3: objBlob,
	4: objTag,
}

// ---------------------------------------------------------------- pkt-lines

// pktLine frames payload (payload must already end with \n when the protocol
// wants one).
func pktLine(payload []byte) []byte {
	l := len(payload) + 4
	if l > 0xffff {
		panic("pkt-line too long")
	}
	out := make([]byte, 0, l)
	out = append(out, []byte(fmt.Sprintf("%04x", l))...)
	out = append(out, payload...)
	return out
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

type pktKind string

const (
	pktData      pktKind = "data"
	pktFlushKind pktKind = "flush"
	pktDelimKind pktKind = "delim"
)

type pkt struct {
	kind    pktKind
	payload []byte
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodePktLines decodes a whole buffer of pkt-lines. Errors on a malformed frame.
func decodePktLines(buf []byte) ([]pkt, error) {
	var out []pkt
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, errors.New("truncated pkt-line header")
		}
		hexStr := string(buf[pos : pos+4])
		if !isHex4(hexStr) {
			return nil, fmt.Errorf("bad pkt-line length %q", hexStr)
		}
		l, _ := hexToInt(hexStr)
		if l == 0 {
			out = append(out, pkt{kind: pktFlushKind})
			pos += 4
			continue
		}
		if l == 1 {
			out = append(out, pkt{kind: pktDelimKind})
			pos += 4
			continue
		}
		if l < 4 || pos+l > len(buf) {
			return nil, errors.New("bad pkt-line length")
		}
		out = append(out, pkt{kind: pktData, payload: buf[pos+4 : pos+l]})
		pos += l
	}
	return out, nil
}

// decodePktPrefix decodes pkt-lines from the start of buf until the first
// flush packet (inclusive). Returns the packets and the offset where the
// trailing bytes (a packfile, for receive-pack) begin.
func decodePktPrefix(buf []byte) ([]pkt, int, error) {
	var packets []pkt
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, errors.New("truncated pkt-line header")
		}
		hexStr := string(buf[pos : pos+4])
		if !isHex4(hexStr) {
			return nil, 0, fmt.Errorf("bad pkt-line length %q", hexStr)
		}
		l, _ := hexToInt(hexStr)
		if l == 0 {
			packets = append(packets, pkt{kind: pktFlushKind})
			return packets, pos + 4, nil
		}
		if l == 1 {
			packets = append(packets, pkt{kind: pktDelimKind})
			pos += 4
			continue
		}
		if l < 4 || pos+l > len(buf) {
			return nil, 0, errors.New("bad pkt-line length")
		}
		packets = append(packets, pkt{kind: pktData, payload: buf[pos+4 : pos+l]})
		pos += l
	}
	return packets, pos, nil
}

func hexToInt(s string) (int, error) {
	var v int
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("bad hex %q", s)
		}
		v = v*16 + d
	}
	return v, nil
}

type pktLineItem struct {
	flush   bool
	delim   bool
	payload []byte
}

// encodePktLines encodes data packets, then a flush.
func encodePktLines(items []pktLineItem) []byte {
	var parts [][]byte
	for _, item := range items {
		switch {
		case item.flush:
			parts = append(parts, pktFlush())
		case item.delim:
			parts = append(parts, pktDelim())
		default:
			parts = append(parts, pktLine(item.payload))
		}
	}
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------- objects

type parsedCommit struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) parsedCommit {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var out parsedCommit
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.parents = append(out.parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.committer = line[10:]
		}
	}
	out.message = message
	return out
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	lines := []string{"tree " + tree}
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+author, "committer "+committer, "", message)
	return []byte(strings.Join(lines, "\n"))
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], 0x20)
		if space < 0 {
			return nil, errors.New("malformed tree: no space")
		}
		space += pos
		nul := bytes.IndexByte(data[space:], 0x00)
		if nul < 0 {
			return nil, errors.New("malformed tree: no NUL")
		}
		nul += space
		if nul+21 > len(data) {
			return nil, errors.New("malformed tree: no NUL")
		}
		entries = append(entries, treeEntry{
			mode: string(data[pos:space]),
			name: string(data[space+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		pos = nul + 21
	}
	return entries, nil
}

// sortTreeEntries applies git's tree sort: entries order by name, directories
// as if named "name/".
func sortTreeEntries(entries []treeEntry) []treeEntry {
	out := make([]treeEntry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		an, bn := out[i].name, out[j].name
		if out[i].mode == "40000" {
			an += "/"
		}
		if out[j].mode == "40000" {
			bn += "/"
		}
		return an < bn
	})
	return out
}

func buildTree(entries []treeEntry) []byte {
	var parts [][]byte
	for _, e := range sortTreeEntries(entries) {
		parts = append(parts, []byte(e.mode+" "+e.name+"\x00"))
		raw, _ := hex.DecodeString(e.oid)
		parts = append(parts, raw)
	}
	return bytes.Join(parts, nil)
}

type parsedTag struct {
	object  string
	typ     string
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) parsedTag {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = text[split+2:]
	}
	var out parsedTag
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			out.object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			out.typ = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			out.tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			out.tagger = line[7:]
		}
	}
	out.message = message
	return out
}

// ---------------------------------------------------------------- packs

// writePack builds a version-2 packfile from plain objects, in the given order.
func writePack(objects []*gitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], 2)
	buf.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], uint32(len(objects)))
	buf.Write(tmp[:])
	for _, object := range objects {
		typeNum := typeCode[object.typ]
		size := len(object.data)
		var header []byte
		first := byte(typeNum<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		header = append(header, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			header = append(header, b)
		}
		buf.Write(header)
		var zb bytes.Buffer
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&zb)
		_, _ = zw.Write(object.data)
		_ = zw.Close()
		zlibWriterPool.Put(zw)
		buf.Write(zb.Bytes())
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out
}

func inflateZlib(buf []byte) ([]byte, error) {
	// Reuse a pooled reader when one is available; Reset re-initializes it
	// for the new input. On any Reset failure, fall back to a fresh reader.
	if v := zlibReaderPool.Get(); v != nil {
		zr := v.(io.ReadCloser)
		if rz, ok := zr.(interface{ Reset(io.Reader, []byte) error }); ok {
			if err := rz.Reset(bytes.NewReader(buf), nil); err == nil {
				data, rerr := io.ReadAll(zr)
				zr.Close()
				zlibReaderPool.Put(zr)
				return data, rerr
			}
		}
		zr.Close()
	}
	zr, err := zlib.NewReader(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	data, rerr := io.ReadAll(zr)
	zr.Close()
	zlibReaderPool.Put(zr)
	return data, rerr
}

// inflateOne inflates the zlib stream starting at buf[0] and says how many
// bytes it consumes. The stream's end is the position of its Adler-32
// checksum, verified by re-inflating the prefix.
func inflateOne(buf []byte, expectedSize int) ([]byte, int, error) {
	inflated, err := inflateZlib(buf)
	if err != nil {
		return nil, 0, err
	}
	if len(inflated) != expectedSize {
		return nil, 0, errors.New("pack object size mismatch")
	}
	var needle [4]byte
	binary.BigEndian.PutUint32(needle[:], adler32.Checksum(inflated))
	from := 0
	for {
		rel := bytes.Index(buf[from:], needle[:])
		if rel < 0 {
			return nil, 0, errors.New("corrupt deflated object")
		}
		idx := from + rel
		candidate := idx + 4
		check, err := inflateZlib(buf[:candidate])
		if err == nil && len(check) == expectedSize && bytes.Equal(check, inflated) {
			return check, candidate, nil
		}
		from = idx + 1
	}
}

// packContents is a parsed packfile: objects by oid, plus the pack order
// (which missingDirectRefs reports in).
type packContents struct {
	objects map[string]*gitObject
	order   []string
}

// readPack reads a version-2 packfile of plain objects. Delta types are rejected.
func readPack(buf []byte) (*packContents, error) {
	if len(buf) < 12 || string(buf[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	objects := make(map[string]*gitObject)
	var order []string
	pos := 12
	for i := uint32(0); i < count; i++ {
		if pos >= len(buf)-20 {
			return nil, errors.New("pack truncated")
		}
		b := buf[pos]
		pos++
		typeNum := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(buf) {
				return nil, errors.New("pack truncated in size")
			}
			b = buf[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		if typeNum == 6 || typeNum == 7 {
			return nil, errors.New("delta packs are not supported")
		}
		typ, ok := codeType[typeNum]
		if !ok {
			return nil, fmt.Errorf("unknown pack object type %d", typeNum)
		}
		inflated, consumed, err := inflateOne(buf[pos:], size)
		if err != nil {
			return nil, err
		}
		pos += consumed
		oid := hashObject(typ, inflated)
		objects[oid] = &gitObject{typ: typ, data: append([]byte(nil), inflated...)}
		order = append(order, oid)
	}
	return &packContents{objects: objects, order: order}, nil
}

// verifyPackChecksum verifies a packfile's SHA-1 trailer.
func verifyPackChecksum(buf []byte) (ok bool, actual, expected string) {
	if len(buf) < 20 {
		return false, "", ""
	}
	expected = hex.EncodeToString(buf[len(buf)-20:])
	actual = sha1Hex(buf[:len(buf)-20])
	return actual == expected, actual, expected
}

// ---------------------------------------------------------------- reachability

type flatEntry struct {
	oid  string
	mode string
}

// flatTree is a flattened tree: path -> entry, plus the insertion order
// (the stack walk's encounter order, matching JS Map iteration order, which
// the contents directory listing exposes as response order).
type flatTree struct {
	m     map[string]flatEntry
	order []string
}

// flattenTree flattens a tree to path -> entry, walking subtrees with an
// index pointer (no shift). Caller must hold at least RLock on the repo.
func flattenTree(r *repo, treeOid string) *flatTree {
	out := &flatTree{m: make(map[string]flatEntry)}
	type frame struct {
		oid    string
		prefix string
	}
	stack := []frame{{oid: treeOid, prefix: ""}}
	seen := make(map[string]bool)
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[f.oid] {
			continue
		}
		seen[f.oid] = true
		object, ok := r.objects[f.oid]
		if !ok || object.typ != objTree {
			continue
		}
		entries, err := parseTree(object.data)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := entry.name
			if f.prefix != "" {
				path = f.prefix + "/" + entry.name
			}
			if entry.mode == "40000" {
				stack = append(stack, frame{oid: entry.oid, prefix: path})
			} else if _, ok := out.m[path]; !ok {
				out.m[path] = flatEntry{oid: entry.oid, mode: entry.mode}
				out.order = append(out.order, path)
			} else {
				out.m[path] = flatEntry{oid: entry.oid, mode: entry.mode}
			}
		}
	}
	return out
}

// walkResult is a commit walk: oid -> distance from the nearest start, plus
// the BFS discovery order (matches JS Map insertion order, which planFetch
// and mergeBase depend on for deterministic output).
type walkResult struct {
	dist  map[string]int
	order []string
}

// walkCommits walks commits from starts, following parents. With maxDepth >=
// 0, commits at distance maxDepth are never visited (their parents are not
// followed). maxDepth < 0 means no limit. Caller must hold at least RLock.
func walkCommits(r *repo, starts []string, maxDepth int) walkResult {
	dist := make(map[string]int)
	var order []string
	var queue []string
	for _, start := range starts {
		if start == zeroOID {
			continue
		}
		if _, ok := dist[start]; !ok {
			dist[start] = 0
			order = append(order, start)
			queue = append(queue, start)
		}
	}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		depth := dist[oid]
		if maxDepth >= 0 && depth >= maxDepth {
			continue
		}
		object, ok := r.objects[oid]
		if !ok || object.typ != objCommit {
			continue
		}
		commit := parseCommit(object.data)
		next := depth + 1
		for _, parent := range commit.parents {
			if _, ok := dist[parent]; !ok {
				dist[parent] = next
				order = append(order, parent)
				queue = append(queue, parent)
			}
		}
	}
	return walkResult{dist: dist, order: order}
}

// isAncestor reports whether ancestor is descendant itself or reachable from
// it by parents. Stops at the first hit instead of walking the whole history.
// Caller must hold at least RLock.
func isAncestor(r *repo, ancestor, descendant string) bool {
	if ancestor == zeroOID {
		return true
	}
	if ancestor == descendant {
		return true
	}
	queue := []string{descendant}
	seen := map[string]bool{descendant: true}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		object, ok := r.objects[oid]
		if !ok || object.typ != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.data).parents {
			if parent == ancestor {
				return true
			}
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	return false
}

// mergeBase finds a merge-base: the common ancestor minimizing dist(a) +
// dist(b). Caller must hold at least RLock.
func mergeBase(r *repo, a, b string) (string, bool) {
	if a == zeroOID || b == zeroOID {
		return "", false
	}
	distA := walkCommits(r, []string{a}, -1)
	if _, ok := distA.dist[b]; ok {
		return b, true
	}
	distB := walkCommits(r, []string{b}, -1)
	best := ""
	bestScore := int(^uint(0) >> 1)
	for _, oid := range distA.order {
		da := distA.dist[oid]
		db, ok := distB.dist[oid]
		if !ok {
			continue
		}
		if score := da + db; score < bestScore {
			bestScore = score
			best = oid
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

type fetchPlan struct {
	// objects to send, in send order (commits, then trees, then blobs).
	objects []*gitObject
	// new shallow boundary commits.
	shallow []string
	// old shallow commits that are no longer shallow.
	unshallow []string
}

// haveClosure returns the objects a client already holds, from its have
// lines: every object reachable from a have, walking parents — except that
// the walk stops at the client's old shallow boundary. Caller must hold at
// least RLock.
func haveClosure(r *repo, haves []string, oldShallow []string) map[string]bool {
	out := make(map[string]bool)
	stop := make(map[string]bool)
	for _, o := range oldShallow {
		stop[o] = true
	}
	var commits []string
	queue := append([]string(nil), haves...)
	seen := make(map[string]bool)
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		if oid == zeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := r.objects[oid]
		if !ok || object.typ != objCommit {
			continue
		}
		commits = append(commits, oid)
		out[oid] = true
		if stop[oid] {
			continue
		}
		for _, parent := range parseCommit(object.data).parents {
			queue = append(queue, parent)
		}
	}
	var treeQueue []string
	for _, oid := range commits {
		if object, ok := r.objects[oid]; ok && object.typ == objCommit {
			treeQueue = append(treeQueue, parseCommit(object.data).tree)
		}
	}
	seenTrees := make(map[string]bool)
	t := 0
	for t < len(treeQueue) {
		treeOid := treeQueue[t]
		t++
		if seenTrees[treeOid] {
			continue
		}
		seenTrees[treeOid] = true
		object, ok := r.objects[treeOid]
		if !ok {
			continue
		}
		out[treeOid] = true
		if object.typ != objTree {
			continue
		}
		entries, err := parseTree(object.data)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.mode == "40000" {
				treeQueue = append(treeQueue, entry.oid)
			} else {
				out[entry.oid] = true
			}
		}
	}
	return out
}

// planFetch plans a fetch: commits reachable from wants within depth (depth <
// 0 = all), plus their trees and blobs, minus the objects the client already
// holds. Caller must hold at least RLock.
func planFetch(r *repo, wants, haves []string, depth int, oldShallow []string) fetchPlan {
	walk := walkCommits(r, wants, depth)
	sentCommits := make(map[string]bool)
	var cut []string
	for _, oid := range walk.order {
		d := walk.dist[oid]
		if depth < 0 || d < depth {
			sentCommits[oid] = true
		}
		if depth >= 0 && d == depth-1 {
			cut = append(cut, oid)
		}
	}
	held := haveClosure(r, haves, oldShallow)
	sendMap := make(map[string]*gitObject)
	var sendOrder []string
	sendSet := func(oid string, obj *gitObject) {
		if _, ok := sendMap[oid]; !ok {
			sendOrder = append(sendOrder, oid)
		}
		sendMap[oid] = obj
	}
	commitList := make([]string, 0, len(sentCommits))
	for _, oid := range walk.order {
		if sentCommits[oid] {
			commitList = append(commitList, oid)
		}
	}
	var treeQueue []string
	for _, oid := range commitList {
		object, ok := r.objects[oid]
		if !ok {
			continue
		}
		if !held[oid] {
			sendSet(oid, object)
		}
		if object.typ == objCommit {
			treeQueue = append(treeQueue, parseCommit(object.data).tree)
		}
	}
	seenTrees := make(map[string]bool)
	head := 0
	for head < len(treeQueue) {
		treeOid := treeQueue[head]
		head++
		if seenTrees[treeOid] {
			continue
		}
		seenTrees[treeOid] = true
		object, ok := r.objects[treeOid]
		if !ok {
			continue
		}
		if !held[treeOid] {
			sendSet(treeOid, object)
		}
		if object.typ != objTree {
			continue
		}
		entries, err := parseTree(object.data)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.mode == "40000" {
				treeQueue = append(treeQueue, entry.oid)
			} else if blob, ok := r.objects[entry.oid]; ok && !held[entry.oid] {
				sendSet(entry.oid, blob)
			}
		}
	}
	var ordered []*gitObject
	for _, oid := range commitList {
		if object, ok := sendMap[oid]; ok {
			ordered = append(ordered, object)
		}
	}
	for _, oid := range sendOrder {
		if object := sendMap[oid]; object.typ != objCommit {
			ordered = append(ordered, object)
		}
	}
	var shallow []string
	for _, oid := range cut {
		if sentCommits[oid] {
			shallow = append(shallow, oid)
		}
	}
	var unshallow []string
	var stillShallow []string
	for _, old := range oldShallow {
		object, ok := r.objects[old]
		if !ok || object.typ != objCommit {
			continue
		}
		commit := parseCommit(object.data)
		complete := true
		for _, p := range commit.parents {
			if !sentCommits[p] && !held[p] {
				complete = false
				break
			}
		}
		if complete {
			unshallow = append(unshallow, old)
		} else if sentCommits[old] || held[old] {
			stillShallow = append(stillShallow, old)
		}
	}
	shallow = append(shallow, stillShallow...)
	return fetchPlan{objects: ordered, shallow: shallow, unshallow: unshallow}
}

// ---------------------------------------------------------------- merge + diff

type mergeResult struct {
	// the merged flat tree, or nil when the merge conflicts.
	tree      map[string]flatEntry
	conflicts []string
}

// threeWayMerge is a file-level three-way merge of flattened trees.
func threeWayMerge(base, ours, theirs map[string]flatEntry) mergeResult {
	out := make(map[string]flatEntry)
	var conflicts []string
	// Path order: base keys, then ours-only, then theirs-only (matches JS Set
	// insertion order, which decides the conflicts list order).
	seen := make(map[string]bool)
	var paths []string
	for p := range base {
		seen[p] = true
		paths = append(paths, p)
	}
	for p := range ours {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range theirs {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	same := func(x, y *flatEntry) bool {
		if x == nil && y == nil {
			return true
		}
		if x == nil || y == nil {
			return false
		}
		return x.oid == y.oid && x.mode == y.mode
	}
	for _, path := range paths {
		b, bOk := base[path]
		o, oOk := ours[path]
		t, tOk := theirs[path]
		var bp, op, tp *flatEntry
		if bOk {
			bp = &b
		}
		if oOk {
			op = &o
		}
		if tOk {
			tp = &t
		}
		if same(op, tp) {
			if op != nil {
				out[path] = *op
			}
			continue
		}
		if same(bp, op) {
			if tp != nil {
				out[path] = *tp
			}
			continue
		}
		if same(bp, tp) {
			if op != nil {
				out[path] = *op
			}
			continue
		}
		conflicts = append(conflicts, path)
	}
	if len(conflicts) > 0 {
		return mergeResult{tree: nil, conflicts: conflicts}
	}
	return mergeResult{tree: out, conflicts: []string{}}
}

// buildTreeFromFlat builds a tree object from a flat path map, creating
// intermediate trees. Returns the root tree oid. Caller must hold Lock.
func buildTreeFromFlat(r *repo, flat map[string]flatEntry) string {
	type node struct {
		entries map[string]struct {
			mode string
			oid  string
		}
	}
	dirs := map[string]*node{"": {entries: map[string]struct {
		mode string
		oid  string
	}{}}}
	dirOf := func(path string) *node {
		if n, ok := dirs[path]; ok {
			return n
		}
		n := &node{entries: map[string]struct {
			mode string
			oid  string
		}{}}
		dirs[path] = n
		return n
	}
	for path, entry := range flat {
		parts := strings.Split(path, "/")
		prefix := ""
		for i := 0; i < len(parts)-1; i++ {
			part := parts[i]
			parent := dirOf(prefix)
			childPrefix := part
			if prefix != "" {
				childPrefix = prefix + "/" + part
			}
			if _, ok := parent.entries[part]; !ok {
				parent.entries[part] = struct {
					mode string
					oid  string
				}{mode: "40000", oid: ""}
			}
			dirOf(childPrefix)
			prefix = childPrefix
		}
		leaf := dirOf(prefix)
		leaf.entries[parts[len(parts)-1]] = struct {
			mode string
			oid  string
		}{mode: entry.mode, oid: entry.oid}
	}
	var writeNode func(prefix string) string
	writeNode = func(prefix string) string {
		n := dirs[prefix]
		var entries []treeEntry
		for name, value := range n.entries {
			oid := value.oid
			if value.mode == "40000" {
				childPrefix := name
				if prefix != "" {
					childPrefix = prefix + "/" + name
				}
				oid = writeNode(childPrefix)
			}
			entries = append(entries, treeEntry{mode: value.mode, name: name, oid: oid})
		}
		data := buildTree(entries)
		oid := hashObject(objTree, data)
		if _, ok := r.objects[oid]; !ok {
			r.objects[oid] = &gitObject{typ: objTree, data: data}
		}
		return oid
	}
	return writeNode("")
}

type fileChange struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// lineDiff counts line additions/deletions between two blob versions
// (multiset diff).
func lineDiff(oldData, newData []byte) (additions, deletions int) {
	linesOf := func(data []byte) []string {
		if data == nil {
			return nil
		}
		text := string(data)
		if text == "" {
			return nil
		}
		lines := strings.Split(text, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	oldLines := linesOf(oldData)
	newLines := linesOf(newData)
	oldCounts := make(map[string]int)
	for _, line := range oldLines {
		oldCounts[line]++
	}
	for _, line := range newLines {
		if left := oldCounts[line]; left > 0 {
			oldCounts[line] = left - 1
		} else {
			additions++
		}
	}
	for _, left := range oldCounts {
		deletions += left
	}
	return additions, deletions
}

// diffTrees returns the file changes between two tree oids (base -> head).
// Caller must hold at least RLock.
func diffTrees(r *repo, baseTree, headTree *string) []fileChange {
	var baseFlat, headFlat *flatTree
	if baseTree == nil {
		baseFlat = &flatTree{m: map[string]flatEntry{}}
	} else {
		baseFlat = flattenTree(r, *baseTree)
	}
	if headTree == nil {
		headFlat = &flatTree{m: map[string]flatEntry{}}
	} else {
		headFlat = flattenTree(r, *headTree)
	}
	seen := make(map[string]bool)
	var paths []string
	for p := range baseFlat.m {
		seen[p] = true
		paths = append(paths, p)
	}
	for p := range headFlat.m {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var changes []fileChange
	for _, path := range paths {
		b, bOk := baseFlat.m[path]
		h, hOk := headFlat.m[path]
		if bOk && hOk && b.oid == h.oid {
			continue
		}
		if !bOk && hOk {
			var blobData []byte
			if blob, ok := r.objects[h.oid]; ok {
				blobData = blob.data
			}
			add, _ := lineDiff(nil, blobData)
			changes = append(changes, fileChange{Filename: path, Status: "added", Additions: add, Deletions: 0, Changes: add})
		} else if bOk && !hOk {
			var blobData []byte
			if blob, ok := r.objects[b.oid]; ok {
				blobData = blob.data
			}
			_, del := lineDiff(blobData, nil)
			changes = append(changes, fileChange{Filename: path, Status: "removed", Additions: 0, Deletions: del, Changes: del})
		} else if bOk && hOk {
			var oldData, newData []byte
			if blob, ok := r.objects[b.oid]; ok {
				oldData = blob.data
			}
			if blob, ok := r.objects[h.oid]; ok {
				newData = blob.data
			}
			add, del := lineDiff(oldData, newData)
			changes = append(changes, fileChange{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	if changes == nil {
		changes = []fileChange{}
	}
	return changes
}
