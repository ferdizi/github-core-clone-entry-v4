// GraphQL port of src/graphql.ts: a small parser for the query/mutation
// subset the documents actually send (variables, inline inputs, inline
// enums, inline fragments, aliases, operation batching) and an executor over
// the shared store. Unknown fields are a 200 with an `errors` array naming
// the field; an unknown repository is data.repository = null with no errors;
// no token is 401.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------- parser

type tokenKind string

const (
	tokName   tokenKind = "name"
	tokString tokenKind = "string"
	tokInt    tokenKind = "int"
	tokFloat  tokenKind = "float"
	tokPunct  tokenKind = "punct"
	tokEOF    tokenKind = "eof"
)

type gqlToken struct {
	kind  tokenKind
	value string
}

// gqlParsePanic is thrown (panicked) by the parser on malformed input and
// recovered in executeOne, mirroring the TypeScript throws.
type gqlParsePanic struct{ msg string }

func gqlParseFail(format string, args ...any) {
	panic(gqlParsePanic{msg: fmt.Sprintf(format, args...)})
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func tokenize(src string) []gqlToken {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@", rune(c)) {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				if src[j] == '.' {
					isFloat = true
				}
				j++
			}
			kind := tokInt
			if isFloat {
				kind = tokFloat
			}
			tokens = append(tokens, gqlToken{kind: kind, value: src[i:j]})
			i = j
			continue
		}
		if isNameStart(c) {
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		gqlParseFail("unexpected character %s", strconv.Quote(string(c)))
	}
	tokens = append(tokens, gqlToken{kind: tokEOF, value: ""})
	return tokens
}

// valueNode is a parsed GraphQL literal / variable reference.
type valueNode struct {
	kind    string // var, string, int, float, bool, null, enum, list, object
	strVal  string
	numVal  float64
	boolVal bool
	items   []valueNode
	fields  map[string]valueNode
}

// selection is one field or inline fragment in a selection set.
type selection struct {
	isInline      bool
	typeCondition string
	alias         string
	name          string
	args          map[string]valueNode
	selections    []*selection
}

type gqlOperation struct {
	kind       string // query | mutation
	name       string
	hasName    bool
	variables  map[string]valueNode
	selections []*selection
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }

func (p *gqlParser) next() gqlToken {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) expectPunct(value string) {
	t := p.next()
	if t.kind != tokPunct || t.value != value {
		gqlParseFail("expected %s, got %s", strconv.Quote(value), strconv.Quote(t.value))
	}
}

func (p *gqlParser) expectName() string {
	t := p.next()
	if t.kind != tokName {
		gqlParseFail("expected a name, got %s", strconv.Quote(t.value))
	}
	return t.value
}

func (p *gqlParser) parseDocument() []*gqlOperation {
	var ops []*gqlOperation
	for p.peek().kind != tokEOF {
		ops = append(ops, p.parseOperation())
	}
	return ops
}

func (p *gqlParser) parseOperation() *gqlOperation {
	kind := "query"
	first := p.peek()
	if first.kind == tokName && (first.value == "query" || first.value == "mutation") {
		kind = p.next().value
	}
	op := &gqlOperation{kind: kind, variables: map[string]valueNode{}}
	if p.peek().kind == tokName {
		op.name = p.next().value
		op.hasName = true
	}
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			p.expectPunct("$")
			varName := p.expectName()
			p.expectPunct(":")
			p.parseType()
			if p.peek().kind == tokPunct && p.peek().value == "=" {
				p.next()
				op.variables[varName] = p.parseValue()
			} else {
				op.variables[varName] = valueNode{kind: "null"}
			}
		}
		p.expectPunct(")")
	}
	op.selections = p.parseSelectionSet()
	return op
}

func (p *gqlParser) parseType() {
	if p.peek().kind == tokPunct && p.peek().value == "[" {
		p.next()
		p.parseType()
		p.expectPunct("]")
	} else {
		p.expectName()
	}
	if p.peek().kind == tokPunct && p.peek().value == "!" {
		p.next()
	}
}

func (p *gqlParser) parseSelectionSet() []*selection {
	p.expectPunct("{")
	var sels []*selection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		sels = append(sels, p.parseSelection())
	}
	p.expectPunct("}")
	return sels
}

func (p *gqlParser) parseSelection() *selection {
	if p.peek().kind == tokPunct && p.peek().value == "..." {
		p.next()
		sel := &selection{isInline: true}
		if p.peek().kind == tokName && p.peek().value == "on" {
			p.next()
			sel.typeCondition = p.expectName()
		} else if p.peek().kind == tokName {
			gqlParseFail("named fragment spreads are not supported")
		}
		sel.selections = p.parseSelectionSet()
		return sel
	}
	first := p.expectName()
	sel := &selection{name: first, args: map[string]valueNode{}}
	if p.peek().kind == tokPunct && p.peek().value == ":" {
		p.next()
		sel.alias = first
		sel.name = p.expectName()
	}
	if p.peek().kind == tokPunct && p.peek().value == "(" {
		p.next()
		for !(p.peek().kind == tokPunct && p.peek().value == ")") {
			argName := p.expectName()
			p.expectPunct(":")
			sel.args[argName] = p.parseValue()
		}
		p.expectPunct(")")
	}
	if p.peek().kind == tokPunct && p.peek().value == "{" {
		sel.selections = p.parseSelectionSet()
	}
	return sel
}

func (p *gqlParser) parseValue() valueNode {
	t := p.next()
	if t.kind == tokPunct && t.value == "$" {
		return valueNode{kind: "var", strVal: p.expectName()}
	}
	if t.kind == tokString {
		return valueNode{kind: "string", strVal: t.value}
	}
	if t.kind == tokInt || t.kind == tokFloat {
		n, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			n = math.NaN()
		}
		kind := "int"
		if t.kind == tokFloat {
			kind = "float"
		}
		return valueNode{kind: kind, numVal: n}
	}
	if t.kind == tokName {
		switch t.value {
		case "true":
			return valueNode{kind: "bool", boolVal: true}
		case "false":
			return valueNode{kind: "bool", boolVal: false}
		case "null":
			return valueNode{kind: "null"}
		default:
			return valueNode{kind: "enum", strVal: t.value}
		}
	}
	if t.kind == tokPunct && t.value == "[" {
		var items []valueNode
		for !(p.peek().kind == tokPunct && p.peek().value == "]") {
			items = append(items, p.parseValue())
		}
		p.expectPunct("]")
		return valueNode{kind: "list", items: items}
	}
	if t.kind == tokPunct && t.value == "{" {
		fields := map[string]valueNode{}
		for !(p.peek().kind == tokPunct && p.peek().value == "}") {
			key := p.expectName()
			p.expectPunct(":")
			fields[key] = p.parseValue()
		}
		p.expectPunct("}")
		return valueNode{kind: "object", fields: fields}
	}
	gqlParseFail("unexpected value token %s", strconv.Quote(t.value))
	return valueNode{}
}

func evalValue(node valueNode, variables map[string]any) any {
	switch node.kind {
	case "var":
		return variables[node.strVal]
	case "string":
		return node.strVal
	case "int", "float":
		return node.numVal
	case "bool":
		return node.boolVal
	case "null":
		return nil
	case "enum":
		return node.strVal
	case "list":
		out := make([]any, 0, len(node.items))
		for _, item := range node.items {
			out = append(out, evalValue(item, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, v := range node.fields {
			out[k] = evalValue(v, variables)
		}
		return out
	}
	return nil
}

// ------------------------------------------------------------------- schema

var gqlTypeFields = map[string][]string{
	"Issue":                  {"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author", "labels", "comments", "closed", "closedAt"},
	"PullRequest":            {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "url", "createdAt", "updatedAt", "author", "additions", "deletions"},
	"Repository":             {"id", "databaseId", "name", "nameWithOwner", "owner", "description", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "createdAt", "updatedAt", "isPrivate", "forkCount", "stargazerCount"},
	"User":                   {"id", "login", "name", "email", "avatarUrl"},
	"IssueConnection":        {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":  {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile": {"path", "additions", "deletions", "changeType"},
	"IssueComment":           {"id", "body", "author", "createdAt", "updatedAt"},
	"Ref":                    {"name", "target"},
	"Label":                  {"id", "name", "color", "description"},
	"PageInfo":               {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
}

var gqlTypeEnums = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "LABEL", "MILESTONE", "PROJECT", "REGISTRY_PACKAGE"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"OrderDirection":         {"ASC", "DESC"},
	"IssueOrderField":        {"COMMENTS", "CREATED_AT", "UPDATED_AT"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
}

var gqlQueryFields = []string{"viewer", "repository", "node", "__type", "__schema"}
var gqlMutationFields = []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}

// ------------------------------------------------------------------- values

// gqlObj is one typed GraphQL value: a __typename plus, for Repository /
// Issue / PullRequest, the live store pointers the resolvers read from, and
// for plain values (User, __Type, connections, payloads, edges) a property
// map.
type gqlObj struct {
	typename string
	repo     *repo
	issue    *issue
	pull     *pull
	props    *omap
}

func (o *gqlObj) get(field string) any {
	if o.props == nil {
		return nil
	}
	v, _ := o.props.get(field)
	return v
}

// toJSON renders the object's plain properties (what a query without
// sub-selections would see).
func (o *gqlObj) toJSON() any {
	out := newOmap()
	out.set("__typename", o.typename)
	if o.props != nil {
		for _, k := range o.props.keys {
			v, _ := o.props.get(k)
			out.set(k, gqlToJSON(v))
		}
	}
	return out
}

// gqlToJSON converts a resolved value for raw storage (no sub-selections):
// typed objects become their plain properties, arrays map element-wise.
func gqlToJSON(v any) any {
	switch t := v.(type) {
	case *gqlObj:
		return t.toJSON()
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, gqlToJSON(e))
		}
		return out
	default:
		return v
	}
}

func gqlUserValue(login string) *gqlObj {
	name := login
	email := login + "@example.invalid"
	if login == viewerLogin {
		name = viewerName
		email = viewerEmail
	}
	return &gqlObj{typename: "User", props: om(
		"login", login,
		"id", globalID("User", login),
		"name", name,
		"email", email,
		"avatarUrl", "",
	)}
}

func gqlRepoValue(r *repo) *gqlObj            { return &gqlObj{typename: "Repository", repo: r} }
func gqlIssueValue(r *repo, i *issue) *gqlObj { return &gqlObj{typename: "Issue", repo: r, issue: i} }
func gqlPullValue(r *repo, p *pull) *gqlObj {
	return &gqlObj{typename: "PullRequest", repo: r, pull: p}
}

func gqlEncodeCursor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

var gqlCursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func gqlDecodeCursor(v any) (int, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	m := gqlCursorRe.FindStringSubmatch(string(raw))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// gqlError is a field error: reported in the errors array with the field
// nulled, never an HTTP error.
type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }

// gqlIDPart reads one numeric part of a decoded global id.
func gqlIDPart(dec decodedGlobalID, i int) (int, bool) {
	if i >= len(dec.parts) {
		return 0, false
	}
	n, err := strconv.Atoi(dec.parts[i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ------------------------------------------------------------------- executor

type gqlExecCtx struct{ store *store }

func gqlKnownFields(typ string) []string {
	if typ == "Query" {
		return gqlQueryFields
	}
	if typ == "Mutation" {
		return gqlMutationFields
	}
	if typ == "__Type" {
		return []string{"name", "fields", "enumValues", "kind"}
	}
	if fields, ok := gqlTypeFields[typ]; ok {
		return fields
	}
	return []string{}
}

var gqlTypedObjects = map[string]bool{
	"Query": true, "Mutation": true, "__Type": true,
	"Repository": true, "Issue": true, "PullRequest": true, "User": true,
	"IssueConnection": true, "PullRequestConnection": true, "PageInfo": true,
	"Ref": true, "Label": true, "IssueComment": true, "PullRequestChangedFile": true,
}

func gqlExecuteSelections(ctx *gqlExecCtx, parentType string, parent *gqlObj, sels []*selection, variables map[string]any, errors *[]*omap) *omap {
	if parent == nil {
		return nil
	}
	out := newOmap()
	for _, sel := range sels {
		if sel.isInline {
			if sel.typeCondition != "" && sel.typeCondition != parentType {
				continue
			}
			nested := gqlExecuteSelections(ctx, parentType, parent, sel.selections, variables, errors)
			if nested != nil {
				for _, k := range nested.keys {
					v, _ := nested.get(k)
					out.set(k, v)
				}
			}
			continue
		}
		key := sel.alias
		if key == "" {
			key = sel.name
		}
		args := map[string]any{}
		for name, node := range sel.args {
			args[name] = evalValue(node, variables)
		}
		value, err := gqlResolveField(ctx, parentType, parent, sel.name, args)
		if err != nil {
			if ge, ok := err.(*gqlError); ok {
				*errors = append(*errors, om("message", ge.msg))
				out.set(key, nil)
				continue
			}
			panic(err)
		}
		if sel.selections != nil {
			if value == nil {
				out.set(key, nil)
			} else if arr, ok := value.([]any); ok {
				mapped := make([]any, 0, len(arr))
				for _, entry := range arr {
					if obj, ok := entry.(*gqlObj); ok {
						mapped = append(mapped, gqlExecuteSelections(ctx, obj.typename, obj, sel.selections, variables, errors))
					} else {
						mapped = append(mapped, entry)
					}
				}
				out.set(key, mapped)
			} else if obj, ok := value.(*gqlObj); ok {
				out.set(key, gqlExecuteSelections(ctx, obj.typename, obj, sel.selections, variables, errors))
			} else {
				out.set(key, value)
			}
		} else {
			out.set(key, gqlToJSON(value))
		}
	}
	return out
}

func gqlResolveField(ctx *gqlExecCtx, parentType string, parent *gqlObj, field string, args map[string]any) (any, error) {
	if parent == nil {
		return nil, nil
	}
	// A field the schema does not hold on a typed object is an error, so a
	// query for a made-up field is reported, not silently nulled.
	if gqlTypedObjects[parentType] {
		known := false
		for _, f := range gqlKnownFields(parentType) {
			if f == field {
				known = true
				break
			}
		}
		if !known {
			return nil, &gqlError{msg: `Cannot query field "` + field + `" on type "` + parentType + `".`}
		}
	}
	switch parentType {
	case "Query":
		return gqlResolveQuery(ctx, field, args), nil
	case "Mutation":
		return gqlResolveMutation(ctx, field, args)
	case "__Type":
		return gqlResolveTypeField(parent, field), nil
	case "Repository":
		return gqlResolveRepository(ctx, parent.repo, field, args), nil
	case "Issue":
		return gqlResolveIssue(parent.repo, parent.issue, field), nil
	case "PullRequest":
		return gqlResolvePull(parent.repo, parent.pull, field, args), nil
	case "User":
		return parent.get(field), nil
	case "IssueConnection", "PullRequestConnection":
		return gqlResolveConnection(parent, field), nil
	}
	// Payload objects, __TypeField/__TypeEnumValue entries, edges and other
	// plain values resolve from their own properties without validation.
	return parent.get(field), nil
}

func gqlResolveQuery(ctx *gqlExecCtx, field string, args map[string]any) any {
	switch field {
	case "viewer":
		return gqlUserValue(viewerLogin)
	case "repository":
		owner, ok1 := args["owner"].(string)
		name, ok2 := args["name"].(string)
		if !ok1 || !ok2 {
			return nil
		}
		r := ctx.store.getRepo(owner, name)
		if r == nil {
			return nil
		}
		return gqlRepoValue(r)
	case "node":
		id, ok := args["id"].(string)
		if !ok {
			return nil
		}
		dec, ok := decodeGlobalID(id)
		if !ok {
			return nil
		}
		switch dec.kind {
		case "Repository":
			id0, ok := gqlIDPart(dec, 0)
			if !ok {
				return nil
			}
			r := ctx.store.findRepoByID(id0)
			if r == nil {
				return nil
			}
			return gqlRepoValue(r)
		case "Issue":
			id0, ok0 := gqlIDPart(dec, 0)
			num, ok1 := gqlIDPart(dec, 1)
			if !ok0 || !ok1 {
				return nil
			}
			r := ctx.store.findRepoByID(id0)
			if r == nil {
				return nil
			}
			r.mu.RLock()
			defer r.mu.RUnlock()
			for _, iss := range r.issues {
				if iss.number == num {
					return gqlIssueValue(r, iss)
				}
			}
			return nil
		case "PullRequest":
			id0, ok0 := gqlIDPart(dec, 0)
			num, ok1 := gqlIDPart(dec, 1)
			if !ok0 || !ok1 {
				return nil
			}
			r := ctx.store.findRepoByID(id0)
			if r == nil {
				return nil
			}
			r.mu.RLock()
			defer r.mu.RUnlock()
			for _, p := range r.pulls {
				if p.number == num {
					return gqlPullValue(r, p)
				}
			}
			return nil
		}
		return nil
	case "__type":
		name, ok := args["name"].(string)
		if !ok {
			return nil
		}
		fields, hasFields := gqlTypeFields[name]
		enums, hasEnums := gqlTypeEnums[name]
		if !hasFields && !hasEnums {
			return nil
		}
		fieldArr := make([]any, 0, len(fields))
		for _, f := range fields {
			fieldArr = append(fieldArr, &gqlObj{typename: "__TypeField", props: om("name", f)})
		}
		enumArr := make([]any, 0, len(enums))
		for _, e := range enums {
			enumArr = append(enumArr, &gqlObj{typename: "__TypeEnumValue", props: om("name", e)})
		}
		kind := "OBJECT"
		if hasEnums {
			kind = "ENUM"
		}
		return &gqlObj{typename: "__Type", props: om(
			"name", name,
			"fields", fieldArr,
			"enumValues", enumArr,
			"kind", kind,
		)}
	}
	return nil
}

func gqlResolveTypeField(parent *gqlObj, field string) any {
	if field == "fields" || field == "enumValues" {
		v, _ := parent.props.get(field)
		return v
	}
	return parent.get(field)
}

func gqlResolveConnection(parent *gqlObj, field string) any {
	switch field {
	case "nodes":
		v, ok := parent.props.get("nodes")
		if !ok {
			return []any{}
		}
		return v
	case "pageInfo":
		v, _ := parent.props.get("pageInfo")
		pi, _ := v.(*omap)
		if pi == nil {
			return &gqlObj{typename: "PageInfo", props: newOmap()}
		}
		out := &gqlObj{typename: "PageInfo", props: newOmap()}
		for _, k := range pi.keys {
			val, _ := pi.get(k)
			out.props.set(k, val)
		}
		return out
	case "totalCount":
		v, ok := parent.props.get("nodes")
		if !ok {
			return 0
		}
		if arr, ok := v.([]any); ok {
			return len(arr)
		}
		return 0
	case "edges":
		v, _ := parent.props.get("nodes")
		arr, _ := v.([]any)
		edges := make([]any, 0, len(arr))
		for _, node := range arr {
			edges = append(edges, &gqlObj{typename: "Edge", props: om("node", node, "cursor", "")})
		}
		return edges
	}
	return nil
}

func gqlOrderIssues(issues []*issue, args map[string]any) []*issue {
	list := append([]*issue(nil), issues...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToLower(fmt.Sprint(s))] = true
		}
		filtered := list[:0]
		for _, iss := range list {
			if wanted[iss.state] {
				filtered = append(filtered, iss)
			}
		}
		list = filtered
	}
	key := func(iss *issue) int { return iss.createdSeq }
	direction := 1
	if orderBy, ok := args["orderBy"].(map[string]any); ok && orderBy != nil {
		if field, _ := orderBy["field"].(string); field == "UPDATED_AT" {
			key = func(iss *issue) int { return iss.updatedSeq }
		} else if field == "COMMENTS" {
			key = func(iss *issue) int { return len(iss.comments) }
		} else {
			key = func(iss *issue) int { return iss.createdSeq }
		}
		if dir, _ := orderBy["direction"].(string); dir == "DESC" {
			direction = -1
		}
	}
	sort.Slice(list, func(i, j int) bool {
		d := (key(list[i]) - key(list[j])) * direction
		if d != 0 {
			return d < 0
		}
		return (list[i].number-list[j].number)*direction < 0
	})
	return list
}

func gqlResolveRepository(ctx *gqlExecCtx, r *repo, field string, args map[string]any) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch field {
	case "id":
		return globalID("Repository", r.id)
	case "databaseId":
		return r.id
	case "name":
		return r.name
	case "nameWithOwner":
		return r.owner + "/" + r.name
	case "owner":
		return gqlUserValue(r.owner)
	case "description":
		return r.description
	case "defaultBranchRef":
		return &gqlObj{typename: "Ref", props: om("name", r.defaultBranch)}
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "isPrivate":
		return r.private
	case "url":
		return ""
	case "createdAt":
		return r.createdAt
	case "updatedAt":
		return r.updatedAt
	case "issue":
		num, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		for _, iss := range r.issues {
			if iss.number == int(num) {
				return gqlIssueValue(r, iss)
			}
		}
		return nil
	case "issues":
		ordered := gqlOrderIssues(r.issues, args)
		first := len(ordered)
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		after, hasAfter := gqlDecodeCursor(args["after"])
		start := 0
		if hasAfter {
			idx := -1
			for i, iss := range ordered {
				if iss.number == after {
					idx = i
					break
				}
			}
			if idx >= 0 {
				start = idx + 1
			}
		}
		end := start + first
		if end < start {
			end = start
		}
		if end > len(ordered) {
			end = len(ordered)
		}
		if start > len(ordered) {
			start = len(ordered)
			end = len(ordered)
		}
		slice := ordered[start:end]
		nodes := make([]any, 0, len(slice))
		for _, iss := range slice {
			nodes = append(nodes, gqlIssueValue(r, iss))
		}
		var endCursor any
		if len(slice) > 0 {
			endCursor = gqlEncodeCursor(slice[len(slice)-1].number)
		}
		return &gqlObj{typename: "IssueConnection", props: om(
			"nodes", nodes,
			"pageInfo", om("hasNextPage", start+len(slice) < len(ordered), "endCursor", endCursor),
		)}
	case "pullRequests":
		first := len(r.pulls)
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		ordered := append([]*pull(nil), r.pulls...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].number > ordered[j].number })
		if first < 0 {
			first = 0
		}
		if first > len(ordered) {
			first = len(ordered)
		}
		slice := ordered[:first]
		nodes := make([]any, 0, len(slice))
		for _, p := range slice {
			nodes = append(nodes, gqlPullValue(r, p))
		}
		return &gqlObj{typename: "PullRequestConnection", props: om(
			"nodes", nodes,
			"pageInfo", om("hasNextPage", len(slice) < len(ordered), "endCursor", nil),
		)}
	case "pullRequest":
		num, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		for _, p := range r.pulls {
			if p.number == int(num) {
				return gqlPullValue(r, p)
			}
		}
		return nil
	}
	return nil
}

func gqlResolveIssue(r *repo, iss *issue, field string) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch field {
	case "id":
		return globalID("Issue", r.id, iss.number)
	case "number":
		return iss.number
	case "title":
		return iss.title
	case "state":
		if iss.state == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "body":
		return iss.body
	case "url":
		return ""
	case "createdAt":
		return iss.createdAt
	case "updatedAt":
		return iss.updatedAt
	case "author":
		return gqlUserValue(viewerLogin)
	case "closed":
		return iss.state == "closed"
	case "closedAt":
		if iss.state == "closed" {
			return iss.updatedAt
		}
		return nil
	case "labels":
		nodes := make([]any, 0, len(iss.labels))
		for _, name := range iss.labels {
			nodes = append(nodes, &gqlObj{typename: "Label", props: om("name", name)})
		}
		return &gqlObj{typename: "IssueConnection", props: om(
			"nodes", nodes,
			"pageInfo", om("hasNextPage", false, "endCursor", nil),
		)}
	case "comments":
		nodes := make([]any, 0, len(iss.comments))
		for _, c := range iss.comments {
			nodes = append(nodes, &gqlObj{typename: "IssueComment", props: om(
				"id", globalID("IssueComment", r.id, iss.number, c.id),
				"body", c.body,
				"author", gqlUserValue(viewerLogin),
				"createdAt", c.createdAt,
				"updatedAt", c.updatedAt,
			)})
		}
		return &gqlObj{typename: "IssueConnection", props: om(
			"nodes", nodes,
			"pageInfo", om("hasNextPage", false, "endCursor", nil),
		)}
	}
	return nil
}

func gqlResolvePull(r *repo, p *pull, field string, args map[string]any) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch field {
	case "id":
		return globalID("PullRequest", r.id, p.number)
	case "number":
		return p.number
	case "title":
		return p.title
	case "body":
		return p.body
	case "state":
		if p.merged {
			return "MERGED"
		}
		if p.state == "open" {
			return "OPEN"
		}
		return "CLOSED"
	case "merged":
		return p.merged
	case "mergeable":
		if mergeableOf(r, p) {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	case "headRefName":
		return p.headRef
	case "baseRefName":
		return p.baseRef
	case "url":
		return ""
	case "createdAt":
		return p.createdAt
	case "updatedAt":
		return p.updatedAt
	case "author":
		return gqlUserValue(viewerLogin)
	case "files":
		first := 100
		if f, ok := args["first"].(float64); ok {
			first = int(f)
		}
		headTip, ok := branchTip(r, p.headRef)
		if !ok {
			headTip = p.headSha
		}
		var baseTree, headTree *string
		if t, ok := commitTree(r, p.baseSha); ok {
			baseTree = &t
		}
		if t, ok := commitTree(r, headTip); ok {
			headTree = &t
		}
		changes := diffTrees(r, baseTree, headTree)
		if first < 0 {
			first = 0
		}
		if first < len(changes) {
			changes = changes[:first]
		}
		nodes := make([]any, 0, len(changes))
		for _, ch := range changes {
			changeType := "MODIFIED"
			if ch.Status == "added" {
				changeType = "ADDED"
			} else if ch.Status == "removed" {
				changeType = "REMOVED"
			}
			nodes = append(nodes, &gqlObj{typename: "PullRequestChangedFile", props: om(
				"path", ch.Filename,
				"additions", ch.Additions,
				"deletions", ch.Deletions,
				"changeType", changeType,
			)})
		}
		return &gqlObj{typename: "PullRequestConnection", props: om(
			"nodes", nodes,
			"pageInfo", om("hasNextPage", false, "endCursor", nil),
		)}
	}
	return nil
}

func gqlResolveMutation(ctx *gqlExecCtx, field string, args map[string]any) (any, error) {
	input, ok := args["input"].(map[string]any)
	if !ok {
		return nil, &gqlError{msg: "mutation " + field + " needs an input object"}
	}
	switch field {
	case "createIssue":
		repoID, ok := input["repositoryId"].(string)
		if !ok {
			return nil, &gqlError{msg: "createIssue needs repositoryId"}
		}
		dec, ok := decodeGlobalID(repoID)
		if !ok || dec.kind != "Repository" {
			return nil, &gqlError{msg: "createIssue: unknown repositoryId"}
		}
		repoNum, ok := gqlIDPart(dec, 0)
		if !ok {
			return nil, &gqlError{msg: "createIssue: unknown repositoryId"}
		}
		r := ctx.store.findRepoByID(repoNum)
		if r == nil {
			return nil, &gqlError{msg: "createIssue: unknown repository"}
		}
		title, ok := input["title"].(string)
		if !ok || title == "" {
			return nil, &gqlError{msg: "createIssue needs a title"}
		}
		body := ""
		if b, ok := input["body"].(string); ok {
			body = b
		}
		iss := ctx.store.createIssue(r, title, body, nil)
		return &gqlObj{typename: "CreateIssuePayload", props: om("issue", gqlIssueValue(r, iss))}, nil
	case "closeIssue":
		issueID, ok := input["issueId"].(string)
		if !ok {
			return nil, &gqlError{msg: "closeIssue needs issueId"}
		}
		dec, ok := decodeGlobalID(issueID)
		if !ok || dec.kind != "Issue" {
			return nil, &gqlError{msg: "closeIssue: unknown issueId"}
		}
		repoNum, ok0 := gqlIDPart(dec, 0)
		issNum, ok1 := gqlIDPart(dec, 1)
		if !ok0 || !ok1 {
			return nil, &gqlError{msg: "closeIssue: unknown issueId"}
		}
		r := ctx.store.findRepoByID(repoNum)
		if r == nil {
			return nil, &gqlError{msg: "closeIssue: unknown issue"}
		}
		r.mu.Lock()
		var iss *issue
		for _, entry := range r.issues {
			if entry.number == issNum {
				iss = entry
				break
			}
		}
		if iss == nil {
			r.mu.Unlock()
			return nil, &gqlError{msg: "closeIssue: unknown issue"}
		}
		iss.state = "closed"
		iss.updatedSeq = ctx.store.nextSeq()
		r.mu.Unlock()
		return &gqlObj{typename: "CloseIssuePayload", props: om("issue", gqlIssueValue(r, iss))}, nil
	case "createPullRequest":
		repoID, ok := input["repositoryId"].(string)
		if !ok {
			return nil, &gqlError{msg: "createPullRequest needs repositoryId"}
		}
		dec, ok := decodeGlobalID(repoID)
		if !ok || dec.kind != "Repository" {
			return nil, &gqlError{msg: "createPullRequest: unknown repositoryId"}
		}
		repoNum, ok := gqlIDPart(dec, 0)
		if !ok {
			return nil, &gqlError{msg: "createPullRequest: unknown repositoryId"}
		}
		r := ctx.store.findRepoByID(repoNum)
		if r == nil {
			return nil, &gqlError{msg: "createPullRequest: unknown repository"}
		}
		head, ok1 := input["headRefName"].(string)
		base, ok2 := input["baseRefName"].(string)
		title, ok3 := input["title"].(string)
		if !ok1 || !ok2 || !ok3 {
			return nil, &gqlError{msg: "createPullRequest needs headRefName, baseRefName and title"}
		}
		r.mu.RLock()
		headTip, headOK := branchTip(r, head)
		baseTip, baseOK := branchTip(r, base)
		var duplicate *pull
		for _, p := range r.pulls {
			if p.state == "open" && p.headRef == head && p.baseRef == base {
				duplicate = p
				break
			}
		}
		r.mu.RUnlock()
		if !headOK {
			return nil, &gqlError{msg: "head branch " + head + " does not exist"}
		}
		if !baseOK {
			return nil, &gqlError{msg: "base branch " + base + " does not exist"}
		}
		if duplicate != nil {
			return nil, &gqlError{msg: "A pull request already exists for these branches"}
		}
		body := ""
		if b, ok := input["body"].(string); ok {
			body = b
		}
		p := ctx.store.createPull(r, title, body, head, base, headTip, baseTip)
		return &gqlObj{typename: "CreatePullRequestPayload", props: om("pullRequest", gqlPullValue(r, p))}, nil
	case "mergePullRequest":
		pullID, ok := input["pullRequestId"].(string)
		if !ok {
			return nil, &gqlError{msg: "mergePullRequest needs pullRequestId"}
		}
		dec, ok := decodeGlobalID(pullID)
		if !ok || dec.kind != "PullRequest" {
			return nil, &gqlError{msg: "mergePullRequest: unknown pullRequestId"}
		}
		repoNum, ok0 := gqlIDPart(dec, 0)
		pullNum, ok1 := gqlIDPart(dec, 1)
		if !ok0 || !ok1 {
			return nil, &gqlError{msg: "mergePullRequest: unknown pullRequestId"}
		}
		r := ctx.store.findRepoByID(repoNum)
		if r == nil {
			return nil, &gqlError{msg: "mergePullRequest: unknown pull request"}
		}
		r.mu.Lock()
		var p *pull
		for _, entry := range r.pulls {
			if entry.number == pullNum {
				p = entry
				break
			}
		}
		if p == nil {
			r.mu.Unlock()
			return nil, &gqlError{msg: "mergePullRequest: unknown pull request"}
		}
		_, reason, mergedOK := mergePull(ctx.store, r, p)
		r.mu.Unlock()
		if !mergedOK {
			return nil, &gqlError{msg: reason}
		}
		return &gqlObj{typename: "MergePullRequestPayload", props: om("pullRequest", gqlPullValue(r, p))}, nil
	case "addComment":
		subjectID, ok := input["subjectId"].(string)
		if !ok {
			return nil, &gqlError{msg: "addComment needs subjectId"}
		}
		dec, ok := decodeGlobalID(subjectID)
		if !ok || dec.kind != "Issue" {
			return nil, &gqlError{msg: "addComment: unknown subjectId"}
		}
		repoNum, ok0 := gqlIDPart(dec, 0)
		issNum, ok1 := gqlIDPart(dec, 1)
		if !ok0 || !ok1 {
			return nil, &gqlError{msg: "addComment: unknown subjectId"}
		}
		r := ctx.store.findRepoByID(repoNum)
		if r == nil {
			return nil, &gqlError{msg: "addComment: unknown issue"}
		}
		r.mu.RLock()
		var iss *issue
		for _, entry := range r.issues {
			if entry.number == issNum {
				iss = entry
				break
			}
		}
		r.mu.RUnlock()
		if iss == nil {
			return nil, &gqlError{msg: "addComment: unknown issue"}
		}
		body, ok := input["body"].(string)
		if !ok {
			return nil, &gqlError{msg: "addComment needs a body"}
		}
		c := ctx.store.addComment(r, iss, body)
		return &gqlObj{typename: "AddCommentPayload", props: om(
			"commentEdge", &gqlObj{typename: "IssueCommentEdge", props: om(
				"node", &gqlObj{typename: "IssueComment", props: om(
					"id", globalID("IssueComment", r.id, iss.number, c.id),
					"body", c.body,
				)},
			)},
		)}, nil
	}
	return nil, nil
}

// ------------------------------------------------------------------- handler

func gqlExecuteOne(ctx *gqlExecCtx, query string, variables map[string]any, operationName string, hasOperationName bool) (result *omap) {
	defer func() {
		if rec := recover(); rec != nil {
			if pe, ok := rec.(gqlParsePanic); ok {
				result = om("data", nil, "errors", []*omap{om("message", pe.msg)})
				return
			}
			panic(rec)
		}
	}()
	parser := &gqlParser{tokens: tokenize(query)}
	ops := parser.parseDocument()
	var op *gqlOperation
	if hasOperationName {
		for _, entry := range ops {
			if entry.hasName && entry.name == operationName {
				op = entry
				break
			}
		}
		if op == nil {
			return om("data", nil, "errors", []*omap{om("message", "unknown operation "+operationName)})
		}
	} else if len(ops) == 1 {
		op = ops[0]
	} else {
		return om("data", nil, "errors", []*omap{om("message", "multiple operations need an operationName")})
	}
	vars := map[string]any{}
	for k, v := range variables {
		vars[k] = v
	}
	for name, node := range op.variables {
		if _, ok := vars[name]; !ok && node.kind != "null" {
			vars[name] = evalValue(node, vars)
		}
	}
	var errors []*omap
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlObj{typename: rootType, props: newOmap()}
	data := gqlExecuteSelections(ctx, rootType, root, op.selections, vars, &errors)
	result = newOmap()
	result.set("data", data)
	if len(errors) > 0 {
		result.set("errors", errors)
	}
	return result
}

func gqlVariablesOf(op map[string]any) map[string]any {
	if v, ok := op["variables"].(map[string]any); ok && v != nil {
		return v
	}
	return map[string]any{}
}

func gqlOperationNameOf(op map[string]any) (string, bool) {
	if s, ok := op["operationName"].(string); ok {
		return s, true
	}
	return "", false
}

func handleGraphql(s *store, method, path string, headers http.Header, body []byte) forgeResponse {
	if method != "POST" {
		return jsonError(http.StatusNotFound, "Not Found")
	}
	if !checkAuth(headers.Get("Authorization")) {
		return jsonError(http.StatusUnauthorized, "Requires authentication")
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return jsonError(http.StatusBadRequest, "Invalid JSON body")
	}
	ctx := &gqlExecCtx{store: s}
	if arr, ok := parsed.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			opMap, ok := entry.(map[string]any)
			if !ok {
				results = append(results, om("data", nil, "errors", []*omap{om("message", "invalid operation")}))
				continue
			}
			query, ok := opMap["query"].(string)
			if !ok {
				results = append(results, om("data", nil, "errors", []*omap{om("message", "query is required")}))
				continue
			}
			opName, hasOpName := gqlOperationNameOf(opMap)
			results = append(results, gqlExecuteOne(ctx, query, gqlVariablesOf(opMap), opName, hasOpName))
		}
		return jsonResponse(http.StatusOK, results)
	}
	opMap, ok := parsed.(map[string]any)
	if !ok {
		return jsonError(http.StatusBadRequest, "Invalid GraphQL body")
	}
	query, ok := opMap["query"].(string)
	if !ok {
		return jsonError(http.StatusBadRequest, "query is required")
	}
	opName, hasOpName := gqlOperationNameOf(opMap)
	return jsonResponse(http.StatusOK, gqlExecuteOne(ctx, query, gqlVariablesOf(opMap), opName, hasOpName))
}
