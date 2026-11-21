// In-memory store: repositories, git objects and refs, issues, pull
// requests, comments and labels. One process, no disk I/O on any path.
// Deterministic: timestamps come from MAKEFUN_TRIAL_NOW, ordering from
// creation sequence, request ids from an atomic counter.
package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Viewer identity: the fixed scenario's viewer. The suite derives the same
// viewer from the token in every trial.
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

// fallbackTrialNow is used when MAKEFUN_TRIAL_NOW is unset (local runs).
const fallbackTrialNow = "2025-01-01T00:00:00Z"

// Issue is an issue record.
type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []*Label
	CreatedAt string
	UpdatedAt string
}

// Pull is a pull request record.
type Pull struct {
	ID            int64
	Number        int
	Title         string
	Body          string
	State         string // open | closed
	Merged        bool
	MergedAt      string
	MergeCommitSH string
	HeadRef       string
	BaseRef       string
	HeadSHA       string
	BaseSHA       string
	CreatedAt     string
	UpdatedAt     string
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Comment is an issue comment.
type Comment struct {
	ID        int64
	Body      string
	CreatedAt string
	UpdatedAt string
}

// Repo is one repository: git state plus the issue tracker.
type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	Objects map[string]*Object
	Refs    map[string]string // full ref name -> oid
	Issues  map[int]*Issue
	Pulls   map[int]*Pull
	Labels  map[string]*Label
	// Comments keyed by issue number.
	Comments      map[int][]*Comment
	nextNumber    int
	nextCommentID int64

	// Caches, cleared on any mutation of git state. Guarded by cacheMu so
	// readers under the store's read lock can use them safely.
	cacheMu   sync.Mutex
	gen       int64 // incremented on every mutation; guards stale caches
	advV2     []byte
	advV0Up   []byte
	advV0Rc   []byte
	advKey    string // key describing the ref set the ads were built for
	packs     map[string][]byte
	// Issue JSON cache for the read workload.
	issueJSON map[int][]byte
}

// invalidate clears the git-derived caches after a mutation.
func (r *Repo) invalidate() {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.gen++
	r.advV2 = nil
	r.advV0Up = nil
	r.advV0Rc = nil
	r.packs = map[string][]byte{}
}

// RefNames returns the ref names sorted.
func (r *Repo) RefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BranchTip returns the tip oid of a branch, or "".
func (r *Repo) BranchTip(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

// Lookup returns the object for an oid.
func (r *Repo) Lookup(oid string) *Object {
	return r.Objects[oid]
}

// Store is the whole forge state.
type Store struct {
	mu        sync.RWMutex
	repos     map[string]*Repo // key: owner/name
	repoOrder []*Repo          // creation order, for list endpoints
	nextRepo  int64
	nextIssue int64
	token     string
	nowISO    string
	nowUnix   int64
	reqCount  int64
}

// NewStore builds an empty store.
func NewStore(token, nowISO string) *Store {
	nowUnix := int64(1735689600) // 2025-01-01T00:00:00Z
	if nowISO == "" {
		nowISO = fallbackTrialNow
	}
	if t, err := time.Parse(time.RFC3339, nowISO); err == nil {
		nowUnix = t.Unix()
		nowISO = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	return &Store{
		repos:     map[string]*Repo{},
		nextRepo:  5000,
		nextIssue: 9000,
		token:     token,
		nowISO:    nowISO,
		nowUnix:   nowUnix,
	}
}

// NowISO is the deterministic timestamp used for all state.
func (s *Store) NowISO() string { return s.nowISO }

// NowUnix is the deterministic unix time used for commit timestamps.
func (s *Store) NowUnix() int64 { return s.nowUnix }

// NextRequestID returns a deterministic request id.
func (s *Store) NextRequestID() string {
	n := atomic.AddInt64(&s.reqCount, 1)
	return fmt.Sprintf("req-%d", n)
}

// TokenOK reports whether the bearer token matches the shop token. When no
// token is configured (local runs), any non-empty bearer token is accepted.
func (s *Store) TokenOK(token string) bool {
	if token == "" {
		return false
	}
	if s.token == "" {
		return true
	}
	return token == s.token
}

// GetRepo returns the repo for owner/name, or nil. Caller holds the lock.
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// CreateRepo creates a repository owned by the viewer. Returns nil if the
// name is taken for that owner.
func (s *Store) CreateRepo(name string, private bool, description string) *Repo {
	if s.repos[viewerLogin+"/"+name] != nil {
		return nil
	}
	r := &Repo{
		ID:            s.nextRepo,
		Owner:         viewerLogin,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     s.nowISO,
		UpdatedAt:     s.nowISO,
		Objects:       map[string]*Object{},
		Refs:          map[string]string{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		Labels:        map[string]*Label{},
		Comments:      map[int][]*Comment{},
		nextNumber:    1,
		packs:         map[string][]byte{},
		issueJSON:     map[int][]byte{},
	}
	s.nextRepo++
	s.repos[viewerLogin+"/"+name] = r
	s.repoOrder = append(s.repoOrder, r)
	return r
}

// DeleteRepo removes a repository. Returns false when absent.
func (s *Store) DeleteRepo(owner, name string) bool {
	key := owner + "/" + name
	r := s.repos[key]
	if r == nil {
		return false
	}
	delete(s.repos, key)
	for i, other := range s.repoOrder {
		if other == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// ListRepos returns the viewer's repositories in creation order.
func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(s.repoOrder))
	for _, r := range s.repoOrder {
		if r.Owner == viewerLogin {
			out = append(out, r)
		}
	}
	return out
}

// CreateIssue creates an issue, validating labels. Unknown labels are an
// error (GitHub returns 422).
func (r *Repo) CreateIssue(s *Store, title, body string, labelNames []string) (*Issue, error) {
	labels := []*Label{}
	for _, name := range labelNames {
		l := r.Labels[name]
		if l == nil {
			return nil, fmt.Errorf("label %q does not exist", name)
		}
		labels = append(labels, l)
	}
	issue := &Issue{
		ID:        s.nextIssue,
		Number:    r.nextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedAt: s.nowISO,
		UpdatedAt: s.nowISO,
	}
	s.nextIssue++
	r.nextNumber++
	r.Issues[issue.Number] = issue
	return issue, nil
}

// CreatePull creates a pull request. head/base are branch names.
func (r *Repo) CreatePull(s *Store, title, body, head, base string) (*Pull, error) {
	headSHA := r.BranchTip(head)
	if headSHA == "" {
		return nil, fmt.Errorf("head branch %q not found", head)
	}
	baseSHA := r.BranchTip(base)
	if baseSHA == "" {
		return nil, fmt.Errorf("base branch %q not found", base)
	}
	pr := &Pull{
		ID:        s.nextIssue,
		Number:    r.nextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		BaseRef:   base,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		CreatedAt: s.nowISO,
		UpdatedAt: s.nowISO,
	}
	s.nextIssue++
	r.nextNumber++
	r.Pulls[pr.Number] = pr
	return pr, nil
}

// ---------------------------------------------------------------------------
// global ids (GraphQL Relay style, opaque and stable)

func encodeGID(kind, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + payload))
}

func decodeGID(id string) (kind, payload string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		// Tolerate standard base64 with padding too.
		raw, err = base64.StdEncoding.DecodeString(id)
		if err != nil {
			return "", "", false
		}
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func repoGID(owner, name string) string    { return encodeGID("repo", owner+"/"+name) }
func issueGID(owner, name string, n int) string {
	return encodeGID("issue", fmt.Sprintf("%s/%s:%d", owner, name, n))
}
func pullGID(owner, name string, n int) string {
	return encodeGID("pr", fmt.Sprintf("%s/%s:%d", owner, name, n))
}
func commentGID(owner, name string, issueN int, id int64) string {
	return encodeGID("comment", fmt.Sprintf("%s/%s:%d:%d", owner, name, issueN, id))
}

func encodeCursor(n int) string { return encodeGID("cursor", fmt.Sprintf("%d", n)) }

func decodeCursor(c string) (int, bool) {
	kind, payload, ok := decodeGID(c)
	if !ok || kind != "cursor" {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(payload, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// tree helpers: path lookups, updates, diffs

// flattenTree returns path -> blob oid for every file under a tree.
func flattenTree(root string, lookup Lookup) (map[string]string, error) {
	out := map[string]string{}
	var walk func(treeOID, prefix string) error
	walk = func(treeOID, prefix string) error {
		obj := lookup(treeOID)
		if obj == nil {
			return fmt.Errorf("missing tree %s", treeOID)
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			path := prefix + e.Name
			if e.Mode == "40000" {
				if err := walk(e.OID, path+"/"); err != nil {
					return err
				}
			} else {
				out[path] = e.OID
			}
		}
		return nil
	}
	if root != "" && root != zeroOID {
		if err := walk(root, ""); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// commitTree returns the tree oid of a commit, or "".
func commitTree(oid string, lookup Lookup) string {
	if oid == "" || oid == zeroOID {
		return ""
	}
	obj := lookup(oid)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	c, err := ParseCommit(obj.Data)
	if err != nil {
		return ""
	}
	return c.Tree
}

// FileAt returns the blob oid and bytes for a path at a commit.
func (r *Repo) FileAt(commitOID, path string) (string, []byte, bool) {
	treeOID := commitTree(commitOID, r.Lookup)
	if treeOID == "" {
		return "", nil, false
	}
	flat, err := flattenTree(treeOID, r.Lookup)
	if err != nil {
		return "", nil, false
	}
	blobOID, ok := flat[path]
	if !ok {
		return "", nil, false
	}
	obj := r.Objects[blobOID]
	if obj == nil || obj.Type != "blob" {
		return "", nil, false
	}
	return blobOID, obj.Data, true
}

// ListDir lists the entries directly under a path at a commit. An empty path
// lists the root. Returns nil when the path is not a directory.
func (r *Repo) ListDir(commitOID, path string) []TreeEntry {
	treeOID := commitTree(commitOID, r.Lookup)
	if treeOID == "" {
		return nil
	}
	if path != "" {
		// Find the subtree for the path by walking the tree structure.
		parts := strings.Split(path, "/")
		cur := treeOID
		for _, part := range parts {
			obj := r.Objects[cur]
			if obj == nil {
				return nil
			}
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil
			}
			found := ""
			for _, e := range entries {
				if e.Name == part && e.Mode == "40000" {
					found = e.OID
					break
				}
			}
			if found == "" {
				return nil
			}
			cur = found
		}
		treeOID = cur
	}
	obj := r.Objects[treeOID]
	if obj == nil {
		return nil
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil
	}
	return entries
}

// setPathInTree returns the oid of a new tree equal to treeOID but with path
// set to blobOID (or removed when blobOID is ""). New objects are written
// into the repo's object map.
func (r *Repo) setPathInTree(treeOID, path, blobOID string) (string, error) {
	parts := strings.Split(path, "/")
	var build func(treeOID string, depth int) (string, error)
	build = func(treeOID string, depth int) (string, error) {
		var entries []TreeEntry
		if treeOID != "" && treeOID != zeroOID {
			obj := r.Objects[treeOID]
			if obj == nil {
				return "", fmt.Errorf("missing tree %s", treeOID)
			}
			var err error
			entries, err = ParseTree(obj.Data)
			if err != nil {
				return "", err
			}
		}
		name := parts[depth]
		last := depth == len(parts)-1
		// Find the existing entry for name.
		idx := -1
		for i, e := range entries {
			if e.Name == name {
				idx = i
				break
			}
		}
		if last {
			if blobOID == "" {
				// Remove.
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else {
				if idx >= 0 {
					entries[idx].Mode = "100644"
					entries[idx].OID = blobOID
				} else {
					entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
				}
			}
		} else {
			var subOID string
			if idx >= 0 {
				if entries[idx].Mode != "40000" {
					// A file blocks the path; replace it with a directory.
					subOID = ""
					entries = append(entries[:idx], entries[idx+1:]...)
					idx = -1
				} else {
					subOID = entries[idx].OID
				}
			}
			newSub, err := build(subOID, depth+1)
			if err != nil {
				return "", err
			}
			if newSub == "" {
				// Subdirectory became empty: drop it.
				if idx >= 0 {
					entries = append(entries[:idx], entries[idx+1:]...)
				}
			} else {
				if idx >= 0 {
					entries[idx].OID = newSub
				} else {
					entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: newSub})
				}
			}
		}
		if len(entries) == 0 {
			return "", nil // empty tree sentinel: caller drops the dir
		}
		data := BuildTree(entries)
		oid := HashObject("tree", data)
		if r.Objects[oid] == nil {
			r.Objects[oid] = &Object{Type: "tree", Data: data}
		}
		return oid, nil
	}
	oid, err := build(treeOID, 0)
	if err != nil {
		return "", err
	}
	if oid == "" {
		// The root became empty: use the canonical empty tree.
		empty := HashObject("tree", []byte{})
		if r.Objects[empty] == nil {
			r.Objects[empty] = &Object{Type: "tree", Data: []byte{}}
		}
		return empty, nil
	}
	return oid, nil
}

// CommitFile writes a blob at path on branch, creating a commit. When
// blobOID is "" the file is removed. Returns the new commit oid.
func (r *Repo) CommitFile(s *Store, branch, path string, content []byte, remove bool, message string) (string, error) {
	tip := r.BranchTip(branch)
	var parents []string
	var baseTree string
	if tip != "" {
		parents = []string{tip}
		baseTree = commitTree(tip, r.Lookup)
	}
	var blobOID string
	if !remove {
		blobOID = HashObject("blob", content)
		if r.Objects[blobOID] == nil {
			r.Objects[blobOID] = &Object{Type: "blob", Data: append([]byte(nil), content...)}
		}
	}
	newTree, err := r.setPathInTree(baseTree, path, blobOID)
	if err != nil {
		return "", err
	}
	author := viewerName + " <" + viewerEmail + ">"
	commitData := BuildCommit(newTree, parents, author, author, s.nowUnix, message+"\n")
	commitOID := HashObject("commit", commitData)
	if r.Objects[commitOID] == nil {
		r.Objects[commitOID] = &Object{Type: "commit", Data: commitData}
	}
	r.Refs["refs/heads/"+branch] = commitOID
	r.invalidate()
	return commitOID, nil
}

// DiffFile describes one changed file between two trees.
type DiffFile struct {
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

// DiffTrees diffs two trees (by oid) and returns the changed files.
func DiffTrees(a, b string, lookup Lookup) ([]DiffFile, error) {
	flatA, err := flattenTree(a, lookup)
	if err != nil {
		return nil, err
	}
	flatB, err := flattenTree(b, lookup)
	if err != nil {
		return nil, err
	}
	countLines := func(oid string) int {
		obj := lookup(oid)
		if obj == nil {
			return 0
		}
		return len(strings.Split(string(obj.Data), "\n"))
	}
	paths := map[string]bool{}
	for p := range flatA {
		paths[p] = true
	}
	for p := range flatB {
		paths[p] = true
	}
	var out []DiffFile
	for p := range paths {
		oidA, inA := flatA[p]
		oidB, inB := flatB[p]
		switch {
		case inA && !inB:
			out = append(out, DiffFile{Path: p, Status: "removed", Additions: 0, Deletions: countLines(oidA)})
		case !inA && inB:
			out = append(out, DiffFile{Path: p, Status: "added", Additions: countLines(oidB), Deletions: 0})
		case oidA != oidB:
			// Crude but honest line counts: additions = lines in the new
			// blob, deletions = lines in the old blob.
			out = append(out, DiffFile{Path: p, Status: "modified", Additions: countLines(oidB), Deletions: countLines(oidA)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// liveTips returns the pull request's head and base tips as they are now,
// falling back to the snapshot taken at creation when a branch is gone.
func (r *Repo) liveTips(pr *Pull) (headSHA, baseSHA string) {
	headSHA = r.BranchTip(pr.HeadRef)
	if headSHA == "" {
		headSHA = pr.HeadSHA
	}
	baseSHA = r.BranchTip(pr.BaseRef)
	if baseSHA == "" {
		baseSHA = pr.BaseSHA
	}
	return headSHA, baseSHA
}

// PRDiff returns the files changed by a pull request (merge-base vs head).
func (r *Repo) PRDiff(pr *Pull) ([]DiffFile, error) {
	headSHA, baseSHA := r.liveTips(pr)
	base := MergeBase(baseSHA, headSHA, r.Lookup)
	if base == "" {
		base = baseSHA
	}
	return DiffTrees(commitTree(base, r.Lookup), commitTree(headSHA, r.Lookup), r.Lookup)
}

// PRConflicted reports whether the PR conflicts with its base branch: a file
// the PR changed whose base-tip version differs from BOTH the merge-base
// version and the head version. The base tip is the branch's tip NOW, not
// the snapshot from when the PR was created: a sibling PR merged first
// advances the base and can create the conflict.
func (r *Repo) PRConflicted(pr *Pull) bool {
	headSHA, baseSHA := r.liveTips(pr)
	base := MergeBase(baseSHA, headSHA, r.Lookup)
	if base == "" {
		return false
	}
	flatBase, err := flattenTree(commitTree(base, r.Lookup), r.Lookup)
	if err != nil {
		return false
	}
	flatHead, err := flattenTree(commitTree(headSHA, r.Lookup), r.Lookup)
	if err != nil {
		return false
	}
	flatTip, err := flattenTree(commitTree(baseSHA, r.Lookup), r.Lookup)
	if err != nil {
		return false
	}
	for p, headOID := range flatHead {
		baseOID, inBase := flatBase[p]
		if inBase && baseOID == headOID {
			continue // unchanged by the PR
		}
		tipOID, inTip := flatTip[p]
		if inTip && tipOID == headOID {
			continue // base already matches head
		}
		if inBase && inTip && baseOID == tipOID {
			continue // base untouched since merge-base
		}
		if !inBase && !inTip {
			continue // added by the PR, absent from base
		}
		return true
	}
	for p := range flatBase {
		if _, inHead := flatHead[p]; !inHead {
			// Deleted by the PR; conflict only if base changed it too.
			if tipOID, inTip := flatTip[p]; inTip && tipOID != flatBase[p] {
				return true
			}
		}
	}
	return false
}

// SquashMerge merges a pull request with a squash commit onto the base
// branch. Returns the merge commit oid, or an error when conflicted.
func (r *Repo) SquashMerge(s *Store, pr *Pull) (string, error) {
	if pr.State != "open" {
		return "", fmt.Errorf("pull request is not open")
	}
	if r.PRConflicted(pr) {
		return "", errConflict
	}
	headSHA, baseSHA := r.liveTips(pr)
	headTree := commitTree(headSHA, r.Lookup)
	author := viewerName + " <" + viewerEmail + ">"
	msg := pr.Title + "\n\nMerge pull request #" + fmt.Sprint(pr.Number) + "\n"
	commitData := BuildCommit(headTree, []string{baseSHA}, author, author, s.nowUnix, msg)
	oid := HashObject("commit", commitData)
	if r.Objects[oid] == nil {
		r.Objects[oid] = &Object{Type: "commit", Data: commitData}
	}
	r.Refs["refs/heads/"+pr.BaseRef] = oid
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = s.nowISO
	pr.MergeCommitSH = oid
	pr.HeadSHA = headSHA
	pr.BaseSHA = oid
	pr.UpdatedAt = s.nowISO
	r.invalidate()
	return oid, nil
}

var errConflict = fmt.Errorf("merge conflict")
