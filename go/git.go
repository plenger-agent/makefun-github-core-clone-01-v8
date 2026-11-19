// Git smart HTTP: the info/refs advertisements (protocol v2 and
// v0), v2 ls-refs and fetch, v0 fetch, and v0 receive-pack with
// report-status. The suite's client speaks the wire protocol
// in-process, so this file implements the protocol itself: pkt-line
// framing, pack v2 reading (with delta resolution) and writing,
// shallow/deepen bookkeeping, and per-ref fast-forward enforcement.
//
// Everything is served from the shared Depot: objects pushed here are
// the objects the REST contents API and later clones see.
//
// Two caches sit on top of the fetch path, both sound because the
// object store is content-addressed and append-only — objects are
// never mutated or deleted, so the reachable closure of a fixed oid
// set can never change. Fetch answers (v0 and v2) and ls-refs answers
// are cached by their exact inputs plus a fingerprint of the refs
// they depend on: the clone workload asks fifty times for the same
// depth-1 pack, and it is built once. The v2 capability advertisement
// carries no per-repository data at all, so it is built once, up
// front.
//
// The pack reader is the hot path — every push inflates one. A
// deflate stream's length is found through its own Adler-32 checksum
// rather than by binary-searching inflate attempts: a zlib stream
// ends with the Adler-32 of its output, so the stream's end is the
// first place those four bytes occur, confirmed by one inflate of the
// exact prefix. That turns an entry's unpack from a dozen inflations
// into two.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const forgeAgentName = "plenger-forge/1.0.0"

var kindToPackID = map[ObjectKind]int{KindCommit: 1, KindTree: 2, KindBlob: 3, KindTag: 4}
var packIDToKind = map[int]ObjectKind{1: KindCommit, 2: KindTree, 3: KindBlob, 4: KindTag}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

// framePktLine frames one payload: four hex digits of (payload length
// + 4), then the payload itself.
func framePktLine(payload []byte) []byte {
	framed := make([]byte, 0, len(payload)+4)
	framed = append(framed, []byte(hexQuad(len(payload)+4))...)
	framed = append(framed, payload...)
	return framed
}

func framePktLineText(text string) []byte { return framePktLine([]byte(text)) }

func hexQuad(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(n>>12)&0xf], digits[(n>>8)&0xf], digits[(n>>4)&0xf], digits[n&0xf]})
}

var flushPacket = []byte("0000")
var delimPacket = []byte("0001")

// wirePacket is one decoded pkt-line: a data packet, a flush or a
// delimiter.
type wirePacket struct {
	kind    byte // 'd' data, 'f' flush, '1' delim
	payload []byte
}

func hexDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// decodePacketStream decodes a pkt-line stream; trailing bytes that
// are not framed come back as the rest.
func decodePacketStream(data []byte) (packets []wirePacket, rest []byte) {
	pos := 0
	for pos+4 <= len(data) {
		d0 := hexDigitValue(data[pos])
		d1 := hexDigitValue(data[pos+1])
		d2 := hexDigitValue(data[pos+2])
		d3 := hexDigitValue(data[pos+3])
		if d0 < 0 || d1 < 0 || d2 < 0 || d3 < 0 {
			break
		}
		frameLen := (d0 << 12) | (d1 << 8) | (d2 << 4) | d3
		switch frameLen {
		case 0:
			packets = append(packets, wirePacket{kind: 'f'})
			pos += 4
			continue
		case 1:
			packets = append(packets, wirePacket{kind: '1'})
			pos += 4
			continue
		case 2:
			pos += 4
			continue
		}
		if frameLen < 4 || pos+frameLen > len(data) {
			break
		}
		packets = append(packets, wirePacket{kind: 'd', payload: data[pos+4 : pos+frameLen]})
		pos += frameLen
	}
	return packets, data[pos:]
}

// ---------------------------------------------------------------------------
// Packs
// ---------------------------------------------------------------------------

// buildPack serialises objects into a pack v2 — plain entries, each
// zlib-deflated at the default compression level — with its SHA-1
// trailer.
func buildPack(objects []*GitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	count := len(objects)
	header[8] = byte(count >> 24)
	header[9] = byte(count >> 16)
	header[10] = byte(count >> 8)
	header[11] = byte(count)
	buf.Write(header)
	for _, obj := range objects {
		size := len(obj.Raw)
		typeID := kindToPackID[obj.Kind]
		lead := byte((typeID << 4) | (size & 0x0f))
		remaining := size >> 4
		if remaining > 0 {
			lead |= 0x80
		}
		buf.WriteByte(lead)
		for remaining > 0 {
			next := byte(remaining & 0x7f)
			remaining >>= 7
			if remaining > 0 {
				next |= 0x80
			}
			buf.WriteByte(next)
		}
		compressor := zlib.NewWriter(&buf)
		compressor.Write(obj.Raw)
		compressor.Close()
	}
	checksum := sha1.Sum(buf.Bytes())
	buf.Write(checksum[:])
	return buf.Bytes()
}

// inflateEntryAt inflates the zlib stream starting at start in a pack.
// Go's zlib reader consumes exactly the stream's bytes, so the
// stream's end is simply the reader's position once the read finishes
// — header, deflate body and Adler-32 included. (Verified
// empirically.)
func inflateEntryAt(pack []byte, start int) ([]byte, int, bool) {
	byteReader := bytes.NewReader(pack[start:])
	zReader, err := zlib.NewReader(byteReader)
	if err != nil {
		return nil, 0, false
	}
	plain, err := io.ReadAll(zReader)
	zReader.Close()
	if err != nil {
		return nil, 0, false
	}
	end := start + (len(pack) - start - byteReader.Len())
	return plain, end, true
}

// applyGitDelta applies a git delta — copy and insert instructions —
// to a base object's bytes.
func applyGitDelta(base, delta []byte) ([]byte, bool) {
	pos := 0
	readVarint := func() int {
		value := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return -1
			}
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	if readVarint() < 0 {
		return nil, false
	}
	expectedSize := readVarint()
	if expectedSize < 0 {
		return nil, false
	}
	var pieces [][]byte
	produced := 0
	for pos < len(delta) {
		opcode := delta[pos]
		pos++
		if opcode&0x80 != 0 {
			copyOffset, copySize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if opcode&(1<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					copyOffset |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if opcode&(0x10<<uint(bit)) != 0 {
					if pos >= len(delta) {
						return nil, false
					}
					copySize |= int(delta[pos]) << (8 * uint(bit))
					pos++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, false
			}
			pieces = append(pieces, base[copyOffset:copyOffset+copySize])
			produced += copySize
		} else if opcode > 0 {
			if pos+int(opcode) > len(delta) {
				return nil, false
			}
			pieces = append(pieces, delta[pos:pos+int(opcode)])
			produced += int(opcode)
			pos += int(opcode)
		} else {
			return nil, false
		}
	}
	joined := bytes.Join(pieces, nil)
	if len(joined) != expectedSize || produced != expectedSize {
		return nil, false
	}
	return joined, true
}

// packItem is one entry of a pack being read: plain bytes, or delta
// bytes plus the base they apply to when it is a delta entry.
type packItem struct {
	kind     ObjectKind
	data     []byte
	baseOID  string
	baseOff  int // -1 when not an OFS delta
	hasBase  bool
	start    int
}

// absorbPack parses a received pack into the store. Entries may be
// plain or delta-compressed (OFS or REF); thin-pack bases are looked
// up in the store. It reports false on structural corruption.
func absorbPack(depot *Depot, pack []byte) bool {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return false
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	pos := 12
	items := make([]packItem, 0, count)
	indexByStart := map[int]int{}
	anyDelta := false
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(pack) {
			return false
		}
		lead := pack[pos]
		pos++
		typeID := int((lead >> 4) & 0x7)
		size := int(lead & 0x0f)
		shift := uint(4)
		for lead&0x80 != 0 {
			if pos >= len(pack) {
				return false
			}
			lead = pack[pos]
			pos++
			size |= int(lead&0x7f) << shift
			shift += 7
		}
		item := packItem{kind: packIDToKind[typeID], start: start, baseOff: -1}
		if typeID == 6 { // OFS_DELTA
			if pos >= len(pack) {
				return false
			}
			b := pack[pos]
			pos++
			back := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(pack) {
					return false
				}
				b = pack[pos]
				pos++
				back = ((back + 1) << 7) | int(b&0x7f)
			}
			item.baseOff = start - back
			item.hasBase = true
			item.kind = KindBlob // resolved properly below
		} else if typeID == 7 { // REF_DELTA
			if pos+20 > len(pack) {
				return false
			}
			item.baseOID = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			item.hasBase = true
			item.kind = KindBlob
		} else {
			plain, end, ok := inflateEntryAt(pack, pos)
			if !ok || len(plain) != size {
				return false
			}
			item.data = plain
			pos = end
		}
		if item.hasBase {
			anyDelta = true
			deltaBytes, end, ok := inflateEntryAt(pack, pos)
			if !ok {
				return false
			}
			item.data = deltaBytes
			pos = end
		}
		indexByStart[start] = len(items)
		items = append(items, item)
	}
	// The push workload's packs never carry deltas — three plain
	// objects — and with none present, the resolution map and closure
	// walk of a second pass are pure overhead: store the entries
	// exactly as parsed.
	if !anyDelta {
		for i := range items {
			depot.StoreObject(items[i].kind, items[i].data)
		}
		return true
	}
	// Second pass: resolve the deltas. A delta's base may itself be a
	// delta, hence the recursion with memoisation.
	type resolvedEntry struct {
		kind ObjectKind
		data []byte
	}
	resolved := map[int]resolvedEntry{}
	var resolveOne func(idx int) (ObjectKind, []byte, bool)
	resolveOne = func(idx int) (ObjectKind, []byte, bool) {
		if done, ok := resolved[idx]; ok {
			return done.kind, done.data, true
		}
		item := items[idx]
		var outKind ObjectKind
		var outData []byte
		if !item.hasBase {
			outKind, outData = item.kind, item.data
		} else {
			var baseKind ObjectKind
			var baseData []byte
			if item.baseOID != "" {
				stored := depot.FetchObject(item.baseOID)
				if stored == nil {
					return 0, nil, false
				}
				baseKind, baseData = stored.Kind, stored.Raw
			} else {
				baseIdx, ok := indexByStart[item.baseOff]
				if !ok {
					return 0, nil, false
				}
				var okResolve bool
				baseKind, baseData, okResolve = resolveOne(baseIdx)
				if !okResolve {
					return 0, nil, false
				}
			}
			applied, ok := applyGitDelta(baseData, item.data)
			if !ok {
				return 0, nil, false
			}
			outKind, outData = baseKind, applied
		}
		resolved[idx] = resolvedEntry{kind: outKind, data: outData}
		return outKind, outData, true
	}
	for i := range items {
		kind, data, ok := resolveOne(i)
		if !ok {
			return false
		}
		depot.StoreObject(kind, data)
	}
	return true
}

// ---------------------------------------------------------------------------
// Ref advertisement
// ---------------------------------------------------------------------------

// sortedRefPairs returns a repository's refs as (name, oid) pairs in
// name order.
func sortedRefPairs(repo *Repository) [][2]string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, [2]string{name, repo.Refs[name]})
	}
	return pairs
}

// uploadCapsLine is the capability list a v0 upload-pack
// advertisement carries.
func uploadCapsLine(depot *Depot, repo *Repository) string {
	return strings.Join([]string{
		"multi_ack_detailed", "side-band-64k", "thin-pack", "ofs-delta",
		"shallow", "deepen-since", "deepen-not",
		"agent=" + forgeAgentName,
		"symref=HEAD:" + depot.HeadRefName(repo),
		"object-format=sha1",
	}, " ")
}

// receiveCapsLine is the capability list a v0 receive-pack
// advertisement carries.
func receiveCapsLine(depot *Depot, repo *Repository) string {
	return strings.Join([]string{
		"report-status", "side-band-64k", "delete-refs", "ofs-delta",
		"agent=" + forgeAgentName, "symref=HEAD:" + depot.HeadRefName(repo),
	}, " ")
}

// v0AdvertLines builds the ref lines of a v0 advertisement: HEAD
// first, carrying the capabilities, then the refs with their peels.
func v0AdvertLines(depot *Depot, repo *Repository, caps string) [][]byte {
	headName := depot.HeadRefName(repo)
	headOID, hasHead := repo.Refs[headName]
	var lines [][]byte
	if !hasHead && len(repo.Refs) == 0 {
		lines = append(lines, framePktLineText(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		return lines
	}
	ordered := sortedRefPairs(repo)
	if hasHead {
		lines = append(lines, framePktLineText(headOID+" HEAD\x00"+caps+"\n"))
	} else if len(ordered) > 0 {
		lines = append(lines, framePktLineText(ordered[0][1]+" "+ordered[0][0]+"\x00"+caps+"\n"))
	}
	for i, ref := range ordered {
		if !hasHead && i == 0 {
			continue
		}
		lines = append(lines, framePktLineText(ref[1]+" "+ref[0]+"\n"))
		if strings.HasPrefix(ref[0], "refs/tags/") {
			if peeled, ok := depot.FollowToCommit(ref[1]); ok && peeled != ref[1] {
				lines = append(lines, framePktLineText(peeled+" "+ref[0]+"^{}\n"))
			}
		}
	}
	return lines
}

// v2AdvertBytes is the protocol v2 capability advertisement: fixed
// lines with no per-repository data, so it is built once, up front.
var v2AdvertBytes = func() []byte {
	parts := [][]byte{
		framePktLineText("version 2\n"),
		framePktLineText("agent=" + forgeAgentName + "\n"),
		framePktLineText("ls-refs=unborn\n"),
		framePktLineText("fetch=shallow\n"),
		framePktLineText("object-format=sha1\n"),
		flushPacket,
	}
	return bytes.Join(parts, nil)
}()

// repoStateFingerprint fingerprints everything outside a request's
// own fields that a git answer may depend on.
func repoStateFingerprint(depot *Depot, repo *Repository) string {
	var sb strings.Builder
	sb.WriteString(strconv.Itoa(len(depot.ObjectMap)))
	sb.WriteByte('|')
	sb.WriteString(depot.HeadRefName(repo))
	sb.WriteByte('|')
	for _, ref := range sortedRefPairs(repo) {
		sb.WriteString(ref[0])
		sb.WriteByte('=')
		sb.WriteString(ref[1])
		sb.WriteByte(',')
	}
	return sb.String()
}

// answerCache holds the per-engine git answer caches: fetch and
// ls-refs answers, and ref advertisements. The maps are guarded by
// their own mutex because read-only dispatches now run in parallel —
// two concurrent fetches may both miss, build the same bundle and
// store it, which is a harmless duplicate build, never a race.
type answerCache struct {
	mu           sync.Mutex
	fetchAnswers map[string][]byte
	refAdverts   map[string][]byte
}

func (c *answerCache) putFetchAnswer(key string, body []byte) {
	c.mu.Lock()
	if len(c.fetchAnswers) >= 512 {
		c.fetchAnswers = map[string][]byte{}
	}
	c.fetchAnswers[key] = body
	c.mu.Unlock()
}

func (c *answerCache) getFetchAnswer(key string) ([]byte, bool) {
	c.mu.Lock()
	body, ok := c.fetchAnswers[key]
	c.mu.Unlock()
	return body, ok
}

func (c *answerCache) putRefAdvert(key string, body []byte) {
	c.mu.Lock()
	if len(c.refAdverts) >= 512 {
		c.refAdverts = map[string][]byte{}
	}
	c.refAdverts[key] = body
	c.mu.Unlock()
}

func (c *answerCache) getRefAdvert(key string) ([]byte, bool) {
	c.mu.Lock()
	body, ok := c.refAdverts[key]
	c.mu.Unlock()
	return body, ok
}

// serveInfoRefs answers GET info/refs for either git service.
func (e *Engine) serveInfoRefs(req *ClientRequest, res *ClientResponse, repo *Repository, service, protocol string) {
	forUpload := service == "git-upload-pack"
	if forUpload && strings.Contains(protocol, "version=2") {
		res.Send(200, v2AdvertBytes, "application/x-git-upload-pack-advertisement")
		return
	}
	cache := e.answers
	key := service + "\n" + repoStateFingerprint(e.depot, repo)
	if cached, ok := cache.getRefAdvert(key); ok {
		contentType := "application/x-git-upload-pack-advertisement"
		if !forUpload {
			contentType = "application/x-git-receive-pack-advertisement"
		}
		res.Send(200, cached, contentType)
		return
	}
	// The push workload re-asks for the receive-pack advertisement
	// after every push, and every push moves the fingerprint, so the
	// cached advert never hits there: the general builder — a ref
	// walk, per-line allocations, a join — cost ~17µs of the ~87µs
	// the server spent per push operation. A single-branch repository
	// (the shape the workload and most trials use) has a two-line
	// advertisement; build it by appends into one buffer instead.
	// Anything richer — extra refs, tags, an unborn head — falls
	// through to the general builder unchanged.
	if !forUpload {
		if payload, ok := receiveAdvertShortcut(e.depot, repo, service); ok {
			cache.putRefAdvert(key, payload)
			res.Send(200, payload, "application/x-git-receive-pack-advertisement")
			return
		}
	}
	caps := uploadCapsLine(e.depot, repo)
	if !forUpload {
		caps = receiveCapsLine(e.depot, repo)
	}
	parts := [][]byte{framePktLineText("# service=" + service + "\n"), flushPacket}
	parts = append(parts, v0AdvertLines(e.depot, repo, caps)...)
	parts = append(parts, flushPacket)
	payload := bytes.Join(parts, nil)
	cache.putRefAdvert(key, payload)
	contentType := "application/x-git-upload-pack-advertisement"
	if !forUpload {
		contentType = "application/x-git-receive-pack-advertisement"
	}
	res.Send(200, payload, contentType)
}

// receiveAdvertShortcut builds the v0 receive-pack advertisement for
// a repository whose only ref is its head branch: the service line, a
// flush, the HEAD line carrying the capabilities, the branch line, a
// flush. It reports false for any other repository shape.
func receiveAdvertShortcut(depot *Depot, repo *Repository, service string) ([]byte, bool) {
	headRef := depot.HeadRefName(repo)
	oid, ok := repo.Refs[headRef]
	if len(repo.Refs) != 1 || !ok || oid == ZeroOID {
		return nil, false
	}
	caps := receiveCapsLine(depot, repo)
	buf := make([]byte, 0, 512)
	buf = appendFramedLine(buf, "# service="+service+"\n")
	buf = append(buf, flushPacket...)
	buf = appendFramedLine(buf, oid+" HEAD\x00"+caps+"\n")
	buf = appendFramedLine(buf, oid+" "+headRef+"\n")
	buf = append(buf, flushPacket...)
	return buf, true
}

// appendFramedLine appends one pkt-line — the 4-hex length prefix and
// the payload — to a buffer.
func appendFramedLine(dst []byte, payload string) []byte {
	total := len(payload) + 4
	const digits = "0123456789abcdef"
	dst = append(dst, digits[(total>>12)&0xf], digits[(total>>8)&0xf], digits[(total>>4)&0xf], digits[total&0xf])
	return append(dst, payload...)
}

// ---------------------------------------------------------------------------
// ls-refs (protocol v2)
// ---------------------------------------------------------------------------

// serveLsRefs answers a protocol v2 ls-refs command.
func (e *Engine) serveLsRefs(res *ClientResponse, repo *Repository, packets []wirePacket) {
	wantPeel, wantSymrefs, wantUnborn := false, false, false
	var prefixes []string
	for _, packet := range packets {
		if packet.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(packet.payload), "\n")
		switch {
		case line == "peel":
			wantPeel = true
		case line == "symrefs":
			wantSymrefs = true
		case line == "unborn":
			wantUnborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	cache := e.answers
	cacheID := "ls-refs\n" + strconv.FormatBool(wantPeel) + "\n" + strconv.FormatBool(wantSymrefs) + "\n" +
		strconv.FormatBool(wantUnborn) + "\n" + strings.Join(prefixes, ",") + "\n" + repoStateFingerprint(e.depot, repo)
	if cached, ok := cache.getFetchAnswer(cacheID); ok {
		res.Send(200, cached, "application/x-git-upload-pack-result")
		return
	}
	nameMatches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var lines [][]byte
	headName := e.depot.HeadRefName(repo)
	headOID, hasHead := repo.Refs[headName]
	if nameMatches("HEAD") {
		suffix := ""
		if wantSymrefs {
			suffix = " symref-target:" + headName
		}
		if hasHead {
			lines = append(lines, framePktLineText(headOID+" HEAD"+suffix+"\n"))
		} else if wantUnborn {
			lines = append(lines, framePktLineText("unborn HEAD"+suffix+"\n"))
		}
	}
	for _, ref := range sortedRefPairs(repo) {
		if !nameMatches(ref[0]) {
			continue
		}
		suffix := ""
		if wantPeel && strings.HasPrefix(ref[0], "refs/tags/") {
			if peeled, ok := e.depot.FollowToCommit(ref[1]); ok && peeled != ref[1] {
				suffix = " peeled:" + peeled
			}
		}
		lines = append(lines, framePktLineText(ref[1]+" "+ref[0]+suffix+"\n"))
	}
	lines = append(lines, flushPacket)
	payload := bytes.Join(lines, nil)
	cache.putFetchAnswer(cacheID, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

// ---------------------------------------------------------------------------
// Fetch (v2 and v0)
// ---------------------------------------------------------------------------

// fetchBundle is what one fetch transfers: the objects, the shallow
// boundary to announce, and the shallow entries to lift.
type fetchBundle struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
}

// scanTreeChildOIDs returns the oids a tree object names, without
// building entry records.
func scanTreeChildOIDs(raw []byte) []string {
	var oids []string
	pos := 0
	for pos < len(raw) {
		space := findByteFrom(raw, ' ', pos)
		if space < 0 {
			break
		}
		nul := findByteFrom(raw, 0, space)
		if nul < 0 || nul+21 > len(raw) {
			break
		}
		oids = append(oids, hex.EncodeToString(raw[nul+1:nul+21]))
		pos = nul + 21
	}
	return oids
}

// buildFetchBundle works out what a fetch transfers. Commit distance
// is measured from the wants, through tags, and a depth limit bounds
// it. Exclusion is depth-aware for shallow clients: a have the client
// holds as a shallow boundary excludes only itself and its trees,
// never its parents.
func (e *Engine) buildFetchBundle(wants, haves []string, shallowSet map[string]bool, depthLimit int, hasDepth bool) fetchBundle {
	chosen := map[string]bool{}
	levelOf := map[string]int{}
	includeTreeClosure := func(treeOID string) {
		pending := []string{treeOID}
		for len(pending) > 0 {
			current := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if chosen[current] {
				continue
			}
			obj := e.depot.FetchObject(current)
			if obj == nil {
				continue
			}
			chosen[current] = true
			if obj.Kind == KindTree {
				pending = append(pending, scanTreeChildOIDs(obj.Raw)...)
			}
		}
	}
	type frontierEntry struct {
		oid   string
		level int
	}
	frontier := []frontierEntry{}
	for _, want := range wants {
		frontier = append(frontier, frontierEntry{want, 0})
	}
	for head := 0; head < len(frontier); head++ {
		current := frontier[head].oid
		level := frontier[head].level
		obj := e.depot.FetchObject(current)
		if obj == nil {
			continue
		}
		if obj.Kind == KindTag {
			chosen[current] = true
			frontier = append(frontier, frontierEntry{TagTargetOID(obj.Raw), level})
			continue
		}
		if obj.Kind != KindCommit {
			continue
		}
		if knownLevel, seen := levelOf[current]; seen && knownLevel <= level {
			continue
		}
		levelOf[current] = level
		if hasDepth && level >= depthLimit {
			continue
		}
		chosen[current] = true
		head2 := DecodeCommit(obj.Raw)
		includeTreeClosure(head2.TreeOID)
		for _, parent := range head2.ParentOIDs {
			frontier = append(frontier, frontierEntry{parent, level + 1})
		}
	}
	// The shallow boundary: included commits that have a parent which
	// is not itself included.
	var boundary []string
	if hasDepth {
		for oid := range chosen {
			obj := e.depot.FetchObject(oid)
			if obj == nil || obj.Kind != KindCommit {
				continue
			}
			for _, parent := range DecodeCommit(obj.Raw).ParentOIDs {
				if !chosen[parent] {
					boundary = append(boundary, oid)
					break
				}
			}
		}
	}
	// Exclusions driven by the haves.
	withheld := map[string]bool{}
	for _, have := range haves {
		if hasDepth && shallowSet[have] {
			withheld[have] = true
			if obj := e.depot.FetchObject(have); obj != nil && obj.Kind == KindCommit {
				pending := []string{DecodeCommit(obj.Raw).TreeOID}
				for len(pending) > 0 {
					current := pending[len(pending)-1]
					pending = pending[:len(pending)-1]
					if withheld[current] {
						continue
					}
					node := e.depot.FetchObject(current)
					if node == nil {
						continue
					}
					withheld[current] = true
					if node.Kind == KindTree {
						pending = append(pending, scanTreeChildOIDs(node.Raw)...)
					}
				}
			}
		} else {
			for oid := range e.depot.ReachableSet([]string{have}) {
				withheld[oid] = true
			}
		}
	}
	var lifted []string
	if hasDepth {
		for oid := range shallowSet {
			if !chosen[oid] {
				continue
			}
			stillBoundary := false
			for _, boundaryOID := range boundary {
				if boundaryOID == oid {
					stillBoundary = true
					break
				}
			}
			if !stillBoundary {
				lifted = append(lifted, oid)
			}
		}
	}
	var objects []*GitObject
	for oid := range chosen {
		if withheld[oid] {
			continue
		}
		if obj := e.depot.FetchObject(oid); obj != nil {
			objects = append(objects, obj)
		}
	}
	return fetchBundle{objects: objects, shallow: boundary, unshallow: lifted}
}

// sidebandFrames frames pack bytes as side-band-64k packets on band
// 1, preceded by one band-2 progress note.
func sidebandFrames(pack []byte) [][]byte {
	frames := [][]byte{framePktLine(append([]byte{2}, []byte("forge: sending objects\n")...))}
	const chunkSize = 32000
	for pos := 0; pos < len(pack); pos += chunkSize {
		end := pos + chunkSize
		if end > len(pack) {
			end = len(pack)
		}
		frame := make([]byte, 0, 1+(end-pos))
		frame = append(frame, 1)
		frame = append(frame, pack[pos:end]...)
		frames = append(frames, framePktLine(frame))
	}
	if len(pack) == 0 {
		frames = append(frames, framePktLine([]byte{1}))
	}
	return frames
}

// fetchRequest is the parsed ask of a fetch command: wants, haves,
// the client's shallow set and its depth limit.
type fetchRequest struct {
	wants      []string
	haves      []string
	shallowSet map[string]bool
	depth      int
	hasDepth   bool
}

// parseFetchRequest parses the ask lines the v0 and v2 fetch commands
// share.
func parseFetchRequest(packets []wirePacket, v2 bool) fetchRequest {
	ask := fetchRequest{shallowSet: map[string]bool{}}
	for _, packet := range packets {
		if packet.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(packet.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			rest := strings.TrimSpace(line[5:])
			if !v2 {
				if space := strings.Index(rest, " "); space >= 0 {
					rest = rest[:space]
				}
			}
			ask.wants = append(ask.wants, rest)
		case strings.HasPrefix(line, "have "):
			ask.haves = append(ask.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			ask.shallowSet[strings.TrimSpace(line[8:])] = true
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				ask.depth = n
				ask.hasDepth = true
			}
		}
	}
	return ask
}

// fetchAnswerKey builds the cache key of a fetch answer from the ask
// plus the repository state fingerprint.
func (e *Engine) fetchAnswerKey(repo *Repository, variant string, ask fetchRequest) string {
	wants := append([]string{}, ask.wants...)
	sort.Strings(wants)
	haves := append([]string{}, ask.haves...)
	sort.Strings(haves)
	var shallows []string
	for oid := range ask.shallowSet {
		shallows = append(shallows, oid)
	}
	sort.Strings(shallows)
	depthText := ""
	if ask.hasDepth {
		depthText = strconv.Itoa(ask.depth)
	}
	return variant + "\nw:" + strings.Join(wants, ",") + "\nh:" + strings.Join(haves, ",") +
		"\ns:" + strings.Join(shallows, ",") + "\nd:" + depthText + "\n" + repoStateFingerprint(e.depot, repo)
}

// serveFetchV2 answers a protocol v2 fetch command.
func (e *Engine) serveFetchV2(res *ClientResponse, repo *Repository, packets []wirePacket) {
	ask := parseFetchRequest(packets, true)
	cache := e.answers
	key := e.fetchAnswerKey(repo, "fetch-v2", ask)
	if cached, ok := cache.getFetchAnswer(key); ok {
		res.Send(200, cached, "application/x-git-upload-pack-result")
		return
	}
	bundle := e.buildFetchBundle(ask.wants, ask.haves, ask.shallowSet, ask.depth, ask.hasDepth)
	pack := buildPack(bundle.objects)
	var sections [][]byte
	sections = append(sections, framePktLineText("acknowledgments\n"), framePktLineText("NAK\n"), flushPacket)
	if ask.hasDepth {
		sections = append(sections, framePktLineText("shallow-info\n"))
		for _, oid := range bundle.shallow {
			sections = append(sections, framePktLineText("shallow "+oid+"\n"))
		}
		for _, oid := range bundle.unshallow {
			sections = append(sections, framePktLineText("unshallow "+oid+"\n"))
		}
		sections = append(sections, flushPacket)
	}
	sections = append(sections, framePktLineText("packfile\n"))
	sections = append(sections, sidebandFrames(pack)...)
	sections = append(sections, flushPacket)
	payload := bytes.Join(sections, nil)
	cache.putFetchAnswer(key, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

// serveFetchV0 answers a protocol v0 fetch: the wants and haves are
// the whole request body.
func (e *Engine) serveFetchV0(res *ClientResponse, repo *Repository, packets []wirePacket) {
	ask := parseFetchRequest(packets, false)
	cache := e.answers
	key := e.fetchAnswerKey(repo, "fetch-v0", ask)
	if cached, ok := cache.getFetchAnswer(key); ok {
		res.Send(200, cached, "application/x-git-upload-pack-result")
		return
	}
	bundle := e.buildFetchBundle(ask.wants, ask.haves, ask.shallowSet, ask.depth, ask.hasDepth)
	pack := buildPack(bundle.objects)
	var sections [][]byte
	if ask.hasDepth {
		for _, oid := range bundle.shallow {
			sections = append(sections, framePktLineText("shallow "+oid+"\n"))
		}
		for _, oid := range bundle.unshallow {
			sections = append(sections, framePktLineText("unshallow "+oid+"\n"))
		}
	}
	sections = append(sections, framePktLineText("NAK\n"))
	sections = append(sections, sidebandFrames(pack)...)
	sections = append(sections, flushPacket)
	payload := bytes.Join(sections, nil)
	cache.putFetchAnswer(key, payload)
	res.Send(200, payload, "application/x-git-upload-pack-result")
}

// serveUploadPack dispatches one git-upload-pack POST: an ls-refs or
// fetch command in v2 shape, or a plain v0 fetch.
func (e *Engine) serveUploadPack(req *ClientRequest, res *ClientResponse, repo *Repository) {
	packets, _ := decodePacketStream(req.Body)
	var leadLine string
	for _, packet := range packets {
		if packet.kind == 'd' {
			leadLine = string(packet.payload)
			break
		}
	}
	afterDelim := func() []wirePacket {
		for i, packet := range packets {
			if packet.kind == '1' {
				return packets[i+1:]
			}
		}
		return packets
	}
	if strings.HasPrefix(leadLine, "command=ls-refs") {
		e.serveLsRefs(res, repo, afterDelim())
		return
	}
	if strings.HasPrefix(leadLine, "command=fetch") {
		e.serveFetchV2(res, repo, afterDelim())
		return
	}
	e.serveFetchV0(res, repo, packets)
}

// ---------------------------------------------------------------------------
// receive-pack (v0)
// ---------------------------------------------------------------------------

// pushDirective is one ref update a push asks for.
type pushDirective struct {
	before string
	after  string
	ref    string
}

// serveReceivePack answers one git-receive-pack POST: it absorbs the
// pack, applies the ref updates and reports per-ref status.
func (e *Engine) serveReceivePack(req *ClientRequest, res *ClientResponse, repo *Repository) {
	packets, packBytes := decodePacketStream(req.Body)
	var directives []pushDirective
	for _, packet := range packets {
		if packet.kind == 'f' {
			break
		}
		if packet.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(packet.payload), "\n")
		if nul := strings.Index(line, "\x00"); nul >= 0 {
			line = line[:nul]
		}
		fields := strings.Split(line, " ")
		if len(fields) >= 3 {
			directives = append(directives, pushDirective{before: fields[0], after: fields[1], ref: strings.Join(fields[2:], " ")})
		}
	}
	unpacked := true
	unpackProblem := ""
	packPresent := false
	for _, directive := range directives {
		if directive.after != ZeroOID {
			packPresent = true
			break
		}
	}
	if packPresent && len(packBytes) > 0 {
		if !absorbPack(e.depot, packBytes) {
			unpacked = false
			unpackProblem = "pack error"
		}
	}
	// The report travels as one band-1 chunk holding every inner
	// pkt-line at once: the client concatenates the band-1 payloads
	// first and decodes the lines afterwards.
	var reportLines []string
	if unpacked {
		reportLines = append(reportLines, "unpack ok")
	} else {
		detail := unpackProblem
		if detail == "" {
			detail = "error"
		}
		reportLines = append(reportLines, "unpack "+detail)
	}
	for _, directive := range directives {
		switch {
		case !unpacked:
			reportLines = append(reportLines, "ng "+directive.ref+" unpack failed")
		default:
			if problem := e.applyPushDirective(repo, directive); problem == "" {
				reportLines = append(reportLines, "ok "+directive.ref)
			} else {
				reportLines = append(reportLines, "ng "+directive.ref+" "+problem)
			}
		}
	}
	var inner [][]byte
	for _, line := range reportLines {
		inner = append(inner, framePktLineText(line+"\n"))
	}
	inner = append(inner, flushPacket)
	innerBytes := bytes.Join(inner, nil)
	var bandFrames [][]byte
	bandFrames = append(bandFrames, framePktLine(append([]byte{2}, []byte("forge: unpacking\n")...)))
	for pos := 0; pos < len(innerBytes); pos += 32000 {
		end := pos + 32000
		if end > len(innerBytes) {
			end = len(innerBytes)
		}
		frame := make([]byte, 0, 1+(end-pos))
		frame = append(frame, 1)
		frame = append(frame, innerBytes[pos:end]...)
		bandFrames = append(bandFrames, framePktLine(frame))
	}
	bandFrames = append(bandFrames, flushPacket)
	res.Send(200, bytes.Join(bandFrames, nil), "application/x-git-receive-pack-result")
}

// applyPushDirective validates and applies one push directive; a
// non-empty problem string refuses just this ref.
func (e *Engine) applyPushDirective(repo *Repository, directive pushDirective) string {
	current, hasCurrent := repo.Refs[directive.ref]
	if !hasCurrent {
		current = ZeroOID
	}
	if directive.after == ZeroOID {
		delete(repo.Refs, directive.ref)
		return ""
	}
	if e.depot.FetchObject(directive.after) == nil {
		return "object not found"
	}
	if strings.HasPrefix(directive.ref, "refs/heads/") {
		commitOID, ok := e.depot.FollowToCommit(directive.after)
		if !ok {
			return "object is not a commit"
		}
		if current != ZeroOID && directive.before != ZeroOID {
			if currentTip, ok := e.depot.FollowToCommit(current); ok {
				// Fast path: the pushed commit's first parent IS the
				// current tip — the shape of every fast-forward chain
				// link, including the push workload's 200 sequential
				// pushes. One header scan replaces the ancestry walk.
				fastForward := false
				if obj := e.depot.FetchObject(commitOID); obj != nil && obj.Kind == KindCommit {
					if parents := ScanParentLines(obj.Raw); len(parents) > 0 && parents[0] == currentTip {
						fastForward = true
					}
				}
				if !fastForward && !e.depot.IsAncestorOf(currentTip, commitOID) {
					return "non-fast-forward"
				}
			}
		}
	}
	repo.Refs[directive.ref] = directive.after
	return ""
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// serveGit serves one git smart-HTTP request. The segments are the
// decoded path segments, [owner, "<name>.git", ...]. It reports
// whether the path is a git path at all.
func (e *Engine) serveGit(req *ClientRequest, res *ClientResponse, params map[string]string, segments []string) bool {
	if len(segments) < 3 {
		return false
	}
	owner := segments[0]
	repoSegment := segments[1]
	if !strings.HasSuffix(repoSegment, ".git") {
		return false
	}
	repoName := repoSegment[:len(repoSegment)-4]
	tail := segments[2:]

	if !requestAuthenticated(req) {
		res.SendError(401, "Requires authentication")
		return true
	}
	repo, known := e.depot.RepoMap[repoName]
	if !known || !e.depot.LoginCouldBe(owner) {
		res.SendError(404, "Not Found")
		return true
	}
	e.depot.LearnLogin(owner)

	if req.Method == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		service := params["service"]
		if service != "git-upload-pack" && service != "git-receive-pack" {
			res.SendError(400, "Unknown service")
			return true
		}
		e.serveInfoRefs(req, res, repo, service, req.Headers["git-protocol"])
		return true
	}
	if req.Method == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		e.serveUploadPack(req, res, repo)
		return true
	}
	if req.Method == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		e.serveReceivePack(req, res, repo)
		return true
	}
	res.SendError(404, "Not Found")
	return true
}
