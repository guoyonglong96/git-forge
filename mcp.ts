// mcp.ts — MCP endpoint (JSON-RPC 2.0 Streamable HTTP) with the six tools the suite uses.
import { issueJson, pullJson, repoJson, type Store } from './store.ts';

export const MCP_PROTOCOL_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

export interface McpTool {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, unknown>; required: string[] };
}

export const MCP_TOOLS: McpTool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The name of the repository.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the issue.' },
        body: { type: 'string', description: 'The body of the issue.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        state: { type: 'string', description: 'Filter issues by state: open, closed, or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        title: { type: 'string', description: 'The title of the pull request.' },
        head: { type: 'string', description: 'The name of the branch where the changes are implemented.' },
        base: { type: 'string', description: 'The name of the branch the changes are pulled into.' },
        body: { type: 'string', description: 'The body of the pull request.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The owner of the repository.' },
        repo: { type: 'string', description: 'The name of the repository.' },
        pull_number: { type: 'integer', description: 'The number of the pull request.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function toolResult(id: unknown, obj: unknown): Record<string, unknown> {
  const text = JSON.stringify(obj);
  return {
    jsonrpc: '2.0',
    id: id ?? null,
    result: {
      content: [{ type: 'text', text }],
      structuredContent: obj,
    },
  };
}

function toolError(id: unknown, message: string): Record<string, unknown> {
  return {
    jsonrpc: '2.0',
    id: id ?? null,
    result: {
      content: [{ type: 'text', text: JSON.stringify({ error: message }) }],
      structuredContent: { error: message },
      isError: true,
    },
  };
}

function rpcError(id: unknown, code: number, message: string): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

function rpcResult(id: unknown, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function missingArg(tool: McpTool, args: Record<string, unknown>): string | null {
  for (const req of tool.inputSchema.required) {
    if (args[req] === undefined || args[req] === null || args[req] === '') return req;
  }
  return null;
}

export function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = MCP_TOOLS.find((t) => t.name === name);
  if (!tool) return { error: `Unknown tool: ${name}`, code: -32602 };
  const missing = missingArg(tool, args);
  if (missing) return { error: `Missing required argument: ${missing}`, code: -32602 };

  switch (name) {
    case 'create_repo': {
      const result = store.createRepo(
        String(args.name),
        args.private === true,
        typeof args.description === 'string' ? args.description : '',
        'main',
      );
      if ('error' in result) return { error: result.error, code: -32602 };
      return { value: repoJson(result) };
    }
    case 'get_repo': {
      const repo = store.getRepo(String(args.owner), String(args.repo));
      if (!repo) return { error: 'Not Found', code: -32602 };
      return { value: repoJson(repo) };
    }
    case 'create_issue': {
      const repo = store.getRepo(String(args.owner), String(args.repo));
      if (!repo) return { error: 'Not Found', code: -32602 };
      const result = store.createIssue(
        repo,
        String(args.title),
        typeof args.body === 'string' ? args.body : '',
        [],
      );
      if ('error' in result) return { error: result.error, code: -32602 };
      return { value: issueJson(result, repo) };
    }
    case 'list_issues': {
      const repo = store.getRepo(String(args.owner), String(args.repo));
      if (!repo) return { error: 'Not Found', code: -32602 };
      const state = typeof args.state === 'string' ? args.state : 'open';
      let issues = [...repo.issues.values()];
      if (state === 'open') issues = issues.filter((i) => i.state === 'open');
      else if (state === 'closed') issues = issues.filter((i) => i.state === 'closed');
      issues.sort((a, b) => b.number - a.number);
      return { value: issues.map((i) => issueJson(i, repo)) };
    }
    case 'create_pull_request': {
      const repo = store.getRepo(String(args.owner), String(args.repo));
      if (!repo) return { error: 'Not Found', code: -32602 };
      const result = store.createPull(
        repo,
        String(args.title),
        typeof args.body === 'string' ? args.body : '',
        String(args.head),
        String(args.base),
      );
      if ('error' in result) return { error: result.error, code: -32602 };
      return { value: pullJson(store, repo, result) };
    }
    case 'merge_pull_request': {
      const repo = store.getRepo(String(args.owner), String(args.repo));
      if (!repo) return { error: 'Not Found', code: -32602 };
      const pull = repo.pulls.get(Number(args.pull_number));
      if (!pull) return { error: 'Not Found', code: -32602 };
      const result = store.mergePull(repo, pull);
      if (typeof result === 'object') return { error: result.error, code: -32602 };
      return { value: { merged: true, sha: result, ...pullJson(store, repo, pull) } };
    }
    default:
      return { error: `Unknown tool: ${name}`, code: -32602 };
  }
}

/**
 * Handle one JSON-RPC request object. Returns the response object, or null for
 * notifications (no response).
 */
export function handleMcpRequest(store: Store, req: Record<string, unknown>): Record<string, unknown> | null {
  const id = req.id ?? null;
  const method = typeof req.method === 'string' ? req.method : '';
  const params = (req.params ?? {}) as Record<string, unknown>;

  if (method === 'notifications/initialized' || method.startsWith('notifications/')) return null;

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: MCP_PROTOCOL_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: 'git-forge', version: '1.0.0' },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'git-forge', version: '1.0.0' } },
    });
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const version = MCP_PROTOCOL_VERSIONS.includes(requested) ? requested : MCP_PROTOCOL_VERSIONS[0]!;
    return rpcResult(id, {
      protocolVersion: version,
      capabilities: { tools: {} },
      serverInfo: { name: 'git-forge', version: '1.0.0' },
    });
  }

  if (method === 'ping') return rpcResult(id, {});

  if (method === 'tools/list') {
    return rpcResult(id, { tools: MCP_TOOLS });
  }

  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    if (!toolName) return rpcError(id, -32602, 'Missing tool name');
    const tool = MCP_TOOLS.find((t) => t.name === toolName);
    if (!tool) return rpcError(id, -32602, `Unknown tool: ${toolName}`);
    const outcome = callTool(store, toolName, args);
    if ('error' in outcome) return toolError(id, String(outcome.error));
    return toolResult(id, outcome.value);
  }

  return rpcError(id, -32601, `Method not found: ${method}`);
}

/** Parse an MCP request body; returns the response object(s) or an error envelope. */
export function handleMcpBody(store: Store, bodyText: string): { status: number; body: Record<string, unknown> | Record<string, unknown>[] | null } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } },
    };
  }
  if (Array.isArray(parsed)) {
    const responses: Record<string, unknown>[] = [];
    for (const item of parsed) {
      if (item && typeof item === 'object') {
        const r = handleMcpRequest(store, item as Record<string, unknown>);
        if (r) responses.push(r);
      }
    }
    return { status: 200, body: responses };
  }
  if (parsed && typeof parsed === 'object') {
    const r = handleMcpRequest(store, parsed as Record<string, unknown>);
    if (r === null) return { status: 202, body: null };
    return { status: 200, body: r };
  }
  return {
    status: 400,
    body: { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } },
  };
}
