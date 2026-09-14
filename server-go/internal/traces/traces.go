// Package traces stores full call traces.
//
// Layout: logs/traces/<YYYY-MM-DD>/<user_id>/<trace_id>.json -- one file per *user
// interaction*, which makes querying by date, user or trace id straightforward. The most
// recent N summaries are indexed in memory so listings are fast.
//
// One file is not one HTTP request. An agentic client (GitHub Copilot, for one) answers a
// single user question with a whole loop of requests: it calls the model, runs the tool the
// model asked for, appends the result and calls again, repeating until the model stops asking
// for tools. Every one of those requests carries the same interaction id, so Add folds them
// into one file as successive `turns` -- the record of what the user did stays one record, and
// the tool calls that made it up are all inside it.
package traces

import (
	"container/list"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/satomic/model-router/server-go/internal/omap"
)

var unsafeSegment = regexp.MustCompile(`[^\w.\-]`)

// MaxQueryDates is how many date *directories* Query will look at when no explicit date is
// given. This is a directory count, not a calendar window: over an idle deployment 60
// directories reach back much further than 60 days, and over a busy one they are simply the
// 60 most recent days.
const MaxQueryDates = 60

// maxTurns is a ceiling on how many turns one interaction record keeps. An agent stuck in a
// tool loop would otherwise grow a single file without bound; past this the turns are counted
// but no longer stored, and `turns_truncated` says so rather than the record quietly lying.
const maxTurns = 200

// safeSegment sanitises a path segment against directory traversal and illegal characters;
// leading/trailing dots are stripped so '..' cannot survive.
func safeSegment(segment string) string {
	s := unsafeSegment.ReplaceAllString(segment, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	s = strings.Trim(s, "._")
	if s == "" {
		return "anonymous"
	}
	return s
}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func summarize(trace *omap.Map) *omap.Map {
	routing := trace.Map("routing")
	if routing == nil {
		routing = omap.New()
	}
	request := trace.Map("request")
	if request == nil {
		request = omap.New()
	}
	turnCount := trace.Value("turn_count")
	if turnCount == nil || omap.AsString(turnCount) == "0" {
		turnCount = json.Number("1")
	}
	return mapOf(
		"id", trace.Str("id"),
		"ts", trace.Str("ts"),
		"user_id", trace.Value("user_id"),
		"session_id", trace.Value("session_id"),
		// The client-supplied id that ties the turns of one user interaction together, and how
		// many turns this record ended up holding. A one-turn trace is the ordinary case; more
		// than one means an agentic tool loop.
		"interaction_id", trace.Value("interaction_id"),
		"turn_count", turnCount,
		"strategy", trace.Value("strategy"),
		"model", routing.Value("model"),
		"reason", routing.Value("reason"),
		"decision_ms", routing.Value("decision_ms"),
		"total_ms", trace.Value("total_ms"),
		"status", trace.Value("status"),
		"stream", request.Bool("stream", false),
		"prompt_preview", trace.Value("prompt_preview"),
	)
}

// sumUsage adds up the token usage of every turn.
//
// The replayed conversation is deliberately counted once per turn: each request really did
// send the whole chain upstream and really was billed for it, so the interaction's cost is the
// sum, not the last turn's figure. Returns nil when no turn reported usage at all -- a zeroed
// usage block would read as "this was free".
func sumUsage(turns []any) *omap.Map {
	fields := []string{"prompt_tokens", "completion_tokens", "total_tokens"}
	totals := map[string]int{}
	seen := false
	for _, item := range turns {
		turn, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		response := turn.Map("response")
		if response == nil {
			continue
		}
		usage := response.Map("usage")
		if usage == nil || usage.Len() == 0 {
			continue
		}
		seen = true
		for _, field := range fields {
			totals[field] += int(usage.Float(field, 0))
		}
	}
	if !seen {
		return nil
	}
	out := omap.New()
	for _, field := range fields {
		out.Set(field, json.Number(fmt.Sprintf("%d", totals[field])))
	}
	return out
}

// sameMessage reports whether messages[index] is still the message the previous turn ended on.
//
// A length comparison alone would call a trimmed chain an append; comparing the one boundary
// message catches that in constant time, which a full prefix compare on a long conversation
// would not be worth.
func sameMessage(previous any, messages []any, index int) bool {
	if index < 0 || index >= len(messages) {
		return false
	}
	candidate, okA := messages[index].(*omap.Map)
	prev, okB := previous.(*omap.Map)
	if !okA || !okB {
		return jsonEqual(messages[index], previous)
	}
	return candidate.Str("role") == prev.Str("role") &&
		jsonEqual(candidate.Value("content"), prev.Value("content"))
}

func jsonEqual(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(left) == string(right)
}

// mergeTurn folds a follow-up request into the interaction record it belongs to.
//
// What is kept from `base`: the id, the opening timestamp, the prompt preview and -- most
// importantly -- `routing`. The model was chosen once for the whole interaction, so a second
// decision block would be a decision that never happened.
//
// What is taken from `incoming`: the accumulated message chain, the latest response and
// status, and the backend that served it. `request.messages` therefore always holds the
// complete conversation including every tool call and tool result, which is what makes the
// single record a full chain rather than a fragment.
func mergeTurn(base, incoming *omap.Map) *omap.Map {
	doc := base
	prior := doc.Slice("turns")
	if prior == nil {
		prior = []any{}
	}
	var prevMessages []any
	if request := doc.Map("request"); request != nil {
		prevMessages = request.Slice("messages")
	}
	newTurns := incoming.Slice("turns")

	for _, item := range newTurns {
		turn, ok := item.(*omap.Map)
		if !ok {
			continue
		}
		turn.Set("index", json.Number(fmt.Sprintf("%d", len(prior)+1)))
		// An agentic client appends to the chain and replays it, so a turn's request is the
		// first `message_count` messages of the final chain -- no need to store a second copy
		// of a conversation that is already recorded in full at the top level. When the client
		// rewrote history instead of appending, that reconstruction would be wrong, so those
		// turns carry their own messages and say why.
		count := turn.Int("message_count", 0)
		// `>=`, not `>`: the opening turn's chain *is* the one stored at the top level, so it
		// is reconstructible too and a second copy of it would double the record for nothing.
		appended := count >= len(prevMessages) &&
			(len(prevMessages) == 0 ||
				sameMessage(prevMessages[len(prevMessages)-1], turn.Slice("messages"), len(prevMessages)-1))
		if appended {
			turn.Delete("messages")
		} else if turn.Has("messages") && turn.Value("messages") != nil {
			turn.Set("rewritten", true)
			// This turn's chain is not an extension of the previous one, so the top level is
			// about to stop being a superset of what came before -- and the earlier turns were
			// stored without their own copies precisely because it was. Give the last of them
			// its chain back before that stops being true.
			if len(prior) > 0 {
				if last, ok := prior[len(prior)-1].(*omap.Map); ok && !last.Has("messages") {
					last.Set("messages", prevMessages)
					last.Set("superseded", true)
				}
			}
		}
		// The parameters are repeated verbatim on every turn of a Copilot loop, and `tools`
		// alone is far bigger than the rest of the record -- keep them only when they differ
		// from the ones already stored at the top level.
		var topParams any
		if request := doc.Map("request"); request != nil {
			topParams = request.Value("params")
		}
		if jsonEqual(turn.Value("params"), topParams) {
			turn.Delete("params")
		}
		prior = append(prior, turn)
	}

	truncated := doc.Int("turns_truncated", 0)
	if len(prior) > maxTurns {
		truncated += len(prior) - maxTurns
		prior = prior[len(prior)-maxTurns:]
	}

	doc.Set("turns", prior)
	// Counts every turn that happened, including any dropped, so the number always matches
	// what the client did rather than what survived the cap.
	doc.Set("turn_count", json.Number(fmt.Sprintf("%d", doc.Int("turn_count", 0)+len(newTurns))))
	if truncated > 0 {
		doc.Set("turns_truncated", json.Number(fmt.Sprintf("%d", truncated)))
	}

	if request := incoming.Value("request"); request != nil {
		doc.Set("request", request)
	}
	if backend := incoming.Value("backend"); backend != nil {
		doc.Set("backend", backend)
	}
	doc.Set("status", incoming.Value("status"))
	if errText := incoming.Value("error"); errText != nil && omap.AsString(errText) != "" {
		doc.Set("error", errText)
	} else {
		doc.Delete("error")
	}

	// The interaction's token cost, at the top level rather than only inside `response`: an
	// interaction whose last turn failed still spent everything the earlier turns spent, and
	// hanging the total off a response that is null would report it as free.
	usage := sumUsage(prior)
	doc.Set("usage", nilIfEmpty(usage))
	if response := incoming.Map("response"); response != nil {
		// Content and finish_reason come from the closing turn -- that is the answer the user
		// actually read -- while usage is the whole interaction's.
		merged := response.Clone()
		merged.Set("usage", nilIfEmpty(usage))
		doc.Set("response", merged)
	} else {
		doc.Set("response", nil)
	}

	total := 0.0
	for _, item := range prior {
		if turn, ok := item.(*omap.Map); ok {
			total += turn.Float("total_ms", 0)
		}
	}
	doc.Set("total_ms", round1(total))
	decisionMS := 0.0
	if routing := doc.Map("routing"); routing != nil {
		decisionMS = routing.Float("decision_ms", 0)
	}
	backend := doc.Map("backend")
	if backend == nil {
		backend = omap.New()
		doc.Set("backend", backend)
	}
	// Same relationship as a single-turn trace (total = decision + backend), just summed, so
	// the console's latency breakdown keeps adding up.
	backend.Set("latency_ms", round1(total-decisionMS))
	return doc
}

func nilIfEmpty(m *omap.Map) any {
	if m == nil {
		return nil
	}
	return m
}

func round1(v float64) json.Number {
	return json.Number(fmt.Sprintf("%.1f", v))
}

type indexEntry struct {
	id      string
	summary *omap.Map
	path    string
}

// Store is the on-disk trace store plus the in-memory index of recent summaries.
type Store struct {
	Root      string
	maxMemory int

	mu    sync.Mutex
	order *list.List // of *indexEntry, oldest first
	index map[string]*list.Element
	// interaction id -> trace id, so a follow-up turn finds its record without a disk search.
	// Only ever a cache: interactionPath falls back to the directory when an entry is missing,
	// which is what makes this survive a restart or an eviction.
	interactions map[string]string
}

func New(root string, maxMemory int) *Store {
	s := &Store{
		Root:         root,
		maxMemory:    maxMemory,
		order:        list.New(),
		index:        map[string]*list.Element{},
		interactions: map[string]string{},
	}
	_ = os.MkdirAll(root, 0o755)
	s.loadRecent()
	return s
}

func (s *Store) tracePath(trace *omap.Map) string {
	ts := trace.Str("ts")
	date := ts
	if len(date) > 10 {
		date = date[:10]
	}
	user := safeSegment(omap.AsString(trace.Value("user_id")))
	return filepath.Join(s.Root, date, user, safeSegment(trace.Str("id"))+".json")
}

// loadRecent walks the date directories newest-first, loading recent trace files into the
// in-memory index.
func (s *Store) loadRecent() {
	dateDirs, err := os.ReadDir(s.Root)
	if err != nil {
		return
	}
	names := []string{}
	for _, entry := range dateDirs {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	type candidate struct {
		path  string
		mtime int64
	}
	files := []candidate{}
	for _, date := range names {
		dateDir := filepath.Join(s.Root, date)
		userDirs, err := os.ReadDir(dateDir)
		if err != nil {
			continue
		}
		for _, userEntry := range userDirs {
			if !userEntry.IsDir() {
				continue
			}
			userDir := filepath.Join(dateDir, userEntry.Name())
			traceFiles, err := os.ReadDir(userDir)
			if err != nil {
				continue
			}
			for _, file := range traceFiles {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				info, err := file.Info()
				if err != nil {
					continue
				}
				files = append(files, candidate{filepath.Join(userDir, file.Name()), info.ModTime().UnixNano()})
			}
		}
		if len(files) >= s.maxMemory {
			break
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })
	if len(files) > s.maxMemory {
		files = files[len(files)-s.maxMemory:]
	}
	for _, file := range files {
		trace := readTrace(file.path)
		if trace == nil || trace.Str("id") == "" { // skip corrupt files
			continue
		}
		s.put(trace.Str("id"), summarize(trace), file.path)
		if interaction := omap.AsString(trace.Value("interaction_id")); interaction != "" {
			s.interactions[interaction] = trace.Str("id")
		}
	}
}

// put inserts or refreshes an index entry, moving it to the newest position. Callers hold s.mu
// (or are still in the constructor).
func (s *Store) put(id string, summary *omap.Map, path string) {
	if element, ok := s.index[id]; ok {
		entry := element.Value.(*indexEntry)
		entry.summary = summary
		entry.path = path
		s.order.MoveToBack(element)
		return
	}
	s.index[id] = s.order.PushBack(&indexEntry{id: id, summary: summary, path: path})
}

// Add persists a turn, folding it into the interaction record it belongs to.
//
// The whole read-merge-write runs under the store lock: the turns of one interaction are
// sequential from the client's point of view, but nothing stops two of them overlapping at the
// server, and a lost update here would silently drop a tool call from the chain.
func (s *Store) Add(trace *omap.Map) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingPath := s.interactionPath(trace)
	var base *omap.Map
	if existingPath != "" {
		base = readTrace(existingPath)
	}
	var doc *omap.Map
	var path string
	if base == nil {
		doc = mergeTurn(newRecord(trace), trace)
		path = s.tracePath(doc)
	} else {
		doc = mergeTurn(base, trace)
		path = existingPath
		// The caller minted an id for this turn; the record keeps the one it opened with, so
		// `x-trace-id` on the response points at the interaction.
		trace.Set("id", doc.Str("id"))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return
	}

	s.put(doc.Str("id"), summarize(doc), path)
	if interaction := omap.AsString(doc.Value("interaction_id")); interaction != "" {
		s.interactions[interaction] = doc.Str("id")
	}
	for s.order.Len() > s.maxMemory {
		oldest := s.order.Front()
		if oldest == nil {
			break
		}
		evicted := oldest.Value.(*indexEntry).id
		s.order.Remove(oldest)
		delete(s.index, evicted)
		// Drop the interaction pointer with the summary it pointed at, or a long-lived
		// interaction id would keep resolving to an id no longer in the index.
		s.forgetInteractions(evicted)
	}
}

// ResolveInteraction points trace["id"] at the record this turn will be folded into, and
// returns it.
//
// Called before the upstream call so the `x-trace-id` response header names the interaction
// rather than a per-request id that would resolve to nothing. Add repeats the lookup under the
// lock, so this is an optimisation for the header only and a stale answer here cannot split a
// record.
func (s *Store) ResolveInteraction(trace *omap.Map) string {
	s.mu.Lock()
	path := s.interactionPath(trace)
	s.mu.Unlock()
	if path != "" {
		if doc := readTrace(path); doc != nil && doc.Str("id") != "" {
			trace.Set("id", doc.Str("id"))
		}
	}
	return trace.Str("id")
}

// newRecord is the interaction record a first turn opens: everything except the per-turn
// fields, which mergeTurn adds.
func newRecord(trace *omap.Map) *omap.Map {
	doc := omap.New()
	for _, key := range trace.Keys() {
		if key == "turns" || key == "response" || key == "error" {
			continue
		}
		doc.Set(key, trace.Value(key))
	}
	doc.Set("turns", []any{})
	doc.Set("turn_count", json.Number("0"))
	return doc
}

// interactionPath is the file already holding this interaction, or "" if this turn opens it.
//
// Called with the lock held. The in-memory pointer answers the common case; the directory walk
// is the fallback for an interaction whose summary has been evicted or that predates this
// process, scoped to the one date/user directory the record can possibly be in.
func (s *Store) interactionPath(trace *omap.Map) string {
	interaction := omap.AsString(trace.Value("interaction_id"))
	if interaction == "" {
		return ""
	}
	if known, ok := s.interactions[interaction]; ok {
		if element, ok := s.index[known]; ok {
			path := element.Value.(*indexEntry).path
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}
	directory := filepath.Dir(s.tracePath(trace))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return ""
	}
	for _, file := range entries {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, file.Name())
		doc := readTrace(path)
		if doc == nil || omap.AsString(doc.Value("interaction_id")) != interaction {
			continue
		}
		id := doc.Str("id")
		if id == "" {
			id = strings.TrimSuffix(file.Name(), ".json")
		}
		s.interactions[interaction] = id
		return path
	}
	return ""
}

func readTrace(path string) *omap.Map {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := omap.FromJSON(data)
	if err != nil { // a corrupt file must not fail the request being traced
		return nil
	}
	if trace, ok := value.(*omap.Map); ok {
		return trace
	}
	return nil
}

// List returns trace summaries newest-first from the in-memory index, optionally filtered by
// date, user or session.
func (s *Store) List(limit int, date, userID, sessionID string) []*omap.Map {
	s.mu.Lock()
	summaries := make([]*omap.Map, 0, s.order.Len())
	for element := s.order.Front(); element != nil; element = element.Next() {
		summaries = append(summaries, element.Value.(*indexEntry).summary)
	}
	s.mu.Unlock()

	result := []*omap.Map{}
	for i := len(summaries) - 1; i >= 0; i-- {
		summary := summaries[i]
		if date != "" && !strings.HasPrefix(summary.Str("ts"), date) {
			continue
		}
		if userID != "" && omap.AsString(summary.Value("user_id")) != userID {
			continue
		}
		if sessionID != "" && omap.AsString(summary.Value("session_id")) != sessionID {
			continue
		}
		result = append(result, summary)
		if len(result) >= limit {
			break
		}
	}
	return result
}

// QueryResult is what the console lists.
type QueryResult struct {
	Total     int         `json:"total"`
	Items     []*omap.Map `json:"items"`
	Offset    int         `json:"offset"`
	Limit     int         `json:"limit"`
	Truncated bool        `json:"truncated"`
}

// Query reads a page of summaries straight off disk.
//
// This is what the console lists, rather than the in-memory index: the index only holds the
// most recent maxMemory summaries, so anything older was invisible to a listing no matter
// which filters were passed.
//
// Ordering is by (date directory, file mtime) rather than by the `ts` inside each file. mtime
// is written when the trace is persisted -- at the end of the very request its ts opens -- so
// the two orders agree, and mtime comes from the directory entry, which means a page costs
// `limit` file reads instead of one read per trace on disk.
//
// `date` and `userID` are directory names, so filtering on them narrows the walk instead of
// scanning everything. `traceID` matches as a substring of the filename, which is what makes
// the console's search box work on a partial id. `sessionID` lives *inside* the file, so it is
// the one filter that costs a read per candidate -- pair it with a date or a user to keep that
// bounded.
func (s *Store) Query(date, userID, traceID, sessionID string, offset, limit, maxDates int) QueryResult {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	if _, err := os.Stat(s.Root); err != nil {
		return QueryResult{Total: 0, Items: []*omap.Map{}, Offset: offset, Limit: limit}
	}

	truncated := false
	var dateDirs []string
	if date != "" {
		dateDirs = []string{filepath.Join(s.Root, date)}
	} else {
		entries, _ := os.ReadDir(s.Root)
		all := []string{}
		for _, entry := range entries {
			if entry.IsDir() {
				all = append(all, entry.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(all)))
		if maxDates < 1 {
			maxDates = 1
		}
		kept := all
		if len(all) > maxDates {
			kept = all[:maxDates]
			// Reported so the console can say the count is a floor rather than a total: a
			// silently shortened total reads as "that is all there is".
			truncated = true
		}
		for _, name := range kept {
			dateDirs = append(dateDirs, filepath.Join(s.Root, name))
		}
	}

	needle := ""
	if traceID != "" {
		needle = safeSegment(traceID)
	}
	type candidate struct {
		date  string
		mtime int64
		path  string
	}
	candidates := []candidate{}
	for _, dateDir := range dateDirs {
		dateName := filepath.Base(dateDir)
		userDirs := []string{}
		if userID != "" {
			userDirs = append(userDirs, filepath.Join(dateDir, safeSegment(userID)))
		} else {
			entries, err := os.ReadDir(dateDir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					userDirs = append(userDirs, filepath.Join(dateDir, entry.Name()))
				}
			}
		}
		for _, userDir := range userDirs {
			entries, err := os.ReadDir(userDir)
			if err != nil {
				continue
			}
			for _, file := range entries {
				name := file.Name()
				if file.IsDir() || !strings.HasSuffix(name, ".json") {
					continue
				}
				if needle != "" && !strings.Contains(strings.TrimSuffix(name, ".json"), needle) {
					continue
				}
				info, err := file.Info()
				if err != nil { // deleted between the listing and the stat
					continue
				}
				candidates = append(candidates, candidate{dateName, info.ModTime().UnixNano(), filepath.Join(userDir, name)})
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].date != candidates[j].date {
			return candidates[i].date > candidates[j].date
		}
		return candidates[i].mtime > candidates[j].mtime
	})

	if sessionID != "" {
		// Not a path segment, so every candidate has to be opened. Done before paging so
		// `total` still counts matches rather than files looked at.
		matched := []*omap.Map{}
		for _, item := range candidates {
			trace := readTrace(item.path)
			if trace == nil || trace.Str("id") == "" { // skip corrupt files
				continue
			}
			if omap.AsString(trace.Value("session_id")) == sessionID {
				matched = append(matched, summarize(trace))
			}
		}
		return QueryResult{
			Total:     len(matched),
			Items:     pageOf(matched, offset, limit),
			Offset:    offset,
			Limit:     limit,
			Truncated: truncated,
		}
	}

	total := len(candidates)
	items := []*omap.Map{}
	end := offset + limit
	if end > total {
		end = total
	}
	if offset < total {
		for _, item := range candidates[offset:end] {
			trace := readTrace(item.path)
			if trace == nil || trace.Str("id") == "" { // skip corrupt files
				continue
			}
			items = append(items, summarize(trace))
		}
	}
	return QueryResult{Total: total, Items: items, Offset: offset, Limit: limit, Truncated: truncated}
}

func pageOf(items []*omap.Map, offset, limit int) []*omap.Map {
	if offset >= len(items) {
		return []*omap.Map{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

// Delete removes one trace by id, reporting whether it existed.
func (s *Store) Delete(traceID string) bool {
	safe := safeSegment(traceID)
	s.mu.Lock()
	var paths []string
	if element, ok := s.index[traceID]; ok {
		paths = append(paths, element.Value.(*indexEntry).path)
		s.order.Remove(element)
		delete(s.index, traceID)
	}
	s.forgetInteractions(traceID)
	s.mu.Unlock()

	if len(paths) == 0 {
		paths = s.globTraces(safe + ".json")
	}
	removed := false
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			continue
		}
		removed = true
		s.pruneDirs(filepath.Dir(path))
	}
	return removed
}

// DeleteMany removes every trace matching date and/or user, returning how many went.
//
// At least one criterion is required: an argument-less call would be a wipe-all, which no
// caller is allowed to reach for by accident.
func (s *Store) DeleteMany(date, userID string) (int, error) {
	if date == "" && userID == "" {
		return 0, fmt.Errorf("DeleteMany requires date and/or user_id")
	}
	var dateDirs []string
	if date != "" {
		dateDirs = []string{filepath.Join(s.Root, date)}
	} else {
		entries, err := os.ReadDir(s.Root)
		if err != nil {
			return 0, nil
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dateDirs = append(dateDirs, filepath.Join(s.Root, entry.Name()))
			}
		}
	}
	deleted := 0
	for _, dateDir := range dateDirs {
		userDirs := []string{}
		if userID != "" {
			userDirs = append(userDirs, filepath.Join(dateDir, safeSegment(userID)))
		} else {
			entries, err := os.ReadDir(dateDir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					userDirs = append(userDirs, filepath.Join(dateDir, entry.Name()))
				}
			}
		}
		for _, userDir := range userDirs {
			entries, err := os.ReadDir(userDir)
			if err != nil {
				continue
			}
			for _, file := range entries {
				name := file.Name()
				if file.IsDir() || !strings.HasSuffix(name, ".json") {
					continue
				}
				if err := os.Remove(filepath.Join(userDir, name)); err != nil {
					continue
				}
				deleted++
				// Drop the in-memory summary too: a stale entry would keep serving Get from a
				// path that no longer exists, and a stale interaction pointer would make the
				// next turn try to append to a deleted file.
				id := strings.TrimSuffix(name, ".json")
				s.mu.Lock()
				if element, ok := s.index[id]; ok {
					s.order.Remove(element)
					delete(s.index, id)
				}
				s.forgetInteractions(id)
				s.mu.Unlock()
			}
			s.pruneDirs(userDir)
		}
	}
	return deleted, nil
}

// forgetInteractions drops every interaction pointer aimed at a trace id. Called with the
// lock held.
func (s *Store) forgetInteractions(traceID string) {
	for interaction, id := range s.interactions {
		if id == traceID {
			delete(s.interactions, interaction)
		}
	}
}

// pruneDirs removes an emptied <user> directory and, if it was the last one, its <date>
// parent -- so a deleted day stops appearing as an empty bucket in Query.
func (s *Store) pruneDirs(userDir string) {
	for _, directory := range []string{userDir, filepath.Dir(userDir)} {
		if directory == s.Root {
			return
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(directory); err != nil {
			return
		}
	}
}

func (s *Store) globTraces(pattern string) []string {
	matches, _ := filepath.Glob(filepath.Join(s.Root, "*", "*", pattern))
	return matches
}

// Scan reads trace summaries for the last `days` days straight off disk, for usage
// aggregation.
//
// The in-memory index only holds the most recent maxMemory entries, which is not enough for
// per-day statistics, hence reading the files here. When userLogin is set, only that user's
// directory is scanned.
func (s *Store) Scan(days int, userLogin string) []*omap.Map {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if days < 1 {
		days = 1
	}
	if len(names) > days {
		names = names[:days]
	}

	summaries := []*omap.Map{}
	for _, date := range names {
		dateDir := filepath.Join(s.Root, date)
		userDirs := []string{}
		if userLogin != "" {
			userDirs = append(userDirs, filepath.Join(dateDir, safeSegment(userLogin)))
		} else {
			dirEntries, err := os.ReadDir(dateDir)
			if err != nil {
				continue
			}
			for _, entry := range dirEntries {
				if entry.IsDir() {
					userDirs = append(userDirs, filepath.Join(dateDir, entry.Name()))
				}
			}
		}
		for _, userDir := range userDirs {
			files, err := os.ReadDir(userDir)
			if err != nil {
				continue
			}
			for _, file := range files {
				if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				trace := readTrace(filepath.Join(userDir, file.Name()))
				if trace == nil || trace.Str("id") == "" { // skip corrupt files
					continue
				}
				summary := summarize(trace)
				// The top-level total covers every turn of the interaction and survives a
				// failed final turn; the response block is the pre-turns fallback.
				usage := trace.Map("usage")
				if usage == nil {
					if response := trace.Map("response"); response != nil {
						usage = response.Map("usage")
					}
				}
				if usage == nil {
					usage = omap.New()
				}
				summary.Set("usage", mapOf(
					"prompt_tokens", json.Number(fmt.Sprintf("%d", int(usage.Float("prompt_tokens", 0)))),
					"completion_tokens", json.Number(fmt.Sprintf("%d", int(usage.Float("completion_tokens", 0)))),
					"total_tokens", json.Number(fmt.Sprintf("%d", int(usage.Float("total_tokens", 0)))),
				))
				summaries = append(summaries, summary)
			}
		}
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].Str("ts") > summaries[j].Str("ts")
	})
	return summaries
}

func (s *Store) Get(traceID string) *omap.Map {
	s.mu.Lock()
	path := ""
	if element, ok := s.index[traceID]; ok {
		path = element.Value.(*indexEntry).path
	}
	s.mu.Unlock()
	if path == "" { // Not in the in-memory index, so search the whole tree
		if matches := s.globTraces(safeSegment(traceID) + ".json"); len(matches) > 0 {
			path = matches[0]
		}
	}
	if path == "" {
		return nil
	}
	return readTrace(path)
}
