// MCP endpoint: JSON-RPC 2.0 over Streamable HTTP (POST /mcp, plain JSON
// responses). Implements the modern era (server/discover) plus the legacy
// initialize handshake as a fallback, and the six tools the suite requires.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// The tools list is pre-built once so tools/list is byte-identical on every
// call, as the suite requires.
var mcpToolsJSON []byte

func init() {
	tools := []map[string]any{
		{
			"name":        "create_repo",
			"description": "Create a new repository owned by the authenticated user.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "The repository name."},
					"description": map[string]any{"type": "string", "description": "A short description of the repository."},
					"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "get_repo",
			"description": "Get a repository by owner and name.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "The repository owner."},
					"repo":  map[string]any{"type": "string", "description": "The repository name."},
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			"name":        "create_issue",
			"description": "Create an issue in a repository.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "The repository owner."},
					"repo":  map[string]any{"type": "string", "description": "The repository name."},
					"title": map[string]any{"type": "string", "description": "The issue title."},
					"body":  map[string]any{"type": "string", "description": "The issue body."},
				},
				"required": []string{"owner", "repo", "title"},
			},
		},
		{
			"name":        "list_issues",
			"description": "List issues in a repository.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "The repository owner."},
					"repo":  map[string]any{"type": "string", "description": "The repository name."},
					"state": map[string]any{"type": "string", "description": "Filter by state: open, closed, or all."},
				},
				"required": []string{"owner", "repo"},
			},
		},
		{
			"name":        "create_pull_request",
			"description": "Create a pull request in a repository.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner": map[string]any{"type": "string", "description": "The repository owner."},
					"repo":  map[string]any{"type": "string", "description": "The repository name."},
					"title": map[string]any{"type": "string", "description": "The pull request title."},
					"body":  map[string]any{"type": "string", "description": "The pull request body."},
					"head":  map[string]any{"type": "string", "description": "The head branch name."},
					"base":  map[string]any{"type": "string", "description": "The base branch name."},
				},
				"required": []string{"owner", "repo", "title", "head", "base"},
			},
		},
		{
			"name":        "merge_pull_request",
			"description": "Merge a pull request with a squash commit.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"owner":       map[string]any{"type": "string", "description": "The repository owner."},
					"repo":        map[string]any{"type": "string", "description": "The repository name."},
					"pull_number": map[string]any{"type": "integer", "description": "The pull request number."},
				},
				"required": []string{"owner", "repo", "pull_number"},
			},
		},
	}
	mcpToolsJSON, _ = json.Marshal(tools)
}

type mcpHandler struct {
	store *Store
}

// jsonRPCRequest is a parsed JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// ServeMCP handles POST /mcp.
func (h *mcpHandler) ServeMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	var body json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		h.writeRPCError(w, nil, -32700, "Parse error", 400)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		h.writeRPCError(w, nil, -32600, "Invalid Request", 400)
		return
	}
	// Notifications carry no id and get no JSON-RPC reply.
	if len(req.ID) == 0 {
		w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
		w.WriteHeader(202)
		return
	}
	switch req.Method {
	case "server/discover":
		h.discover(w, req)
	case "initialize":
		h.initialize(w, req)
	case "tools/list":
		h.toolsList(w, req)
	case "tools/call":
		h.toolsCall(w, r, req)
	case "ping":
		h.writeRPCResult(w, req.ID, map[string]any{})
	default:
		h.writeRPCError(w, req.ID, -32601, "Method not found: "+req.Method, 200)
	}
}

func (h *mcpHandler) writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	resultJSON, _ := json.Marshal(result)
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(id)
	buf.WriteString(`,"result":`)
	buf.Write(resultJSON)
	buf.WriteByte('}')
	writeJSONBytes(w, h.store, 200, buf.Bytes())
}

func (h *mcpHandler) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string, status int) {
	if id == nil {
		id = json.RawMessage("null")
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%s}}`,
		string(id), code, quoteJSON(message))
	writeJSONBytes(w, h.store, status, []byte(body))
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (h *mcpHandler) discover(w http.ResponseWriter, req jsonRPCRequest) {
	serverInfo := map[string]any{"name": "forge", "version": "1.0.0"}
	result := map[string]any{
		"supportedVersions": []string{"2026-07-28"},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"serverInfo":        serverInfo,
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
	}
	h.writeRPCResult(w, req.ID, result)
}

func (h *mcpHandler) initialize(w http.ResponseWriter, req jsonRPCRequest) {
	// Legacy handshake: echo the client's protocol version when it is one we
	// know, else offer the newest legacy revision.
	version := "2025-11-25"
	var params map[string]any
	if len(req.Params) > 0 {
		json.Unmarshal(req.Params, &params)
	}
	if v, ok := params["protocolVersion"].(string); ok {
		switch v {
		case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
			version = v
		}
	}
	result := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
	}
	h.writeRPCResult(w, req.ID, result)
}

func (h *mcpHandler) toolsList(w http.ResponseWriter, req jsonRPCRequest) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":`)
	buf.Write(req.ID)
	buf.WriteString(`,"result":{"tools":`)
	buf.Write(mcpToolsJSON)
	buf.WriteString(`}}`)
	writeJSONBytes(w, h.store, 200, buf.Bytes())
}

// toolParams are the parsed arguments of a tools/call request.
type toolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (h *mcpHandler) toolsCall(w http.ResponseWriter, r *http.Request, req jsonRPCRequest) {
	var params toolParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			h.writeRPCError(w, req.ID, -32602, "Invalid params", 200)
			return
		}
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	known := map[string]bool{
		"create_repo": true, "get_repo": true, "create_issue": true,
		"list_issues": true, "create_pull_request": true, "merge_pull_request": true,
	}
	if !known[params.Name] {
		h.writeRPCError(w, req.ID, -32602, "Unknown tool: "+params.Name, 200)
		return
	}
	result, isErr := h.runTool(params.Name, params.Arguments)
	text, _ := json.Marshal(result)
	resp := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
		"structuredContent": result,
	}
	if isErr {
		resp["isError"] = true
	}
	h.writeRPCResult(w, req.ID, resp)
}

func mcpStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func mcpBool(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func mcpInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

// runTool executes a tool against the store. Returns the result object and
// whether it is an error result.
func (h *mcpHandler) runTool(name string, args map[string]any) (map[string]any, bool) {
	s := h.store
	switch name {
	case "create_repo":
		repoName := mcpStr(args, "name")
		if repoName == "" {
			return map[string]any{"error": "name is required"}, true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.CreateRepo(repoName, mcpBool(args, "private"), mcpStr(args, "description"))
		if repo == nil {
			return map[string]any{"error": "Repository already exists"}, true
		}
		return repoJSON(repo), false
	case "get_repo":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		if owner == "" || repoName == "" {
			return map[string]any{"error": "owner and repo are required"}, true
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		return repoJSON(repo), false
	case "create_issue":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		title := mcpStr(args, "title")
		if owner == "" || repoName == "" || title == "" {
			return map[string]any{"error": "owner, repo and title are required"}, true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		issue, err := repo.CreateIssue(s, title, mcpStr(args, "body"), nil)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		return issueJSONObj(issue), false
	case "list_issues":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		state := mcpStr(args, "state")
		if state == "" {
			state = "open"
		}
		if owner == "" || repoName == "" {
			return map[string]any{"error": "owner and repo are required"}, true
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		issues := []*Issue{}
		for _, issue := range repo.Issues {
			if state != "all" && issue.State != state {
				continue
			}
			issues = append(issues, issue)
		}
		// Creation order ascending.
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
		items := make([]any, 0, len(issues))
		for _, issue := range issues {
			items = append(items, issueJSONObj(issue))
		}
		return map[string]any{"issues": items}, false
	case "create_pull_request":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		title := mcpStr(args, "title")
		head := mcpStr(args, "head")
		base := mcpStr(args, "base")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return map[string]any{"error": "owner, repo, title, head and base are required"}, true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		pr, err := repo.CreatePull(s, title, mcpStr(args, "body"), head, base)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		return pullJSONObj(pr), false
	case "merge_pull_request":
		owner, repoName := mcpStr(args, "owner"), mcpStr(args, "repo")
		num, ok := mcpInt(args, "pull_number")
		if owner == "" || repoName == "" || !ok {
			return map[string]any{"error": "owner, repo and pull_number are required"}, true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return map[string]any{"error": "Repository not found"}, true
		}
		pr := repo.Pulls[num]
		if pr == nil {
			return map[string]any{"error": "Pull request not found"}, true
		}
		sha, err := repo.SquashMerge(s, pr)
		if err != nil {
			return map[string]any{"error": err.Error()}, true
		}
		return map[string]any{"sha": sha, "merged": true, "message": "Pull request successfully merged", "number": num}, false
	}
	return map[string]any{"error": "unknown tool"}, true
}
