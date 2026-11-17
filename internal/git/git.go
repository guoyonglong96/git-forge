// Package git is the pure git object model: hashing, tree/commit/tag
// parse/build, and traversal. No I/O, no git binary. Ported from git.ts.
//
// Phase 2a note: only the object-model + traversal functions the store
// depends on are implemented here. Pkt-line framing (encodePktLine,
// decodePktLines, splitCommandsAndPack) and all of pack.go (readPack /
// writePack / applyDelta) are TODO stubs for later phases — the server's
// git routes are 501 stubs until then.
package git

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ZeroOID is the all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

// ObjType is a git object type.
type ObjType string

// Object types.
const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

// GitObject is one object in a repo's object store.
type GitObject struct {
	Type ObjType
	Data []byte
}

// Lookup resolves an oid to an object, or nil when absent.
type Lookup func(oid string) *GitObject

// SHA1Hex returns the lowercase hex SHA-1 of data.
func SHA1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// HashObject returns the git object id for (type, data): SHA-1 over
// "<type> <len>\0" + data, hashed incrementally (no concat copy).
func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// IsOID reports whether v is a 40-char lowercase hex string.
func IsOID(v string) bool {
	if len(v) != 40 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// TreeEntry is one entry in a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses tree object bytes into entries. Throws 'bad tree' on
// truncation, mirroring git.ts.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("bad tree")
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("bad tree")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, errors.New("bad tree")
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{
			Mode: string(data[off:sp]),
			Name: string(data[sp+1 : nul]),
			OID:  oid,
		})
		off = nul + 21
	}
	return entries, nil
}

func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	if a.Mode == "40000" {
		an += "/"
	}
	bn := b.Name
	if b.Mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

// BuildTree serialises tree entries in git tree order (directory entries
// compare as name + '/').
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit parses commit object bytes. Continuation (folded) headers are
// not handled, mirroring git.ts.
func ParseCommit(data []byte) CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headerText := string(data[:headerEnd])
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	var c CommitView
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		key, value := line, ""
		if sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author = value
		case "committer":
			c.Committer = value
		}
	}
	c.Message = message
	return c
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses tag object bytes.
func ParseTag(data []byte) TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	if blank >= 0 {
		headerEnd = blank
	}
	headers := map[string]string{}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.Index(line, " ")
		if sp < 0 {
			continue
		}
		headers[line[:sp]] = line[sp+1:]
	}
	msg := ""
	if blank >= 0 {
		msg = string(data[blank+2:])
	}
	return TagView{
		Object:  headers["object"],
		Type:    headers["type"],
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: msg,
	}
}

// BuildCommit builds a commit object's bytes. Message is written verbatim
// (no trailing newline added).
func BuildCommit(tree string, parents []string, message, who string, timestamp int64) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", who, timestamp)
	fmt.Fprintf(&b, "committer %s %d +0000\n", who, timestamp)
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// BuildTag builds an annotated tag object's bytes.
func BuildTag(object string, typ ObjType, tag, message, tagger string, timestamp int64) []byte {
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s",
		object, typ, tag, tagger, timestamp, message))
}

// FlatEntry is one path in a flattened tree: object id + mode.
type FlatEntry struct {
	OID  string
	Mode string
}

// CollectReachable returns all object ids reachable from tips (commits,
// trees, blobs, tags), mirroring git.ts (stack DFS, missing objects skipped
// silently).
func CollectReachable(tips []string, lookup Lookup) map[string]struct{} {
	seen := map[string]struct{}{}
	stack := append([]string(nil), tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[oid]; ok || !IsOID(oid) {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		seen[oid] = struct{}{}
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			stack = append(stack, c.Tree)
			stack = append(stack, c.Parents...)
		case ObjTree:
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				stack = append(stack, e.OID)
			}
		case ObjTag:
			stack = append(stack, ParseTag(obj.Data).Object)
		}
	}
	return seen
}

// IsAncestor reports whether ancestor is reachable from oid (following
// commit parents, peeling tags).
func IsAncestor(ancestor, oid string, lookup Lookup) bool {
	if ancestor == oid {
		return true
	}
	seen := map[string]struct{}{}
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[cur]; ok {
			continue
		}
		seen[cur] = struct{}{}
		obj := lookup(cur)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case ObjCommit:
			c := ParseCommit(obj.Data)
			for _, p := range c.Parents {
				if p == ancestor {
					return true
				}
				stack = append(stack, p)
			}
		case ObjTag:
			t := ParseTag(obj.Data)
			if t.Object == ancestor {
				return true
			}
			stack = append(stack, t.Object)
		}
	}
	return false
}

// ListCommits returns commits reachable from tip, newest-first (DFS
// preorder, parents after the commit). Tags are peeled.
func ListCommits(tip string, lookup Lookup) []string {
	var out []string
	seen := map[string]struct{}{}
	var visit func(oid string)
	visit = func(oid string) {
		if _, ok := seen[oid]; ok {
			return
		}
		obj := lookup(oid)
		if obj == nil {
			return
		}
		if obj.Type == ObjTag {
			visit(ParseTag(obj.Data).Object)
			return
		}
		if obj.Type != ObjCommit {
			return
		}
		seen[oid] = struct{}{}
		out = append(out, oid)
		for _, p := range ParseCommit(obj.Data).Parents {
			visit(p)
		}
	}
	visit(tip)
	return out
}

// MergeBase returns a merge-base of a and b: the common ancestor nearest to
// a (BFS from a, then BFS from b). Returns ("", false) when there is none.
// The BFS queue uses an index cursor instead of Node's shift(), same order.
func MergeBase(a, b string, lookup Lookup) (string, bool) {
	ancestorsA := map[string]struct{}{}
	queueA := []string{a}
	for head := 0; head < len(queueA); head++ {
		cur := queueA[head]
		if _, ok := ancestorsA[cur]; ok {
			continue
		}
		obj := lookup(cur)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		ancestorsA[cur] = struct{}{}
		queueA = append(queueA, ParseCommit(obj.Data).Parents...)
	}
	seenB := map[string]struct{}{}
	queueB := []string{b}
	for head := 0; head < len(queueB); head++ {
		cur := queueB[head]
		if _, ok := seenB[cur]; ok {
			continue
		}
		seenB[cur] = struct{}{}
		if _, ok := ancestorsA[cur]; ok {
			return cur, true
		}
		obj := lookup(cur)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		queueB = append(queueB, ParseCommit(obj.Data).Parents...)
	}
	return "", false
}

// FlattenTree returns the flat path -> {oid, mode} map of a tree
// (recursive; mode "40000" recurses).
func FlattenTree(treeOID string, lookup Lookup) map[string]FlatEntry {
	out := map[string]FlatEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := lookup(oid)
		if obj == nil || obj.Type != ObjTree {
			return
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return
		}
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = FlatEntry{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, "")
	return out
}

// TreeLevel returns the entries of a tree at one level (for directory
// listings). Empty when the object is missing or not a tree.
func TreeLevel(treeOID string, lookup Lookup) []TreeEntry {
	obj := lookup(treeOID)
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil
	}
	return entries
}

// ---------------------------------------------------------------------------
// pkt-line framing (git.ts 528-571)
// ---------------------------------------------------------------------------

// PktKind is the kind of a decoded pkt-line.
type PktKind int

// Pkt-line kinds.
const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

// Pkt is one decoded pkt-line.
type Pkt struct {
	Kind    PktKind
	Payload []byte
}

// DataPkt builds a data pkt-line item from raw bytes.
func DataPkt(payload []byte) Pkt { return Pkt{Kind: PktData, Payload: payload} }

// StrPkt builds a data pkt-line item from a string.
func StrPkt(s string) Pkt { return Pkt{Kind: PktData, Payload: []byte(s)} }

// FlushPkt builds a flush pkt-line item.
func FlushPkt() Pkt { return Pkt{Kind: PktFlush} }

// DelimPkt builds a delimiter pkt-line item.
func DelimPkt() Pkt { return Pkt{Kind: PktDelim} }

// PKTFlush is the 4-byte flush pkt-line ("0000").
var PKTFlush = []byte("0000")

// PKTDelim is the 4-byte delimiter pkt-line ("0001").
var PKTDelim = []byte("0001")

// PKTResponseEnd is the 4-byte response-end pkt-line ("0002").
var PKTResponseEnd = []byte("0002")

// EncodePktLine encodes one pkt-line: 4 lowercase-hex length (len+4,
// zero-padded), max payload 65,516 else 'pkt-line too long' (git.ts).
func EncodePktLine(payload []byte) ([]byte, error) {
	if len(payload) > 65516 {
		return nil, errors.New("pkt-line too long")
	}
	out := make([]byte, 0, len(payload)+4)
	out = append(out, fmt.Sprintf("%04x", len(payload)+4)...)
	out = append(out, payload...)
	return out, nil
}

// EncodePktLineString encodes one pkt-line from a string payload.
func EncodePktLineString(s string) ([]byte, error) {
	return EncodePktLine([]byte(s))
}

// EncodePktLines encodes a sequence of pkt-line items (data, flush,
// delim, response-end) into one buffer.
func EncodePktLines(items []Pkt) ([]byte, error) {
	var out []byte
	for _, it := range items {
		switch it.Kind {
		case PktFlush:
			out = append(out, PKTFlush...)
		case PktDelim:
			out = append(out, PKTDelim...)
		case PktResponseEnd:
			out = append(out, PKTResponseEnd...)
		default:
			b, err := EncodePktLine(it.Payload)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	}
	return out, nil
}

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// DecodePktLines decodes a whole buffer of pkt-lines. Strict: non-hex
// length -> 'bad pkt-line length ...'; len 0/1/2 -> flush/delim/
// response-end; len < 4 or overrun -> 'truncated pkt-line' (git.ts).
// Payloads are views into buf.
func DecodePktLines(buf []byte) ([]Pkt, error) {
	var out []Pkt
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, errors.New("truncated pkt-line header")
		}
		lenHex := buf[off : off+4]
		if !isHex4(lenHex) {
			return nil, fmt.Errorf("bad pkt-line length %s", strconv.Quote(string(lenHex)))
		}
		length, _ := strconv.ParseInt(string(lenHex), 16, 32)
		if length == 0 {
			out = append(out, Pkt{Kind: PktFlush})
			off += 4
			continue
		}
		if length == 1 {
			out = append(out, Pkt{Kind: PktDelim})
			off += 4
			continue
		}
		if length == 2 {
			out = append(out, Pkt{Kind: PktResponseEnd})
			off += 4
			continue
		}
		if length < 4 || off+int(length) > len(buf) {
			return nil, errors.New("truncated pkt-line")
		}
		out = append(out, Pkt{Kind: PktData, Payload: buf[off+4 : off+int(length)]})
		off += int(length)
	}
	return out, nil
}

// SplitCommandsAndPack splits a receive-pack body into command pkt-lines
// and the trailing pack bytes. Decodes until the first flush (inclusive);
// the pack is never fed to the pkt decoder (git.ts). A len-2
// (response-end) line is 'truncated pkt-line' here, as in git.ts, which
// only special-cases flush and delim in this function.
func SplitCommandsAndPack(body []byte) ([]Pkt, []byte, error) {
	var packets []Pkt
	off := 0
	for off < len(body) {
		if off+4 > len(body) {
			return nil, nil, errors.New("truncated pkt-line header")
		}
		lenHex := body[off : off+4]
		if !isHex4(lenHex) {
			return nil, nil, errors.New("bad pkt-line length")
		}
		length, _ := strconv.ParseInt(string(lenHex), 16, 32)
		if length == 0 {
			packets = append(packets, Pkt{Kind: PktFlush})
			off += 4
			break
		}
		if length == 1 {
			packets = append(packets, Pkt{Kind: PktDelim})
			off += 4
			continue
		}
		if length < 4 || off+int(length) > len(body) {
			return nil, nil, errors.New("truncated pkt-line")
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: body[off+4 : off+int(length)]})
		off += int(length)
	}
	return packets, body[off:], nil
}
