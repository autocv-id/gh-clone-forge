// GraphQL endpoint: a small parser and executor for the fixed set of
// operation documents the suite sends (queries, mutations, introspection,
// batched arrays). Field names are validated against a minimal schema so an
// unknown field produces a GraphQL error naming it.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// parser

type gqlValue struct {
	kind    string // var | string | int | float | bool | null | enum | list | object
	str     string
	num     int64
	fnum    float64
	boolean bool
	list    []*gqlValue
	obj     map[string]*gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []*gqlSelection
}

type gqlSelection struct {
	field    *gqlField
	fragType string // "" for a plain field; type name for an inline fragment
	sel      []*gqlSelection
}

type gqlOperation struct {
	opType  string // query | mutation
	name    string
	varDefs map[string]*gqlValue // default values by variable name
	sel     []*gqlSelection
}

type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) skipWS() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
		} else if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		} else {
			break
		}
	}
}

func (p *gqlParser) peek() byte {
	p.skipWS()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) expect(c byte) error {
	p.skipWS()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return fmt.Errorf("expected %q at %d", c, p.pos)
	}
	p.pos++
	return nil
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c >= '0' && c <= '9'
}

func (p *gqlParser) readName() (string, error) {
	p.skipWS()
	if p.pos >= len(p.src) || !isNameStart(p.src[p.pos]) {
		return "", fmt.Errorf("expected a name at %d", p.pos)
	}
	start := p.pos
	for p.pos < len(p.src) && isNameChar(p.src[p.pos]) {
		p.pos++
	}
	return p.src[start:p.pos], nil
}

func (p *gqlParser) readString() (string, error) {
	// p.pos is at the opening quote.
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		p.pos++
		if c == '"' {
			return sb.String(), nil
		}
		if c == '\\' && p.pos < len(p.src) {
			esc := p.src[p.pos]
			p.pos++
			switch esc {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'u':
				if p.pos+4 <= len(p.src) {
					if n, err := strconv.ParseInt(p.src[p.pos:p.pos+4], 16, 32); err == nil {
						sb.WriteRune(rune(n))
						p.pos += 4
					}
				}
			default:
				sb.WriteByte(esc)
			}
			continue
		}
		sb.WriteByte(c)
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *gqlParser) readNumber() (*gqlValue, error) {
	p.skipWS()
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	isFloat := false
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= '0' && c <= '9' {
			p.pos++
		} else if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			isFloat = true
			p.pos++
		} else {
			break
		}
	}
	text := p.src[start:p.pos]
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "float", fnum: f}, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, err
	}
	return &gqlValue{kind: "int", num: n}, nil
}

func (p *gqlParser) readValue() (*gqlValue, error) {
	p.skipWS()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("expected a value")
	}
	switch c := p.src[p.pos]; {
	case c == '$':
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", str: name}, nil
	case c == '"':
		s, err := p.readString()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "string", str: s}, nil
	case c == '[':
		p.pos++
		v := &gqlValue{kind: "list"}
		for p.peek() != ']' {
			item, err := p.readValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, item)
		}
		p.pos++
		return v, nil
	case c == '{':
		p.pos++
		v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for p.peek() != '}' {
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			val, err := p.readValue()
			if err != nil {
				return nil, err
			}
			v.obj[name] = val
		}
		p.pos++
		return v, nil
	case c == '-' || c >= '0' && c <= '9':
		return p.readNumber()
	default:
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gqlValue{kind: "bool", boolean: true}, nil
		case "false":
			return &gqlValue{kind: "bool", boolean: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: name}, nil
		}
	}
}

// readType consumes a GraphQL type reference (String!, [Int], ...).
func (p *gqlParser) readType() error {
	p.skipWS()
	if p.peek() == '[' {
		p.pos++
		if err := p.readType(); err != nil {
			return err
		}
		if err := p.expect(']'); err != nil {
			return err
		}
	} else {
		if _, err := p.readName(); err != nil {
			return err
		}
	}
	if p.peek() == '!' {
		p.pos++
	}
	return nil
}

func (p *gqlParser) readArguments() (map[string]*gqlValue, error) {
	args := map[string]*gqlValue{}
	if err := p.expect('('); err != nil {
		return nil, err
	}
	for p.peek() != ')' {
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		val, err := p.readValue()
		if err != nil {
			return nil, err
		}
		args[name] = val
	}
	p.pos++
	return args, nil
}

func (p *gqlParser) readSelectionSet() ([]*gqlSelection, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var sel []*gqlSelection
	for p.peek() != '}' {
		if p.pos+3 <= len(p.src) && p.src[p.pos:p.pos+3] == "..." {
			p.pos += 3
			// Inline fragment: ... on Type { ... }
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			if name != "on" {
				return nil, fmt.Errorf("fragment spreads are not supported")
			}
			typeName, err := p.readName()
			if err != nil {
				return nil, err
			}
			sub, err := p.readSelectionSet()
			if err != nil {
				return nil, err
			}
			sel = append(sel, &gqlSelection{fragType: typeName, sel: sub})
			continue
		}
		// Field, possibly aliased.
		first, err := p.readName()
		if err != nil {
			return nil, err
		}
		f := &gqlField{name: first, alias: first, args: map[string]*gqlValue{}}
		if p.peek() == ':' {
			p.pos++
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			f.alias = first
			f.name = name
		}
		if p.peek() == '(' {
			args, err := p.readArguments()
			if err != nil {
				return nil, err
			}
			f.args = args
		}
		// Directives (@skip/@include) are not used by the suite; ignore any.
		for p.peek() == '@' {
			p.pos++
			if _, err := p.readName(); err != nil {
				return nil, err
			}
			if p.peek() == '(' {
				if _, err := p.readArguments(); err != nil {
					return nil, err
				}
			}
		}
		if p.peek() == '{' {
			sub, err := p.readSelectionSet()
			if err != nil {
				return nil, err
			}
			f.sel = sub
		}
		sel = append(sel, &gqlSelection{field: f})
	}
	p.pos++
	return sel, nil
}

// parseDocument parses a GraphQL document into its operations.
func parseDocument(src string) ([]*gqlOperation, error) {
	p := &gqlParser{src: src}
	var ops []*gqlOperation
	for {
		if p.peek() == 0 {
			break
		}
		op := &gqlOperation{opType: "query", varDefs: map[string]*gqlValue{}}
		if p.peek() == '{' {
			sel, err := p.readSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sel = sel
			ops = append(ops, op)
			continue
		}
		kw, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch kw {
		case "query", "mutation":
			op.opType = kw
		case "fragment":
			return nil, fmt.Errorf("fragment definitions are not supported")
		default:
			return nil, fmt.Errorf("unexpected keyword %q", kw)
		}
		if isNameStart(p.peek()) {
			name, err := p.readName()
			if err != nil {
				return nil, err
			}
			op.name = name
		}
		if p.peek() == '(' {
			p.pos++
			for p.peek() != ')' {
				if err := p.expect('$'); err != nil {
					return nil, err
				}
				varName, err := p.readName()
				if err != nil {
					return nil, err
				}
				if err := p.expect(':'); err != nil {
					return nil, err
				}
				if err := p.readType(); err != nil {
					return nil, err
				}
				if p.peek() == '=' {
					p.pos++
					def, err := p.readValue()
					if err != nil {
						return nil, err
					}
					op.varDefs[varName] = def
				}
			}
			p.pos++
		}
		sel, err := p.readSelectionSet()
		if err != nil {
			return nil, err
		}
		op.sel = sel
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return ops, nil
}

// ---------------------------------------------------------------------------
// values and variables

// resolveValue converts a parsed literal (possibly referencing a variable)
// into a Go value.
func resolveValue(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		if val, ok := vars[v.str]; ok {
			return val
		}
		return nil
	case "string", "enum":
		return v.str
	case "int":
		return v.num
	case "float":
		return v.fnum
	case "bool":
		return v.boolean
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = resolveValue(item, vars)
		}
		return out
	}
	return nil
}

func resolveArgs(f *gqlField, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range f.args {
		out[k] = resolveValue(v, vars)
	}
	return out
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case int:
		return v
	}
	return def
}

func argMap(args map[string]any, key string) map[string]any {
	if v, ok := args[key].(map[string]any); ok {
		return v
	}
	return nil
}

func argList(args map[string]any, key string) []any {
	if v, ok := args[key].([]any); ok {
		return v
	}
	return nil
}

// ---------------------------------------------------------------------------
// schema: allowed fields per type (for validation)

var gqlSchema = map[string]map[string]bool{
	"Query": {
		"viewer": true, "repository": true, "node": true, "__type": true,
	},
	"Mutation": {
		"createIssue": true, "closeIssue": true, "createPullRequest": true,
		"mergePullRequest": true, "addComment": true,
	},
	"User": {"login": true, "id": true, "name": true},
	"Repository": {
		"id": true, "databaseId": true, "name": true, "nameWithOwner": true,
		"owner": true, "defaultBranchRef": true, "viewerPermission": true,
		"hasIssuesEnabled": true, "issue": true, "issues": true,
		"pullRequests": true, "pullRequest": true,
	},
	"Ref":      {"name": true},
	"Issue":    {"id": true, "number": true, "title": true, "state": true, "body": true},
	"IssueConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PageInfo": {"hasNextPage": true, "endCursor": true},
	"PullRequest": {
		"id": true, "number": true, "title": true, "body": true, "state": true,
		"merged": true, "headRefName": true, "baseRefName": true,
		"mergeable": true, "files": true,
	},
	"PullRequestConnection": {"nodes": true},
	"PullRequestChangedFileConnection": {"nodes": true},
	"PullRequestChangedFile": {"path": true},
	"__Type": {"name": true, "kind": true, "fields": true, "enumValues": true, "description": true},
	"__Field": {"name": true},
	"__EnumValue": {"name": true},
	"CreateIssuePayload": {"issue": true},
	"CloseIssuePayload":  {"issue": true},
	"CreatePullRequestPayload": {"pullRequest": true},
	"MergePullRequestPayload":  {"pullRequest": true},
	"AddCommentPayload":        {"commentEdge": true},
	"IssueCommentEdge":         {"node": true},
	"IssueComment":             {"id": true, "body": true},
}

// Introspection data: the field and enum-value names the suite asserts.
var gqlIntrospection = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "createdAt", "updatedAt"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "headRefName", "baseRefName", "mergeable", "files"},
	"Repository": {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
}

var gqlEnumValues = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER", "CODE", "DISCUSSION", "COMMIT"},
}

// ---------------------------------------------------------------------------
// executor

type gqlExecutor struct {
	store  *Store
	vars   map[string]any
	errors []map[string]any
}

type gqlNode interface {
	TypeName() string
	Field(name string, args map[string]any, e *gqlExecutor) (any, error)
}

// execute runs one operation and returns the data object.
func (e *gqlExecutor) execute(op *gqlOperation) map[string]any {
	rootType := "Query"
	if op.opType == "mutation" {
		rootType = "Mutation"
	}
	data, _ := e.executeSelectionSet(op.sel, rootType, &rootNode{e: e})
	return data
}

// executeSelectionSet resolves a selection set against a node.
func (e *gqlExecutor) executeSelectionSet(sel []*gqlSelection, typeName string, node gqlNode) (map[string]any, bool) {
	out := map[string]any{}
	for _, s := range sel {
		if s.field == nil {
			// Inline fragment: apply only when the type matches.
			if s.fragType == typeName {
				sub, ok := e.executeSelectionSet(s.sel, typeName, node)
				if !ok {
					return nil, false
				}
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		f := s.field
		if !gqlSchema[typeName][f.name] {
			e.errors = append(e.errors, map[string]any{
				"message": fmt.Sprintf("Cannot query field %q on type %q.", f.name, typeName),
			})
			return nil, false
		}
		args := resolveArgs(f, e.vars)
		val, err := node.Field(f.name, args, e)
		if err != nil {
			e.errors = append(e.errors, map[string]any{"message": err.Error()})
			return nil, false
		}
		out[f.alias] = e.materialize(val, f.sel)
	}
	return out, true
}

// materialize applies a field's sub-selections to its value.
func (e *gqlExecutor) materialize(val any, sel []*gqlSelection) any {
	switch v := val.(type) {
	case nil:
		return nil
	case gqlNode:
		if len(sel) == 0 {
			return nil
		}
		sub, ok := e.executeSelectionSet(sel, v.TypeName(), v)
		if !ok {
			return nil
		}
		return sub
	case []gqlNode:
		out := make([]any, 0, len(v))
		for _, node := range v {
			out = append(out, e.materialize(node, sel))
		}
		return out
	default:
		return val
	}
}

// ---------------------------------------------------------------------------
// node types

type rootNode struct{ e *gqlExecutor }

func (n *rootNode) TypeName() string {
	return "Query"
}

func (n *rootNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	s := e.store
	switch name {
	case "viewer":
		return &userNode{}, nil
	case "repository":
		owner := argString(args, "owner")
		repoName := argString(args, "name")
		if owner != viewerLogin {
			return nil, nil
		}
		repo := s.GetRepo(owner, repoName)
		if repo == nil {
			return nil, nil
		}
		return &repoNode{repo: repo}, nil
	case "node":
		id := argString(args, "id")
		kind, payload, ok := decodeGID(id)
		if !ok {
			return nil, nil
		}
		if kind == "repo" {
			parts := strings.SplitN(payload, "/", 2)
			if len(parts) != 2 {
				return nil, nil
			}
			repo := s.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, nil
			}
			return &repoNode{repo: repo}, nil
		}
		return nil, nil
	case "__type":
		typeName := argString(args, "name")
		return &typeNode{name: typeName}, nil
	}
	return nil, fmt.Errorf("unknown root field %q", name)
}

type mutationNode struct{ e *gqlExecutor }

func (n *mutationNode) TypeName() string { return "Mutation" }

func (n *mutationNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	s := e.store
	input := argMap(args, "input")
	if input == nil {
		return nil, fmt.Errorf("input is required")
	}
	switch name {
	case "createIssue":
		repoID := argString(input, "repositoryId")
		kind, payload, ok := decodeGID(repoID)
		if !ok || kind != "repo" {
			return nil, fmt.Errorf("unknown repository id")
		}
		parts := strings.SplitN(payload, "/", 2)
		repo := s.GetRepo(parts[0], parts[1])
		if repo == nil {
			return nil, fmt.Errorf("unknown repository id")
		}
		issue, err := repo.CreateIssue(s, argString(input, "title"), argString(input, "body"), nil)
		if err != nil {
			return nil, err
		}
		return &payloadNode{typ: "CreateIssuePayload", issue: issue, repo: repo}, nil
	case "closeIssue":
		issueID := argString(input, "issueId")
		kind, payload, ok := decodeGID(issueID)
		if !ok || kind != "issue" {
			return nil, fmt.Errorf("unknown issue id")
		}
		repo, num := parseIssueGIDPayload(s, payload)
		if repo == nil {
			return nil, fmt.Errorf("unknown issue id")
		}
		issue := repo.Issues[num]
		if issue == nil {
			return nil, fmt.Errorf("unknown issue id")
		}
		issue.State = "closed"
		issue.UpdatedAt = s.nowISO
		delete(repo.issueJSON, num)
		return &payloadNode{typ: "CloseIssuePayload", issue: issue, repo: repo}, nil
	case "createPullRequest":
		repoID := argString(input, "repositoryId")
		kind, payload, ok := decodeGID(repoID)
		if !ok || kind != "repo" {
			return nil, fmt.Errorf("unknown repository id")
		}
		parts := strings.SplitN(payload, "/", 2)
		repo := s.GetRepo(parts[0], parts[1])
		if repo == nil {
			return nil, fmt.Errorf("unknown repository id")
		}
		pr, err := repo.CreatePull(s, argString(input, "title"), argString(input, "body"), argString(input, "headRefName"), argString(input, "baseRefName"))
		if err != nil {
			return nil, err
		}
		return &payloadNode{typ: "CreatePullRequestPayload", pull: pr, repo: repo}, nil
	case "mergePullRequest":
		prID := argString(input, "pullRequestId")
		kind, payload, ok := decodeGID(prID)
		if !ok || kind != "pr" {
			return nil, fmt.Errorf("unknown pull request id")
		}
		repo, num := parseIssueGIDPayload(s, payload)
		if repo == nil {
			return nil, fmt.Errorf("unknown pull request id")
		}
		pr := repo.Pulls[num]
		if pr == nil {
			return nil, fmt.Errorf("unknown pull request id")
		}
		if _, err := repo.SquashMerge(s, pr); err != nil {
			return nil, err
		}
		return &payloadNode{typ: "MergePullRequestPayload", pull: pr, repo: repo}, nil
	case "addComment":
		subjectID := argString(input, "subjectId")
		body := argString(input, "body")
		kind, payload, ok := decodeGID(subjectID)
		if !ok || (kind != "issue" && kind != "pr") {
			return nil, fmt.Errorf("unknown subject id")
		}
		repo, num := parseIssueGIDPayload(s, payload)
		if repo == nil {
			return nil, fmt.Errorf("unknown subject id")
		}
		if repo.Issues[num] == nil && repo.Pulls[num] == nil {
			return nil, fmt.Errorf("unknown subject id")
		}
		repo.nextCommentID++
		c := &Comment{ID: repo.nextCommentID, Body: body, CreatedAt: s.nowISO, UpdatedAt: s.nowISO}
		repo.Comments[num] = append(repo.Comments[num], c)
		return &payloadNode{typ: "AddCommentPayload", comment: c, repo: repo, subjectNum: num}, nil
	}
	return nil, fmt.Errorf("unknown mutation %q", name)
}

// parseIssueGIDPayload decodes the "owner/name:number" payload of an issue
// or pull-request global id.
func parseIssueGIDPayload(s *Store, payload string) (*Repo, int) {
	i := strings.LastIndex(payload, ":")
	if i < 0 {
		return nil, 0
	}
	var num int
	if _, err := fmt.Sscanf(payload[i+1:], "%d", &num); err != nil {
		return nil, 0
	}
	parts := strings.SplitN(payload[:i], "/", 2)
	if len(parts) != 2 {
		return nil, 0
	}
	return s.GetRepo(parts[0], parts[1]), num
}

type payloadNode struct {
	typ        string
	issue      *Issue
	pull       *Pull
	comment    *Comment
	repo       *Repo
	subjectNum int
}

func (n *payloadNode) TypeName() string { return n.typ }

func (n *payloadNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "issue":
		if n.issue == nil {
			return nil, nil
		}
		return &issueNode{repo: n.repo, issue: n.issue}, nil
	case "pullRequest":
		if n.pull == nil {
			return nil, nil
		}
		return &pullNode{repo: n.repo, pull: n.pull}, nil
	case "commentEdge":
		if n.comment == nil {
			return nil, nil
		}
		return &commentEdgeNode{comment: n.comment, repo: n.repo, subjectNum: n.subjectNum}, nil
	}
	return nil, fmt.Errorf("unknown payload field %q", name)
}

type commentEdgeNode struct {
	comment    *Comment
	repo       *Repo
	subjectNum int
}

func (n *commentEdgeNode) TypeName() string { return "IssueCommentEdge" }

func (n *commentEdgeNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	if name == "node" {
		return &commentNode{comment: n.comment, repo: n.repo, subjectNum: n.subjectNum}, nil
	}
	return nil, fmt.Errorf("unknown edge field %q", name)
}

type commentNode struct {
	comment    *Comment
	repo       *Repo
	subjectNum int
}

func (n *commentNode) TypeName() string { return "IssueComment" }

func (n *commentNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "id":
		return commentGID(n.repo.Owner, n.repo.Name, n.subjectNum, n.comment.ID), nil
	case "body":
		return n.comment.Body, nil
	}
	return nil, fmt.Errorf("unknown comment field %q", name)
}

type userNode struct{}

func (n *userNode) TypeName() string { return "User" }

func (n *userNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "login":
		return viewerLogin, nil
	case "id":
		return encodeGID("user", viewerLogin), nil
	case "name":
		return viewerName, nil
	}
	return nil, fmt.Errorf("unknown user field %q", name)
}

type repoNode struct{ repo *Repo }

func (n *repoNode) TypeName() string { return "Repository" }

func (n *repoNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	r := n.repo
	switch name {
	case "id":
		return repoGID(r.Owner, r.Name), nil
	case "databaseId":
		return r.ID, nil
	case "name":
		return r.Name, nil
	case "nameWithOwner":
		return r.Owner + "/" + r.Name, nil
	case "owner":
		return &userNode{}, nil
	case "defaultBranchRef":
		if r.BranchTip(r.DefaultBranch) == "" {
			return nil, nil
		}
		return &refNode{name: r.DefaultBranch}, nil
	case "viewerPermission":
		return "ADMIN", nil
	case "hasIssuesEnabled":
		return true, nil
	case "issue":
		num := argInt(args, "number", 0)
		issue := r.Issues[num]
		if issue == nil {
			return nil, nil
		}
		return &issueNode{repo: r, issue: issue}, nil
	case "issues":
		return n.issues(args), nil
	case "pullRequests":
		first := argInt(args, "first", 100)
		pulls := []*Pull{}
		for _, pr := range r.Pulls {
			pulls = append(pulls, pr)
		}
		sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
		if first < len(pulls) {
			pulls = pulls[:first]
		}
		nodes := make([]gqlNode, 0, len(pulls))
		for _, pr := range pulls {
			nodes = append(nodes, &pullNode{repo: r, pull: pr})
		}
		return &connNode{typ: "PullRequestConnection", nodes: nodes}, nil
	case "pullRequest":
		num := argInt(args, "number", 0)
		pr := r.Pulls[num]
		if pr == nil {
			return nil, nil
		}
		return &pullNode{repo: r, pull: pr}, nil
	}
	return nil, fmt.Errorf("unknown repository field %q", name)
}

func (n *repoNode) issues(args map[string]any) *issueConnNode {
	r := n.repo
	first := argInt(args, "first", 100)
	after := argString(args, "after")
	// State filter.
	wantStates := map[string]bool{}
	if list := argList(args, "states"); list != nil {
		for _, v := range list {
			if s, ok := v.(string); ok {
				wantStates[s] = true
			}
		}
	}
	// Ordering.
	desc := false
	if orderBy := argMap(args, "orderBy"); orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok && d == "DESC" {
			desc = true
		}
	}
	issues := []*Issue{}
	for _, issue := range r.Issues {
		if len(wantStates) > 0 {
			state := "OPEN"
			if issue.State == "closed" {
				state = "CLOSED"
			}
			if !wantStates[state] {
				continue
			}
		}
		issues = append(issues, issue)
	}
	sort.Slice(issues, func(i, j int) bool {
		if desc {
			return issues[i].Number > issues[j].Number
		}
		return issues[i].Number < issues[j].Number
	})
	// Cursor: start after the given issue number.
	start := 0
	if after != "" {
		if n, ok := decodeCursor(after); ok {
			for i, issue := range issues {
				if issue.Number == n {
					start = i + 1
					break
				}
			}
		}
	}
	if start > len(issues) {
		start = len(issues)
	}
	page := issues[start:]
	hasNext := false
	if len(page) > first {
		page = page[:first]
		hasNext = true
	}
	endCursor := ""
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Number)
	}
	nodes := make([]gqlNode, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, &issueNode{repo: r, issue: issue})
	}
	return &issueConnNode{nodes: nodes, hasNext: hasNext, endCursor: endCursor}
}

type connNode struct {
	typ   string
	nodes []gqlNode
}

func (n *connNode) TypeName() string { return n.typ }

func (n *connNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	if name == "nodes" {
		return n.nodes, nil
	}
	if name == "totalCount" {
		return len(n.nodes), nil
	}
	return nil, fmt.Errorf("unknown connection field %q", name)
}

type issueConnNode struct {
	nodes     []gqlNode
	hasNext   bool
	endCursor string
}

func (n *issueConnNode) TypeName() string { return "IssueConnection" }

func (n *issueConnNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "nodes":
		return n.nodes, nil
	case "pageInfo":
		return &pageInfoNode{hasNext: n.hasNext, endCursor: n.endCursor}, nil
	case "totalCount":
		return len(n.nodes), nil
	}
	return nil, fmt.Errorf("unknown issue connection field %q", name)
}

type pageInfoNode struct {
	hasNext   bool
	endCursor string
}

func (n *pageInfoNode) TypeName() string { return "PageInfo" }

func (n *pageInfoNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "hasNextPage":
		return n.hasNext, nil
	case "endCursor":
		if n.endCursor == "" {
			return nil, nil
		}
		return n.endCursor, nil
	}
	return nil, fmt.Errorf("unknown page info field %q", name)
}

type refNode struct{ name string }

func (n *refNode) TypeName() string { return "Ref" }

func (n *refNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	if name == "name" {
		return n.name, nil
	}
	return nil, fmt.Errorf("unknown ref field %q", name)
}

type issueNode struct {
	repo  *Repo
	issue *Issue
}

func (n *issueNode) TypeName() string { return "Issue" }

func (n *issueNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "id":
		return issueGID(n.repo.Owner, n.repo.Name, n.issue.Number), nil
	case "number":
		return n.issue.Number, nil
	case "title":
		return n.issue.Title, nil
	case "state":
		if n.issue.State == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "body":
		return n.issue.Body, nil
	}
	return nil, fmt.Errorf("unknown issue field %q", name)
}

type pullNode struct {
	repo *Repo
	pull *Pull
}

func (n *pullNode) TypeName() string { return "PullRequest" }

func (n *pullNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	pr := n.pull
	switch name {
	case "id":
		return pullGID(n.repo.Owner, n.repo.Name, pr.Number), nil
	case "number":
		return pr.Number, nil
	case "title":
		return pr.Title, nil
	case "body":
		return pr.Body, nil
	case "state":
		if pr.Merged {
			return "MERGED", nil
		}
		if pr.State == "closed" {
			return "CLOSED", nil
		}
		return "OPEN", nil
	case "merged":
		return pr.Merged, nil
	case "headRefName":
		return pr.HeadRef, nil
	case "baseRefName":
		return pr.BaseRef, nil
	case "mergeable":
		if pr.State != "open" {
			return "UNKNOWN", nil
		}
		if n.repo.PRConflicted(pr) {
			return "CONFLICTING", nil
		}
		return "MERGEABLE", nil
	case "files":
		first := argInt(args, "first", 100)
		diff, err := n.repo.PRDiff(pr)
		if err != nil {
			return nil, err
		}
		if first < len(diff) {
			diff = diff[:first]
		}
		nodes := make([]gqlNode, 0, len(diff))
		for _, f := range diff {
			nodes = append(nodes, &fileNode{path: f.Path})
		}
		return &connNode{typ: "PullRequestChangedFileConnection", nodes: nodes}, nil
	}
	return nil, fmt.Errorf("unknown pull request field %q", name)
}

type fileNode struct{ path string }

func (n *fileNode) TypeName() string { return "PullRequestChangedFile" }

func (n *fileNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	if name == "path" {
		return n.path, nil
	}
	return nil, fmt.Errorf("unknown file field %q", name)
}

type typeNode struct{ name string }

func (n *typeNode) TypeName() string { return "__Type" }

func (n *typeNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	switch name {
	case "name":
		return n.name, nil
	case "kind":
		if _, ok := gqlEnumValues[n.name]; ok {
			return "ENUM", nil
		}
		return "OBJECT", nil
	case "fields":
		fields := gqlIntrospection[n.name]
		nodes := make([]gqlNode, 0, len(fields))
		for _, f := range fields {
			nodes = append(nodes, &nameNode{typ: "__Field", name: f})
		}
		return nodes, nil
	case "enumValues":
		values := gqlEnumValues[n.name]
		nodes := make([]gqlNode, 0, len(values))
		for _, v := range values {
			nodes = append(nodes, &nameNode{typ: "__EnumValue", name: v})
		}
		return nodes, nil
	case "description":
		return nil, nil
	}
	return nil, fmt.Errorf("unknown __Type field %q", name)
}

type nameNode struct {
	typ  string
	name string
}

func (n *nameNode) TypeName() string { return n.typ }

func (n *nameNode) Field(name string, args map[string]any, e *gqlExecutor) (any, error) {
	if name == "name" {
		return n.name, nil
	}
	return nil, fmt.Errorf("unknown name field %q", name)
}

// ---------------------------------------------------------------------------
// HTTP handler

type gqlHandler struct {
	store *Store
}

// ServeGraphQL handles POST /api/graphql (single or batched operations).
func (h *gqlHandler) ServeGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, h.store, 404, "Not Found")
		return
	}
	var raw json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&raw); err != nil {
		writeErr(w, h.store, 400, "Problems parsing JSON")
		return
	}
	// Batch or single?
	var results []map[string]any
	if len(raw) > 0 && raw[0] == '[' {
		var ops []map[string]any
		if err := json.Unmarshal(raw, &ops); err != nil {
			writeErr(w, h.store, 400, "Problems parsing JSON")
			return
		}
		for _, op := range ops {
			results = append(results, h.runOperation(op))
		}
		writeJSON(w, h.store, 200, results)
		return
	}
	var op map[string]any
	if err := json.Unmarshal(raw, &op); err != nil {
		writeErr(w, h.store, 400, "Problems parsing JSON")
		return
	}
	writeJSON(w, h.store, 200, h.runOperation(op))
}

func (h *gqlHandler) runOperation(op map[string]any) map[string]any {
	query, _ := op["query"].(string)
	operationName, _ := op["operationName"].(string)
	vars := map[string]any{}
	if v, ok := op["variables"].(map[string]any); ok {
		vars = v
	}
	ops, err := parseDocument(query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		}
	}
	var chosen *gqlOperation
	if operationName != "" {
		for _, o := range ops {
			if o.name == operationName {
				chosen = o
				break
			}
		}
	}
	if chosen == nil {
		chosen = ops[0]
	}
	// Apply variable defaults.
	for name, def := range chosen.varDefs {
		if _, ok := vars[name]; !ok {
			vars[name] = resolveValue(def, vars)
		}
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	e := &gqlExecutor{store: h.store, vars: vars}
	var data map[string]any
	if chosen.opType == "mutation" {
		data, _ = e.executeSelectionSet(chosen.sel, "Mutation", &mutationNode{e: e})
	} else {
		data = e.execute(chosen)
	}
	if len(e.errors) > 0 {
		return map[string]any{"data": nil, "errors": e.errors}
	}
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{"data": data}
}
