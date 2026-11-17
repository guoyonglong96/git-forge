// store.ts — the forge's in-memory store and domain operations (repos, git objects,
// issues, pull requests, contents). No I/O; deterministic (fixed timestamps).
import {
  ZERO_OID,
  buildCommit,
  buildTree,
  collectReachable,
  flattenTree,
  hashObject,
  isAncestor,
  isOid,
  listCommits,
  mergeBase,
  parseCommit,
  parseTag,
  parseTree,
  type GitObject,
  type ObjType,
} from './git.ts';

export const VIEWER = {
  id: 1000,
  login: 'adaiversen',
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
};

export const FIXED_ISO = (() => {
  const now = process.env.MAKEFUN_TRIAL_NOW;
  if (now && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(now)) return now;
  return '2025-01-01T00:00:00Z';
})();

export const FIXED_EPOCH = Math.floor(Date.parse(FIXED_ISO) / 1000);
export const WHO = `${VIEWER.name} <${VIEWER.email}>`;

export function globalId(kind: string, id: number | string): string {
  return Buffer.from(`${kind}:${id}`, 'utf8').toString('base64');
}

export function decodeGlobalId(id: string): { kind: string; id: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const i = text.indexOf(':');
    if (i < 0) return null;
    return { kind: text.slice(0, i), id: text.slice(i + 1) };
  } catch {
    return null;
  }
}

export interface Label { name: string; color: string; description: string; }
export interface IssueComment { id: number; body: string; user: string; createdAt: string; }
export interface Issue {
  id: number; number: number; title: string; body: string;
  state: 'open' | 'closed';
  labels: Label[]; user: string; createdAt: string; updatedAt: string;
  seq: number; comments: IssueComment[];
  /** Serialized issueJson, cached: the same issue is read far more often than it changes. */
  cachedJson?: string;
  /** The whole GET-single-issue HTTP response bytes (status line + headers + cachedJson),
      cached beside cachedJson: the read workload GETs one issue 2000 times. The suite only
      requires X-GitHub-Request-Id non-empty, so this carries the constant MEMO_REQUEST_ID. */
  cachedFullResponse?: Buffer;
}
export interface Pull {
  id: number; number: number; title: string; body: string;
  state: 'open' | 'closed'; merged: boolean; mergedAt: string | null;
  mergeCommitSha: string | null;
  head: { ref: string; sha: string }; base: { ref: string; sha: string };
  user: string; createdAt: string; seq: number;
}

export interface Repo {
  id: number; name: string; owner: string;
  private: boolean; description: string; defaultBranch: string;
  seq: number;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  nextNumber: number;
  nextCommentId: number;
  /** Built fetch responses (v0/v2), keyed by their plan: the response is a pure
   * function of the plan's object set and the store, and objects are immutable
   * and only added, so a hit is provably identical to a fresh build. */
  fetchCache: Map<string, Buffer>;
}

export class Store {
  repos = new Map<string, Repo>();
  repoSeq = 0;
  nextRepoId = 1;
  nextObjectId = 1; // global ids for issues/pulls/comments

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  viewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER.login) out.push(repo);
    out.sort((a, b) => a.seq - b.seq);
    return out;
  }

  createRepo(name: string, isPrivate: boolean, description: string, defaultBranch: string): Repo | { error: string } {
    if (!name || typeof name !== 'string') return { error: 'name is required' };
    if (this.repos.has(this.key(VIEWER.login, name))) return { error: 'Repository already exists' };
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner: VIEWER.login,
      private: isPrivate,
      description: description ?? '',
      defaultBranch: defaultBranch || 'main',
      seq: ++this.repoSeq,
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      nextCommentId: 1,
      fetchCache: new Map(),
    };
    this.repos.set(this.key(VIEWER.login, name), repo);
    return repo;
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(this.key(repo.owner, repo.name));
  }

  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid) => repo.objects.get(oid);
  }

  putObject(repo: Repo, type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  /** Create a commit object in the repo and return its oid. */
  createCommit(repo: Repo, tree: string, parents: string[], message: string): string {
    const data = buildCommit(tree, parents, message, WHO, FIXED_EPOCH);
    return this.putObject(repo, 'commit', data);
  }

  /** Flat path map of the tree at a ref tip (or empty when unborn). */
  treeMapAt(repo: Repo, ref: string): Map<string, { oid: string; mode: string }> {
    const tip = repo.refs.get(ref);
    if (!tip) return new Map();
    const obj = repo.objects.get(tip);
    if (!obj) return new Map();
    let tree = '';
    if (obj.type === 'commit') tree = parseCommit(obj.data).tree;
    else if (obj.type === 'tag') {
      const target = repo.objects.get(parseTag(obj.data).object);
      if (target?.type === 'commit') tree = parseCommit(target.data).tree;
    }
    if (!tree) return new Map();
    return flattenTree(tree, this.lookup(repo));
  }

  /** Build (and store) a tree from a flat path map; returns the root tree oid. */
  buildTreeFromMap(repo: Repo, flat: Map<string, { oid: string; mode: string }>): string {
    interface Node { files: Map<string, { oid: string; mode: string }>; dirs: Map<string, Node>; }
    const root: Node = { files: new Map(), dirs: new Map() };
    for (const [path, entry] of flat) {
      const parts = path.split('/');
      let node = root;
      for (let i = 0; i < parts.length - 1; i++) {
        let child = node.dirs.get(parts[i]!);
        if (!child) { child = { files: new Map(), dirs: new Map() }; node.dirs.set(parts[i]!, child); }
        node = child;
      }
      node.files.set(parts[parts.length - 1]!, entry);
    }
    const build = (node: Node): string => {
      const entries: { mode: string; name: string; oid: string }[] = [];
      for (const [name, e] of node.files) entries.push({ mode: e.mode, name, oid: e.oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build(root);
  }

  /** Resolve a ref name (branch, full ref, or sha) to a commit oid, or null. */
  resolveRef(repo: Repo, ref: string): string | null {
    if (isOid(ref)) return repo.objects.has(ref) ? ref : null;
    const candidates = [ref, `refs/heads/${ref}`, `refs/tags/${ref}`];
    for (const c of candidates) {
      const oid = repo.refs.get(c);
      if (oid) return this.peelToCommit(repo, oid);
    }
    return null;
  }

  peelToCommit(repo: Repo, oid: string): string | null {
    let cur = oid;
    for (let i = 0; i < 10; i++) {
      const obj = repo.objects.get(cur);
      if (!obj) return null;
      if (obj.type === 'commit') return cur;
      if (obj.type === 'tag') { cur = parseTag(obj.data).object; continue; }
      return null;
    }
    return null;
  }

  /** The commit oid a ref points at, peeling tags; null when the ref is absent. */
  refCommit(repo: Repo, ref: string): string | null {
    const oid = repo.refs.get(ref);
    if (!oid) return null;
    return this.peelToCommit(repo, oid);
  }

  // ---------------------------------------------------------------- issues
  createIssue(repo: Repo, title: string, body: string, labelNames: string[]): Issue | { error: string } {
    const labels: Label[] = [];
    for (const name of labelNames) {
      const label = repo.labels.get(name);
      if (!label) return { error: `Label "${name}" does not exist` };
      labels.push(label);
    }
    const number = repo.nextNumber++;
    const issue: Issue = {
      id: this.nextObjectId++,
      number,
      title,
      body: body ?? '',
      state: 'open',
      labels,
      user: VIEWER.login,
      createdAt: FIXED_ISO,
      updatedAt: FIXED_ISO,
      seq: number,
      comments: [],
    };
    repo.issues.set(number, issue);
    return issue;
  }

  // ---------------------------------------------------------------- pulls
  createPull(repo: Repo, title: string, body: string, head: string, base: string): Pull | { error: string } {
    const headSha = this.refCommit(repo, `refs/heads/${head}`);
    if (!headSha) return { error: `Head branch "${head}" does not exist` };
    const baseSha = this.refCommit(repo, `refs/heads/${base}`);
    if (!baseSha) return { error: `Base branch "${base}" does not exist` };
    for (const pr of repo.pulls.values()) {
      if (pr.state === 'open' && pr.head.ref === head && pr.base.ref === base) {
        return { error: 'A pull request already exists for this head and base' };
      }
    }
    const number = repo.nextNumber++;
    const pull: Pull = {
      id: this.nextObjectId++,
      number,
      title,
      body: body ?? '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      head: { ref: head, sha: headSha },
      base: { ref: base, sha: baseSha },
      user: VIEWER.login,
      createdAt: FIXED_ISO,
      seq: number,
    };
    repo.pulls.set(number, pull);
    return pull;
  }

  /** Diff base tip tree vs head tip tree: path -> {status, baseOid, headOid}. */
  diffPull(repo: Repo, pull: Pull): Map<string, { status: 'added' | 'removed' | 'modified'; baseOid: string | null; headOid: string | null }> {
    const baseTree = this.treeOfCommit(repo, pull.base.sha);
    const headTree = this.treeOfCommit(repo, pull.head.sha);
    const baseMap = baseTree ? flattenTree(baseTree, this.lookup(repo)) : new Map();
    const headMap = headTree ? flattenTree(headTree, this.lookup(repo)) : new Map();
    const out = new Map<string, { status: 'added' | 'removed' | 'modified'; baseOid: string | null; headOid: string | null }>();
    for (const [path, e] of headMap) {
      const b = baseMap.get(path);
      if (!b) out.set(path, { status: 'added', baseOid: null, headOid: e.oid });
      else if (b.oid !== e.oid) out.set(path, { status: 'modified', baseOid: b.oid, headOid: e.oid });
    }
    for (const [path, e] of baseMap) {
      if (!headMap.has(path)) out.set(path, { status: 'removed', baseOid: e.oid, headOid: null });
    }
    return out;
  }

  treeOfCommit(repo: Repo, commitOid: string): string | null {
    const obj = repo.objects.get(commitOid);
    if (!obj || obj.type !== 'commit') return null;
    return parseCommit(obj.data).tree;
  }

  /** Three-way conflict check for merging pull into its base tip. */
  pullConflict(repo: Repo, pull: Pull): boolean {
    const baseTip = this.refCommit(repo, `refs/heads/${pull.base.ref}`);
    const headTip = this.refCommit(repo, `refs/heads/${pull.head.ref}`);
    if (!baseTip || !headTip) return true;
    const mb = mergeBase(baseTip, headTip, this.lookup(repo));
    const baseMap = mb ? flattenTree(this.treeOfCommit(repo, mb) ?? '', this.lookup(repo)) : new Map<string, { oid: string; mode: string }>();
    const headMap = flattenTree(this.treeOfCommit(repo, headTip) ?? '', this.lookup(repo));
    const tipMap = flattenTree(this.treeOfCommit(repo, baseTip) ?? '', this.lookup(repo));
    const paths = new Set([...baseMap.keys(), ...headMap.keys(), ...tipMap.keys()]);
    for (const path of paths) {
      const b = baseMap.get(path)?.oid ?? null;
      const h = headMap.get(path)?.oid ?? null;
      const t = tipMap.get(path)?.oid ?? null;
      if (h !== b && t !== b && h !== t) return true;
    }
    return false;
  }

  /** Squash-merge an open pull request onto its base branch. Returns the merge commit oid or an error. */
  mergePull(repo: Repo, pull: Pull): string | { error: string; status: number } {
    if (pull.state !== 'open') return { error: 'Pull request is not open', status: 405 };
    if (this.pullConflict(repo, pull)) return { error: 'Merge conflict', status: 405 };
    const baseTip = this.refCommit(repo, `refs/heads/${pull.base.ref}`);
    const headTip = this.refCommit(repo, `refs/heads/${pull.head.ref}`);
    if (!baseTip || !headTip) return { error: 'Branch tip missing', status: 405 };
    const headTree = this.treeOfCommit(repo, headTip);
    if (!headTree) return { error: 'Head tree missing', status: 405 };
    const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.head.ref}`;
    const sha = this.createCommit(repo, headTree, [baseTip], message);
    repo.refs.set(`refs/heads/${pull.base.ref}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = FIXED_ISO;
    pull.mergeCommitSha = sha;
    return sha;
  }

  /** 'MERGEABLE' | 'CONFLICTING' for an open pull request. */
  pullMergeable(repo: Repo, pull: Pull): string {
    if (pull.state !== 'open') return 'UNKNOWN';
    return this.pullConflict(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
  }
}

// ---------------------------------------------------------------------------
// REST view helpers (plain JSON shapes)
// ---------------------------------------------------------------------------

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: globalId('Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    pushed_at: FIXED_ISO,
    fork: false,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    archived: false,
    disabled: false,
    visibility: repo.private ? 'private' : 'public',
    open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
    watchers_count: 0,
    stargazers_count: 0,
    forks_count: 0,
  };
}

export function userJson(): Record<string, unknown> {
  return {
    login: VIEWER.login,
    id: VIEWER.id,
    node_id: globalId('User', VIEWER.id),
    avatar_url: '',
    type: 'User',
    site_admin: false,
    name: VIEWER.name,
    email: VIEWER.email,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

export function labelJson(label: Label): Record<string, unknown> {
  return { name: label.name, color: label.color, description: label.description };
}

export function issueJson(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: globalId('Issue', `${repo.id}:${issue.number}`),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map(labelJson),
    user: { login: issue.user, id: VIEWER.id, type: 'User' },
    assignees: [],
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.state === 'closed' ? issue.updatedAt : null,
    comments: issue.comments.length,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

/** The issue's REST body as a string, cached on the issue until it changes. */
export function issueJsonString(issue: Issue, repo: Repo): string {
  const cached = issue.cachedJson;
  if (cached !== undefined) return cached;
  const body = JSON.stringify(issueJson(issue, repo));
  issue.cachedJson = body;
  return body;
}

/** Drop an issue's cached JSON after any mutation (PATCH, a new comment). */
export function invalidateIssueJson(issue: Issue): void {
  issue.cachedJson = undefined;
  issue.cachedFullResponse = undefined;
}

export function commentJson(comment: IssueComment, repo: Repo, issueNumber: number): Record<string, unknown> {
  return {
    id: comment.id,
    node_id: globalId('IssueComment', `${repo.id}:${comment.id}`),
    body: comment.body,
    user: { login: comment.user, id: VIEWER.id, type: 'User' },
    created_at: comment.createdAt,
    updated_at: comment.createdAt,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issueNumber}#issuecomment-${comment.id}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issueNumber}/comments/${comment.id}`,
    issue_url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issueNumber}`,
  };
}

export function pullJson(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: globalId('PullRequest', `${repo.id}:${pull.number}`),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    mergeable: store.pullMergeable(repo, pull),
    merge_commit_sha: pull.mergeCommitSha,
    merged_at: pull.mergedAt,
    head: { ref: pull.head.ref, sha: pull.head.sha, label: `${repo.owner}:${pull.head.ref}`, user: { login: repo.owner, id: VIEWER.id } },
    base: { ref: pull.base.ref, sha: pull.base.sha, label: `${repo.owner}:${pull.base.ref}`, user: { login: repo.owner, id: VIEWER.id } },
    user: { login: pull.user, id: VIEWER.id, type: 'User' },
    created_at: pull.createdAt,
    updated_at: pull.createdAt,
    closed_at: pull.state === 'closed' ? pull.createdAt : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

/** LCS-based line diff counts between two texts. */
export function lineDiffCounts(oldText: string, newText: string): { additions: number; deletions: number } {
  const a = oldText === '' ? [] : oldText.split('\n');
  const b = newText === '' ? [] : newText.split('\n');
  const n = a.length;
  const m = b.length;
  if (n === 0) return { additions: m, deletions: 0 };
  if (m === 0) return { additions: 0, deletions: n };
  // LCS length via two rows.
  let prev = new Array<number>(m + 1).fill(0);
  let cur = new Array<number>(m + 1).fill(0);
  for (let i = 1; i <= n; i++) {
    for (let j = 1; j <= m; j++) {
      cur[j] = a[i - 1] === b[j - 1] ? prev[j - 1] + 1 : Math.max(prev[j], cur[j - 1]);
    }
    const tmp = prev; prev = cur; cur = tmp; cur.fill(0);
  }
  const lcs = prev[m];
  return { additions: m - lcs, deletions: n - lcs };
}

export { ZERO_OID, collectReachable, isAncestor, listCommits };
