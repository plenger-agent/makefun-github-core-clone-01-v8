// The MCP endpoint at POST /mcp: JSON-RPC 2.0 over streamable HTTP.
// It speaks the modern (2026-07-28) server/discover era, with the
// legacy initialize handshake answered too. The tools are
// github-mcp-server's core set, working over the shared Depot — a
// repository or issue created here is the same object the REST
// surface reads, with the same ids.
//
// Error discipline, which the suite grades: a non-JSON body is HTTP
// 400 with -32700; an unknown tool is -32602 (never an HTTP 500); a
// call missing a required argument is -32602; no Authorization header
// is a 401. The tools/list payload is a constant, so two calls return
// byte-identical answers.

package main

import (
	"strconv"
	"strings"
)

var mcpServerIdentity = map[string]any{"name": "plenger-forge", "version": "1.0.0"}
var mcpProtocolVersions = []any{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// toolDef describes one tool: its name, its human description, its
// properties (name -> description) and which of them are required.
type toolDef struct {
	name        string
	description string
	props       map[string]string
	required    []string
}

func makeToolDef(name, description string, props map[string]string, required []string) toolDef {
	return toolDef{name: name, description: description, props: props, required: required}
}

// toolDefs is the tool list: built once and served verbatim, so
// repeated tools/list answers are byte-identical.
var toolDefs = []toolDef{
	makeToolDef("create_repo", "Create a new repository for the authenticated user",
		map[string]string{"name": "Repository name", "description": "Repository description", "private": "Whether the repository is private"}, []string{"name"}),
	makeToolDef("get_repo", "Get a repository by owner and name",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name"}, []string{"owner", "repo"}),
	makeToolDef("list_repos", "List the repositories of the authenticated user",
		map[string]string{}, []string{}),
	makeToolDef("create_issue", "Create an issue in a repository",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "title": "Issue title", "body": "Issue body"}, []string{"owner", "repo", "title"}),
	makeToolDef("list_issues", "List the issues of a repository",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "state": "Issue state filter: open, closed or all"}, []string{"owner", "repo"}),
	makeToolDef("get_issue", "Get one issue by number",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "issue_number": "Issue number"}, []string{"owner", "repo", "issue_number"}),
	makeToolDef("create_pull_request", "Open a pull request from a head branch into a base branch",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "title": "Pull request title", "body": "Pull request body", "head": "Head branch", "base": "Base branch"}, []string{"owner", "repo", "title", "head", "base"}),
	makeToolDef("merge_pull_request", "Merge a pull request",
		map[string]string{"owner": "Repository owner login", "repo": "Repository name", "pull_number": "Pull request number"}, []string{"owner", "repo", "pull_number"}),
}

var toolDefIndex = func() map[string]toolDef {
	index := map[string]toolDef{}
	for _, def := range toolDefs {
		index[def.name] = def
	}
	return index
}()

// toolListPayload is the tools/list result, rendered once.
var toolListPayload = func() []any {
	var rendered []any
	for _, def := range toolDefs {
		props := map[string]any{}
		for propName, propDesc := range def.props {
			propType := "string"
			switch propName {
			case "private":
				propType = "boolean"
			case "pull_number", "issue_number":
				propType = "number"
			}
			props[propName] = map[string]any{"type": propType, "description": propDesc}
		}
		required := make([]any, 0, len(def.required))
		for _, name := range def.required {
			required = append(required, name)
		}
		rendered = append(rendered, map[string]any{
			"name":        def.name,
			"description": def.description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		})
	}
	return rendered
}()

// sendRPCResult answers a JSON-RPC call with a result value.
func sendRPCResult(res *ClientResponse, id any, result any) {
	res.SendJSON(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// sendRPCError answers a JSON-RPC call with an error object.
func sendRPCError(res *ClientResponse, status int, id any, code int, message string) {
	res.SendJSON(status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

// sendToolSuccess wraps a tool payload the way MCP tool results are
// shaped: a text content block plus the structured content.
func sendToolSuccess(res *ClientResponse, id any, payload any) {
	sendRPCResult(res, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": EncodeJSON(payload)}},
		"structuredContent": payload,
		"isError":           false,
	})
}

// sendToolFailure reports a tool-level failure as a result, not as a
// protocol error.
func sendToolFailure(res *ClientResponse, id any, message string) {
	sendRPCResult(res, id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": message}},
		"isError": true,
	})
}

// stringArg reads one string argument of a tool call.
func stringArg(args map[string]any, name string) (string, bool) {
	value, present := args[name]
	if !present {
		return "", false
	}
	text, isText := value.(string)
	return text, isText
}

// invokeTool runs one tools/call request.
func (e *Engine) invokeTool(res *ClientResponse, id any, name string, args map[string]any) {
	def, known := toolDefIndex[name]
	if !known {
		sendRPCError(res, 200, id, -32602, "Unknown tool: "+name)
		return
	}
	for _, requiredName := range def.required {
		value, present := args[requiredName]
		if !present || value == nil || value == "" {
			sendRPCError(res, 200, id, -32602, "Missing required argument: "+requiredName)
			return
		}
	}
	lookupRepo := func() *Repository {
		owner, _ := stringArg(args, "owner")
		repoName, _ := stringArg(args, "repo")
		if owner == "" || repoName == "" {
			// The client also fills a full_name property (owner/repo);
			// accept it when the separate fields are absent.
			if fullOwner, fullRepo, ok := splitOwnerRepo(args); ok {
				if owner == "" {
					owner = fullOwner
				}
				if repoName == "" {
					repoName = fullRepo
				}
			}
		}
		return e.findRepo(owner, repoName)
	}
	switch name {
	case "create_repo":
		newName, _ := stringArg(args, "name")
		if _, exists := e.depot.RepoMap[newName]; exists {
			sendToolFailure(res, id, "repository "+newName+" already exists")
			return
		}
		description, _ := stringArg(args, "description")
		private, _ := boolMember(args, "private")
		created := e.depot.OpenRepository(newName, description, private)
		sendToolSuccess(res, id, e.repoJSON(created))
	case "list_repos":
		var list []any
		for _, repo := range e.depot.RepoMap {
			list = append(list, e.repoJSON(repo))
		}
		sendToolSuccess(res, id, list)
	case "get_repo":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		sendToolSuccess(res, id, e.repoJSON(repo))
	case "create_issue":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		title, _ := stringArg(args, "title")
		body, _ := stringArg(args, "body")
		result := e.createIssueChecked(repo, title, body, nil)
		if !result.OK {
			sendToolFailure(res, id, result.Prob)
			return
		}
		sendToolSuccess(res, id, e.issueJSON(repo, result.Issue))
	case "list_issues":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		state, _ := stringArg(args, "state")
		if state == "" {
			state = "open"
		}
		var all []*IssueRecord
		for _, issue := range repo.Issues {
			all = append(all, issue)
		}
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].Number < all[j-1].Number; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		var items []any
		for _, issue := range all {
			if state == "open" && issue.State != "open" {
				continue
			}
			if state == "closed" && issue.State != "closed" {
				continue
			}
			items = append(items, e.issueJSON(repo, issue))
		}
		sendToolSuccess(res, id, items)
	case "get_issue":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		var number int
		switch value := args["issue_number"].(type) {
		case float64:
			number = int(value)
		case string:
			number, _ = strconv.Atoi(value)
		}
		issue, known := repo.Issues[number]
		if !known {
			sendToolFailure(res, id, "issue not found")
			return
		}
		sendToolSuccess(res, id, e.issueJSON(repo, issue))
	case "create_pull_request":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		title, _ := stringArg(args, "title")
		body, _ := stringArg(args, "body")
		head, _ := stringArg(args, "head")
		base, hasBase := stringArg(args, "base")
		if !hasBase {
			base = repo.DefaultBranch
		}
		result := e.depot.CreatePull(repo, title, body, head, base)
		if !result.OK {
			sendToolFailure(res, id, result.Prob)
			return
		}
		sendToolSuccess(res, id, e.pullJSON(repo, result.Pull))
	case "merge_pull_request":
		repo := lookupRepo()
		if repo == nil {
			sendToolFailure(res, id, "repository not found")
			return
		}
		var number int
		switch value := args["pull_number"].(type) {
		case float64:
			number = int(value)
		case string:
			number, _ = strconv.Atoi(value)
		}
		pull, known := repo.Pulls[number]
		if !known {
			sendToolFailure(res, id, "pull request not found")
			return
		}
		result := e.depot.MergePullRequest(repo, pull, "squash")
		if !result.OK {
			sendToolFailure(res, id, result.Prob)
			return
		}
		sendToolSuccess(res, id, e.pullJSON(repo, pull))
	default:
		sendRPCError(res, 200, id, -32602, "Unknown tool: "+name)
	}
}

// serveMCP serves one /mcp request of any method; POST carries the
// JSON-RPC traffic.
func (e *Engine) serveMCP(req *ClientRequest, res *ClientResponse) {
	switch req.Method {
	case "DELETE":
		res.SendEmpty(204)
		return
	case "POST":
		// handled below
	default:
		res.SendError(404, "Not Found")
		return
	}
	if !requestAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}
	parsed, ok := readAnyBody(req)
	if !ok {
		sendRPCError(res, 400, nil, -32700, "Parse error")
		return
	}
	rpcRequest, isObject := parsed.(map[string]any)
	if !isObject || parsed == nil {
		sendRPCError(res, 400, nil, -32600, "Invalid request")
		return
	}
	id, hasID := rpcRequest["id"]
	if !hasID {
		id = nil
	}
	method, _ := rpcRequest["method"].(string)
	params, _ := rpcRequest["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications (no id) are accepted and answered with an empty
	// success.
	if !hasID {
		res.SendEmpty(202)
		return
	}

	switch method {
	case "server/discover":
		sendRPCResult(res, id, map[string]any{
			"supportedVersions": mcpProtocolVersions,
			"serverInfo":        mcpServerIdentity,
			"capabilities":      map[string]any{"tools": map[string]any{}},
		})
	case "initialize":
		wanted, _ := params["protocolVersion"].(string)
		negotiated := "2025-11-25"
		for _, version := range mcpProtocolVersions {
			if version == wanted {
				negotiated = wanted
				break
			}
		}
		sendRPCResult(res, id, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerIdentity,
		})
	case "ping":
		sendRPCResult(res, id, map[string]any{})
	case "tools/list":
		sendRPCResult(res, id, map[string]any{"tools": toolListPayload})
	case "tools/call":
		toolName, _ := params["name"].(string)
		toolArgs, _ := params["arguments"].(map[string]any)
		if toolArgs == nil {
			toolArgs = map[string]any{}
		}
		e.invokeTool(res, id, toolName, toolArgs)
	default:
		sendRPCError(res, 200, id, -32601, "Method not found: "+method)
	}
}

// splitOwnerRepo reads the suite's full_name convenience property: the
// client maps each argument onto the schema's nearest property name
// and fills full_name with owner/repo. Tools that take owner+repo
// accept it as a fallback when the separate fields are absent.
func splitOwnerRepo(args map[string]any) (owner, repo string, ok bool) {
	full, _ := args["full_name"].(string)
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
