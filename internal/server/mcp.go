// Package server — MCP layer (mcp.go).
//
// Ported from mcp.ts (286 lines; map §7): JSON-RPC 2.0 over Streamable
// HTTP, six tools, exact schemas, exact error codes/messages.
//
// One correction to the map: map §7.3 says structuredContent is
// `{value: {...}}`, but mcp.ts passes `outcome.value` (not `outcome`) to
// toolResult, so structuredContent — and the text block — hold the view
// object itself (repoJson / issueJson / pullJson / the merged object).
// This is confirmed by checks-mcp.ts, which reads
// `structuredContent.number` directly. The code below follows mcp.ts.
//
// tools/list byte-identity (map §10.2): the `{"tools":[...]}` result
// object is serialised once at startup (sync.Once) and reused verbatim;
// only the request id varies per response, so responses are assembled
// by byte concatenation around the cached fragment.
//
// Wiring: server.go's router currently stubs POST /mcp. The parent wires
// it with HandleMCP (full route handler, Handler signature) or with
// HandleMcpBody / HandleMcpRequest / CallTool.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"forge/internal/store"
)

// MCP_PROTOCOL_VERSIONS (mcp.ts:5), in negotiation order.
var mcpProtocolVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ---------------------------------------------------------------------------
// Tools (MCP_TOOLS, mcp.ts:24-110) — exact names, descriptions, property
// order, types and required lists. Built once as ordered objects so the
// serialised form is stable.
// ---------------------------------------------------------------------------

func mcpProp(typ, desc string) *orderedObj {
	return newOrderedObj().set("type", typ).set("description", desc)
}

func mcpTool(name, desc string, props *orderedObj, required []string) *orderedObj {
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return newOrderedObj().
		set("name", name).
		set("description", desc).
		set("inputSchema", newOrderedObj().
			set("type", "object").
			set("properties", props).
			set("required", req))
}

func buildMCPTools() []*orderedObj {
	return []*orderedObj{
		mcpTool("create_repo", "Create a new repository for the authenticated user.",
			newOrderedObj().
				set("name", mcpProp("string", "The name of the repository.")).
				set("description", mcpProp("string", "A short description of the repository.")).
				set("private", mcpProp("boolean", "Whether the repository is private.")),
			[]string{"name"}),
		mcpTool("get_repo", "Get a repository by owner and name.",
			newOrderedObj().
				set("owner", mcpProp("string", "The owner of the repository.")).
				set("repo", mcpProp("string", "The name of the repository.")),
			[]string{"owner", "repo"}),
		mcpTool("create_issue", "Create an issue in a repository.",
			newOrderedObj().
				set("owner", mcpProp("string", "The owner of the repository.")).
				set("repo", mcpProp("string", "The name of the repository.")).
				set("title", mcpProp("string", "The title of the issue.")).
				set("body", mcpProp("string", "The body of the issue.")),
			[]string{"owner", "repo", "title"}),
		mcpTool("list_issues", "List issues in a repository.",
			newOrderedObj().
				set("owner", mcpProp("string", "The owner of the repository.")).
				set("repo", mcpProp("string", "The name of the repository.")).
				set("state", mcpProp("string", "Filter issues by state: open, closed, or all.")),
			[]string{"owner", "repo"}),
		mcpTool("create_pull_request", "Create a pull request in a repository.",
			newOrderedObj().
				set("owner", mcpProp("string", "The owner of the repository.")).
				set("repo", mcpProp("string", "The name of the repository.")).
				set("title", mcpProp("string", "The title of the pull request.")).
				set("head", mcpProp("string", "The name of the branch where the changes are implemented.")).
				set("base", mcpProp("string", "The name of the branch the changes are pulled into.")).
				set("body", mcpProp("string", "The body of the pull request.")),
			[]string{"owner", "repo", "title", "head", "base"}),
		mcpTool("merge_pull_request", "Merge a pull request in a repository.",
			newOrderedObj().
				set("owner", mcpProp("string", "The owner of the repository.")).
				set("repo", mcpProp("string", "The name of the repository.")).
				set("pull_number", mcpProp("integer", "The number of the pull request.")),
			[]string{"owner", "repo", "pull_number"}),
	}
}

// mcpToolsRequired lists the required properties per tool, in MCP_TOOLS
// order, for missingArg checks.
var mcpToolsRequired = [][]string{
	{"name"},
	{"owner", "repo"},
	{"owner", "repo", "title"},
	{"owner", "repo"},
	{"owner", "repo", "title", "head", "base"},
	{"owner", "repo", "pull_number"},
}

var mcpToolsNames = []string{
	"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request",
}

// mcpToolsListResult is the serialised `{"tools":[...]}` result object,
// built once (sync.Once) so every tools/list answer is byte-identical.
var (
	mcpToolsListResult []byte
	mcpToolsOnce       sync.Once
)

func toolsListResultBytes() []byte {
	mcpToolsOnce.Do(func() {
		tools := buildMCPTools()
		toolAny := make([]any, len(tools))
		for i, t := range tools {
			toolAny[i] = t
		}
		mcpToolsListResult = store.MarshalJSONValue(newOrderedObj().set("tools", toolAny))
	})
	return mcpToolsListResult
}

// ---------------------------------------------------------------------------
// JSON-RPC envelopes (mcp.ts:113-139)
// ---------------------------------------------------------------------------

// rpcResultObj builds {jsonrpc:'2.0', id, result}.
func rpcResultObj(id any, result any) *orderedObj {
	return newOrderedObj().set("jsonrpc", "2.0").set("id", id).set("result", result)
}

// rpcErrorObj builds {jsonrpc:'2.0', id, error:{code, message}}.
func rpcErrorObj(id any, code int, message string) *orderedObj {
	return newOrderedObj().set("jsonrpc", "2.0").set("id", id).
		set("error", newOrderedObj().set("code", code).set("message", message))
}

// toolResultObj builds a successful tool envelope: the text block is the
// JSON of the value, structuredContent is the value itself (mcp.ts:
// toolResult(id, outcome.value)).
func toolResultObj(id any, value any) *orderedObj {
	text, _ := marshalJSONValue(value)
	return newOrderedObj().set("jsonrpc", "2.0").set("id", id).
		set("result", newOrderedObj().
			set("content", []any{newOrderedObj().set("type", "text").set("text", string(text))}).
			set("structuredContent", value))
}

// toolErrorObj builds a tool-error envelope (HTTP 200, isError: true).
func toolErrorObj(id any, message string) *orderedObj {
	errText, _ := marshalJSONValue(newOrderedObj().set("error", message))
	return newOrderedObj().set("jsonrpc", "2.0").set("id", id).
		set("result", newOrderedObj().
			set("content", []any{newOrderedObj().set("type", "text").set("text", string(errText))}).
			set("structuredContent", newOrderedObj().set("error", message)).
			set("isError", true))
}

// ---------------------------------------------------------------------------
// callTool (mcp.ts:155-194)
// ---------------------------------------------------------------------------

// ToolError is a callTool failure: {error, code} in mcp.ts. The code is
// -32602 for every tool-level failure; handleMcpRequest renders it as a
// tool error (isError: true, HTTP 200), never as an RPC error.
type ToolError struct {
	Msg  string
	Code int
}

func (e *ToolError) Error() string { return e.Msg }

func toolErr(msg string) *ToolError { return &ToolError{Msg: msg, Code: -32602} }

// missingArg mirrors mcp.ts missingArg: undefined, null or ” counts as
// missing (0 and false count as present).
func missingArg(required []string, args map[string]any) string {
	for _, req := range required {
		v, ok := args[req]
		if !ok || v == nil {
			return req
		}
		if s, ok := v.(string); ok && s == "" {
			return req
		}
	}
	return ""
}

// CallTool runs one tool by exact name (mcp.ts:155-194). It returns the
// tool's value — the object toolResult wraps as text + structuredContent
// — or a *ToolError. Unknown tool names are an error here too (the
// handleMcpRequest pre-check normally intercepts them first).
func CallTool(s *store.Store, name string, args map[string]any) (any, *ToolError) {
	idx := -1
	for i, n := range mcpToolsNames {
		if n == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, toolErr(fmt.Sprintf("Unknown tool: %s", name))
	}
	if missing := missingArg(mcpToolsRequired[idx], args); missing != "" {
		return nil, toolErr(fmt.Sprintf("Missing required argument: %s", missing))
	}

	switch name {
	case "create_repo":
		repo, err := s.CreateRepo(
			jsString(args["name"]),
			args["private"] == true,
			func() string {
				if d, ok := args["description"].(string); ok {
					return d
				}
				return ""
			}(),
			"main",
		)
		if err != nil {
			return nil, toolErr(err.Error())
		}
		return s.RepoJSON(repo), nil
	case "get_repo":
		repo := s.GetRepo(jsString(args["owner"]), jsString(args["repo"]))
		if repo == nil {
			return nil, toolErr("Not Found")
		}
		return s.RepoJSON(repo), nil
	case "create_issue":
		repo := s.GetRepo(jsString(args["owner"]), jsString(args["repo"]))
		if repo == nil {
			return nil, toolErr("Not Found")
		}
		body := ""
		if b, ok := args["body"].(string); ok {
			body = b
		}
		issue, err := s.CreateIssue(repo, jsString(args["title"]), body, []string{})
		if err != nil {
			return nil, toolErr(err.Error())
		}
		return store.IssueJSON(issue, repo), nil
	case "list_issues":
		repo := s.GetRepo(jsString(args["owner"]), jsString(args["repo"]))
		if repo == nil {
			return nil, toolErr("Not Found")
		}
		state := "open"
		if st, ok := args["state"].(string); ok {
			state = st
		}
		issues := make([]*store.Issue, 0, len(repo.Issues))
		for _, iss := range repo.Issues {
			issues = append(issues, iss)
		}
		// Any state other than open/closed (including 'all' and
		// garbage) means no filter — unlike REST, where it is a 422.
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
		}
		// Sort number desc (numbers are unique, so the order is fully
		// determined; sort.Slice is fine).
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number > issues[j].Number })
		views := make([]any, 0, len(issues))
		for _, iss := range issues {
			views = append(views, store.IssueJSON(iss, repo))
		}
		return views, nil
	case "create_pull_request":
		repo := s.GetRepo(jsString(args["owner"]), jsString(args["repo"]))
		if repo == nil {
			return nil, toolErr("Not Found")
		}
		body := ""
		if b, ok := args["body"].(string); ok {
			body = b
		}
		pull, err := s.CreatePull(repo, jsString(args["title"]), body,
			jsString(args["head"]), jsString(args["base"]))
		if err != nil {
			return nil, toolErr(err.Error())
		}
		return pullViewObj(s.PullJSON(repo, pull)), nil
	case "merge_pull_request":
		repo := s.GetRepo(jsString(args["owner"]), jsString(args["repo"]))
		if repo == nil {
			return nil, toolErr("Not Found")
		}
		num, isNaN := jsNumber(args["pull_number"])
		var pull *store.Pull
		if !isNaN && !math.IsInf(num, 0) && math.Trunc(num) == num {
			pull = repo.Pulls[int(num)]
		}
		if pull == nil {
			return nil, toolErr("Not Found")
		}
		sha, err := s.MergePull(repo, pull)
		if err != nil {
			return nil, toolErr(err.Error())
		}
		// {merged: true, sha, ...pullJson(...)} — spread after, so
		// pullJson's keys follow; pullJson has no 'sha' key, so value.sha
		// survives. Built as an ordered object in pullJson field order.
		pv := s.PullJSON(repo, pull)
		return pullJSONWithMergePrefix(pv, sha), nil
	}
	return nil, toolErr(fmt.Sprintf("Unknown tool: %s", name))
}

// pullHeadBaseObj builds a pull head/base object in pullJson field order.
// NOTE: store.PullJSON's Head/Base User is an OwnerRef with an empty Type,
// which marshals a stray `"type":""` — Node's pullJson head/base user is
// {login, id} only (a foundation store bug, reported separately). The
// ordered rebuild here omits it so MCP output matches Node exactly.
func pullHeadBaseObj(hb store.PullHeadBase) *orderedObj {
	return newOrderedObj().
		set("ref", hb.Ref).
		set("sha", hb.Sha).
		set("label", hb.Label).
		set("user", newOrderedObj().set("login", hb.User.Login).set("id", hb.User.ID))
}

// pullViewObj rebuilds a store.PullView as an ordered object in pullJson
// field order, without the stray "type":"" in head/base user (see above).
// The top-level user keeps its struct form: its Type is "User", as Node's.
func pullViewObj(pv store.PullView) *orderedObj {
	return newOrderedObj().
		set("id", pv.ID).
		set("node_id", pv.NodeID).
		set("number", pv.Number).
		set("title", pv.Title).
		set("body", pv.Body).
		set("state", pv.State).
		set("merged", pv.Merged).
		set("mergeable", pv.Mergeable).
		set("merge_commit_sha", pv.MergeCommitSha).
		set("merged_at", pv.MergedAt).
		set("head", pullHeadBaseObj(pv.Head)).
		set("base", pullHeadBaseObj(pv.Base)).
		set("user", pv.User).
		set("created_at", pv.CreatedAt).
		set("updated_at", pv.UpdatedAt).
		set("closed_at", pv.ClosedAt).
		set("html_url", pv.HTMLURL).
		set("url", pv.URL)
}

// pullJSONWithMergePrefix builds {merged:true, sha, ...pullJson} in
// pullJson field order (mcp.ts merge_pull_request success value).
func pullJSONWithMergePrefix(pv store.PullView, sha string) *orderedObj {
	out := newOrderedObj().set("merged", true).set("sha", sha)
	inner := pullViewObj(pv)
	for _, k := range inner.keys {
		out.set(k, inner.vals[k])
	}
	return out
}

// ---------------------------------------------------------------------------
// handleMcpRequest (mcp.ts:196-244)
// ---------------------------------------------------------------------------

// HandleMcpRequest handles one JSON-RPC request object and returns the
// serialised response bytes, or nil for a notification (no response).
// The id is echoed verbatim (string ids included); absent → null.
func HandleMcpRequest(s *store.Store, req map[string]any) []byte {
	var id any
	if v, ok := req["id"]; ok {
		id = v
	}
	method, _ := req["method"].(string)
	var params map[string]any
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	} else {
		params = map[string]any{}
	}

	if strings.HasPrefix(method, "notifications/") {
		return nil
	}

	switch method {
	case "server/discover":
		return store.MarshalJSONValue(rpcResultObj(id, newOrderedObj().
			set("supportedVersions", stringsToAny(mcpProtocolVersions)).
			set("capabilities", newOrderedObj().set("tools", newOrderedObj())).
			set("serverInfo", mcpServerInfo()).
			set("_meta", newOrderedObj().set("io.modelcontextprotocol/serverInfo", mcpServerInfo()))))
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		version := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if v == requested {
				version = v
				break
			}
		}
		return store.MarshalJSONValue(rpcResultObj(id, newOrderedObj().
			set("protocolVersion", version).
			set("capabilities", newOrderedObj().set("tools", newOrderedObj())).
			set("serverInfo", mcpServerInfo())))
	case "ping":
		return store.MarshalJSONValue(rpcResultObj(id, newOrderedObj()))
	case "tools/list":
		// Byte-identical every time: the cached result fragment is
		// spliced in; only the id varies.
		idBytes, _ := marshalJSONValue(id)
		var buf bytes.Buffer
		buf.WriteString(`{"jsonrpc":"2.0","id":`)
		buf.Write(idBytes)
		buf.WriteString(`,"result":`)
		buf.Write(toolsListResultBytes())
		buf.WriteString(`}`)
		return buf.Bytes()
	case "tools/call":
		toolName, _ := params["name"].(string)
		var args map[string]any
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		} else {
			args = map[string]any{}
		}
		if toolName == "" {
			return store.MarshalJSONValue(rpcErrorObj(id, -32602, "Missing tool name"))
		}
		known := false
		for _, n := range mcpToolsNames {
			if n == toolName {
				known = true
				break
			}
		}
		if !known {
			return store.MarshalJSONValue(rpcErrorObj(id, -32602, fmt.Sprintf("Unknown tool: %s", toolName)))
		}
		value, terr := CallTool(s, toolName, args)
		if terr != nil {
			return store.MarshalJSONValue(toolErrorObj(id, terr.Msg))
		}
		return store.MarshalJSONValue(toolResultObj(id, value))
	}
	return store.MarshalJSONValue(rpcErrorObj(id, -32601, fmt.Sprintf("Method not found: %s", method)))
}

func mcpServerInfo() *orderedObj {
	return newOrderedObj().set("name", "git-forge").set("version", "1.0.0")
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// handleMcpBody (mcp.ts:246-285)
// ---------------------------------------------------------------------------

// HandleMcpBody parses an MCP request body and returns (HTTP status,
// response body bytes). Body is nil for a notification (HTTP 202 with an
// empty body, per app.ts). Parse failure → 400 with JSON-RPC -32700;
// a non-object JSON value → 400 with -32600 Invalid Request.
func HandleMcpBody(s *store.Store, bodyText string) (int, []byte) {
	parsed, err := decodeJSONBody([]byte(bodyText))
	if err != nil {
		return 400, store.MarshalJSONValue(rpcErrorObj(nil, -32700, "Parse error"))
	}
	if arr, ok := parsed.([]any); ok {
		responses := make([]json.RawMessage, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if r := HandleMcpRequest(s, m); r != nil {
					responses = append(responses, r)
				}
			}
		}
		return 200, store.MarshalJSONValue(responses)
	}
	if m, ok := parsed.(map[string]any); ok {
		r := HandleMcpRequest(s, m)
		if r == nil {
			return 202, nil
		}
		return 200, r
	}
	return 400, store.MarshalJSONValue(rpcErrorObj(nil, -32600, "Invalid Request"))
}

// HandleMCP is the full /mcp route handler (app.ts:481-492): DELETE →
// 200 empty body; non-POST → 405; POST → HandleMcpBody. Auth is enforced
// by the router before this runs (app.ts §2.4 #3).
func HandleMCP(s *store.Store, req *RawRequest) []byte {
	if req.Method == "DELETE" {
		return BuildResponse(200, []Header{{Key: "Content-Type", Value: "application/json"}}, []byte{})
	}
	if req.Method != "POST" {
		return BuildErrorResponse(405, "Method Not Allowed")
	}
	status, body := HandleMcpBody(s, string(req.Body))
	if body == nil {
		return BuildResponse(status, []Header{{Key: "Content-Type", Value: "application/json"}}, []byte{})
	}
	return BuildResponse(status, []Header{{Key: "Content-Type", Value: "application/json"}}, body)
}
