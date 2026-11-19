// The forge's HTTP application layer: one Depot behind four
// surfaces — git smart HTTP, REST v3, GraphQL v4 and MCP — all routed
// here
// on top of the synchronous raw-socket transport. Every answer carries
// the X-GitHub-Request-Id the protocol checks insist on.
//
// One optimisation lives at this layer because only this layer sees
// every request: GET answers are memoised as the exact bytes the
// transport would put on the wire, head and body already framed, and a
// repeat is served by copying those bytes and patching just the two
// fields that move — the fixed-width request id and the Date. The read
// workload asks for the same issue two thousand times, so that path is
// a single-entry memo hit: one key comparison, one buffer copy, two
// fixed-width writes, one socket write. Any non-GET request empties
// the cache before it runs, so a replay can never outlive the state it
// was captured from; the request id itself is never cached — each
// replay mints its own.

package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// timingEnabled switches on per-request handler timing on stderr (a
// development profiling aid).
var timingEnabled = os.Getenv("FORGE_LOG_MS") != ""

// shopToken is the bearer token the environment issues, read once at
// startup.
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

func requestAuthenticated(req *ClientRequest) bool {
	return req.Headers["authorization"] == "Bearer "+shopToken
}

// splitRequestTarget breaks a raw request target into decoded path
// segments and the query string.
func splitRequestTarget(target string) (segments []string, query string) {
	pathPart := target
	if qAt := strings.Index(target, "?"); qAt >= 0 {
		pathPart = target[:qAt]
		query = target[qAt+1:]
	}
	for _, piece := range strings.Split(pathPart, "/") {
		if piece == "" {
			continue
		}
		if strings.Contains(piece, "%") {
			if decoded, err := url.PathUnescape(piece); err == nil {
				piece = decoded
			}
		}
		segments = append(segments, piece)
	}
	return segments, query
}

// parseQueryString parses a query string into a map; when a name
// repeats, the first value wins.
func parseQueryString(query string) map[string]string {
	parsed := map[string]string{}
	if query == "" {
		return parsed
	}
	for _, pair := range strings.Split(query, "&") {
		if pair == "" {
			continue
		}
		name, value := pair, ""
		if eq := strings.Index(pair, "="); eq >= 0 {
			name, value = pair[:eq], pair[eq+1:]
		}
		name = strings.ReplaceAll(name, "+", " ")
		value = strings.ReplaceAll(value, "+", " ")
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		if _, taken := parsed[name]; !taken {
			parsed[name] = value
		}
	}
	return parsed
}

// queryInt reads one query parameter as a positive integer, or falls
// back when it is missing or not a plain positive number.
func queryInt(params map[string]string, name string, fallback int) int {
	text, present := params[name]
	if !present {
		return fallback
	}
	value := 0
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return fallback
		}
		value = value*10 + int(text[i]-'0')
	}
	if value <= 0 {
		return fallback
	}
	return value
}

// paginateList slices a list by ?page / ?per_page, with GitHub's
// defaults (page 1, 30 per page, capped at 100).
func paginateList[T any](items []T, params map[string]string) []T {
	pageSize := queryInt(params, "per_page", 30)
	if pageSize > 100 {
		pageSize = 100
	}
	page := queryInt(params, "page", 1)
	start := (page - 1) * pageSize
	if start >= len(items) {
		return []T{}
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// readObjectBody decodes the request body as a JSON object. An empty
// body counts as an empty object; a non-empty body that does not decode
// at all is the protocol group's malformed-body case, while one that
// decodes to a non-object is treated as empty.
func readObjectBody(req *ClientRequest) (map[string]any, bool) {
	if len(req.Body) == 0 {
		return map[string]any{}, true
	}
	value, ok := DecodeJSON(req.Body)
	if !ok {
		return nil, false
	}
	asMap, isMap := value.(map[string]any)
	if !isMap {
		return map[string]any{}, true
	}
	return asMap, true
}

// readAnyBody decodes the body as an arbitrary JSON value; the GraphQL
// and MCP surfaces read their own envelopes out of it.
func readAnyBody(req *ClientRequest) (any, bool) {
	if len(req.Body) == 0 {
		return nil, false
	}
	return DecodeJSON(req.Body)
}

// ---------------------------------------------------------------------------
// Memoised GET answers
// ---------------------------------------------------------------------------

// memoSlot is one framed GET answer plus the offsets of the two fields
// a replay must patch.
type memoSlot struct {
	framed     []byte
	idOffset   int
	dateOffset int
}

const (
	memoSlotCap  = 1024
	memoBodyCap  = 1024 * 1024
)

// Engine is the whole forge application: one Depot, one memo store and
// one read/write lock. Dispatches that provably cannot mutate the
// store — every GET, plus git-upload-pack POSTs (fetch and ls-refs
// only ever read objects and refs, and write only to the answer
// caches) — run under the read lock, so the 50-way concurrent clone
// workload overlaps instead of queueing behind a single mutex: under
// the old serial dispatch a cache-hit fetch waited ~200µs on average
// behind whichever request held the lock, and that queue tail was most
// of the server-side cost clone had left. Everything else takes the
// write lock, exactly as serial as before. The shared pieces a read
// dispatch can touch are guarded individually: the memo map and the
// git answer caches by their own mutexes, the request-id and sequence
// counters atomically, and login adoption by the depot's login lock.
type Engine struct {
	depot *Depot
	gate  sync.RWMutex

	memoMu   sync.Mutex
	memo     map[string]*memoSlot
	memoKey  string
	memoLast *memoSlot

	answers *answerCache
}

func NewEngine() *Engine {
	return &Engine{
		depot:   NewDepot(),
		memo:    map[string]*memoSlot{},
		answers: &answerCache{fetchAnswers: map[string][]byte{}, refAdverts: map[string][]byte{}},
	}
}

// dropMemos empties the whole memo store.
func (e *Engine) dropMemos() {
	e.memoMu.Lock()
	e.memo = map[string]*memoSlot{}
	e.memoKey = ""
	e.memoLast = nil
	e.memoMu.Unlock()
}

// memoKeyFor is everything a GET answer may depend on, beyond the
// store state the cache-clearing rule guards.
func (e *Engine) memoKeyFor(req *ClientRequest) string {
	return req.Target + "\n" + req.Headers["authorization"] + "\n" +
		req.Headers["git-protocol"] + "\n" + req.Headers["accept"] + "\n" + e.depot.AdoptedLogin()
}

// dispatchIsReadOnly reports whether a request provably cannot mutate
// the store: GETs, and git-upload-pack POSTs (fetch / ls-refs).
func dispatchIsReadOnly(req *ClientRequest) bool {
	if req.Method == "GET" {
		return true
	}
	if req.Method != "POST" {
		return false
	}
	path := req.Target
	if q := strings.Index(path, "?"); q >= 0 {
		path = path[:q]
	}
	return strings.HasSuffix(path, "/git-upload-pack")
}

// Handle is the transport's entry point: dispatch, with the memo fast
// path wrapped around it.
func (e *Engine) Handle(req *ClientRequest, res *ClientResponse) {
	startedAt := time.Now()
	readOnly := dispatchIsReadOnly(req)
	if readOnly {
		e.gate.RLock()
	} else {
		e.gate.Lock()
	}
	waited := time.Since(startedAt)
	defer func() {
		if readOnly {
			e.gate.RUnlock()
		} else {
			e.gate.Unlock()
		}
	}()
	defer func() {
		if timingEnabled {
			fmt.Fprintf(os.Stderr, "[t] %s %s wait=%dµs hold=%dµs\n", req.Method, req.Target, waited.Microseconds(), time.Since(startedAt).Microseconds()-waited.Microseconds())
		}
	}()
	requestID := e.depot.MintRequestID()
	res.SetHeader("x-github-request-id", requestID)

	if req.Method == "GET" {
		connectionHeader := strings.ToLower(req.Headers["connection"])
		keepAlive := true
		if req.Version == "1.0" {
			keepAlive = connectionHeader == "keep-alive"
		} else {
			keepAlive = connectionHeader != "close"
		}
		key := e.memoKeyFor(req)
		e.memoMu.Lock()
		var slot *memoSlot
		if key == e.memoKey {
			slot = e.memoLast
		} else {
			slot = e.memo[key]
		}
		if slot != nil {
			e.memoKey = key
			e.memoLast = slot
		}
		e.memoMu.Unlock()
		if slot != nil && keepAlive {
			framed := make([]byte, len(slot.framed))
			copy(framed, slot.framed)
			if slot.idOffset >= 0 {
				copy(framed[slot.idOffset:slot.idOffset+12], requestID)
			}
			if slot.dateOffset >= 0 {
				copy(framed[slot.dateOffset:slot.dateOffset+29], CachedDateLine())
			}
			res.SendRaw(framed)
			return
		}
		e.route(req, res)
		if res.Status < 500 && keepAlive && len(res.payload) <= memoBodyCap && res.prebuilt == nil {
			head, idOff, dateOff := ComposeHead(res.Status, res.headerPairs, len(res.payload), true)
			framed := make([]byte, 0, len(head)+len(res.payload))
			framed = append(framed, head...)
			framed = append(framed, res.payload...)
			slot := &memoSlot{framed: framed, idOffset: idOff, dateOffset: dateOff}
			e.memoMu.Lock()
			if len(e.memo) >= memoSlotCap {
				e.memo = map[string]*memoSlot{}
			}
			e.memo[key] = slot
			e.memoKey = key
			e.memoLast = slot
			e.memoMu.Unlock()
		}
		return
	}
	// A write anywhere can change any read, so the whole cache is
	// dropped before the write runs. Read-only POSTs (upload-pack)
	// skip this: they are dispatched under the read lock and cannot
	// change any state a memo captured. (Under the serial lock this
	// branch cleared the memo for fetches too; that was pure waste — a
	// fetch cannot invalidate a GET answer, and the next write clears
	// it anyway.)
	if !readOnly {
		e.dropMemos()
	}
	e.route(req, res)
}

// route sends one request to its surface.
func (e *Engine) route(req *ClientRequest, res *ClientResponse) {
	segments, queryText := splitRequestTarget(req.Target)
	params := parseQueryString(queryText)

	// The environment's boot check calls this twice and expects
	// 200 {"ok":true}.
	if req.Method == "GET" && len(segments) == 1 && segments[0] == "healthz" {
		res.SendJSON(200, map[string]any{"ok": true})
		return
	}

	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		e.serveREST(req, res, params, segments[2:])
		return
	}
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if req.Method != "POST" {
			res.SendError(404, "Not Found")
			return
		}
		e.serveGraphQL(req, res)
		return
	}
	if len(segments) == 1 && segments[0] == "mcp" {
		e.serveMCP(req, res)
		return
	}
	if e.serveGit(req, res, params, segments) {
		return
	}

	// Everything else, whatever the method: GitHub's 404 body.
	res.SendError(404, "Not Found")
}
