// The forge's memory: one git object store (blobs, trees, commits
// and tags, each addressed by its SHA-1) with the repositories built
// on top of it, plus the issue and pull-request domain that the REST,
// GraphQL and MCP surfaces all share. There is exactly one store per
// process, so an object brought into being through any surface is the
// same object through all of them.
//
// Determinism is a design rule here, not an accident: nothing reads
// the wall clock or a random source. Timestamps are a monotonic
// counter laid over a fixed base epoch, ids come from counters, and
// even request ids are counter output. The store's state is therefore
// a pure function of the sequence of requests it has served, which is
// precisely what a trial's live pass and its replay both require.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ObjectKind distinguishes the four kinds of git object.
type ObjectKind int

const (
	KindCommit ObjectKind = 1
	KindTree   ObjectKind = 2
	KindBlob   ObjectKind = 3
	KindTag    ObjectKind = 4
)

// Name is the word git itself uses for the kind, as it appears in
// object headers.
func (k ObjectKind) Name() string {
	switch k {
	case KindCommit:
		return "commit"
	case KindTree:
		return "tree"
	case KindBlob:
		return "blob"
	default:
		return "tag"
	}
}

// GitObject is one stored object: its kind and its raw bytes.
type GitObject struct {
	Kind ObjectKind
	Raw  []byte
}

// ZeroOID is the all-zero object id git uses for "no object".
const ZeroOID = "0000000000000000000000000000000000000000"

// baseEpochSec anchors the deterministic clock: sequence number n
// corresponds to baseEpochSec + n.
const baseEpochSec = 1700000000

// HashGitObject computes an object's id: the SHA-1 of the header
// "<kind> <length>\0" followed by the raw bytes.
func HashGitObject(kind ObjectKind, raw []byte) string {
	digest := sha1.New()
	digest.Write([]byte(kind.Name() + " " + strconv.Itoa(len(raw)) + "\x00"))
	digest.Write(raw)
	return hex.EncodeToString(digest.Sum(nil))
}

// TreeRow is one entry of a tree object.
type TreeRow struct {
	Mode string
	Name string
	OID  string
}

// SplitTreeBytes decodes a tree object's raw bytes into its rows, in
// the order they are stored.
func SplitTreeBytes(raw []byte) []TreeRow {
	var rows []TreeRow
	at := 0
	for at < len(raw) {
		space := findByteFrom(raw, ' ', at)
		if space < 0 {
			break
		}
		nul := findByteFrom(raw, 0, space)
		if nul < 0 || nul+21 > len(raw) {
			break
		}
		rows = append(rows, TreeRow{
			Mode: string(raw[at:space]),
			Name: string(raw[space+1 : nul]),
			OID:  hex.EncodeToString(raw[nul+1 : nul+21]),
		})
		at = nul + 21
	}
	return rows
}

func findByteFrom(haystack []byte, needle byte, from int) int {
	for i := from; i < len(haystack); i++ {
		if haystack[i] == needle {
			return i
		}
	}
	return -1
}

// treeOrderKey is the sort key git applies to a tree row: directories
// sort as if their name ended in a slash.
func treeOrderKey(row TreeRow) string {
	if row.Mode == "40000" || row.Mode == "040000" {
		return row.Name + "/"
	}
	return row.Name
}

// SerializeTree renders rows as a tree object's raw bytes, ordered the
// way git orders them.
func SerializeTree(rows []TreeRow) []byte {
	ordered := make([]TreeRow, len(rows))
	copy(ordered, rows)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && treeOrderKey(ordered[j]) < treeOrderKey(ordered[j-1]); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	var out []byte
	for _, row := range ordered {
		out = append(out, []byte(row.Mode+" "+row.Name+"\x00")...)
		binary, _ := hex.DecodeString(row.OID)
		out = append(out, binary...)
	}
	return out
}

// CommitHead is the decoded header of a commit object.
type CommitHead struct {
	TreeOID       string
	ParentOIDs    []string
	AuthorLine    string
	CommitterLine string
	MessageBody   string
}

// ScanParentLines reads just the parent lines out of a commit header.
// The push fast path needs this cheap shape test without paying for a
// full header decode.
func ScanParentLines(raw []byte) []string {
	var parents []string
	rest := raw
	for len(rest) > 0 {
		var line []byte
		if cut := bytes.IndexByte(rest, '\n'); cut >= 0 {
			line, rest = rest[:cut], rest[cut+1:]
		} else {
			line, rest = rest, nil
		}
		if len(line) == 0 {
			break // the blank line ends the header block
		}
		if bytes.HasPrefix(line, []byte("parent ")) && len(line) == 47 {
			parents = append(parents, string(line[7:]))
		}
	}
	return parents
}

// DecodeCommit splits a commit object into its header fields and its
// message. Only the header block is treated as text; the message is
// cut from the raw bytes after the blank line.
func DecodeCommit(raw []byte) CommitHead {
	var head CommitHead
	blank := -1
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == '\n' && raw[i+1] == '\n' {
			blank = i
			break
		}
	}
	headerText := string(raw)
	if blank >= 0 {
		headerText = string(raw[:blank])
		head.MessageBody = string(raw[blank+2:])
	}
	for _, line := range strings.Split(headerText, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			head.TreeOID = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			head.ParentOIDs = append(head.ParentOIDs, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			head.AuthorLine = line[7:]
		case strings.HasPrefix(line, "committer "):
			head.CommitterLine = line[10:]
		}
	}
	return head
}

// TagTargetOID returns the object a tag object points at, from its
// `object` header.
func TagTargetOID(raw []byte) string {
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

// identPieces is one git identity line taken apart.
type identPieces struct {
	displayName string
	mailbox     string
	epochSec    int64
}

// SplitIdentLine parses "Name <email> 1700000000 +0000".
func SplitIdentLine(line string) identPieces {
	line = strings.TrimSpace(line)
	open := strings.Index(line, "<")
	close := strings.Index(line, ">")
	if open < 0 || close < 0 || close < open {
		return identPieces{displayName: line, epochSec: baseEpochSec}
	}
	epoch := int64(baseEpochSec)
	if tail := strings.Fields(line[close+1:]); len(tail) > 0 {
		if n, err := strconv.ParseInt(tail[0], 10, 64); err == nil {
			epoch = n
		}
	}
	return identPieces{
		displayName: strings.TrimSpace(line[:open]),
		mailbox:     line[open+1 : close],
		epochSec:    epoch,
	}
}

// FormatISOSecond renders epoch seconds as an ISO-8601 timestamp that
// matches Date.toISOString byte for byte at whole-second precision.
func FormatISOSecond(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// ---------------------------------------------------------------------------
// Domain records
// ---------------------------------------------------------------------------

// CommentRecord is one comment on an issue or a pull request.
type CommentRecord struct {
	ID       int
	BodyText string
	SeqNo    int64
}

// IssueRecord is one issue in a repository.
type IssueRecord struct {
	Number     int
	ID         int
	Title      string
	BodyText   string
	State      string // "open" | "closed"
	LabelNames []string
	Comments   []*CommentRecord
	SeqNo      int64
}

// PullRecord is one pull request in a repository.
type PullRecord struct {
	Number         int
	ID             int
	Title          string
	BodyText       string
	State          string // "open" | "closed"
	Merged         bool
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	MergeCommitSHA string
	MergedSeq      int64
	Comments       []*CommentRecord
	SeqNo          int64
}

// LabelRecord is one label a repository defines.
type LabelRecord struct {
	Name  string
	Color string
}

// Repository is the state of one repository: its refs plus the issue
// and pull-request domain layered over them.
type Repository struct {
	ID            int
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	Refs          map[string]string // full ref name -> commit oid
	Labels        map[string]*LabelRecord
	Issues        map[int]*IssueRecord
	Pulls         map[int]*PullRecord
	NextNumber    int
	SeqNo         int64
}

// FileEntry is one file of a flattened tree: blob oid plus mode.
type FileEntry struct {
	OID  string
	Mode string
}

// FileMap flattens a whole tree into path -> FileEntry.
type FileMap map[string]FileEntry

// PathMatch is the resolution of one path inside a tree: either a file
// (with its bytes) or a directory (with its rows).
type PathMatch struct {
	Kind    string // "file" | "dir"
	OID     string
	Raw     []byte
	Mode    string
	Entries []TreeRow
}

// ---------------------------------------------------------------------------
// The store itself
// ---------------------------------------------------------------------------

// Depot is the single in-memory store behind every surface.
type Depot struct {
	ObjectMap   map[string]*GitObject
	RepoMap     map[string]*Repository
	login       string
	hasLogin    bool
	loginLock   sync.RWMutex
	counter     int64
	repoSeq     int
	issueSeq    int
	commentSeq  int
	requestSeq  int64
}

func NewDepot() *Depot {
	return &Depot{
		ObjectMap: map[string]*GitObject{},
		RepoMap:   map[string]*Repository{},
	}
}

// NextSeq hands out the next deterministic sequence number. It is
// atomic because read-only dispatches run in parallel and must never
// tear the counter; under parallel dispatch the values may interleave
// differently than a serial run's would, but each stays unique and the
// sequence stays monotonic.
func (d *Depot) NextSeq() int64 {
	return atomic.AddInt64(&d.counter, 1)
}

// ISOForSeq renders the timestamp a sequence number stands for.
func (d *Depot) ISOForSeq(seq int64) string { return FormatISOSecond(baseEpochSec + seq) }

// EpochForSeq is ISOForSeq in epoch seconds.
func (d *Depot) EpochForSeq(seq int64) int64 { return baseEpochSec + seq }

// MintRequestID builds a fixed-width request id ("FORGE-" plus six
// base-36 digits). The fixed width matters: a memoised response head
// gets a fresh id patched into it in place.
func (d *Depot) MintRequestID() string {
	n := int(atomic.AddInt64(&d.requestSeq, 1))
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var reversed [6]byte
	for i := 5; i >= 0; i-- {
		reversed[i] = alphabet[n%36]
		n /= 36
	}
	return "FORGE-" + string(reversed[:])
}

// LearnLogin adopts the token owner's login from the first request
// that carries an owner; from then on the login is fixed for the life
// of the process.
func (d *Depot) LearnLogin(owner string) {
	if owner == "" {
		return
	}
	d.loginLock.Lock()
	if !d.hasLogin {
		d.login = owner
		d.hasLogin = true
	}
	d.loginLock.Unlock()
}

// LoginCouldBe reports whether owner is the adopted login, or could
// still become it because none has been adopted yet.
func (d *Depot) LoginCouldBe(owner string) bool {
	d.loginLock.RLock()
	defer d.loginLock.RUnlock()
	return !d.hasLogin || d.login == owner
}

// AdoptedLogin returns the adopted login ("" before adoption).
func (d *Depot) AdoptedLogin() string {
	d.loginLock.RLock()
	defer d.loginLock.RUnlock()
	return d.login
}

// ---------------------------------------------------------------------------
// Object storage and graph walks
// ---------------------------------------------------------------------------

// StoreObject writes an object if its id is not already present and
// returns the id either way.
func (d *Depot) StoreObject(kind ObjectKind, raw []byte) string {
	oid := HashGitObject(kind, raw)
	if _, present := d.ObjectMap[oid]; !present {
		d.ObjectMap[oid] = &GitObject{Kind: kind, Raw: raw}
	}
	return oid
}

// FetchObject returns the object under an id, or nil.
func (d *Depot) FetchObject(oid string) *GitObject { return d.ObjectMap[oid] }

// FollowToCommit peels tag objects until it reaches the commit they
// ultimately name.
func (d *Depot) FollowToCommit(oid string) (string, bool) {
	current := oid
	for hop := 0; hop < 16; hop++ {
		obj := d.ObjectMap[current]
		if obj == nil {
			return "", false
		}
		switch obj.Kind {
		case KindCommit:
			return current, true
		case KindTag:
			current = TagTargetOID(obj.Raw)
		default:
			return "", false
		}
	}
	return "", false
}

// ReachableSet returns every object reachable from the given tips.
func (d *Depot) ReachableSet(tips []string) map[string]bool {
	found := map[string]bool{}
	stack := append([]string{}, tips...)
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if found[current] {
			continue
		}
		obj := d.ObjectMap[current]
		if obj == nil {
			continue
		}
		found[current] = true
		switch obj.Kind {
		case KindCommit:
			head := DecodeCommit(obj.Raw)
			stack = append(stack, head.TreeOID)
			stack = append(stack, head.ParentOIDs...)
		case KindTree:
			for _, row := range SplitTreeBytes(obj.Raw) {
				stack = append(stack, row.OID)
			}
		case KindTag:
			stack = append(stack, TagTargetOID(obj.Raw))
		}
	}
	return found
}

// IsAncestorOf reports whether ancestor is the descendant itself or
// one of its ancestors.
func (d *Depot) IsAncestorOf(ancestor, descendant string) bool {
	visited := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == ancestor {
			return true
		}
		if visited[current] {
			continue
		}
		visited[current] = true
		if obj := d.ObjectMap[current]; obj != nil && obj.Kind == KindCommit {
			stack = append(stack, DecodeCommit(obj.Raw).ParentOIDs...)
		}
	}
	return false
}

// CommonAncestor finds a best common ancestor of two commits by
// breadth-first search from both sides, or "" when there is none.
func (d *Depot) CommonAncestor(a, b string) string {
	if a == b {
		return a
	}
	type frontierItem struct {
		oid   string
		depth int
	}
	depthFromA := map[string]int{}
	queueA := []frontierItem{{a, 0}}
	for head := 0; head < len(queueA); head++ {
		item := queueA[head]
		if _, seen := depthFromA[item.oid]; seen {
			continue
		}
		depthFromA[item.oid] = item.depth
		if obj := d.ObjectMap[item.oid]; obj != nil && obj.Kind == KindCommit {
			for _, parent := range DecodeCommit(obj.Raw).ParentOIDs {
				queueA = append(queueA, frontierItem{parent, item.depth + 1})
			}
		}
	}
	visitedB := map[string]bool{}
	queueB := []frontierItem{{b, 0}}
	best := ""
	bestScore := int(^uint(0) >> 1)
	for head := 0; head < len(queueB); head++ {
		item := queueB[head]
		if visitedB[item.oid] {
			continue
		}
		visitedB[item.oid] = true
		if da, ok := depthFromA[item.oid]; ok && da+item.depth < bestScore {
			best = item.oid
			bestScore = da + item.depth
		}
		if obj := d.ObjectMap[item.oid]; obj != nil && obj.Kind == KindCommit {
			for _, parent := range DecodeCommit(obj.Raw).ParentOIDs {
				queueB = append(queueB, frontierItem{parent, item.depth + 1})
			}
		}
	}
	return best
}

// HistoryFrom returns the commits reachable from a tip, newest first
// (depth-first, following parents in order).
func (d *Depot) HistoryFrom(tip string) []string {
	var ordered []string
	visited := map[string]bool{}
	var descend func(current string)
	descend = func(current string) {
		if visited[current] {
			return
		}
		obj := d.ObjectMap[current]
		if obj == nil || obj.Kind != KindCommit {
			return
		}
		visited[current] = true
		ordered = append(ordered, current)
		for _, parent := range DecodeCommit(obj.Raw).ParentOIDs {
			descend(parent)
		}
	}
	descend(tip)
	return ordered
}

// ---------------------------------------------------------------------------
// Trees and files
// ---------------------------------------------------------------------------

// FlattenTree flattens a tree, recursively, into a path -> FileEntry
// map.
func (d *Depot) FlattenTree(treeOID string) FileMap {
	flat := FileMap{}
	var descend func(tree, prefix string)
	descend = func(tree, prefix string) {
		obj := d.ObjectMap[tree]
		if obj == nil || obj.Kind != KindTree {
			return
		}
		for _, row := range SplitTreeBytes(obj.Raw) {
			full := row.Name
			if prefix != "" {
				full = prefix + "/" + row.Name
			}
			if row.Mode == "40000" || row.Mode == "040000" {
				descend(row.OID, full)
			} else {
				flat[full] = FileEntry{OID: row.OID, Mode: row.Mode}
			}
		}
	}
	descend(treeOID, "")
	return flat
}

// FlattenCommit flattens the tree of the commit under an id.
func (d *Depot) FlattenCommit(commitOID string) FileMap {
	obj := d.ObjectMap[commitOID]
	if obj == nil || obj.Kind != KindCommit {
		return FileMap{}
	}
	return d.FlattenTree(DecodeCommit(obj.Raw).TreeOID)
}

// BuildTreeFromFiles stores a tree hierarchy holding exactly the given
// flattened file map and returns the root tree's id.
func (d *Depot) BuildTreeFromFiles(files FileMap) string {
	type treeBuilderNode struct {
		leafFiles map[string]FileEntry
		subDirs   map[string]*treeBuilderNode
	}
	freshNode := func() *treeBuilderNode {
		return &treeBuilderNode{
			leafFiles: map[string]FileEntry{},
			subDirs:   map[string]*treeBuilderNode{},
		}
	}
	root := freshNode()
	for path, entry := range files {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			seg := segments[i]
			if _, ok := node.subDirs[seg]; !ok {
				node.subDirs[seg] = freshNode()
			}
			node = node.subDirs[seg]
		}
		node.leafFiles[segments[len(segments)-1]] = entry
	}
	var emitTree func(n *treeBuilderNode) string
	emitTree = func(n *treeBuilderNode) string {
		var rows []TreeRow
		for name, entry := range n.leafFiles {
			rows = append(rows, TreeRow{Mode: entry.Mode, Name: name, OID: entry.OID})
		}
		for name, child := range n.subDirs {
			rows = append(rows, TreeRow{Mode: "40000", Name: name, OID: emitTree(child)})
		}
		return d.StoreObject(KindTree, SerializeTree(rows))
	}
	return emitTree(root)
}

// ResolvePath resolves a repository-relative path at a commit; the
// empty path names the root directory.
func (d *Depot) ResolvePath(commitOID, path string) *PathMatch {
	commitObj := d.ObjectMap[commitOID]
	if commitObj == nil || commitObj.Kind != KindCommit {
		return nil
	}
	rootTree := DecodeCommit(commitObj.Raw).TreeOID
	if path == "" {
		treeObj := d.ObjectMap[rootTree]
		if treeObj == nil {
			return nil
		}
		return &PathMatch{Kind: "dir", Entries: SplitTreeBytes(treeObj.Raw)}
	}
	segments := strings.Split(path, "/")
	currentTree := rootTree
	for i, seg := range segments {
		treeObj := d.ObjectMap[currentTree]
		if treeObj == nil || treeObj.Kind != KindTree {
			return nil
		}
		var hit *TreeRow
		for _, row := range SplitTreeBytes(treeObj.Raw) {
			if row.Name == seg {
				candidate := row
				hit = &candidate
				break
			}
		}
		if hit == nil {
			return nil
		}
		isLast := i == len(segments)-1
		if !isLast {
			if hit.Mode != "40000" && hit.Mode != "040000" {
				return nil
			}
			currentTree = hit.OID
			continue
		}
		if hit.Mode == "40000" || hit.Mode == "040000" {
			sub := d.ObjectMap[hit.OID]
			if sub == nil {
				return nil
			}
			return &PathMatch{Kind: "dir", Entries: SplitTreeBytes(sub.Raw)}
		}
		blob := d.ObjectMap[hit.OID]
		if blob == nil {
			return nil
		}
		return &PathMatch{Kind: "file", OID: hit.OID, Raw: blob.Raw, Mode: hit.Mode}
	}
	return nil
}

// MintCommit creates a commit object carrying the forge's
// deterministic identity line.
func (d *Depot) MintCommit(treeOID string, parents []string, message string, seq int64) string {
	stamp := d.EpochForSeq(seq)
	ident := "Forge <forge@example.invalid> " + strconv.FormatInt(stamp, 10) + " +0000"
	var headerLines []string
	headerLines = append(headerLines, "tree "+treeOID)
	for _, parent := range parents {
		headerLines = append(headerLines, "parent "+parent)
	}
	headerLines = append(headerLines, "author "+ident, "committer "+ident)
	text := strings.Join(headerLines, "\n") + "\n\n" + message
	return d.StoreObject(KindCommit, []byte(text))
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

// OpenRepository creates an empty repository record.
func (d *Depot) OpenRepository(name, description string, private bool) *Repository {
	d.repoSeq++
	repo := &Repository{
		ID:            d.repoSeq,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Labels:        map[string]*LabelRecord{},
		Issues:        map[int]*IssueRecord{},
		Pulls:         map[int]*PullRecord{},
		NextNumber:    1,
		SeqNo:         d.NextSeq(),
	}
	d.RepoMap[name] = repo
	return repo
}

// HeadRefName is the full ref the repository's HEAD names.
func (d *Depot) HeadRefName(repo *Repository) string {
	return "refs/heads/" + repo.DefaultBranch
}

// BranchTipOID returns the commit a branch name (or full ref) points
// at.
func (d *Depot) BranchTipOID(repo *Repository, branch string) (string, bool) {
	full := branch
	if !strings.HasPrefix(branch, "refs/") {
		full = "refs/heads/" + branch
	}
	oid, ok := repo.Refs[full]
	return oid, ok
}

// ResolveToCommit resolves a ref-ish string (branch, full ref or
// object id) to a commit id.
func (d *Depot) ResolveToCommit(repo *Repository, refish string) (string, bool) {
	if tip, ok := d.BranchTipOID(repo, refish); ok {
		return d.FollowToCommit(tip)
	}
	obj := d.ObjectMap[refish]
	if obj == nil {
		return "", false
	}
	switch obj.Kind {
	case KindCommit:
		return refish, true
	case KindTag:
		return d.FollowToCommit(refish)
	}
	return "", false
}

// WriteResult reports the outcome of a file write.
type WriteResult struct {
	OK     bool
	Status int
	Prob   string
	Commit string
	Blob   string // empty when the write deleted the file
}

// WritePath writes a file on a branch (or deletes it, when content is
// nil), committing on top of the branch tip. The branch is created
// when the repository is still unborn, or when it is missing and the
// repository has a default tip to start from.
func (d *Depot) WritePath(repo *Repository, branch, path string, content []byte, message string, expectedSHA string, hasSHA bool) WriteResult {
	fullRef := "refs/heads/" + branch
	tip, hasTip := repo.Refs[fullRef]
	if !hasTip {
		if fallback, ok := repo.Refs[d.HeadRefName(repo)]; ok && fullRef != d.HeadRefName(repo) {
			tip, hasTip = fallback, true
		}
	}
	var baseCommit string
	var baseOK bool
	if hasTip {
		baseCommit, baseOK = d.FollowToCommit(tip)
	}
	flat := FileMap{}
	if baseOK {
		flat = d.FlattenCommit(baseCommit)
	}
	prior, priorKnown := flat[path]
	if content == nil {
		if !priorKnown {
			return WriteResult{OK: false, Status: 404, Prob: "Not Found"}
		}
		if hasSHA && expectedSHA != prior.OID {
			return WriteResult{OK: false, Status: 409, Prob: "Conflict: the file has changed since the given sha"}
		}
		delete(flat, path)
		newTree := d.BuildTreeFromFiles(flat)
		var parents []string
		if baseOK {
			parents = []string{baseCommit}
		}
		commitOID := d.MintCommit(newTree, parents, message, d.NextSeq())
		repo.Refs[fullRef] = commitOID
		return WriteResult{OK: true, Commit: commitOID}
	}
	if priorKnown && hasSHA && expectedSHA != prior.OID {
		return WriteResult{OK: false, Status: 409, Prob: "Conflict: the file has changed since the given sha"}
	}
	if !priorKnown && hasSHA {
		return WriteResult{OK: false, Status: 422, Prob: "Invalid request: sha supplied for a file that does not exist"}
	}
	blobOID := d.StoreObject(KindBlob, content)
	mode := "100644"
	if priorKnown {
		mode = prior.Mode
	}
	flat[path] = FileEntry{OID: blobOID, Mode: mode}
	newTree := d.BuildTreeFromFiles(flat)
	var parents []string
	if baseOK {
		parents = []string{baseCommit}
	}
	commitOID := d.MintCommit(newTree, parents, message, d.NextSeq())
	repo.Refs[fullRef] = commitOID
	return WriteResult{OK: true, Commit: commitOID, Blob: blobOID}
}

// PathIsFile reports whether a branch holds a file at a path.
func (d *Depot) PathIsFile(repo *Repository, branch, path string) bool {
	tip, ok := d.BranchTipOID(repo, branch)
	if !ok {
		return false
	}
	commit, ok := d.FollowToCommit(tip)
	if !ok {
		return false
	}
	match := d.ResolvePath(commit, path)
	return match != nil && match.Kind == "file"
}

// ---------------------------------------------------------------------------
// Issues and pull requests
// ---------------------------------------------------------------------------

// OpenIssue files a new issue in a repository.
func (d *Depot) OpenIssue(repo *Repository, title, body string, labels []string) *IssueRecord {
	number := repo.NextNumber
	repo.NextNumber++
	d.issueSeq++
	record := &IssueRecord{
		Number:     number,
		ID:         d.issueSeq,
		Title:      title,
		BodyText:   body,
		State:      "open",
		LabelNames: append([]string{}, labels...),
		SeqNo:      d.NextSeq(),
	}
	repo.Issues[number] = record
	return record
}

// commentSink is anything a comment can be appended to (an issue or a
// pull request).
type commentSink interface {
	AppendComment(*CommentRecord)
}

// PostComment appends a comment to its target and returns the record.
func (d *Depot) PostComment(target commentSink, body string) *CommentRecord {
	d.commentSeq++
	record := &CommentRecord{ID: d.commentSeq, BodyText: body, SeqNo: d.NextSeq()}
	target.AppendComment(record)
	return record
}

func (i *IssueRecord) AppendComment(c *CommentRecord) { i.Comments = append(i.Comments, c) }
func (p *PullRecord) AppendComment(c *CommentRecord)  { p.Comments = append(p.Comments, c) }

// CreatePullResult reports the outcome of opening a pull request.
type CreatePullResult struct {
	OK   bool
	Prob string
	Pull *PullRecord
}

// CreatePull opens a pull request between two branches, refusing a
// duplicate of an already-open one for the same head and base.
func (d *Depot) CreatePull(repo *Repository, title, body, head, base string) CreatePullResult {
	headName := strings.TrimPrefix(head, "refs/heads/")
	baseName := strings.TrimPrefix(base, "refs/heads/")
	headOID, ok := d.BranchTipOID(repo, headName)
	if !ok {
		return CreatePullResult{Prob: "head branch " + headName + " does not exist"}
	}
	baseOID, ok := d.BranchTipOID(repo, baseName)
	if !ok {
		return CreatePullResult{Prob: "base branch " + baseName + " does not exist"}
	}
	for _, existing := range repo.Pulls {
		if existing.State == "open" && existing.HeadRef == headName && existing.BaseRef == baseName {
			return CreatePullResult{Prob: "a pull request already exists for " + headName + " into " + baseName}
		}
	}
	number := repo.NextNumber
	repo.NextNumber++
	d.issueSeq++
	headCommit, _ := d.FollowToCommit(headOID)
	baseCommit, _ := d.FollowToCommit(baseOID)
	record := &PullRecord{
		Number:   number,
		ID:       d.issueSeq,
		Title:    title,
		BodyText: body,
		State:    "open",
		HeadRef:  headName,
		BaseRef:  baseName,
		HeadSHA:  headCommit,
		BaseSHA:  baseCommit,
		SeqNo:    d.NextSeq(),
	}
	repo.Pulls[number] = record
	return CreatePullResult{OK: true, Pull: record}
}

// PullSummary is the live merge picture of a pull request.
type PullSummary struct {
	BaseOID    string
	HeadOID    string
	BaseTipOID string
	Conflicted bool
}

// SummarizePull computes a pull request's merge picture from the
// branches as they stand now.
func (d *Depot) SummarizePull(repo *Repository, pull *PullRecord) (PullSummary, bool) {
	rawHead, ok := d.BranchTipOID(repo, pull.HeadRef)
	if !ok {
		return PullSummary{}, false
	}
	rawBase, ok := d.BranchTipOID(repo, pull.BaseRef)
	if !ok {
		return PullSummary{}, false
	}
	headCommit, okHead := d.FollowToCommit(rawHead)
	baseCommit, okBase := d.FollowToCommit(rawBase)
	if !okHead || !okBase {
		return PullSummary{}, false
	}
	common := d.CommonAncestor(headCommit, baseCommit)
	if common == "" {
		return PullSummary{BaseOID: "", HeadOID: headCommit, BaseTipOID: baseCommit, Conflicted: true}, true
	}
	filesAtBase := d.FlattenCommit(common)
	filesAtHead := d.FlattenCommit(headCommit)
	filesAtTip := d.FlattenCommit(baseCommit)
	paths := map[string]bool{}
	for path := range filesAtBase {
		paths[path] = true
	}
	for path := range filesAtHead {
		paths[path] = true
	}
	for path := range filesAtTip {
		paths[path] = true
	}
	conflict := false
	for path := range paths {
		baseEntry, inBase := filesAtBase[path]
		headEntry, inHead := filesAtHead[path]
		tipEntry, inTip := filesAtTip[path]
		headMoved := inHead != inBase || (inHead && inBase && headEntry.OID != baseEntry.OID)
		tipMoved := inTip != inBase || (inTip && inBase && tipEntry.OID != baseEntry.OID)
		if headMoved && tipMoved {
			headOID := ""
			if inHead {
				headOID = headEntry.OID
			}
			tipOID := ""
			if inTip {
				tipOID = tipEntry.OID
			}
			if headOID != tipOID {
				conflict = true
			}
		}
	}
	return PullSummary{BaseOID: common, HeadOID: headCommit, BaseTipOID: baseCommit, Conflicted: conflict}, true
}

// FileDelta is one file's change in a pull request.
type FileDelta struct {
	Path      string
	Status    string
	Additions int
	Deletions int
}

// ChangedFiles lists a pull request's changed files, from its merge
// base to its head.
func (d *Depot) ChangedFiles(repo *Repository, pull *PullRecord) []FileDelta {
	headRaw, _ := d.BranchTipOID(repo, pull.HeadRef)
	if headRaw == "" {
		headRaw = pull.HeadSHA
	}
	baseRaw, _ := d.BranchTipOID(repo, pull.BaseRef)
	if baseRaw == "" {
		baseRaw = pull.BaseSHA
	}
	headOID, _ := d.FollowToCommit(headRaw)
	baseOID, _ := d.FollowToCommit(baseRaw)
	common := d.CommonAncestor(headOID, baseOID)
	if common == "" {
		common = baseOID
	}
	return CompareFileMaps(d, d.FlattenCommit(common), d.FlattenCommit(headOID))
}

// MergeResult reports the outcome of a merge attempt.
type MergeResult struct {
	OK     bool
	Status int
	Prob   string
	SHA    string
}

// MergePullRequest merges a pull request into its base branch:
// "squash" lands a single commit holding the head's changes, "merge"
// a two-parent commit, "rebase" a replay of the head's own commits. A
// conflicted pull request is refused with status 405.
func (d *Depot) MergePullRequest(repo *Repository, pull *PullRecord, method string) MergeResult {
	if pull.Merged {
		return MergeResult{Status: 405, Prob: "Pull Request was already merged"}
	}
	if pull.State == "closed" {
		return MergeResult{Status: 405, Prob: "Pull Request is closed"}
	}
	summary, ok := d.SummarizePull(repo, pull)
	if !ok {
		return MergeResult{Status: 405, Prob: "Pull Request is not mergeable"}
	}
	if summary.Conflicted {
		return MergeResult{Status: 405, Prob: "Pull Request is not mergeable: conflicts must be resolved first"}
	}
	filesAtBase := d.FlattenCommit(summary.BaseOID)
	filesAtHead := d.FlattenCommit(summary.HeadOID)
	combined := d.FlattenCommit(summary.BaseTipOID)
	// Lay the head's changes, measured against the merge base, over
	// the base branch's tip.
	for path, headEntry := range filesAtHead {
		baseEntry, inBase := filesAtBase[path]
		if !inBase || headEntry.OID != baseEntry.OID {
			combined[path] = headEntry
		}
	}
	for path := range filesAtBase {
		if _, inHead := filesAtHead[path]; !inHead {
			if current, ok := combined[path]; ok && current.OID == filesAtBase[path].OID {
				delete(combined, path)
			}
		}
	}
	mergedTree := d.BuildTreeFromFiles(combined)
	var landed string
	switch method {
	case "merge":
		landed = d.MintCommit(mergedTree, []string{summary.BaseTipOID, summary.HeadOID}, "Merge pull request #"+strconv.Itoa(pull.Number)+": "+pull.Title, d.NextSeq())
	case "rebase":
		var chain []string
		marked := map[string]bool{}
		var collect func(current string)
		collect = func(current string) {
			if current == summary.BaseOID || marked[current] {
				return
			}
			obj := d.FetchObject(current)
			if obj == nil || obj.Kind != KindCommit {
				return
			}
			marked[current] = true
			chain = append(chain, current)
			for _, parent := range DecodeCommit(obj.Raw).ParentOIDs {
				collect(parent)
			}
		}
		collect(summary.HeadOID)
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		cursor := summary.BaseTipOID
		landed = summary.BaseTipOID
		for _, oldOID := range chain {
			oldHead := DecodeCommit(d.FetchObject(oldOID).Raw)
			landed = d.MintCommit(oldHead.TreeOID, []string{cursor}, oldHead.MessageBody, d.NextSeq())
			cursor = landed
		}
		if len(chain) == 0 {
			landed = d.MintCommit(mergedTree, []string{summary.BaseTipOID}, pull.Title, d.NextSeq())
		}
	default: // squash
		landed = d.MintCommit(mergedTree, []string{summary.BaseTipOID}, pull.Title, d.NextSeq())
	}
	repo.Refs["refs/heads/"+pull.BaseRef] = landed
	pull.State = "closed"
	pull.Merged = true
	pull.MergeCommitSHA = landed
	pull.MergedSeq = d.NextSeq()
	return MergeResult{OK: true, SHA: landed}
}

// LineDelta counts the line additions and deletions between two file
// versions, as a multi-set difference over lines.
func LineDelta(oldRaw, newRaw []byte) (int, int) {
	countLines := func(raw []byte) map[string]int {
		tally := map[string]int{}
		if raw == nil {
			return tally
		}
		for _, line := range strings.Split(string(raw), "\n") {
			tally[line]++
		}
		return tally
	}
	before := countLines(oldRaw)
	after := countLines(newRaw)
	added, removed := 0, 0
	for line, n := range after {
		if extra := n - before[line]; extra > 0 {
			added += extra
		}
	}
	for line, n := range before {
		if extra := n - after[line]; extra > 0 {
			removed += extra
		}
	}
	return added, removed
}

// CompareFileMaps diffs two flattened file maps into a pull request's
// file list.
func CompareFileMaps(d *Depot, before, after FileMap) []FileDelta {
	union := map[string]bool{}
	for path := range before {
		union[path] = true
	}
	for path := range after {
		union[path] = true
	}
	paths := make([]string, 0, len(union))
	for path := range union {
		paths = append(paths, path)
	}
	sortLexical(paths)
	var rows []FileDelta
	for _, path := range paths {
		beforeEntry, inBefore := before[path]
		afterEntry, inAfter := after[path]
		if inBefore && inAfter && beforeEntry.OID == afterEntry.OID {
			continue
		}
		var oldRaw, newRaw []byte
		if inBefore {
			if obj := d.FetchObject(beforeEntry.OID); obj != nil {
				oldRaw = obj.Raw
			}
		}
		if inAfter {
			if obj := d.FetchObject(afterEntry.OID); obj != nil {
				newRaw = obj.Raw
			}
		}
		additions, deletions := LineDelta(oldRaw, newRaw)
		status := "modified"
		if !inBefore {
			status = "added"
		} else if !inAfter {
			status = "removed"
		}
		rows = append(rows, FileDelta{Path: path, Status: status, Additions: additions, Deletions: deletions})
	}
	return rows
}
