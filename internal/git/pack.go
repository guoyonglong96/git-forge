// Package git — packfile support.
//
// Ported from git.ts (138-375): readPack / writePack / applyDelta /
// inflateAt / adler32 / verifyPackChecksum. The single-pass REF-delta
// resolution order is replicated exactly (map §5.2 / N2): a REF-delta
// whose base appears later in the same pack is NOT found in-pack and
// falls back to resolveBase; a pack Node rejects must be rejected the
// same way. Do NOT "fix" this to two-pass.
package git

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"io"
)

// PackReadResult is the result of reading a packfile.
type PackReadResult struct {
	Objects    map[string]GitObject
	Entries    int
	DeltaCount int
}

func typeCode(t ObjType) (int, bool) {
	switch t {
	case ObjCommit:
		return 1, true
	case ObjTree:
		return 2, true
	case ObjBlob:
		return 3, true
	case ObjTag:
		return 4, true
	}
	return 0, false
}

func codeType(c int) (ObjType, bool) {
	switch c {
	case 1:
		return ObjCommit, true
	case 2:
		return ObjTree, true
	case 3:
		return ObjBlob, true
	case 4:
		return ObjTag, true
	}
	return "", false
}

// readVarintSize reads a pack object header's size varint (git.ts).
func readVarintSize(buf []byte, start int) (size int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("truncated pack")
	}
	size = int(buf[start] & 0x0f)
	shift := uint(4)
	i := start + 1
	for buf[i-1]&0x80 != 0 {
		if i >= len(buf) {
			return 0, 0, errors.New("truncated pack")
		}
		size |= int(buf[i]&0x7f) << shift
		shift += 7
		i++
	}
	return size, i, nil
}

// readOfsDeltaBase reads an OFS-delta base offset (git.ts).
func readOfsDeltaBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("truncated pack")
	}
	b := buf[start]
	offset := int(b & 0x7f)
	i := start + 1
	for b&0x80 != 0 {
		if i >= len(buf) {
			return 0, 0, errors.New("truncated pack")
		}
		b = buf[i]
		offset = ((offset + 1) << 7) | int(b&0x7f)
		i++
	}
	return offset, i, nil
}

// ApplyDelta applies a git delta to base (git.ts 154-188). Copy/insert
// slices are clamped the way Node's Buffer.subarray clamps, so an
// out-of-range copy fails loudly at the final size check ('delta result
// size mismatch') instead of panicking.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		out := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("truncated delta")
			}
			b := delta[pos]
			pos++
			out |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return out, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	var out []byte
	total := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			if op&0x01 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpOff = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpSize = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("truncated delta")
				}
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff < len(base) {
				end := cpOff + cpSize
				if end > len(base) {
					end = len(base)
				}
				out = append(out, base[cpOff:end]...)
			}
			total += cpSize
		} else if op != 0 {
			end := pos + int(op)
			if end > len(delta) {
				end = len(delta)
			}
			out = append(out, delta[pos:end]...)
			total += int(op)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if total != dstSize || len(out) != dstSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

// Adler32 returns the zlib Adler-32 checksum of data (hash/adler32).
func Adler32(data []byte) uint32 {
	return adler32.Checksum(data)
}

// inflateZlib inflates one zlib stream; trailing bytes in b are ignored,
// a truncated stream is an error (mirrors Node's inflateSync).
func inflateZlib(b []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// inflateAt inflates one zlib stream starting at offset and returns its
// bytes plus the offset just past the stream (git.ts 287-330). Fast path:
// inflate the whole tail (up to len-20, never into the pack trailer) and
// find the stream's end by scanning for its 4-byte Adler-32 trailer. On
// a miss, fall back to binary search for the shortest succeeding prefix.
func inflateAt(pack []byte, offset int) (data []byte, next int, err error) {
	limit := len(pack) - 20 // never run into the pack trailer
	if offset < 0 || offset >= limit {
		return nil, 0, errors.New("truncated zlib stream in pack")
	}
	data, err = inflateZlib(pack[offset:limit])
	if err != nil {
		return nil, 0, errors.New("truncated zlib stream in pack")
	}
	trailer := make([]byte, 4)
	binary.BigEndian.PutUint32(trailer, adler32.Checksum(data))
	// The stream is at least a 2-byte zlib header plus a 2-byte final
	// empty block.
	if offset+2 < limit {
		if idx := bytes.Index(pack[offset+2:limit], trailer); idx >= 0 {
			abs := offset + 2 + idx
			if abs+4 <= limit {
				return data, abs + 4, nil
			}
		}
	}
	return inflateAtBinarySearch(pack, offset)
}

// inflateAtBinarySearch finds the shortest prefix of the tail starting at
// offset that inflates successfully (git.ts).
func inflateAtBinarySearch(pack []byte, offset int) (data []byte, next int, err error) {
	limit := len(pack) - 20 // never run into the pack trailer
	data, err = inflateZlib(pack[offset:limit])
	if err != nil {
		return nil, 0, errors.New("truncated zlib stream in pack")
	}
	lo := offset + 1
	hi := limit
	// Invariant: inflateZlib(pack[offset:hi]) succeeds; find the smallest such hi.
	for lo < hi {
		mid := (lo + hi) >> 1
		if _, err := inflateZlib(pack[offset:mid]); err == nil {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	data, err = inflateZlib(pack[offset:lo])
	if err != nil {
		return nil, 0, errors.New("truncated zlib stream in pack")
	}
	return data, lo, nil
}

// ReadPack parses a packfile, resolving OFS/REF deltas. resolveBase looks
// up objects outside the pack (may be nil). Single-pass REF-delta order
// (map §5.2 / N2): a REF-delta base is found only among in-pack objects
// already resolved at top level (oidByOffset, populated in iteration
// order); otherwise it falls back to resolveBase. Resolved objects are
// hashed with HashObject to get their oid (identity verified from bytes).
func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (PackReadResult, error) {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return PackReadResult{}, errors.New("not a packfile")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return PackReadResult{}, fmt.Errorf("pack version %d", version)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	trailer := pack[len(pack)-20:]
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(trailer, sum[:]) {
		return PackReadResult{}, errors.New("pack checksum mismatch")
	}

	type raw struct {
		offset  int
		typ     int
		data    []byte
		baseOff int
		baseOID string
	}
	raws := make([]raw, 0, count)
	byOffset := make(map[int]int, count)
	off := 12
	for i := uint32(0); i < count; i++ {
		if off >= len(pack) {
			return PackReadResult{}, errors.New("truncated pack")
		}
		start := off
		typ := int(pack[off]>>4) & 0x7
		_, next, err := readVarintSize(pack, off)
		if err != nil {
			return PackReadResult{}, err
		}
		off = next
		r := raw{offset: start, typ: typ}
		if typ == 6 {
			baseOff, next2, err := readOfsDeltaBase(pack, off)
			if err != nil {
				return PackReadResult{}, err
			}
			r.baseOff = start - baseOff
			off = next2
		} else if typ == 7 {
			if off+20 > len(pack) {
				return PackReadResult{}, errors.New("truncated pack")
			}
			r.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		}
		data, after, err := inflateAt(pack, off)
		if err != nil {
			return PackReadResult{}, err
		}
		off = after
		r.data = data
		byOffset[start] = len(raws)
		raws = append(raws, r)
	}

	objects := make(map[string]GitObject, count)
	oidByOffset := make(map[int]string, count)
	deltaCount := 0
	var resolve func(idx int, seen map[int]struct{}) (GitObject, error)
	resolve = func(idx int, seen map[int]struct{}) (GitObject, error) {
		r := raws[idx]
		if r.typ >= 1 && r.typ <= 4 {
			typ, _ := codeType(r.typ)
			return GitObject{Type: typ, Data: r.data}, nil
		}
		deltaCount++
		var base GitObject
		if r.typ == 6 {
			baseIdx, ok := byOffset[r.baseOff]
			if !ok {
				return GitObject{}, errors.New("ofs-delta base not in pack")
			}
			if _, dup := seen[r.baseOff]; dup {
				return GitObject{}, errors.New("delta cycle")
			}
			seen[r.baseOff] = struct{}{}
			b, err := resolve(baseIdx, seen)
			if err != nil {
				return GitObject{}, err
			}
			base = b
		} else if r.typ == 7 {
			// Single-pass: linear scan over raws in order; only offsets
			// already resolved at top level (oidByOffset) can match, so
			// a base appearing later in the pack is NOT found in-pack.
			baseIdx := -1
			for j := range raws {
				if oid, ok := oidByOffset[raws[j].offset]; ok && oid == r.baseOID {
					baseIdx = j
					break
				}
			}
			if baseIdx >= 0 {
				if _, dup := seen[raws[baseIdx].offset]; dup {
					return GitObject{}, errors.New("delta cycle")
				}
				seen[raws[baseIdx].offset] = struct{}{}
				b, err := resolve(baseIdx, seen)
				if err != nil {
					return GitObject{}, err
				}
				base = b
			} else if resolveBase != nil {
				b := resolveBase(r.baseOID)
				if b == nil {
					return GitObject{}, fmt.Errorf("ref-delta base %s not found", r.baseOID)
				}
				base = *b
			} else {
				return GitObject{}, fmt.Errorf("ref-delta base %s not found", r.baseOID)
			}
		} else {
			return GitObject{}, fmt.Errorf("unknown pack object type %d", r.typ)
		}
		data, err := ApplyDelta(base.Data, r.data)
		if err != nil {
			return GitObject{}, err
		}
		return GitObject{Type: base.Type, Data: data}, nil
	}
	for idx := range raws {
		obj, err := resolve(idx, map[int]struct{}{raws[idx].offset: {}})
		if err != nil {
			return PackReadResult{}, err
		}
		oid := HashObject(obj.Type, obj.Data)
		oidByOffset[raws[idx].offset] = oid
		objects[oid] = obj
	}
	return PackReadResult{Objects: objects, Entries: int(count), DeltaCount: deltaCount}, nil
}

// WritePack builds a packfile from plain objects (no deltas, git.ts
// 308-335). Deterministic for a given input order. Deflate level is free
// (pack container bytes are not compared, map §9.2); BestSpeed is used.
// Trailer is the SHA-1 of all preceding bytes.
func WritePack(objects []GitObject) ([]byte, error) {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	buf.Write(header)
	for _, obj := range objects {
		code, ok := typeCode(obj.Type)
		if !ok {
			return nil, fmt.Errorf("unknown object type %s", obj.Type)
		}
		remaining := len(obj.Data)
		first := (code << 4) | (remaining & 0x0f)
		remaining = remaining / 16
		var hdr []byte
		for remaining > 0 {
			hdr = append(hdr, byte(first|0x80))
			first = remaining & 0x7f
			remaining = remaining / 128
		}
		hdr = append(hdr, byte(first))
		buf.Write(hdr)
		zw, err := zlib.NewWriterLevel(&buf, flate.BestSpeed)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(obj.Data); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	out = append(out, sum[:]...)
	return out, nil
}

// VerifyPackChecksum reports whether a pack's trailing SHA-1 matches.
func VerifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(pack[len(pack)-20:], sum[:])
}
