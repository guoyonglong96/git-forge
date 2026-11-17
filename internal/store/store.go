// Package store is the forge's in-memory store and domain operations
// (repos, git objects, issues, pull requests, contents). No I/O, no
// persistence. Deterministic: every timestamp derives from FixedISO /
// FixedEpoch. Ported from store.ts (499 lines).
//
// LOCKING: Node is single-threaded so store.ts locks nothing. In Go the
// Store carries a sync.RWMutex, and every method in this package MUST be
// called with the store lock held (server.HandleRequest acquires RLock
// for read-only requests, Lock for mutating ones). Methods here do NOT
// lock internally, so internal composition (e.g. MergePull calling
// PullConflict) cannot deadlock. Per-issue lazy caches (CachedJSON,
// CachedFullResponse) have their own mutex (Issue.cacheMu) because they
// are written on the read path; gitproto/server memo caches likewise
// carry their own mutexes. Lock order: store lock first, then a cache
// mutex — never the reverse.
package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"forge/internal/git"
)

// Viewer is the hardcoded authenticated user (store.ts VIEWER). It is never
// derived from the scenario.
var Viewer = struct {
	ID    int
	Login string
	Name  string
	Email string
}{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}

var fixedISONowRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

// FixedISO is MAKEFUN_TRIAL_NOW when it matches the validated shape, else
// the default '2025-01-01T00:00:00Z' (store.ts:21-27). Computed once at
// startup; never call time.Now() in a request path.
var FixedISO = func() string {
	now := os.Getenv("MAKEFUN_TRIAL_NOW")
	if now != "" && fixedISONowRe.MatchString(now) {
		return now
	}
	return "2025-01-01T00:00:00Z"
}()

// FixedEpoch is floor(Date.parse(FIXED_ISO)/1000) — 1735689600 by default.
// Falls back to the default epoch if the override fails to parse (Node
// would produce NaN here; the env never sets the variable in trials, so
// this is unobservable — see map N1).
var FixedEpoch = func() int64 {
	if t, err := time.Parse(time.RFC3339, FixedISO); err == nil {
		return t.Unix()
	}
	return 1735689600
}()

// WHO is the commit author/committer identity line.
var WHO = Viewer.Name + " <" + Viewer.Email + ">"

// GlobalID returns base64("<kind>:<id>") (standard base64, with padding).
func GlobalID(kind string, id interface{}) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%v", kind, id)))
}

// DecodedGlobalID is the result of DecodeGlobalID.
type DecodedGlobalID struct {
	Kind string
	ID   string
}

// DecodeGlobalID reverses GlobalID, returning nil on any failure (matching
// Node's try/catch). Node's base64 decoder is lenient about padding, so
// RawStdEncoding is tried as a fallback.
func DecodeGlobalID(id string) *DecodedGlobalID {
	var text []byte
	var err error
	text, err = base64.StdEncoding.DecodeString(id)
	if err != nil {
		text, err = base64.RawStdEncoding.DecodeString(id)
		if err != nil {
			return nil
		}
	}
	i := bytes.IndexByte(text, ':')
	if i < 0 {
		return nil
	}
	return &DecodedGlobalID{Kind: string(text[:i]), ID: string(text[i+1:])}
}

// DomainError is a fallible domain operation's error. Status is 0 when the
// caller decides the HTTP status (createRepo/createIssue/createPull); it is
// set (e.g. 405) when the store prescribes it (MergePull).
type DomainError struct {
	Status int
	Msg    string
}

func (e *DomainError) Error() string { return e.Msg }

// Label is a repo label.
type Label struct {
	Name        string
	Color       string
	Description string
}

// IssueComment is a comment on an issue.
type IssueComment struct {
	ID        int
	Body      string
	User      string // login
	CreatedAt string
}

// Issue is a repo issue.
type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []*Label
	User      string // login
	CreatedAt string
	UpdatedAt string
	Seq       int
	Comments  []*IssueComment

	// CachedJSON is the serialised issueJson, cached until invalidated.
	// Empty means absent (an issue JSON is never the empty string).
	CachedJSON string
	// CachedFullResponse is the whole GET-single-issue HTTP response bytes
	// (status line + headers + CachedJSON), cached beside CachedJSON.
	// Nil means absent. Carries the constant MEMO_REQUEST_ID.
	CachedFullResponse []byte

	// cacheMu guards CachedJSON and CachedFullResponse. These are written
	// on the read path (lazy caching), so they need their own mutex once
	// reads run under the store RLock (which forbids writes). Lock order:
	// store lock first, then cacheMu — never the reverse.
	cacheMu sync.Mutex
}

// Pull is a repo pull request.
type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	Head           PullRef
	Base           PullRef
	User           string // login
	CreatedAt      string
	Seq            int
}

// PullRef is one side of a pull request (branch name + tip SHA snapshot at
// creation — not re-resolved later).
type PullRef struct {
	Ref string
	Sha string
}

// Repo is a repository.
type Repo struct {
	ID            int
	Name          string
	Owner         string
	Private       bool
	Description   string
	DefaultBranch string
	Seq           int
	Refs          map[string]string        // full refname -> oid
	Objects       map[string]git.GitObject // oid -> object
	Labels        map[string]*Label        // by name
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	NextNumber    int // SHARED counter: issue numbers AND pull numbers
	NextCommentID int // per-repo comment ids

	// RefsKey caches the memo-key fragment built from the sorted refs
	// (map §9.2 optimisation target); RefsKeyValid is false until built.
	// Unused in Phase 2a.
	RefsKey      string
	RefsKeyValid bool
}

// Store is the single in-memory store. All methods require the lock held
// (see package doc). Use Lock/RLock/Unlock/RUnlock.
type Store struct {
	mu           sync.RWMutex
	Repos        map[string]*Repo // key "owner/name"
	RepoSeq      int
	NextRepoID   int
	NextObjectID int // SHARED global counter for Issue.id AND Pull.id
}

// New returns an empty Store.
func New() *Store {
	return &Store{
		Repos:        map[string]*Repo{},
		NextRepoID:   1,
		NextObjectID: 1,
	}
}

// Lock acquires the store write lock.
func (s *Store) Lock() { s.mu.Lock() }

// Unlock releases the store write lock.
func (s *Store) Unlock() { s.mu.Unlock() }

// RLock acquires the store read lock.
func (s *Store) RLock() { s.mu.RLock() }

// RUnlock releases the store read lock.
func (s *Store) RUnlock() { s.mu.RUnlock() }

// Key returns the repos map key "owner/name".
func (s *Store) Key(owner, name string) string { return owner + "/" + name }

// GetRepo returns the repo or nil.
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[s.Key(owner, name)]
}

// ViewerRepos returns the viewer's repos sorted by creation seq asc.
func (s *Store) ViewerRepos() []*Repo {
	var out []*Repo
	for _, repo := range s.Repos {
		if repo.Owner == Viewer.Login {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// CreateRepo creates a repo under VIEWER.login. Repos can only ever be
// created under the viewer — there is no way to create another owner's repo.
func (s *Store) CreateRepo(name string, isPrivate bool, description, defaultBranch string) (*Repo, error) {
	if name == "" {
		return nil, &DomainError{Msg: "name is required"}
	}
	if _, ok := s.Repos[s.Key(Viewer.Login, name)]; ok {
		return nil, &DomainError{Msg: "Repository already exists"}
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	repo := &Repo{
		ID:            s.NextRepoID,
		Name:          name,
		Owner:         Viewer.Login,
		Private:       isPrivate,
		Description:   description,
		DefaultBranch: defaultBranch,
		Seq:           s.RepoSeq + 1,
		Refs:          map[string]string{},
		Objects:       map[string]git.GitObject{},
		Labels:        map[string]*Label{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		NextNumber:    1,
		NextCommentID: 1,
	}
	s.NextRepoID++
	s.RepoSeq++
	s.Repos[s.Key(Viewer.Login, name)] = repo
	return repo, nil
}

// DeleteRepo removes a repo from the store entirely.
func (s *Store) DeleteRepo(repo *Repo) {
	delete(s.Repos, s.Key(repo.Owner, repo.Name))
}

// Lookup returns an oid -> object resolver for the repo.
func (s *Store) Lookup(repo *Repo) git.Lookup {
	return func(oid string) *git.GitObject {
		if obj, ok := repo.Objects[oid]; ok {
			o := obj
			return &o
		}
		return nil
	}
}

// PutObject stores an object if absent (dedupe) and returns its oid.
func (s *Store) PutObject(repo *Repo, typ git.ObjType, data []byte) string {
	oid := git.HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = git.GitObject{Type: typ, Data: data}
	}
	return oid
}

// CreateCommit creates a commit object in the repo and returns its oid.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	data := git.BuildCommit(tree, parents, message, WHO, FixedEpoch)
	return s.PutObject(repo, git.ObjCommit, data)
}

// TreeMapAt returns the flat path map of the tree at a ref tip (or empty
// when unborn/missing).
func (s *Store) TreeMapAt(repo *Repo, ref string) map[string]git.FlatEntry {
	tip, ok := repo.Refs[ref]
	if !ok {
		return map[string]git.FlatEntry{}
	}
	obj, ok := repo.Objects[tip]
	if !ok {
		return map[string]git.FlatEntry{}
	}
	var tree string
	switch obj.Type {
	case git.ObjCommit:
		tree = git.ParseCommit(obj.Data).Tree
	case git.ObjTag:
		target, ok := repo.Objects[git.ParseTag(obj.Data).Object]
		if ok && target.Type == git.ObjCommit {
			tree = git.ParseCommit(target.Data).Tree
		}
	}
	if tree == "" {
		return map[string]git.FlatEntry{}
	}
	return git.FlattenTree(tree, s.Lookup(repo))
}

// BuildTreeFromMap builds (and stores) a tree from a flat path map and
// returns the root tree oid. Intermediate dirs get mode "40000".
func (s *Store) BuildTreeFromMap(repo *Repo, flat map[string]git.FlatEntry) string {
	type node struct {
		files map[string]git.FlatEntry
		dirs  map[string]*node
	}
	root := &node{files: map[string]git.FlatEntry{}, dirs: map[string]*node{}}
	for path, entry := range flat {
		parts := splitPath(path)
		n := root
		for i := 0; i < len(parts)-1; i++ {
			child, ok := n.dirs[parts[i]]
			if !ok {
				child = &node{files: map[string]git.FlatEntry{}, dirs: map[string]*node{}}
				n.dirs[parts[i]] = child
			}
			n = child
		}
		n.files[parts[len(parts)-1]] = entry
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []git.TreeEntry
		for name, e := range n.files {
			entries = append(entries, git.TreeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, git.TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return s.PutObject(repo, git.ObjTree, git.BuildTree(entries))
	}
	return build(root)
}

func splitPath(path string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	return append(parts, path[start:])
}

// ResolveRef resolves a ref name (branch, full ref, or sha) to a commit
// oid. A bare branch name resolves.
func (s *Store) ResolveRef(repo *Repo, ref string) (string, bool) {
	if git.IsOID(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref, true
		}
		return "", false
	}
	candidates := []string{ref, "refs/heads/" + ref, "refs/tags/" + ref}
	for _, c := range candidates {
		if oid, ok := repo.Refs[c]; ok {
			return s.PeelToCommit(repo, oid)
		}
	}
	return "", false
}

// PeelToCommit follows tags up to 10 hops to a commit oid.
func (s *Store) PeelToCommit(repo *Repo, oid string) (string, bool) {
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

// RefCommit returns the commit oid a ref points at, peeling tags; false
// when the ref is absent.
func (s *Store) RefCommit(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	if !ok {
		return "", false
	}
	return s.PeelToCommit(repo, oid)
}

// ---------------------------------------------------------------- issues

// CreateIssue creates an issue. ALL label names must exist first, else
// nothing is created. Label objects are stored by reference (pointer), so
// a later POST /labels overwrite of the same name does NOT change an
// already-created issue's labels — replicate Node (map N3: no
// invalidation on label POST).
func (s *Store) CreateIssue(repo *Repo, title, body string, labelNames []string) (*Issue, error) {
	labels := make([]*Label, 0, len(labelNames))
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, &DomainError{Msg: fmt.Sprintf("Label %q does not exist", name)}
		}
		labels = append(labels, label)
	}
	number := repo.NextNumber
	repo.NextNumber++
	issue := &Issue{
		ID:        s.NextObjectID,
		Number:    number,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		User:      Viewer.Login,
		CreatedAt: FixedISO,
		UpdatedAt: FixedISO,
		Seq:       number,
		Comments:  []*IssueComment{},
	}
	s.NextObjectID++
	repo.Issues[number] = issue
	return issue, nil
}

// ---------------------------------------------------------------- pulls

// CreatePull creates a pull request. head/base are branch names; their tip
// SHAs are snapshotted at creation (not re-resolved later).
func (s *Store) CreatePull(repo *Repo, title, body, head, base string) (*Pull, error) {
	headSha, ok := s.RefCommit(repo, "refs/heads/"+head)
	if !ok {
		return nil, &DomainError{Msg: fmt.Sprintf("Head branch %q does not exist", head)}
	}
	baseSha, ok := s.RefCommit(repo, "refs/heads/"+base)
	if !ok {
		return nil, &DomainError{Msg: fmt.Sprintf("Base branch %q does not exist", base)}
	}
	for _, pr := range repo.Pulls {
		if pr.State == "open" && pr.Head.Ref == head && pr.Base.Ref == base {
			return nil, &DomainError{Msg: "A pull request already exists for this head and base"}
		}
	}
	number := repo.NextNumber
	repo.NextNumber++
	pull := &Pull{
		ID:             s.NextObjectID,
		Number:         number,
		Title:          title,
		Body:           body,
		State:          "open",
		Merged:         false,
		MergedAt:       nil,
		MergeCommitSha: nil,
		Head:           PullRef{Ref: head, Sha: headSha},
		Base:           PullRef{Ref: base, Sha: baseSha},
		User:           Viewer.Login,
		CreatedAt:      FixedISO,
		Seq:            number,
	}
	s.NextObjectID++
	repo.Pulls[number] = pull
	return pull, nil
}

// DiffStatus is a file's diff status in a pull.
type DiffStatus string

// Diff statuses.
const (
	DiffAdded    DiffStatus = "added"
	DiffRemoved  DiffStatus = "removed"
	DiffModified DiffStatus = "modified"
)

// DiffEntry is one path's diff in a pull.
type DiffEntry struct {
	Status  DiffStatus
	BaseOID *string
	HeadOID *string
}

// DiffPull diffs base tip tree vs head tip tree. NOTE: it uses the PR's
// snapshotted SHAs (pull.base.sha / pull.head.sha), unlike PullConflict
// which uses current branch tips.
func (s *Store) DiffPull(repo *Repo, pull *Pull) map[string]DiffEntry {
	baseTree, _ := s.TreeOfCommit(repo, pull.Base.Sha)
	headTree, _ := s.TreeOfCommit(repo, pull.Head.Sha)
	baseMap := map[string]git.FlatEntry{}
	if baseTree != "" {
		baseMap = git.FlattenTree(baseTree, s.Lookup(repo))
	}
	headMap := map[string]git.FlatEntry{}
	if headTree != "" {
		headMap = git.FlattenTree(headTree, s.Lookup(repo))
	}
	out := map[string]DiffEntry{}
	for path, e := range headMap {
		b, ok := baseMap[path]
		if !ok {
			oid := e.OID
			out[path] = DiffEntry{Status: DiffAdded, BaseOID: nil, HeadOID: &oid}
		} else if b.OID != e.OID {
			boid, hoid := b.OID, e.OID
			out[path] = DiffEntry{Status: DiffModified, BaseOID: &boid, HeadOID: &hoid}
		}
	}
	for path, e := range baseMap {
		if _, ok := headMap[path]; !ok {
			oid := e.OID
			out[path] = DiffEntry{Status: DiffRemoved, BaseOID: &oid, HeadOID: nil}
		}
	}
	return out
}

// TreeOfCommit returns the tree oid of a commit, or ("", false).
func (s *Store) TreeOfCommit(repo *Repo, commitOID string) (string, bool) {
	obj, ok := repo.Objects[commitOID]
	if !ok || obj.Type != git.ObjCommit {
		return "", false
	}
	return git.ParseCommit(obj.Data).Tree, true
}

// PullConflict is the three-way conflict check for merging a pull into its
// base tip. It uses CURRENT branch tips, not the PR's snapshotted SHAs.
func (s *Store) PullConflict(repo *Repo, pull *Pull) bool {
	baseTip, ok1 := s.RefCommit(repo, "refs/heads/"+pull.Base.Ref)
	headTip, ok2 := s.RefCommit(repo, "refs/heads/"+pull.Head.Ref)
	if !ok1 || !ok2 {
		return true
	}
	mb, hasMB := git.MergeBase(baseTip, headTip, s.Lookup(repo))
	baseMap := map[string]git.FlatEntry{}
	if hasMB {
		if tree, ok := s.TreeOfCommit(repo, mb); ok {
			baseMap = git.FlattenTree(tree, s.Lookup(repo))
		}
	}
	headMap := map[string]git.FlatEntry{}
	if tree, ok := s.TreeOfCommit(repo, headTip); ok {
		headMap = git.FlattenTree(tree, s.Lookup(repo))
	}
	tipMap := map[string]git.FlatEntry{}
	if tree, ok := s.TreeOfCommit(repo, baseTip); ok {
		tipMap = git.FlattenTree(tree, s.Lookup(repo))
	}
	paths := map[string]struct{}{}
	for p := range baseMap {
		paths[p] = struct{}{}
	}
	for p := range headMap {
		paths[p] = struct{}{}
	}
	for p := range tipMap {
		paths[p] = struct{}{}
	}
	oidOrNull := func(m map[string]git.FlatEntry, path string) *string {
		if e, ok := m[path]; ok {
			return &e.OID
		}
		return nil
	}
	for path := range paths {
		b, h, t := oidOrNull(baseMap, path), oidOrNull(headMap, path), oidOrNull(tipMap, path)
		if !strPtrEq(h, b) && !strPtrEq(t, b) && !strPtrEq(h, t) {
			return true
		}
	}
	return false
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MergePull squash-merges an open pull request onto its base branch: one
// commit with the HEAD tree, single parent [baseTip], message
// "Merge pull request #<n> from <owner>/<head.ref>". Returns the merge
// commit oid or a DomainError with Status 405.
func (s *Store) MergePull(repo *Repo, pull *Pull) (string, error) {
	if pull.State != "open" {
		return "", &DomainError{Status: 405, Msg: "Pull request is not open"}
	}
	if s.PullConflict(repo, pull) {
		return "", &DomainError{Status: 405, Msg: "Merge conflict"}
	}
	baseTip, ok1 := s.RefCommit(repo, "refs/heads/"+pull.Base.Ref)
	headTip, ok2 := s.RefCommit(repo, "refs/heads/"+pull.Head.Ref)
	if !ok1 || !ok2 {
		return "", &DomainError{Status: 405, Msg: "Branch tip missing"}
	}
	headTree, ok := s.TreeOfCommit(repo, headTip)
	if !ok {
		return "", &DomainError{Status: 405, Msg: "Head tree missing"}
	}
	message := fmt.Sprintf("Merge pull request #%d from %s/%s", pull.Number, repo.Owner, pull.Head.Ref)
	sha := s.CreateCommit(repo, headTree, []string{baseTip}, message)
	repo.Refs["refs/heads/"+pull.Base.Ref] = sha
	pull.State = "closed"
	pull.Merged = true
	mergedAt := FixedISO
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &sha
	return sha, nil
}

// PullMergeable returns 'MERGEABLE' | 'CONFLICTING' for an open pull
// request, 'UNKNOWN' otherwise.
func (s *Store) PullMergeable(repo *Repo, pull *Pull) string {
	if pull.State != "open" {
		return "UNKNOWN"
	}
	if s.PullConflict(repo, pull) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// ---------------------------------------------------------------------------
// REST view helpers (plain JSON shapes, field order matches store.ts)
// ---------------------------------------------------------------------------

// MarshalJSONValue serialises v with encoding/json, field order = struct
// field order, WITHOUT HTML escaping (<, >, & stay literal, as in Node's
// JSON.stringify).
func MarshalJSONValue(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder.Encode appends a newline; strip it.
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// MarshalJSONString is MarshalJSONValue as a string.
func MarshalJSONString(v interface{}) string { return string(MarshalJSONValue(v)) }

// OwnerRef is the {login, id, type} owner object. Type is omitempty so
// the pull head/base user (built with Type unset) marshals as exactly
// {login, id} like Node's pullJson, while the repo owner (Type "User")
// still emits "type":"User". (Phase 2a bug found by the REST/GraphQL
// porters: without omitempty, PullJSON emitted "type":"".)
type OwnerRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type,omitempty"`
}

// UserRef is the {login, id, type} user object in issue/comment/pull JSON.
type UserRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

// RepoView is repoJson's shape.
type RepoView struct {
	ID              int      `json:"id"`
	NodeID          string   `json:"node_id"`
	Name            string   `json:"name"`
	FullName        string   `json:"full_name"`
	Owner           OwnerRef `json:"owner"`
	Private         bool     `json:"private"`
	Description     string   `json:"description"`
	DefaultBranch   string   `json:"default_branch"`
	HTMLURL         string   `json:"html_url"`
	URL             string   `json:"url"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	PushedAt        string   `json:"pushed_at"`
	Fork            bool     `json:"fork"`
	HasIssues       bool     `json:"has_issues"`
	HasProjects     bool     `json:"has_projects"`
	HasWiki         bool     `json:"has_wiki"`
	HasPages        bool     `json:"has_pages"`
	HasDownloads    bool     `json:"has_downloads"`
	Archived        bool     `json:"archived"`
	Disabled        bool     `json:"disabled"`
	Visibility      string   `json:"visibility"`
	OpenIssuesCount int      `json:"open_issues_count"`
	WatchersCount   int      `json:"watchers_count"`
	StargazersCount int      `json:"stargazers_count"`
	ForksCount      int      `json:"forks_count"`
}

// RepoJSON builds repoJson for a repo. open_issues_count is computed live.
func (s *Store) RepoJSON(repo *Repo) RepoView {
	open := 0
	for _, iss := range repo.Issues {
		if iss.State == "open" {
			open++
		}
	}
	vis := "public"
	if repo.Private {
		vis = "private"
	}
	return RepoView{
		ID:              repo.ID,
		NodeID:          GlobalID("Repository", repo.ID),
		Name:            repo.Name,
		FullName:        repo.Owner + "/" + repo.Name,
		Owner:           OwnerRef{Login: repo.Owner, ID: Viewer.ID, Type: "User"},
		Private:         repo.Private,
		Description:     repo.Description,
		DefaultBranch:   repo.DefaultBranch,
		HTMLURL:         "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		URL:             "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		CreatedAt:       FixedISO,
		UpdatedAt:       FixedISO,
		PushedAt:        FixedISO,
		Fork:            false,
		HasIssues:       true,
		HasProjects:     true,
		HasWiki:         true,
		HasPages:        false,
		HasDownloads:    true,
		Archived:        false,
		Disabled:        false,
		Visibility:      vis,
		OpenIssuesCount: open,
		WatchersCount:   0,
		StargazersCount: 0,
		ForksCount:      0,
	}
}

// UserView is userJson's shape.
type UserView struct {
	Login     string `json:"login"`
	ID        int    `json:"id"`
	NodeID    string `json:"node_id"`
	AvatarURL string `json:"avatar_url"`
	Type      string `json:"type"`
	SiteAdmin bool   `json:"site_admin"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// UserJSON builds userJson (the viewer).
func UserJSON() UserView {
	return UserView{
		Login:     Viewer.Login,
		ID:        Viewer.ID,
		NodeID:    GlobalID("User", Viewer.ID),
		AvatarURL: "",
		Type:      "User",
		SiteAdmin: false,
		Name:      Viewer.Name,
		Email:     Viewer.Email,
		CreatedAt: FixedISO,
		UpdatedAt: FixedISO,
	}
}

// LabelView is labelJson's shape.
type LabelView struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// LabelJSON builds labelJson.
func LabelJSON(label *Label) LabelView {
	return LabelView{Name: label.Name, Color: label.Color, Description: label.Description}
}

// IssueView is issueJson's shape.
type IssueView struct {
	ID            int         `json:"id"`
	NodeID        string      `json:"node_id"`
	Number        int         `json:"number"`
	Title         string      `json:"title"`
	Body          string      `json:"body"`
	State         string      `json:"state"`
	Labels        []LabelView `json:"labels"`
	User          UserRef     `json:"user"`
	Assignees     []any       `json:"assignees"`
	CreatedAt     string      `json:"created_at"`
	UpdatedAt     string      `json:"updated_at"`
	ClosedAt      *string     `json:"closed_at"`
	Comments      int         `json:"comments"`
	HTMLURL       string      `json:"html_url"`
	URL           string      `json:"url"`
	RepositoryURL string      `json:"repository_url"`
}

// IssueJSON builds issueJson for an issue. closed_at is updatedAt when
// closed, else null.
func IssueJSON(issue *Issue, repo *Repo) IssueView {
	labels := make([]LabelView, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, LabelJSON(l))
	}
	var closedAt *string
	if issue.State == "closed" {
		closedAt = &issue.UpdatedAt
	}
	return IssueView{
		ID:            issue.ID,
		NodeID:        GlobalID("Issue", fmt.Sprintf("%d:%d", repo.ID, issue.Number)),
		Number:        issue.Number,
		Title:         issue.Title,
		Body:          issue.Body,
		State:         issue.State,
		Labels:        labels,
		User:          UserRef{Login: issue.User, ID: Viewer.ID, Type: "User"},
		Assignees:     []any{},
		CreatedAt:     issue.CreatedAt,
		UpdatedAt:     issue.UpdatedAt,
		ClosedAt:      closedAt,
		Comments:      len(issue.Comments),
		HTMLURL:       "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		URL:           "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		RepositoryURL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

// IssueJSONString returns JSON.stringify(issueJson(...)), cached on the
// issue until invalidated. The cache is guarded by the issue's own mutex
// (written on the read path, so it must be safe under the store RLock).
func IssueJSONString(issue *Issue, repo *Repo) string {
	issue.cacheMu.Lock()
	defer issue.cacheMu.Unlock()
	if issue.CachedJSON != "" {
		return issue.CachedJSON
	}
	body := MarshalJSONString(IssueJSON(issue, repo))
	issue.CachedJSON = body
	return body
}

// GetCachedFullResponse returns the cached whole-response bytes, or nil.
func (issue *Issue) GetCachedFullResponse() []byte {
	issue.cacheMu.Lock()
	defer issue.cacheMu.Unlock()
	return issue.CachedFullResponse
}

// SetCachedFullResponse stores the whole-response bytes.
func (issue *Issue) SetCachedFullResponse(b []byte) {
	issue.cacheMu.Lock()
	defer issue.cacheMu.Unlock()
	issue.CachedFullResponse = b
}

// InvalidateIssueJSON drops an issue's cached JSON after any mutation
// (PATCH, a new comment). Called by REST PATCH issue, REST POST comment,
// GraphQL closeIssue / addComment. NOT called by label changes (map N3).
func InvalidateIssueJSON(issue *Issue) {
	issue.cacheMu.Lock()
	defer issue.cacheMu.Unlock()
	issue.CachedJSON = ""
	issue.CachedFullResponse = nil
}

// CommentView is commentJson's shape.
type CommentView struct {
	ID        int     `json:"id"`
	NodeID    string  `json:"node_id"`
	Body      string  `json:"body"`
	User      UserRef `json:"user"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	HTMLURL   string  `json:"html_url"`
	URL       string  `json:"url"`
	IssueURL  string  `json:"issue_url"`
}

// CommentJSON builds commentJson. updated_at equals created_at.
func CommentJSON(comment *IssueComment, repo *Repo, issueNumber int) CommentView {
	base := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issueNumber)
	return CommentView{
		ID:        comment.ID,
		NodeID:    GlobalID("IssueComment", fmt.Sprintf("%d:%d", repo.ID, comment.ID)),
		Body:      comment.Body,
		User:      UserRef{Login: comment.User, ID: Viewer.ID, Type: "User"},
		CreatedAt: comment.CreatedAt,
		UpdatedAt: comment.CreatedAt,
		HTMLURL:   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issueNumber) + "#issuecomment-" + itoa(comment.ID),
		URL:       base + "/comments/" + itoa(comment.ID),
		IssueURL:  base,
	}
}

// PullHeadBase is the head/base object in pullJson.
type PullHeadBase struct {
	Ref   string   `json:"ref"`
	Sha   string   `json:"sha"`
	Label string   `json:"label"`
	User  OwnerRef `json:"user"`
}

// PullView is pullJson's shape.
type PullView struct {
	ID             int          `json:"id"`
	NodeID         string       `json:"node_id"`
	Number         int          `json:"number"`
	Title          string       `json:"title"`
	Body           string       `json:"body"`
	State          string       `json:"state"`
	Merged         bool         `json:"merged"`
	Mergeable      string       `json:"mergeable"`
	MergeCommitSha *string      `json:"merge_commit_sha"`
	MergedAt       *string      `json:"merged_at"`
	Head           PullHeadBase `json:"head"`
	Base           PullHeadBase `json:"base"`
	User           UserRef      `json:"user"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	ClosedAt       *string      `json:"closed_at"`
	HTMLURL        string       `json:"html_url"`
	URL            string       `json:"url"`
}

// PullJSON builds pullJson. updated_at / closed_at both equal createdAt
// (never updated on merge). mergeable is computed live.
func (s *Store) PullJSON(repo *Repo, pull *Pull) PullView {
	var closedAt *string
	if pull.State == "closed" {
		closedAt = &pull.CreatedAt
	}
	headUser := OwnerRef{Login: repo.Owner, ID: Viewer.ID}
	baseUser := OwnerRef{Login: repo.Owner, ID: Viewer.ID}
	return PullView{
		ID:             pull.ID,
		NodeID:         GlobalID("PullRequest", fmt.Sprintf("%d:%d", repo.ID, pull.Number)),
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Merged:         pull.Merged,
		Mergeable:      s.PullMergeable(repo, pull),
		MergeCommitSha: pull.MergeCommitSha,
		MergedAt:       pull.MergedAt,
		Head:           PullHeadBase{Ref: pull.Head.Ref, Sha: pull.Head.Sha, Label: repo.Owner + ":" + pull.Head.Ref, User: headUser},
		Base:           PullHeadBase{Ref: pull.Base.Ref, Sha: pull.Base.Sha, Label: repo.Owner + ":" + pull.Base.Ref, User: baseUser},
		User:           UserRef{Login: pull.User, ID: Viewer.ID, Type: "User"},
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.CreatedAt,
		ClosedAt:       closedAt,
		HTMLURL:        "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
		URL:            "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number),
	}
}

// LineDiffCounts returns LCS-based line diff counts between two texts.
// Additions = m - lcs, deletions = n - lcs; two-row DP, O(n*m) time,
// O(m) space. Used only by GET /pulls/{n}/files.
func LineDiffCounts(oldText, newText string) (additions, deletions int) {
	var a, b []string
	if oldText != "" {
		a = splitLines(oldText)
	}
	if newText != "" {
		b = splitLines(newText)
	}
	n, m := len(a), len(b)
	if n == 0 {
		return m, 0
	}
	if m == 0 {
		return 0, n
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
		for j := range cur {
			cur[j] = 0
		}
	}
	lcs := prev[m]
	return m - lcs, n - lcs
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
