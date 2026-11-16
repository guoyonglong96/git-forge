// rest.ts — REST v3 API handlers (/api/v3/*).
import { createHash } from 'node:crypto';
import {
  ZERO_OID,
  hashObject,
  isOid,
  listCommits,
  parseCommit,
  parseTree,
  type GitObject,
} from './git.ts';
import {
  FIXED_ISO,
  VIEWER,
  commentJson,
  invalidateIssueJson,
  issueJsonString,
  labelJson,
  lineDiffCounts,
  pullJson,
  repoJson,
  userJson,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

export interface HttpRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Map<string, string>;
  bodyText: string;
  bodyJson: unknown;
  bodyError: boolean;
}

export interface HttpResponse {
  status: number;
  headers: Record<string, string>;
  body: string | Buffer;
}

export function json(status: number, value: unknown, extraHeaders: Record<string, string> = {}): HttpResponse {
  return {
    status,
    headers: { 'Content-Type': 'application/json', ...extraHeaders },
    body: JSON.stringify(value),
  };
}

/** A response whose JSON body is already serialized (see store.ts issueJsonString). */
export function jsonRaw(status: number, body: string, extraHeaders: Record<string, string> = {}): HttpResponse {
  return {
    status,
    headers: { 'Content-Type': 'application/json', ...extraHeaders },
    body,
  };
}

export function err(status: number, message: string): HttpResponse {
  return json(status, { message, documentation_url: '' });
}

export function noContent(): HttpResponse {
  return { status: 204, headers: {}, body: '' };
}

function notFound(message = 'Not Found'): HttpResponse {
  return err(404, message);
}

function paginate<T>(items: T[], query: URLSearchParams, path: string, baseUrl: string): { page: T[]; link: string | null } {
  const perPageRaw = parseInt(query.get('per_page') ?? '', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const pageRaw = parseInt(query.get('page') ?? '', 10);
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link: string | null = null;
  const total = items.length;
  const links: string[] = [];
  const mk = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('per_page', String(perPage));
    q.set('page', String(p));
    return `<${baseUrl}${path}?${q.toString()}>`;
  };
  if (start + perPage < total) links.push(`${mk(page + 1)}; rel="next"`);
  if (page > 1) links.push(`${mk(page - 1)}; rel="prev"`);
  if (total > 0) {
    const last = Math.max(1, Math.ceil(total / perPage));
    links.push(`${mk(1)}; rel="first"`);
    links.push(`${mk(last)}; rel="last"`);
  }
  if (links.length) link = links.join(', ');
  return { page: slice, link };
}

function linkHeader(link: string | null): Record<string, string> {
  return link ? { Link: link } : {};
}

function bodyObj(req: HttpRequest): Record<string, unknown> {
  if (req.bodyJson && typeof req.bodyJson === 'object' && !Array.isArray(req.bodyJson)) {
    return req.bodyJson as Record<string, unknown>;
  }
  return {};
}

/** Strict base64: reject characters outside the alphabet and bad padding/length
 * (Node's Buffer.from silently drops invalid characters, so without this check
 * a garbage `content` would be stored as garbage bytes). Whitespace is ignored
 * and missing padding is tolerated, as real clients send both forms. */
function decodeBase64Strict(s: string): Buffer {
  const t = s.replace(/\s/g, '');
  if (t === '' || t.length % 4 === 1 || !/^[A-Za-z0-9+/]*={0,2}$/.test(t)) throw new Error('Invalid base64 content');
  const padded = t + '='.repeat((4 - (t.length % 4)) % 4);
  const buf = Buffer.from(padded, 'base64');
  if (buf.toString('base64') !== padded) throw new Error('Invalid base64 content');
  return buf;
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function bool(v: unknown, fallback = false): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

function repoRef(store: Store, owner: string, repo: string): { repo: Repo } | HttpResponse {
  const found = store.getRepo(owner, repo);
  if (!found) return notFound('Not Found');
  return { repo: found };
}

function branchNameFrom(path: string): string {
  // path is everything after /api/v3/repos/{o}/{r}/branches/
  return decodeURIComponent(path);
}

export function handleRest(store: Store, req: HttpRequest, baseUrl: string): HttpResponse | null {
  const path = req.path;
  if (!path.startsWith('/api/v3')) return null;
  const rest = path.slice('/api/v3'.length) || '/';
  const m = req.method;

  // GET /api/v3/ — root
  if (rest === '/' || rest === '') {
    if (m !== 'GET') return err(405, 'Method Not Allowed');
    return json(200, { current_user_url: '/api/v3/user' }, { 'X-OAuth-Scopes': 'repo, read:org' });
  }

  // GET /api/v3/user
  if (rest === '/user') {
    if (m === 'GET') return json(200, userJson());
    return err(405, 'Method Not Allowed');
  }

  // GET /api/v3/meta
  if (rest === '/meta') {
    if (m === 'GET') return json(200, { installed_version: '1.0.0' });
    return err(405, 'Method Not Allowed');
  }

  // /api/v3/user/repos
  if (rest === '/user/repos') {
    if (m === 'GET') {
      const repos = store.viewerRepos();
      const { page, link } = paginate(repos.map(repoJson), req.query, '/api/v3/user/repos', baseUrl);
      return json(200, page, linkHeader(link));
    }
    if (m === 'POST') {
      if (req.bodyError) return err(400, 'Invalid JSON body');
      const b = bodyObj(req);
      const result = store.createRepo(
        str(b.name),
        bool(b.private, false),
        str(b.description),
        str(b.default_branch, 'main'),
      );
      if ('error' in result) {
        if (result.error === 'Repository already exists') return err(422, 'name already exists on this account');
        return err(400, result.error);
      }
      return json(201, repoJson(result));
    }
    return err(405, 'Method Not Allowed');
  }

  // /api/v3/repos/{owner}/{repo}/...
  const repoMatch = rest.match(/^\/repos\/([^/]+)\/([^/]+)(\/.*)?$/);
  if (repoMatch) {
    const owner = decodeURIComponent(repoMatch[1]!);
    const repoName = decodeURIComponent(repoMatch[2]!);
    const sub = repoMatch[3] ?? '';
    const found = repoRef(store, owner, repoName);
    if ('status' in found) return found;
    const repo = found.repo;

    // GET/PATCH/DELETE /repos/{o}/{r}
    if (sub === '' || sub === '/') {
      if (m === 'GET') return json(200, repoJson(repo));
      if (m === 'PATCH') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        if (typeof b.description === 'string') repo.description = b.description;
        if (typeof b.default_branch === 'string' && b.default_branch) repo.defaultBranch = b.default_branch;
        return json(200, repoJson(repo));
      }
      if (m === 'DELETE') {
        store.deleteRepo(repo);
        return noContent();
      }
      return err(405, 'Method Not Allowed');
    }

    // Branches
    if (sub === '/branches') {
      if (m !== 'GET') return err(405, 'Method Not Allowed');
      const branches = [...repo.refs.keys()]
        .filter((r) => r.startsWith('refs/heads/'))
        .map((r) => r.slice('refs/heads/'.length))
        .sort();
      return json(200, branches.map((name) => ({
        name,
        commit: { sha: repo.refs.get(`refs/heads/${name}`) ?? ZERO_OID, url: '' },
        protected: false,
      })));
    }
    const branchMatch = sub.match(/^\/branches\/(.+)$/);
    if (branchMatch) {
      const branch = branchNameFrom(branchMatch[1]!);
      const ref = `refs/heads/${branch}`;
      const oid = repo.refs.get(ref);
      if (!oid) return notFound('Branch not found');
      if (m !== 'GET') return err(405, 'Method Not Allowed');
      return json(200, {
        name: branch,
        commit: { sha: oid, url: '' },
        protected: false,
      });
    }

    // git/refs
    const gitRefMatch = sub.match(/^\/git\/refs\/(.+)$/);
    if (gitRefMatch) {
      const ref = decodeURIComponent(gitRefMatch[1]!);
      if (m === 'GET') {
        const oid = repo.refs.get(ref);
        if (!oid) return notFound('Reference does not exist');
        return json(200, { ref, node_id: '', object: { sha: oid, type: 'commit', url: '' }, url: '' });
      }
      return err(405, 'Method Not Allowed');
    }
    if (sub === '/git/refs') {
      if (m === 'POST') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        const ref = str(b.ref);
        const sha = str(b.sha);
        if (!ref.startsWith('refs/') || !isOid(sha)) return err(422, 'Invalid ref or sha');
        if (repo.refs.has(ref)) return err(422, 'Reference already exists');
        if (!repo.objects.has(sha)) return err(422, 'Object does not exist');
        repo.refs.set(ref, sha);
        return json(201, { ref, node_id: '', object: { sha, type: 'commit', url: '' }, url: '' });
      }
      return err(405, 'Method Not Allowed');
    }

    // Commits
    if (sub === '/commits') {
      if (m !== 'GET') return err(405, 'Method Not Allowed');
      const shaParam = req.query.get('sha');
      let tip: string | null = null;
      if (shaParam) {
        tip = store.resolveRef(repo, shaParam);
        if (!tip) return notFound('No commit found for SHA: ' + shaParam);
      } else {
        tip = store.refCommit(repo, `refs/heads/${repo.defaultBranch}`);
      }
      const list = tip ? listCommits(tip, store.lookup(repo)) : [];
      const { page, link } = paginate(
        list.map((c) => {
          const parsed = parseCommit(repo.objects.get(c)!.data);
          return {
            sha: c,
            node_id: '',
            commit: {
              message: parsed.message,
              author: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
              committer: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
              tree: { sha: parsed.tree, url: '' },
            },
            parents: parsed.parents.map((p) => ({ sha: p, url: '' })),
            url: '',
            html_url: '',
          };
        }),
        req.query,
        `/api/v3/repos/${owner}/${repoName}/commits`,
        baseUrl,
      );
      return json(200, page, linkHeader(link));
    }

    // Contents
    if (sub === '/contents' || sub === '/contents/') {
      return handleContents(store, repo, req, owner, repoName, '');
    }
    const contentsMatch = sub.match(/^\/contents\/(.+)$/);
    if (contentsMatch) {
      return handleContents(store, repo, req, owner, repoName, contentsMatch[1]!);
    }

    // Labels
    if (sub === '/labels') {
      if (m === 'GET') {
        const labels = [...repo.labels.values()].sort((a, b) => (a.name < b.name ? -1 : 1));
        return json(200, labels.map(labelJson));
      }
      if (m === 'POST') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        const name = str(b.name);
        if (!name) return err(422, 'Validation Failed: name is required');
        const color = str(b.color, 'ffffff').replace(/^#/, '');
        const label = { name, color, description: str(b.description) };
        repo.labels.set(name, label);
        return json(201, labelJson(label));
      }
      return err(405, 'Method Not Allowed');
    }

    // Issues (list/create)
    if (sub === '/issues') {
      if (m === 'GET') {
        const state = req.query.get('state') ?? 'open';
        const sort = req.query.get('sort') ?? 'created';
        const direction = req.query.get('direction') ?? 'desc';
        let issues = [...repo.issues.values()];
        if (state === 'open') issues = issues.filter((i) => i.state === 'open');
        else if (state === 'closed') issues = issues.filter((i) => i.state === 'closed');
        else if (state !== 'all') return err(422, `Invalid state "${state}"`);
        issues.sort((a, b) => (sort === 'created' ? a.number - b.number : a.number - b.number));
        if (direction === 'desc') issues.reverse();
        const { page, link } = paginate(
          issues,
          req.query,
          `/api/v3/repos/${owner}/${repoName}/issues`,
          baseUrl,
        );
        return jsonRaw(200, `[${page.map((i) => issueJsonString(i, repo)).join(',')}]`, linkHeader(link));
      }
      if (m === 'POST') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        const title = str(b.title);
        if (!title) return err(422, 'Validation Failed: title is required');
        const labelNames: string[] = Array.isArray(b.labels)
          ? (b.labels as unknown[]).map((l) => (typeof l === 'string' ? l : str((l as Record<string, unknown>).name)))
          : [];
        const result = store.createIssue(repo, title, str(b.body), labelNames);
        if ('error' in result) return err(422, result.error);
        return jsonRaw(201, issueJsonString(result, repo));
      }
      return err(405, 'Method Not Allowed');
    }

    // Issue single / comments
    const issueMatch = sub.match(/^\/issues\/(\d+)(\/comments)?$/);
    if (issueMatch) {
      const number = parseInt(issueMatch[1]!, 10);
      const issue = repo.issues.get(number);
      if (!issue) return notFound('Not Found');
      if (!issueMatch[2]) {
        if (m === 'GET') return jsonRaw(200, issueJsonString(issue, repo));
        if (m === 'PATCH') {
          if (req.bodyError) return err(400, 'Invalid JSON body');
          const b = bodyObj(req);
          if (typeof b.state === 'string' && (b.state === 'open' || b.state === 'closed')) {
            issue.state = b.state;
            issue.updatedAt = FIXED_ISO;
          }
          if (typeof b.title === 'string') issue.title = b.title;
          if (typeof b.body === 'string') issue.body = b.body;
          invalidateIssueJson(issue);
          return jsonRaw(200, issueJsonString(issue, repo));
        }
        return err(405, 'Method Not Allowed');
      }
      // /comments
      if (m === 'GET') {
        return json(200, issue.comments.map((c) => commentJson(c, repo, number)));
      }
      if (m === 'POST') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        const body = str(b.body);
        if (!body) return err(422, 'Validation Failed: body is required');
        const comment = {
          id: repo.nextCommentId++,
          body,
          user: VIEWER.login,
          createdAt: FIXED_ISO,
        };
        issue.comments.push(comment);
        invalidateIssueJson(issue);
        return json(201, commentJson(comment, repo, number));
      }
      return err(405, 'Method Not Allowed');
    }

    // Pulls list/create
    if (sub === '/pulls') {
      if (m === 'GET') {
        const state = req.query.get('state') ?? 'open';
        let pulls = [...repo.pulls.values()];
        if (state === 'open') pulls = pulls.filter((p) => p.state === 'open');
        else if (state === 'closed') pulls = pulls.filter((p) => p.state === 'closed');
        else if (state !== 'all') return err(422, `Invalid state "${state}"`);
        pulls.sort((a, b) => b.number - a.number);
        const { page, link } = paginate(
          pulls.map((p) => pullJson(store, repo, p)),
          req.query,
          `/api/v3/repos/${owner}/${repoName}/pulls`,
          baseUrl,
        );
        return json(200, page, linkHeader(link));
      }
      if (m === 'POST') {
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const b = bodyObj(req);
        const title = str(b.title);
        const head = str(b.head);
        const base = str(b.base);
        if (!title || !head || !base) return err(422, 'Validation Failed: title, head and base are required');
        const result = store.createPull(repo, title, str(b.body), head, base);
        if ('error' in result) return err(422, result.error);
        return json(201, pullJson(store, repo, result));
      }
      return err(405, 'Method Not Allowed');
    }

    // Pull single / files / merge
    const pullMatch = sub.match(/^\/pulls\/(\d+)(\/(files|merge))?$/);
    if (pullMatch) {
      const number = parseInt(pullMatch[1]!, 10);
      const pull = repo.pulls.get(number);
      if (!pull) return notFound('Not Found');
      const action = pullMatch[3];
      if (!action) {
        if (m === 'GET') return json(200, pullJson(store, repo, pull));
        if (m === 'PATCH') {
          if (req.bodyError) return err(400, 'Invalid JSON body');
          const b = bodyObj(req);
          if (typeof b.state === 'string' && b.state === 'closed' && pull.state === 'open') {
            pull.state = 'closed';
          }
          if (typeof b.title === 'string') pull.title = b.title;
          if (typeof b.body === 'string') pull.body = b.body;
          return json(200, pullJson(store, repo, pull));
        }
        return err(405, 'Method Not Allowed');
      }
      if (action === 'files') {
        if (m !== 'GET') return err(405, 'Method Not Allowed');
        const diff = store.diffPull(repo, pull);
        const files = [...diff.entries()]
          .sort((a, b) => (a[0] < b[0] ? -1 : 1))
          .map(([filename, d]) => {
            const oldText = d.baseOid ? blobText(repo, d.baseOid) : '';
            const newText = d.headOid ? blobText(repo, d.headOid) : '';
            const counts = lineDiffCounts(oldText, newText);
            const additions = d.status === 'removed' ? 0 : counts.additions;
            const deletions = d.status === 'added' ? 0 : counts.deletions;
            return {
              filename,
              status: d.status,
              additions: Math.max(additions, d.status === 'removed' ? 0 : 1),
              deletions,
              changes: additions + deletions,
              sha: d.headOid ?? d.baseOid ?? '',
              raw_url: '',
              blob_url: '',
            };
          });
        return json(200, files);
      }
      if (action === 'merge') {
        if (m !== 'PUT') return err(405, 'Method Not Allowed');
        if (req.bodyError) return err(400, 'Invalid JSON body');
        const result = store.mergePull(repo, pull);
        if (typeof result === 'object') {
          return err(result.status, result.error);
        }
        return json(200, {
          sha: result,
          merged: true,
          message: `Pull Request successfully merged`,
        });
      }
      return err(405, 'Method Not Allowed');
    }

    return notFound('Not Found');
  }

  return notFound('Not Found');
}

function blobText(repo: Repo, oid: string): string {
  const obj = repo.objects.get(oid);
  if (!obj || obj.type !== 'blob') return '';
  return obj.data.toString('utf8');
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

function handleContents(
  store: Store,
  repo: Repo,
  req: HttpRequest,
  owner: string,
  repoName: string,
  rawPath: string,
): HttpResponse {
  const path = decodeURIComponent(rawPath);
  const m = req.method;
  const refParam = req.query.get('ref');
  const refName = refParam ?? repo.defaultBranch;
  const ref = `refs/heads/${refName}`;

  if (m === 'GET') {
    const treeMap = store.treeMapAt(repo, ref);
    const entry = treeMap.get(path);
    if (entry) {
      const obj = repo.objects.get(entry.oid);
      const content = obj?.type === 'blob' ? obj.data.toString('base64') : '';
      return json(200, {
        type: 'file',
        encoding: 'base64',
        size: obj?.data.length ?? 0,
        name: path.split('/').pop() ?? path,
        path,
        content,
        sha: entry.oid,
        url: `/api/v3/repos/${owner}/${repoName}/contents/${path}`,
        html_url: `https://example.invalid/${owner}/${repoName}/blob/${refName}/${path}`,
      });
    }
    // Directory listing?
    const prefix = path === '' ? '' : `${path}/`;
    const children = new Map<string, { type: 'file' | 'dir'; oid: string }>();
    for (const [p, e] of treeMap) {
      if (!p.startsWith(prefix)) continue;
      const rest = p.slice(prefix.length);
      if (!rest) continue;
      const first = rest.split('/')[0]!;
      if (rest.includes('/')) {
        if (!children.has(first)) children.set(first, { type: 'dir', oid: '' });
      } else {
        children.set(first, { type: 'file', oid: e.oid });
      }
    }
    if (children.size === 0) return notFound('Not Found');
    const listing = [...children.entries()]
      .sort((a, b) => (a[0] < b[0] ? -1 : 1))
      .map(([name, c]) => ({
        type: c.type,
        name,
        path: prefix + name,
        sha: c.oid || createHash('sha1').update(prefix + name).digest('hex'),
        size: 0,
        url: '',
        html_url: '',
      }));
    return json(200, listing);
  }

  if (m === 'PUT') {
    if (req.bodyError) return err(400, 'Invalid JSON body');
    const b = bodyObj(req);
    const message = str(b.message);
    const contentB64 = str(b.content);
    if (!message) return err(422, 'Validation Failed: message is required');
    if (!contentB64) return err(422, 'Validation Failed: content is required');
    let content: Buffer;
    try {
      content = decodeBase64Strict(contentB64);
    } catch {
      return err(422, 'Invalid base64 content');
    }
    const branch = str(b.branch, repo.defaultBranch);
    let branchRef = `refs/heads/${branch}`;
    let existing = repo.refs.get(branchRef);
    if (!existing) {
      // Create the branch at the default-branch tip (or unborn).
      const defaultTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (defaultTip) {
        repo.refs.set(branchRef, defaultTip);
        existing = defaultTip;
      }
    }
    const treeMap = store.treeMapAt(repo, branchRef);
    const current = treeMap.get(path);
    const isCreate = !current;
    if (!isCreate) {
      const sha = str(b.sha);
      if (!sha || sha !== current!.oid) return err(409, 'sha does not match the current file');
    }
    const blobOid = store.putObject(repo, 'blob', content);
    treeMap.set(path, { oid: blobOid, mode: '100644' });
    const treeOid = store.buildTreeFromMap(repo, treeMap);
    const parents = existing ? [existing] : [];
    const commitOid = store.createCommit(repo, treeOid, parents, message);
    repo.refs.set(branchRef, commitOid);
    return json(isCreate ? 201 : 200, {
      content: {
        name: path.split('/').pop() ?? path,
        path,
        sha: blobOid,
        size: content.length,
        encoding: 'base64',
        content: contentB64,
        url: `/api/v3/repos/${owner}/${repoName}/contents/${path}`,
      },
      commit: {
        sha: commitOid,
        message,
        author: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
        committer: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
      },
    });
  }

  if (m === 'DELETE') {
    if (req.bodyError) return err(400, 'Invalid JSON body');
    const b = bodyObj(req);
    const message = str(b.message);
    const sha = str(b.sha);
    const branch = str(b.branch, repo.defaultBranch);
    const branchRef = `refs/heads/${branch}`;
    const existing = repo.refs.get(branchRef);
    if (!existing) return notFound('Not Found');
    const treeMap = store.treeMapAt(repo, branchRef);
    const current = treeMap.get(path);
    if (!current) return notFound('Not Found');
    if (!sha || sha !== current.oid) return err(409, 'sha does not match the current file');
    treeMap.delete(path);
    const treeOid = store.buildTreeFromMap(repo, treeMap);
    const commitOid = store.createCommit(repo, treeOid, [existing], message || `Delete ${path}`);
    repo.refs.set(branchRef, commitOid);
    return json(200, {
      content: null,
      commit: {
        sha: commitOid,
        message: message || `Delete ${path}`,
        author: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
        committer: { name: VIEWER.name, email: VIEWER.email, date: FIXED_ISO },
      },
    });
  }

  return err(405, 'Method Not Allowed');
}

// Re-export helpers used by other modules.
export { parseCommit, parseTree, listCommits, hashObject, ZERO_OID };
export type { GitObject, Issue, Pull, Repo };
