// REST API v3 handlers. Every reply is JSON, carries X-GitHub-Request-Id,
// and uses GitHub's status conventions (404, never 403, for a repo owned by
// someone else; 422 for a duplicate name or an unknown label; 409 for a
// stale content sha; 405 for a conflicted merge).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// JSON shapes

func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id":            r.ID,
		"name":          r.Name,
		"full_name":     r.Owner + "/" + r.Name,
		"owner":         map[string]any{"login": r.Owner, "id": viewerID, "type": "User"},
		"private":       r.Private,
		"description":   r.Description,
		"default_branch": r.DefaultBranch,
		"created_at":    r.CreatedAt,
		"updated_at":    r.UpdatedAt,
		"html_url":      "https://example.invalid/" + r.Owner + "/" + r.Name,
		"url":           "/api/v3/repos/" + r.Owner + "/" + r.Name,
		"has_issues":    true,
	}
}

func labelJSON(l *Label) map[string]any {
	return map[string]any{"name": l.Name, "color": l.Color}
}

func issueJSONObj(i *Issue) map[string]any {
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, labelJSON(l))
	}
	return map[string]any{
		"id":         i.ID,
		"number":     i.Number,
		"title":      i.Title,
		"body":       i.Body,
		"state":      i.State,
		"labels":     labels,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": i.CreatedAt,
		"updated_at": i.UpdatedAt,
		"comments":   0,
	}
}

func pullJSONObj(p *Pull) map[string]any {
	return map[string]any{
		"id":               p.ID,
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"merged":           p.Merged,
		"merged_at":        nullable(p.MergedAt),
		"merge_commit_sha": nullable(p.MergeCommitSH),
		"head":             map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA},
		"base":             map[string]any{"ref": p.BaseRef, "sha": p.BaseSHA},
		"user":             map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at":       p.CreatedAt,
		"updated_at":       p.UpdatedAt,
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// plumbing

type restHandler struct {
	store *Store
}

func writeJSON(w http.ResponseWriter, s *Store, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", s.NextRequestID())
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", s.NextRequestID())
	w.WriteHeader(status)
	w.Write(body)
}

func writeJSONBytes(w http.ResponseWriter, s *Store, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", s.NextRequestID())
	w.WriteHeader(status)
	w.Write(body)
}

func writeErr(w http.ResponseWriter, s *Store, status int, msg string) {
	writeJSON(w, s, status, map[string]any{"message": msg})
}

// readBodyJSON parses the request body as JSON. Returns false on failure
// (and writes the 400 reply).
func readBodyJSON(w http.ResponseWriter, s *Store, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		writeErr(w, s, 400, "Problems parsing JSON")
		return nil, false
	}
	if body == nil {
		body = map[string]any{}
	}
	return body, true
}

func strField(body map[string]any, key, def string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return def
}

func boolField(body map[string]any, key string, def bool) bool {
	if v, ok := body[key].(bool); ok {
		return v
	}
	return def
}

// pagination parses page/per_page query params.
func pagination(r *http.Request) (page, perPage int) {
	page, perPage = 1, 30
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			perPage = n
		}
	}
	return page, perPage
}

// linkHeader builds a GitHub-style Link header with absolute URLs on the
// same origin as the request.
func linkHeader(r *http.Request, page, perPage, total int) string {
	if total <= perPage {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host + r.URL.Path
	last := (total + perPage - 1) / perPage
	var parts []string
	if page < last {
		parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"next\"", base, page+1, perPage))
	}
	if page > 1 {
		parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"prev\"", base, page-1, perPage))
	}
	parts = append(parts, fmt.Sprintf("<%s?page=1&per_page=%d>; rel=\"first\"", base, perPage))
	parts = append(parts, fmt.Sprintf("<%s?page=%d&per_page=%d>; rel=\"last\"", base, last, perPage))
	return strings.Join(parts, ", ")
}

// pageOf slices a list for the given page.
func pageOf[T any](items []T, page, perPage int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// ---------------------------------------------------------------------------
// routing

// ServeREST handles all /api/v3/* routes.
func (h *restHandler) ServeREST(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if path == "" || path == "/" {
		h.root(w, r)
		return
	}
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	switch parts[0] {
	case "meta":
		if r.Method == http.MethodGet && len(parts) == 1 {
			writeJSON(w, h.store, 200, map[string]any{"installed_version": "1.0.0"})
			return
		}
	case "user":
		h.user(w, r, parts[1:])
		return
	case "repos":
		h.repos(w, r, parts[1:])
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) root(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, read:org")
	writeJSON(w, h.store, 200, map[string]any{
		"current_user_url": "https://example.invalid/api/v3/user",
		"rate_limit_url":   "https://example.invalid/api/v3/rate_limit",
	})
}

func (h *restHandler) user(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		if r.Method == http.MethodGet {
			writeJSON(w, h.store, 200, map[string]any{
				"login": viewerLogin, "id": viewerID, "name": viewerName,
				"email": viewerEmail, "type": "User",
			})
			return
		}
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if parts[0] == "repos" && len(parts) == 1 {
		switch r.Method {
		case http.MethodPost:
			h.createRepo(w, r)
		case http.MethodGet:
			h.listRepos(w, r)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) createRepo(w http.ResponseWriter, r *http.Request) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	name := strField(body, "name", "")
	if name == "" {
		writeErr(w, h.store, 422, "name is required")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	repo := h.store.CreateRepo(name, boolField(body, "private", false), strField(body, "description", ""))
	if repo == nil {
		writeErr(w, h.store, 422, "Repository already exists")
		return
	}
	writeJSON(w, h.store, 201, repoJSON(repo))
}

func (h *restHandler) listRepos(w http.ResponseWriter, r *http.Request) {
	page, perPage := pagination(r)
	h.store.mu.RLock()
	repos := h.store.ListRepos()
	total := len(repos)
	pageRepos := pageOf(repos, page, perPage)
	items := make([]any, 0, len(pageRepos))
	for _, repo := range pageRepos {
		items = append(items, repoJSON(repo))
	}
	h.store.mu.RUnlock()
	if link := linkHeader(r, page, perPage, total); link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, h.store, 200, items)
}

// repos routes /repos/{owner}/{repo}/...
func (h *restHandler) repos(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	owner, repoName := parts[0], parts[1]
	rest := parts[2:]

	// A repo under another owner is 404, never 403.
	if owner != viewerLogin {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	h.store.mu.RLock()
	repo := h.store.GetRepo(owner, repoName)
	h.store.mu.RUnlock()
	if repo == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			h.store.mu.RLock()
			writeJSON(w, h.store, 200, repoJSON(repo))
			h.store.mu.RUnlock()
		case http.MethodPatch:
			h.patchRepo(w, r, repo)
		case http.MethodDelete:
			h.store.mu.Lock()
			h.store.DeleteRepo(owner, repoName)
			h.store.mu.Unlock()
			w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
			w.WriteHeader(204)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		h.branches(w, r, repo, rest[1:])
	case "git":
		h.gitRefs(w, r, repo, rest[1:])
	case "commits":
		h.commits(w, r, repo)
	case "contents":
		h.contents(w, r, repo, rest[1:])
	case "labels":
		h.labels(w, r, repo, rest[1:])
	case "issues":
		h.issues(w, r, repo, rest[1:])
	case "pulls":
		h.pulls(w, r, repo, rest[1:])
	default:
		writeErr(w, h.store, 404, "Not Found")
	}
}

func (h *restHandler) patchRepo(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if v, ok := body["description"].(string); ok {
		repo.Description = v
	}
	if v, ok := body["private"].(bool); ok {
		repo.Private = v
	}
	writeJSON(w, h.store, 200, repoJSON(repo))
}

func (h *restHandler) branches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != http.MethodGet {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	if len(rest) == 0 {
		names := []string{}
		for _, name := range repo.RefNames() {
			if strings.HasPrefix(name, "refs/heads/") {
				names = append(names, strings.TrimPrefix(name, "refs/heads/"))
			}
		}
		sort.Strings(names)
		items := make([]any, 0, len(names))
		for _, name := range names {
			items = append(items, map[string]any{
				"name":   name,
				"commit": map[string]any{"sha": repo.BranchTip(name)},
			})
		}
		writeJSON(w, h.store, 200, items)
		return
	}
	branch := strings.Join(rest, "/")
	tip := repo.BranchTip(branch)
	if tip == "" {
		writeErr(w, h.store, 404, "Branch not found")
		return
	}
	writeJSON(w, h.store, 200, map[string]any{
		"name":   branch,
		"commit": map[string]any{"sha": tip},
	})
}

func (h *restHandler) gitRefs(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) >= 2 && rest[0] == "refs" && rest[1] == "heads" {
		if r.Method == http.MethodGet {
			branch := strings.Join(rest[2:], "/")
			h.store.mu.RLock()
			tip := repo.BranchTip(branch)
			h.store.mu.RUnlock()
			if tip == "" {
				writeErr(w, h.store, 404, "Reference does not exist")
				return
			}
			writeJSON(w, h.store, 200, map[string]any{
				"ref":    "refs/heads/" + branch,
				"object": map[string]any{"sha": tip, "type": "commit"},
			})
			return
		}
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if len(rest) == 1 && rest[0] == "refs" && r.Method == http.MethodPost {
		body, ok := readBodyJSON(w, h.store, r)
		if !ok {
			return
		}
		ref := strField(body, "ref", "")
		sha := strField(body, "sha", "")
		if ref == "" || sha == "" {
			writeErr(w, h.store, 422, "ref and sha are required")
			return
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		if repo.Objects[sha] == nil {
			writeErr(w, h.store, 422, "Object does not exist")
			return
		}
		if _, exists := repo.Refs[ref]; exists {
			writeErr(w, h.store, 422, "Reference already exists")
			return
		}
		repo.Refs[ref] = sha
		repo.invalidate()
		writeJSON(w, h.store, 201, map[string]any{
			"ref":    ref,
			"object": map[string]any{"sha": sha, "type": "commit"},
		})
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) commits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if r.Method != http.MethodGet {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	shaParam := r.URL.Query().Get("sha")
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	start := repo.BranchTip(repo.DefaultBranch)
	if shaParam != "" {
		if tip := repo.BranchTip(shaParam); tip != "" {
			start = tip
		} else if repo.Objects[shaParam] != nil {
			start = shaParam
		} else {
			writeErr(w, h.store, 404, "No commit found for SHA: "+shaParam)
			return
		}
	}
	// Walk newest first (BFS over parents).
	items := []any{}
	seen := map[string]bool{}
	queue := []string{}
	if start != "" {
		queue = append(queue, start)
	}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		items = append(items, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				// GitHub's API strips the trailing newline git stores.
				"message": strings.TrimSuffix(c.Message, "\n"),
				"author":  map[string]any{"name": viewerName, "email": viewerEmail, "date": h.store.nowISO},
				"committer": map[string]any{"name": viewerName, "email": viewerEmail, "date": h.store.nowISO},
			},
			"author": map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		})
		queue = append(queue, c.Parents...)
	}
	writeJSON(w, h.store, 200, items)
}

func (h *restHandler) contents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	path := strings.Join(rest, "/")
	if path == "" {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getContents(w, r, repo, path)
	case http.MethodPut:
		h.putContents(w, r, repo, path)
	case http.MethodDelete:
		h.deleteContents(w, r, repo, path)
	default:
		writeErr(w, h.store, 404, "Not Found")
	}
}

func (h *restHandler) resolveRef(repo *Repo, ref string) string {
	if ref == "" {
		return repo.BranchTip(repo.DefaultBranch)
	}
	if tip := repo.BranchTip(ref); tip != "" {
		return tip
	}
	if repo.Objects[ref] != nil {
		return ref
	}
	return ""
}

func (h *restHandler) getContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	tip := h.resolveRef(repo, ref)
	if tip == "" {
		writeErr(w, h.store, 404, "This repository is empty.")
		return
	}
	if blobOID, data, ok := repo.FileAt(tip, path); ok {
		writeJSON(w, h.store, 200, map[string]any{
			"type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(data),
			"sha":     blobOID,
			"size":    len(data),
			"name":    path[strings.LastIndex(path, "/")+1:],
			"path":    path,
		})
		return
	}
	if entries := repo.ListDir(tip, path); entries != nil {
		items := make([]any, 0, len(entries))
		for _, e := range entries {
			typ := "file"
			if e.Mode == "40000" {
				typ = "dir"
			}
			items = append(items, map[string]any{
				"type": typ, "name": e.Name, "path": path + "/" + e.Name, "sha": e.OID,
			})
		}
		writeJSON(w, h.store, 200, items)
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) putContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	message := strField(body, "message", "Update "+path)
	contentB64 := strField(body, "content", "")
	branch := strField(body, "branch", repo.DefaultBranch)
	sha := strField(body, "sha", "")
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		writeErr(w, h.store, 400, "content is not valid base64")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	// Lenient branch creation: a named branch that does not exist yet starts
	// from the default branch's tip.
	if repo.BranchTip(branch) == "" && branch != repo.DefaultBranch {
		if tip := repo.BranchTip(repo.DefaultBranch); tip != "" {
			repo.Refs["refs/heads/"+branch] = tip
		}
	}
	tip := repo.BranchTip(branch)
	exists := false
	var currentBlob string
	if tip != "" {
		if blobOID, _, ok := repo.FileAt(tip, path); ok {
			exists = true
			currentBlob = blobOID
		}
	}
	if exists && sha != "" && sha != currentBlob {
		writeErr(w, h.store, 409, "sha does not match the current blob")
		return
	}
	commitOID, err := repo.CommitFile(h.store, branch, path, content, false, message)
	if err != nil {
		writeErr(w, h.store, 422, err.Error())
		return
	}
	blobOID, _, _ := repo.FileAt(commitOID, path)
	status := 201
	if exists {
		status = 200
	}
	writeJSON(w, h.store, status, map[string]any{
		"commit": map[string]any{"sha": commitOID},
		"content": map[string]any{
			"type": "file", "encoding": "base64", "content": contentB64,
			"sha": blobOID, "size": len(content), "name": path, "path": path,
		},
	})
}

func (h *restHandler) deleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	message := strField(body, "message", "Delete "+path)
	branch := strField(body, "branch", repo.DefaultBranch)
	sha := strField(body, "sha", "")
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	tip := repo.BranchTip(branch)
	if tip == "" {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	currentBlob, _, exists := repo.FileAt(tip, path)
	if !exists {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if sha != "" && sha != currentBlob {
		writeErr(w, h.store, 409, "sha does not match the current blob")
		return
	}
	commitOID, err := repo.CommitFile(h.store, branch, path, nil, true, message)
	if err != nil {
		writeErr(w, h.store, 422, err.Error())
		return
	}
	writeJSON(w, h.store, 200, map[string]any{
		"commit": map[string]any{"sha": commitOID},
	})
}

func (h *restHandler) labels(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) != 0 {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodPost:
		body, ok := readBodyJSON(w, h.store, r)
		if !ok {
			return
		}
		name := strField(body, "name", "")
		color := strField(body, "color", "ededed")
		if name == "" {
			writeErr(w, h.store, 422, "name is required")
			return
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		if existing := repo.Labels[name]; existing != nil {
			writeErr(w, h.store, 422, "Label already exists")
			return
		}
		label := &Label{Name: name, Color: color}
		repo.Labels[name] = label
		writeJSON(w, h.store, 201, labelJSON(label))
	case http.MethodGet:
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		names := make([]string, 0, len(repo.Labels))
		for name := range repo.Labels {
			names = append(names, name)
		}
		sort.Strings(names)
		items := make([]any, 0, len(names))
		for _, name := range names {
			items = append(items, labelJSON(repo.Labels[name]))
		}
		writeJSON(w, h.store, 200, items)
	default:
		writeErr(w, h.store, 404, "Not Found")
	}
}

// ---------------------------------------------------------------------------
// issues

func (h *restHandler) issues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			h.createIssue(w, r, repo)
		case http.MethodGet:
			h.listIssues(w, r, repo)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.getIssue(w, r, repo, n)
		case http.MethodPatch:
			h.patchIssue(w, r, repo, n)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodPost:
			h.createComment(w, r, repo, n)
		case http.MethodGet:
			h.listComments(w, r, repo, n)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) createIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	title := strField(body, "title", "")
	if title == "" {
		writeErr(w, h.store, 422, "title is required")
		return
	}
	labelNames := []string{}
	if raw, ok := body["labels"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				labelNames = append(labelNames, s)
			} else if m, ok := v.(map[string]any); ok {
				if s, ok := m["name"].(string); ok {
					labelNames = append(labelNames, s)
				}
			}
		}
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	issue, err := repo.CreateIssue(h.store, title, strField(body, "body", ""), labelNames)
	if err != nil {
		writeErr(w, h.store, 422, err.Error())
		return
	}
	writeJSON(w, h.store, 201, issueJSONObj(issue))
}

func (h *restHandler) listIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortField := q.Get("sort")
	direction := q.Get("direction")
	page, perPage := pagination(r)
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	issues := []*Issue{}
	for _, issue := range repo.Issues {
		if state != "all" && issue.State != state {
			continue
		}
		issues = append(issues, issue)
	}
	// Sort by creation order (number). sort=created with direction=asc/desc.
	desc := direction == "desc" || (direction == "" && sortField == "created")
	if sortField == "" {
		desc = true // GitHub's default for issues is created desc
	}
	sort.Slice(issues, func(i, j int) bool {
		if desc {
			return issues[i].Number > issues[j].Number
		}
		return issues[i].Number < issues[j].Number
	})
	total := len(issues)
	pageIssues := pageOf(issues, page, perPage)
	items := make([]any, 0, len(pageIssues))
	for _, issue := range pageIssues {
		items = append(items, issueJSONObj(issue))
	}
	if link := linkHeader(r, page, perPage, total); link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, h.store, 200, items)
}

func (h *restHandler) getIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	// The read workload hammers this endpoint: serve cached bytes. The
	// cache write needs the write lock; the workload is sequential, so the
	// lock costs nothing.
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	issue := repo.Issues[n]
	if issue == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	// The read workload hammers this endpoint: serve cached bytes.
	if cached, ok := repo.issueJSON[n]; ok {
		writeJSONBytes(w, h.store, 200, cached)
		return
	}
	body, _ := json.Marshal(issueJSONObj(issue))
	if repo.issueJSON == nil {
		repo.issueJSON = map[int][]byte{}
	}
	repo.issueJSON[n] = body
	writeJSONBytes(w, h.store, 200, body)
}

func (h *restHandler) patchIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	issue := repo.Issues[n]
	if issue == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if v, ok := body["state"].(string); ok {
		issue.State = v
	}
	if v, ok := body["title"].(string); ok {
		issue.Title = v
	}
	if v, ok := body["body"].(string); ok {
		issue.Body = v
	}
	issue.UpdatedAt = h.store.nowISO
	delete(repo.issueJSON, n)
	writeJSON(w, h.store, 200, issueJSONObj(issue))
}

func (h *restHandler) createComment(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	commentBody := strField(body, "body", "")
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if repo.Issues[n] == nil && repo.Pulls[n] == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	repo.nextCommentID++
	c := &Comment{ID: repo.nextCommentID, Body: commentBody, CreatedAt: h.store.nowISO, UpdatedAt: h.store.nowISO}
	repo.Comments[n] = append(repo.Comments[n], c)
	writeJSON(w, h.store, 201, map[string]any{
		"id": c.ID, "body": c.Body,
		"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
	})
}

func (h *restHandler) listComments(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	if repo.Issues[n] == nil && repo.Pulls[n] == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	items := make([]any, 0, len(repo.Comments[n]))
	for _, c := range repo.Comments[n] {
		items = append(items, map[string]any{
			"id": c.ID, "body": c.Body,
			"user":       map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
			"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
		})
	}
	writeJSON(w, h.store, 200, items)
}

// ---------------------------------------------------------------------------
// pull requests

func (h *restHandler) pulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			h.createPull(w, r, repo)
		case http.MethodGet:
			h.listPulls(w, r, repo)
		default:
			writeErr(w, h.store, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method == http.MethodGet {
			h.getPull(w, r, repo, n)
			return
		}
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		h.pullFiles(w, r, repo, n)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		h.mergePull(w, r, repo, n)
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *restHandler) createPull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readBodyJSON(w, h.store, r)
	if !ok {
		return
	}
	title := strField(body, "title", "")
	head := strField(body, "head", "")
	base := strField(body, "base", "")
	if title == "" || head == "" || base == "" {
		writeErr(w, h.store, 422, "title, head and base are required")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	pr, err := repo.CreatePull(h.store, title, strField(body, "body", ""), head, base)
	if err != nil {
		writeErr(w, h.store, 422, err.Error())
		return
	}
	writeJSON(w, h.store, 201, pullJSONObj(pr))
}

func (h *restHandler) listPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	page, perPage := pagination(r)
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	pulls := []*Pull{}
	for _, pr := range repo.Pulls {
		if state != "all" && pr.State != state {
			continue
		}
		pulls = append(pulls, pr)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	total := len(pulls)
	pagePulls := pageOf(pulls, page, perPage)
	items := make([]any, 0, len(pagePulls))
	for _, pr := range pagePulls {
		items = append(items, pullJSONObj(pr))
	}
	if link := linkHeader(r, page, perPage, total); link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, h.store, 200, items)
}

func (h *restHandler) getPull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	pr := repo.Pulls[n]
	if pr == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	writeJSON(w, h.store, 200, pullJSONObj(pr))
}

func (h *restHandler) pullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	pr := repo.Pulls[n]
	if pr == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	diff, err := repo.PRDiff(pr)
	if err != nil {
		writeErr(w, h.store, 422, err.Error())
		return
	}
	items := make([]any, 0, len(diff))
	for _, f := range diff {
		items = append(items, map[string]any{
			"filename":  f.Path,
			"status":    f.Status,
			"additions": f.Additions,
			"deletions": f.Deletions,
			"changes":   f.Additions + f.Deletions,
		})
	}
	writeJSON(w, h.store, 200, items)
}

func (h *restHandler) mergePull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	// The merge body is optional; read it leniently.
	_ = json.NewDecoder(r.Body).Decode(&struct{}{})
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	pr := repo.Pulls[n]
	if pr == nil {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	sha, err := repo.SquashMerge(h.store, pr)
	if err != nil {
		if err == errConflict {
			writeErr(w, h.store, 405, "Merge conflict")
			return
		}
		writeErr(w, h.store, 405, err.Error())
		return
	}
	writeJSON(w, h.store, 200, map[string]any{
		"sha": sha, "merged": true, "message": "Pull request successfully merged",
	})
}
