// The REST v3 surface: repositories, branches, commits, contents,
// labels, issues, comments and pull requests, all under /api/v3 and
// rendered in GitHub's shapes. Everything reads and writes the one Depot, so
// what REST shows is exactly the data the git, GraphQL and MCP
// surfaces see.
//
// Wire discipline, which the protocol group grades: every answer is
// JSON with an X-GitHub-Request-Id (set centrally in router.go), error
// bodies are {"message": ...}, a missing token is a 401, a repository
// under another owner is a 404 (never a 403), and a malformed JSON
// body is a 400.
//
// The hottest shapes — repository and issue — are rendered as JSON
// text by hand with the fast string escaper instead of building a
// value and marshalling it: the write workload creates one issue per
// request, and the value round-trip is pure overhead there.

package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

const restSurfaceVersion = "1.0.0"

// repoJSONText renders a repository directly as JSON text.
func (e *Engine) repoJSONText(repo *Repository) string {
	stamp := e.depot.ISOForSeq(repo.SeqNo)
	login := e.depot.AdoptedLogin()
	var out strings.Builder
	out.WriteString(`{"id":`)
	out.WriteString(strconv.Itoa(repo.ID))
	out.WriteString(`,"name":`)
	out.WriteString(quoteJSONString(repo.Name))
	out.WriteString(`,"full_name":`)
	out.WriteString(quoteJSONString(login + "/" + repo.Name))
	out.WriteString(`,"owner":{"login":`)
	out.WriteString(quoteJSONString(login))
	out.WriteString(`,"id":1,"type":"User"},"private":`)
	if repo.Private {
		out.WriteString("true")
	} else {
		out.WriteString("false")
	}
	out.WriteString(`,"description":`)
	out.WriteString(quoteJSONString(repo.Description))
	out.WriteString(`,"default_branch":`)
	out.WriteString(quoteJSONString(repo.DefaultBranch))
	out.WriteString(`,"created_at":`)
	out.WriteString(quoteJSONString(stamp))
	out.WriteString(`,"updated_at":`)
	out.WriteString(quoteJSONString(stamp))
	out.WriteString("}\n")
	return out.String()
}

// repoJSON renders a repository as a value.
func (e *Engine) repoJSON(repo *Repository) map[string]any {
	login := e.depot.AdoptedLogin()
	stamp := e.depot.ISOForSeq(repo.SeqNo)
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      login + "/" + repo.Name,
		"owner":          map[string]any{"login": login, "id": 1, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     stamp,
		"updated_at":     stamp,
	}
}

// issueJSONText renders an issue directly as JSON text, with its
// labels as objects.
func (e *Engine) issueJSONText(repo *Repository, issue *IssueRecord) string {
	stamp := e.depot.ISOForSeq(issue.SeqNo)
	var out strings.Builder
	out.WriteString(`{"id":`)
	out.WriteString(strconv.Itoa(issue.ID))
	out.WriteString(`,"number":`)
	out.WriteString(strconv.Itoa(issue.Number))
	out.WriteString(`,"title":`)
	out.WriteString(quoteJSONString(issue.Title))
	out.WriteString(`,"body":`)
	out.WriteString(quoteJSONString(issue.BodyText))
	out.WriteString(`,"state":`)
	out.WriteString(quoteJSONString(issue.State))
	out.WriteString(`,"labels":[`)
	for i, name := range issue.LabelNames {
		if i > 0 {
			out.WriteByte(',')
		}
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		out.WriteString(`{"name":`)
		out.WriteString(quoteJSONString(name))
		out.WriteString(`,"color":`)
		out.WriteString(quoteJSONString(color))
		out.WriteByte('}')
	}
	out.WriteString(`],"user":{"login":`)
	out.WriteString(quoteJSONString(e.depot.AdoptedLogin()))
	out.WriteString(`,"id":1,"type":"User"},"comments":`)
	out.WriteString(strconv.Itoa(len(issue.Comments)))
	out.WriteString(`,"created_at":`)
	out.WriteString(quoteJSONString(stamp))
	out.WriteString(`,"updated_at":`)
	out.WriteString(quoteJSONString(stamp))
	out.WriteString("}\n")
	return out.String()
}

// issueJSON renders an issue as a value.
func (e *Engine) issueJSON(repo *Repository, issue *IssueRecord) map[string]any {
	labels := []any{}
	for _, name := range issue.LabelNames {
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	stamp := e.depot.ISOForSeq(issue.SeqNo)
	return map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.BodyText,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": e.depot.AdoptedLogin(), "id": 1, "type": "User"},
		"comments":   len(issue.Comments),
		"created_at": stamp,
		"updated_at": stamp,
	}
}

// pullJSON renders a pull request as a value.
func (e *Engine) pullJSON(repo *Repository, pull *PullRecord) map[string]any {
	login := e.depot.AdoptedLogin()
	var mergedAt any
	if pull.Merged {
		mergedAt = e.depot.ISOForSeq(pull.MergedSeq)
	}
	var mergeSHA any
	if pull.MergeCommitSHA != "" {
		mergeSHA = pull.MergeCommitSHA
	}
	stamp := e.depot.ISOForSeq(pull.SeqNo)
	return map[string]any{
		"id":               pull.ID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.BodyText,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head":             map[string]any{"ref": pull.HeadRef, "sha": pull.HeadSHA, "label": login + ":" + pull.HeadRef},
		"base":             map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSHA, "label": login + ":" + pull.BaseRef},
		"user":             map[string]any{"login": login, "id": 1, "type": "User"},
		"created_at":       stamp,
		"updated_at":       stamp,
	}
}

// commentJSON renders a comment as a value.
func (e *Engine) commentJSON(comment *CommentRecord) map[string]any {
	stamp := e.depot.ISOForSeq(comment.SeqNo)
	return map[string]any{
		"id":         comment.ID,
		"body":       comment.BodyText,
		"user":       map[string]any{"login": e.depot.AdoptedLogin(), "id": 1, "type": "User"},
		"created_at": stamp,
		"updated_at": stamp,
	}
}

// findRepo resolves /repos/{owner}/{name}: nil for a foreign owner or
// a missing name. A successful lookup is also what adopts the login.
func (e *Engine) findRepo(owner, name string) *Repository {
	repo, ok := e.depot.RepoMap[name]
	if !ok {
		return nil
	}
	if !e.depot.LoginCouldBe(owner) {
		return nil
	}
	e.depot.LearnLogin(owner)
	return repo
}

// stringField reads a string member of a request body.
func stringField(body map[string]any, name string) (string, bool) {
	value, present := body[name]
	if !present {
		return "", false
	}
	text, isText := value.(string)
	return text, isText
}

// boolFieldValue reads a boolean member of a request body.
func boolFieldValue(body map[string]any, name string) (bool, bool) {
	value, present := body[name]
	if !present {
		return false, false
	}
	flag, isBool := value.(bool)
	return flag, isBool
}

// serveREST serves one /api/v3 request; segments are the decoded path
// segments after the /api/v3 prefix.
func (e *Engine) serveREST(req *ClientRequest, res *ClientResponse, params map[string]string, segments []string) {
	// The API root and the meta endpoint answer without a repository.
	if len(segments) == 0 {
		if req.Method != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		res.SetHeader("x-oauth-scopes", "repo, read:org")
		res.SendJSON(200, map[string]any{
			"current_user_url":              "/api/v3/user",
			"current_user_repositories_url": "/api/v3/user/repos",
			"repository_url":                "/api/v3/repos/{owner}/{repo}",
		})
		return
	}
	if len(segments) == 1 && segments[0] == "meta" {
		if req.Method != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, map[string]any{"installed_version": restSurfaceVersion, "github_services_sha": "forge"})
		return
	}

	// Everything below the root needs the token.
	if !requestAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return
	}

	if len(segments) == 1 && segments[0] == "user" {
		if req.Method != "GET" {
			res.SendError(404, "Not Found")
			return
		}
		login := e.depot.AdoptedLogin()
		if login == "" {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, map[string]any{"login": login, "id": 1, "type": "User"})
		return
	}

	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		switch req.Method {
		case "GET":
			e.listOwnRepos(req, res, params)
		case "POST":
			e.createOwnRepo(req, res)
		default:
			res.SendError(404, "Not Found")
		}
		return
	}

	if len(segments) >= 3 && segments[0] == "repos" {
		repo := e.findRepo(segments[1], segments[2])
		if repo == nil {
			res.SendError(404, "Not Found")
			return
		}
		e.serveRepo(req, res, params, repo, segments[3:])
		return
	}

	res.SendError(404, "Not Found")
}

// listOwnRepos serves GET /user/repos, oldest first, with a Link
// header while further pages remain.
func (e *Engine) listOwnRepos(req *ClientRequest, res *ClientResponse, params map[string]string) {
	var all []*Repository
	for _, repo := range e.depot.RepoMap {
		all = append(all, repo)
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].SeqNo < all[j-1].SeqNo; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	pageSize := queryInt(params, "per_page", 30)
	if pageSize > 100 {
		pageSize = 100
	}
	pageNumber := queryInt(params, "page", 1)
	page := paginateList(all, params)
	items := make([]any, 0, len(page))
	for _, repo := range page {
		items = append(items, e.repoJSON(repo))
	}
	if pageNumber*pageSize < len(all) {
		host := req.Headers["host"]
		if host == "" {
			host = "127.0.0.1"
		}
		base := "http://" + host + "/api/v3/user/repos"
		lastPage := (len(all) + pageSize - 1) / pageSize
		res.SetHeader("link", "<"+base+"?per_page="+strconv.Itoa(pageSize)+"&page="+strconv.Itoa(pageNumber+1)+">; rel=\"next\", <"+base+"?per_page="+strconv.Itoa(pageSize)+"&page="+strconv.Itoa(lastPage)+">; rel=\"last\"")
	}
	res.SendJSON(200, items)
}

// createOwnRepo serves POST /user/repos.
func (e *Engine) createOwnRepo(req *ClientRequest, res *ClientResponse) {
	body, ok := readObjectBody(req)
	if !ok {
		res.SendError(400, "Problems parsing JSON")
		return
	}
	name, hasName := stringField(body, "name")
	if !hasName || name == "" {
		res.SendError(422, "Repository creation failed: name is required")
		return
	}
	if _, exists := e.depot.RepoMap[name]; exists {
		res.SendError(422, "Repository creation failed: name already exists on this account")
		return
	}
	description, _ := stringField(body, "description")
	private, _ := boolFieldValue(body, "private")
	created := e.depot.OpenRepository(name, description, private)
	res.SendJSONText(201, e.repoJSONText(created))
}

// serveRepo routes everything under /repos/{owner}/{name}/...
func (e *Engine) serveRepo(req *ClientRequest, res *ClientResponse, params map[string]string, repo *Repository, sub []string) {
	if len(sub) == 0 {
		switch req.Method {
		case "GET":
			res.SendJSONText(200, e.repoJSONText(repo))
		case "PATCH":
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			if description, ok := stringField(body, "description"); ok {
				repo.Description = description
			}
			if newName, ok := stringField(body, "name"); ok && newName != "" && newName != repo.Name {
				if _, taken := e.depot.RepoMap[newName]; !taken {
					delete(e.depot.RepoMap, repo.Name)
					repo.Name = newName
					e.depot.RepoMap[newName] = repo
				}
			}
			if private, ok := boolFieldValue(body, "private"); ok {
				repo.Private = private
			}
			if branch, ok := stringField(body, "default_branch"); ok && branch != "" {
				repo.DefaultBranch = branch
			}
			res.SendJSONText(200, e.repoJSONText(repo))
		case "DELETE":
			delete(e.depot.RepoMap, repo.Name)
			res.SendEmpty(204)
		default:
			res.SendError(404, "Not Found")
		}
		return
	}

	head, tail := sub[0], sub[1:]

	if head == "branches" {
		if len(tail) == 0 && req.Method == "GET" {
			var list []any
			for refName, oid := range repo.Refs {
				if !strings.HasPrefix(refName, "refs/heads/") {
					continue
				}
				branchName := refName[len("refs/heads/"):]
				sha, _ := e.depot.FollowToCommit(oid)
				list = append(list, map[string]any{"name": branchName, "commit": map[string]any{"sha": sha}, "protected": false})
			}
			// Sort by name so the answer is stable.
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j].(map[string]any)["name"].(string) < list[j-1].(map[string]any)["name"].(string); j-- {
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			res.SendJSON(200, paginateList(list, params))
			return
		}
		if len(tail) == 1 && req.Method == "GET" {
			tip, ok := e.depot.BranchTipOID(repo, tail[0])
			if !ok {
				res.SendError(404, "Not Found")
				return
			}
			sha, _ := e.depot.FollowToCommit(tip)
			res.SendJSON(200, map[string]any{"name": tail[0], "commit": map[string]any{"sha": sha}, "protected": false})
			return
		}
		res.SendError(404, "Not Found")
		return
	}

	if head == "commits" && len(tail) == 0 && req.Method == "GET" {
		refName := params["sha"]
		if refName == "" {
			refName = repo.DefaultBranch
		}
		tip, ok := e.depot.ResolveToCommit(repo, refName)
		if !ok {
			res.SendJSON(200, []any{})
			return
		}
		var rows []any
		for _, oid := range e.depot.HistoryFrom(tip) {
			head := DecodeCommit(e.depot.FetchObject(oid).Raw)
			author := SplitIdentLine(head.AuthorLine)
			committer := SplitIdentLine(head.CommitterLine)
			var parents []any
			for _, parent := range head.ParentOIDs {
				parents = append(parents, map[string]any{"sha": parent})
			}
			rows = append(rows, map[string]any{
				"sha": oid,
				"commit": map[string]any{
					"message":   head.MessageBody,
					"author":    map[string]any{"name": author.displayName, "email": author.mailbox, "date": FormatISOSecond(author.epochSec)},
					"committer": map[string]any{"name": committer.displayName, "email": committer.mailbox, "date": FormatISOSecond(committer.epochSec)},
					"tree":      map[string]any{"sha": head.TreeOID},
				},
				"parents": parents,
			})
		}
		res.SendJSON(200, paginateList(rows, params))
		return
	}

	if head == "git" && len(tail) > 0 && tail[0] == "refs" {
		if len(tail) == 1 && req.Method == "POST" {
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			refName, hasRef := stringField(body, "ref")
			shaHex, hasSHA := stringField(body, "sha")
			if !hasRef || !hasSHA {
				res.SendError(422, "ref and sha are required")
				return
			}
			if _, exists := repo.Refs[refName]; exists {
				res.SendError(422, "Reference already exists")
				return
			}
			if _, ok := e.depot.FollowToCommit(shaHex); !ok {
				res.SendError(422, "sha is not a commit in this repository")
				return
			}
			repo.Refs[refName] = shaHex
			res.SendJSON(201, map[string]any{"ref": refName, "object": map[string]any{"sha": shaHex, "type": "commit"}})
			return
		}
		if len(tail) >= 2 && req.Method == "GET" {
			refName := "refs/" + strings.Join(tail[1:], "/")
			oid, ok := repo.Refs[refName]
			if !ok {
				res.SendError(404, "Not Found")
				return
			}
			kindName := "commit"
			if obj := e.depot.FetchObject(oid); obj != nil {
				kindName = obj.Kind.Name()
			}
			res.SendJSON(200, map[string]any{"ref": refName, "object": map[string]any{"sha": oid, "type": kindName}})
			return
		}
		res.SendError(404, "Not Found")
		return
	}

	switch head {
	case "contents":
		e.serveContents(req, res, params, repo, strings.Join(tail, "/"))
	case "labels":
		e.serveLabels(req, res, repo, tail)
	case "issues":
		e.serveIssues(req, res, params, repo, tail)
	case "pulls":
		e.servePulls(req, res, params, repo, tail)
	default:
		res.SendError(404, "Not Found")
	}
}

// serveContents serves the contents endpoints: GET reads a file or a
// directory listing, PUT writes a file, DELETE removes one.
func (e *Engine) serveContents(req *ClientRequest, res *ClientResponse, params map[string]string, repo *Repository, path string) {
	if req.Method == "GET" {
		refName := params["ref"]
		if refName == "" {
			refName = repo.DefaultBranch
		}
		commitOID, ok := e.depot.ResolveToCommit(repo, refName)
		if !ok {
			res.SendError(404, "Not Found")
			return
		}
		match := e.depot.ResolvePath(commitOID, path)
		if match == nil {
			res.SendError(404, "Not Found")
			return
		}
		if match.Kind == "dir" {
			var entries []any
			for _, child := range match.Entries {
				isDir := child.Mode == "40000" || child.Mode == "040000"
				fullPath := child.Name
				if path != "" {
					fullPath = path + "/" + child.Name
				}
				size := 0
				if !isDir {
					if blob := e.depot.FetchObject(child.OID); blob != nil {
						size = len(blob.Raw)
					}
				}
				kindName := "file"
				if isDir {
					kindName = "dir"
				}
				entries = append(entries, map[string]any{
					"type": kindName, "name": child.Name, "path": fullPath, "sha": child.OID, "size": size,
				})
			}
			res.SendJSON(200, entries)
			return
		}
		baseName := path
		if cut := strings.LastIndex(path, "/"); cut >= 0 {
			baseName = path[cut+1:]
		}
		res.SendJSON(200, map[string]any{
			"type": "file", "name": baseName, "path": path, "sha": match.OID,
			"size": len(match.Raw), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(match.Raw),
		})
		return
	}
	if req.Method == "PUT" || req.Method == "DELETE" {
		body, ok := readObjectBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		message, _ := stringField(body, "message")
		branch, hasBranch := stringField(body, "branch")
		if !hasBranch {
			branch = repo.DefaultBranch
		}
		shaHex, hasSHA := stringField(body, "sha")
		if req.Method == "PUT" {
			contentText, hasContent := stringField(body, "content")
			if !hasContent {
				res.SendError(422, "content is required")
				return
			}
			stripped := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
					return -1
				}
				return r
			}, contentText)
			data, err := base64.StdEncoding.DecodeString(stripped)
			if err != nil {
				res.SendError(422, "content is not valid base64")
				return
			}
			alreadyThere := e.depot.PathIsFile(repo, branch, path)
			result := e.depot.WritePath(repo, branch, path, data, message, shaHex, hasSHA)
			if !result.OK {
				res.SendError(result.Status, result.Prob)
				return
			}
			status := 201
			if alreadyThere {
				status = 200
			}
			baseName := path
			if cut := strings.LastIndex(path, "/"); cut >= 0 {
				baseName = path[cut+1:]
			}
			res.SendJSON(status, map[string]any{
				"content": map[string]any{"sha": result.Blob, "encoding": "base64", "name": baseName, "path": path},
				"commit":  map[string]any{"sha": result.Commit},
			})
			return
		}
		result := e.depot.WritePath(repo, branch, path, nil, message, shaHex, hasSHA)
		if !result.OK {
			res.SendError(result.Status, result.Prob)
			return
		}
		res.SendJSON(200, map[string]any{"content": nil, "commit": map[string]any{"sha": result.Commit}})
		return
	}
	res.SendError(404, "Not Found")
}

// serveLabels serves the label list and label creation.
func (e *Engine) serveLabels(req *ClientRequest, res *ClientResponse, repo *Repository, tail []string) {
	if len(tail) == 0 && req.Method == "GET" {
		var list []any
		for _, label := range repo.Labels {
			list = append(list, map[string]any{"name": label.Name, "color": label.Color})
		}
		res.SendJSON(200, list)
		return
	}
	if len(tail) == 0 && req.Method == "POST" {
		body, ok := readObjectBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		name, hasName := stringField(body, "name")
		if !hasName || name == "" {
			res.SendError(422, "name is required")
			return
		}
		if _, exists := repo.Labels[name]; exists {
			res.SendError(422, "Label already exists")
			return
		}
		color, _ := stringField(body, "color")
		if color == "" {
			color = "ededed"
		}
		repo.Labels[name] = &LabelRecord{Name: name, Color: color}
		res.SendJSON(201, map[string]any{"name": name, "color": color})
		return
	}
	res.SendError(404, "Not Found")
}

// labelNamesFrom extracts the label names an issue body names, whether
// they arrive as plain strings or as {name} objects.
func labelNamesFrom(value any) []string {
	list, isList := value.([]any)
	if !isList {
		return nil
	}
	var names []string
	for _, item := range list {
		if text, isText := item.(string); isText {
			names = append(names, text)
		} else if object, isObject := item.(map[string]any); isObject {
			if text, isText := object["name"].(string); isText {
				names = append(names, text)
			}
		}
	}
	return names
}

// issueCreateResult reports the outcome of a checked issue creation.
type issueCreateResult struct {
	OK     bool
	Status int
	Prob   string
	Issue  *IssueRecord
}

// createIssueChecked is the shared, validating issue creation used by
// REST and, through the store, by GraphQL and MCP.
func (e *Engine) createIssueChecked(repo *Repository, title, body string, labels []string) issueCreateResult {
	if title == "" {
		return issueCreateResult{Status: 422, Prob: "title is required"}
	}
	for _, wanted := range labels {
		if _, known := repo.Labels[wanted]; !known {
			return issueCreateResult{Status: 422, Prob: "unknown label: " + wanted}
		}
	}
	created := e.depot.OpenIssue(repo, title, body, labels)
	return issueCreateResult{OK: true, Issue: created}
}

// serveIssues serves the issue collection, single issues and their
// comments.
func (e *Engine) serveIssues(req *ClientRequest, res *ClientResponse, params map[string]string, repo *Repository, tail []string) {
	if len(tail) == 0 {
		if req.Method == "POST" {
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			title, _ := stringField(body, "title")
			bodyText, _ := stringField(body, "body")
			result := e.createIssueChecked(repo, title, bodyText, labelNamesFrom(body["labels"]))
			if !result.OK {
				res.SendError(result.Status, result.Prob)
				return
			}
			res.SendJSONText(201, e.issueJSONText(repo, result.Issue))
			return
		}
		if req.Method == "GET" {
			stateFilter := params["state"]
			if stateFilter == "" {
				stateFilter = "open"
			}
			direction := params["direction"]
			if direction == "" {
				direction = "desc"
			}
			var list []*IssueRecord
			for _, issue := range repo.Issues {
				if stateFilter == "open" && issue.State != "open" {
					continue
				}
				if stateFilter == "closed" && issue.State != "closed" {
					continue
				}
				list = append(list, issue)
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0; j-- {
					var outOfOrder bool
					if direction == "asc" {
						outOfOrder = list[j].SeqNo < list[j-1].SeqNo
					} else {
						outOfOrder = list[j].SeqNo > list[j-1].SeqNo
					}
					if !outOfOrder {
						break
					}
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			page := paginateList(list, params)
			items := make([]any, 0, len(page))
			for _, issue := range page {
				items = append(items, e.issueJSON(repo, issue))
			}
			res.SendJSON(200, items)
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		res.SendError(404, "Not Found")
		return
	}
	issue, known := repo.Issues[number]
	if len(tail) == 1 {
		if !known {
			res.SendError(404, "Not Found")
			return
		}
		if req.Method == "GET" {
			res.SendJSONText(200, e.issueJSONText(repo, issue))
			return
		}
		if req.Method == "PATCH" {
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			if title, ok := stringField(body, "title"); ok {
				issue.Title = title
			}
			if bodyText, ok := stringField(body, "body"); ok {
				issue.BodyText = bodyText
			}
			if state, ok := stringField(body, "state"); ok && (state == "open" || state == "closed") {
				issue.State = state
			}
			if _, present := body["labels"]; present {
				wanted := labelNamesFrom(body["labels"])
				for _, name := range wanted {
					if _, known := repo.Labels[name]; !known {
						res.SendError(422, "unknown label: "+name)
						return
					}
				}
				issue.LabelNames = wanted
			}
			res.SendJSONText(200, e.issueJSONText(repo, issue))
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		if !known {
			res.SendError(404, "Not Found")
			return
		}
		if req.Method == "GET" {
			page := paginateList(issue.Comments, params)
			items := make([]any, 0, len(page))
			for _, comment := range page {
				items = append(items, e.commentJSON(comment))
			}
			res.SendJSON(200, items)
			return
		}
		if req.Method == "POST" {
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			commentBody, hasBody := stringField(body, "body")
			if !hasBody {
				res.SendError(422, "body is required")
				return
			}
			added := e.depot.PostComment(issue, commentBody)
			res.SendJSON(201, e.commentJSON(added))
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	res.SendError(404, "Not Found")
}

// servePulls serves the pull-request collection, single pulls, their
// file lists and merges.
func (e *Engine) servePulls(req *ClientRequest, res *ClientResponse, params map[string]string, repo *Repository, tail []string) {
	if len(tail) == 0 {
		if req.Method == "POST" {
			body, ok := readObjectBody(req)
			if !ok {
				res.SendError(400, "Problems parsing JSON")
				return
			}
			title, _ := stringField(body, "title")
			bodyText, _ := stringField(body, "body")
			head, _ := stringField(body, "head")
			base, hasBase := stringField(body, "base")
			if !hasBase {
				base = repo.DefaultBranch
			}
			result := e.depot.CreatePull(repo, title, bodyText, head, base)
			if !result.OK {
				res.SendError(422, result.Prob)
				return
			}
			res.SendJSON(201, e.pullJSON(repo, result.Pull))
			return
		}
		if req.Method == "GET" {
			stateFilter := params["state"]
			if stateFilter == "" {
				stateFilter = "open"
			}
			var list []*PullRecord
			for _, pull := range repo.Pulls {
				if stateFilter == "open" && pull.State != "open" {
					continue
				}
				if stateFilter == "closed" && pull.State != "closed" {
					continue
				}
				list = append(list, pull)
			}
			for i := 1; i < len(list); i++ {
				for j := i; j > 0 && list[j].SeqNo > list[j-1].SeqNo; j-- {
					list[j], list[j-1] = list[j-1], list[j]
				}
			}
			page := paginateList(list, params)
			items := make([]any, 0, len(page))
			for _, pull := range page {
				items = append(items, e.pullJSON(repo, pull))
			}
			res.SendJSON(200, items)
			return
		}
		res.SendError(404, "Not Found")
		return
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		res.SendError(404, "Not Found")
		return
	}
	pull, known := repo.Pulls[number]
	if len(tail) == 1 && req.Method == "GET" {
		if !known {
			res.SendError(404, "Not Found")
			return
		}
		res.SendJSON(200, e.pullJSON(repo, pull))
		return
	}
	if len(tail) == 2 && tail[1] == "files" && req.Method == "GET" {
		if !known {
			res.SendError(404, "Not Found")
			return
		}
		var rows []any
		for _, delta := range e.depot.ChangedFiles(repo, pull) {
			rows = append(rows, map[string]any{
				"filename": delta.Path, "status": delta.Status,
				"additions": delta.Additions, "deletions": delta.Deletions,
				"changes": delta.Additions + delta.Deletions,
			})
		}
		res.SendJSON(200, paginateList(rows, params))
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && req.Method == "PUT" {
		if !known {
			res.SendError(404, "Not Found")
			return
		}
		body, ok := readObjectBody(req)
		if !ok {
			res.SendError(400, "Problems parsing JSON")
			return
		}
		methodText, _ := stringField(body, "merge_method")
		mergeMethod := "squash"
		if methodText == "merge" || methodText == "rebase" {
			mergeMethod = methodText
		}
		result := e.depot.MergePullRequest(repo, pull, mergeMethod)
		if !result.OK {
			res.SendError(result.Status, result.Prob)
			return
		}
		res.SendJSON(200, map[string]any{"sha": result.SHA, "merged": true, "message": "Pull Request successfully merged"})
		return
	}
	res.SendError(404, "Not Found")
}
