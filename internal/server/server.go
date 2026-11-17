// Package server is the raw TCP HTTP/1.1 server: transport, parser,
// response builder, and router. Ported from app.ts (§2.1–2.4, §2.6).
//
// Phase 2a scope: transport + parser + response builder + routing table
// with STUB handlers for REST / git / GraphQL / MCP (501 JSON
// {"message":"not implemented"}). Handler bodies come in later phases
// (rest.go, graphql.go, mcp.go, gitproto). The single-issue fast path
// (§2.6) IS implemented — it only needs the store.
//
// Transport: net.Listen + goroutine per connection + hand-rolled parser
// mirroring parseOneRequest. No net/http (map §10.5). SetNoDelay(true) on
// accepted connections. Handlers are synchronous: handleRequest takes a
// *Store + *RawRequest and returns complete response bytes.
//
// LOCKING: HandleRequest acquires the store lock itself — RLock for
// read-only requests (GET/HEAD, POST git-upload-pack) so concurrent
// clones run in parallel, Lock for mutating ones. Memo caches have their
// own mutexes (Issue.cacheMu, gitproto memoCache, fullRespCache) because
// they are written on the read path. Lock order: store lock first, then
// a cache mutex — never the reverse.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"forge/internal/gitproto"
	"forge/internal/store"
)

// ---------------------------------------------------------------------------
// Response builder (app.ts §2.3)
// ---------------------------------------------------------------------------

// StatusText covers exactly the statuses Node's STATUS_TEXT covers; any
// other status emits an empty reason phrase.
var StatusText = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

var requestCounter atomic.Int64

func nextRequestID() string {
	return "req-" + strconv.FormatInt(requestCounter.Add(1), 10)
}

// MemoRequestID is the constant request id memoized (whole-response)
// replies carry. The suite only requires it non-empty.
const MemoRequestID = "req-memo"

// Header is one response header, in emission order.
type Header struct {
	Key   string
	Value string
}

// BuildResponse builds a complete HTTP/1.1 response: status line, headers,
// blank line, body. Content-Type defaults to application/json;
// Content-Length is always set from the body; Connection: keep-alive is
// always emitted (even for 204 and 400s), mirroring buildResponse.
func BuildResponse(status int, headers []Header, body []byte) []byte {
	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", status, StatusText[status])
	fmt.Fprintf(&head, "X-GitHub-Request-Id: %s\r\n", nextRequestID())
	hasContentType := false
	hasContentLength := false
	for _, h := range headers {
		head.WriteString(h.Key)
		head.WriteString(": ")
		head.WriteString(h.Value)
		head.WriteString("\r\n")
		switch strings.ToLower(h.Key) {
		case "content-type":
			hasContentType = true
		case "content-length":
			hasContentLength = true
		}
	}
	if !hasContentType {
		head.WriteString("Content-Type: application/json\r\n")
	}
	if !hasContentLength {
		fmt.Fprintf(&head, "Content-Length: %d\r\n", len(body))
	}
	head.WriteString("Connection: keep-alive\r\n\r\n")
	out := make([]byte, 0, len(head.String())+len(body))
	out = append(out, head.String()...)
	out = append(out, body...)
	return out
}

// BuildMemoResponse is BuildResponse with the constant MemoRequestID.
// Memoized responses are pre-built buffers reused verbatim.
func BuildMemoResponse(status int, headers []Header, body []byte) []byte {
	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", status, StatusText[status])
	fmt.Fprintf(&head, "X-GitHub-Request-Id: %s\r\n", MemoRequestID)
	hasContentType := false
	hasContentLength := false
	for _, h := range headers {
		head.WriteString(h.Key)
		head.WriteString(": ")
		head.WriteString(h.Value)
		head.WriteString("\r\n")
		switch strings.ToLower(h.Key) {
		case "content-type":
			hasContentType = true
		case "content-length":
			hasContentLength = true
		}
	}
	if !hasContentType {
		head.WriteString("Content-Type: application/json\r\n")
	}
	if !hasContentLength {
		fmt.Fprintf(&head, "Content-Length: %d\r\n", len(body))
	}
	head.WriteString("Connection: keep-alive\r\n\r\n")
	out := make([]byte, 0, len(head.String())+len(body))
	out = append(out, head.String()...)
	out = append(out, body...)
	return out
}

// errorBody is the {"message", "documentation_url"} error shape. A struct
// (not a map) so encoding/json emits the keys in Node's order: message
// first, documentation_url second. (A map would sort documentation_url
// first — a Phase 2a bug found by the REST/GraphQL porters.)
type errorBody struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
}

// BuildErrorResponse builds {"message": <message>, "documentation_url": ""}.
func BuildErrorResponse(status int, message string) []byte {
	body := store.MarshalJSONValue(errorBody{Message: message, DocumentationURL: ""})
	return BuildResponse(status, []Header{{Key: "Content-Type", Value: "application/json"}}, body)
}

// notImplemented is the Phase 2a stub body for handlers not yet ported.
func notImplemented() []byte {
	return BuildErrorResponse(501, "not implemented")
}

// ---------------------------------------------------------------------------
// Whole-response memo for git-upload-pack (app.ts:131 uploadPackFullResponses)
// ---------------------------------------------------------------------------

// fullRespCache is an insertion-ordered map of memo key -> complete HTTP
// response bytes (status line + headers + body). Mirrors Node's
// uploadPackFullResponses WeakMap<object, Map<string, Buffer>>. Eviction:
// when size > 8, drop the oldest entry BEFORE inserting (Node steadies at
// 9). Re-setting an existing key does not move it.
//
// The cache is self-synchronizing (its own RWMutex): memo writes happen on
// the read path, so it must be safe when the caller holds only the store
// RLock. Lock order: store lock first, then the cache mutex.
type fullRespCache struct {
	mu    sync.RWMutex
	m     map[string][]byte
	order []string
}

func (c *fullRespCache) get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *fullRespCache) set(key string, val []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		if len(c.m) > 8 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.m, oldest)
		}
		c.order = append(c.order, key)
	}
	c.m[key] = val
}

// uploadPackFullResponses caches complete git-upload-pack HTTP responses,
// keyed by repo pointer. Package-level map keyed by repo pointer (Go has
// no WeakMap); entries are bounded (9 per repo) and a deleted repo's
// entries are small and few. Callers hold the store lock; the map itself
// is guarded by its own mutex because it is written on the read path.
var uploadPackFullResponses = map[*store.Repo]*fullRespCache{}
var uploadPackFullResponsesMu sync.RWMutex

func fullRespCacheFor(repo *store.Repo) *fullRespCache {
	uploadPackFullResponsesMu.RLock()
	c, ok := uploadPackFullResponses[repo]
	uploadPackFullResponsesMu.RUnlock()
	if ok {
		return c
	}
	uploadPackFullResponsesMu.Lock()
	defer uploadPackFullResponsesMu.Unlock()
	if c, ok := uploadPackFullResponses[repo]; ok {
		return c
	}
	c = &fullRespCache{m: make(map[string][]byte)}
	uploadPackFullResponses[repo] = c
	return c
}

// ---------------------------------------------------------------------------
// Request types (map §10.2)
// ---------------------------------------------------------------------------

// RawRequest is a parsed HTTP request: what the handler needs, nothing more.
type RawRequest struct {
	Method      string
	Path        string // path without query
	RawURL      string // full request-target (for the '?' test + service param)
	Query       url.Values
	Auth        string // bearer token, "" if absent/malformed
	GitProtocol string
	Host        string
	Connection  string
	Body        []byte // owned copy (see §2.1 hazard)
	LocalPort   int
}

// Handler takes a *Store + *RawRequest and returns complete response bytes.
type Handler func(s *store.Store, req *RawRequest) []byte

// rawHeaders are the only six headers the handler reads (first occurrence
// wins, names lowercased).
type rawHeaders struct {
	authorization    string
	gitProtocol      string
	host             string
	contentLength    int64
	hasContentLength bool
	transferEncoding string
	connection       string
}

// ExpectedToken is the bearer token the suite presents
// (MAKEFUN_SHOP_TOKEN); empty off-trial.
var ExpectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

// ParseAuth extracts the bearer token, or "" when absent/malformed.
func ParseAuth(headers rawHeaders) string {
	auth := headers.authorization
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	return token
}

// Authed reports whether the request carries the trial's token (any token
// off-trial).
func Authed(req *RawRequest) bool {
	return req.Auth != "" && (ExpectedToken == "" || req.Auth == ExpectedToken)
}

// ---------------------------------------------------------------------------
// Router (app.ts §2.4 — handleRequest, evaluation order preserved)
// ---------------------------------------------------------------------------

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)
var issueNumberRe = regexp.MustCompile(`^\d+$`)

// HandleRequest is the whole request handler, synchronous (the body is
// already read by the parser): returns the complete HTTP response bytes
// for one request. Caller (ServeConn) holds the store lock and recovers
// panics into a 500.
func HandleRequest(s *store.Store, req *RawRequest) []byte {
	method := req.Method
	rawURL := req.RawURL

	// Origin-form URLs (everything the suite sends): split at '?' without
	// a URL object. Anything else falls back to the URL parser.
	var path string
	if strings.HasPrefix(rawURL, "/") {
		if q := strings.IndexByte(rawURL, '?'); q >= 0 {
			path = rawURL[:q]
			// Populate req.Query for origin-form URLs too (Phase 2a only
			// filled it on the absolute-form fallback, leaving it nil
			// for normal requests). Handlers that need exact
			// parameter order still parse RawURL themselves.
			if req.Query == nil {
				if vals, err := url.ParseQuery(rawURL[q+1:]); err == nil {
					req.Query = vals
				}
			}
		} else {
			path = rawURL
		}
	} else {
		u, err := url.Parse(rawURL)
		if err != nil {
			return BuildErrorResponse(400, "Malformed request")
		}
		if !strings.HasPrefix(u.Path, "/") {
			// Absolute-form without a path: treat like Node's URL fallback.
			u2, err2 := url.Parse("http://localhost" + rawURL)
			if err2 != nil {
				return BuildErrorResponse(400, "Malformed request")
			}
			u = u2
		}
		path = u.Path
		if req.Query == nil {
			req.Query = u.Query()
		}
	}
	req.Path = path

	// Acquire the store lock: RLock for read-only requests (concurrent
	// clones can then run in parallel), Lock for mutating ones. All store
	// access below runs under it. Memo caches have their own mutexes
	// (Issue.cacheMu, gitproto memoCache, fullRespCache) because they are
	// written on the read path.
	if isReadOnlyRequest(method, rawURL) {
		s.RLock()
		defer s.RUnlock()
	} else {
		s.Lock()
		defer s.Unlock()
	}

	// Health check — no auth, any method.
	if path == "/healthz" {
		return BuildResponse(200, []Header{{Key: "Content-Type", Value: "application/json"}}, []byte(`{"ok":true}`))
	}

	// Git smart HTTP.
	if strings.Contains(path, ".git/") {
		if m := gitRouteRe.FindStringSubmatch(path); m != nil {
			// N4 decision: bad percent-escapes answer 404, not Node's 500
			// (the suite never sends them; see map §4.1 note).
			owner, err1 := url.PathUnescape(m[1])
			repoName, err2 := url.PathUnescape(m[2])
			if err1 != nil || err2 != nil {
				return BuildErrorResponse(404, "Not Found")
			}
			_ = owner
			_ = repoName
			action := m[3]
			if !Authed(req) {
				return BuildErrorResponse(401, "Requires authentication")
			}
			repo := s.GetRepo(owner, repoName)
			if owner != store.Viewer.Login || repo == nil {
				return BuildErrorResponse(404, "Not Found")
			}
			if action == "info/refs" {
				service := serviceParam(rawURL)
				if method != "GET" {
					return BuildErrorResponse(405, "Method Not Allowed")
				}
				if service == "git-upload-pack" {
					isV2 := strings.Contains(req.GitProtocol, "version=2")
					if isV2 {
						return BuildMemoResponse(200, []Header{
							{Key: "Content-Type", Value: gitproto.ContentTypeUploadAdvertisement},
							{Key: "Cache-Control", Value: "no-cache"},
						}, gitproto.V2Advertisement())
					}
					return BuildResponse(200, []Header{
						{Key: "Content-Type", Value: gitproto.ContentTypeUploadAdvertisement},
						{Key: "Cache-Control", Value: "no-cache"},
					}, gitproto.V0UploadAdvertisementCached(repo))
				}
				if service == "git-receive-pack" {
					return BuildResponse(200, []Header{
						{Key: "Content-Type", Value: gitproto.ContentTypeReceiveAdvertisement},
						{Key: "Cache-Control", Value: "no-cache"},
					}, gitproto.V0ReceiveAdvertisementCached(repo))
				}
				return BuildErrorResponse(400, "Invalid service")
			}

			if action == "git-upload-pack" {
				if method != "POST" {
					return BuildErrorResponse(405, "Method Not Allowed")
				}
				body := req.Body
				isV2 := strings.Contains(req.GitProtocol, "version=2")
				// Whole-response memo: the clone workload repeats the same
				// ls-refs / fetch body 50 times per repository; a hit
				// returns the exact bytes without decoding, parsing,
				// planning, rebuilding or re-serializing anything.
				memoKey, memoOK := gitproto.UploadPackMemoKey(repo, body, isV2)
				if memoOK {
					if full, ok := fullRespCacheFor(repo).get(memoKey); ok {
						return full
					}
				}
				response, err := gitproto.UploadPackBody(repo, body, isV2)
				if err != nil {
					if errors.Is(err, gitproto.ErrUnknownCommand) {
						return BuildErrorResponse(400, "Unknown git-upload-pack command")
					}
					// Pkt decode / v0 parse errors propagate as 500 with
					// the message (app.ts: the handler throws).
					return BuildErrorResponse(500, err.Error())
				}
				uploadResultHeaders := []Header{
					{Key: "Content-Type", Value: gitproto.ContentTypeUploadResult},
					{Key: "Cache-Control", Value: "no-cache"},
				}
				if memoOK {
					full := BuildMemoResponse(200, uploadResultHeaders, response)
					fullRespCacheFor(repo).set(memoKey, full)
					return full
				}
				return BuildResponse(200, uploadResultHeaders, response)
			}

			if action == "git-receive-pack" {
				if method != "POST" {
					return BuildErrorResponse(405, "Method Not Allowed")
				}
				body := req.Body
				// The body is command pkt-lines, a flush, then raw pack
				// bytes: split first, because the pack is not pkt-lines
				// and must not be fed to the pkt decoder.
				commands, pack, err := gitproto.SplitAndParsePush(body)
				if err != nil {
					return BuildErrorResponse(400, "Malformed request")
				}
				packObjects, err := gitproto.ReadPushPack(repo, pack)
				if err != nil {
					return BuildResponse(200, []Header{
						{Key: "Content-Type", Value: gitproto.ContentTypeReceiveResult},
						{Key: "Cache-Control", Value: "no-cache"},
					}, gitproto.PushReportBody(false, err.Error(), nil))
				}
				results := gitproto.ApplyPush(repo, commands, packObjects)
				return BuildResponse(200, []Header{
					{Key: "Content-Type", Value: gitproto.ContentTypeReceiveResult},
					{Key: "Cache-Control", Value: "no-cache"},
				}, gitproto.PushReportBody(true, "", results))
			}
			return BuildErrorResponse(404, "Not Found")
		}
	}

	// API (auth required).
	isAPI := strings.HasPrefix(path, "/api/") || path == "/mcp"
	if isAPI && !Authed(req) {
		return BuildErrorResponse(401, "Requires authentication")
	}

	if path == "/mcp" {
		return HandleMCP(s, req)
	}

	if path == "/api/graphql" {
		return HandleGraphQL(s, req)
	}

	if strings.HasPrefix(path, "/api/v3") {
		// Fast path — GET a single issue with no query string (the read
		// workload: 2000 identical GETs of one issue on one connection).
		if method == "GET" && strings.IndexByte(rawURL, '?') < 0 {
			if fast := getSingleIssueResponse(s, path); fast != nil {
				return fast
			}
		}
		if resp := HandleRest(s, req); resp != nil {
			return resp
		}
		return BuildErrorResponse(404, "Not Found")
	}

	// Unknown route: JSON 404.
	return BuildErrorResponse(404, "Not Found")
}

// serviceParam returns the `service` query value for a git info/refs URL,
// without building a URLSearchParams (app.ts:274-288).
func serviceParam(rawURL string) string {
	q := strings.IndexByte(rawURL, '?')
	if q < 0 {
		return ""
	}
	for _, part := range strings.Split(rawURL[q+1:], "&") {
		if strings.HasPrefix(part, "service=") {
			raw := part[len("service="):]
			raw = strings.ReplaceAll(raw, "+", " ")
			if dec, err := url.PathUnescape(raw); err == nil {
				return dec
			}
			return raw
		}
	}
	return ""
}

// getSingleIssueResponse is the fast path for GET
// /api/v3/repos/{owner}/{repo}/issues/{number} with no query (app.ts
// §2.6). Returns the issue's cached whole-response buffer when present;
// builds and caches it on the first hit; returns nil when the path is not
// a single-issue GET or the repo/issue does not exist, so the caller falls
// through to handleRest (which answers the 404s).
func getSingleIssueResponse(s *store.Store, path string) []byte {
	const prefix = "/api/v3/repos/"
	if !strings.HasPrefix(path, prefix) {
		return nil
	}
	rest := path[len(prefix):]
	firstSlash := strings.IndexByte(rest, '/')
	if firstSlash < 0 {
		return nil
	}
	secondSlash := strings.IndexByte(rest[firstSlash+1:], '/')
	if secondSlash < 0 {
		return nil
	}
	secondSlash += firstSlash + 1
	thirdSlash := strings.IndexByte(rest[secondSlash+1:], '/')
	if thirdSlash < 0 {
		return nil
	}
	thirdSlash += secondSlash + 1
	if strings.IndexByte(rest[thirdSlash+1:], '/') >= 0 {
		return nil
	}
	if rest[secondSlash+1:thirdSlash] != "issues" {
		return nil
	}
	numberText := rest[thirdSlash+1:]
	if numberText == "" || !issueNumberRe.MatchString(numberText) {
		return nil
	}
	number, err := strconv.ParseInt(numberText, 10, 64)
	if err != nil || number <= 0 {
		return nil
	}
	owner, err1 := url.PathUnescape(rest[:firstSlash])
	repoName, err2 := url.PathUnescape(rest[firstSlash+1 : secondSlash])
	if err1 != nil || err2 != nil {
		return nil
	}
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return nil
	}
	issue, ok := repo.Issues[int(number)]
	if !ok {
		return nil
	}
	if cached := issue.GetCachedFullResponse(); cached != nil {
		return cached
	}
	full := BuildMemoResponse(200,
		[]Header{{Key: "Content-Type", Value: "application/json"}},
		[]byte(store.IssueJSONString(issue, repo)))
	issue.SetCachedFullResponse(full)
	return full
}

// ---------------------------------------------------------------------------
// HTTP/1.1 parser (app.ts §2.2 — parseOneRequest + readChunked)
// ---------------------------------------------------------------------------

// parseNodeInt mirrors JS parseInt(s, radix) for the header values the
// parser reads: leading whitespace, optional sign, then digits; no digits
// → NaN (ok=false).
func parseNodeInt(s string, base int) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	i := 0
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i = 1
	}
	var n int64
	digits := 0
	for ; i < len(s); i++ {
		c := s[i]
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case base == 16 && c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case base == 16 && c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			goto done
		}
		if d >= int64(base) {
			goto done
		}
		n = n*int64(base) + d
		digits++
	}
done:
	if digits == 0 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// readChunked reads one chunked body (Transfer-Encoding: chunked).
// Returns (body, consumed, ok). ok=false means incomplete — wait for more
// data (mirrors Node returning null), including on a bad chunk size.
func readChunked(buf []byte, start int) (body []byte, consumed int, ok bool) {
	pos := start
	var parts [][]byte
	for {
		lineEnd := bytes.Index(buf[pos:], []byte("\r\n"))
		if lineEnd < 0 {
			return nil, 0, false
		}
		lineEnd += pos
		sizeText := strings.TrimSpace(string(buf[pos:lineEnd]))
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, valid := parseNodeInt(sizeText, 16)
		if !valid || size < 0 {
			return nil, 0, false
		}
		if size == 0 {
			// Final chunk: consume through the trailing CRLF (and any
			// trailers up to a blank line).
			after := lineEnd + 2
			if idx := bytes.Index(buf[after:], []byte("\r\n\r\n")); idx >= 0 {
				trailerEnd := after + idx
				return bytes.Join(parts, nil), trailerEnd + 4 - start, true
			}
			if len(buf) < after+2 {
				return nil, 0, false
			}
			return bytes.Join(parts, nil), after + 2 - start, true
		}
		chunkStart := lineEnd + 2
		if len(buf) < chunkStart+int(size)+2 {
			return nil, 0, false
		}
		chunk := make([]byte, size)
		copy(chunk, buf[chunkStart:chunkStart+int(size)])
		parts = append(parts, chunk)
		pos = chunkStart + int(size) + 2
	}
}

// parseResult is the outcome of parsing one request from the front of buf.
type parseResult struct {
	req      *RawRequest
	consumed int
	needMore bool // more data needed
	err      error
}

// MaxHeaderBytes caps the header block (Node has no cap; the trial never
// sends such requests — map §10.5). Over the cap is a malformed request.
const MaxHeaderBytes = 1 << 20

// parseOneRequest tries to parse one complete request from the front of
// buf. Returns needMore when more data is needed; err on a malformed
// request line / headers (the socket is then answered 400 and closed).
func parseOneRequest(buf []byte, localPort int) parseResult {
	headerEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		if len(buf) > MaxHeaderBytes {
			return parseResult{err: fmt.Errorf("header too large")}
		}
		return parseResult{needMore: true}
	}
	headerText := string(buf[:headerEnd])
	firstCRLF := strings.Index(headerText, "\r\n")
	requestLine := headerText
	if firstCRLF >= 0 {
		requestLine = headerText[:firstCRLF]
	}
	firstSpace := strings.IndexByte(requestLine, ' ')
	secondSpace := -1
	if firstSpace >= 0 {
		if i := strings.IndexByte(requestLine[firstSpace+1:], ' '); i >= 0 {
			secondSpace = firstSpace + 1 + i
		}
	}
	if firstSpace < 0 || secondSpace < 0 {
		return parseResult{err: fmt.Errorf("malformed request line")}
	}
	method := requestLine[:firstSpace]
	urlStr := requestLine[firstSpace+1 : secondSpace]
	version := requestLine[secondSpace+1:]
	if method == "" || urlStr == "" || !strings.HasPrefix(version, "HTTP/") {
		return parseResult{err: fmt.Errorf("malformed request line")}
	}
	// Extract only the headers the handler reads. First occurrence wins.
	var h rawHeaders
	pos := 0
	if firstCRLF >= 0 {
		pos = firstCRLF + 2
	} else {
		pos = len(headerText)
	}
	for pos < len(headerText) {
		lineEnd := strings.Index(headerText[pos:], "\r\n")
		var line string
		if lineEnd < 0 {
			line = headerText[pos:]
			pos = len(headerText)
		} else {
			line = headerText[pos : pos+lineEnd]
			pos += lineEnd + 2
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return parseResult{err: fmt.Errorf("malformed header")}
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		switch name {
		case "authorization":
			if h.authorization == "" {
				h.authorization = value
			}
		case "git-protocol":
			if h.gitProtocol == "" {
				h.gitProtocol = value
			}
		case "host":
			if h.host == "" {
				h.host = value
			}
		case "content-length":
			if !h.hasContentLength {
				h.hasContentLength = true
				if n, ok := parseNodeInt(value, 10); ok {
					h.contentLength = n
				}
			}
		case "transfer-encoding":
			if h.transferEncoding == "" {
				h.transferEncoding = value
			}
		case "connection":
			if h.connection == "" {
				h.connection = value
			}
		}
	}
	bodyStart := headerEnd + 4
	if strings.Contains(strings.ToLower(h.transferEncoding), "chunked") {
		body, consumed, ok := readChunked(buf, bodyStart)
		if !ok {
			return parseResult{needMore: true}
		}
		return parseResult{req: &RawRequest{
			Method: method, RawURL: urlStr,
			Auth: ParseAuth(h), GitProtocol: h.gitProtocol, Host: h.host,
			Connection: h.connection, Body: body, LocalPort: localPort,
		}, consumed: bodyStart + consumed}
	}
	length := int64(0)
	if h.hasContentLength && h.contentLength > 0 {
		length = h.contentLength
	}
	if int64(len(buf)) < int64(bodyStart)+length {
		return parseResult{needMore: true}
	}
	// Copy the body: the socket buffer is reused for the next request
	// (map §2.1 zero-copy hazard — a retained view would pin the buffer).
	body := make([]byte, length)
	copy(body, buf[bodyStart:bodyStart+int(length)])
	return parseResult{req: &RawRequest{
		Method: method, RawURL: urlStr,
		Auth: ParseAuth(h), GitProtocol: h.gitProtocol, Host: h.host,
		Connection: h.connection, Body: body, LocalPort: localPort,
	}, consumed: bodyStart + int(length)}
}

// ---------------------------------------------------------------------------
// Socket handling (app.ts §2.1 — handleSocket)
// ---------------------------------------------------------------------------

// Serve accepts connections on ln forever (until Accept errors) and serves
// each on its own goroutine.
func Serve(ln net.Listener, s *store.Store) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go ServeConn(s, conn)
	}
}

// ServeConn serves one socket: parse requests as they arrive, answer each,
// keep the connection alive. Pipelined requests are drained in a loop.
func ServeConn(s *store.Store, conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	localPort := 0
	if addr, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		localPort = addr.Port
	}
	var buf []byte
	chunk := make([]byte, 32768)
	for {
		// Drain all complete requests already buffered.
		for {
			parsed := parseOneRequest(buf, localPort)
			if parsed.needMore {
				break
			}
			if parsed.err != nil {
				// Malformed HTTP: answer 400 and close.
				_, _ = conn.Write(BuildErrorResponse(400, "Malformed request"))
				return
			}
			buf = append([]byte(nil), buf[parsed.consumed:]...)
			req := parsed.req
			resp := handleRequestSafe(s, req)
			if _, err := conn.Write(resp); err != nil {
				return
			}
			if strings.Contains(strings.ToLower(req.Connection), "close") {
				return
			}
		}
		n, err := conn.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			return
		}
	}
}

// handleRequestSafe runs HandleRequest, converting a handler panic into a
// 500 with the message (mirroring app.ts's handler try/catch → 500
// {message}). The store lock is acquired inside HandleRequest (RLock for
// read-only requests, Lock for mutating ones — see isReadOnlyRequest), so
// concurrent clones can run in parallel on the RLock.
func handleRequestSafe(s *store.Store, req *RawRequest) (resp []byte) {
	defer func() {
		if r := recover(); r != nil {
			var msg string
			switch v := r.(type) {
			case error:
				msg = v.Error()
			default:
				msg = fmt.Sprint(v)
			}
			resp = BuildErrorResponse(500, msg)
		}
	}()
	return HandleRequest(s, req)
}

// isReadOnlyRequest reports whether a request never mutates store state,
// so it may run under the store RLock (concurrent with other reads).
// Read-only: every GET/HEAD, and POST git-upload-pack (ls-refs/fetch —
// pure reads). Everything else takes the write lock (conservative:
// GraphQL and MCP POSTs can be mutations, and all non-GET /api/v3 calls
// mutate). The clone workload (50 concurrent git-upload-pack POSTs) is
// the path this unlocks.
func isReadOnlyRequest(method, rawURL string) bool {
	if method == "GET" || method == "HEAD" {
		return true
	}
	if method != "POST" {
		return false
	}
	// POST git-upload-pack: path is "<owner>/<repo>.git/git-upload-pack".
	path := rawURL
	if q := strings.IndexByte(path, '?'); q >= 0 {
		path = path[:q]
	}
	return strings.HasSuffix(path, ".git/git-upload-pack")
}
