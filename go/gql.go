// GraphQL v4 at POST /api/graphql, covering the documents gh and
// github-mcp-server actually send — viewer, repository reads, issue
// and pull-request connections with cursor pagination, the create,
// close and merge mutations, Relay node(id:), batching, and the
// __type introspection probes. A small hand-written lexer and parser
// plus a resolver executor over the shared Depot; no schema library,
// no dependencies.
//
// Global ids are opaque base64 ("R:<repoId>", "I:<repoId>:<number>",
// ...) — stable across reads and never the plain database id, which
// is what the suite's global-id check requires.

package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Global ids
// ---------------------------------------------------------------------------

// encodeNodeID builds an opaque global id from its parts.
func encodeNodeID(parts ...any) string {
	var segments []string
	for _, part := range parts {
		switch value := part.(type) {
		case string:
			segments = append(segments, value)
		case int:
			segments = append(segments, strconv.Itoa(value))
		}
	}
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(segments, ":")))
}

// decodeNodeID takes a global id apart again, or returns nil when it
// is not one of ours.
func decodeNodeID(id string) []string {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil
	}
	return parts
}

// ---------------------------------------------------------------------------
// Lexer and parser
// ---------------------------------------------------------------------------

// gqlTokKind classifies lexer tokens.
type gqlTokKind int

const (
	tokName gqlTokKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
	tokSpread
	tokEOF
)

// gqlToken is one lexed token.
type gqlToken struct {
	kind   gqlTokKind
	text   string
	number float64
}

// tokenizeGraphQL lexes a GraphQL document. Whitespace, commas and
// comments are insignificant; block strings and the usual escapes are
// supported; an unknown character is skipped (the parser will fail on
// the resulting shape if it mattered).
func tokenizeGraphQL(input string) []gqlToken {
	var tokens []gqlToken
	pos := 0
	for pos < len(input) {
		c := input[pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == 0xEF:
			pos++
		case c == '#':
			for pos < len(input) && input[pos] != '\n' {
				pos++
			}
		case c == '.' && pos+2 < len(input) && input[pos+1] == '.' && input[pos+2] == '.':
			tokens = append(tokens, gqlToken{kind: tokSpread})
			pos += 3
		case strings.IndexByte("!$():=@[]{}|", c) >= 0:
			tokens = append(tokens, gqlToken{kind: tokPunct, text: string(c)})
			pos++
		case c == '"':
			if pos+2 < len(input) && input[pos+1] == '"' && input[pos+2] == '"' {
				end := strings.Index(input[pos+3:], `"""`)
				if end < 0 {
					tokens = append(tokens, gqlToken{kind: tokString, text: input[pos+3:]})
					pos = len(input)
				} else {
					tokens = append(tokens, gqlToken{kind: tokString, text: input[pos+3 : pos+3+end]})
					pos = pos + 3 + end + 3
				}
			} else {
				scan := pos + 1
				var sb strings.Builder
				for scan < len(input) && input[scan] != '"' {
					if input[scan] == '\\' && scan+1 < len(input) {
						escaped := input[scan+1]
						switch escaped {
						case 'n':
							sb.WriteByte('\n')
						case 't':
							sb.WriteByte('\t')
						case 'r':
							sb.WriteByte('\r')
						default:
							sb.WriteByte(escaped)
						}
						scan += 2
					} else {
						sb.WriteByte(input[scan])
						scan++
					}
				}
				tokens = append(tokens, gqlToken{kind: tokString, text: sb.String()})
				pos = scan + 1
			}
		case (c >= '0' && c <= '9') || c == '-':
			scan := pos
			var sb strings.Builder
			for scan < len(input) {
				ch := input[scan]
				if (ch >= '0' && ch <= '9') || ch == '.' || ch == 'e' || ch == 'E' || ch == '+' || ch == '-' {
					sb.WriteByte(ch)
					scan++
				} else {
					break
				}
			}
			text := sb.String()
			number, _ := strconv.ParseFloat(text, 64)
			if strings.ContainsAny(text, ".eE") {
				tokens = append(tokens, gqlToken{kind: tokFloat, number: number})
			} else {
				tokens = append(tokens, gqlToken{kind: tokInt, number: number})
			}
			pos = scan
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_':
			scan := pos
			var sb strings.Builder
			for scan < len(input) {
				ch := input[scan]
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
					sb.WriteByte(ch)
					scan++
				} else {
					break
				}
			}
			tokens = append(tokens, gqlToken{kind: tokName, text: sb.String()})
			pos = scan
		default:
			pos++
		}
	}
	tokens = append(tokens, gqlToken{kind: tokEOF})
	return tokens
}

// gqlLiteral is one argument value in a parsed document.
type gqlLiteral struct {
	kind   string // var, int, float, string, bool, null, enum, list, object
	number float64
	text   string
	flag   bool
	items  []*gqlLiteral
	fields map[string]*gqlLiteral
}

// gqlFieldNode is one field selection: name, optional alias,
// arguments and an optional sub-selection.
type gqlFieldNode struct {
	alias       string
	name        string
	args        map[string]*gqlLiteral
	selection   []gqlSelection
	hasSelection bool
}

// gqlSelection is one entry of a selection set: a field, an inline
// fragment, or a named fragment spread.
type gqlSelection struct {
	kind   string // field, inline, spread
	field  *gqlFieldNode
	onType string
	sel    []gqlSelection
	name   string
}

// gqlOperation is one query or mutation of a document.
type gqlOperation struct {
	kind      string // query | mutation
	name      string
	selection []gqlSelection
}

// gqlFragmentDef is one named fragment definition.
type gqlFragmentDef struct {
	onType    string
	selection []gqlSelection
}

// gqlDocument is a whole parsed document.
type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string]gqlFragmentDef
}

// gqlParseState is the parser's cursor over the token stream.
type gqlParseState struct {
	tokens []gqlToken
	at     int
}

func (p *gqlParseState) peekToken() gqlToken { return p.tokens[p.at] }

func (p *gqlParseState) takeToken() gqlToken {
	tok := p.tokens[p.at]
	p.at++
	return tok
}

func (p *gqlParseState) expectPunct(want string) bool {
	tok := p.takeToken()
	return tok.kind == tokPunct && tok.text == want
}

func (p *gqlParseState) eatPunct(want string) bool {
	tok := p.peekToken()
	if tok.kind == tokPunct && tok.text == want {
		p.at++
		return true
	}
	return false
}

func (p *gqlParseState) expectName() (string, bool) {
	tok := p.takeToken()
	if tok.kind != tokName {
		return "", false
	}
	return tok.text, true
}

// readDocument parses a whole document: operations and fragment
// definitions.
func (p *gqlParseState) readDocument() (*gqlDocument, bool) {
	doc := &gqlDocument{fragments: map[string]gqlFragmentDef{}}
	for p.peekToken().kind != tokEOF {
		head := p.peekToken()
		if head.kind == tokPunct && head.text == "{" {
			selection, ok := p.readSelectionSet()
			if !ok {
				return nil, false
			}
			doc.operations = append(doc.operations, gqlOperation{kind: "query", selection: selection})
			continue
		}
		word, ok := p.expectName()
		if !ok {
			return nil, false
		}
		switch word {
		case "query", "mutation":
			var opName string
			if p.peekToken().kind == tokName {
				opName, _ = p.expectName()
			}
			if p.peekToken().kind == tokPunct && p.peekToken().text == "(" {
				if !p.readVarDefs() {
					return nil, false
				}
			}
			p.skipDirectiveList()
			selection, ok := p.readSelectionSet()
			if !ok {
				return nil, false
			}
			doc.operations = append(doc.operations, gqlOperation{kind: word, name: opName, selection: selection})
		case "fragment":
			fragName, ok := p.expectName()
			if !ok {
				return nil, false
			}
			onKeyword, ok := p.expectName()
			if !ok || onKeyword != "on" {
				return nil, false
			}
			onType, ok := p.expectName()
			if !ok {
				return nil, false
			}
			p.skipDirectiveList()
			selection, ok := p.readSelectionSet()
			if !ok {
				return nil, false
			}
			doc.fragments[fragName] = gqlFragmentDef{onType: onType, selection: selection}
		default:
			return nil, false
		}
	}
	return doc, true
}

// readVarDefs parses a variable definition list, "($x: Int = 3, ...)".
func (p *gqlParseState) readVarDefs() bool {
	if !p.expectPunct("(") {
		return false
	}
	for !p.eatPunct(")") {
		if !p.expectPunct("$") {
			return false
		}
		if _, ok := p.expectName(); !ok {
			return false
		}
		if !p.expectPunct(":") {
			return false
		}
		if !p.readTypeRef() {
			return false
		}
		if p.eatPunct("=") {
			if _, ok := p.readLiteral(); !ok {
				return false
			}
		}
		p.skipDirectiveList()
		if p.peekToken().kind == tokEOF {
			return false
		}
	}
	return true
}

// readTypeRef parses a type reference: a name or a list, with an
// optional trailing bang.
func (p *gqlParseState) readTypeRef() bool {
	if p.eatPunct("[") {
		if !p.readTypeRef() {
			return false
		}
		if !p.expectPunct("]") {
			return false
		}
	} else {
		if _, ok := p.expectName(); !ok {
			return false
		}
	}
	p.eatPunct("!")
	return true
}

// skipDirectiveList consumes any directives at the cursor.
func (p *gqlParseState) skipDirectiveList() {
	for p.peekToken().kind == tokPunct && p.peekToken().text == "@" {
		p.takeToken()
		if _, ok := p.expectName(); !ok {
			return
		}
		if p.peekToken().kind == tokPunct && p.peekToken().text == "(" {
			p.readArgs()
		}
	}
}

// readArgs parses an argument list, "(name: value, ...)".
func (p *gqlParseState) readArgs() (map[string]*gqlLiteral, bool) {
	args := map[string]*gqlLiteral{}
	if !p.expectPunct("(") {
		return nil, false
	}
	for !p.eatPunct(")") {
		name, ok := p.expectName()
		if !ok {
			return nil, false
		}
		if !p.expectPunct(":") {
			return nil, false
		}
		value, ok := p.readLiteral()
		if !ok {
			return nil, false
		}
		args[name] = value
	}
	return args, true
}

// readLiteral parses one argument value.
func (p *gqlParseState) readLiteral() (*gqlLiteral, bool) {
	tok := p.takeToken()
	switch tok.kind {
	case tokPunct:
		switch tok.text {
		case "$":
			name, ok := p.expectName()
			if !ok {
				return nil, false
			}
			return &gqlLiteral{kind: "var", text: name}, true
		case "[":
			list := &gqlLiteral{kind: "list"}
			for !p.eatPunct("]") {
				item, ok := p.readLiteral()
				if !ok {
					return nil, false
				}
				list.items = append(list.items, item)
			}
			return list, true
		case "{":
			object := &gqlLiteral{kind: "object", fields: map[string]*gqlLiteral{}}
			for !p.eatPunct("}") {
				name, ok := p.expectName()
				if !ok {
					return nil, false
				}
				if !p.expectPunct(":") {
					return nil, false
				}
				value, ok := p.readLiteral()
				if !ok {
					return nil, false
				}
				object.fields[name] = value
			}
			return object, true
		}
	case tokInt:
		return &gqlLiteral{kind: "int", number: tok.number}, true
	case tokFloat:
		return &gqlLiteral{kind: "float", number: tok.number}, true
	case tokString:
		return &gqlLiteral{kind: "string", text: tok.text}, true
	case tokName:
		switch tok.text {
		case "true":
			return &gqlLiteral{kind: "bool", flag: true}, true
		case "false":
			return &gqlLiteral{kind: "bool", flag: false}, true
		case "null":
			return &gqlLiteral{kind: "null"}, true
		}
		return &gqlLiteral{kind: "enum", text: tok.text}, true
	}
	return nil, false
}

// readSelectionSet parses "{ ... }".
func (p *gqlParseState) readSelectionSet() ([]gqlSelection, bool) {
	if !p.expectPunct("{") {
		return nil, false
	}
	var entries []gqlSelection
	for !p.eatPunct("}") {
		if p.peekToken().kind == tokSpread {
			p.takeToken()
			if p.peekToken().kind == tokName && p.peekToken().text == "on" {
				p.takeToken()
				onType, ok := p.expectName()
				if !ok {
					return nil, false
				}
				p.skipDirectiveList()
				inner, ok := p.readSelectionSet()
				if !ok {
					return nil, false
				}
				entries = append(entries, gqlSelection{kind: "inline", onType: onType, sel: inner})
			} else {
				name, ok := p.expectName()
				if !ok {
					return nil, false
				}
				entries = append(entries, gqlSelection{kind: "spread", name: name})
			}
			continue
		}
		fieldName, ok := p.expectName()
		if !ok {
			return nil, false
		}
		var alias string
		if p.eatPunct(":") {
			alias = fieldName
			fieldName, ok = p.expectName()
			if !ok {
				return nil, false
			}
		}
		var args map[string]*gqlLiteral
		if p.peekToken().kind == tokPunct && p.peekToken().text == "(" {
			args, ok = p.readArgs()
			if !ok {
				return nil, false
			}
		} else {
			args = map[string]*gqlLiteral{}
		}
		p.skipDirectiveList()
		field := &gqlFieldNode{alias: alias, name: fieldName, args: args}
		if p.peekToken().kind == tokPunct && p.peekToken().text == "{" {
			inner, ok := p.readSelectionSet()
			if !ok {
				return nil, false
			}
			field.selection = inner
			field.hasSelection = true
		}
		entries = append(entries, gqlSelection{kind: "field", field: field})
	}
	return entries, true
}

// evaluateLiteral evaluates an argument value against the operation's
// variables.
func evaluateLiteral(value *gqlLiteral, vars map[string]any) any {
	switch value.kind {
	case "var":
		return vars[value.text]
	case "int", "float":
		return value.number
	case "string":
		return value.text
	case "bool":
		return value.flag
	case "null":
		return nil
	case "enum":
		return value.text
	case "list":
		evaluated := make([]any, 0, len(value.items))
		for _, item := range value.items {
			evaluated = append(evaluated, evaluateLiteral(item, vars))
		}
		return evaluated
	case "object":
		evaluated := map[string]any{}
		for key, fieldValue := range value.fields {
			evaluated[key] = evaluateLiteral(fieldValue, vars)
		}
		return evaluated
	}
	return nil
}

// evaluateArgs evaluates all of a field's arguments.
func evaluateArgs(field *gqlFieldNode, vars map[string]any) map[string]any {
	evaluated := map[string]any{}
	for name, value := range field.args {
		evaluated[name] = evaluateLiteral(value, vars)
	}
	return evaluated
}

// ---------------------------------------------------------------------------
// Schema: the type field lists the introspection probes read
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Query":        {"viewer", "repository", "node", "nodes", "rateLimit", "search"},
	"Mutation":     {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment", "reopenIssue"},
	"User":         {"login", "id", "name", "email", "url"},
	"Organization": {"login", "id", "name"},
	"Ref":          {"name", "target", "repository"},
	"Commit":       {"oid", "message", "author"},
	"Repository": {
		"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate", "viewerPermission",
		"hasIssuesEnabled", "defaultBranchRef", "issue", "issues", "pullRequest", "pullRequests",
		"createdAt", "updatedAt", "url", "labels", "refs", "ref", "stargazerCount", "forkCount",
	},
	"Issue": {
		"id", "number", "title", "state", "body", "author", "comments", "labels", "repository",
		"createdAt", "updatedAt", "url", "closed", "closedAt",
	},
	"PullRequest": {
		"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName",
		"files", "author", "commits", "comments", "mergeCommit", "additions", "deletions", "changedFiles",
		"createdAt", "updatedAt", "url", "closed", "mergedAt",
	},
	"IssueComment": {"id", "body", "author", "createdAt", "updatedAt", "url"},
	"PageInfo":     {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":       {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":           {"path", "additions", "deletions", "changeType"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":       {"node", "cursor"},
	"PullRequestEdge": {"node", "cursor"},
	"Label":           {"id", "name", "color", "description"},
	"LabelConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"CommentConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"CreateIssuePayload":       {"issue", "clientMutationId"},
	"CloseIssuePayload":        {"issue", "clientMutationId"},
	"CreatePullRequestPayload": {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":  {"pullRequest", "clientMutationId"},
	"AddCommentPayload":        {"commentEdge", "subject", "clientMutationId"},
	"CommentEdge":              {"node", "cursor"},
	"__Type":       {"name", "kind", "description", "fields", "enumValues", "possibleTypes", "inputFields", "ofType"},
	"__Field":      {"name", "description", "type", "args", "isDeprecated", "deprecationReason"},
	"__EnumValue":  {"name", "description", "isDeprecated", "deprecationReason"},
	"__InputValue": {"name", "description", "type", "defaultValue"},
}

var gqlEnumValues = map[string][]string{
	"IssueState":                       {"OPEN", "CLOSED"},
	"PullRequestState":                 {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":                   {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod":           {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":             {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"SearchType":                       {"ISSUE", "REPOSITORY", "USER"},
	"IssueOrderField":                  {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":                   {"ASC", "DESC"},
	"PullRequestChangedFileChangeType": {"ADDED", "MODIFIED", "DELETED", "RENAMED"},
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// gqlExecCtx carries one operation's execution state.
type gqlExecCtx struct {
	engine    *Engine
	variables map[string]any
	fragments map[string]gqlFragmentDef
	errors    []map[string]any
}

// gqlObject is a node under resolution: a map carrying __typename
// plus typed payload entries.
type gqlObject map[string]any

func makeUserNode(engine *Engine) gqlObject {
	return gqlObject{"__typename": "User", "login": engine.depot.AdoptedLogin()}
}

func makeRepoNode(repo *Repository) gqlObject {
	return gqlObject{"__typename": "Repository", "repo": repo}
}

func makeIssueNode(repo *Repository, issue *IssueRecord) gqlObject {
	return gqlObject{"__typename": "Issue", "repo": repo, "issue": issue}
}

func makePullNode(repo *Repository, pull *PullRecord) gqlObject {
	return gqlObject{"__typename": "PullRequest", "repo": repo, "pull": pull}
}

// encodeCursor renders a connection cursor for an item number.
func encodeCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

// decodeCursorNumber reads the item number back out of a cursor.
func decodeCursorNumber(cursor string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	number, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return number, true
}

// runSelection executes a selection set against one source object.
func (c *gqlExecCtx) runSelection(typeName string, source gqlObject, selection []gqlSelection) map[string]any {
	out := map[string]any{}
	for _, entry := range selection {
		switch entry.kind {
		case "inline":
			if source != nil && source["__typename"] == entry.onType {
				for key, value := range c.runSelection(entry.onType, source, entry.sel) {
					out[key] = value
				}
			}
		case "spread":
			if fragment, ok := c.fragments[entry.name]; ok && source != nil && source["__typename"] == fragment.onType {
				for key, value := range c.runSelection(fragment.onType, source, fragment.selection) {
					out[key] = value
				}
			}
		case "field":
			field := entry.field
			key := field.name
			if field.alias != "" {
				key = field.alias
			}
			if field.name == "__typename" {
				out[key] = typeName
				continue
			}
			resolved := c.resolveAnyField(typeName, source, field)
			out[key] = c.completeFieldValue(resolved, field)
		}
	}
	return out
}

// completeFieldValue applies a field's selection set to the value the
// field resolved to.
func (c *gqlExecCtx) completeFieldValue(resolved any, field *gqlFieldNode) any {
	if resolved == nil {
		return nil
	}
	if !field.hasSelection {
		return resolved
	}
	if list, isList := resolved.([]any); isList {
		completed := make([]any, 0, len(list))
		for _, element := range list {
			completed = append(completed, c.completeFieldValue(element, field))
		}
		return completed
	}
	if node, isNode := resolved.(gqlObject); isNode {
		return c.runSelection(node["__typename"].(string), node, field.selection)
	}
	if plain, isMap := resolved.(map[string]any); isMap {
		if typeName, hasType := plain["__typename"].(string); hasType {
			return c.runSelection(typeName, plain, field.selection)
		}
	}
	return resolved
}

// resolveAnyField resolves one field on one type. Unknown fields on
// known types record the GraphQL "Cannot query field" error.
func (c *gqlExecCtx) resolveAnyField(typeName string, source gqlObject, field *gqlFieldNode) any {
	depot := c.engine.depot
	args := evaluateArgs(field, c.variables)
	if knownFields, isKnownType := gqlTypeFields[typeName]; isKnownType {
		found := false
		for _, known := range knownFields {
			if known == field.name {
				found = true
				break
			}
		}
		if !found && field.name != "__type" && field.name != "__schema" {
			c.errors = append(c.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"" + typeName + "\"."})
			return nil
		}
	}
	switch typeName {
	case "Query":
		return c.resolveRootQuery(field.name, args)
	case "Mutation":
		return c.resolveRootMutation(field.name, args)
	case "User":
		switch field.name {
		case "login":
			if login, ok := source["login"].(string); ok {
				return login
			}
			return depot.AdoptedLogin()
		case "id":
			return encodeNodeID("U", 1)
		case "name":
			return depot.AdoptedLogin()
		}
		return nil
	case "Repository":
		return c.resolveRepoField(source, field.name, args)
	case "Issue":
		issue := source["issue"].(*IssueRecord)
		repo := source["repo"].(*Repository)
		switch field.name {
		case "id":
			return encodeNodeID("I", repo.ID, issue.Number)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.BodyText
		case "state":
			return strings.ToUpper(issue.State)
		case "author":
			return makeUserNode(c.engine)
		case "repository":
			return makeRepoNode(repo)
		case "createdAt", "updatedAt":
			return depot.ISOForSeq(issue.SeqNo)
		case "closed":
			return issue.State == "closed"
		case "labels":
			nodes := []any{}
			for _, name := range issue.LabelNames {
				color := "ededed"
				if label, ok := repo.Labels[name]; ok {
					color = label.Color
				}
				nodes = append(nodes, gqlObject{"__typename": "Label", "name": name, "color": color})
			}
			return gqlObject{"__typename": "LabelConnection", "nodes": nodes}
		case "comments":
			nodes := []any{}
			for _, comment := range issue.Comments {
				nodes = append(nodes, c.makeCommentNode(repo, issue.Number, comment))
			}
			return gqlObject{"__typename": "CommentConnection", "nodes": nodes}
		}
		return nil
	case "PullRequest":
		pull := source["pull"].(*PullRecord)
		repo := source["repo"].(*Repository)
		switch field.name {
		case "id":
			return encodeNodeID("P", repo.ID, pull.Number)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.BodyText
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "mergeable":
			return c.pullMergeableState(repo, pull)
		case "author":
			return makeUserNode(c.engine)
		case "createdAt", "updatedAt":
			return depot.ISOForSeq(pull.SeqNo)
		case "closed":
			return pull.State == "closed"
		case "mergedAt":
			if pull.Merged {
				return depot.ISOForSeq(pull.MergedSeq)
			}
			return nil
		case "mergeCommit":
			if pull.MergeCommitSHA == "" {
				return nil
			}
			return gqlObject{"__typename": "Commit", "oid": pull.MergeCommitSHA}
		case "files":
			changed := depot.ChangedFiles(repo, pull)
			limit := len(changed)
			if n, ok := args["first"].(float64); ok {
				limit = int(n)
			}
			if limit > len(changed) {
				limit = len(changed)
			}
			nodes := []any{}
			for _, delta := range changed[:limit] {
				nodes = append(nodes, gqlObject{"__typename": "PullRequestChangedFile", "path": delta.Path, "additions": delta.Additions, "deletions": delta.Deletions})
			}
			return gqlObject{"__typename": "PullRequestChangedFileConnection", "nodes": nodes, "totalCount": len(changed)}
		case "additions", "deletions", "changedFiles":
			changed := depot.ChangedFiles(repo, pull)
			if field.name == "changedFiles" {
				return len(changed)
			}
			total := 0
			for _, delta := range changed {
				if field.name == "additions" {
					total += delta.Additions
				} else {
					total += delta.Deletions
				}
			}
			return total
		case "comments":
			nodes := []any{}
			for _, comment := range pull.Comments {
				nodes = append(nodes, c.makeCommentNode(repo, pull.Number, comment))
			}
			return gqlObject{"__typename": "CommentConnection", "nodes": nodes}
		}
		return nil
	case "Ref":
		switch field.name {
		case "name":
			return source["name"]
		case "target":
			return gqlObject{"__typename": "Commit", "oid": source["oid"]}
		}
		return nil
	case "Commit":
		if field.name == "oid" {
			return source["oid"]
		}
		return nil
	case "PageInfo":
		return source[field.name]
	case "__Type":
		definition := source["typeDef"].(map[string]any)
		typeName2 := definition["name"].(string)
		switch field.name {
		case "name":
			return typeName2
		case "kind":
			return definition["kind"]
		case "fields":
			var fields []any
			for _, fieldName := range gqlTypeFields[typeName2] {
				fields = append(fields, gqlObject{"__typename": "__Field", "name": fieldName})
			}
			return fields
		case "enumValues":
			var values []any
			for _, enumName := range gqlEnumValues[typeName2] {
				values = append(values, gqlObject{"__typename": "__EnumValue", "name": enumName})
			}
			return values
		}
		return nil
	case "__Field", "__EnumValue":
		if field.name == "name" {
			return source["name"]
		}
		return nil
	}
	// Connection-shaped sources: nodes, edges, pageInfo and totalCount
	// read straight off the source object.
	if source != nil {
		if value, ok := source[field.name]; ok {
			return value
		}
	}
	if _, isKnownType := gqlTypeFields[typeName]; !isKnownType {
		return nil
	}
	c.errors = append(c.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"" + typeName + "\"."})
	return nil
}

// makeCommentNode builds the node for one comment.
func (c *gqlExecCtx) makeCommentNode(repo *Repository, subjectNumber int, comment *CommentRecord) gqlObject {
	stamp := c.engine.depot.ISOForSeq(comment.SeqNo)
	return gqlObject{
		"__typename": "IssueComment",
		"id":         encodeNodeID("C", repo.ID, subjectNumber, comment.ID),
		"body":       comment.BodyText,
		"createdAt":  stamp,
		"updatedAt":  stamp,
	}
}

// pullMergeableState reports a pull request's mergeability in
// GraphQL's vocabulary.
func (c *gqlExecCtx) pullMergeableState(repo *Repository, pull *PullRecord) string {
	if pull.Merged || pull.State == "closed" {
		return "UNKNOWN"
	}
	summary, ok := c.engine.depot.SummarizePull(repo, pull)
	if !ok {
		return "UNKNOWN"
	}
	if summary.Conflicted {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// resolveRootQuery resolves the Query root's fields.
func (c *gqlExecCtx) resolveRootQuery(name string, args map[string]any) any {
	depot := c.engine.depot
	switch name {
	case "viewer":
		return makeUserNode(c.engine)
	case "repository":
		owner, _ := args["owner"].(string)
		repoName, _ := args["name"].(string)
		repo, ok := depot.RepoMap[repoName]
		if !ok || !depot.LoginCouldBe(owner) {
			return nil
		}
		depot.LearnLogin(owner)
		return makeRepoNode(repo)
	case "node":
		id, _ := args["id"].(string)
		return c.nodeForGlobalID(id)
	case "__type":
		wanted, _ := args["name"].(string)
		if _, ok := gqlTypeFields[wanted]; ok {
			return gqlObject{"__typename": "__Type", "typeDef": map[string]any{"name": wanted, "kind": "OBJECT"}}
		}
		if _, ok := gqlEnumValues[wanted]; ok {
			return gqlObject{"__typename": "__Type", "typeDef": map[string]any{"name": wanted, "kind": "ENUM"}}
		}
		return nil
	}
	return nil
}

// nodeForGlobalID resolves Relay's node(id:) from a global id.
func (c *gqlExecCtx) nodeForGlobalID(id string) gqlObject {
	depot := c.engine.depot
	parts := decodeNodeID(id)
	if parts == nil {
		return nil
	}
	kind, first, second := parts[0], parts[1], ""
	if len(parts) > 2 {
		second = parts[2]
	}
	switch kind {
	case "R":
		wantedID, _ := strconv.Atoi(first)
		for _, repo := range depot.RepoMap {
			if repo.ID == wantedID {
				return makeRepoNode(repo)
			}
		}
	case "I", "P":
		wantedID, _ := strconv.Atoi(first)
		for _, repo := range depot.RepoMap {
			if repo.ID != wantedID {
				continue
			}
			number, _ := strconv.Atoi(second)
			if kind == "I" {
				if issue, ok := repo.Issues[number]; ok {
					return makeIssueNode(repo, issue)
				}
			} else if pull, ok := repo.Pulls[number]; ok {
				return makePullNode(repo, pull)
			}
		}
	case "U":
		return makeUserNode(c.engine)
	}
	return nil
}

// resolveRepoField resolves the Repository type's fields.
func (c *gqlExecCtx) resolveRepoField(source gqlObject, name string, args map[string]any) any {
	depot := c.engine.depot
	repo := source["repo"].(*Repository)
	switch name {
	case "id":
		return encodeNodeID("R", repo.ID)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return depot.AdoptedLogin() + "/" + repo.Name
	case "owner":
		return makeUserNode(c.engine)
	case "description":
		return repo.Description
	case "isPrivate":
		return repo.Private
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "createdAt", "updatedAt":
		return depot.ISOForSeq(repo.SeqNo)
	case "defaultBranchRef":
		tip, ok := depot.BranchTipOID(repo, repo.DefaultBranch)
		if !ok {
			return nil
		}
		return gqlObject{"__typename": "Ref", "name": repo.DefaultBranch, "oid": tip}
	case "issue":
		numberFloat, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		if issue, ok := repo.Issues[int(numberFloat)]; ok {
			return makeIssueNode(repo, issue)
		}
		return nil
	case "issues":
		return c.buildIssuesConnection(repo, args)
	case "pullRequest":
		numberFloat, ok := args["number"].(float64)
		if !ok {
			return nil
		}
		if pull, ok := repo.Pulls[int(numberFloat)]; ok {
			return makePullNode(repo, pull)
		}
		return nil
	case "pullRequests":
		var all []*PullRecord
		for _, pull := range repo.Pulls {
			all = append(all, pull)
		}
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].Number > all[j-1].Number; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		limit := len(all)
		if n, ok := args["first"].(float64); ok {
			limit = int(n)
		}
		if limit > len(all) {
			limit = len(all)
		}
		nodes := []any{}
		for _, pull := range all[:limit] {
			nodes = append(nodes, makePullNode(repo, pull))
		}
		return gqlObject{
			"__typename": "PullRequestConnection", "nodes": nodes, "totalCount": len(all),
			"pageInfo": gqlObject{"__typename": "PageInfo", "hasNextPage": limit < len(all), "hasPreviousPage": false, "startCursor": nil, "endCursor": nil},
		}
	}
	return nil
}

// buildIssuesConnection builds the issues connection of a repository:
// state filtering, ordering and cursor pagination.
func (c *gqlExecCtx) buildIssuesConnection(repo *Repository, args map[string]any) gqlObject {
	var all []*IssueRecord
	for _, issue := range repo.Issues {
		all = append(all, issue)
	}
	if states, ok := args["states"].([]any); ok {
		var wanted []string
		for _, state := range states {
			if text, ok := state.(string); ok {
				wanted = append(wanted, strings.ToLower(text))
			}
		}
		var filtered []*IssueRecord
		for _, issue := range all {
			for _, want := range wanted {
				if issue.State == want {
					filtered = append(filtered, issue)
					break
				}
			}
		}
		all = filtered
	}
	direction := "DESC"
	var order map[string]any
	if orderArg, ok := args["orderBy"].(map[string]any); ok {
		order = orderArg
	}
	orderField := "CREATED_AT"
	if order != nil {
		if fieldName, ok := order["field"].(string); ok {
			orderField = fieldName
		}
		if dirText, ok := order["direction"].(string); ok && dirText == "ASC" {
			direction = "ASC"
		}
	} else {
		direction = "ASC"
	}
	sortKeyOf := func(issue *IssueRecord) int {
		if orderField == "COMMENTS" {
			return len(issue.Comments)*100000 + int(issue.SeqNo)
		}
		return int(issue.SeqNo)
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0; j-- {
			delta := sortKeyOf(all[j]) - sortKeyOf(all[j-1])
			var outOfOrder bool
			if delta != 0 {
				outOfOrder = (direction == "DESC" && delta > 0) || (direction == "ASC" && delta < 0)
			} else {
				outOfOrder = (direction == "DESC" && all[j].Number > all[j-1].Number) || (direction == "ASC" && all[j].Number < all[j-1].Number)
			}
			if !outOfOrder {
				break
			}
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	startAt := 0
	if after, ok := args["after"].(string); ok {
		if number, ok := decodeCursorNumber(after); ok {
			for idx, issue := range all {
				if issue.Number == number {
					startAt = idx + 1
					break
				}
			}
		}
	}
	limit := len(all)
	if n, ok := args["first"].(float64); ok {
		limit = int(n)
	}
	if startAt+limit > len(all) {
		limit = len(all) - startAt
	}
	if limit < 0 {
		limit = 0
	}
	page := all[startAt : startAt+limit]
	nodes := []any{}
	for _, issue := range page {
		nodes = append(nodes, makeIssueNode(repo, issue))
	}
	var startCursor, endCursor any
	if len(page) > 0 {
		startCursor = encodeCursor(page[0].Number)
		endCursor = encodeCursor(page[len(page)-1].Number)
	} else if after, ok := args["after"].(string); ok {
		endCursor = after
	}
	return gqlObject{
		"__typename": "IssueConnection", "nodes": nodes, "totalCount": len(all),
		"pageInfo": gqlObject{"__typename": "PageInfo", "hasNextPage": startAt+limit < len(all), "hasPreviousPage": startAt > 0, "startCursor": startCursor, "endCursor": endCursor},
	}
}

// resolveRootMutation resolves the Mutation root's fields.
func (c *gqlExecCtx) resolveRootMutation(name string, args map[string]any) any {
	depot := c.engine.depot
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	repoFromGlobalID := func(idValue any) *Repository {
		idText, ok := idValue.(string)
		if !ok {
			return nil
		}
		parts := decodeNodeID(idText)
		if parts == nil || parts[0] != "R" {
			return nil
		}
		wantedID, _ := strconv.Atoi(parts[1])
		for _, repo := range depot.RepoMap {
			if repo.ID == wantedID {
				return repo
			}
		}
		return nil
	}
	switch name {
	case "createIssue":
		repo := repoFromGlobalID(input["repositoryId"])
		if repo == nil {
			c.errors = append(c.errors, map[string]any{"message": "repository not found for createIssue"})
			return gqlObject{"__typename": "CreateIssuePayload", "issue": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		created := depot.OpenIssue(repo, title, body, nil)
		return gqlObject{"__typename": "CreateIssuePayload", "issue": makeIssueNode(repo, created)}
	case "closeIssue":
		idText, _ := input["issueId"].(string)
		parts := decodeNodeID(idText)
		var repo *Repository
		var issue *IssueRecord
		if parts != nil && parts[0] == "I" {
			wantedID, _ := strconv.Atoi(parts[1])
			for _, candidate := range depot.RepoMap {
				if candidate.ID == wantedID {
					repo = candidate
					issue = candidate.Issues[atoiOrZero(parts[2])]
				}
			}
		}
		if repo == nil || issue == nil {
			c.errors = append(c.errors, map[string]any{"message": "issue not found for closeIssue"})
			return gqlObject{"__typename": "CloseIssuePayload", "issue": nil}
		}
		issue.State = "closed"
		return gqlObject{"__typename": "CloseIssuePayload", "issue": makeIssueNode(repo, issue)}
	case "createPullRequest":
		repo := repoFromGlobalID(input["repositoryId"])
		if repo == nil {
			c.errors = append(c.errors, map[string]any{"message": "repository not found for createPullRequest"})
			return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		head, _ := input["headRefName"].(string)
		base, hasBase := input["baseRefName"].(string)
		if !hasBase {
			base = repo.DefaultBranch
		}
		result := depot.CreatePull(repo, title, body, head, base)
		if !result.OK {
			c.errors = append(c.errors, map[string]any{"message": result.Prob})
			return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": nil}
		}
		return gqlObject{"__typename": "CreatePullRequestPayload", "pullRequest": makePullNode(repo, result.Pull)}
	case "mergePullRequest":
		idText, _ := input["pullRequestId"].(string)
		parts := decodeNodeID(idText)
		var repo *Repository
		var pull *PullRecord
		if parts != nil && parts[0] == "P" {
			wantedID, _ := strconv.Atoi(parts[1])
			for _, candidate := range depot.RepoMap {
				if candidate.ID == wantedID {
					repo = candidate
					pull = candidate.Pulls[atoiOrZero(parts[2])]
				}
			}
		}
		if repo == nil || pull == nil {
			c.errors = append(c.errors, map[string]any{"message": "pull request not found for mergePullRequest"})
			return gqlObject{"__typename": "MergePullRequestPayload", "pullRequest": nil}
		}
		method := "squash"
		if methodText, ok := input["mergeMethod"].(string); ok {
			switch methodText {
			case "MERGE":
				method = "merge"
			case "REBASE":
				method = "rebase"
			}
		}
		result := depot.MergePullRequest(repo, pull, method)
		if !result.OK {
			c.errors = append(c.errors, map[string]any{"message": result.Prob})
			return gqlObject{"__typename": "MergePullRequestPayload", "pullRequest": makePullNode(repo, pull)}
		}
		return gqlObject{"__typename": "MergePullRequestPayload", "pullRequest": makePullNode(repo, pull)}
	case "addComment":
		idText, _ := input["subjectId"].(string)
		parts := decodeNodeID(idText)
		body, _ := input["body"].(string)
		if parts != nil && (parts[0] == "I" || parts[0] == "P") {
			wantedID, _ := strconv.Atoi(parts[1])
			for _, repo := range depot.RepoMap {
				if repo.ID != wantedID {
					continue
				}
				number := atoiOrZero(parts[2])
				commentOn := func(node gqlObject) gqlObject {
					return gqlObject{"__typename": "AddCommentPayload", "commentEdge": gqlObject{"__typename": "CommentEdge", "node": node, "cursor": encodeCursor(number)}}
				}
				if parts[0] == "I" {
					if issue, ok := repo.Issues[number]; ok {
						added := depot.PostComment(issue, body)
						node := gqlObject{
							"__typename": "IssueComment",
							"id":         encodeNodeID("C", repo.ID, number, added.ID),
							"body":       added.BodyText,
							"createdAt":  depot.ISOForSeq(added.SeqNo),
						}
						return commentOn(node)
					}
				} else if pull, ok := repo.Pulls[number]; ok {
					added := depot.PostComment(pull, body)
					node := gqlObject{
						"__typename": "IssueComment",
						"id":         encodeNodeID("C", repo.ID, number, added.ID),
						"body":       added.BodyText,
						"createdAt":  depot.ISOForSeq(added.SeqNo),
					}
					return commentOn(node)
				}
			}
		}
		c.errors = append(c.errors, map[string]any{"message": "subject not found for addComment"})
		return gqlObject{"__typename": "AddCommentPayload", "commentEdge": nil}
	}
	return nil
}

// atoiOrZero parses an int, treating any failure as zero.
func atoiOrZero(text string) int {
	number, _ := strconv.Atoi(text)
	return number
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

// gqlOutcome is one executed operation: its data plus any errors.
type gqlOutcome struct {
	data   map[string]any
	errors []map[string]any
}

// executeOperation parses and executes one operation object.
func (e *Engine) executeOperation(body map[string]any) gqlOutcome {
	queryText, _ := body["query"].(string)
	variables, _ := body["variables"].(map[string]any)
	if variables == nil {
		variables = map[string]any{}
	}
	parser := &gqlParseState{tokens: tokenizeGraphQL(queryText)}
	doc, ok := parser.readDocument()
	if !ok {
		return gqlOutcome{errors: []map[string]any{{"message": "parse error"}}}
	}
	if len(doc.operations) == 0 {
		return gqlOutcome{errors: []map[string]any{{"message": "no operation in document"}}}
	}
	opName, _ := body["operationName"].(string)
	chosen := doc.operations[0]
	for _, operation := range doc.operations {
		if operation.name == opName {
			chosen = operation
			break
		}
	}
	ctx := &gqlExecCtx{engine: e, variables: variables, fragments: doc.fragments}
	rootType := "Query"
	if chosen.kind == "mutation" {
		rootType = "Mutation"
	}
	data := ctx.runSelection(rootType, gqlObject{"__typename": rootType}, chosen.selection)
	return gqlOutcome{data: data, errors: ctx.errors}
}

// serveGraphQL serves POST /api/graphql: single operations and
// JSON-array batches alike.
func (e *Engine) serveGraphQL(req *ClientRequest, res *ClientResponse) {
	if !requestAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}
	parsed, ok := readAnyBody(req)
	if !ok {
		res.SendJSON(400, map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}})
		return
	}
	if batch, isBatch := parsed.([]any); isBatch {
		results := make([]any, 0, len(batch))
		for _, entry := range batch {
			operation, _ := entry.(map[string]any)
			if operation == nil {
				operation = map[string]any{}
			}
			results = append(results, e.outcomeJSON(e.executeOperation(operation)))
		}
		res.SendJSON(200, results)
		return
	}
	if operation, isOperation := parsed.(map[string]any); isOperation {
		res.SendJSON(200, e.outcomeJSON(e.executeOperation(operation)))
		return
	}
	res.SendJSON(400, map[string]any{"errors": []any{map[string]any{"message": "Expected a GraphQL operation object"}}})
}

// outcomeJSON renders an executed operation as the wire object,
// omitting the errors key entirely when there are none.
func (e *Engine) outcomeJSON(outcome gqlOutcome) map[string]any {
	rendered := map[string]any{"data": outcome.data}
	if len(outcome.errors) > 0 {
		errors := make([]any, 0, len(outcome.errors))
		for _, err := range outcome.errors {
			errors = append(errors, err)
		}
		rendered["errors"] = errors
	}
	return rendered
}
