// MCP port of src/mcp.ts: Model Context Protocol over Streamable HTTP
// (plain JSON responses): the modern 2026-07-28 era via server/discover,
// with the legacy initialize handshake as a fallback. Six tools, all acting
// on the same store REST reads. Unknown tools are JSON-RPC -32602/-32601,
// never HTTP 500; missing arguments are isError:true; a non-JSON body is
// HTTP 400 with -32700; no Authorization header is 401. tools/list is a
// frozen array, byte-identical on every call.

package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const mcpServerName = "forge-core-clone"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpProp struct {
	name string
	typ  string
	desc string
}

type mcpToolDef struct {
	name     string
	desc     string
	props    []mcpProp
	required []string
}

var mcpTools = []mcpToolDef{
	{
		name: "create_repo",
		desc: "Create a repository owned by the authenticated user.",
		props: []mcpProp{
			{"owner", "string", "The repository owner (the authenticated user)."},
			{"name", "string", "The repository name."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name: "get_repo",
		desc: "Get a repository by owner and name.",
		props: []mcpProp{
			{"owner", "string", "The repository owner."},
			{"repo", "string", "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_issue",
		desc: "Create an issue in a repository.",
		props: []mcpProp{
			{"owner", "string", "The repository owner."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The issue title."},
			{"body", "string", "The issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name: "list_issues",
		desc: "List the issues of a repository.",
		props: []mcpProp{
			{"owner", "string", "The repository owner."},
			{"repo", "string", "The repository name."},
			{"state", "string", "Filter by state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name: "create_pull_request",
		desc: "Create a pull request from a head branch into a base branch.",
		props: []mcpProp{
			{"owner", "string", "The repository owner."},
			{"repo", "string", "The repository name."},
			{"title", "string", "The pull request title."},
			{"body", "string", "The pull request body."},
			{"head", "string", "The head branch name."},
			{"base", "string", "The base branch name."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name: "merge_pull_request",
		desc: "Merge a pull request by number.",
		props: []mcpProp{
			{"owner", "string", "The repository owner."},
			{"repo", "string", "The repository name."},
			{"pull_number", "integer", "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolsJSON() []any {
	out := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		props := newOmap()
		for _, p := range t.props {
			props.set(p.name, om("type", p.typ, "description", p.desc))
		}
		out = append(out, om(
			"name", t.name,
			"description", t.desc,
			"inputSchema", om(
				"type", "object",
				"properties", props,
				"required", t.required,
			),
		))
	}
	return out
}

func mcpToolResult(value any) *omap {
	text, ok := value.(string)
	if !ok {
		text = jsonString(value)
	}
	return om(
		"content", []any{om("type", "text", "text", text)},
		"structuredContent", value,
		"isError", false,
	)
}

func mcpToolError(message string) *omap {
	return om(
		"content", []any{om("type", "text", "text", message)},
		"isError", true,
	)
}

// mcpPick returns the first present argument under any of the given names.
func mcpPick(args map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		if v, ok := args[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func mcpRepoJSON(r *repo) *omap {
	return om(
		"id", r.id,
		"name", r.name,
		"full_name", r.owner+"/"+r.name,
		"owner", om("login", r.owner),
		"private", r.private,
		"description", r.description,
		"default_branch", r.defaultBranch,
	)
}

func mcpIssueJSON(iss *issue) *omap {
	labels := iss.labels
	if labels == nil {
		labels = []string{}
	}
	return om(
		"id", iss.id,
		"number", iss.number,
		"title", iss.title,
		"body", iss.body,
		"state", iss.state,
		"labels", labels,
		"created_at", iss.createdAt,
		"updated_at", iss.updatedAt,
	)
}

func mcpPullJSON(p *pull) *omap {
	return om(
		"id", p.id,
		"number", p.number,
		"title", p.title,
		"body", p.body,
		"state", p.state,
		"merged", p.merged,
		"head", om("ref", p.headRef, "sha", p.headSha),
		"base", om("ref", p.baseRef, "sha", p.baseSha),
	)
}

func mcpNeedRepo(s *store, args map[string]any) (*repo, string) {
	owner, _ := mcpPick(args, "owner", "owner_login")
	repoName, _ := mcpPick(args, "repo", "repository", "repo_name")
	ownerStr, ok1 := owner.(string)
	nameStr, ok2 := repoName.(string)
	if !ok1 || !ok2 {
		return nil, "owner and repo are required"
	}
	if ownerStr != viewerLogin {
		return nil, "repository " + ownerStr + "/" + nameStr + " not found"
	}
	r := s.getRepo(ownerStr, nameStr)
	if r == nil {
		return nil, "repository " + ownerStr + "/" + nameStr + " not found"
	}
	return r, ""
}

func mcpFindTool(name string) *mcpToolDef {
	for i := range mcpTools {
		if mcpTools[i].name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

func mcpCallTool(s *store, name string, args map[string]any) *omap {
	tool := mcpFindTool(name)
	if tool == nil {
		return mcpToolError("unknown tool " + name)
	}
	var missing []string
	for _, req := range tool.required {
		v, ok := args[req]
		if !ok || v == nil || v == "" {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return mcpToolError("missing required argument: " + strings.Join(missing, ", "))
	}

	switch name {
	case "create_repo":
		repoName, _ := mcpPick(args, "name", "repo", "repository")
		nameStr, ok := repoName.(string)
		if !ok || nameStr == "" {
			return mcpToolError("name is required")
		}
		if s.getRepo(viewerLogin, nameStr) != nil {
			return mcpToolError("repository " + nameStr + " already exists")
		}
		desc := ""
		if d, ok := mcpPick(args, "description"); ok {
			if ds, ok := d.(string); ok {
				desc = ds
			}
		}
		priv := false
		if pv, ok := mcpPick(args, "private", "is_private"); ok && pv == true {
			priv = true
		}
		r := s.createRepo(nameStr, desc, priv, "main")
		return mcpToolResult(mcpRepoJSON(r))

	case "get_repo":
		r, errStr := mcpNeedRepo(s, args)
		if errStr != "" {
			return mcpToolError(errStr)
		}
		return mcpToolResult(mcpRepoJSON(r))

	case "create_issue":
		r, errStr := mcpNeedRepo(s, args)
		if errStr != "" {
			return mcpToolError(errStr)
		}
		title, _ := mcpPick(args, "title")
		titleStr, ok := title.(string)
		if !ok || titleStr == "" {
			return mcpToolError("title is required")
		}
		body := ""
		if b, ok := mcpPick(args, "body", "description"); ok {
			if bs, ok := b.(string); ok {
				body = bs
			}
		}
		iss := s.createIssue(r, titleStr, body, nil)
		return mcpToolResult(mcpIssueJSON(iss))

	case "list_issues":
		r, errStr := mcpNeedRepo(s, args)
		if errStr != "" {
			return mcpToolError(errStr)
		}
		state, _ := mcpPick(args, "state", "status")
		stateStr, _ := state.(string)
		r.mu.RLock()
		list := append([]*issue(nil), r.issues...)
		items := make([]any, 0, len(list))
		for _, iss := range list {
			if stateStr == "open" || stateStr == "closed" {
				if iss.state != stateStr {
					continue
				}
			}
			items = append(items, mcpIssueJSON(iss))
		}
		r.mu.RUnlock()
		return om(
			"content", []any{om("type", "text", "text", jsonString(items))},
			"structuredContent", om("issues", items),
			"isError", false,
		)

	case "create_pull_request":
		r, errStr := mcpNeedRepo(s, args)
		if errStr != "" {
			return mcpToolError(errStr)
		}
		title, _ := mcpPick(args, "title")
		head, _ := mcpPick(args, "head", "head_ref", "head_branch")
		base, _ := mcpPick(args, "base", "base_ref", "base_branch")
		titleStr, ok1 := title.(string)
		headStr, ok2 := head.(string)
		baseStr, ok3 := base.(string)
		if !ok1 || !ok2 || !ok3 {
			return mcpToolError("title, head and base are required")
		}
		r.mu.RLock()
		headTip, headOK := r.refs["refs/heads/"+headStr]
		baseTip, baseOK := r.refs["refs/heads/"+baseStr]
		var duplicate *pull
		for _, p := range r.pulls {
			if p.state == "open" && p.headRef == headStr && p.baseRef == baseStr {
				duplicate = p
				break
			}
		}
		r.mu.RUnlock()
		if !headOK {
			return mcpToolError("head branch " + headStr + " does not exist")
		}
		if !baseOK {
			return mcpToolError("base branch " + baseStr + " does not exist")
		}
		if duplicate != nil {
			return mcpToolError("A pull request already exists for these branches")
		}
		body := ""
		if b, ok := mcpPick(args, "body", "description"); ok {
			if bs, ok := b.(string); ok {
				body = bs
			}
		}
		p := s.createPull(r, titleStr, body, headStr, baseStr, headTip, baseTip)
		return mcpToolResult(mcpPullJSON(p))

	case "merge_pull_request":
		r, errStr := mcpNeedRepo(s, args)
		if errStr != "" {
			return mcpToolError(errStr)
		}
		number, _ := mcpPick(args, "pull_number", "pull_request_number", "pr_number", "number")
		pullNumber, isInt := mcpParseNumber(number)
		if !isInt {
			return mcpToolError("pull_number is required")
		}
		r.mu.RLock()
		var p *pull
		for _, entry := range r.pulls {
			if entry.number == int(pullNumber) {
				p = entry
				break
			}
		}
		r.mu.RUnlock()
		if p == nil {
			return mcpToolError("pull request #" + mcpFormatNumber(pullNumber) + " not found")
		}
		r.mu.Lock()
		commitOid, reason, ok := mergePull(s, r, p)
		r.mu.Unlock()
		if !ok {
			return mcpToolError(reason)
		}
		return mcpToolResult(om("number", p.number, "merged", true, "state", "closed", "sha", commitOid))
	}

	return mcpToolError("unknown tool " + name)
}

// mcpParseNumber converts a JSON-RPC argument to a pull number the way
// Number(value) does, reporting whether it is an integer.
func mcpParseNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		if math.Trunc(t) == t && !math.IsInf(t, 0) && !math.IsNaN(t) {
			return t, true
		}
		return 0, false
	case string:
		n, err := strconv.ParseFloat(t, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func mcpFormatNumber(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func mcpRPCResult(id any, result any) *omap {
	return om("jsonrpc", "2.0", "id", mcpIDOrNull(id), "result", result)
}

func mcpRPCError(id any, code int, message string, data ...any) *omap {
	errObj := om("code", code, "message", message)
	if len(data) > 0 && data[0] != nil {
		errObj.set("data", data[0])
	}
	return om("jsonrpc", "2.0", "id", mcpIDOrNull(id), "error", errObj)
}

func mcpIDOrNull(id any) any {
	if id == nil {
		return nil
	}
	return id
}

// mcpHandleOne handles one JSON-RPC request object. It returns the response
// object, or (nil, true) for a notification (no response).
func mcpHandleOne(s *store, req map[string]any) (any, bool) {
	id := req["id"]
	method, ok := req["method"].(string)
	if !ok {
		return mcpRPCError(id, -32600, "Invalid Request"), false
	}
	params := map[string]any{}
	if p, ok := req["params"].(map[string]any); ok && p != nil {
		params = p
	}

	switch {
	case method == "server/discover":
		return mcpRPCResult(id, om(
			"protocolVersion", "2026-07-28",
			"supportedVersions", mcpSupportedVersions,
			"serverInfo", om("name", mcpServerName, "version", mcpServerVersion),
			"capabilities", om("tools", newOmap()),
			"_meta", om("io.modelcontextprotocol/serverInfo", om("name", mcpServerName, "version", mcpServerVersion)),
		)), false

	case method == "initialize":
		wanted := "2025-11-25"
		if v, ok := params["protocolVersion"].(string); ok {
			wanted = v
		}
		negotiated := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if v == wanted {
				negotiated = wanted
				break
			}
		}
		return mcpRPCResult(id, om(
			"protocolVersion", negotiated,
			"capabilities", om("tools", newOmap()),
			"serverInfo", om("name", mcpServerName, "version", mcpServerVersion),
		)), false

	case method == "notifications/initialized" || strings.HasPrefix(method, "notifications/"):
		return nil, true

	case method == "ping":
		return mcpRPCResult(id, newOmap()), false

	case method == "tools/list":
		return mcpRPCResult(id, om("tools", mcpToolsJSON())), false

	case method == "tools/call":
		toolName, ok := params["name"].(string)
		if !ok || toolName == "" {
			return mcpRPCError(id, -32602, "tools/call needs a tool name"), false
		}
		args := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok && a != nil {
			args = a
		}
		if mcpFindTool(toolName) == nil {
			return mcpRPCError(id, -32602, "unknown tool "+toolName), false
		}
		return mcpRPCResult(id, mcpCallTool(s, toolName, args)), false
	}

	return mcpRPCError(id, -32601, "Method not found: "+method), false
}

func handleMcp(s *store, method, path string, headers http.Header, body []byte) forgeResponse {
	if method != "POST" {
		return jsonError(http.StatusNotFound, "Not Found")
	}
	if !checkAuth(headers.Get("Authorization")) {
		return jsonError(http.StatusUnauthorized, "Requires authentication")
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return jsonResponse(http.StatusBadRequest, mcpRPCError(nil, -32700, "Parse error"))
	}
	if arr, ok := parsed.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				results = append(results, mcpRPCError(nil, -32600, "Invalid Request"))
				continue
			}
			answer, isNotification := mcpHandleOne(s, entryMap)
			if !isNotification {
				results = append(results, answer)
			}
		}
		return jsonResponse(http.StatusOK, results)
	}
	reqMap, ok := parsed.(map[string]any)
	if !ok {
		return jsonResponse(http.StatusBadRequest, mcpRPCError(nil, -32700, "Parse error"))
	}
	answer, isNotification := mcpHandleOne(s, reqMap)
	if isNotification {
		return forgeResponse{status: http.StatusAccepted, headers: map[string]string{"x-github-request-id": "forge-mcp"}, body: []byte{}}
	}
	return jsonResponse(http.StatusOK, answer)
}
