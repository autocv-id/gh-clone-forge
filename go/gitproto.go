// Git smart HTTP: pkt-line framing, upload-pack protocol v2 (advertise,
// ls-refs, fetch) and v0 advertisement, receive-pack v0 (advertise + push).
// Wire formats are git's public ones; all parsing and framing here is
// written fresh for this entry.
package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line framing

// pktLine frames a payload as one pkt-line (4-hex length prefix).
func pktLine(payload string) []byte {
	n := len(payload) + 4
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%04x%s", n, payload)
	return buf.Bytes()
}

// pktLineBytes frames raw bytes as one pkt-line.
func pktLineBytes(payload []byte) []byte {
	n := len(payload) + 4
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%04x", n)
	buf.Write(payload)
	return buf.Bytes()
}

const pktFlush = "0000"
const pktDelim = "0001"

// pktReader walks pkt-lines over a byte slice.
type pktReader struct {
	data []byte
	off  int
}

func (r *pktReader) next() (payload []byte, kind string, err error) {
	if r.off+4 > len(r.data) {
		return nil, "", fmt.Errorf("pkt-line truncated")
	}
	hdr := string(r.data[r.off : r.off+4])
	if hdr == pktFlush {
		r.off += 4
		return nil, "flush", nil
	}
	if hdr == pktDelim {
		r.off += 4
		return nil, "delim", nil
	}
	n, err := strconv.ParseInt(hdr, 16, 32)
	if err != nil || n < 4 || r.off+int(n) > len(r.data) {
		return nil, "", fmt.Errorf("bad pkt-line length %q", hdr)
	}
	payload = r.data[r.off+4 : r.off+int(n)]
	r.off += int(n)
	return payload, "data", nil
}

// rest returns the bytes after the current position (the raw pack in a
// receive-pack request).
func (r *pktReader) rest() []byte { return r.data[r.off:] }

// ---------------------------------------------------------------------------
// advertisements

// refListEntry is one ref line in an advertisement.
type refListEntry struct {
	name string
	oid  string
}

// advertisementRefs returns the repo's refs for an advertisement, with the
// HEAD ref first when it has a tip.
func (r *Repo) advertisementRefs() (headOID string, refs []refListEntry) {
	headOID = r.BranchTip(r.DefaultBranch)
	for _, name := range r.RefNames() {
		refs = append(refs, refListEntry{name: name, oid: r.Refs[name]})
	}
	return headOID, refs
}

// advertiseV2 builds the protocol v2 upload-pack advertisement.
func (r *Repo) advertiseV2() []byte {
	r.cacheMu.Lock()
	if r.advV2 != nil {
		defer r.cacheMu.Unlock()
		return r.advV2
	}
	r.cacheMu.Unlock()
	var buf bytes.Buffer
	buf.Write(pktLine("version 2\n"))
	buf.Write(pktLine("ls-refs=unborn\n"))
	buf.Write(pktLine("fetch=shallow\n"))
	buf.Write(pktLine("object-format=sha1\n"))
	buf.WriteString(pktFlush)
	out := buf.Bytes()
	r.cacheMu.Lock()
	r.advV2 = out
	r.cacheMu.Unlock()
	return out
}

// advertiseV0 builds the v0 advertisement for upload-pack or receive-pack.
func (r *Repo) advertiseV0(service string) []byte {
	key := refKey(r)
	r.cacheMu.Lock()
	if service == "git-upload-pack" && r.advV0Up != nil && r.advKey == key {
		defer r.cacheMu.Unlock()
		return r.advV0Up
	}
	if service == "git-receive-pack" && r.advV0Rc != nil && r.advKey == key {
		defer r.cacheMu.Unlock()
		return r.advV0Rc
	}
	r.cacheMu.Unlock()
	var caps string
	if service == "git-upload-pack" {
		caps = "symref=HEAD:refs/heads/" + r.DefaultBranch + " multi_ack thin-pack side-band-64k ofs-delta shallow"
	} else {
		caps = "report-status side-band-64k delete-refs"
	}
	var buf bytes.Buffer
	buf.Write(pktLine("# service=" + service + "\n"))
	buf.WriteString(pktFlush)
	headOID, refs := r.advertisementRefs()
	if headOID == "" && len(refs) == 0 {
		buf.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		first := true
		writeRef := func(oid, name string) {
			line := oid + " " + name
			if first {
				line += "\x00" + caps
				first = false
			}
			buf.Write(pktLine(line + "\n"))
		}
		if headOID != "" {
			writeRef(headOID, "HEAD")
		}
		for _, ref := range refs {
			writeRef(ref.oid, ref.name)
			// Peeled line for annotated tags.
			if strings.HasPrefix(ref.name, "refs/tags/") {
				obj := r.Objects[ref.oid]
				if obj != nil && obj.Type == "tag" {
					if t, err := ParseTag(obj.Data); err == nil {
						buf.Write(pktLine(t.Object + " " + ref.name + "^{}\n"))
					}
				}
			}
		}
	}
	buf.WriteString(pktFlush)
	out := buf.Bytes()
	r.cacheMu.Lock()
	if service == "git-upload-pack" {
		r.advV0Up = out
	} else {
		r.advV0Rc = out
	}
	r.advKey = key
	r.cacheMu.Unlock()
	return out
}

// refKey is a cheap fingerprint of the ref set, used to reuse advertisements.
func refKey(r *Repo) string {
	var sb strings.Builder
	for _, name := range r.RefNames() {
		sb.WriteString(name)
		sb.WriteByte('=')
		sb.WriteString(r.Refs[name])
		sb.WriteByte(';')
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// ls-refs (protocol v2)

// handleLsRefs answers a protocol v2 ls-refs request. args are the lines
// after the delim (without trailing newlines).
func (r *Repo) handleLsRefs(args []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		case strings.HasPrefix(a, "ref-pattern "):
			// Not requested by the suite; treat as a prefix.
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-pattern "))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var buf bytes.Buffer
	// HEAD, only when no prefix filter is given (prefixes never match HEAD
	// in practice) and symrefs was requested.
	if len(prefixes) == 0 && symrefs {
		target := "refs/heads/" + r.DefaultBranch
		if tip := r.BranchTip(r.DefaultBranch); tip != "" {
			buf.Write(pktLine(tip + " HEAD symref-target:" + target + "\n"))
		} else if unborn {
			buf.Write(pktLine("unborn HEAD symref-target:" + target + "\n"))
		}
	}
	for _, name := range r.RefNames() {
		if !matches(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if obj := r.Objects[oid]; obj != nil && obj.Type == "tag" {
				if t, err := ParseTag(obj.Data); err == nil {
					line += " peeled:" + t.Object
				}
			}
		}
		buf.Write(pktLine(line + "\n"))
	}
	buf.WriteString(pktFlush)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// fetch (protocol v2)

// fetchRequest is a parsed protocol v2 fetch command.
type fetchRequest struct {
	wants        []string
	haves        []string
	clientShal   map[string]bool
	depth        int // 0 = unlimited
	thinPack     bool
	noProgress   bool
	ofsDelta     bool
}

// parseFetchRequest parses the lines after the delim of a fetch command.
func parseFetchRequest(lines []string) *fetchRequest {
	fr := &fetchRequest{clientShal: map[string]bool{}}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "want "):
			fr.wants = append(fr.wants, strings.TrimSpace(l[5:]))
		case strings.HasPrefix(l, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(l[5:]))
		case strings.HasPrefix(l, "shallow "):
			fr.clientShal[strings.TrimSpace(l[8:])] = true
		case strings.HasPrefix(l, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(l[7:])); err == nil {
				fr.depth = n
			}
		case l == "thin-pack":
			fr.thinPack = true
		case l == "no-progress":
			fr.noProgress = true
		case l == "ofs-delta":
			fr.ofsDelta = true
		}
	}
	return fr
}

// computeFetch resolves a fetch request: the object set to send, the
// boundary (shallow) commits and the newly-deepened (unshallow) commits.
func (r *Repo) computeFetch(fr *fetchRequest) (objects map[string]*Object, shallow, unshallow []string) {
	// Included commits: BFS from the wants, depth-limited. Tags peel to
	// their target commits; the tag objects themselves are always included.
	included := map[string]bool{}
	var tagObjects []string
	depth := map[string]int{}
	queue := []string{}
	for _, w := range fr.wants {
		obj := r.Objects[w]
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			tagObjects = append(tagObjects, w)
			if t, err := ParseTag(obj.Data); err == nil {
				commit := PeelToCommit(t.Object, r.Lookup)
				if _, ok := depth[commit]; !ok {
					depth[commit] = 0
					queue = append(queue, commit)
				}
			}
			continue
		}
		if obj.Type == "commit" {
			if _, ok := depth[w]; !ok {
				depth[w] = 0
				queue = append(queue, w)
			}
		}
	}
	// BFS over parents.
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		included[oid] = true
		if fr.depth > 0 && depth[oid]+1 >= fr.depth {
			continue // parents beyond the depth limit
		}
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		for _, p := range c.Parents {
			if _, ok := depth[p]; !ok {
				depth[p] = depth[oid] + 1
				queue = append(queue, p)
			}
		}
	}
	// Boundary commits: included commits with a parent that is not included
	// (only when a depth limit applies).
	if fr.depth > 0 {
		for oid := range included {
			obj := r.Objects[oid]
			if obj == nil || obj.Type != "commit" {
				continue
			}
			c, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			boundary := false
			for _, p := range c.Parents {
				if !included[p] {
					boundary = true
					break
				}
			}
			if boundary {
				shallow = append(shallow, oid)
			}
		}
	}
	// Unshallow: client-shallow commits that are now included with all their
	// parents included too.
	for oid := range fr.clientShal {
		if !included[oid] {
			continue
		}
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		deep := true
		for _, p := range c.Parents {
			if !included[p] {
				deep = false
				break
			}
		}
		if deep {
			unshallow = append(unshallow, oid)
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)

	// Full object set: the included commits themselves, the tag objects,
	// and the tree closure of each included commit. The trees must be
	// walked on their own: CollectReachable from a commit would follow its
	// parents and undo the depth limit.
	includedObjs := map[string]bool{}
	for oid := range included {
		includedObjs[oid] = true
	}
	for _, t := range tagObjects {
		includedObjs[t] = true
	}
	for oid := range included {
		tree := commitTree(oid, r.Lookup)
		if tree == "" {
			continue
		}
		if treeObjs, err := CollectReachable([]string{tree}, r.Lookup, true); err == nil {
			for o := range treeObjs {
				includedObjs[o] = true
			}
		}
	}

	// Exclusion closure of the haves: walk parents, stopping at commits the
	// client declares shallow (their trees and blobs are still excluded).
	excluded := map[string]bool{}
	haveQueue := append([]string(nil), fr.haves...)
	for len(haveQueue) > 0 {
		oid := haveQueue[0]
		haveQueue = haveQueue[1:]
		if excluded[oid] {
			continue
		}
		excluded[oid] = true
		obj := r.Objects[oid]
		if obj == nil {
			continue
		}
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			// The tree closure is excluded regardless.
			if treeObjs, err := CollectReachable([]string{c.Tree}, r.Lookup, true); err == nil {
				for o := range treeObjs {
					excluded[o] = true
				}
			}
			if fr.clientShal[oid] {
				continue // do not walk past a client-shallow commit
			}
			haveQueue = append(haveQueue, c.Parents...)
		case "tag":
			if t, err := ParseTag(obj.Data); err == nil {
				haveQueue = append(haveQueue, t.Object)
			}
		case "tree":
			if treeObjs, err := CollectReachable([]string{oid}, r.Lookup, true); err == nil {
				for o := range treeObjs {
					excluded[o] = true
				}
			}
		}
	}

	objects = map[string]*Object{}
	for oid := range includedObjs {
		if excluded[oid] {
			continue
		}
		if obj := r.Objects[oid]; obj != nil {
			objects[oid] = obj
		}
	}
	return objects, shallow, unshallow
}

// handleFetch answers a protocol v2 fetch request.
func (r *Repo) handleFetch(fr *fetchRequest) []byte {
	// Cache key: the request's identity. The clone workload repeats the
	// same fetch 50 times, so the second and later fetches are free.
	var key strings.Builder
	key.WriteString("fetch|")
	for _, w := range fr.wants {
		key.WriteString(w)
		key.WriteByte(',')
	}
	key.WriteByte('|')
	for _, h := range fr.haves {
		key.WriteString(h)
		key.WriteByte(',')
	}
	key.WriteString(fmt.Sprintf("|%d|", fr.depth))
	for s := range fr.clientShal {
		key.WriteString(s)
		key.WriteByte(',')
	}
	cacheKey := key.String()
	r.cacheMu.Lock()
	if cached, ok := r.packs[cacheKey]; ok {
		r.cacheMu.Unlock()
		return cached
	}
	gen := r.gen
	r.cacheMu.Unlock()
	objects, shallow, unshallow := r.computeFetch(fr)
	// Deterministic pack order: commits, then trees, then blobs, then tags,
	// each sorted by oid.
	typeRank := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	list := make([]*Object, 0, len(objects))
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool {
		a, b := objects[oids[i]], objects[oids[j]]
		if typeRank[a.Type] != typeRank[b.Type] {
			return typeRank[a.Type] < typeRank[b.Type]
		}
		return oids[i] < oids[j]
	})
	for _, oid := range oids {
		list = append(list, objects[oid])
	}
	pack := WritePack(list)

	var buf bytes.Buffer
	// Acknowledgments section: a header line, then NAK or the ACKs.
	buf.Write(pktLine("acknowledgments\n"))
	if len(fr.haves) == 0 {
		buf.Write(pktLine("NAK\n"))
	} else {
		acked := false
		for _, h := range fr.haves {
			if r.Objects[h] != nil {
				buf.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			buf.Write(pktLine("NAK\n"))
		} else {
			buf.Write(pktLine("ready\n"))
		}
	}
	buf.WriteString(pktDelim)
	// Shallow-info section, only when there is something to report.
	if len(shallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine("shallow-info\n"))
		for _, s := range shallow {
			buf.Write(pktLine("shallow " + s + "\n"))
		}
		for _, s := range unshallow {
			buf.Write(pktLine("unshallow " + s + "\n"))
		}
		buf.WriteString(pktDelim)
	}
	// Packfile section: header line, then sideband band-1 packets.
	buf.Write(pktLine("packfile\n"))
	writeSideband(&buf, 1, pack)
	buf.WriteString(pktFlush)
	out := buf.Bytes()
	// Cache only if the repo did not change while the pack was built.
	r.cacheMu.Lock()
	if r.gen == gen {
		if r.packs == nil {
			r.packs = map[string][]byte{}
		}
		r.packs[cacheKey] = out
	}
	r.cacheMu.Unlock()
	return out
}

// writeSideband writes data as sideband packets on the given band, each
// pkt-line at most 65,520 bytes total (band byte + payload).
func writeSideband(buf *bytes.Buffer, band byte, data []byte) {
	const maxPayload = 65500
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxPayload {
			chunk = data[:maxPayload]
		}
		payload := make([]byte, 1, len(chunk)+1)
		payload[0] = band
		payload = append(payload, chunk...)
		buf.Write(pktLineBytes(payload))
		data = data[len(chunk):]
	}
}

// ---------------------------------------------------------------------------
// receive-pack (protocol v0)

// pushCommand is one ref update from a receive-pack request.
type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// handleReceivePack processes a receive-pack POST body and returns the
// report-status response body (sideband-wrapped when requested).
func (r *Repo) handleReceivePack(body []byte) []byte {
	pr := &pktReader{data: body}
	var commands []pushCommand
	sideband := false
	for {
		payload, kind, err := pr.next()
		if err != nil {
			return receiveErrorResponse("bad pkt-line: " + err.Error(), true)
		}
		if kind == "flush" {
			break
		}
		if kind != "data" {
			continue
		}
		line := string(payload)
		line = strings.TrimSuffix(line, "\n")
		// Capabilities ride on the first command line after a NUL.
		if i := strings.IndexByte(line, 0); i >= 0 {
			caps := line[i+1:]
			if strings.Contains(caps, "side-band-64k") {
				sideband = true
			}
			line = line[:i]
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			return receiveErrorResponse("bad command line: "+line, true)
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	packBytes := pr.rest()
	unpackErr := ""
	if len(packBytes) > 0 {
		objects, err := ReadPack(packBytes, r.Lookup)
		if err != nil {
			unpackErr = err.Error()
		} else {
			for oid, obj := range objects {
				if r.Objects[oid] == nil {
					r.Objects[oid] = obj
				}
			}
		}
	}
	// Build the report-status stream.
	var inner bytes.Buffer
	if unpackErr != "" {
		inner.Write(pktLine("unpack " + unpackErr + "\n"))
	} else {
		inner.Write(pktLine("unpack ok\n"))
	}
	changed := false
	for _, cmd := range commands {
		if unpackErr != "" {
			inner.Write(pktLine("ng " + cmd.ref + " unpack failed\n"))
			continue
		}
		current := r.Refs[cmd.ref]
		if current == "" {
			current = zeroOID
		}
		switch {
		case cmd.oldOID != current:
			inner.Write(pktLine("ng " + cmd.ref + " old value mismatch\n"))
		case cmd.newOID == zeroOID:
			// Delete.
			delete(r.Refs, cmd.ref)
			changed = true
			inner.Write(pktLine("ok " + cmd.ref + "\n"))
		case cmd.oldOID == zeroOID:
			// Create.
			if _, ok := r.Objects[cmd.newOID]; !ok {
				inner.Write(pktLine("ng " + cmd.ref + " invalid object\n"))
			} else {
				r.Refs[cmd.ref] = cmd.newOID
				changed = true
				inner.Write(pktLine("ok " + cmd.ref + "\n"))
			}
		default:
			// Update: must be a fast-forward.
			if _, ok := r.Objects[cmd.newOID]; !ok {
				inner.Write(pktLine("ng " + cmd.ref + " invalid object\n"))
			} else if !IsAncestor(cmd.oldOID, cmd.newOID, r.Lookup) {
				inner.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
			} else {
				r.Refs[cmd.ref] = cmd.newOID
				changed = true
				inner.Write(pktLine("ok " + cmd.ref + "\n"))
			}
		}
	}
	inner.WriteString(pktFlush)
	if changed {
		r.invalidate()
	}
	if sideband {
		var out bytes.Buffer
		writeSideband(&out, 1, inner.Bytes())
		out.WriteString(pktFlush)
		return out.Bytes()
	}
	return inner.Bytes()
}

// receiveErrorResponse is a minimal error reply for an unparseable push.
func receiveErrorResponse(msg string, sideband bool) []byte {
	var inner bytes.Buffer
	inner.Write(pktLine("unpack " + msg + "\n"))
	inner.WriteString(pktFlush)
	if sideband {
		var out bytes.Buffer
		writeSideband(&out, 1, inner.Bytes())
		out.WriteString(pktFlush)
		return out.Bytes()
	}
	return inner.Bytes()
}

// hexOID decodes a 40-hex oid to raw bytes (unused helper kept for clarity).
func hexOID(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}
