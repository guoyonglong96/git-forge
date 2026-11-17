package server

// REST v3 API (/api/v3/*), ported from rest.ts (map §4).
//
// Entry: HandleRest(s, req) returns complete HTTP response bytes, or nil
// when the path does not start with /api/v3 (caller answers 404).
//
// WIRING NOTE: server.go's /api/v3 branch currently returns
// notImplemented(); the integrator must call HandleRest there, e.g.
//   if resp := HandleRest(s, req); resp != nil { return resp }
//   return BuildErrorResponse(404, "Not Found")
// (this file is the only file this phase may touch, so the one-line
// wiring lives outside it).
//
// Fidelity notes:
//   - Response JSON field order matches Node exactly: every body is a
//     struct with explicit field order (never map[string]any — Go sorts
//     map keys). Error bodies are {"message", "documentation_url": ""}.
//   - N4 deviation (map §4.1 note, as decided in server.go): a bad
//     percent-escape in owner/repo/branch/ref/contents-path answers 404,
//     not Node's 500. The suite never sends bad escapes.
//   - Pull JSON is built locally (pullView) rather than via
//     store.PullJSON: the store's PullHeadBase.User is an OwnerRef whose
//     empty Type marshals as "type":"", but Node's head/base user is
//     exactly {login, id} — see the report.
//   - RawRequest.Query is nil for origin-form URLs (the parser only fills
//     it on the absolute-form fallback), so the query is parsed here from
//     RawURL, preserving parameter order for Link-header rebuilding
//     (Node's URLSearchParams preserves insertion order; Go's
//     url.Values.Encode would sort keys).
//   - Performance (map §9.3 items 1–2): GET single issue and the issues
//     list are served from store.IssueJSONString's per-issue cache (the
//     whole-response memo lives in server.go's fast path); POST issue
//     serialises once via the same cache. No time.Now() anywhere on the
//     request path; no map iteration feeds output unsorted.

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"forge/internal/git"
	"forge/internal/store"
)

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

// errBody is {"message", "documentation_url": ""} in Node's field order.
type errBody struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
}

func restErr(status int, message string) []byte {
	return BuildResponse(status,
		[]Header{{Key: "Content-Type", Value: "application/json"}},
		store.MarshalJSONValue(errBody{Message: message, DocumentationURL: ""}))
}

func restNotFound(message string) []byte { return restErr(404, message) }

// restJSON marshals v (a struct: field order = Node's) with no Link header.
func restJSON(status int, v any) []byte {
	return BuildResponse(status,
		[]Header{{Key: "Content-Type", Value: "application/json"}},
		store.MarshalJSONValue(v))
}

// restJSONLink is restJSON with an optional Link header (after Content-Type,
// matching Node's json() header order: Content-Type first, then extras).
func restJSONLink(status int, v any, link string) []byte {
	h := []Header{{Key: "Content-Type", Value: "application/json"}}
	if link != "" {
		h = append(h, Header{Key: "Link", Value: link})
	}
	return BuildResponse(status, h, store.MarshalJSONValue(v))
}

// restRaw answers with an already-serialised JSON body.
func restRaw(status int, body string, link string) []byte {
	h := []Header{{Key: "Content-Type", Value: "application/json"}}
	if link != "" {
		h = append(h, Header{Key: "Link", Value: link})
	}
	return BuildResponse(status, h, []byte(body))
}

func restNoContent() []byte {
	// Node noContent(): empty headers/body; buildResponse still adds
	// Content-Type: application/json + Content-Length: 0.
	return BuildResponse(204, nil, []byte{})
}

// ---------------------------------------------------------------------------
// Ordered query parsing (Node URLSearchParams semantics)
// ---------------------------------------------------------------------------

type queryParam struct {
	key, val string
}

type orderedQuery struct {
	pairs []queryParam
}

func parseQuery(rawURL string) orderedQuery {
	var q orderedQuery
	i := strings.IndexByte(rawURL, '?')
	if i < 0 {
		return q
	}
	for _, part := range strings.Split(rawURL[i+1:], "&") {
		if part == "" {
			continue
		}
		key, val := part, ""
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			key, val = part[:eq], part[eq+1:]
		}
		q.pairs = append(q.pairs, queryParam{decodeQueryComponent(key), decodeQueryComponent(val)})
	}
	return q
}

// decodeQueryComponent: '+' -> space, %XX -> byte; an invalid % sequence
// becomes U+FFFD, as in the URL spec's percent-decode.
func decodeQueryComponent(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '+':
			b.WriteByte(' ')
			i++
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(hexVal(s[i+1])<<4 | hexVal(s[i+2]))
			i += 3
		case c == '%':
			b.WriteString("\uFFFD")
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// get returns the first value for key, or "" when absent (Node's .get()).
func (q orderedQuery) get(key string) string {
	v, _ := q.getOk(key)
	return v
}

// getOk reports whether key is present at all (distinguishing ?k= from a
// missing k: Node's ?? defaults only apply when the param is absent).
func (q orderedQuery) getOk(key string) (string, bool) {
	for _, p := range q.pairs {
		if p.key == key {
			return p.val, true
		}
	}
	return "", false
}

// set replaces the first occurrence (dropping later duplicates) or appends,
// matching URLSearchParams.set.
func (q *orderedQuery) set(key, val string) {
	for i, p := range q.pairs {
		if p.key == key {
			q.pairs[i].val = val
			out := q.pairs[:i+1]
			for _, p2 := range q.pairs[i+1:] {
				if p2.key != key {
					out = append(out, p2)
				}
			}
			q.pairs = out
			return
		}
	}
	q.pairs = append(q.pairs, queryParam{key, val})
}

// encode serialises like URLSearchParams.toString(): alphanumerics and
// *-._ literal, space as '+', everything else %XX (uppercase hex).
func (q orderedQuery) encode() string {
	var b strings.Builder
	for i, p := range q.pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		encodeQueryComponent(&b, p.key)
		b.WriteByte('=')
		encodeQueryComponent(&b, p.val)
	}
	return b.String()
}

func encodeQueryComponent(b *strings.Builder, s string) {
	const hexd = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
			c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xf])
		}
	}
}

// ---------------------------------------------------------------------------
// JS parseInt(s, 10) for pagination / numbers (rest.ts uses parseInt)
// ---------------------------------------------------------------------------

// restParseInt mirrors JS parseInt(s, 10): leading whitespace, optional sign,
// then digits; no digits -> ok=false. Saturates instead of overflowing.
func restParseInt(s string) (int64, bool) {
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			i++
		default:
			goto digits
		}
	}
digits:
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var n int64
	d := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		if n < (1 << 62) {
			n = n*10 + int64(c-'0')
		}
		d++
	}
	if d == 0 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// parseDigits parses an all-digit string (issue/pull numbers); ok=false on
// empty, non-digit, or overflow input.
func parseDigits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n > (1<<31-1)/10 {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Pagination (rest.ts paginate)
// ---------------------------------------------------------------------------

// paginate slices items and builds the Link header ("" when no link
// applies). path is the decoded request path used in the links.
func paginate[T any](items []T, q orderedQuery, path, baseURL string) ([]T, string) {
	perPage := 30
	if n, ok := restParseInt(q.get("per_page")); ok && n > 0 {
		perPage = int(n)
		if perPage > 100 {
			perPage = 100
		}
	}
	page := 1
	if n, ok := restParseInt(q.get("page")); ok && n > 0 {
		page = int(n)
	}
	start := 0
	if page > 1 {
		if page-1 > (1<<62)/perPage {
			start = len(items) // saturating: beyond the end, empty page
		} else {
			start = (page - 1) * perPage
		}
	}
	var slice []T
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = items[len(items):len(items)]
	}
	total := len(items)
	var links []string
	mk := func(p int) string {
		qq := orderedQuery{pairs: append([]queryParam(nil), q.pairs...)}
		qq.set("per_page", strconv.Itoa(perPage))
		qq.set("page", strconv.Itoa(p))
		return "<" + baseURL + path + "?" + qq.encode() + ">"
	}
	if start+perPage < total {
		links = append(links, mk(page+1)+`; rel="next"`)
	}
	if page > 1 {
		links = append(links, mk(page-1)+`; rel="prev"`)
	}
	if total > 0 {
		last := (total + perPage - 1) / perPage
		if last < 1 {
			last = 1
		}
		links = append(links, mk(1)+`; rel="first"`)
		links = append(links, mk(last)+`; rel="last"`)
	}
	return slice, strings.Join(links, ", ")
}

// ---------------------------------------------------------------------------
// Body helpers (rest.ts bodyObj / str / bool)
// ---------------------------------------------------------------------------

func bodyObj(bodyJSON any) map[string]any {
	if m, ok := bodyJSON.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func bodyStr(b map[string]any, key, fallback string) string {
	if v, ok := b[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func bodyBool(b map[string]any, key string, fallback bool) bool {
	if v, ok := b[key]; ok {
		if f, ok := v.(bool); ok {
			return f
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// View structs (field order = Node's object literals)
// ---------------------------------------------------------------------------

type rootView struct {
	CurrentUserURL string `json:"current_user_url"`
}

type metaView struct {
	InstalledVersion string `json:"installed_version"`
}

type branchCommitView struct {
	Sha string `json:"sha"`
	URL string `json:"url"`
}

type branchView struct {
	Name      string           `json:"name"`
	Commit    branchCommitView `json:"commit"`
	Protected bool             `json:"protected"`
}

type gitRefObjectView struct {
	Sha  string `json:"sha"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

type gitRefView struct {
	Ref    string           `json:"ref"`
	NodeID string           `json:"node_id"`
	Object gitRefObjectView `json:"object"`
	URL    string           `json:"url"`
}

type personView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type commitTreeView struct {
	Sha string `json:"sha"`
	URL string `json:"url"`
}

type commitInnerView struct {
	Message   string         `json:"message"`
	Author    personView     `json:"author"`
	Committer personView     `json:"committer"`
	Tree      commitTreeView `json:"tree"`
}

type commitParentView struct {
	Sha string `json:"sha"`
	URL string `json:"url"`
}

type commitView struct {
	Sha     string             `json:"sha"`
	NodeID  string             `json:"node_id"`
	Commit  commitInnerView    `json:"commit"`
	Parents []commitParentView `json:"parents"`
	URL     string             `json:"url"`
	HTMLURL string             `json:"html_url"`
}

// pullUserView is {login, id} — exactly Node's head/base user (no "type").
type pullUserView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type pullRefView struct {
	Ref   string       `json:"ref"`
	Sha   string       `json:"sha"`
	Label string       `json:"label"`
	User  pullUserView `json:"user"`
}

type pullView struct {
	ID             int           `json:"id"`
	NodeID         string        `json:"node_id"`
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	State          string        `json:"state"`
	Merged         bool          `json:"merged"`
	Mergeable      string        `json:"mergeable"`
	MergeCommitSha *string       `json:"merge_commit_sha"`
	MergedAt       *string       `json:"merged_at"`
	Head           pullRefView   `json:"head"`
	Base           pullRefView   `json:"base"`
	User           store.UserRef `json:"user"`
	CreatedAt      string        `json:"created_at"`
	UpdatedAt      string        `json:"updated_at"`
	ClosedAt       *string       `json:"closed_at"`
	HTMLURL        string        `json:"html_url"`
	URL            string        `json:"url"`
}

func buildPullView(s *store.Store, repo *store.Repo, pull *store.Pull) pullView {
	var closedAt *string
	if pull.State == "closed" {
		closedAt = &pull.CreatedAt
	}
	return pullView{
		ID:             pull.ID,
		NodeID:         store.GlobalID("PullRequest", strconv.Itoa(repo.ID)+":"+strconv.Itoa(pull.Number)),
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Merged:         pull.Merged,
		Mergeable:      s.PullMergeable(repo, pull),
		MergeCommitSha: pull.MergeCommitSha,
		MergedAt:       pull.MergedAt,
		Head:           pullRefView{Ref: pull.Head.Ref, Sha: pull.Head.Sha, Label: repo.Owner + ":" + pull.Head.Ref, User: pullUserView{Login: repo.Owner, ID: store.Viewer.ID}},
		Base:           pullRefView{Ref: pull.Base.Ref, Sha: pull.Base.Sha, Label: repo.Owner + ":" + pull.Base.Ref, User: pullUserView{Login: repo.Owner, ID: store.Viewer.ID}},
		User:           store.UserRef{Login: pull.User, ID: store.Viewer.ID, Type: "User"},
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.CreatedAt,
		ClosedAt:       closedAt,
		HTMLURL:        "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		URL:            "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

type pullFileView struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	Sha       string `json:"sha"`
	RawURL    string `json:"raw_url"`
	BlobURL   string `json:"blob_url"`
}

type mergeView struct {
	Sha     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

type contentFileView struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Sha      string `json:"sha"`
	URL      string `json:"url"`
	HTMLURL  string `json:"html_url"`
}

type contentDirView struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Sha     string `json:"sha"`
	Size    int    `json:"size"`
	URL     string `json:"url"`
	HTMLURL string `json:"html_url"`
}

type contentPutContentView struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Sha      string `json:"sha"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	URL      string `json:"url"`
}

type commitAuthorView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type contentCommitView struct {
	Sha       string           `json:"sha"`
	Message   string           `json:"message"`
	Author    commitAuthorView `json:"author"`
	Committer commitAuthorView `json:"committer"`
}

type contentPutView struct {
	Content contentPutContentView `json:"content"`
	Commit  contentCommitView     `json:"commit"`
}

type contentDeleteView struct {
	Content *contentPutContentView `json:"content"`
	Commit  contentCommitView      `json:"commit"`
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// HandleRest serves /api/v3/* requests. Returns nil when the path does not
// start with /api/v3. The store lock is held by the caller (server.go).
func HandleRest(s *store.Store, req *RawRequest) []byte {
	path := req.Path
	if !strings.HasPrefix(path, "/api/v3") {
		return nil
	}
	rest := path[len("/api/v3"):]
	if rest == "" {
		rest = "/"
	}
	m := req.Method
	q := parseQuery(req.RawURL)
	baseURL := "http://" + req.Host
	if req.Host == "" {
		baseURL = "http://localhost:" + strconv.Itoa(req.LocalPort)
	}

	// Body: Node parses a non-empty body as JSON once; a parse failure sets
	// bodyError (checked only by endpoints that read a body).
	var bodyJSON any
	bodyError := false
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &bodyJSON); err != nil {
			bodyError = true
		}
	}

	// GET /api/v3/ — root
	if rest == "/" {
		if m != "GET" {
			return restErr(405, "Method Not Allowed")
		}
		return BuildResponse(200,
			[]Header{{Key: "Content-Type", Value: "application/json"}, {Key: "X-OAuth-Scopes", Value: "repo, read:org"}},
			store.MarshalJSONValue(rootView{CurrentUserURL: "/api/v3/user"}))
	}

	// GET /api/v3/user
	if rest == "/user" {
		if m == "GET" {
			return restJSON(200, store.UserJSON())
		}
		return restErr(405, "Method Not Allowed")
	}

	// GET /api/v3/meta
	if rest == "/meta" {
		if m == "GET" {
			return restJSON(200, metaView{InstalledVersion: "1.0.0"})
		}
		return restErr(405, "Method Not Allowed")
	}

	// /api/v3/user/repos
	if rest == "/user/repos" {
		if m == "GET" {
			repos := s.ViewerRepos()
			views := make([]store.RepoView, 0, len(repos))
			for _, r := range repos {
				views = append(views, s.RepoJSON(r))
			}
			page, link := paginate(views, q, "/api/v3/user/repos", baseURL)
			return restJSONLink(200, page, link)
		}
		if m == "POST" {
			if bodyError {
				return restErr(400, "Invalid JSON body")
			}
			b := bodyObj(bodyJSON)
			repo, err := s.CreateRepo(
				bodyStr(b, "name", ""),
				bodyBool(b, "private", false),
				bodyStr(b, "description", ""),
				bodyStr(b, "default_branch", "main"),
			)
			if err != nil {
				if err.Error() == "Repository already exists" {
					return restErr(422, "name already exists on this account")
				}
				return restErr(400, err.Error())
			}
			return restJSON(201, s.RepoJSON(repo))
		}
		return restErr(405, "Method Not Allowed")
	}

	// /api/v3/repos/{owner}/{repo}/...
	if strings.HasPrefix(rest, "/repos/") {
		if resp, handled := handleRepoRest(s, req, rest, m, q, baseURL, bodyJSON, bodyError); handled {
			return resp
		}
	}

	return restNotFound("Not Found")
}

// handleRepoRest handles everything under /api/v3/repos/{owner}/{repo}.
// handled=false means the path did not match the repo regex at all
// (caller answers 404).
func handleRepoRest(s *store.Store, req *RawRequest, rest, m string, q orderedQuery, baseURL string, bodyJSON any, bodyError bool) ([]byte, bool) {
	r := rest[len("/repos/"):]
	i := strings.IndexByte(r, '/')
	if i <= 0 {
		return nil, false
	}
	ownerRaw := r[:i]
	r2 := r[i+1:]
	var repoRaw, sub string
	if j := strings.IndexByte(r2, '/'); j < 0 {
		repoRaw, sub = r2, ""
	} else {
		repoRaw, sub = r2[:j], r2[j:]
	}
	if repoRaw == "" {
		return nil, false
	}
	// N4 deviation: bad percent-escapes answer 404, not Node's 500.
	owner, err1 := url.PathUnescape(ownerRaw)
	repoName, err2 := url.PathUnescape(repoRaw)
	if err1 != nil || err2 != nil {
		return restNotFound("Not Found"), true
	}
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return restNotFound("Not Found"), true
	}

	// GET/PATCH/DELETE /repos/{o}/{r} (trailing slash == the repo itself)
	if sub == "" || sub == "/" {
		if m == "GET" {
			return restJSON(200, s.RepoJSON(repo)), true
		}
		if m == "PATCH" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			if v, ok := b["description"]; ok {
				if d, ok := v.(string); ok {
					repo.Description = d
				}
			}
			if v, ok := b["default_branch"]; ok {
				if d, ok := v.(string); ok && d != "" {
					repo.DefaultBranch = d
				}
			}
			return restJSON(200, s.RepoJSON(repo)), true
		}
		if m == "DELETE" {
			s.DeleteRepo(repo)
			return restNoContent(), true
		}
		return restErr(405, "Method Not Allowed"), true
	}

	// Branches list
	if sub == "/branches" {
		if m != "GET" {
			return restErr(405, "Method Not Allowed"), true
		}
		names := make([]string, 0, len(repo.Refs))
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref[len("refs/heads/"):])
			}
		}
		sort.Strings(names)
		views := make([]branchView, 0, len(names))
		for _, name := range names {
			sha := repo.Refs["refs/heads/"+name]
			if sha == "" {
				sha = git.ZeroOID
			}
			views = append(views, branchView{Name: name, Commit: branchCommitView{Sha: sha, URL: ""}, Protected: false})
		}
		return restJSON(200, views), true
	}

	// Single branch (may contain '/'; existence checked before method)
	if strings.HasPrefix(sub, "/branches/") {
		raw := sub[len("/branches/"):]
		if raw != "" {
			// N4 deviation: bad percent-escape answers 404, not Node's 500.
			branch, err := url.PathUnescape(raw)
			if err != nil {
				return restNotFound("Not Found"), true
			}
			oid, ok := repo.Refs["refs/heads/"+branch]
			if !ok {
				return restNotFound("Branch not found"), true
			}
			if m != "GET" {
				return restErr(405, "Method Not Allowed"), true
			}
			return restJSON(200, branchView{Name: branch, Commit: branchCommitView{Sha: oid, URL: ""}, Protected: false}), true
		}
	}

	// GET /git/refs/{ref}
	if strings.HasPrefix(sub, "/git/refs/") {
		raw := sub[len("/git/refs/"):]
		if raw != "" {
			// N4 deviation: bad percent-escape answers 404, not Node's 500.
			ref, err := url.PathUnescape(raw)
			if err != nil {
				return restNotFound("Not Found"), true
			}
			if m == "GET" {
				oid, ok := repo.Refs[ref]
				if !ok {
					return restNotFound("Reference does not exist"), true
				}
				return restJSON(200, gitRefView{Ref: ref, NodeID: "", Object: gitRefObjectView{Sha: oid, Type: "commit", URL: ""}, URL: ""}), true
			}
			return restErr(405, "Method Not Allowed"), true
		}
	}

	// POST /git/refs
	if sub == "/git/refs" {
		if m == "POST" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			ref := bodyStr(b, "ref", "")
			sha := bodyStr(b, "sha", "")
			if !strings.HasPrefix(ref, "refs/") || !git.IsOID(sha) {
				return restErr(422, "Invalid ref or sha"), true
			}
			if _, ok := repo.Refs[ref]; ok {
				return restErr(422, "Reference already exists"), true
			}
			if _, ok := repo.Objects[sha]; !ok {
				return restErr(422, "Object does not exist"), true
			}
			repo.Refs[ref] = sha
			return restJSON(201, gitRefView{Ref: ref, NodeID: "", Object: gitRefObjectView{Sha: sha, Type: "commit", URL: ""}, URL: ""}), true
		}
		return restErr(405, "Method Not Allowed"), true
	}

	// Commits
	if sub == "/commits" {
		if m != "GET" {
			return restErr(405, "Method Not Allowed"), true
		}
		shaParam := q.get("sha")
		var tip string
		var hasTip bool
		if shaParam != "" {
			tip, hasTip = s.ResolveRef(repo, shaParam)
			if !hasTip {
				return restNotFound("No commit found for SHA: " + shaParam), true
			}
		} else {
			tip, hasTip = s.RefCommit(repo, "refs/heads/"+repo.DefaultBranch)
		}
		var list []string
		if hasTip {
			list = git.ListCommits(tip, s.Lookup(repo))
		} else {
			list = []string{}
		}
		views := make([]commitView, 0, len(list))
		for _, c := range list {
			var parsed git.CommitView
			if obj, ok := repo.Objects[c]; ok {
				parsed = git.ParseCommit(obj.Data)
			}
			parents := make([]commitParentView, 0, len(parsed.Parents))
			for _, p := range parsed.Parents {
				parents = append(parents, commitParentView{Sha: p, URL: ""})
			}
			author := personView{Name: store.Viewer.Name, Email: store.Viewer.Email, Date: store.FixedISO}
			views = append(views, commitView{
				Sha:    c,
				NodeID: "",
				Commit: commitInnerView{
					Message:   parsed.Message,
					Author:    author,
					Committer: author,
					Tree:      commitTreeView{Sha: parsed.Tree, URL: ""},
				},
				Parents: parents,
				URL:     "",
				HTMLURL: "",
			})
		}
		page, link := paginate(views, q, "/api/v3/repos/"+owner+"/"+repoName+"/commits", baseURL)
		return restJSONLink(200, page, link), true
	}

	// Contents
	if sub == "/contents" || sub == "/contents/" {
		return handleContents(s, repo, req, owner, repoName, "", m, q, bodyJSON, bodyError), true
	}
	if strings.HasPrefix(sub, "/contents/") {
		return handleContents(s, repo, req, owner, repoName, sub[len("/contents/"):], m, q, bodyJSON, bodyError), true
	}

	// Labels
	if sub == "/labels" {
		if m == "GET" {
			labels := make([]*store.Label, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				labels = append(labels, l)
			}
			sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
			views := make([]store.LabelView, 0, len(labels))
			for _, l := range labels {
				views = append(views, store.LabelJSON(l))
			}
			return restJSON(200, views), true
		}
		if m == "POST" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			name := bodyStr(b, "name", "")
			if name == "" {
				return restErr(422, "Validation Failed: name is required"), true
			}
			color := strings.TrimPrefix(bodyStr(b, "color", "ffffff"), "#")
			label := &store.Label{Name: name, Color: color, Description: bodyStr(b, "description", "")}
			repo.Labels[name] = label
			return restJSON(201, store.LabelJSON(label)), true
		}
		return restErr(405, "Method Not Allowed"), true
	}

	// Issues list/create
	if sub == "/issues" {
		if m == "GET" {
			state, ok := q.getOk("state")
			if !ok {
				state = "open"
			}
			issues := make([]*store.Issue, 0, len(repo.Issues))
			for _, iss := range repo.Issues {
				issues = append(issues, iss)
			}
			if state == "open" {
				filtered := issues[:0]
				for _, iss := range issues {
					if iss.State == "open" {
						filtered = append(filtered, iss)
					}
				}
				issues = filtered
			} else if state == "closed" {
				filtered := issues[:0]
				for _, iss := range issues {
					if iss.State == "closed" {
						filtered = append(filtered, iss)
					}
				}
				issues = filtered
			} else if state != "all" {
				return restErr(422, `Invalid state "`+state+`"`), true
			}
			// sort is ignored in practice: always by number asc (rest.ts:354);
			// only direction 'desc' reverses (default is 'desc' when absent).
			sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
			direction, ok := q.getOk("direction")
			if !ok {
				direction = "desc"
			}
			if direction == "desc" {
				for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
					issues[i], issues[j] = issues[j], issues[i]
				}
			}
			page, link := paginate(issues, q, "/api/v3/repos/"+owner+"/"+repoName+"/issues", baseURL)
			var sb strings.Builder
			sb.WriteByte('[')
			for i, iss := range page {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(store.IssueJSONString(iss, repo))
			}
			sb.WriteByte(']')
			return restRaw(200, sb.String(), link), true
		}
		if m == "POST" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			title := bodyStr(b, "title", "")
			if title == "" {
				return restErr(422, "Validation Failed: title is required"), true
			}
			var labelNames []string
			if arr, ok := b["labels"].([]any); ok {
				labelNames = make([]string, 0, len(arr))
				for _, l := range arr {
					if s, ok := l.(string); ok {
						labelNames = append(labelNames, s)
					} else if lm, ok := l.(map[string]any); ok {
						labelNames = append(labelNames, bodyStr(lm, "name", ""))
					} else {
						labelNames = append(labelNames, "")
					}
				}
			} else {
				labelNames = []string{}
			}
			issue, err := s.CreateIssue(repo, title, bodyStr(b, "body", ""), labelNames)
			if err != nil {
				return restErr(422, err.Error()), true
			}
			return restRaw(201, store.IssueJSONString(issue, repo), ""), true
		}
		return restErr(405, "Method Not Allowed"), true
	}

	// Issue single / comments
	if strings.HasPrefix(sub, "/issues/") {
		if resp, handled := handleIssueRest(s, repo, sub, m, bodyJSON, bodyError); handled {
			return resp, true
		}
	}

	// Pulls list/create
	if sub == "/pulls" {
		if m == "GET" {
			state, ok := q.getOk("state")
			if !ok {
				state = "open"
			}
			pulls := make([]*store.Pull, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				pulls = append(pulls, p)
			}
			if state == "open" {
				filtered := pulls[:0]
				for _, p := range pulls {
					if p.State == "open" {
						filtered = append(filtered, p)
					}
				}
				pulls = filtered
			} else if state == "closed" {
				filtered := pulls[:0]
				for _, p := range pulls {
					if p.State == "closed" {
						filtered = append(filtered, p)
					}
				}
				pulls = filtered
			} else if state != "all" {
				return restErr(422, `Invalid state "`+state+`"`), true
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			views := make([]pullView, 0, len(pulls))
			for _, p := range pulls {
				views = append(views, buildPullView(s, repo, p))
			}
			page, link := paginate(views, q, "/api/v3/repos/"+owner+"/"+repoName+"/pulls", baseURL)
			return restJSONLink(200, page, link), true
		}
		if m == "POST" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			title := bodyStr(b, "title", "")
			head := bodyStr(b, "head", "")
			base := bodyStr(b, "base", "")
			if title == "" || head == "" || base == "" {
				return restErr(422, "Validation Failed: title, head and base are required"), true
			}
			pull, err := s.CreatePull(repo, title, bodyStr(b, "body", ""), head, base)
			if err != nil {
				return restErr(422, err.Error()), true
			}
			return restJSON(201, buildPullView(s, repo, pull)), true
		}
		return restErr(405, "Method Not Allowed"), true
	}

	// Pull single / files / merge
	if strings.HasPrefix(sub, "/pulls/") {
		if resp, handled := handlePullRest(s, repo, sub, m, bodyJSON, bodyError); handled {
			return resp, true
		}
	}

	return restNotFound("Not Found"), true
}

// handleIssueRest handles /issues/{n} and /issues/{n}/comments.
// handled=false means the path did not match the issue regex (caller 404s).
func handleIssueRest(s *store.Store, repo *store.Repo, sub, m string, bodyJSON any, bodyError bool) ([]byte, bool) {
	rest := sub[len("/issues/"):]
	parts := strings.Split(rest, "/")
	number, ok := parseDigits(parts[0])
	if !ok {
		return nil, false
	}
	var comments bool
	switch len(parts) {
	case 1:
	case 2:
		if parts[1] != "comments" {
			return nil, false
		}
		comments = true
	default:
		return nil, false
	}
	issue, ok := repo.Issues[number]
	if !ok {
		return restNotFound("Not Found"), true
	}
	if !comments {
		if m == "GET" {
			return restRaw(200, store.IssueJSONString(issue, repo), ""), true
		}
		if m == "PATCH" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			if v, ok := b["state"]; ok {
				if st, ok := v.(string); ok && (st == "open" || st == "closed") {
					issue.State = st
					issue.UpdatedAt = store.FixedISO
				}
			}
			if v, ok := b["title"]; ok {
				if t, ok := v.(string); ok {
					issue.Title = t
				}
			}
			if v, ok := b["body"]; ok {
				if bd, ok := v.(string); ok {
					issue.Body = bd
				}
			}
			store.InvalidateIssueJSON(issue)
			return restRaw(200, store.IssueJSONString(issue, repo), ""), true
		}
		return restErr(405, "Method Not Allowed"), true
	}
	// /comments
	if m == "GET" {
		views := make([]store.CommentView, 0, len(issue.Comments))
		for _, c := range issue.Comments {
			views = append(views, store.CommentJSON(c, repo, number))
		}
		return restJSON(200, views), true
	}
	if m == "POST" {
		if bodyError {
			return restErr(400, "Invalid JSON body"), true
		}
		b := bodyObj(bodyJSON)
		body := bodyStr(b, "body", "")
		if body == "" {
			return restErr(422, "Validation Failed: body is required"), true
		}
		comment := &store.IssueComment{
			ID:        repo.NextCommentID,
			Body:      body,
			User:      store.Viewer.Login,
			CreatedAt: store.FixedISO,
		}
		repo.NextCommentID++
		issue.Comments = append(issue.Comments, comment)
		store.InvalidateIssueJSON(issue)
		return restJSON(201, store.CommentJSON(comment, repo, number)), true
	}
	return restErr(405, "Method Not Allowed"), true
}

// handlePullRest handles /pulls/{n}, /pulls/{n}/files, /pulls/{n}/merge.
// handled=false means the path did not match the pull regex (caller 404s).
func handlePullRest(s *store.Store, repo *store.Repo, sub, m string, bodyJSON any, bodyError bool) ([]byte, bool) {
	rest := sub[len("/pulls/"):]
	parts := strings.Split(rest, "/")
	number, ok := parseDigits(parts[0])
	if !ok {
		return nil, false
	}
	var action string
	switch len(parts) {
	case 1:
	case 2:
		if parts[1] != "files" && parts[1] != "merge" {
			return nil, false
		}
		action = parts[1]
	default:
		return nil, false
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		return restNotFound("Not Found"), true
	}
	if action == "" {
		if m == "GET" {
			return restJSON(200, buildPullView(s, repo, pull)), true
		}
		if m == "PATCH" {
			if bodyError {
				return restErr(400, "Invalid JSON body"), true
			}
			b := bodyObj(bodyJSON)
			if v, ok := b["state"]; ok {
				if st, ok := v.(string); ok && st == "closed" && pull.State == "open" {
					pull.State = "closed"
				}
			}
			if v, ok := b["title"]; ok {
				if t, ok := v.(string); ok {
					pull.Title = t
				}
			}
			if v, ok := b["body"]; ok {
				if bd, ok := v.(string); ok {
					pull.Body = bd
				}
			}
			return restJSON(200, buildPullView(s, repo, pull)), true
		}
		return restErr(405, "Method Not Allowed"), true
	}
	if action == "files" {
		if m != "GET" {
			return restErr(405, "Method Not Allowed"), true
		}
		diff := s.DiffPull(repo, pull)
		names := make([]string, 0, len(diff))
		for name := range diff {
			names = append(names, name)
		}
		sort.Strings(names)
		files := make([]pullFileView, 0, len(names))
		for _, filename := range names {
			d := diff[filename]
			oldText := blobText(repo, d.BaseOID)
			newText := blobText(repo, d.HeadOID)
			additions, deletions := store.LineDiffCounts(oldText, newText)
			if d.Status == store.DiffRemoved {
				additions = 0
			}
			if d.Status == store.DiffAdded {
				deletions = 0
			}
			outAdditions := additions
			if d.Status != store.DiffRemoved && outAdditions < 1 {
				outAdditions = 1
			}
			sha := ""
			if d.HeadOID != nil {
				sha = *d.HeadOID
			} else if d.BaseOID != nil {
				sha = *d.BaseOID
			}
			// changes uses the pre-max additions (rest.ts files handler).
			files = append(files, pullFileView{
				Filename:  filename,
				Status:    string(d.Status),
				Additions: outAdditions,
				Deletions: deletions,
				Changes:   additions + deletions,
				Sha:       sha,
				RawURL:    "",
				BlobURL:   "",
			})
		}
		return restJSON(200, files), true
	}
	// merge
	if m != "PUT" {
		return restErr(405, "Method Not Allowed"), true
	}
	if bodyError {
		return restErr(400, "Invalid JSON body"), true
	}
	sha, err := s.MergePull(repo, pull)
	if err != nil {
		status := 405
		if de, ok := err.(*store.DomainError); ok && de.Status != 0 {
			status = de.Status
		}
		return restErr(status, err.Error()), true
	}
	return restJSON(200, mergeView{Sha: sha, Merged: true, Message: "Pull Request successfully merged"}), true
}

func blobText(repo *store.Repo, oid *string) string {
	if oid == nil {
		return ""
	}
	obj, ok := repo.Objects[*oid]
	if !ok || obj.Type != git.ObjBlob {
		return ""
	}
	return string(obj.Data)
}

// ---------------------------------------------------------------------------
// Contents (rest.ts handleContents)
// ---------------------------------------------------------------------------

// decodeBase64Strict: whitespace stripped, empty or length%4==1 or chars
// outside [A-Za-z0-9+/=] (≤2 trailing '=') rejected; missing padding
// tolerated; round-trip check must hold.
func decodeBase64Strict(s string) ([]byte, error) {
	t := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	if t == "" || len(t)%4 == 1 {
		return nil, errInvalidBase64
	}
	eq := strings.IndexByte(t, '=')
	if eq >= 0 {
		rest := t[eq:]
		if len(rest) > 2 || strings.Trim(rest, "=") != "" {
			return nil, errInvalidBase64
		}
		// Validate the part before '='.
		for i := 0; i < eq; i++ {
			if !isB64Char(t[i]) {
				return nil, errInvalidBase64
			}
		}
	} else {
		for i := 0; i < len(t); i++ {
			if !isB64Char(t[i]) {
				return nil, errInvalidBase64
			}
		}
	}
	padded := t + strings.Repeat("=", (4-len(t)%4)%4)
	buf, err := base64.StdEncoding.DecodeString(padded)
	if err != nil {
		return nil, errInvalidBase64
	}
	if base64.StdEncoding.EncodeToString(buf) != padded {
		return nil, errInvalidBase64
	}
	return buf, nil
}

var errInvalidBase64 = errString("Invalid base64 content")

type errString string

func (e errString) Error() string { return string(e) }

func isB64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func contentAuthor() commitAuthorView {
	return commitAuthorView{Name: store.Viewer.Name, Email: store.Viewer.Email, Date: store.FixedISO}
}

func handleContents(s *store.Store, repo *store.Repo, req *RawRequest, owner, repoName, rawPath, m string, q orderedQuery, bodyJSON any, bodyError bool) []byte {
	// N4 deviation: a bad percent-escape answers 404, not Node's 500.
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return restNotFound("Not Found")
	}
	refParam, hasRef := q.getOk("ref")
	refName := refParam
	if !hasRef {
		refName = repo.DefaultBranch
	}
	ref := "refs/heads/" + refName

	if m == "GET" {
		treeMap := s.TreeMapAt(repo, ref)
		if entry, ok := treeMap[path]; ok {
			var content string
			size := 0
			if obj, ok := repo.Objects[entry.OID]; ok {
				size = len(obj.Data)
				if obj.Type == git.ObjBlob {
					content = base64.StdEncoding.EncodeToString(obj.Data)
				}
			}
			return restJSON(200, contentFileView{
				Type:     "file",
				Encoding: "base64",
				Size:     size,
				Name:     baseName(path),
				Path:     path,
				Content:  content,
				Sha:      entry.OID,
				URL:      "/api/v3/repos/" + owner + "/" + repoName + "/contents/" + path,
				HTMLURL:  "https://example.invalid/" + owner + "/" + repoName + "/blob/" + refName + "/" + path,
			})
		}
		// Directory listing: immediate children of prefix.
		prefix := ""
		if path != "" {
			prefix = path + "/"
		}
		type child struct {
			typ string
			oid string
		}
		children := map[string]child{}
		for p, e := range treeMap {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := p[len(prefix):]
			if rest == "" {
				continue
			}
			first := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				first = rest[:i]
			}
			if strings.Contains(rest, "/") {
				if _, ok := children[first]; !ok {
					children[first] = child{typ: "dir", oid: ""}
				}
			} else {
				children[first] = child{typ: "file", oid: e.OID}
			}
		}
		if len(children) == 0 {
			return restNotFound("Not Found")
		}
		names := make([]string, 0, len(children))
		for name := range children {
			names = append(names, name)
		}
		sort.Strings(names)
		listing := make([]contentDirView, 0, len(names))
		for _, name := range names {
			c := children[name]
			sha := c.oid
			if sha == "" {
				sum := sha1.Sum([]byte(prefix + name))
				sha = hex.EncodeToString(sum[:])
			}
			listing = append(listing, contentDirView{
				Type: c.typ, Name: name, Path: prefix + name, Sha: sha, Size: 0, URL: "", HTMLURL: "",
			})
		}
		return restJSON(200, listing)
	}

	if m == "PUT" {
		if bodyError {
			return restErr(400, "Invalid JSON body")
		}
		b := bodyObj(bodyJSON)
		message := bodyStr(b, "message", "")
		contentB64 := bodyStr(b, "content", "")
		if message == "" {
			return restErr(422, "Validation Failed: message is required")
		}
		if contentB64 == "" {
			return restErr(422, "Validation Failed: content is required")
		}
		content, err := decodeBase64Strict(contentB64)
		if err != nil {
			return restErr(422, "Invalid base64 content")
		}
		branch := bodyStr(b, "branch", repo.DefaultBranch)
		branchRef := "refs/heads/" + branch
		existing, hasExisting := repo.Refs[branchRef]
		if !hasExisting {
			// Create the branch at the default-branch tip (or unborn).
			if defaultTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
				repo.Refs[branchRef] = defaultTip
				existing, hasExisting = defaultTip, true
			}
		}
		treeMap := s.TreeMapAt(repo, branchRef)
		current, isUpdate := treeMap[path]
		if isUpdate {
			sha := bodyStr(b, "sha", "")
			if sha == "" || sha != current.OID {
				return restErr(409, "sha does not match the current file")
			}
		}
		blobOid := s.PutObject(repo, git.ObjBlob, content)
		treeMap[path] = git.FlatEntry{OID: blobOid, Mode: "100644"}
		treeOid := s.BuildTreeFromMap(repo, treeMap)
		var parents []string
		if hasExisting {
			parents = []string{existing}
		}
		commitOid := s.CreateCommit(repo, treeOid, parents, message)
		repo.Refs[branchRef] = commitOid
		status := 201
		if isUpdate {
			status = 200
		}
		return restJSON(status, contentPutView{
			Content: contentPutContentView{
				Name: baseName(path), Path: path, Sha: blobOid, Size: len(content),
				Encoding: "base64", Content: contentB64,
				URL: "/api/v3/repos/" + owner + "/" + repoName + "/contents/" + path,
			},
			Commit: contentCommitView{Sha: commitOid, Message: message, Author: contentAuthor(), Committer: contentAuthor()},
		})
	}

	if m == "DELETE" {
		if bodyError {
			return restErr(400, "Invalid JSON body")
		}
		b := bodyObj(bodyJSON)
		message := bodyStr(b, "message", "")
		sha := bodyStr(b, "sha", "")
		branch := bodyStr(b, "branch", repo.DefaultBranch)
		branchRef := "refs/heads/" + branch
		existing, ok := repo.Refs[branchRef]
		if !ok {
			return restNotFound("Not Found")
		}
		treeMap := s.TreeMapAt(repo, branchRef)
		current, ok := treeMap[path]
		if !ok {
			return restNotFound("Not Found")
		}
		if sha == "" || sha != current.OID {
			return restErr(409, "sha does not match the current file")
		}
		if message == "" {
			message = "Delete " + path
		}
		delete(treeMap, path)
		treeOid := s.BuildTreeFromMap(repo, treeMap)
		commitOid := s.CreateCommit(repo, treeOid, []string{existing}, message)
		repo.Refs[branchRef] = commitOid
		return restJSON(200, contentDeleteView{
			Content: nil,
			Commit:  contentCommitView{Sha: commitOid, Message: message, Author: contentAuthor(), Committer: contentAuthor()},
		})
	}

	return restErr(405, "Method Not Allowed")
}
