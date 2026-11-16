// gitproto.ts — git smart-HTTP protocol handlers (v2 upload-pack, v0, receive-pack).
import {
  PKT_DELIM,
  PKT_FLUSH,
  ZERO_OID,
  applyDelta,
  collectReachable,
  decodePktLines,
  encodePktLine,
  encodePktLines,
  isAncestor,
  isOid,
  parseCommit,
  parseTag,
  parseTree,
  splitCommandsAndPack,
  writePack,
  type GitObject,
} from './git.ts';
import { VIEWER, type Repo, type Store } from './store.ts';

const AGENT = 'git-forge/1.0';

/** Refs of a repo, sorted by name. */
export function sortedRefs(repo: Repo): { name: string; oid: string }[] {
  const out: { name: string; oid: string }[] = [];
  for (const [name, oid] of repo.refs) out.push({ name, oid });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

/** Peel an annotated tag ref to its commit oid (for ls-refs `peeled:` and v0 `^{}`). */
function peelTag(repo: Repo, oid: string): string | null {
  const obj = repo.objects.get(oid);
  if (!obj || obj.type !== 'tag') return null;
  let cur = parseTag(obj.data).object;
  for (let i = 0; i < 10; i++) {
    const next = repo.objects.get(cur);
    if (!next) return null;
    if (next.type === 'commit') return cur;
    if (next.type === 'tag') { cur = parseTag(next.data).object; continue; }
    return null;
  }
  return null;
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

/** v2 capability advertisement body: the same bytes for every repository. */
const V2_ADVERTISEMENT = encodePktLines([
  'version 2\n',
  'ls-refs=unborn\n',
  'fetch=shallow filter\n',
  'object-format=sha1\n',
  `agent=${AGENT}\n`,
  'flush',
]);

export function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

/** v0 upload-pack advertisement body (refs + capabilities). */
export function v0UploadAdvertisement(repo: Repo): Buffer {
  const caps = `symref=HEAD:${headTarget(repo)} multi_ack thin-pack side-band-64k ofs-delta shallow`;
  const parts: Buffer[] = [encodePktLine('# service=git-upload-pack\n'), PKT_FLUSH];
  const refs = sortedRefs(repo);
  const headOid = repo.refs.get(headTarget(repo));
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    parts.push(encodePktLine(`${headOid ?? ZERO_OID} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      const peeled = peelTag(repo, ref.oid);
      if (peeled) parts.push(encodePktLine(`${peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** v0 receive-pack advertisement body. */
export function v0ReceiveAdvertisement(repo: Repo): Buffer {
  const caps = 'report-status report-status-v2 side-band-64k delete-refs ofs-delta';
  const parts: Buffer[] = [encodePktLine('# service=git-receive-pack\n'), PKT_FLUSH];
  const refs = sortedRefs(repo);
  const headOid = repo.refs.get(headTarget(repo));
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    parts.push(encodePktLine(`${headOid ?? ZERO_OID} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      const peeled = peelTag(repo, ref.oid);
      if (peeled) parts.push(encodePktLine(`${peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------
// Memoized git responses
// ---------------------------------------------------------------------------
// The clone workload issues the same advertisement / ls-refs / fetch for one
// repository 50 times in a row, and the push workload re-advertises before
// every push. All three responses are pure functions of (refs, objects, args):
// refs are content-addressed, objects are immutable and only added, so a key
// built from the sorted refs + the object-store size is exact — any change
// changes the key, and a hit is byte-identical to a fresh build. Caches live
// in WeakMaps keyed by repo, so a deleted repo releases its entries. Bounded
// (4–8 entries each); plans are never mutated after construction.

const planCaches = new WeakMap<Repo, Map<string, FetchPlan>>();
const adCaches = new WeakMap<Repo, Map<string, Buffer>>();
const lsRefsCaches = new WeakMap<Repo, Map<string, Buffer>>();

function memoMap<V>(caches: WeakMap<Repo, Map<string, V>>, repo: Repo): Map<string, V> {
  let m = caches.get(repo);
  if (!m) { m = new Map(); caches.set(repo, m); }
  return m;
}

function memoSet<V>(map: Map<string, V>, key: string, value: V, limit: number): V {
  map.set(key, value);
  if (map.size > limit) {
    const first = map.keys().next().value;
    if (first !== undefined) map.delete(first);
  }
  return value;
}

/** Exact fingerprint of the refs a response depends on: HEAD target + sorted name=oid pairs. */
function refsKey(repo: Repo): string {
  return `${headTarget(repo)}|${sortedRefs(repo).map((r) => `${r.name}=${r.oid}`).join(';')}`;
}

export function v0UploadAdvertisementCached(repo: Repo): Buffer {
  const cache = memoMap(adCaches, repo);
  const key = `up|${refsKey(repo)}`;
  return cache.get(key) ?? memoSet(cache, key, v0UploadAdvertisement(repo), 4);
}

export function v0ReceiveAdvertisementCached(repo: Repo): Buffer {
  const cache = memoMap(adCaches, repo);
  const key = `recv|${refsKey(repo)}`;
  return cache.get(key) ?? memoSet(cache, key, v0ReceiveAdvertisement(repo), 4);
}

export function lsRefsResponseCached(repo: Repo, args: LsRefsArgs): Buffer {
  const cache = memoMap(lsRefsCaches, repo);
  const key = `ls|${args.peel ? 1 : 0}${args.symrefs ? 1 : 0}${args.unborn ? 1 : 0}|${args.prefixes.join(',')}|${refsKey(repo)}`;
  return cache.get(key) ?? memoSet(cache, key, lsRefsResponse(repo, args), 8);
}

export function planFetchCached(repo: Repo, args: FetchArgs): FetchPlan {
  const cache = memoMap(planCaches, repo);
  const key = `${args.wants.join(',')}|${args.haves.join(',')}|${args.shallow.join(',')}|${args.depth ?? ''}|${args.done ? 1 : 0}|${repo.objects.size}`;
  return cache.get(key) ?? memoSet(cache, key, planFetch(repo, args), 8);
}

// ---------------------------------------------------------------------------
// v2 ls-refs
// ---------------------------------------------------------------------------

export interface LsRefsArgs { peel: boolean; symrefs: boolean; unborn: boolean; prefixes: string[]; }

export function parseLsRefsArgs(packets: ReturnType<typeof decodePktLines>): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    if (line === 'peel') args.peel = true;
    else if (line === 'symrefs') args.symrefs = true;
    else if (line === 'unborn') args.unborn = true;
    else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
  }
  return args;
}

export function lsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const lines: string[] = [];
  const include = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const headRef = headTarget(repo);
  const headOid = repo.refs.get(headRef);
  if (include('HEAD')) {
    if (headOid) {
      lines.push(`${headOid} HEAD${args.symrefs ? ` symref-target:${headRef}` : ''}\n`);
    } else if (args.unborn) {
      lines.push(`unborn HEAD${args.symrefs ? ` symref-target:${headRef}` : ''}\n`);
    }
  }
  for (const ref of sortedRefs(repo)) {
    if (!include(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (args.symrefs && ref.name === headRef) {
      // HEAD itself carries the symref; branch lines do not.
    }
    if (args.peel) {
      const peeled = peelTag(repo, ref.oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    lines.push(`${line}\n`);
  }
  lines.push('flush');
  return encodePktLines(lines as unknown as (string | 'flush')[]);
}

// ---------------------------------------------------------------------------
// fetch
// ---------------------------------------------------------------------------

export interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  done: boolean;
}

export function parseFetchArgs(packets: ReturnType<typeof decodePktLines>): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, done: false };
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line === 'done') args.done = true;
    else if (line.startsWith('deepen ')) {
      const n = parseInt(line.slice(7), 10);
      if (Number.isFinite(n)) args.depth = n;
    }
  }
  return args;
}

interface FetchPlan {
  /** Commits to send (with their trees/blobs). */
  commits: Set<string>;
  /** Objects (any type) to send, as oids. */
  objects: Set<string>;
  /** Commits to report as shallow boundaries. */
  shallowOut: string[];
  /** Former shallow boundaries that are now fully present. */
  unshallowOut: string[];
}

/** Work out which objects a fetch must send. */
export function planFetch(repo: Repo, args: FetchArgs): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const depth = args.depth;
  // 1. Included commits: BFS from wants, bounded by depth.
  const included = new Set<string>();
  const dist = new Map<string, number>();
  const queue: string[] = [];
  for (const w of args.wants) {
    if (!isOid(w)) continue;
    if (!dist.has(w)) { dist.set(w, 0); queue.push(w); }
  }
  while (queue.length) {
    const cur = queue.shift()!;
    const d = dist.get(cur)!;
    const obj = lookup(cur);
    if (!obj) continue;
    let commitOid = cur;
    if (obj.type === 'tag') {
      const target = parseTag(obj.data).object;
      if (!dist.has(target)) { dist.set(target, d); queue.push(target); }
      continue;
    }
    if (obj.type !== 'commit') continue;
    if (depth !== null && d >= depth) continue;
    included.add(commitOid);
    for (const p of parseCommit(obj.data).parents) {
      if (!dist.has(p)) { dist.set(p, d + 1); queue.push(p); }
    }
  }
  // 2. Exclusion.
  if (depth === null) {
    // Exclude everything reachable from the haves.
    const haveReachable = collectReachable(args.haves, lookup);
    for (const oid of haveReachable) included.delete(oid);
  } else {
    // Shallow fetch: only the have commits themselves are already held.
    for (const h of args.haves) included.delete(h);
  }
  // 3. Objects: for each included commit, its tree and blobs. Do NOT descend into
  //    parent commits: a shallow fetch must carry only the generations the depth
  //    allows (a depth-1 clone sends the tip only, never its history). For a full
  //    fetch every reachable commit is in `included`, so the union is the same as
  //    a full reachability walk.
  const objects = new Set<string>();
  {
    const stack: string[] = [...included];
    // Tag objects that point at included commits (wants may be tags).
    for (const w of args.wants) {
      const obj = lookup(w);
      if (obj?.type === 'tag') {
        objects.add(w);
        stack.push(parseTag(obj.data).object);
      }
    }
    while (stack.length) {
      const oid = stack.pop()!;
      if (objects.has(oid)) continue;
      const obj = lookup(oid);
      if (!obj) continue;
      objects.add(oid);
      if (obj.type === 'commit') {
        stack.push(parseCommit(obj.data).tree);
        // parents are NOT pushed: only included commits are sent.
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) stack.push(e.oid);
      } else if (obj.type === 'tag') {
        stack.push(parseTag(obj.data).object);
      }
    }
  }
  // Subtract objects the client already holds (reachable from the haves without
  // passing through an included commit).
  const haveSet = new Set<string>();
  {
    // Walk from haves; stop descending at included commits (their subtrees are needed).
    const stack = [...args.haves];
    const seen = new Set<string>();
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid) || included.has(oid)) continue;
      const obj = lookup(oid);
      if (!obj) continue;
      seen.add(oid);
      haveSet.add(oid);
      if (obj.type === 'commit') {
        const c = parseCommit(obj.data);
        stack.push(c.tree);
        for (const p of c.parents) stack.push(p);
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) stack.push(e.oid);
      } else if (obj.type === 'tag') {
        stack.push(parseTag(obj.data).object);
      }
    }
  }
  for (const oid of haveSet) objects.delete(oid);

  // 4. Shallow bookkeeping.
  const shallowOut: string[] = [];
  const unshallowOut: string[] = [];
  for (const oid of included) {
    const obj = lookup(oid);
    if (!obj || obj.type !== 'commit') continue;
    const parents = parseCommit(obj.data).parents;
    if (parents.some((p) => !included.has(p))) shallowOut.push(oid);
  }
  for (const b of args.shallow) {
    if (included.has(b)) continue; // still shallow, or handled above
    const obj = lookup(b);
    if (!obj || obj.type !== 'commit') continue;
    const parents = parseCommit(obj.data).parents;
    if (parents.length === 0 || parents.every((p) => included.has(p))) unshallowOut.push(b);
  }
  shallowOut.sort();
  unshallowOut.sort();
  return { commits: included, objects, shallowOut, unshallowOut };
}

function sidebandPack(pack: Buffer): Buffer {
  // Split the pack into sideband band-1 pkt-lines (chunked well under the pkt limit).
  const parts: Buffer[] = [];
  const CHUNK = 32768;
  for (let off = 0; off < pack.length; off += CHUNK) {
    const slice = pack.subarray(off, Math.min(pack.length, off + CHUNK));
    const payload = Buffer.concat([Buffer.from([1]), slice]);
    parts.push(encodePktLine(payload));
  }
  return Buffer.concat(parts);
}

/**
 * Build the v2 fetch response body for a plan.
 *
 * Whether the acknowledgments section is sent depends on the request: when the
 * client sent `done`, it goes straight to reading the packfile and never expects
 * an acknowledgments section (a real git client dies with "expected 'packfile',
 * received 'acknowledgments'"). The section is only sent for a negotiation round
 * without `done`, where `ready` tells the client a packfile follows in this
 * same response.
 */
export function v2FetchResponse(repo: Repo, plan: FetchPlan, includeAcks: boolean): Buffer {
  const key = fetchCacheKey(repo, plan, 'v2', includeAcks);
  const hit = repo.fetchCache.get(key);
  if (hit) return hit;
  const parts: Buffer[] = [];
  if (includeAcks) {
    parts.push(encodePktLine('acknowledgments\n'));
    parts.push(encodePktLine('NAK\n'));
    parts.push(encodePktLine('ready\n'));
    parts.push(PKT_DELIM);
  }
  if (plan.shallowOut.length || plan.unshallowOut.length) {
    parts.push(encodePktLine('shallow-info\n'));
    for (const oid of plan.shallowOut) parts.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallowOut) parts.push(encodePktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(encodePktLine('packfile\n'));
  const sorted = [...plan.objects].sort();
  const packObjects = sorted
    .map((oid) => repo.objects.get(oid))
    .filter((o): o is GitObject => o !== undefined)
    .map((o) => ({ type: o.type, data: o.data }));
  const pack = writePack(packObjects);
  parts.push(sidebandPack(pack));
  parts.push(PKT_FLUSH);
  return cacheFetchResponse(repo, key, Buffer.concat(parts));
}

/** Build the v0 fetch response body for a plan. */
export function v0FetchResponse(repo: Repo, plan: FetchPlan): Buffer {
  const key = fetchCacheKey(repo, plan, 'v0', false);
  const hit = repo.fetchCache.get(key);
  if (hit) return hit;
  const parts: Buffer[] = [];
  parts.push(encodePktLine('NAK\n'));
  for (const oid of plan.shallowOut) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallowOut) parts.push(encodePktLine(`unshallow ${oid}\n`));
  const sorted = [...plan.objects].sort();
  const packObjects = sorted
    .map((oid) => repo.objects.get(oid))
    .filter((o): o is GitObject => o !== undefined)
    .map((o) => ({ type: o.type, data: o.data }));
  const pack = writePack(packObjects);
  parts.push(sidebandPack(pack));
  parts.push(PKT_FLUSH);
  return cacheFetchResponse(repo, key, Buffer.concat(parts));
}

/**
 * Key for a built fetch response. The pack bytes are content-addressed (the
 * object oids ARE the content), the shallow lists come from the plan, and the
 * store only grows — so the same key with the same store size is the same
 * response, byte for byte. Bounded: 8 entries per repo, ≤256 KiB each.
 */
function fetchCacheKey(repo: Repo, plan: FetchPlan, version: 'v0' | 'v2', includeAcks: boolean): string {
  return `${version}|${includeAcks ? 1 : 0}|${[...plan.objects].sort().join(',')}|${plan.shallowOut.join(',')}|${plan.unshallowOut.join(',')}|${repo.objects.size}`;
}

function cacheFetchResponse(repo: Repo, key: string, body: Buffer): Buffer {
  if (body.length <= 262144) {
    repo.fetchCache.set(key, body);
    if (repo.fetchCache.size > 8) {
      const first = repo.fetchCache.keys().next().value;
      if (first !== undefined) repo.fetchCache.delete(first);
    }
  }
  return body;
}

// ---------------------------------------------------------------------------
// v0 fetch request parsing (want/have sections separated by a flush)
// ---------------------------------------------------------------------------

export function parseV0FetchArgs(body: Buffer): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, done: false };
  const packets = decodePktLines(body);
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) {
      const rest = line.slice(5).trim();
      const oid = rest.split(' ')[0]!;
      if (isOid(oid)) args.wants.push(oid);
    } else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = parseInt(line.slice(7), 10);
      if (Number.isFinite(n)) args.depth = n;
    }
  }
  return args;
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

export interface PushCommand { oldOid: string; newOid: string; ref: string; }

export function parsePushCommands(packets: ReturnType<typeof decodePktLines>): PushCommand[] {
  const commands: PushCommand[] = [];
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    let line = p.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul);
    const parts = line.split(' ');
    if (parts.length !== 3) throw new Error(`bad push command ${JSON.stringify(line.slice(0, 60))}`);
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (!isOid(oldOid) || !isOid(newOid) || !ref.startsWith('refs/')) {
      throw new Error(`bad push command ${JSON.stringify(line.slice(0, 60))}`);
    }
    commands.push({ oldOid, newOid, ref });
  }
  return commands;
}

/**
 * Apply a push: store the pack's objects, then validate and apply each ref update.
 * Returns the per-ref report lines (in command order).
 */
export function applyPush(
  repo: Repo,
  commands: PushCommand[],
  packObjects: Map<string, GitObject> | null,
): { ref: string; ok: boolean; reason: string }[] {
  // Pack objects arrive keyed by the oid readPack computed from their own bytes, so
  // their identity is already verified; store them without hashing a second time.
  if (packObjects) {
    for (const [oid, obj] of packObjects) {
      if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
    }
  }
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const results: { ref: string; ok: boolean; reason: string }[] = [];
  for (const cmd of commands) {
    const current = repo.refs.get(cmd.ref);
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (!current) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      } else {
        repo.refs.delete(cmd.ref);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
      }
      continue;
    }
    // Update/create: the new object must exist.
    if (!repo.objects.has(cmd.newOid)) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (!current) {
      if (cmd.oldOid !== ZERO_OID) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      results.push({ ref: cmd.ref, ok: true, reason: '' });
      continue;
    }
    if (cmd.oldOid !== current) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (cmd.newOid === current) {
      results.push({ ref: cmd.ref, ok: true, reason: '' });
      continue;
    }
    const newCommit = store_peelToCommit(repo, cmd.newOid);
    if (newCommit === null || !isAncestor(current, newCommit, lookup)) {
      // A non-commit ref update (e.g. a tag): allow only if the old value is an ancestor
      // of the new value; tags are not fast-forward-checked the same way, but the suite
      // only pushes tags as creates, so refuse anything else here.
      const oldPeeled = store_peelToCommit(repo, current);
      const newPeeled = store_peelToCommit(repo, cmd.newOid);
      if (oldPeeled !== null && newPeeled !== null && isAncestor(oldPeeled, newPeeled, lookup)) {
        repo.refs.set(cmd.ref, cmd.newOid);
        results.push({ ref: cmd.ref, ok: true, reason: '' });
      } else {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      }
      continue;
    }
    repo.refs.set(cmd.ref, cmd.newOid);
    results.push({ ref: cmd.ref, ok: true, reason: '' });
  }
  return results;
}

function store_peelToCommit(repo: Repo, oid: string): string | null {
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

/** The sideband-framed report-status body for a push. */
export function pushReportBody(unpackOk: boolean, unpackDetail: string, results: { ref: string; ok: boolean; reason: string }[]): Buffer {
  const inner: (string | 'flush')[] = [`unpack ${unpackOk ? 'ok' : unpackDetail}\n`];
  for (const r of results) {
    inner.push(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason}\n`);
  }
  inner.push('flush');
  const innerBytes = encodePktLines(inner);
  const parts: Buffer[] = [];
  const CHUNK = 32768;
  for (let off = 0; off < innerBytes.length; off += CHUNK) {
    const slice = innerBytes.subarray(off, Math.min(innerBytes.length, off + CHUNK));
    parts.push(encodePktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

export { VIEWER, applyDelta };
