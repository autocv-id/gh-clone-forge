// Git object model: hashing, tree/commit/tag parse and build, packfile
// read (plain + OFS/REF deltas) and write (plain, no deltas). All code
// written fresh for this entry; the wire formats are git's public ones.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

const zeroOID = "0000000000000000000000000000000000000000"

// Object is one git object: its type and raw payload bytes.
type Object struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

// HashObject returns the SHA-1 id of an object: sha1("<type> <len>\0<data>").
func HashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// IsOID reports whether s is a 40-hex object id.
func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// trees

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string // "100644" for files, "40000" for directories
	Name string
	OID  string
}

// ParseTree decodes a tree object's entries.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry without mode")
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry without name")
		}
		name := string(data[i : i+nul])
		i += nul + 1
		if i+20 > len(data) {
			return nil, errors.New("tree entry without oid")
		}
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out, nil
}

// treeSortKey is git's tree ordering: directory names sort as if they ended in '/'.
func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTree encodes entries in git's canonical order.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", e.Mode, e.Name)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// commits and tags

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit decodes a commit object.
func ParseCommit(data []byte) (CommitView, error) {
	var c CommitView
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return c, errors.New("commit without header terminator")
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			break
		}
		// Multi-line (continued) headers start with a space; the suite never
		// sends them, but skip them rather than misreading the field.
		if line[0] == ' ' {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "tree":
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author = val
		case "committer":
			c.Committer = val
		}
	}
	c.Message = string(rest)
	return c, nil
}

// BuildCommit encodes a commit object.
func BuildCommit(tree string, parents []string, author, committer string, timestamp int64, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p)
	}
	fmt.Fprintf(&buf, "author %s %d +0000\n", author, timestamp)
	fmt.Fprintf(&buf, "committer %s %d +0000\n", committer, timestamp)
	buf.WriteByte('\n')
	buf.WriteString(message)
	return buf.Bytes()
}

// TagView is a parsed annotated-tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// ParseTag decodes a tag object.
func ParseTag(data []byte) (TagView, error) {
	var t TagView
	rest := data
	for {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return t, errors.New("tag without header terminator")
		}
		line := rest[:nl]
		rest = rest[nl+1:]
		if len(line) == 0 {
			break
		}
		if line[0] == ' ' {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := string(line[:sp]), string(line[sp+1:])
		switch key {
		case "object":
			t.Object = val
		case "type":
			t.Type = val
		case "tag":
			t.Tag = val
		case "tagger":
			t.Tagger = val
		}
	}
	t.Message = string(rest)
	return t, nil
}

// ---------------------------------------------------------------------------
// reachability

// Lookup resolves an oid to its object, or nil.
type Lookup func(oid string) *Object

// CollectReachable returns every object reachable from roots (commits, their
// trees and blobs, tag targets). Missing objects are an error unless
// allowMissing is set, in which case the walk stops at them.
func CollectReachable(roots []string, lookup Lookup, allowMissing bool) (map[string]bool, error) {
	seen := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] || oid == zeroOID {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("missing object %s", oid)
		}
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				return nil, err
			}
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case "tree":
			entries, err := ParseTree(obj.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				stack = append(stack, e.OID)
			}
		case "tag":
			t, err := ParseTag(obj.Data)
			if err != nil {
				return nil, err
			}
			stack = append(stack, t.Object)
		}
	}
	return seen, nil
}

// IsAncestor reports whether ancestor is reachable from descendant by walking
// commit parents only (tags are peeled first). descendant == ancestor is true.
func IsAncestor(ancestor, descendant string, lookup Lookup) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case "commit":
			c, err := ParseCommit(obj.Data)
			if err != nil {
				continue
			}
			for _, p := range c.Parents {
				if p == ancestor {
					return true
				}
				stack = append(stack, p)
			}
		case "tag":
			t, err := ParseTag(obj.Data)
			if err != nil {
				continue
			}
			if t.Object == ancestor {
				return true
			}
			stack = append(stack, t.Object)
		}
	}
	return false
}

// PeelToCommit follows tag objects to the commit they point at.
func PeelToCommit(oid string, lookup Lookup) string {
	for i := 0; i < 16; i++ {
		obj := lookup(oid)
		if obj == nil || obj.Type != "tag" {
			return oid
		}
		t, err := ParseTag(obj.Data)
		if err != nil {
			return oid
		}
		oid = t.Object
	}
	return oid
}

// MergeBase finds a common ancestor of a and b (the first found by a
// breadth-first walk from both sides, newest first).
func MergeBase(a, b string, lookup Lookup) string {
	if a == b {
		return a
	}
	aSet := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if aSet[oid] {
			continue
		}
		aSet[oid] = true
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	seen := map[string]bool{}
	queue = []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if aSet[oid] {
			return oid
		}
		obj := lookup(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c, err := ParseCommit(obj.Data)
		if err != nil {
			continue
		}
		queue = append(queue, c.Parents...)
	}
	return ""
}

// ---------------------------------------------------------------------------
// packfiles

var packTypeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta"}

// applyDelta reconstructs an object from a base and a git delta.
func applyDelta(base, delta []byte) ([]byte, error) {
	// Delta header: source size varint, target size varint.
	readVarint := func(b []byte, off int) (int, int, error) {
		result := 0
		shift := uint(0)
		for {
			if off >= len(b) {
				return 0, 0, errors.New("delta varint truncated")
			}
			c := b[off]
			off++
			result |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return result, off, nil
	}
	srcSize, off, err := readVarint(delta, 0)
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size %d, expected %d", srcSize, len(base))
	}
	dstSize, off, err := readVarint(delta, off)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			// Copy from base: offset/size bitfields.
			var cpOff, cpSize int
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					cpOff |= int(delta[off]) << uint(8*i)
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy truncated")
					}
					cpSize |= int(delta[off]) << uint(8*i)
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			if off+int(op) > len(delta) {
				return nil, errors.New("delta insert truncated")
			}
			out = append(out, delta[off:off+int(op)]...)
			off += int(op)
		} else {
			return nil, errors.New("delta op 0")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta produced %d bytes, expected %d", len(out), dstSize)
	}
	return out, nil
}

// ReadPack parses a packfile (version 2 or 3) and resolves every object,
// including OFS_DELTA and REF_DELTA entries. resolveBase supplies a base
// object a thin pack leaves out.
func ReadPack(pack []byte, resolveBase Lookup) (map[string]*Object, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	// Verify the trailer checksum.
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	type rawEntry struct {
		offset    int
		typeCode  int
		size      int
		data      []byte // inflated payload (delta bytes for delta entries)
		baseOff   int    // ofs-delta: absolute offset of the base
		baseOID   string // ref-delta: oid of the base
		resolved  *Object
		resolving bool
	}
	raw := make([]*rawEntry, 0, count)
	byOffset := map[int]*rawEntry{}
	off := 12
	for i := 0; i < count; i++ {
		entryOff := off
		if off >= trailerStart {
			return nil, errors.New("pack truncated in entry header")
		}
		// Type/size varint: first byte holds type (bits 4-6) and size low 4 bits.
		first := pack[off]
		off++
		typeCode := int(first>>4) & 0x07
		size := int(first & 0x0f)
		shift := uint(4)
		for first&0x80 != 0 {
			if off >= trailerStart {
				return nil, errors.New("pack truncated in size varint")
			}
			first = pack[off]
			off++
			size |= int(first&0x7f) << shift
			shift += 7
		}
		e := &rawEntry{offset: entryOff, typeCode: typeCode, size: size}
		switch typeCode {
		case 6: // OFS_DELTA
			if off >= trailerStart {
				return nil, errors.New("pack truncated in ofs-delta base")
			}
			b := pack[off]
			off++
			baseOff := int(b & 0x7f)
			for b&0x80 != 0 {
				if off >= trailerStart {
					return nil, errors.New("pack truncated in ofs-delta base")
				}
				b = pack[off]
				off++
				baseOff = ((baseOff + 1) << 7) | int(b&0x7f)
			}
			e.baseOff = entryOff - baseOff
			if e.baseOff < 0 || e.baseOff >= entryOff {
				return nil, fmt.Errorf("ofs-delta at %d names invalid base offset %d", entryOff, e.baseOff)
			}
		case 7: // REF_DELTA
			if off+20 > trailerStart {
				return nil, errors.New("pack truncated in ref-delta base")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		default:
			if packCodeType[typeCode] == "" {
				return nil, fmt.Errorf("pack entry %d has unknown type code %d", i, typeCode)
			}
		}
		// Inflate the zlib stream; the bytes.Reader reports how many bytes
		// the stream consumed so the next entry starts at the right offset.
		br := bytes.NewReader(pack[off:trailerStart])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack entry at %d: bad zlib stream: %w", entryOff, err)
		}
		inflated, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("pack entry at %d: inflate failed: %w", entryOff, err)
		}
		if len(inflated) != size {
			return nil, fmt.Errorf("pack entry at %d declares %d bytes but inflated to %d", entryOff, size, len(inflated))
		}
		e.data = inflated
		off += len(pack[off:trailerStart]) - br.Len()
		raw = append(raw, e)
		byOffset[entryOff] = e
	}
	if off != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-off)
	}

	objects := map[string]*Object{}
	resolvedByOID := map[string]*rawEntry{}
	var resolve func(e *rawEntry) (*Object, error)
	resolve = func(e *rawEntry) (*Object, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if e.resolving {
			return nil, errors.New("delta cycle in pack")
		}
		e.resolving = true
		var obj *Object
		switch e.typeCode {
		case 6:
			base := byOffset[e.baseOff]
			if base == nil {
				return nil, fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOff)
			}
			b, err := resolve(base)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(b.Data, e.data)
			if err != nil {
				return nil, err
			}
			obj = &Object{Type: b.Type, Data: data}
		case 7:
			var b *Object
			if base, ok := resolvedByOID[e.baseOID]; ok {
				var err error
				b, err = resolve(base)
				if err != nil {
					return nil, err
				}
			} else if resolveBase != nil {
				b = resolveBase(e.baseOID)
			}
			if b == nil {
				return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor in the store", e.baseOID)
			}
			data, err := applyDelta(b.Data, e.data)
			if err != nil {
				return nil, err
			}
			obj = &Object{Type: b.Type, Data: data}
		default:
			obj = &Object{Type: packCodeType[e.typeCode], Data: e.data}
		}
		e.resolved = obj
		e.resolving = false
		oid := HashObject(obj.Type, obj.Data)
		resolvedByOID[oid] = e
		if _, ok := objects[oid]; !ok {
			objects[oid] = obj
		}
		return obj, nil
	}
	for _, e := range raw {
		if _, err := resolve(e); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// WritePack builds a packfile (version 2) from plain objects, no deltas.
// The order is the caller's; git accepts any order.
func WritePack(objects []*Object) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	writeU32(&buf, 2)
	writeU32(&buf, uint32(len(objects)))
	for _, obj := range objects {
		writePackObjectHeader(&buf, obj.Type, len(obj.Data))
		w := zlib.NewWriter(&buf)
		w.Write(obj.Data)
		w.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func writeU32(buf *bytes.Buffer, v uint32) {
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}

func writePackObjectHeader(buf *bytes.Buffer, typ string, size int) {
	code := packTypeCode[typ]
	first := byte(code<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		buf.WriteByte(first | 0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(first)
}
