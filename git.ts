// git.ts — pure git object/pack/protocol helpers for the forge (no I/O, no git binary).
import { createHash } from 'node:crypto';
import { deflateSync as zlibDeflate, inflateSync as zlibInflate } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);
export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject { type: ObjType; data: Buffer; }

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjType, data: Buffer): string {
  // Hash incrementally: the Buffer.concat a naive version builds is pure
  // allocation (one copy of the object bytes per hash, three per push pack).
  // Same bytes hashed, same oid, no copy.
  const hash = createHash('sha1');
  hash.update(`${type} ${data.length}\0`, 'ascii');
  hash.update(data);
  return hash.digest('hex');
}

export function isOid(v: string): boolean {
  return /^[0-9a-f]{40}$/.test(v);
}

export interface TreeEntry { mode: string; name: string; oid: string; }

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp + 1);
    if (sp < 0 || nul < 0 || nul + 21 > data.length) throw new Error('bad tree');
    entries.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return entries;
}

function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const l = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const r = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(l, r);
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView { tree: string; parents: string[]; author: string; committer: string; message: string; }

export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headerText = data.subarray(0, headerEnd).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of headerText.split('\n')) {
    if (!line) continue;
    const sp = line.indexOf(' ');
    const key = sp < 0 ? line : line.slice(0, sp);
    const value = sp < 0 ? '' : line.slice(sp + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

export interface TagView { object: string; type: string; tag: string; tagger: string; message: string; }

export function parseTag(data: Buffer): TagView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headers = new Map<string, string>();
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const sp = line.indexOf(' ');
    if (sp < 0) continue;
    headers.set(line.slice(0, sp), line.slice(sp + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: headers.get('type') ?? '',
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

/** Build a commit object's bytes. Message is written verbatim (no trailing newline added). */
export function buildCommit(tree: string, parents: string[], message: string, who: string, timestamp: number): Buffer {
  const lines = [`tree ${tree}`];
  for (const p of parents) lines.push(`parent ${p}`);
  lines.push(`author ${who} ${timestamp} +0000`, `committer ${who} ${timestamp} +0000`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

/** Build an annotated tag object's bytes. */
export function buildTag(object: string, type: ObjType, tag: string, message: string, tagger: string, timestamp: number): Buffer {
  return Buffer.from(
    `object ${object}\ntype ${type}\ntag ${tag}\ntagger ${tagger} ${timestamp} +0000\n\n${message}`,
    'utf8',
  );
}

// ---------------------------------------------------------------------------
// pack reading / writing
// ---------------------------------------------------------------------------

function readVarintSize(buf: Buffer, start: number): { size: number; next: number } {
  let size = buf[start]! & 0x0f;
  let shift = 4;
  let i = start + 1;
  while (buf[i - 1]! & 0x80) {
    size |= (buf[i]! & 0x7f) << shift;
    shift += 7;
    i += 1;
  }
  return { size, next: i };
}

function readOfsDeltaBase(buf: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buf[start]!;
  let offset = byte & 0x7f;
  let i = start + 1;
  while (byte & 0x80) {
    byte = buf[i]!;
    offset = ((offset + 1) << 7) | (byte & 0x7f);
    i += 1;
  }
  return { baseOffset: offset, next: i };
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let out = 0;
    let shift = 0;
    for (;;) {
      const b = delta[pos++]!;
      out |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return out;
  };
  const srcSize = readVarint();
  const dstSize = readVarint();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out: Buffer[] = [];
  let total = 0;
  while (pos < delta.length) {
    const op = delta[pos++]!;
    if (op & 0x80) {
      let cpOff = 0;
      let cpSize = 0;
      if (op & 0x01) cpOff = delta[pos++]!;
      if (op & 0x02) cpOff |= delta[pos++]! << 8;
      if (op & 0x04) cpOff |= delta[pos++]! << 16;
      if (op & 0x08) cpOff |= delta[pos++]! << 24;
      if (op & 0x10) cpSize = delta[pos++]!;
      if (op & 0x20) cpSize |= delta[pos++]! << 8;
      if (op & 0x40) cpSize |= delta[pos++]! << 16;
      if (cpSize === 0) cpSize = 0x10000;
      out.push(base.subarray(cpOff, cpOff + cpSize));
      total += cpSize;
    } else if (op !== 0) {
      out.push(delta.subarray(pos, pos + op));
      total += op;
      pos += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out);
  if (total !== dstSize || result.length !== dstSize) throw new Error('delta result size mismatch');
  return result;
}

export interface PackReadResult {
  objects: Map<string, GitObject>;
  entries: number;
  deltaCount: number;
}

/** Parse a packfile, resolving OFS/REF deltas. resolveBase looks up objects outside the pack. */
export function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): PackReadResult {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  const expect = sha1Hex(pack.subarray(0, pack.length - 20));
  if (trailer !== expect) throw new Error('pack checksum mismatch');

  interface Raw { offset: number; type: number; data: Buffer; baseOff?: number; baseOid?: string; }
  const raws: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let off = 12;
  for (let i = 0; i < count; i++) {
    const start = off;
    const type = (pack[off]! >> 4) & 0x7;
    const { next } = readVarintSize(pack, off);
    off = next;
    let baseOff: number | undefined;
    let baseOid: string | undefined;
    if (type === 6) {
      const r = readOfsDeltaBase(pack, off);
      baseOff = start - r.baseOffset;
      off = r.next;
    } else if (type === 7) {
      baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
    }
    const { data, next: after } = inflateAt(pack, off);
    off = after;
    const raw: Raw = { offset: start, type, data, baseOff, baseOid };
    raws.push(raw);
    byOffset.set(start, raw);
  }

  const objects = new Map<string, GitObject>();
  const oidByOffset = new Map<number, string>();
  let deltaCount = 0;
  const resolve = (raw: Raw, seen: Set<number>): GitObject => {
    if (raw.type >= 1 && raw.type <= 4) {
      const type = CODE_TYPE[raw.type]!;
      return { type, data: raw.data };
    }
    deltaCount += 1;
    let base: GitObject | undefined;
    if (raw.type === 6) {
      const baseRaw = byOffset.get(raw.baseOff!);
      if (!baseRaw) throw new Error('ofs-delta base not in pack');
      if (seen.has(raw.baseOff!)) throw new Error('delta cycle');
      seen.add(raw.baseOff!);
      base = resolve(baseRaw, seen);
    } else if (raw.type === 7) {
      const baseRaw = raws.find((r) => oidByOffset.get(r.offset) === raw.baseOid);
      if (baseRaw) {
        if (seen.has(baseRaw.offset)) throw new Error('delta cycle');
        seen.add(baseRaw.offset);
        base = resolve(baseRaw, seen);
      } else if (resolveBase) {
        base = resolveBase(raw.baseOid!);
      }
      if (!base) throw new Error(`ref-delta base ${raw.baseOid} not found`);
    } else {
      throw new Error(`unknown pack object type ${raw.type}`);
    }
    const data = applyDelta(base.data, raw.data);
    return { type: base.type, data };
  };
  for (const raw of raws) {
    const obj = resolve(raw, new Set([raw.offset]));
    const oid = hashObject(obj.type, obj.data);
    oidByOffset.set(raw.offset, oid);
    objects.set(oid, obj);
  }
  return { objects, entries: count, deltaCount };
}

/**
 * Inflate one zlib stream starting at `offset` and return its bytes plus the offset just
 * past the stream. inflateSync ignores trailing bytes but throws on a truncated stream,
 * so the exact end is found by binary search for the shortest succeeding prefix.
 *
 * Fast path first: one inflateSync of the whole tail returns the stream's bytes, and the
 * stream's end is the 4-byte Adler-32 of those bytes (zlib's trailer), found with a scan.
 * A false hit is ~1 in 2^32 per position, and a wrong end fails loudly downstream (the
 * next object's header will not parse, or the pack trailer check will not line up), never
 * silently. When the scan misses, fall back to the binary search below.
 */
function inflateAt(pack: Buffer, offset: number): { data: Buffer; next: number } {
  const limit = pack.length - 20; // never run into the pack trailer
  let data: Buffer;
  try {
    data = zlibInflate(pack.subarray(offset, limit));
  } catch {
    throw new Error('truncated zlib stream in pack');
  }
  const adler = adler32(data);
  const trailer = Buffer.alloc(4);
  trailer.writeUInt32BE(adler, 0);
  // The stream is at least a 2-byte zlib header plus a 2-byte final empty block.
  const idx = pack.indexOf(trailer, offset + 2);
  if (idx >= 0 && idx + 4 <= limit) {
    return { data, next: idx + 4 };
  }
  return inflateAtBinarySearch(pack, offset);
}

/** Adler-32 (zlib's checksum) of `data`, as an unsigned 32-bit number. */
function adler32(data: Buffer): number {
  const MOD = 65521;
  let a = 1;
  let b = 0;
  // Defer the mod: 5552 is the largest run that cannot overflow a 32-bit accumulator.
  for (let off = 0; off < data.length; off += 5552) {
    const end = Math.min(data.length, off + 5552);
    for (let i = off; i < end; i++) {
      a += data[i]!;
      b += a;
    }
    a %= MOD;
    b %= MOD;
  }
  return ((b << 16) | a) >>> 0;
}

function inflateAtBinarySearch(pack: Buffer, offset: number): { data: Buffer; next: number } {
  const limit = pack.length - 20; // never run into the pack trailer
  let lo = offset + 1;
  let hi = limit;
  // Invariant: inflateSync(pack[offset:hi]) succeeds; find the smallest such hi.
  let data: Buffer;
  try {
    data = zlibInflate(pack.subarray(offset, hi));
  } catch {
    throw new Error('truncated zlib stream in pack');
  }
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    try {
      zlibInflate(pack.subarray(offset, mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  data = zlibInflate(pack.subarray(offset, lo));
  return { data, next: lo };
}

/** Build a packfile from plain objects (no deltas). Deterministic for a given input order. */
export function writePack(objects: { type: ObjType; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    const code = TYPE_CODE[object.type];
    const bytes: number[] = [];
    let remaining = object.data.length;
    let first = (code << 4) | (remaining & 0x0f);
    remaining = Math.floor(remaining / 16);
    while (remaining > 0) {
      bytes.push(first | 0x80);
      first = remaining & 0x7f;
      remaining = Math.floor(remaining / 128);
    }
    bytes.push(first);
    parts.push(Buffer.from(bytes));
    parts.push(zlibDeflate(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

export function verifyPackChecksum(pack: Buffer): boolean {
  if (pack.length < 20) return false;
  return pack.subarray(pack.length - 20).toString('hex') === sha1Hex(pack.subarray(0, pack.length - 20));
}

// ---------------------------------------------------------------------------
// traversal
// ---------------------------------------------------------------------------

export type Lookup = (oid: string) => GitObject | undefined;

/** All object ids reachable from tips (commits, trees, blobs, tags), in visit order. */
export function collectReachable(tips: Iterable<string>, lookup: Lookup): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length) {
    const oid = stack.pop()!;
    if (seen.has(oid) || !isOid(oid)) continue;
    const obj = lookup(oid);
    if (!obj) continue;
    seen.add(oid);
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
  return seen;
}

/** Is `ancestor` reachable from `oid` (following commit parents, peeling tags)? */
export function isAncestor(ancestor: string, oid: string, lookup: Lookup): boolean {
  if (ancestor === oid) return true;
  const seen = new Set<string>();
  const stack = [oid];
  while (stack.length) {
    const cur = stack.pop()!;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const obj = lookup(cur);
    if (!obj) continue;
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      for (const p of c.parents) {
        if (p === ancestor) return true;
        stack.push(p);
      }
    } else if (obj.type === 'tag') {
      const t = parseTag(obj.data);
      if (t.object === ancestor) return true;
      stack.push(t.object);
    }
  }
  return false;
}

/** Commits reachable from `tip`, newest-first (DFS preorder, parents after the commit). */
export function listCommits(tip: string, lookup: Lookup): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const visit = (oid: string): void => {
    if (seen.has(oid)) return;
    const obj = lookup(oid);
    if (!obj) return;
    if (obj.type === 'tag') { visit(parseTag(obj.data).object); return; }
    if (obj.type !== 'commit') return;
    seen.add(oid);
    out.push(oid);
    for (const p of parseCommit(obj.data).parents) visit(p);
  };
  visit(tip);
  return out;
}

/** A merge-base of a and b: the common ancestor nearest to a (BFS from a, then BFS from b). */
export function mergeBase(a: string, b: string, lookup: Lookup): string | null {
  const ancestorsA = new Set<string>();
  const queueA = [a];
  while (queueA.length) {
    const cur = queueA.shift()!;
    if (ancestorsA.has(cur)) continue;
    const obj = lookup(cur);
    if (!obj || obj.type !== 'commit') continue;
    ancestorsA.add(cur);
    for (const p of parseCommit(obj.data).parents) queueA.push(p);
  }
  const queueB = [b];
  const seenB = new Set<string>();
  while (queueB.length) {
    const cur = queueB.shift()!;
    if (seenB.has(cur)) continue;
    seenB.add(cur);
    if (ancestorsA.has(cur)) return cur;
    const obj = lookup(cur);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) queueB.push(p);
  }
  return null;
}

/** Flat path -> {oid, mode} map of a tree (recursive). */
export function flattenTree(treeOid: string, lookup: Lookup): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  const walk = (oid: string, prefix: string): void => {
    const obj = lookup(oid);
    if (!obj || obj.type !== 'tree') return;
    for (const e of parseTree(obj.data)) {
      const path = prefix ? `${prefix}/${e.name}` : e.name;
      if (e.mode === '40000') walk(e.oid, path);
      else out.set(path, { oid: e.oid, mode: e.mode });
    }
  };
  walk(treeOid, '');
  return out;
}

/** Entries of a tree at one level (for directory listings). */
export function treeLevel(treeOid: string, lookup: Lookup): TreeEntry[] {
  const obj = lookup(treeOid);
  if (!obj || obj.type !== 'tree') return [];
  return parseTree(obj.data);
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

export function encodePktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  if (data.length > 65516) throw new Error('pkt-line too long');
  const header = Buffer.alloc(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export function encodePktLines(items: (string | Buffer | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(PKT_FLUSH);
    else if (item === 'delim') parts.push(PKT_DELIM);
    else parts.push(encodePktLine(item));
  }
  return Buffer.concat(parts);
}

export type Pkt = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'response-end' };

/** Decode a whole buffer of pkt-lines. Throws on garbage. */
export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off < buf.length) {
    if (off + 4 > buf.length) throw new Error('truncated pkt-line header');
    const lenHex = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(lenHex)) throw new Error(`bad pkt-line length ${JSON.stringify(lenHex)}`);
    const len = parseInt(lenHex, 16);
    if (len === 0) { out.push({ kind: 'flush' }); off += 4; continue; }
    if (len === 1) { out.push({ kind: 'delim' }); off += 4; continue; }
    if (len === 2) { out.push({ kind: 'response-end' }); off += 4; continue; }
    if (len < 4 || off + len > buf.length) throw new Error('truncated pkt-line');
    out.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return out;
}

/** Split a receive-pack body into command pkt-lines and the trailing pack bytes. */
export function splitCommandsAndPack(body: Buffer): { packets: Pkt[]; pack: Buffer } {
  let off = 0;
  const packets: Pkt[] = [];
  while (off < body.length) {
    if (off + 4 > body.length) throw new Error('truncated pkt-line header');
    const lenHex = body.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(lenHex)) throw new Error('bad pkt-line length');
    const len = parseInt(lenHex, 16);
    if (len === 0) { packets.push({ kind: 'flush' }); off += 4; break; }
    if (len === 1) { packets.push({ kind: 'delim' }); off += 4; continue; }
    if (len < 4 || off + len > body.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: body.subarray(off + 4, off + len) });
    off += len;
  }
  return { packets, pack: body.subarray(off) };
}
