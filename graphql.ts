// graphql.ts — a small GraphQL parser and executor for the fixed set of documents
// the suite sends. Supports: variables, aliases, inline fragments, inline input
// objects and enum lists, operation names, fragments are not used by the suite.
import {
  FIXED_ISO,
  VIEWER,
  decodeGlobalId,
  globalId,
  invalidateIssueJson,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

export interface Selection {
  name: string;
  alias: string | null;
  args: Record<string, unknown>;
  children: Selection[] | null;
  fragmentType: string | null; // for inline fragments: "... on Type"
}

interface Token { kind: 'name' | 'punct' | 'string' | 'number' | 'var' | 'eof'; value: string; }

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i]!;
    if (/\s/.test(ch) || ch === ',') { i++; continue; }
    if (ch === '#') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (ch === '$') {
      let j = i + 1;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j]!)) j++;
      tokens.push({ kind: 'var', value: src.slice(i + 1, j) });
      i = j;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1]!;
          if (esc === 'u' && /^[0-9a-fA-F]{4}$/.test(src.slice(j + 2, j + 6))) {
            out += String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16));
            j += 6;
            continue;
          }
          const simple: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
          out += simple[esc] ?? esc;
          j += 2;
          continue;
        }
        out += src[j];
        j++;
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j]!)) j++;
      tokens.push({ kind: 'number', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j]!)) j++;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if ('{}()[]:!@=|&.'.includes(ch)) {
      // '...' is three dots.
      if (ch === '.' && src.slice(i, i + 3) === '...') {
        tokens.push({ kind: 'punct', value: '...' });
        i += 3;
        continue;
      }
      tokens.push({ kind: 'punct', value: ch });
      i++;
      continue;
    }
    i++;
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

class Parser {
  tokens: Token[];
  pos = 0;
  variables: Record<string, unknown>;
  constructor(src: string, variables: Record<string, unknown>) {
    this.tokens = tokenize(src);
    this.variables = variables;
  }
  peek(): Token { return this.tokens[this.pos]!; }
  next(): Token { return this.tokens[this.pos++]!; }
  expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`expected ${v}, got ${t.value}`);
  }
  expectName(): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error(`expected name, got ${t.value}`);
    return t.value;
  }

  parseValue(): unknown {
    const t = this.next();
    if (t.kind === 'string') return t.value;
    if (t.kind === 'number') return Number(t.value);
    if (t.kind === 'var') return this.variables[t.value] ?? null;
    if (t.kind === 'name') {
      if (t.value === 'true') return true;
      if (t.value === 'false') return false;
      if (t.value === 'null') return null;
      return { __enum: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const arr: unknown[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) arr.push(this.parseValue());
      this.expectPunct(']');
      return arr;
    }
    if (t.kind === 'punct' && t.value === '{') {
      const obj: Record<string, unknown> = {};
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const key = this.expectName();
        this.expectPunct(':');
        obj[key] = this.parseValue();
      }
      this.expectPunct('}');
      return obj;
    }
    throw new Error(`unexpected token ${t.value}`);
  }

  parseArgs(): Record<string, unknown> {
    const args: Record<string, unknown> = {};
    this.expectPunct('(');
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      const key = this.expectName();
      this.expectPunct(':');
      args[key] = this.parseValue();
    }
    this.expectPunct(')');
    return args;
  }

  parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const sels: Selection[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      sels.push(this.parseSelection());
    }
    this.expectPunct('}');
    return sels;
  }

  parseSelection(): Selection {
    let t = this.peek();
    if (t.kind === 'punct' && t.value === '...') {
      this.next();
      let fragmentType: string | null = null;
      const nt = this.peek();
      if (nt.kind === 'name' && nt.value === 'on') {
        this.next();
        fragmentType = this.expectName();
      } else if (nt.kind === 'name') {
        // Named fragment spread — the suite does not use these; skip its selection set.
        this.next();
      }
      const children = this.parseSelectionSet();
      return { name: '__fragment', alias: null, args: {}, children, fragmentType };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    t = this.peek();
    if (t.kind === 'punct' && t.value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    let args: Record<string, unknown> = {};
    t = this.peek();
    if (t.kind === 'punct' && t.value === '(') args = this.parseArgs();
    let children: Selection[] | null = null;
    t = this.peek();
    if (t.kind === 'punct' && t.value === '{') children = this.parseSelectionSet();
    return { name, alias, args, children, fragmentType: null };
  }

  parseOperation(): { operation: 'query' | 'mutation'; selections: Selection[] } {
    let operation: 'query' | 'mutation' = 'query';
    let t = this.peek();
    if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
      operation = t.value as 'query' | 'mutation';
      this.next();
      t = this.peek();
      if (t.kind === 'name') this.next(); // operation name
      // variable definitions
      t = this.peek();
      if (t.kind === 'punct' && t.value === '(') {
        let depth = 0;
        while (true) {
          const tok = this.next();
          if (tok.kind === 'punct' && tok.value === '(') depth++;
          if (tok.kind === 'punct' && tok.value === ')') { depth--; if (depth === 0) break; }
          if (tok.kind === 'eof') break;
        }
      }
    }
    const selections = this.parseSelectionSet();
    return { operation, selections };
  }
}

// ---------------------------------------------------------------------------
// Introspection tables
// ---------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'author', 'createdAt', 'updatedAt', 'repository'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'files', 'author', 'createdAt', 'updatedAt', 'repository'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'createdAt', 'updatedAt'],
  SearchType: [],
  IssueState: [],
  PullRequestState: [],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
  MergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  MergeStateStatus: ['BEHIND', 'BLOCKED', 'CLEAN', 'DIRTY', 'DRAFT', 'HAS_HOOKS', 'MERGEABLE', 'UNKNOWN', 'UNSTABLE'],
};

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type Ctx = { store: Store };

function enumValue(v: unknown): string | null {
  if (typeof v === 'object' && v !== null && '__enum' in (v as Record<string, unknown>)) {
    return (v as { __enum: string }).__enum;
  }
  return typeof v === 'string' ? v : null;
}

function decodeCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string') return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    return m ? parseInt(m[1]!, 10) : null;
  } catch {
    return null;
  }
}

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function scalar(value: unknown, sel: Selection, ctx: Ctx): unknown {
  void sel; void ctx;
  return value ?? null;
}

function repositoryNode(store: Store, repo: Repo, sels: Selection[], ctx: Ctx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of sels) {
    if (sel.name === '__fragment') {
      if (sel.fragmentType && sel.fragmentType !== 'Repository') continue;
      Object.assign(out, repositoryNode(store, repo, sel.children ?? [], ctx));
      continue;
    }
    const key = sel.alias ?? sel.name;
    switch (sel.name) {
      case 'id': out[key] = globalId('Repository', repo.id); break;
      case 'databaseId': out[key] = repo.id; break;
      case 'name': out[key] = repo.name; break;
      case 'nameWithOwner': out[key] = `${repo.owner}/${repo.name}`; break;
      case 'owner': out[key] = { login: repo.owner }; break;
      case 'defaultBranchRef': {
        const ref = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        out[key] = ref ? { name: repo.defaultBranch } : null;
        break;
      }
      case 'viewerPermission': out[key] = 'ADMIN'; break;
      case 'hasIssuesEnabled': out[key] = true; break;
      case 'issue': {
        const n = sel.args.number as number | undefined;
        const issue = typeof n === 'number' ? repo.issues.get(n) : undefined;
        out[key] = issue ? issueNode(store, repo, issue, sel.children ?? [], ctx) : null;
        break;
      }
      case 'issues': {
        const nodes = issueConnection(repo, sel, ctx);
        out[key] = nodes;
        break;
      }
      case 'pullRequests': {
        out[key] = pullConnection(repo, sel, ctx);
        break;
      }
      case 'pullRequest': {
        const n = sel.args.number as number | undefined;
        const pull = typeof n === 'number' ? repo.pulls.get(n) : undefined;
        out[key] = pull ? pullNode(store, repo, pull, sel.children ?? [], ctx) : null;
        break;
      }
      case 'createdAt': case 'updatedAt': out[key] = FIXED_ISO; break;
      default:
        throw new Error(`unknown field Repository.${sel.name}`);
    }
  }
  return out;
}

function issueConnection(repo: Repo, sel: Selection, ctx: Ctx): Record<string, unknown> {
  const first = typeof sel.args.first === 'number' ? sel.args.first : 100;
  const after = decodeCursor(sel.args.after);
  const statesArg = sel.args.states;
  const states = new Set<string>();
  if (Array.isArray(statesArg)) {
    for (const s of statesArg) {
      const e = enumValue(s);
      if (e) states.add(e);
    }
  }
  const orderBy = sel.args.orderBy as Record<string, unknown> | undefined;
  const direction = enumValue(orderBy?.direction) ?? 'DESC';
  let issues = [...repo.issues.values()];
  if (states.size) {
    issues = issues.filter((i) => states.has(i.state.toUpperCase()));
  }
  issues.sort((a, b) => (direction === 'ASC' ? a.number - b.number : b.number - a.number));
  if (after !== null) {
    issues = issues.filter((i) => (direction === 'ASC' ? i.number > after : i.number < after));
  }
  const page = issues.slice(0, first);
  const hasNext = issues.length > first;
  const nodes = page.map((i) => issueNode(ctx.store, repo, i, nodeChildren(sel, 'nodes'), ctx));
  const out: Record<string, unknown> = {};
  for (const child of sel.children ?? []) {
    const key = child.alias ?? child.name;
    if (child.name === 'nodes') out[key] = nodes;
    else if (child.name === 'edges') {
      const nodeSels = child.children?.find((c) => c.name === 'node')?.children ?? [];
      out[key] = page.map((i) => ({ node: issueNode(ctx.store, repo, i, nodeSels, ctx), cursor: encodeCursor(i.number) }));
    } else if (child.name === 'pageInfo') {
      out[key] = {
        hasNextPage: hasNext,
        hasPreviousPage: after !== null,
        startCursor: page.length ? encodeCursor(page[0]!.number) : null,
        endCursor: page.length ? encodeCursor(page[page.length - 1]!.number) : null,
      };
    } else if (child.name === 'totalCount') out[key] = issues.length;
  }
  return out;
}

function nodeChildren(sel: Selection, field: string): Selection[] {
  const child = (sel.children ?? []).find((c) => c.name === field);
  return child?.children ?? [];
}

function pullConnection(repo: Repo, sel: Selection, ctx: Ctx): Record<string, unknown> {
  const first = typeof sel.args.first === 'number' ? sel.args.first : 100;
  const after = decodeCursor(sel.args.after);
  const statesArg = sel.args.states;
  const states = new Set<string>();
  if (Array.isArray(statesArg)) {
    for (const s of statesArg) { const e = enumValue(s); if (e) states.add(e); }
  }
  let pulls = [...repo.pulls.values()];
  if (states.size) {
    pulls = pulls.filter((p) => {
      const st = p.merged ? 'MERGED' : p.state.toUpperCase();
      return states.has(st) || states.has(p.state.toUpperCase());
    });
  } else {
    pulls = pulls.filter((p) => p.state === 'open');
  }
  pulls.sort((a, b) => b.number - a.number);
  if (after !== null) pulls = pulls.filter((p) => p.number < after);
  const page = pulls.slice(0, first);
  const hasNext = pulls.length > first;
  const nodes = page.map((p) => pullNode(ctx.store, repo, p, nodeChildren(sel, 'nodes'), ctx));
  const out: Record<string, unknown> = {};
  for (const child of sel.children ?? []) {
    const key = child.alias ?? child.name;
    if (child.name === 'nodes') out[key] = nodes;
    else if (child.name === 'edges') {
      out[key] = page.map((p) => ({ node: pullNode(ctx.store, repo, p, child.children?.find((c) => c.name === 'node')?.children ?? [], ctx), cursor: encodeCursor(p.number) }));
    } else if (child.name === 'pageInfo') {
      out[key] = {
        hasNextPage: hasNext,
        hasPreviousPage: after !== null,
        startCursor: page.length ? encodeCursor(page[0]!.number) : null,
        endCursor: page.length ? encodeCursor(page[page.length - 1]!.number) : null,
      };
    } else if (child.name === 'totalCount') out[key] = pulls.length;
  }
  return out;
}

function issueNode(store: Store, repo: Repo, issue: Issue, sels: Selection[], ctx: Ctx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of sels) {
    if (sel.name === '__fragment') {
      if (sel.fragmentType && sel.fragmentType !== 'Issue') continue;
      Object.assign(out, issueNode(store, repo, issue, sel.children ?? [], ctx));
      continue;
    }
    const key = sel.alias ?? sel.name;
    switch (sel.name) {
      case 'id': out[key] = globalId('Issue', `${repo.id}:${issue.number}`); break;
      case 'number': out[key] = issue.number; break;
      case 'title': out[key] = issue.title; break;
      case 'body': out[key] = issue.body; break;
      case 'state': out[key] = issue.state.toUpperCase(); break;
      case 'author': out[key] = { login: issue.user }; break;
      case 'createdAt': out[key] = issue.createdAt; break;
      case 'updatedAt': out[key] = issue.updatedAt; break;
      case 'repository': out[key] = repositoryNode(store, repo, sel.children ?? [], ctx); break;
      default: throw new Error(`unknown field Issue.${sel.name}`);
    }
  }
  return out;
}

function pullNode(store: Store, repo: Repo, pull: Pull, sels: Selection[], ctx: Ctx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of sels) {
    if (sel.name === '__fragment') {
      if (sel.fragmentType && sel.fragmentType !== 'PullRequest') continue;
      Object.assign(out, pullNode(store, repo, pull, sel.children ?? [], ctx));
      continue;
    }
    const key = sel.alias ?? sel.name;
    switch (sel.name) {
      case 'id': out[key] = globalId('PullRequest', `${repo.id}:${pull.number}`); break;
      case 'number': out[key] = pull.number; break;
      case 'title': out[key] = pull.title; break;
      case 'state': out[key] = pull.merged ? 'MERGED' : pull.state.toUpperCase(); break;
      case 'merged': out[key] = pull.merged; break;
      case 'mergeable': out[key] = store.pullMergeable(repo, pull); break;
      case 'headRefName': out[key] = pull.head.ref; break;
      case 'baseRefName': out[key] = pull.base.ref; break;
      case 'headRef': out[key] = { name: pull.head.ref }; break;
      case 'baseRef': out[key] = { name: pull.base.ref }; break;
      case 'author': out[key] = { login: pull.user }; break;
      case 'createdAt': out[key] = pull.createdAt; break;
      case 'updatedAt': out[key] = pull.createdAt; break;
      case 'repository': out[key] = repositoryNode(store, repo, sel.children ?? [], ctx); break;
      case 'files': {
        const diff = store.diffPull(repo, pull);
        const first = typeof sel.args.first === 'number' ? sel.args.first : 100;
        const paths = [...diff.keys()].sort().slice(0, first);
        const nodes = paths.map((p) => ({ path: p }));
        const out2: Record<string, unknown> = {};
        for (const child of sel.children ?? []) {
          const k = child.alias ?? child.name;
          if (child.name === 'nodes') out2[k] = nodes;
          else if (child.name === 'edges') out2[k] = paths.map((p) => ({ node: { path: p } }));
          else if (child.name === 'totalCount') out2[k] = diff.size;
        }
        out[key] = out2;
        break;
      }
      default: throw new Error(`unknown field PullRequest.${sel.name}`);
    }
  }
  return out;
}

function typeNode(typeName: string, sels: Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of sels) {
    const key = sel.alias ?? sel.name;
    switch (sel.name) {
      case 'name': out[key] = typeName; break;
      case 'fields': {
        const fields = TYPE_FIELDS[typeName] ?? [];
        const nodes = fields.map((f) => ({ name: f }));
        const out2: Record<string, unknown> = {};
        for (const child of sel.children ?? []) {
          const k = child.alias ?? child.name;
          if (child.name === 'name') out2[k] = typeName;
        }
        // fields is a list of {name}; the suite asks fields { name }.
        out[key] = nodes;
        void out2;
        break;
      }
      case 'enumValues': {
        const values = TYPE_ENUM_VALUES[typeName] ?? [];
        out[key] = values.map((v) => ({ name: v }));
        break;
      }
      default: out[key] = null;
    }
  }
  return out;
}

function executeQuery(store: Store, sels: Selection[], ctx: Ctx): { data: Record<string, unknown>; errors: { message: string }[] } {
  const data: Record<string, unknown> = {};
  const errors: { message: string }[] = [];
  for (const sel of sels) {
    const key = sel.alias ?? sel.name;
    try {
      switch (sel.name) {
        case 'viewer': {
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'login') out[k] = VIEWER.login;
            else if (child.name === 'id') out[k] = globalId('User', VIEWER.id);
            else if (child.name === 'name') out[k] = VIEWER.name;
            else throw new Error(`Cannot query field "${child.name}" on type "User".`);
          }
          data[key] = out;
          break;
        }
        case 'repository': {
          const owner = sel.args.owner as string | undefined;
          const name = sel.args.name as string | undefined;
          const repo = owner && name ? store.getRepo(owner, name) : undefined;
          data[key] = repo ? repositoryNode(store, repo, sel.children ?? [], ctx) : null;
          break;
        }
        case 'node': {
          const id = sel.args.id as string | undefined;
          const decoded = id ? decodeGlobalId(id) : null;
          let node: Record<string, unknown> | null = null;
          if (decoded && decoded.kind === 'Repository') {
            const repoId = parseInt(decoded.id, 10);
            const repo = [...store.repos.values()].find((r) => r.id === repoId);
            if (repo) node = repositoryNode(store, repo, sel.children ?? [], ctx);
          }
          data[key] = node;
          break;
        }
        case '__type': {
          const name = sel.args.name as string | undefined;
          data[key] = name ? typeNode(name, sel.children ?? []) : null;
          break;
        }
        case '__schema': {
          data[key] = { queryType: { name: 'Query' }, mutationType: { name: 'Mutation' } };
          break;
        }
        default:
          throw new Error(`Cannot query field "${sel.name}" on type "Query".`);
      }
    } catch (e) {
      errors.push({ message: e instanceof Error ? e.message : String(e) });
      data[key] = null;
    }
  }
  return { data, errors };
}

function executeMutation(store: Store, sels: Selection[], ctx: Ctx): { data: Record<string, unknown>; errors: { message: string }[] } {
  const data: Record<string, unknown> = {};
  const errors: { message: string }[] = [];
  for (const sel of sels) {
    const key = sel.alias ?? sel.name;
    try {
      const input = (sel.args.input ?? {}) as Record<string, unknown>;
      switch (sel.name) {
        case 'createIssue': {
          const repoId = decodeGlobalId(String(input.repositoryId ?? ''));
          const repo = repoId ? [...store.repos.values()].find((r) => r.id === parseInt(repoId.id, 10)) : undefined;
          if (!repo) throw new Error('Repository not found');
          const result = store.createIssue(repo, String(input.title ?? ''), String(input.body ?? ''), []);
          if ('error' in result) throw new Error(result.error);
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'issue') out[k] = issueNode(store, repo, result, child.children ?? [], ctx);
            else if (child.name === 'clientMutationId') out[k] = input.clientMutationId ?? null;
          }
          data[key] = out;
          break;
        }
        case 'closeIssue': {
          const decoded = decodeGlobalId(String(input.issueId ?? ''));
          if (!decoded) throw new Error('Issue not found');
          const [repoId, num] = decoded.id.split(':');
          const repo = [...store.repos.values()].find((r) => r.id === parseInt(repoId!, 10));
          const issue = repo?.issues.get(parseInt(num!, 10));
          if (!issue) throw new Error('Issue not found');
          issue.state = 'closed';
          issue.updatedAt = FIXED_ISO;
          invalidateIssueJson(issue);
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'issue') out[k] = issueNode(store, repo!, issue, child.children ?? [], ctx);
            else if (child.name === 'clientMutationId') out[k] = input.clientMutationId ?? null;
          }
          data[key] = out;
          break;
        }
        case 'createPullRequest': {
          const repoId = decodeGlobalId(String(input.repositoryId ?? ''));
          const repo = repoId ? [...store.repos.values()].find((r) => r.id === parseInt(repoId.id, 10)) : undefined;
          if (!repo) throw new Error('Repository not found');
          const result = store.createPull(
            repo,
            String(input.title ?? ''),
            String(input.body ?? ''),
            String(input.headRefName ?? ''),
            String(input.baseRefName ?? ''),
          );
          if ('error' in result) throw new Error(result.error);
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'pullRequest') out[k] = pullNode(store, repo, result, child.children ?? [], ctx);
            else if (child.name === 'clientMutationId') out[k] = input.clientMutationId ?? null;
          }
          data[key] = out;
          break;
        }
        case 'mergePullRequest': {
          const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
          if (!decoded) throw new Error('Pull request not found');
          const [repoId, num] = decoded.id.split(':');
          const repo = [...store.repos.values()].find((r) => r.id === parseInt(repoId!, 10));
          const pull = repo?.pulls.get(parseInt(num!, 10));
          if (!pull || !repo) throw new Error('Pull request not found');
          const result = store.mergePull(repo, pull);
          if (typeof result === 'object') throw new Error(result.error);
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'pullRequest') out[k] = pullNode(store, repo, pull, child.children ?? [], ctx);
            else if (child.name === 'clientMutationId') out[k] = input.clientMutationId ?? null;
          }
          data[key] = out;
          break;
        }
        case 'addComment': {
          const decoded = decodeGlobalId(String(input.subjectId ?? ''));
          if (!decoded) throw new Error('Subject not found');
          const [repoId, num] = decoded.id.split(':');
          const repo = [...store.repos.values()].find((r) => r.id === parseInt(repoId!, 10));
          const issue = repo?.issues.get(parseInt(num!, 10));
          const pull = repo?.pulls.get(parseInt(num!, 10));
          const target = issue ?? pull;
          if (!target || !repo) throw new Error('Subject not found');
          const comment = {
            id: repo.nextCommentId++,
            body: String(input.body ?? ''),
            user: VIEWER.login,
            createdAt: FIXED_ISO,
          };
          if (issue) issue.comments.push(comment);
          if (issue) invalidateIssueJson(issue);
          const commentId = globalId('IssueComment', `${repo.id}:${comment.id}`);
          const out: Record<string, unknown> = {};
          for (const child of sel.children ?? []) {
            const k = child.alias ?? child.name;
            if (child.name === 'commentEdge') {
              const nodeSels = child.children?.find((c) => c.name === 'node')?.children ?? [];
              const node: Record<string, unknown> = {};
              for (const n of nodeSels) {
                const nk = n.alias ?? n.name;
                if (n.name === 'id') node[nk] = commentId;
                else if (n.name === 'body') node[nk] = comment.body;
              }
              out[k] = { node, cursor: encodeCursor(comment.id) };
            } else if (child.name === 'clientMutationId') out[k] = input.clientMutationId ?? null;
          }
          data[key] = out;
          break;
        }
        default:
          throw new Error(`Cannot query field "${sel.name}" on type "Mutation".`);
      }
    } catch (e) {
      errors.push({ message: e instanceof Error ? e.message : String(e) });
      data[key] = null;
    }
  }
  return { data, errors };
}

/** Execute one GraphQL document. Returns the result object (without HTTP concerns). */
export function executeGraphQL(store: Store, query: string, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: Ctx = { store };
  let parsed: { operation: 'query' | 'mutation'; selections: Selection[] };
  try {
    parsed = new Parser(query, variables).parseOperation();
  } catch (e) {
    return { data: null, errors: [{ message: e instanceof Error ? e.message : String(e) }] };
  }
  const result = parsed.operation === 'mutation'
    ? executeMutation(store, parsed.selections, ctx)
    : executeQuery(store, parsed.selections, ctx);
  if (result.errors.length === 0) return { data: result.data };
  return { data: result.data, errors: result.errors };
}
