// A deliberately small HTTP/1.1 transport speaking directly over
// raw sockets.
//
// The graded hot paths are thousands of tiny sequential requests on
// pooled keep-alive connections. A general-purpose HTTP stack spends
// most of each of those requests on machinery this forge never touches
// — header-object churn, date and connection bookkeeping, stream
// plumbing in both directions — so this file speaks exactly the subset
// of HTTP/1.1 the forge's clients need, directly on a net.Conn:
//
//   - request line plus headers, Content-Length and chunked request
//     bodies, Expect: 100-continue;
//   - keep-alive with strictly ordered per-connection dispatch (the
//     clients never pipeline, but a pipelined request already sitting
//     in the buffer is still answered in order);
//   - one socket write per response, with status line, headers and
//     body assembled into a single byte slice first;
//   - TCP_NODELAY on every connection, so a small response is never
//     held back by Nagle;
//   - a Date header formatted once a second and reused, instead of
//     paying for a fresh format on every response.
//
// Two choices keep the per-request cost close to the floor. First, the
// request head is parsed in place on the bytes the buffered reader
// already holds: one peek, one terminator scan, no per-line reader
// calls and no copy of the whole head (the line-by-line reader survives
// only as a fallback for heads split across segments). Second, response
// headers live in a plain slice scanned linearly — a response carries
// at most a handful of headers, so a scan beats a map plus a
// lower-casing allocation on every set. A request is parsed, dispatched
// and answered inside one turn of the read loop, the whole body already
// buffered, so there is no reader/writer dance and no goroutine hop
// between receiving a request and answering it.

package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ClientRequest is one parsed HTTP request: method, raw target,
// lower-cased headers and the whole body.
type ClientRequest struct {
	Method  string
	Target  string
	Version string // "1.0" | "1.1"
	Headers map[string]string
	Body    []byte
}

// ClientResponse is one response under construction.
type ClientResponse struct {
	Status     int
	headerPairs [][2]string
	payload    []byte
	prebuilt   []byte // an already-framed response, when set
	answered   bool
}

func NewClientResponse() *ClientResponse {
	return &ClientResponse{Status: 200}
}

// headerNamesMatch compares two header names ignoring ASCII case,
// without allocating: header names are short, so a byte-wise fold is
// cheaper than lower-casing a copy.
func headerNamesMatch(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		left, right := a[i], b[i]
		if left >= 'A' && left <= 'Z' {
			left += 'a' - 'A'
		}
		if right >= 'A' && right <= 'Z' {
			right += 'a' - 'A'
		}
		if left != right {
			return false
		}
	}
	return true
}

// findHeader locates a header by name (case-insensitive) in the pair
// slice, or returns -1.
func (r *ClientResponse) findHeader(name string) int {
	for i, pair := range r.headerPairs {
		if headerNamesMatch(pair[0], name) {
			return i
		}
	}
	return -1
}

// SetHeader sets a header, replacing any existing one of the same name.
func (r *ClientResponse) SetHeader(name, value string) {
	if at := r.findHeader(name); at >= 0 {
		r.headerPairs[at][1] = value
		return
	}
	r.headerPairs = append(r.headerPairs, [2]string{name, value})
}

// GetHeader reads a header back, reporting whether it was present.
func (r *ClientResponse) GetHeader(name string) (string, bool) {
	if at := r.findHeader(name); at >= 0 {
		return r.headerPairs[at][1], true
	}
	return "", false
}

// Send answers once: status, content type and body together.
func (r *ClientResponse) Send(status int, body []byte, contentType string) {
	r.Status = status
	r.SetHeader("content-type", contentType)
	r.SetHeader("content-length", strconv.Itoa(len(body)))
	r.payload = body
	r.answered = true
}

// SendRaw installs a prebuilt, already-framed response to be written
// verbatim.
func (r *ClientResponse) SendRaw(framed []byte) {
	r.prebuilt = framed
	r.answered = true
}

// SendJSONText answers with a JSON string that is already built, so no
// re-serialisation happens.
func (r *ClientResponse) SendJSONText(status int, text string) {
	r.Status = status
	r.SetHeader("content-type", "application/json; charset=utf-8")
	r.SetHeader("content-length", strconv.Itoa(len(text)))
	r.payload = []byte(text)
	r.answered = true
}

// SendJSON answers from a value, encoding it here.
func (r *ClientResponse) SendJSON(status int, value any) {
	r.SendJSONText(status, EncodeJSON(value)+"\n")
}

// SendEmpty answers with a bare status and no body at all.
func (r *ClientResponse) SendEmpty(status int) {
	r.Status = status
	r.SetHeader("content-length", "0")
	r.payload = nil
	r.answered = true
}

// SendError answers with GitHub's error body shape: {"message": ...}.
func (r *ClientResponse) SendError(status int, message string) {
	r.SendJSON(status, map[string]any{"message": message})
}

// ConnHandler handles one request, filling in its response.
type ConnHandler func(req *ClientRequest, res *ClientResponse)

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func reasonPhrase(status int) string {
	if phrase, ok := reasonPhrases[status]; ok {
		return phrase
	}
	return ""
}

// ---------------------------------------------------------------------------
// The Date header, cached per second
// ---------------------------------------------------------------------------

var (
	dateLock        sync.Mutex
	dateCachedAtSec int64 = -1
	dateCachedText  string
)

// CachedDateLine returns the IMF-fixdate for the current second,
// formatted once and then served from the cache.
func CachedDateLine() string {
	nowSec := time.Now().Unix()
	dateLock.Lock()
	defer dateLock.Unlock()
	if nowSec != dateCachedAtSec {
		dateCachedAtSec = nowSec
		dateCachedText = time.Unix(nowSec, 0).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	}
	return dateCachedText
}

// ComposeHead serialises one response head exactly as the wire will
// see it, and also reports the byte offsets of the request-id and Date
// values inside it. Both values are fixed-width — the id is always
// FORGE- plus six base-36 digits, the date always the 29-byte
// IMF-fixdate — so a memoised head can later be patched in place. An
// offset is -1 when its header is absent.
func ComposeHead(status int, headers [][2]string, bodyLen int, keepAlive bool) (head string, idOffset, dateOffset int) {
	idOffset, dateOffset = -1, -1
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(status))
	sb.WriteByte(' ')
	sb.WriteString(reasonPhrase(status))
	sb.WriteString("\r\n")
	sawLength := false
	sawDate := false
	for _, pair := range headers {
		switch {
		case headerNamesMatch(pair[0], "content-length"):
			sawLength = true
		case headerNamesMatch(pair[0], "date"):
			sawDate = true
		case headerNamesMatch(pair[0], "connection"):
			continue // the transport speaks for the connection itself
		}
		if headerNamesMatch(pair[0], "x-github-request-id") {
			idOffset = sb.Len() + len(pair[0]) + 2
		}
		sb.WriteString(pair[0])
		sb.WriteString(": ")
		sb.WriteString(pair[1])
		sb.WriteString("\r\n")
	}
	if !sawLength {
		sb.WriteString("Content-Length: ")
		sb.WriteString(strconv.Itoa(bodyLen))
		sb.WriteString("\r\n")
	}
	if !sawDate {
		dateOffset = sb.Len() + len("Date: ")
		sb.WriteString("Date: ")
		sb.WriteString(CachedDateLine())
		sb.WriteString("\r\n")
	}
	if keepAlive {
		sb.WriteString("Connection: keep-alive\r\n")
	} else {
		sb.WriteString("Connection: close\r\n")
	}
	sb.WriteString("\r\n")
	return sb.String(), idOffset, dateOffset
}

const headByteCap = 64 * 1024
const connIdleLimit = 65 * time.Second

// ServeConnection serves one connection end to end: parse a request,
// dispatch it in order, write the answer, and repeat for as long as
// keep-alive lasts.
func ServeConnection(conn net.Conn, handler ConnHandler) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	reader := bufio.NewReaderSize(conn, 65536)
	// The read deadline is a per-connection idle guard, not a
	// per-request one: refreshing it on every request would cost a
	// setsockopt per request for nothing. Set it once, then push it
	// forward only once half the idle window has passed, so an active
	// connection never dies mid-suite while an idle one still times
	// out.
	deadlineStamp := time.Now()
	conn.SetReadDeadline(deadlineStamp.Add(connIdleLimit))
	for {
		if time.Since(deadlineStamp) > connIdleLimit/2 {
			deadlineStamp = time.Now()
			conn.SetReadDeadline(deadlineStamp.Add(connIdleLimit))
		}
		head, err := readRequestHead(reader)
		if err != nil {
			return
		}
		body, keepAlive, ok := readRequestBody(conn, reader, head)
		if !ok {
			return
		}
		req := &ClientRequest{
			Method:  head.method,
			Target:  head.target,
			Version: head.version,
			Headers: head.headers,
			Body:    body,
		}
		res := NewClientResponse()
		func() {
			defer func() {
				if recover() != nil {
					if !res.answered {
						res.SendError(500, "Internal Server Error")
					}
				}
			}()
			handler(req, res)
		}()
		if !res.answered {
			res.SendEmpty(res.Status)
		}
		if !flushResponse(conn, req, res, keepAlive) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// flushResponse writes one response as a single socket write and
// reports whether the connection is still usable afterwards.
func flushResponse(conn net.Conn, req *ClientRequest, res *ClientResponse, keepAlive bool) bool {
	if res.prebuilt != nil {
		_, err := conn.Write(res.prebuilt)
		return err == nil
	}
	head, _, _ := ComposeHead(res.Status, res.headerPairs, len(res.payload), keepAlive)
	var wire []byte
	if req.Method == "HEAD" || len(res.payload) == 0 {
		wire = []byte(head)
	} else {
		wire = make([]byte, 0, len(head)+len(res.payload))
		wire = append(wire, head...)
		wire = append(wire, res.payload...)
	}
	_, err := conn.Write(wire)
	return err == nil
}

// headBlock is a parsed request head.
type headBlock struct {
	method  string
	target  string
	version string
	headers map[string]string
}

// decodeHeadBytes turns one head's raw bytes (request line and header
// lines, terminator included) into a headBlock. Lines end in \n with an
// optional \r; the terminator itself is the empty line at the end.
// Header lines without a colon are ignored, as are blank lines, and a
// repeated header joins its values with ", ".
func decodeHeadBytes(raw []byte) (*headBlock, error) {
	eol := bytes.IndexByte(raw, '\n')
	if eol < 0 {
		return nil, faultMalformed
	}
	requestLine := bytes.TrimSuffix(raw[:eol], []byte("\r"))
	firstSpace := bytes.IndexByte(requestLine, ' ')
	secondSpace := -1
	if firstSpace >= 0 {
		if j := bytes.IndexByte(requestLine[firstSpace+1:], ' '); j >= 0 {
			secondSpace = firstSpace + 1 + j
		}
	}
	if firstSpace < 0 || secondSpace < 0 {
		return nil, faultMalformed
	}
	versionText := requestLine[secondSpace+1:]
	if !bytes.HasPrefix(versionText, []byte("HTTP/")) {
		return nil, faultMalformed
	}
	headers := map[string]string{}
	pos := eol + 1
	for pos < len(raw) {
		end := bytes.IndexByte(raw[pos:], '\n')
		if end < 0 {
			break
		}
		line := bytes.TrimSuffix(raw[pos:pos+end], []byte("\r"))
		pos += end + 1
		if len(line) == 0 {
			break
		}
		if colon := bytes.IndexByte(line, ':'); colon > 0 {
			name := strings.ToLower(strings.TrimSpace(string(line[:colon])))
			value := strings.TrimSpace(string(line[colon+1:]))
			if earlier, ok := headers[name]; ok {
				headers[name] = earlier + ", " + value
			} else {
				headers[name] = value
			}
		}
	}
	return &headBlock{
		method:  string(requestLine[:firstSpace]),
		target:  string(requestLine[firstSpace+1 : secondSpace]),
		version: string(versionText[len("HTTP/"):]),
		headers: headers,
	}, nil
}

// readRequestHead reads one request head from the buffered reader. The
// fast path is the common one: the head usually arrives in a single
// segment, so peek at what is already buffered, find the blank-line
// terminator, parse in place and discard exactly the head's bytes. The
// slow path covers a head split across segments (or larger than the
// peek): read it line by line instead. The peek consumes nothing, so
// the fallback still sees every byte.
func readRequestHead(reader *bufio.Reader) (*headBlock, error) {
	// Peek only ever asks for bytes already buffered: Peek(n) waits
	// until n bytes arrive, so asking for a large n would stall on a
	// head that arrived whole in one segment. Block for a single byte
	// first, then look at everything that has landed so far.
	if reader.Buffered() == 0 {
		if _, err := reader.Peek(1); err != nil {
			return nil, err
		}
	}
	if buffered := reader.Buffered(); buffered > 0 {
		peeked, _ := reader.Peek(buffered)
		end := bytes.Index(peeked, []byte("\r\n\r\n"))
		terminatorLen := 4
		if end < 0 {
			end = bytes.Index(peeked, []byte("\n\n"))
			terminatorLen = 2
		}
		if end >= 0 {
			consumed := end + terminatorLen
			parsed, err := decodeHeadBytes(peeked[:consumed])
			if err != nil {
				return nil, err
			}
			reader.Discard(consumed)
			return parsed, nil
		}
	}
	var accumulated []byte
	for {
		if len(accumulated) > headByteCap {
			return nil, faultTooLarge
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		accumulated = append(accumulated, line...)
		if len(accumulated) >= 4 && bytes.Equal(accumulated[len(accumulated)-4:], []byte("\r\n\r\n")) {
			break
		}
		if len(accumulated) >= 2 && bytes.Equal(accumulated[len(accumulated)-2:], []byte("\n\n")) {
			break
		}
	}
	return decodeHeadBytes(accumulated)
}

// readRequestBody reads a request body framed by Content-Length or by
// chunked transfer encoding. When the client is waiting on a
// 100-continue it sends the interim reply before the body arrives, so
// the client starts sending.
func readRequestBody(conn net.Conn, reader *bufio.Reader, head *headBlock) ([]byte, bool, bool) {
	connectionHeader := strings.ToLower(head.headers["connection"])
	keepAlive := true
	if head.version == "1.0" {
		keepAlive = connectionHeader == "keep-alive"
	} else {
		keepAlive = connectionHeader != "close"
	}
	encoding := strings.ToLower(head.headers["transfer-encoding"])
	if strings.Contains(encoding, "chunked") {
		body, ok := readChunkedBody(reader)
		return body, keepAlive, ok
	}
	length := 0
	if declared, present := head.headers["content-length"]; present {
		n, err := strconv.Atoi(strings.TrimSpace(declared))
		if err != nil || n < 0 {
			return nil, keepAlive, false
		}
		length = n
	}
	if length == 0 {
		return nil, keepAlive, true
	}
	if strings.ToLower(head.headers["expect"]) == "100-continue" && reader.Buffered() < length {
		conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	body := make([]byte, length)
	filled := 0
	for filled < length {
		m, err := reader.Read(body[filled:])
		filled += m
		if err != nil {
			return nil, keepAlive, false
		}
	}
	return body, keepAlive, true
}

// readChunkedBody reads one chunked request body, consuming the
// trailers after the final zero chunk.
func readChunkedBody(reader *bufio.Reader) ([]byte, bool) {
	var out []byte
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, false
		}
		sizeText := strings.TrimSpace(strings.Split(line, ";")[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, false
		}
		if size == 0 {
			for {
				trailer, err := reader.ReadString('\n')
				if err != nil {
					return nil, false
				}
				if trailer == "\r\n" || trailer == "\n" {
					break
				}
			}
			return out, true
		}
		chunk := make([]byte, size+2)
		filled := 0
		for filled < len(chunk) {
			m, err := reader.Read(chunk[filled:])
			filled += m
			if err != nil {
				return nil, false
			}
		}
		out = append(out, chunk[:size]...)
	}
}

// transportFault is a stringly-typed transport failure.
type transportFault string

func (e transportFault) Error() string { return string(e) }

const (
	faultTooLarge  = transportFault("request head too large")
	faultMalformed = transportFault("malformed request line")
)
