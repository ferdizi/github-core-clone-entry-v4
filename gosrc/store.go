// The forge's one store: every surface (git smart HTTP, REST v3, GraphQL, MCP)
// reads and writes the same objects, so an issue created over MCP is the same
// issue REST lists and GraphQL reads. In-memory only; the trial mounts the repo
// read-only and the suite drives one process, so nothing needs to persist.
//
// The viewer is fixed by the kit's seeded scenario: the token owner is always
// `adaiversen` (id 1000). The bearer token itself is only a key: the entry
// receives MAKEFUN_SHOP_TOKEN in its environment and accepts exactly that
// token (or, with no token in the environment, any non-empty bearer token,
// for local runs only).

package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
)

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	zeroOID     = "0000000000000000000000000000000000000000"
)

type objectType string

const (
	objCommit objectType = "commit"
	objTree   objectType = "tree"
	objBlob   objectType = "blob"
	objTag    objectType = "tag"
)

type gitObject struct {
	typ  objectType
	data []byte
}

type label struct {
	name        string
	color       string
	description string
}

type comment struct {
	id         int
	body       string
	createdSeq int
	createdAt  string
	updatedAt  string
}

type issue struct {
	id         int
	number     int
	title      string
	body       string
	state      string // "open" | "closed"
	labels     []string
	comments   []*comment
	createdSeq int
	updatedSeq int
	createdAt  string
	updatedAt  string
}

type pull struct {
	id             int
	number         int
	title          string
	body           string
	state          string // "open" | "closed"
	merged         bool
	mergedAt       *string
	mergeCommitSha *string
	headRef        string
	baseRef        string
	headSha        string
	baseSha        string
	createdSeq     int
	updatedSeq     int
	createdAt      string
	updatedAt      string
}

type cachedBody struct {
	version int
	body    []byte
}

type repo struct {
	mu            sync.RWMutex
	id            int
	name          string
	owner         string
	description   string
	private       bool
	defaultBranch string
	createdSeq    int
	createdAt     string
	updatedAt     string
	objects       map[string]*gitObject
	refs          map[string]string
	issues        []*issue
	issueSeq      int
	pulls         []*pull
	pullSeq       int
	labels        map[string]*label
	commentSeq    int
	version       int
	// Serialized single-issue GET bodies, keyed by origin + number; valid
	// only at the recorded repo version.
	issueBodyCache map[string]cachedBody
	// Cached deterministic git responses (fetch packs, ls-refs,
	// advertisements), keyed by request fingerprint; valid only at the
	// recorded repo version.
	fetchCache map[string]cachedBody
	// In-flight git computations, keyed like fetchCache: concurrent
	// identical fetches share one computation (singleflight).
	sfMu     sync.Mutex
	inflight map[string]*inflightCall
}

// globalID builds one global id (GraphQL Relay style): base64url of
// "Kind:parts...". Opaque, stable.
func globalID(kind string, parts ...any) string {
	strs := make([]string, 0, len(parts)+1)
	strs = append(strs, kind)
	for _, p := range parts {
		strs = append(strs, fmt.Sprint(p))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(strs, ":")))
}

type decodedGlobalID struct {
	kind  string
	parts []string
}

// decodeGlobalID decodes a global id made by globalID, or returns ok=false.
func decodeGlobalID(id string) (decodedGlobalID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return decodedGlobalID{}, false
	}
	split := strings.Split(string(raw), ":")
	if len(split) < 2 || split[0] == "" {
		return decodedGlobalID{}, false
	}
	return decodedGlobalID{kind: split[0], parts: split[1:]}, true
}

type store struct {
	mu          sync.Mutex
	repos       map[string]*repo
	repoIDSeq   int
	objectIDSeq int
	seq         int
}

func newStore() *store {
	return &store{
		repos:       make(map[string]*repo),
		repoIDSeq:   1,
		objectIDSeq: 1,
	}
}

// nextSeq returns a fresh monotonically increasing sequence number.
func (s *store) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *store) nextObjectID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.objectIDSeq
	s.objectIDSeq++
	return id
}

func (s *store) createRepo(name string, description string, private bool, defaultBranch string) *repo {
	now := isoNow()
	s.mu.Lock()
	id := s.repoIDSeq
	s.repoIDSeq++
	s.mu.Unlock()
	r := &repo{
		id:             id,
		name:           name,
		owner:          viewerLogin,
		description:    description,
		private:        private,
		defaultBranch:  defaultBranch,
		createdSeq:     s.nextSeq(),
		createdAt:      now,
		updatedAt:      now,
		objects:        make(map[string]*gitObject),
		refs:           make(map[string]string),
		issues:         []*issue{},
		pulls:          []*pull{},
		labels:         make(map[string]*label),
		issueBodyCache: make(map[string]cachedBody),
		fetchCache:     make(map[string]cachedBody),
		inflight:       make(map[string]*inflightCall),
	}
	s.mu.Lock()
	s.repos[viewerLogin+"/"+name] = r
	s.mu.Unlock()
	return r
}

func (s *store) getRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[owner+"/"+name]
}

func (s *store) findRepoByID(id int) *repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.repos {
		if r.id == id {
			return r
		}
	}
	return nil
}

func (s *store) createIssue(r *repo, title, body string, labels []string) *issue {
	now := isoNow()
	iss := &issue{
		id:         s.nextObjectID(),
		title:      title,
		body:       body,
		state:      "open",
		labels:     append([]string(nil), labels...),
		comments:   []*comment{},
		createdSeq: s.nextSeq(),
		updatedSeq: s.nextSeq(),
		createdAt:  now,
		updatedAt:  now,
	}
	r.mu.Lock()
	r.issueSeq++
	iss.number = r.issueSeq
	r.issues = append(r.issues, iss)
	r.version++
	r.mu.Unlock()
	return iss
}

func (s *store) createPull(r *repo, title, body, headRef, baseRef, headSha, baseSha string) *pull {
	now := isoNow()
	p := &pull{
		id:         s.nextObjectID(),
		title:      title,
		body:       body,
		state:      "open",
		merged:     false,
		mergedAt:   nil,
		headRef:    headRef,
		baseRef:    baseRef,
		headSha:    headSha,
		baseSha:    baseSha,
		createdSeq: s.nextSeq(),
		updatedSeq: s.nextSeq(),
		createdAt:  now,
		updatedAt:  now,
	}
	r.mu.Lock()
	r.pullSeq++
	p.number = r.pullSeq
	r.pulls = append(r.pulls, p)
	r.version++
	r.mu.Unlock()
	return p
}

func (s *store) addComment(r *repo, iss *issue, body string) *comment {
	now := isoNow()
	c := &comment{
		id:         s.nextObjectID(),
		body:       body,
		createdSeq: s.nextSeq(),
		createdAt:  now,
		updatedAt:  now,
	}
	r.mu.Lock()
	r.commentSeq++
	iss.comments = append(iss.comments, c)
	iss.updatedSeq = s.nextSeq()
	iss.updatedAt = now
	r.version++
	r.mu.Unlock()
	return c
}

// expectedToken returns the bearer token this process accepts, from the
// environment (empty means any non-empty bearer token is accepted). Read
// once and cached: checkAuth runs on every request and os.Getenv is a
// locked map lookup each time.
var expectedTokenOnce sync.Once
var cachedExpectedToken string

func expectedToken() string {
	expectedTokenOnce.Do(func() { cachedExpectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN") })
	return cachedExpectedToken
}

// checkAuth reports whether the request's Authorization header carries the
// accepted token.
func checkAuth(authorization string) bool {
	if !strings.HasPrefix(authorization, "Bearer ") {
		return false
	}
	presented := authorization[len("Bearer "):]
	if presented == "" {
		return false
	}
	expected := expectedToken()
	if expected == "" {
		return true
	}
	return presented == expected
}

// viewerIdent renders a git author/committer identity line for the viewer at
// a logical epoch.
func viewerIdent(epochSec int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, epochSec)
}

// nextCommitTime returns a fresh logical timestamp (epoch seconds) for a
// server-made commit.
func nextCommitTime() int64 {
	return tickNow()
}
