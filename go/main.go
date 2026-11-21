// forge: a GitHub-compatible git forge in one static Go binary.
// Binds an ephemeral port, prints FORGE_PORT=<n> on stdout, and serves
// REST v3, GraphQL, MCP and git smart HTTP until killed.
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	nowISO := os.Getenv("MAKEFUN_TRIAL_NOW")
	store := NewStore(token, nowISO)
	srv := &server{
		store: store,
		rest:  &restHandler{store: store},
		gql:   &gqlHandler{store: store},
		mcp:   &mcpHandler{store: store},
		git:   &gitHandler{store: store},
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: listen failed: %v\n", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Printf("FORGE_PORT=%d\n", addr.Port)
	httpServer := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "forge: serve failed: %v\n", err)
		os.Exit(1)
	}
}

type server struct {
	store *Store
	rest  *restHandler
	gql   *gqlHandler
	mcp   *mcpHandler
	git   *gitHandler
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The boot check calls /healthz without a token.
	if r.URL.Path == "/healthz" {
		writeJSON(w, s.store, 200, map[string]any{"ok": true})
		return
	}
	if !s.store.TokenOK(bearerToken(r)) {
		writeErr(w, s.store, 401, "Requires authentication")
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		s.rest.ServeREST(w, r)
	case path == "/api/graphql":
		s.gql.ServeGraphQL(w, r)
	case path == "/mcp":
		s.mcp.ServeMCP(w, r)
	default:
		s.git.ServeGit(w, r)
	}
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// ---------------------------------------------------------------------------
// git smart HTTP routing

type gitHandler struct {
	store *Store
}

func (h *gitHandler) ServeGit(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	// Expected: {owner}/{repo}.git/{info/refs|git-upload-pack|git-receive-pack}
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	rest := parts[2:]
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
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" && r.Method == http.MethodGet {
		h.infoRefs(w, r, repo)
		return
	}
	if len(rest) == 1 && rest[0] == "git-upload-pack" && r.Method == http.MethodPost {
		h.uploadPack(w, r, repo)
		return
	}
	if len(rest) == 1 && rest[0] == "git-receive-pack" && r.Method == http.MethodPost {
		h.receivePack(w, r, repo)
		return
	}
	writeErr(w, h.store, 404, "Not Found")
}

func (h *gitHandler) infoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	switch service {
	case "git-upload-pack":
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
		w.WriteHeader(200)
		if r.Header.Get("Git-Protocol") == "version=2" {
			w.Write(repo.advertiseV2())
		} else {
			w.Write(repo.advertiseV0("git-upload-pack"))
		}
	case "git-receive-pack":
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
		w.WriteHeader(200)
		w.Write(repo.advertiseV0("git-receive-pack"))
	default:
		writeErr(w, h.store, 400, "Unknown service")
	}
}

func (h *gitHandler) uploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, h.store, 400, "Could not read request body")
		return
	}
	// Parse the protocol v2 command: command=<name>, optional agent and
	// object-format lines, delim, then arguments until flush.
	pr := &pktReader{data: body}
	first, kind, err := pr.next()
	if err != nil || kind != "data" {
		writeErr(w, h.store, 400, "Bad pkt-line")
		return
	}
	command := strings.TrimSuffix(string(first), "\n")
	if !strings.HasPrefix(command, "command=") {
		writeErr(w, h.store, 400, "Expected a protocol v2 command")
		return
	}
	command = strings.TrimPrefix(command, "command=")
	// Skip lines until the delim.
	for {
		_, kind, err := pr.next()
		if err != nil {
			writeErr(w, h.store, 400, "Bad pkt-line")
			return
		}
		if kind == "delim" {
			break
		}
		if kind == "flush" {
			break
		}
	}
	var args []string
	for {
		payload, kind, err := pr.next()
		if err != nil {
			writeErr(w, h.store, 400, "Bad pkt-line")
			return
		}
		if kind == "flush" {
			break
		}
		if kind == "data" {
			args = append(args, strings.TrimSuffix(string(payload), "\n"))
		}
	}
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	var resp []byte
	switch command {
	case "ls-refs":
		resp = repo.handleLsRefs(args)
	case "fetch":
		resp = repo.handleFetch(parseFetchRequest(args))
	default:
		writeErr(w, h.store, 400, "Unknown command: "+command)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
	w.WriteHeader(200)
	w.Write(resp)
}

func (h *gitHandler) receivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, h.store, 400, "Could not read request body")
		return
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	resp := repo.handleReceivePack(body)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("X-GitHub-Request-Id", h.store.NextRequestID())
	w.WriteHeader(200)
	w.Write(resp)
}
