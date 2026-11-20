// REST v3: repositories, branches, commits, contents, git refs, issues,
// comments, labels and pull requests, over the shared store. Every reply is
// JSON with X-GitHub-Request-Id; a repository under another owner is 404,
// never 403; a missing or wrong token is 401. The API root needs no token,
// matching api.github.com; every other route does.
//
// JSON shapes are structs (not maps) so key order matches the Node
// implementation's object-literal order exactly. Repo reads take RLock,
// mutations take Lock; store.createIssue/createPull/addComment take the repo
// lock themselves, so callers must not hold it when calling them.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const scopes = "repo, read:org"

// queryParams is an ordered query string: pairs in first-appearance order
// (URLSearchParams semantics) plus a first-value lookup map.
type queryParams struct {
	pairs [][2]string
	vals  map[string]string
}

func parseQuery(raw string) queryParams {
	q := queryParams{vals: make(map[string]string)}
	if raw == "" {
		return q
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			key, value = part[:i], part[i+1:]
		}
		if un, err := url.QueryUnescape(key); err == nil {
			key = un
		}
		if un, err := url.QueryUnescape(value); err == nil {
			value = un
		}
		q.pairs = append(q.pairs, [2]string{key, value})
		if _, ok := q.vals[key]; !ok {
			q.vals[key] = value
		}
	}
	return q
}

func (q queryParams) get(key string) string {
	return q.vals[key]
}

// setQueryParam mirrors URLSearchParams.set: replace the first occurrence's
// value and drop later duplicates, or append if absent.
func setQueryParam(pairs [][2]string, key, value string) [][2]string {
	out := pairs[:0]
	found := false
	for _, p := range pairs {
		if p[0] == key {
			if !found {
				out = append(out, [2]string{key, value})
				found = true
			}
			continue
		}
		out = append(out, p)
	}
	if !found {
		out = append(out, [2]string{key, value})
	}
	return out
}

func encodeQuery(pairs [][2]string) string {
	var parts []string
	for _, p := range pairs {
		parts = append(parts, url.QueryEscape(p[0])+"="+url.QueryEscape(p[1]))
	}
	return strings.Join(parts, "&")
}

type restCtx struct {
	s      *store
	origin string
	header http.Header
	query  queryParams
}

func parseBodyJSON(body []byte) (map[string]any, bool) {
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false
	}
	obj, ok := parsed.(map[string]any)
	if !ok || obj == nil {
		return nil, false
	}
	return obj, true
}

func bodyStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func bodyBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// paginate slices a list by ?per_page (default 30) and ?page (default 1),
// with a Link header. Mirrors the Node paginate exactly.
func paginate[T any](ctx *restCtx, items []T, path string) ([]T, string) {
	numParam := func(raw string, def int) int {
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return def
		}
		return int(f) // truncates toward zero, matching Math.trunc for positives
	}
	perPage := numParam(ctx.query.get("per_page"), 30)
	page := numParam(ctx.query.get("page"), 1)
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	var link string
	if start+perPage < len(items) {
		buildLink := func(targetPage int) string {
			pairs := make([][2]string, len(ctx.query.pairs))
			copy(pairs, ctx.query.pairs)
			for _, p := range ctx.query.pairs {
				pairs = setQueryParam(pairs, p[0], p[1])
			}
			pairs = setQueryParam(pairs, "per_page", strconv.Itoa(perPage))
			pairs = setQueryParam(pairs, "page", strconv.Itoa(targetPage))
			return ctx.origin + path + "?" + encodeQuery(pairs)
		}
		lastPage := (len(items) + perPage - 1) / perPage
		link = fmt.Sprintf("<%s>; rel=\"next\", <%s>; rel=\"last\"", buildLink(page+1), buildLink(lastPage))
	}
	return slice, link
}

func linkHeaders(link string) map[string]string {
	if link == "" {
		return nil
	}
	return map[string]string{"link": link}
}

// ---------------------------------------------------------------- JSON shapes

type userJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoJSON struct {
	ID              int      `json:"id"`
	Name            string   `json:"name"`
	FullName        string   `json:"full_name"`
	Owner           userJSON `json:"owner"`
	Private         bool     `json:"private"`
	Description     string   `json:"description"`
	DefaultBranch   string   `json:"default_branch"`
	HTMLURL         string   `json:"html_url"`
	CloneURL        string   `json:"clone_url"`
	URL             string   `json:"url"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	PushedAt        string   `json:"pushed_at"`
	Size            int      `json:"size"`
	WatchersCount   int      `json:"watchers_count"`
	ForksCount      int      `json:"forks_count"`
	OpenIssuesCount int      `json:"open_issues_count"`
}

type labelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueJSON struct {
	ID          int         `json:"id"`
	Number      int         `json:"number"`
	Title       string      `json:"title"`
	Body        string      `json:"body"`
	State       string      `json:"state"`
	Labels      []labelJSON `json:"labels"`
	User        userJSON    `json:"user"`
	Assignees   []any       `json:"assignees"`
	Locked      bool        `json:"locked"`
	Comments    int         `json:"comments"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
	HTMLURL     string      `json:"html_url"`
	URL         string      `json:"url"`
	CommentsURL string      `json:"comments_url"`
}

type commentJSON struct {
	ID        int      `json:"id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
	URL       string   `json:"url"`
}

type pullRefJSON struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
	User  struct {
		Login string `json:"login"`
		ID    int    `json:"id"`
	} `json:"user"`
}

type pullJSON struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	User           userJSON    `json:"user"`
	Draft          bool        `json:"draft"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
	Mergeable      *bool       `json:"mergeable,omitempty"`
}

func toRepoJSON(ctx *restCtx, r *repo) repoJSON {
	// Caller must hold RLock on the repo.
	open := 0
	for _, iss := range r.issues {
		if iss.state == "open" {
			open++
		}
	}
	return repoJSON{
		ID:              r.id,
		Name:            r.name,
		FullName:        r.owner + "/" + r.name,
		Owner:           userJSON{Login: r.owner, ID: viewerID, Type: "User"},
		Private:         r.private,
		Description:     r.description,
		DefaultBranch:   r.defaultBranch,
		HTMLURL:         ctx.origin + "/" + r.owner + "/" + r.name,
		CloneURL:        ctx.origin + "/" + r.owner + "/" + r.name + ".git",
		URL:             ctx.origin + "/api/v3/repos/" + r.owner + "/" + r.name,
		CreatedAt:       r.createdAt,
		UpdatedAt:       r.updatedAt,
		PushedAt:        r.updatedAt,
		Size:            0,
		WatchersCount:   0,
		ForksCount:      0,
		OpenIssuesCount: open,
	}
}

func toLabelJSON(r *repo, name string) labelJSON {
	// Caller must hold RLock on the repo.
	if l, ok := r.labels[name]; ok {
		return labelJSON{Name: name, Color: l.color, Description: l.description}
	}
	return labelJSON{Name: name, Color: "ededed", Description: ""}
}

func toIssueJSON(ctx *restCtx, r *repo, iss *issue) issueJSON {
	// Caller must hold RLock on the repo.
	labels := make([]labelJSON, 0, len(iss.labels))
	for _, name := range iss.labels {
		if l, ok := r.labels[name]; ok {
			labels = append(labels, labelJSON{Name: name, Color: l.color, Description: l.description})
		} else {
			labels = append(labels, labelJSON{Name: name, Color: "ededed", Description: ""})
		}
	}
	return issueJSON{
		ID:          iss.id,
		Number:      iss.number,
		Title:       iss.title,
		Body:        iss.body,
		State:       iss.state,
		Labels:      labels,
		User:        userJSON{Login: viewerLogin, ID: viewerID, Type: "User"},
		Assignees:   []any{},
		Locked:      false,
		Comments:    len(iss.comments),
		CreatedAt:   iss.createdAt,
		UpdatedAt:   iss.updatedAt,
		HTMLURL:     ctx.origin + "/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number),
		URL:         ctx.origin + "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number),
		CommentsURL: ctx.origin + "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number) + "/comments",
	}
}

func toCommentJSON(ctx *restCtx, r *repo, iss *issue, c *comment) commentJSON {
	return commentJSON{
		ID:        c.id,
		Body:      c.body,
		User:      userJSON{Login: viewerLogin, ID: viewerID, Type: "User"},
		CreatedAt: c.createdAt,
		UpdatedAt: c.updatedAt,
		HTMLURL:   ctx.origin + "/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number) + "#issuecomment-" + strconv.Itoa(c.id),
		URL:       ctx.origin + "/api/v3/repos/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(iss.number) + "/comments/" + strconv.Itoa(c.id),
	}
}

// branchTip returns the tip of a branch, or "" when it does not exist.
// Caller must hold at least RLock.
func branchTip(r *repo, branch string) (string, bool) {
	tip, ok := r.refs["refs/heads/"+branch]
	return tip, ok
}

// commitTree returns the tree oid of a commit, or "" when it is not a commit.
// Caller must hold at least RLock.
func commitTree(r *repo, commitOid string) (string, bool) {
	object, ok := r.objects[commitOid]
	if !ok || object.typ != objCommit {
		return "", false
	}
	return parseCommit(object.data).tree, true
}

// mergeableOf computes a pull request's mergeability from its tips.
// Caller must hold at least RLock.
func mergeableOf(r *repo, p *pull) bool {
	headTip, ok := branchTip(r, p.headRef)
	if !ok {
		headTip = p.headSha
	}
	baseTip, ok := branchTip(r, p.baseRef)
	if !ok {
		baseTip = p.baseSha
	}
	if headTip == baseTip {
		return true
	}
	if isAncestor(r, headTip, baseTip) {
		return true
	}
	base, ok := mergeBase(r, baseTip, headTip)
	if !ok {
		return false
	}
	baseTree, ok1 := commitTree(r, base)
	oursTree, ok2 := commitTree(r, baseTip)
	theirsTree, ok3 := commitTree(r, headTip)
	if !ok1 || !ok2 || !ok3 {
		return false
	}
	merged := threeWayMerge(flattenTree(r, baseTree).m, flattenTree(r, oursTree).m, flattenTree(r, theirsTree).m)
	return merged.tree != nil
}

func toPullJSON(ctx *restCtx, r *repo, p *pull, withMergeable bool) pullJSON {
	// Caller must hold RLock on the repo.
	headTip, ok := branchTip(r, p.headRef)
	if !ok {
		headTip = p.headSha
	}
	baseTip, ok := branchTip(r, p.baseRef)
	if !ok {
		baseTip = p.baseSha
	}
	var mergeable *bool
	if withMergeable {
		m := mergeableOf(r, p)
		mergeable = &m
	}
	head := pullRefJSON{Ref: p.headRef, Sha: headTip, Label: r.owner + ":" + p.headRef}
	head.User.Login = viewerLogin
	head.User.ID = viewerID
	base := pullRefJSON{Ref: p.baseRef, Sha: baseTip, Label: r.owner + ":" + p.baseRef}
	base.User.Login = viewerLogin
	base.User.ID = viewerID
	return pullJSON{
		ID:             p.id,
		Number:         p.number,
		Title:          p.title,
		Body:           p.body,
		State:          p.state,
		Merged:         p.merged,
		MergedAt:       p.mergedAt,
		MergeCommitSha: p.mergeCommitSha,
		Head:           head,
		Base:           base,
		User:           userJSON{Login: viewerLogin, ID: viewerID, Type: "User"},
		Draft:          false,
		CreatedAt:      p.createdAt,
		UpdatedAt:      p.updatedAt,
		HTMLURL:        ctx.origin + "/" + r.owner + "/" + r.name + "/pull/" + strconv.Itoa(p.number),
		URL:            ctx.origin + "/api/v3/repos/" + r.owner + "/" + r.name + "/pulls/" + strconv.Itoa(p.number),
		Mergeable:      mergeable,
	}
}

// commitFile commits a file change (create/update/delete) on a branch.
// Returns the new commit oid and blob oid (nil when deleting). Caller must
// hold Lock on the repo.
func commitFile(r *repo, branch, path string, content []byte, isDelete bool, message string) (string, *string) {
	tip, hasTip := branchTip(r, branch)
	var baseTree string
	var hasBaseTree bool
	if hasTip {
		baseTree, hasBaseTree = commitTree(r, tip)
	}
	var flat *flatTree
	if hasBaseTree {
		flat = flattenTree(r, baseTree)
	} else {
		flat = &flatTree{m: make(map[string]flatEntry)}
	}
	var blobOid *string
	if !isDelete {
		oid := hashObject(objBlob, content)
		if _, ok := r.objects[oid]; !ok {
			r.objects[oid] = &gitObject{typ: objBlob, data: content}
		}
		blobOid = &oid
		flat.m[path] = flatEntry{oid: oid, mode: "100644"}
	} else {
		delete(flat.m, path)
	}
	treeOid := buildTreeFromFlat(r, flat.m)
	epoch := nextCommitTime()
	ident := viewerIdent(epoch)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commit := buildCommit(treeOid, parents, ident, ident, message)
	commitOid := hashObject(objCommit, commit)
	r.objects[commitOid] = &gitObject{typ: objCommit, data: commit}
	r.refs["refs/heads/"+branch] = commitOid
	r.version++
	return commitOid, blobOid
}

// mergePull squash-merges a pull request onto its base branch. Shared by
// REST, GraphQL and MCP. Caller must hold Lock on the repo.
func mergePull(s *store, r *repo, p *pull) (string, string, bool) {
	if p.merged {
		return "", "Pull request has already been merged", false
	}
	if p.state == "closed" {
		return "", "Pull request is closed", false
	}
	headTip, ok := branchTip(r, p.headRef)
	if !ok {
		headTip = p.headSha
	}
	baseTip, ok := branchTip(r, p.baseRef)
	if !ok {
		baseTip = p.baseSha
	}
	base, hasBase := mergeBase(r, baseTip, headTip)
	var baseTree string
	var ok1 bool
	if hasBase {
		baseTree, ok1 = commitTree(r, base)
	}
	oursTree, ok2 := commitTree(r, baseTip)
	theirsTree, ok3 := commitTree(r, headTip)
	if !ok1 || !ok2 || !ok3 {
		return "", "Merge conflict", false
	}
	merged := threeWayMerge(flattenTree(r, baseTree).m, flattenTree(r, oursTree).m, flattenTree(r, theirsTree).m)
	if merged.tree == nil {
		return "", "Merge conflict", false
	}
	treeOid := buildTreeFromFlat(r, merged.tree)
	epoch := nextCommitTime()
	ident := viewerIdent(epoch)
	message := fmt.Sprintf("Merge pull request #%d from %s/%s", p.number, r.owner, p.headRef)
	commit := buildCommit(treeOid, []string{baseTip}, ident, ident, message)
	commitOid := hashObject(objCommit, commit)
	r.objects[commitOid] = &gitObject{typ: objCommit, data: commit}
	r.refs["refs/heads/"+p.baseRef] = commitOid
	p.merged = true
	p.state = "closed"
	mergedAt := unixMillisISO(epoch)
	p.mergedAt = &mergedAt
	p.mergeCommitSha = &commitOid
	p.updatedSeq = s.nextSeq()
	p.updatedAt = mergedAt
	r.version++
	return commitOid, "", true
}

// unixMillisISO renders an epoch second as an ISO-8601 UTC string with
// milliseconds (JS Date.toISOString()).
func unixMillisISO(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// ---------------------------------------------------------------- handleRest

func (q queryParams) has(key string) bool {
	_, ok := q.vals[key]
	return ok
}

func inputStr(input map[string]any, key string) (string, bool) {
	s, ok := input[key].(string)
	return s, ok
}

func inputBool(input map[string]any, key string) (bool, bool) {
	b, ok := input[key].(bool)
	return b, ok
}

// labelsFrom reads labels from a POST/PATCH body: strings, or objects with
// a name.
func labelsFrom(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, entry := range arr {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		} else if m, ok := entry.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

// treeOidAtPath returns the child tree oids at a path, for a directory
// listing's sha values. Nil when the path does not resolve to a tree.
// Caller must hold at least RLock.
func treeOidAtPath(r *repo, treeOid, path string) map[string]string {
	parts := strings.Split(path, "/")
	var filtered []string
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	current := treeOid
	for _, part := range filtered {
		object, ok := r.objects[current]
		if !ok || object.typ != objTree {
			return nil
		}
		entries, err := parseTree(object.data)
		if err != nil {
			return nil
		}
		found := ""
		for _, e := range entries {
			if e.name == part && e.mode == "40000" {
				found = e.oid
				break
			}
		}
		if found == "" {
			return nil
		}
		current = found
	}
	object, ok := r.objects[current]
	if !ok || object.typ != objTree {
		return nil
	}
	entries, err := parseTree(object.data)
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	for _, e := range entries {
		out[e.name] = e.oid
	}
	return out
}

// decodeBase64Lenient mirrors Node's Buffer.from(s, 'base64'): invalid
// characters are ignored.
func decodeBase64Lenient(s string) []byte {
	var clean strings.Builder
	for _, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			clean.WriteRune(c)
		}
	}
	b, _ := base64.StdEncoding.DecodeString(clean.String())
	if b == nil {
		return []byte{}
	}
	return b
}

type apiRootJSON struct {
	CurrentUserURL string `json:"current_user_url"`
	RateLimitURL   string `json:"rate_limit_url"`
}

type metaJSON struct {
	InstalledVersion                 string   `json:"installed_version"`
	VerifiablePasswordAuthentication bool     `json:"verifiable_password_authentication"`
	Git                              []string `json:"git"`
	Hooks                            []string `json:"hooks"`
	GithubEnterpriseImporter         []string `json:"github_enterprise_importer"`
}

type viewerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		Sha string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

type refJSON struct {
	Ref    string `json:"ref"`
	Object struct {
		Sha  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

type commitListJSON struct {
	Sha    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"author"`
		Committer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"committer"`
	} `json:"commit"`
	Parents []struct {
		Sha string `json:"sha"`
	} `json:"parents"`
}

type contentItemJSON struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Sha      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding,omitempty"`
	Content  string `json:"content,omitempty"`
}

type contentPutJSON struct {
	Content struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Sha      string `json:"sha"`
		Size     int    `json:"size"`
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	} `json:"content"`
	Commit struct {
		Sha     string `json:"sha"`
		Message string `json:"message"`
	} `json:"commit"`
}

type commitOnlyJSON struct {
	Commit struct {
		Sha     string `json:"sha"`
		Message string `json:"message"`
	} `json:"commit"`
}

type mergeResultJSON struct {
	Sha     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

func handleRest(s *store, method, path, rawQuery, host string, headers http.Header, body []byte) forgeResponse {
	if host == "" {
		host = "127.0.0.1"
	}
	ctx := &restCtx{s: s, origin: "http://" + host, header: headers, query: parseQuery(rawQuery)}

	// The API root: no token needed, as api.github.com answers it.
	if method == "GET" && (path == "/api/v3" || path == "/api/v3/") {
		return jsonResponse(200, apiRootJSON{CurrentUserURL: "/api/v3/user", RateLimitURL: "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": scopes})
	}
	if !checkAuth(headers.Get("Authorization")) {
		return jsonError(401, "Requires authentication")
	}
	input, ok := parseBodyJSON(body)
	if !ok {
		return jsonError(400, "Invalid JSON body")
	}

	if method == "GET" && path == "/api/v3/meta" {
		return jsonResponse(200, metaJSON{InstalledVersion: "1.0.0", VerifiablePasswordAuthentication: false, Git: []string{"1.0.0"}, Hooks: []string{}, GithubEnterpriseImporter: []string{}}, nil)
	}
	if method == "GET" && path == "/api/v3/user" {
		return jsonResponse(200, viewerJSON{Login: viewerLogin, ID: viewerID, Name: viewerName, Email: viewerEmail, Type: "User"}, nil)
	}

	// POST /api/v3/user/repos — create a repository.
	if path == "/api/v3/user/repos" {
		if method == "POST" {
			name, nameOk := inputStr(input, "name")
			if !nameOk || name == "" || strings.ContainsAny(name, "/ \t\n\r") {
				return jsonError(422, "Invalid repository name")
			}
			if s.getRepo(viewerLogin, name) != nil {
				return jsonError(422, "Repository "+name+" already exists")
			}
			desc, _ := inputStr(input, "description")
			priv, _ := inputBool(input, "private")
			defBranch, hasDef := inputStr(input, "default_branch")
			if !hasDef {
				defBranch = "main"
			}
			repo := s.createRepo(name, desc, priv, defBranch)
			repo.mu.RLock()
			ans := jsonResponse(201, toRepoJSON(ctx, repo))
			repo.mu.RUnlock()
			return ans
		}
		if method == "GET" {
			s.mu.Lock()
			repos := make([]*repo, 0, len(s.repos))
			for _, r := range s.repos {
				repos = append(repos, r)
			}
			s.mu.Unlock()
			sort.Slice(repos, func(i, j int) bool { return repos[i].createdSeq < repos[j].createdSeq })
			page, link := paginate(ctx, repos, path)
			items := make([]repoJSON, 0, len(page))
			for _, r := range page {
				r.mu.RLock()
				items = append(items, toRepoJSON(ctx, r))
				r.mu.RUnlock()
			}
			return jsonResponse(200, items, linkHeaders(link))
		}
		return jsonError(404, "Not Found")
	}

	if !strings.HasPrefix(path, "/api/v3/repos/") {
		return jsonError(404, "Not Found")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 6 || parts[4] == "" || parts[5] == "" {
		return jsonError(404, "Not Found")
	}
	owner, err1 := url.PathUnescape(parts[4])
	repoName, err2 := url.PathUnescape(parts[5])
	if err1 != nil || err2 != nil {
		return jsonError(500, "Internal Server Error")
	}
	rest := ""
	if len(parts) > 6 {
		rest = "/" + strings.Join(parts[6:], "/")
	}
	if owner != viewerLogin {
		return jsonError(404, "Not Found")
	}
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		return jsonError(404, "Not Found")
	}

	// The repository itself.
	if rest == "" {
		if method == "GET" {
			repo.mu.RLock()
			ans := jsonResponse(200, toRepoJSON(ctx, repo))
			repo.mu.RUnlock()
			return ans
		}
		if method == "PATCH" {
			repo.mu.Lock()
			if v, ok := inputStr(input, "description"); ok {
				repo.description = v
			}
			if v, ok := inputBool(input, "private"); ok {
				repo.private = v
			}
			if v, ok := inputStr(input, "default_branch"); ok {
				repo.defaultBranch = v
			}
			repo.updatedAt = isoNow()
			ans := jsonResponse(200, toRepoJSON(ctx, repo))
			repo.mu.Unlock()
			return ans
		}
		if method == "DELETE" {
			s.mu.Lock()
			delete(s.repos, owner+"/"+repoName)
			s.mu.Unlock()
			return rawResponse(204, "application/json", []byte{})
		}
		return jsonError(404, "Not Found")
	}

	// Branches.
	if rest == "/branches" && method == "GET" {
		repo.mu.RLock()
		var refs []string
		for ref := range repo.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				refs = append(refs, ref)
			}
		}
		sort.Strings(refs)
		branches := make([]branchJSON, 0, len(refs))
		for _, ref := range refs {
			var b branchJSON
			b.Name = ref[len("refs/heads/"):]
			b.Commit.Sha = repo.refs[ref]
			b.Protected = false
			branches = append(branches, b)
		}
		repo.mu.RUnlock()
		return jsonResponse(200, branches, nil)
	}
	if strings.HasPrefix(rest, "/branches/") && method == "GET" {
		branchRaw := rest[len("/branches/"):]
		if !strings.Contains(branchRaw, "/") {
			branch, err := url.PathUnescape(branchRaw)
			if err != nil {
				return jsonError(500, "Internal Server Error")
			}
			repo.mu.RLock()
			tip, hasTip := branchTip(repo, branch)
			repo.mu.RUnlock()
			if !hasTip {
				return jsonError(404, "Not Found")
			}
			var b branchJSON
			b.Name = branch
			b.Commit.Sha = tip
			b.Protected = false
			return jsonResponse(200, b, nil)
		}
	}

	// Commits.
	if rest == "/commits" && method == "GET" {
		repo.mu.RLock()
		defer repo.mu.RUnlock()
		shaParam := ctx.query.get("sha")
		var tip string
		var hasTip bool
		if !ctx.query.has("sha") {
			tip, hasTip = branchTip(repo, repo.defaultBranch)
		} else {
			if t, ok := repo.refs["refs/heads/"+shaParam]; ok {
				tip, hasTip = t, true
			} else if _, ok := repo.objects[shaParam]; ok {
				tip, hasTip = shaParam, true
			}
		}
		if !hasTip {
			return jsonResponse(200, []any{}, nil)
		}
		walk := walkCommits(repo, []string{tip}, -1)
		page, link := paginate(ctx, walk.order, path)
		items := make([]commitListJSON, 0, len(page))
		for _, oid := range page {
			commit := parseCommit(repo.objects[oid].data)
			var item commitListJSON
			item.Sha = oid
			item.Commit.Message = commit.message
			item.Commit.Author.Name = viewerName
			item.Commit.Author.Email = viewerEmail
			item.Commit.Committer.Name = viewerName
			item.Commit.Committer.Email = viewerEmail
			item.Parents = make([]struct {
				Sha string `json:"sha"`
			}, 0, len(commit.parents))
			for _, p := range commit.parents {
				item.Parents = append(item.Parents, struct {
					Sha string `json:"sha"`
				}{Sha: p})
			}
			items = append(items, item)
		}
		return jsonResponse(200, items, linkHeaders(link))
	}

	// Git Data: refs.
	if rest == "/git/refs" && method == "GET" {
		repo.mu.RLock()
		var keys []string
		for ref := range repo.refs {
			keys = append(keys, ref)
		}
		sort.Strings(keys)
		refs := make([]refJSON, 0, len(keys))
		for _, ref := range keys {
			var rj refJSON
			rj.Ref = ref
			rj.Object.Sha = repo.refs[ref]
			rj.Object.Type = "commit"
			refs = append(refs, rj)
		}
		repo.mu.RUnlock()
		return jsonResponse(200, refs, nil)
	}
	if strings.HasPrefix(rest, "/git/refs/") && method == "GET" {
		refRaw := rest[len("/git/refs/"):]
		decoded, err := url.PathUnescape(refRaw)
		if err != nil {
			return jsonError(500, "Internal Server Error")
		}
		ref := "refs/" + decoded
		repo.mu.RLock()
		oid, ok := repo.refs[ref]
		typ := "commit"
		if ok {
			if obj, ok2 := repo.objects[oid]; ok2 {
				typ = string(obj.typ)
			}
		}
		repo.mu.RUnlock()
		if !ok {
			return jsonError(404, "Not Found")
		}
		var rj refJSON
		rj.Ref = ref
		rj.Object.Sha = oid
		rj.Object.Type = typ
		return jsonResponse(200, rj, nil)
	}
	if rest == "/git/refs" && method == "POST" {
		ref, refOk := inputStr(input, "ref")
		sha, shaOk := inputStr(input, "sha")
		repo.mu.Lock()
		_, shaExists := repo.objects[sha]
		if !refOk || !strings.HasPrefix(ref, "refs/") || !shaOk || !shaExists {
			repo.mu.Unlock()
			return jsonError(422, "Invalid ref or sha")
		}
		if _, exists := repo.refs[ref]; exists {
			repo.mu.Unlock()
			return jsonError(422, "Reference "+ref+" already exists")
		}
		repo.refs[ref] = sha
		repo.version++
		typ := "commit"
		if obj, ok := repo.objects[sha]; ok {
			typ = string(obj.typ)
		}
		repo.mu.Unlock()
		var rj refJSON
		rj.Ref = ref
		rj.Object.Sha = sha
		rj.Object.Type = typ
		return jsonResponse(201, rj, nil)
	}

	// Contents.
	if rest == "/contents" || strings.HasPrefix(rest, "/contents/") {
		return handleContents(ctx, repo, method, rest, input)
	}

	// Labels.
	if rest == "/labels" {
		if method == "GET" {
			repo.mu.RLock()
			labels := make([]labelJSON, 0, len(repo.labels))
			for _, l := range repo.labels {
				labels = append(labels, labelJSON{Name: l.name, Color: l.color, Description: l.description})
			}
			repo.mu.RUnlock()
			return jsonResponse(200, labels, nil)
		}
		if method == "POST" {
			name, nameOk := inputStr(input, "name")
			color, _ := inputStr(input, "color")
			if color == "" {
				color = "ededed"
			}
			if !nameOk || name == "" {
				return jsonError(422, "Invalid label name")
			}
			desc, _ := inputStr(input, "description")
			repo.mu.Lock()
			if existing, ok := repo.labels[name]; ok {
				ans := jsonResponse(201, labelJSON{Name: name, Color: existing.color, Description: existing.description}, nil)
				repo.mu.Unlock()
				return ans
			}
			repo.labels[name] = &label{name: name, color: color, description: desc}
			repo.version++
			repo.mu.Unlock()
			return jsonResponse(201, labelJSON{Name: name, Color: color, Description: desc}, nil)
		}
		return jsonError(404, "Not Found")
	}

	// Issues.
	if rest == "/issues" {
		if method == "GET" {
			repo.mu.RLock()
			state := ctx.query.get("state")
			if !ctx.query.has("state") {
				state = "open"
			}
			sortMode := ctx.query.get("sort")
			if !ctx.query.has("sort") {
				sortMode = "created"
			}
			direction := ctx.query.get("direction")
			if !ctx.query.has("direction") {
				direction = "desc"
			}
			labelsParam := ctx.query.get("labels")
			list := make([]*issue, 0, len(repo.issues))
			for _, iss := range repo.issues {
				if state != "all" && iss.state != state {
					continue
				}
				list = append(list, iss)
			}
			if ctx.query.has("labels") && labelsParam != "" {
				var wanted []string
				for _, s := range strings.Split(labelsParam, ",") {
					s = strings.TrimSpace(s)
					if s != "" {
						wanted = append(wanted, s)
					}
				}
				var filtered []*issue
				for _, iss := range list {
					matchAll := true
					for _, w := range wanted {
						found := false
						for _, l := range iss.labels {
							if l == w {
								found = true
								break
							}
						}
						if !found {
							matchAll = false
							break
						}
					}
					if matchAll {
						filtered = append(filtered, iss)
					}
				}
				list = filtered
			}
			if sortMode == "created" {
				sort.SliceStable(list, func(i, j int) bool {
					if direction == "asc" {
						return list[i].createdSeq < list[j].createdSeq
					}
					return list[i].createdSeq > list[j].createdSeq
				})
			}
			page, link := paginate(ctx, list, path)
			items := make([]issueJSON, 0, len(page))
			for _, iss := range page {
				items = append(items, toIssueJSON(ctx, repo, iss))
			}
			repo.mu.RUnlock()
			return jsonResponse(200, items, linkHeaders(link))
		}
		if method == "POST" {
			title, titleOk := inputStr(input, "title")
			if !titleOk || title == "" {
				return jsonError(422, "title is required")
			}
			labels := labelsFrom(input["labels"])
			repo.mu.RLock()
			for _, l := range labels {
				if _, ok := repo.labels[l]; !ok {
					repo.mu.RUnlock()
					return jsonError(422, "Label '"+l+"' does not exist")
				}
			}
			repo.mu.RUnlock()
			body, _ := inputStr(input, "body")
			iss := s.createIssue(repo, title, body, labels)
			repo.mu.RLock()
			ans := jsonResponse(201, toIssueJSON(ctx, repo, iss))
			repo.mu.RUnlock()
			return ans
		}
		return jsonError(404, "Not Found")
	}
	if strings.HasPrefix(rest, "/issues/") {
		tail := rest[len("/issues/"):]
		segments := strings.Split(tail, "/")
		if len(segments) >= 1 && len(segments) <= 2 && isAllDigits(segments[0]) {
			number, _ := strconv.Atoi(segments[0])
			isComments := len(segments) == 2 && segments[1] == "comments"
			if len(segments) == 1 || isComments {
				return handleSingleIssue(ctx, s, repo, number, isComments, method, input)
			}
		}
	}

	// Pull requests.
	if rest == "/pulls" {
		if method == "GET" {
			repo.mu.RLock()
			state := ctx.query.get("state")
			if !ctx.query.has("state") {
				state = "open"
			}
			list := make([]*pull, 0, len(repo.pulls))
			for _, p := range repo.pulls {
				if state != "all" && p.state != state {
					continue
				}
				list = append(list, p)
			}
			sort.SliceStable(list, func(i, j int) bool { return list[i].createdSeq > list[j].createdSeq })
			page, link := paginate(ctx, list, path)
			items := make([]pullJSON, 0, len(page))
			for _, p := range page {
				items = append(items, toPullJSON(ctx, repo, p, false))
			}
			repo.mu.RUnlock()
			return jsonResponse(200, items, linkHeaders(link))
		}
		if method == "POST" {
			title, titleOk := inputStr(input, "title")
			head, headOk := inputStr(input, "head")
			base, baseOk := inputStr(input, "base")
			if !titleOk || title == "" || !headOk || !baseOk {
				return jsonError(422, "title, head and base are required")
			}
			repo.mu.RLock()
			headTip, headExists := branchTip(repo, head)
			baseTip, baseExists := branchTip(repo, base)
			repo.mu.RUnlock()
			if !headExists {
				return jsonError(422, "head branch "+head+" does not exist")
			}
			if !baseExists {
				return jsonError(422, "base branch "+base+" does not exist")
			}
			repo.mu.RLock()
			for _, p := range repo.pulls {
				if p.state == "open" && p.headRef == head && p.baseRef == base {
					repo.mu.RUnlock()
					return jsonError(422, "A pull request already exists for these branches")
				}
			}
			repo.mu.RUnlock()
			body, _ := inputStr(input, "body")
			p := s.createPull(repo, title, body, head, base, headTip, baseTip)
			repo.mu.RLock()
			ans := jsonResponse(201, toPullJSON(ctx, repo, p, false))
			repo.mu.RUnlock()
			return ans
		}
		return jsonError(404, "Not Found")
	}
	if strings.HasPrefix(rest, "/pulls/") {
		tail := rest[len("/pulls/"):]
		segments := strings.Split(tail, "/")
		if len(segments) >= 1 && len(segments) <= 2 && isAllDigits(segments[0]) {
			number, _ := strconv.Atoi(segments[0])
			suffix := ""
			if len(segments) == 2 {
				suffix = segments[1]
			}
			if suffix == "" || suffix == "files" || suffix == "merge" {
				return handleSinglePull(ctx, s, repo, number, suffix, method)
			}
		}
	}

	return jsonError(404, "Not Found")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func handleSingleIssue(ctx *restCtx, s *store, repo *repo, number int, isComments bool, method string, input map[string]any) forgeResponse {
	repo.mu.RLock()
	var iss *issue
	for _, entry := range repo.issues {
		if entry.number == number {
			iss = entry
			break
		}
	}
	repo.mu.RUnlock()
	if iss == nil {
		return jsonError(404, "Not Found")
	}
	if isComments {
		if method == "GET" {
			repo.mu.RLock()
			items := make([]commentJSON, 0, len(iss.comments))
			for _, c := range iss.comments {
				items = append(items, toCommentJSON(ctx, repo, iss, c))
			}
			repo.mu.RUnlock()
			return jsonResponse(200, items, nil)
		}
		if method == "POST" {
			bodyText, bodyOk := inputStr(input, "body")
			if !bodyOk || bodyText == "" {
				return jsonError(422, "body is required")
			}
			c := s.addComment(repo, iss, bodyText)
			repo.mu.RLock()
			ans := jsonResponse(201, toCommentJSON(ctx, repo, iss, c))
			repo.mu.RUnlock()
			return ans
		}
		return jsonError(404, "Not Found")
	}
	if method == "GET" {
		cacheKey := ctx.origin + ":" + strconv.Itoa(iss.number)
		repo.mu.RLock()
		cached, ok := repo.issueBodyCache[cacheKey]
		version := repo.version
		repo.mu.RUnlock()
		if ok && cached.version == version {
			return rawResponse(200, "application/json", cached.body)
		}
		repo.mu.RLock()
		ans := jsonResponse(200, toIssueJSON(ctx, repo, iss))
		ver := repo.version
		repo.mu.RUnlock()
		repo.mu.Lock()
		if repo.version == ver {
			repo.issueBodyCache[cacheKey] = cachedBody{version: ver, body: ans.body}
		}
		repo.mu.Unlock()
		return ans
	}
	if method == "PATCH" {
		repo.mu.Lock()
		if v, ok := inputStr(input, "state"); ok && (v == "open" || v == "closed") {
			iss.state = v
		}
		if v, ok := inputStr(input, "title"); ok {
			iss.title = v
		}
		if v, ok := inputStr(input, "body"); ok {
			iss.body = v
		}
		if labelsRaw, present := input["labels"]; present {
			labels := labelsFrom(labelsRaw)
			for _, l := range labels {
				if _, ok := repo.labels[l]; !ok {
					repo.mu.Unlock()
					return jsonError(422, "Label '"+l+"' does not exist")
				}
			}
			iss.labels = labels
		}
		iss.updatedSeq = s.nextSeq()
		iss.updatedAt = isoNow()
		repo.version++
		ans := jsonResponse(200, toIssueJSON(ctx, repo, iss))
		repo.mu.Unlock()
		return ans
	}
	return jsonError(404, "Not Found")
}

func handleSinglePull(ctx *restCtx, s *store, repo *repo, number int, suffix, method string) forgeResponse {
	repo.mu.RLock()
	var p *pull
	for _, entry := range repo.pulls {
		if entry.number == number {
			p = entry
			break
		}
	}
	repo.mu.RUnlock()
	if p == nil {
		return jsonError(404, "Not Found")
	}
	if suffix == "files" && method == "GET" {
		repo.mu.RLock()
		headTip, ok := branchTip(repo, p.headRef)
		if !ok {
			headTip = p.headSha
		}
		base, hasBase := mergeBase(repo, p.baseSha, p.headSha)
		if !hasBase {
			base = p.baseSha
		}
		var baseTree, headTree *string
		if t, ok := commitTree(repo, base); ok {
			baseTree = &t
		}
		if t, ok := commitTree(repo, headTip); ok {
			headTree = &t
		}
		changes := diffTrees(repo, baseTree, headTree)
		repo.mu.RUnlock()
		return jsonResponse(200, changes, nil)
	}
	if suffix == "merge" && method == "PUT" {
		repo.mu.Lock()
		commitOid, reason, ok := mergePull(s, repo, p)
		repo.mu.Unlock()
		if !ok {
			return jsonError(405, reason)
		}
		return jsonResponse(200, mergeResultJSON{Sha: commitOid, Merged: true, Message: "Pull request successfully merged"}, nil)
	}
	if suffix == "" && method == "GET" {
		repo.mu.RLock()
		ans := jsonResponse(200, toPullJSON(ctx, repo, p, true))
		repo.mu.RUnlock()
		return ans
	}
	return jsonError(404, "Not Found")
}

func handleContents(ctx *restCtx, repo *repo, method, rest string, input map[string]any) forgeResponse {
	rawPath := ""
	if strings.HasPrefix(rest, "/contents/") {
		rawPath = rest[len("/contents/"):]
	}
	var contentPathParts []string
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == "" {
			continue
		}
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return jsonError(500, "Internal Server Error")
		}
		contentPathParts = append(contentPathParts, decoded)
	}
	contentPath := strings.Join(contentPathParts, "/")
	var refParam *string
	if ctx.query.has("ref") {
		v := ctx.query.get("ref")
		refParam = &v
	} else if v, ok := inputStr(input, "branch"); ok {
		refParam = &v
	}
	branch := repo.defaultBranch
	if refParam != nil {
		branch = *refParam
	}
	repo.mu.RLock()
	tip, hasTip := branchTip(repo, branch)
	if !hasTip {
		if _, ok := repo.objects[branch]; ok {
			tip, hasTip = branch, true
		}
	}
	repo.mu.RUnlock()

	if method == "GET" {
		if !hasTip {
			return jsonError(404, "Not Found")
		}
		repo.mu.RLock()
		defer repo.mu.RUnlock()
		treeOid, ok := commitTree(repo, tip)
		if !ok {
			return jsonError(404, "Not Found")
		}
		if contentPath == "" {
			entries, err := parseTree(repo.objects[treeOid].data)
			if err != nil {
				return jsonError(500, "Internal Server Error")
			}
			items := make([]contentItemJSON, 0, len(entries))
			for _, e := range entries {
				size := 0
				typ := "file"
				if e.mode == "40000" {
					typ = "dir"
				} else if obj, ok := repo.objects[e.oid]; ok {
					size = len(obj.data)
				}
				items = append(items, contentItemJSON{Type: typ, Name: e.name, Path: e.name, Sha: e.oid, Size: size})
			}
			return jsonResponse(200, items, nil)
		}
		flat := flattenTree(repo, treeOid)
		if file, ok := flat.m[contentPath]; ok {
			var blobData []byte
			if blob, ok := repo.objects[file.oid]; ok {
				blobData = blob.data
			}
			name := contentPath
			if i := strings.LastIndex(contentPath, "/"); i >= 0 {
				name = contentPath[i+1:]
			}
			return jsonResponse(200, contentItemJSON{
				Type:     "file",
				Encoding: "base64",
				Content:  base64.StdEncoding.EncodeToString(blobData),
				Sha:      file.oid,
				Size:     len(blobData),
				Name:     name,
				Path:     contentPath,
			}, nil)
		}
		prefix := contentPath + "/"
		type dirChild struct {
			name string
			typ  string
			oid  string
			size int
		}
		var children []dirChild
		seenChild := make(map[string]bool)
		for _, p := range flat.order {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			restPath := p[len(prefix):]
			first := strings.SplitN(restPath, "/", 2)[0]
			if seenChild[first] {
				continue
			}
			seenChild[first] = true
			if strings.Contains(restPath, "/") {
				children = append(children, dirChild{name: first, typ: "dir"})
			} else {
				entry := flat.m[p]
				size := 0
				if blob, ok := repo.objects[entry.oid]; ok {
					size = len(blob.data)
				}
				children = append(children, dirChild{name: first, typ: "file", oid: entry.oid, size: size})
			}
		}
		if len(children) == 0 {
			return jsonError(404, "Not Found")
		}
		treeOids := treeOidAtPath(repo, treeOid, contentPath)
		items := make([]contentItemJSON, 0, len(children))
		for _, c := range children {
			sha := c.oid
			if c.typ == "dir" {
				sha = ""
				if treeOids != nil {
					sha = treeOids[c.name]
				}
			}
			items = append(items, contentItemJSON{Type: c.typ, Name: c.name, Path: contentPath + "/" + c.name, Sha: sha, Size: c.size})
		}
		return jsonResponse(200, items, nil)
	}
	if method == "PUT" {
		message, _ := inputStr(input, "message")
		if message == "" {
			message = "Update file"
		}
		contentB64, contentOk := inputStr(input, "content")
		if !contentOk {
			return jsonError(422, "content is required")
		}
		content := decodeBase64Lenient(contentB64)
		targetBranch, _ := inputStr(input, "branch")
		if targetBranch == "" {
			targetBranch = repo.defaultBranch
		}
		repo.mu.Lock()
		defer repo.mu.Unlock()
		writeBranch := targetBranch
		if _, ok := branchTip(repo, targetBranch); !ok {
			if len(repo.refs) == 0 {
				writeBranch = repo.defaultBranch
			} else {
				defaultTip, ok := branchTip(repo, repo.defaultBranch)
				if !ok {
					return jsonError(422, "cannot create branch from an unborn repository")
				}
				repo.refs["refs/heads/"+targetBranch] = defaultTip
				repo.version++
			}
		}
		tipNow, hasTipNow := branchTip(repo, writeBranch)
		var treeNow string
		var hasTreeNow bool
		if hasTipNow {
			treeNow, hasTreeNow = commitTree(repo, tipNow)
		}
		var flatNow *flatTree
		if hasTreeNow {
			flatNow = flattenTree(repo, treeNow)
		} else {
			flatNow = &flatTree{m: map[string]flatEntry{}}
		}
		existing, exists := flatNow.m[contentPath]
		givenSha, hasGivenSha := inputStr(input, "sha")
		if exists && !hasGivenSha {
			return jsonError(422, "File already exists")
		}
		if !exists && hasGivenSha {
			return jsonError(404, "Not Found")
		}
		if exists && hasGivenSha && existing.oid != givenSha {
			return jsonError(409, "sha does not match")
		}
		commitOid, blobOid := commitFile(repo, writeBranch, contentPath, content, false, message)
		status := 200
		if !exists {
			status = 201
		}
		name := contentPath
		if i := strings.LastIndex(contentPath, "/"); i >= 0 {
			name = contentPath[i+1:]
		}
		var resp contentPutJSON
		resp.Content.Name = name
		resp.Content.Path = contentPath
		if blobOid != nil {
			resp.Content.Sha = *blobOid
		}
		resp.Content.Size = len(content)
		resp.Content.Type = "file"
		resp.Content.Encoding = "base64"
		resp.Content.Content = base64.StdEncoding.EncodeToString(content)
		resp.Commit.Sha = commitOid
		resp.Commit.Message = message
		return jsonResponse(status, resp, nil)
	}
	if method == "DELETE" {
		givenSha, _ := inputStr(input, "sha")
		message, _ := inputStr(input, "message")
		if message == "" {
			message = "Delete file"
		}
		targetBranch, _ := inputStr(input, "branch")
		if targetBranch == "" {
			targetBranch = repo.defaultBranch
		}
		repo.mu.Lock()
		defer repo.mu.Unlock()
		tipNow, hasTipNow := branchTip(repo, targetBranch)
		if !hasTipNow {
			return jsonError(404, "Not Found")
		}
		treeNow, ok := commitTree(repo, tipNow)
		if !ok {
			return jsonError(404, "Not Found")
		}
		existing, exists := flattenTree(repo, treeNow).m[contentPath]
		if !exists {
			return jsonError(404, "Not Found")
		}
		if givenSha != "" && existing.oid != givenSha {
			return jsonError(409, "sha does not match")
		}
		commitOid, _ := commitFile(repo, targetBranch, contentPath, nil, true, message)
		var resp commitOnlyJSON
		resp.Commit.Sha = commitOid
		resp.Commit.Message = message
		return jsonResponse(200, resp, nil)
	}
	return jsonError(404, "Not Found")
}
