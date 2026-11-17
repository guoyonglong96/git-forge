// Package gitproto — git smart-HTTP protocol handlers (v2 upload-pack,
// v0, receive-pack). Ported from gitproto.ts (668 lines); map §5.4–5.5.
//
// LOCKING: every function here must be called with the store lock held
// (server.HandleRequest acquires RLock for read-only requests, Lock for
// mutating ones — see internal/server). The per-repo memo caches are
// self-synchronizing (their own RWMutex) because they are written on the
// read path; the store's maps/slices themselves are only read here for
// read-only calls, and mutations (ApplyPush) run under the write lock.
// Lock order: store lock first, then a cache mutex — never the reverse.
//
// The only push failure reason string ever produced is
// "non-fast-forward" (gitproto.ts applyPush).
package gitproto

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"forge/internal/git"
	"forge/internal/store"
)

// Agent is the agent string advertised in v2 capabilities.
const Agent = "git-forge/1.0"

// Content types for git smart-HTTP responses (app.ts CT_GIT_*).
const (
	ContentTypeUploadAdvertisement  = "application/x-git-upload-pack-advertisement"
	ContentTypeReceiveAdvertisement = "application/x-git-receive-pack-advertisement"
	ContentTypeUploadResult         = "application/x-git-upload-pack-result"
	ContentTypeReceiveResult        = "application/x-git-receive-pack-result"
)

// RefEntry is one sorted ref: full refname + oid.
type RefEntry struct {
	Name string
	OID  string
}

// SortedRefs returns the repo's refs sorted by name (lexicographic).
func SortedRefs(repo *store.Repo) []RefEntry {
	out := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// peelTag peels an annotated tag ref to its commit oid (up to 10 hops).
// Returns ("", false) when oid is not a tag or does not peel to a commit.
func peelTag(repo *store.Repo, oid string) (string, bool) {
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != git.ObjTag {
		return "", false
	}
	cur := git.ParseTag(obj.Data).Object
	for i := 0; i < 10; i++ {
		next, ok := repo.Objects[cur]
		if !ok {
			return "", false
		}
		if next.Type == git.ObjCommit {
			return cur, true
		}
		if next.Type == git.ObjTag {
			cur = git.ParseTag(next.Data).Object
			continue
		}
		return "", false
	}
	return "", false
}

// peelToCommit follows tags up to 10 hops to a commit oid (gitproto.ts
// store_peelToCommit). Returns ("", false) when it does not resolve.
func peelToCommit(repo *store.Repo, oid string) (string, bool) {
	cur := oid
	for i := 0; i < 10; i++ {
		obj, ok := repo.Objects[cur]
		if !ok {
			return "", false
		}
		switch obj.Type {
		case git.ObjCommit:
			return cur, true
		case git.ObjTag:
			cur = git.ParseTag(obj.Data).Object
		default:
			return "", false
		}
	}
	return "", false
}

func headTarget(repo *store.Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

func mustPktLine(s string) []byte {
	b, err := git.EncodePktLineString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Advertisements (gitproto.ts 56-111)
// ---------------------------------------------------------------------------

var v2AdvertisementBytes = func() []byte {
	b, err := git.EncodePktLines([]git.Pkt{
		git.StrPkt("version 2\n"),
		git.StrPkt("ls-refs=unborn\n"),
		git.StrPkt("fetch=shallow filter\n"),
		git.StrPkt("object-format=sha1\n"),
		git.StrPkt("agent=" + Agent + "\n"),
		git.FlushPkt(),
	})
	if err != nil {
		panic(err)
	}
	return b
}()

// V2Advertisement returns the v2 capability advertisement body: the same
// bytes for every repository.
func V2Advertisement() []byte {
	return v2AdvertisementBytes
}

// V0UploadAdvertisement builds the v0 upload-pack advertisement body
// (refs + capabilities).
func V0UploadAdvertisement(repo *store.Repo) []byte {
	caps := "symref=HEAD:" + headTarget(repo) + " multi_ack thin-pack side-band-64k ofs-delta shallow"
	items := []git.Pkt{git.StrPkt("# service=git-upload-pack\n"), git.FlushPkt()}
	refs := SortedRefs(repo)
	headOID, hasHead := repo.Refs[headTarget(repo)]
	if len(refs) == 0 {
		items = append(items, git.StrPkt(git.ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		oid := headOID
		if !hasHead {
			oid = git.ZeroOID
		}
		items = append(items, git.StrPkt(oid+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			items = append(items, git.StrPkt(ref.OID+" "+ref.Name+"\n"))
			if peeled, ok := peelTag(repo, ref.OID); ok {
				items = append(items, git.StrPkt(peeled+" "+ref.Name+"^{}\n"))
			}
		}
	}
	items = append(items, git.FlushPkt())
	b, err := git.EncodePktLines(items)
	if err != nil {
		panic(err)
	}
	return b
}

// V0ReceiveAdvertisement builds the v0 receive-pack advertisement body.
func V0ReceiveAdvertisement(repo *store.Repo) []byte {
	caps := "report-status report-status-v2 side-band-64k delete-refs ofs-delta"
	items := []git.Pkt{git.StrPkt("# service=git-receive-pack\n"), git.FlushPkt()}
	refs := SortedRefs(repo)
	headOID, hasHead := repo.Refs[headTarget(repo)]
	if len(refs) == 0 {
		items = append(items, git.StrPkt(git.ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		oid := headOID
		if !hasHead {
			oid = git.ZeroOID
		}
		items = append(items, git.StrPkt(oid+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			items = append(items, git.StrPkt(ref.OID+" "+ref.Name+"\n"))
			if peeled, ok := peelTag(repo, ref.OID); ok {
				items = append(items, git.StrPkt(peeled+" "+ref.Name+"^{}\n"))
			}
		}
	}
	items = append(items, git.FlushPkt())
	b, err := git.EncodePktLines(items)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Memoization (gitproto.ts 113-201; map §5.5)
//
// All memo layers are pure functions of (refs, objects, args): refs are
// content-addressed, objects are immutable and only added, so a key built
// from the sorted refs + the object-store size is exact. Nothing is ever
// explicitly invalidated — any mutation changes the key. Caches live in a
// package-level map keyed by repo pointer (Go has no WeakMap); entries are
// bounded (4–8 each) and a deleted repo's entries are small and few.
// Callers hold the store lock, so no locking here (see package doc).
// ---------------------------------------------------------------------------

// memoCache is a bounded FIFO map: insertion-ordered eviction of the
// oldest key, mirroring JS Map (memoSet in gitproto.ts). Re-setting an
// existing key does not move it.
//
// The cache is self-synchronizing (its own RWMutex): memo writes happen on
// the read path (lazy caching), so the cache must be safe when the caller
// holds only the store RLock. Lock order: store lock first, then the
// cache mutex — never the reverse.
type memoCache[V any] struct {
	mu    sync.RWMutex
	m     map[string]V
	order []string
	limit int
}

func newMemoCache[V any](limit int) *memoCache[V] {
	return &memoCache[V]{m: make(map[string]V), limit: limit}
}

func (c *memoCache[V]) get(key string) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *memoCache[V]) set(key string, value V) V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	c.m[key] = value
	for len(c.m) > c.limit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
	return value
}

// repoMemos holds one repo's memo caches.
type repoMemos struct {
	plans      *memoCache[*FetchPlan]
	ads        *memoCache[[]byte]
	lsRefs     *memoCache[[]byte]
	uploadPack *memoCache[[]byte]
	fetch      *memoCache[[]byte] // built fetch responses (§5.5), bounded 8
}

var allMemos = map[*store.Repo]*repoMemos{}
var allMemosMu sync.RWMutex

func memosFor(repo *store.Repo) *repoMemos {
	allMemosMu.RLock()
	m, ok := allMemos[repo]
	allMemosMu.RUnlock()
	if ok {
		return m
	}
	allMemosMu.Lock()
	defer allMemosMu.Unlock()
	if m, ok := allMemos[repo]; ok {
		return m
	}
	m = &repoMemos{
		plans:      newMemoCache[*FetchPlan](8),
		ads:        newMemoCache[[]byte](4),
		lsRefs:     newMemoCache[[]byte](8),
		uploadPack: newMemoCache[[]byte](8),
		fetch:      newMemoCache[[]byte](8),
	}
	allMemos[repo] = m
	return m
}

// refsKey is the exact fingerprint of the refs a response depends on:
// HEAD target + sorted name=oid pairs.
func refsKey(repo *store.Repo) string {
	var b strings.Builder
	b.WriteString(headTarget(repo))
	b.WriteString("|")
	for i, r := range SortedRefs(repo) {
		if i > 0 {
			b.WriteString(";")
		}
		b.WriteString(r.Name)
		b.WriteString("=")
		b.WriteString(r.OID)
	}
	return b.String()
}

// V0UploadAdvertisementCached returns the memoised v0 upload-pack
// advertisement (key `up|<refsKey>`, limit 4, shared with receive).
func V0UploadAdvertisementCached(repo *store.Repo) []byte {
	cache := memosFor(repo).ads
	key := "up|" + refsKey(repo)
	if hit, ok := cache.get(key); ok {
		return hit
	}
	return cache.set(key, V0UploadAdvertisement(repo))
}

// V0ReceiveAdvertisementCached returns the memoised v0 receive-pack
// advertisement (key `recv|<refsKey>`, limit 4, shared with upload).
func V0ReceiveAdvertisementCached(repo *store.Repo) []byte {
	cache := memosFor(repo).ads
	key := "recv|" + refsKey(repo)
	if hit, ok := cache.get(key); ok {
		return hit
	}
	return cache.set(key, V0ReceiveAdvertisement(repo))
}

// LsRefsResponseCached returns the memoised ls-refs response (limit 8).
func LsRefsResponseCached(repo *store.Repo, args LsRefsArgs) []byte {
	cache := memosFor(repo).lsRefs
	key := "ls|" + boolBit(args.Peel) + boolBit(args.Symrefs) + boolBit(args.Unborn) +
		"|" + strings.Join(args.Prefixes, ",") + "|" + refsKey(repo)
	if hit, ok := cache.get(key); ok {
		return hit
	}
	return cache.set(key, LsRefsResponse(repo, args))
}

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// PlanFetchCached returns the memoised fetch plan (limit 8). Plans are
// never mutated after construction.
func PlanFetchCached(repo *store.Repo, args FetchArgs) *FetchPlan {
	cache := memosFor(repo).plans
	depth := ""
	if args.Depth != nil {
		depth = strconv.Itoa(*args.Depth)
	}
	key := strings.Join(args.Wants, ",") + "|" + strings.Join(args.Haves, ",") + "|" +
		strings.Join(args.Shallow, ",") + "|" + depth + "|" + boolBit(args.Done) + "|" +
		strconv.Itoa(len(repo.Objects))
	if hit, ok := cache.get(key); ok {
		return hit
	}
	return cache.set(key, PlanFetch(repo, args))
}

// ---------------------------------------------------------------------------
// Whole-request memo for git-upload-pack POSTs (gitproto.ts 175-201)
// ---------------------------------------------------------------------------

// UploadPackMemoKey returns the memo key for a git-upload-pack POST body,
// or ("", false) when the body is over 4 KiB (Node: null). The body is
// keyed byte for byte: a Go string holds arbitrary bytes, so string(body)
// is the same latin1 bijection Node uses.
func UploadPackMemoKey(repo *store.Repo, body []byte, isV2 bool) (string, bool) {
	if len(body) > 4096 {
		return "", false
	}
	ver := "0"
	if isV2 {
		ver = "2"
	}
	return ver + "|" + refsKey(repo) + "|" + strconv.Itoa(len(repo.Objects)) + "|" + string(body), true
}

// UploadPackMemoGet returns a memoised upload-pack response body, if any.
func UploadPackMemoGet(repo *store.Repo, body []byte, isV2 bool) ([]byte, bool) {
	key, ok := UploadPackMemoKey(repo, body, isV2)
	if !ok {
		return nil, false
	}
	return memosFor(repo).uploadPack.get(key)
}

// UploadPackMemoSet memoises an upload-pack response body. Responses over
// 256 KiB are never cached; 8 entries per repo, FIFO eviction.
func UploadPackMemoSet(repo *store.Repo, body []byte, isV2 bool, response []byte) {
	key, ok := UploadPackMemoKey(repo, body, isV2)
	if !ok || len(response) > 262144 {
		return
	}
	memosFor(repo).uploadPack.set(key, response)
}

// ---------------------------------------------------------------------------
// v2 ls-refs (gitproto.ts 227-297)
// ---------------------------------------------------------------------------

// LsRefsArgs are the parsed arguments of a v2 ls-refs request.
type LsRefsArgs struct {
	Peel     bool
	Symrefs  bool
	Unborn   bool
	Prefixes []string
}

// ParseLsRefsArgs parses ls-refs arguments from decoded pkt-lines.
// Lines other than peel/symrefs/unborn/ref-prefix (command=ls-refs,
// agent=..., object-format=..., delim) are ignored.
func ParseLsRefsArgs(packets []git.Pkt) LsRefsArgs {
	args := LsRefsArgs{}
	for _, p := range packets {
		if p.Kind != git.PktData {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		switch {
		case line == "peel":
			args.Peel = true
		case line == "symrefs":
			args.Symrefs = true
		case line == "unborn":
			args.Unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.Prefixes = append(args.Prefixes, line[len("ref-prefix "):])
		}
	}
	return args
}

// LsRefsResponse builds the v2 ls-refs response body. Note the symrefs
// flag adds nothing to branch lines (dead branch in gitproto.ts —
// replicated): only the HEAD line carries symref-target:.
func LsRefsResponse(repo *store.Repo, args LsRefsArgs) []byte {
	include := func(name string) bool {
		if len(args.Prefixes) == 0 {
			return true
		}
		for _, prefix := range args.Prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var lines []git.Pkt
	headRef := headTarget(repo)
	headOID, hasHead := repo.Refs[headRef]
	if include("HEAD") {
		if hasHead {
			s := headOID + " HEAD"
			if args.Symrefs {
				s += " symref-target:" + headRef
			}
			lines = append(lines, git.StrPkt(s+"\n"))
		} else if args.Unborn {
			s := "unborn HEAD"
			if args.Symrefs {
				s += " symref-target:" + headRef
			}
			lines = append(lines, git.StrPkt(s+"\n"))
		}
	}
	for _, ref := range SortedRefs(repo) {
		if !include(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if args.Peel {
			if peeled, ok := peelTag(repo, ref.OID); ok {
				line += " peeled:" + peeled
			}
		}
		lines = append(lines, git.StrPkt(line+"\n"))
	}
	lines = append(lines, git.FlushPkt())
	b, err := git.EncodePktLines(lines)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// fetch (gitproto.ts 299-508)
// ---------------------------------------------------------------------------

// FetchArgs are the parsed arguments of a fetch request (v2 or v0).
// Depth is nil when no deepen line was sent. Done is never set for v0.
type FetchArgs struct {
	Wants   []string
	Haves   []string
	Shallow []string
	Depth   *int
	Done    bool
}

// ParseFetchArgs parses v2 fetch arguments from decoded pkt-lines.
// (thin-pack, no-progress, ofs-delta, filter, agent, object-format
// lines are ignored.)
func ParseFetchArgs(packets []git.Pkt) FetchArgs {
	args := FetchArgs{}
	for _, p := range packets {
		if p.Kind != git.PktData {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			args.Wants = append(args.Wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			args.Haves = append(args.Haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.Shallow = append(args.Shallow, strings.TrimSpace(line[8:]))
		case line == "done":
			args.Done = true
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseJSInt(line[7:]); ok {
				args.Depth = &n
			}
		}
	}
	return args
}

// ParseV0FetchArgs parses v0 fetch arguments from the raw request body.
// A want line's first whitespace-separated token must be an oid (else
// ignored — this is how the capability suffix on the first want line is
// skipped). Done is never set for v0.
func ParseV0FetchArgs(body []byte) (FetchArgs, error) {
	args := FetchArgs{}
	packets, err := git.DecodePktLines(body)
	if err != nil {
		return args, err
	}
	for _, p := range packets {
		if p.Kind != git.PktData {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[5:])
			oid := rest
			if i := strings.IndexByte(rest, ' '); i >= 0 {
				oid = rest[:i]
			}
			if git.IsOID(oid) {
				args.Wants = append(args.Wants, oid)
			}
		case strings.HasPrefix(line, "have "):
			args.Haves = append(args.Haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			args.Shallow = append(args.Shallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseJSInt(line[7:]); ok {
				args.Depth = &n
			}
		}
	}
	return args, nil
}

// parseJSInt mirrors JS parseInt(s, 10) for the deepen values the client
// sends: leading whitespace skipped, optional sign, leading digits;
// no digits -> not ok.
func parseJSInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	if s == "" {
		return 0, false
	}
	i := 0
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i = 1
	}
	n := 0
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		digits++
		i++
	}
	if digits == 0 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// FetchPlan is the result of PlanFetch: which commits/objects to send
// and which shallow boundaries to report.
type FetchPlan struct {
	// Commits to send (with their trees/blobs).
	Commits map[string]struct{}
	// Objects (any type) to send, as oids.
	Objects map[string]struct{}
	// Commits to report as shallow boundaries.
	ShallowOut []string
	// Former shallow boundaries that are now fully present.
	UnshallowOut []string
}

// PlanFetch works out which objects a fetch must send (gitproto.ts
// 347-447). Steps: (1) BFS from wants bounded by depth (tag wants peel
// to their target at the same distance); (2) exclusion — no depth:
// remove everything reachable from haves; depth: remove only the have
// commits themselves; (3) objects — for each included commit its tree
// and blobs recursively, parents NOT followed; tag objects named by
// wants are included; then subtract the haveSet (a walk from haves that
// stops at included commits); (4) shallow/unshallow bookkeeping.
func PlanFetch(repo *store.Repo, args FetchArgs) *FetchPlan {
	lookup := func(oid string) *git.GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
	// 1. Included commits: BFS from wants, bounded by depth.
	included := map[string]struct{}{}
	dist := map[string]int{}
	var queue []string
	for _, w := range args.Wants {
		if !git.IsOID(w) {
			continue
		}
		if _, ok := dist[w]; !ok {
			dist[w] = 0
			queue = append(queue, w)
		}
	}
	for head := 0; head < len(queue); head++ {
		cur := queue[head]
		d := dist[cur]
		obj := lookup(cur)
		if obj == nil {
			continue
		}
		if obj.Type == git.ObjTag {
			target := git.ParseTag(obj.Data).Object
			if _, ok := dist[target]; !ok {
				dist[target] = d
				queue = append(queue, target)
			}
			continue
		}
		if obj.Type != git.ObjCommit {
			continue
		}
		if args.Depth != nil && d >= *args.Depth {
			continue
		}
		included[cur] = struct{}{}
		for _, p := range git.ParseCommit(obj.Data).Parents {
			if _, ok := dist[p]; !ok {
				dist[p] = d + 1
				queue = append(queue, p)
			}
		}
	}
	// 2. Exclusion.
	if args.Depth == nil {
		for oid := range git.CollectReachable(args.Haves, lookup) {
			delete(included, oid)
		}
	} else {
		for _, h := range args.Haves {
			delete(included, h)
		}
	}
	// 3. Objects.
	objects := map[string]struct{}{}
	var stack []string
	// Seed in sorted order (the resulting set is order-independent).
	for oid := range included {
		stack = append(stack, oid)
	}
	sort.Strings(stack)
	// Tag objects that point at included commits (wants may be tags).
	for _, w := range args.Wants {
		if obj := lookup(w); obj != nil && obj.Type == git.ObjTag {
			objects[w] = struct{}{}
			stack = append(stack, git.ParseTag(obj.Data).Object)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := objects[oid]; ok {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			continue
		}
		objects[oid] = struct{}{}
		switch obj.Type {
		case git.ObjCommit:
			// Parents are NOT pushed: only included commits are sent.
			stack = append(stack, git.ParseCommit(obj.Data).Tree)
		case git.ObjTree:
			entries, err := git.ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				stack = append(stack, e.OID)
			}
		case git.ObjTag:
			stack = append(stack, git.ParseTag(obj.Data).Object)
		}
	}
	// Subtract objects the client already holds (reachable from the
	// haves without passing through an included commit).
	haveSet := map[string]struct{}{}
	{
		haveStack := append([]string(nil), args.Haves...)
		seen := map[string]struct{}{}
		for len(haveStack) > 0 {
			oid := haveStack[len(haveStack)-1]
			haveStack = haveStack[:len(haveStack)-1]
			if _, ok := seen[oid]; ok {
				continue
			}
			if _, ok := included[oid]; ok {
				continue
			}
			obj := lookup(oid)
			if obj == nil {
				continue
			}
			seen[oid] = struct{}{}
			haveSet[oid] = struct{}{}
			switch obj.Type {
			case git.ObjCommit:
				c := git.ParseCommit(obj.Data)
				haveStack = append(haveStack, c.Tree)
				haveStack = append(haveStack, c.Parents...)
			case git.ObjTree:
				entries, err := git.ParseTree(obj.Data)
				if err != nil {
					continue
				}
				for _, e := range entries {
					haveStack = append(haveStack, e.OID)
				}
			case git.ObjTag:
				haveStack = append(haveStack, git.ParseTag(obj.Data).Object)
			}
		}
	}
	for oid := range haveSet {
		delete(objects, oid)
	}
	// 4. Shallow bookkeeping.
	var shallowOut, unshallowOut []string
	for oid := range included {
		obj := lookup(oid)
		if obj == nil || obj.Type != git.ObjCommit {
			continue
		}
		for _, p := range git.ParseCommit(obj.Data).Parents {
			if _, ok := included[p]; !ok {
				shallowOut = append(shallowOut, oid)
				break
			}
		}
	}
	for _, b := range args.Shallow {
		if _, ok := included[b]; ok {
			continue // still shallow, or handled above
		}
		obj := lookup(b)
		if obj == nil || obj.Type != git.ObjCommit {
			continue
		}
		parents := git.ParseCommit(obj.Data).Parents
		if len(parents) == 0 {
			unshallowOut = append(unshallowOut, b)
			continue
		}
		all := true
		for _, p := range parents {
			if _, ok := included[p]; !ok {
				all = false
				break
			}
		}
		if all {
			unshallowOut = append(unshallowOut, b)
		}
	}
	sort.Strings(shallowOut)
	sort.Strings(unshallowOut)
	return &FetchPlan{Commits: included, Objects: objects, ShallowOut: shallowOut, UnshallowOut: unshallowOut}
}

// sidebandFrame splits data into sideband band-1 pkt-lines (32,768-byte
// chunks), used for packs and for the push report body.
func sidebandFrame(data []byte) []byte {
	var parts []byte
	const chunk = 32768
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		payload := make([]byte, 0, 1+end-off)
		payload = append(payload, 1)
		payload = append(payload, data[off:end]...)
		b, err := git.EncodePktLine(payload)
		if err != nil {
			panic(err)
		}
		parts = append(parts, b...)
	}
	return parts
}

// writePackForPlan builds the pack for a fetch plan: objects sorted by
// oid asc, no deltas.
func writePackForPlan(repo *store.Repo, plan *FetchPlan) []byte {
	sorted := make([]string, 0, len(plan.Objects))
	for oid := range plan.Objects {
		sorted = append(sorted, oid)
	}
	sort.Strings(sorted)
	objs := make([]git.GitObject, 0, len(sorted))
	for _, oid := range sorted {
		if obj, ok := repo.Objects[oid]; ok {
			objs = append(objs, obj)
		}
	}
	pack, err := git.WritePack(objs)
	if err != nil {
		panic(err)
	}
	return pack
}

// fetchCacheKey is the key for a built fetch response (gitproto.ts
// 520-524). The pack bytes are content-addressed, the shallow lists come
// from the plan, and the store only grows — the same key with the same
// store size is the same response, byte for byte.
func fetchCacheKey(repo *store.Repo, plan *FetchPlan, version string, includeAcks bool) string {
	objs := make([]string, 0, len(plan.Objects))
	for oid := range plan.Objects {
		objs = append(objs, oid)
	}
	sort.Strings(objs)
	acks := "0"
	if includeAcks {
		acks = "1"
	}
	return version + "|" + acks + "|" + strings.Join(objs, ",") + "|" +
		strings.Join(plan.ShallowOut, ",") + "|" + strings.Join(plan.UnshallowOut, ",") + "|" +
		strconv.Itoa(len(repo.Objects))
}

// cacheFetchResponse memoises a built fetch response on the repo (limit 8
// per repo, bodies ≤ 256 KiB only), with FIFO eviction matching Node's Map.
func cacheFetchResponse(repo *store.Repo, key string, body []byte) []byte {
	if len(body) <= 262144 {
		memosFor(repo).fetch.set(key, body)
	}
	return body
}

// V2FetchResponse builds the v2 fetch response body for a plan. Whether
// the acknowledgments section is sent depends on the request: when the
// client sent `done`, it goes straight to reading the packfile and never
// expects an acknowledgments section (a real git client dies with
// "expected 'packfile', received 'acknowledgments'"). Call with
// includeAcks = !args.Done (app.ts).
func V2FetchResponse(repo *store.Repo, plan *FetchPlan, includeAcks bool) []byte {
	key := fetchCacheKey(repo, plan, "v2", includeAcks)
	if hit, ok := memosFor(repo).fetch.get(key); ok {
		return hit
	}
	var parts []byte
	if includeAcks {
		parts = append(parts, mustPktLine("acknowledgments\n")...)
		parts = append(parts, mustPktLine("NAK\n")...)
		parts = append(parts, mustPktLine("ready\n")...)
		parts = append(parts, git.PKTDelim...)
	}
	if len(plan.ShallowOut) > 0 || len(plan.UnshallowOut) > 0 {
		parts = append(parts, mustPktLine("shallow-info\n")...)
		for _, oid := range plan.ShallowOut {
			parts = append(parts, mustPktLine("shallow "+oid+"\n")...)
		}
		for _, oid := range plan.UnshallowOut {
			parts = append(parts, mustPktLine("unshallow "+oid+"\n")...)
		}
		parts = append(parts, git.PKTDelim...)
	}
	parts = append(parts, mustPktLine("packfile\n")...)
	parts = append(parts, sidebandFrame(writePackForPlan(repo, plan))...)
	parts = append(parts, git.PKTFlush...)
	return cacheFetchResponse(repo, key, parts)
}

// V0FetchResponse builds the v0 fetch response body for a plan:
// `NAK`, shallow / unshallow lines, sideband pack, flush. No `packfile`
// line, no acks section.
func V0FetchResponse(repo *store.Repo, plan *FetchPlan) []byte {
	key := fetchCacheKey(repo, plan, "v0", false)
	if hit, ok := memosFor(repo).fetch.get(key); ok {
		return hit
	}
	var parts []byte
	parts = append(parts, mustPktLine("NAK\n")...)
	for _, oid := range plan.ShallowOut {
		parts = append(parts, mustPktLine("shallow "+oid+"\n")...)
	}
	for _, oid := range plan.UnshallowOut {
		parts = append(parts, mustPktLine("unshallow "+oid+"\n")...)
	}
	parts = append(parts, sidebandFrame(writePackForPlan(repo, plan))...)
	parts = append(parts, git.PKTFlush...)
	return cacheFetchResponse(repo, key, parts)
}

// ---------------------------------------------------------------------------
// git-upload-pack POST entry point (app.ts 387-447, minus the
// whole-response memo, which lives in the server layer — see
// UploadPackMemoKey/Get/Set)
// ---------------------------------------------------------------------------

// ErrUnknownCommand is returned by UploadPackBody when a git-upload-pack
// body carries no recognised command. The server answers it with
// HTTP 400 "Unknown git-upload-pack command" (app.ts).
var ErrUnknownCommand = errors.New("Unknown git-upload-pack command")

// UploadPackBody processes a POST git-upload-pack body and returns the
// response body bytes. A body-memo hit returns without any pkt parsing.
// A decode error is returned as-is: in app.ts it propagates out of the
// handler (HTTP 500 with the message), unlike receive-pack errors, which
// are answered 400 — the server must map the two cases differently.
func UploadPackBody(repo *store.Repo, body []byte, isV2 bool) ([]byte, error) {
	if hit, ok := UploadPackMemoGet(repo, body, isV2); ok {
		return hit, nil
	}
	packets, err := git.DecodePktLines(body)
	if err != nil {
		return nil, err
	}
	command := ""
	for _, p := range packets {
		if p.Kind == git.PktData {
			command = strings.TrimSpace(string(p.Payload))
			break
		}
	}
	var response []byte
	if isV2 && strings.HasPrefix(command, "command=ls-refs") {
		args := ParseLsRefsArgs(packets)
		response = LsRefsResponseCached(repo, args)
	} else if isV2 && strings.HasPrefix(command, "command=fetch") {
		args := ParseFetchArgs(packets)
		plan := PlanFetchCached(repo, args)
		response = V2FetchResponse(repo, plan, !args.Done)
	} else if !isV2 {
		args, err := ParseV0FetchArgs(body)
		if err != nil {
			return nil, err
		}
		plan := PlanFetchCached(repo, args)
		response = V0FetchResponse(repo, plan)
	} else {
		return nil, ErrUnknownCommand
	}
	UploadPackMemoSet(repo, body, isV2, response)
	return response, nil
}

// ---------------------------------------------------------------------------
// receive-pack (gitproto.ts 511-668)
// ---------------------------------------------------------------------------

// PushCommand is one ref update from a receive-pack body.
type PushCommand struct {
	OldOID string
	NewOID string
	Ref    string
}

// PushResult is one per-ref report line, in command order.
type PushResult struct {
	Ref    string
	OK     bool
	Reason string // "" when OK; the only reason ever produced is "non-fast-forward"
}

// ParsePushCommands parses push commands from decoded pkt-lines. Per
// data packet: strip one trailing newline, cut at the first NUL
// (capabilities on the first command line), split on single spaces —
// must be exactly 3 parts `<old> <new> <ref>`, both oids, ref starting
// `refs/`, else `bad push command ...` (first 60 chars in the message).
func ParsePushCommands(packets []git.Pkt) ([]PushCommand, error) {
	var commands []PushCommand
	for _, p := range packets {
		if p.Kind != git.PktData {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			return nil, fmt.Errorf("bad push command %s", strconv.Quote(first60(line)))
		}
		oldOID, newOID, ref := parts[0], parts[1], parts[2]
		if !git.IsOID(oldOID) || !git.IsOID(newOID) || !strings.HasPrefix(ref, "refs/") {
			return nil, fmt.Errorf("bad push command %s", strconv.Quote(first60(line)))
		}
		commands = append(commands, PushCommand{OldOID: oldOID, NewOID: newOID, Ref: ref})
	}
	return commands, nil
}

func first60(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// SplitAndParsePush splits a receive-pack body into its command
// pkt-lines and trailing pack bytes, then parses the commands. Any error
// maps to HTTP 400 "Malformed request" in app.ts.
func SplitAndParsePush(body []byte) ([]PushCommand, []byte, error) {
	packets, pack, err := git.SplitCommandsAndPack(body)
	if err != nil {
		return nil, nil, err
	}
	commands, err := ParsePushCommands(packets)
	if err != nil {
		return nil, nil, err
	}
	return commands, pack, nil
}

// ReadPushPack reads the pack bytes of a receive-pack body. An empty pack
// returns (nil, nil) — no objects. A read error is reported in-band by
// the caller (HTTP 200 with pushReportBody(false, detail, []), app.ts).
func ReadPushPack(repo *store.Repo, pack []byte) (map[string]git.GitObject, error) {
	if len(pack) == 0 {
		return nil, nil
	}
	res, err := git.ReadPack(pack, func(oid string) *git.GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res.Objects, nil
}

// ApplyPush applies a push: stores the pack's objects first (keyed by
// the oid ReadPack computed — no re-hash), then validates and applies
// each ref update in order. Returns the per-ref report lines (in command
// order). A push with one ok + one ng ref applies the ok one (mixed
// report). The only reason string ever produced is "non-fast-forward".
func ApplyPush(repo *store.Repo, commands []PushCommand, packObjects map[string]git.GitObject) []PushResult {
	if packObjects != nil {
		for oid, obj := range packObjects {
			if _, ok := repo.Objects[oid]; !ok {
				repo.Objects[oid] = obj
			}
		}
	}
	lookup := func(oid string) *git.GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
	results := make([]PushResult, 0, len(commands))
	for _, cmd := range commands {
		current, hasCurrent := repo.Refs[cmd.Ref]
		if cmd.NewOID == git.ZeroOID {
			// Delete.
			if !hasCurrent {
				results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			} else if cmd.OldOID != git.ZeroOID && cmd.OldOID != current {
				results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.Ref)
				results = append(results, PushResult{Ref: cmd.Ref, OK: true})
			}
			continue
		}
		// Update/create: the new object must exist.
		if _, ok := repo.Objects[cmd.NewOID]; !ok {
			results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			continue
		}
		if !hasCurrent {
			if cmd.OldOID != git.ZeroOID {
				results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
				continue
			}
			repo.Refs[cmd.Ref] = cmd.NewOID
			results = append(results, PushResult{Ref: cmd.Ref, OK: true})
			continue
		}
		if cmd.OldOID != current {
			results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			continue
		}
		if cmd.NewOID == current {
			results = append(results, PushResult{Ref: cmd.Ref, OK: true})
			continue
		}
		// Fast path for the common linear push: the new commit's parent
		// is the current tip, so it is a fast-forward without walking
		// the graph. Every other shape falls through to the general
		// check below; a tag newOid never takes this branch.
		fastOK := false
		if newObj, ok := repo.Objects[cmd.NewOID]; ok && newObj.Type == git.ObjCommit {
			for _, p := range git.ParseCommit(newObj.Data).Parents {
				if p == current {
					fastOK = true
					break
				}
			}
		}
		if fastOK {
			repo.Refs[cmd.Ref] = cmd.NewOID
			results = append(results, PushResult{Ref: cmd.Ref, OK: true})
			continue
		}
		newCommit, newOK := peelToCommit(repo, cmd.NewOID)
		if !newOK || !git.IsAncestor(current, newCommit, lookup) {
			// A non-commit ref update (e.g. a tag): allow only if the
			// old value is an ancestor of the new value.
			oldPeeled, oldOK := peelToCommit(repo, current)
			newPeeled, newPeeledOK := peelToCommit(repo, cmd.NewOID)
			if oldOK && newPeeledOK && git.IsAncestor(oldPeeled, newPeeled, lookup) {
				repo.Refs[cmd.Ref] = cmd.NewOID
				results = append(results, PushResult{Ref: cmd.Ref, OK: true})
			} else {
				results = append(results, PushResult{Ref: cmd.Ref, Reason: "non-fast-forward"})
			}
			continue
		}
		repo.Refs[cmd.Ref] = cmd.NewOID
		results = append(results, PushResult{Ref: cmd.Ref, OK: true})
	}
	return results
}

// PushReportBody builds the sideband-framed report-status body for a
// push: inner pkt-lines `unpack ok` (or `unpack <detail>`) + `ok <ref>` /
// `ng <ref> <reason>` per result + flush, the whole inner block
// sideband-framed (band 1, 32 KiB chunks) + final flush. A failed unpack
// still returns HTTP 200 with `unpack <detail>` (app.ts).
func PushReportBody(unpackOK bool, unpackDetail string, results []PushResult) []byte {
	inner := []git.Pkt{}
	if unpackOK {
		inner = append(inner, git.StrPkt("unpack ok\n"))
	} else {
		inner = append(inner, git.StrPkt("unpack "+unpackDetail+"\n"))
	}
	for _, r := range results {
		if r.OK {
			inner = append(inner, git.StrPkt("ok "+r.Ref+"\n"))
		} else {
			inner = append(inner, git.StrPkt("ng "+r.Ref+" "+r.Reason+"\n"))
		}
	}
	inner = append(inner, git.FlushPkt())
	innerBytes, err := git.EncodePktLines(inner)
	if err != nil {
		panic(err)
	}
	out := sidebandFrame(innerBytes)
	out = append(out, git.PKTFlush...)
	return out
}

// PushReportBody builds the sideband-framed report-status body for a
