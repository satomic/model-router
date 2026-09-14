// Package usagestats holds precomputed usage statistics.
//
// Answering a Usage page request by reading and parsing every trace file is O(traces): at a
// few thousand records that is seconds of blocking work per page view, repeated for every
// range button and every drill-down. The records are immutable once written, so the scan is
// done once in the background and the page reads the result.
//
// What is stored is a per (date, user, model) bucket rather than a finished report. A finished
// report would have to be precomputed for the cross product of every range and every user; the
// buckets answer all of those by summing a few thousand small rows in memory.
//
// Latency is the one figure that does not survive plain summing. Each bucket therefore carries
// a histogram, and each histogram entry keeps both a count and the sum of the values that
// landed in it -- so a percentile is reported as the mean of the bucket it falls in rather than
// the bucket's floor. In the sparse tail, where the buckets are widest and a P95 actually
// lands, that entry usually holds a single sample and the figure comes back exact.
package usagestats

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/traces"
)

var (
	RollupPath string
	// Separate from the rollup: it has to be writable *while* the rollup is being computed,
	// and it is what makes "a build is running" visible to the other workers sharing data/.
	BuildPath string
)

// Init binds the package to the data directory.
func Init(dataDir string) {
	RollupPath = filepath.Join(dataDir, "usage_rollup.json")
	BuildPath = filepath.Join(dataDir, "usage_build.json")
}

const RollupVersion = 1

const (
	DefaultInterval = 3600.0
	// The longest range the console offers, so every button is answered from one file.
	DefaultDays = 90
	// A build that claimed the lease this long ago is assumed dead -- a worker killed
	// mid-scan must not leave the button greyed out forever.
	buildLeaseTTL = 1800.0
)

// latencyBucket rounds a latency into a histogram bucket.
func latencyBucket(ms float64) int {
	switch {
	case ms < 1000:
		return int(ms/10) * 10
	case ms < 10000:
		return int(ms/100) * 100
	case ms < 100000:
		return int(ms/1000) * 1000
	default:
		return int(ms/10000) * 10000
	}
}

type bucket struct {
	Requests         int
	Errors           int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// bucket floor -> [count, sum of the real values]
	Latency map[int][2]float64
}

type bucketKey struct{ date, user, model string }

func num(v int) json.Number      { return omap.Int(v) }
func fnum(v float64) json.Number { return omap.Num(v) }

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// Build scans the trace files and folds them into buckets. Blocking; run it off the request path.
func Build(store *traces.Store, days int) *omap.Map {
	started := time.Now()
	buckets := map[bucketKey]*bucket{}
	records := store.Scan(days, "")
	for _, record := range records {
		ts := record.Str("ts")
		if len(ts) < 10 {
			continue
		}
		date := ts[:10]
		user := omap.AsString(record.Value("user_id"))
		if user == "" {
			user = "anonymous"
		}
		model := omap.AsString(record.Value("model"))
		if model == "" {
			model = "unknown"
		}
		key := bucketKey{date, user, model}
		b := buckets[key]
		if b == nil {
			b = &bucket{Latency: map[int][2]float64{}}
			buckets[key] = b
		}
		usage := record.Map("usage")
		if usage == nil {
			usage = omap.New()
		}
		b.Requests++
		if omap.AsString(record.Value("status")) == "error" {
			b.Errors++
		}
		b.PromptTokens += int(usage.Float("prompt_tokens", 0))
		b.CompletionTokens += int(usage.Float("completion_tokens", 0))
		b.TotalTokens += int(usage.Float("total_tokens", 0))
		if record.Value("total_ms") != nil {
			totalMS, ok := omap.AsFloat(record.Value("total_ms"))
			if ok {
				slot := latencyBucket(totalMS)
				entry := b.Latency[slot]
				entry[0]++
				entry[1] += totalMS
				b.Latency[slot] = entry
			}
		}
	}

	keys := make([]bucketKey, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].date != keys[j].date {
			return keys[i].date < keys[j].date
		}
		if keys[i].user != keys[j].user {
			return keys[i].user < keys[j].user
		}
		return keys[i].model < keys[j].model
	})

	rows := make([]any, 0, len(keys))
	for _, key := range keys {
		b := buckets[key]
		latency := omap.New()
		slots := make([]int, 0, len(b.Latency))
		for slot := range b.Latency {
			slots = append(slots, slot)
		}
		sort.Ints(slots)
		for _, slot := range slots {
			entry := b.Latency[slot]
			latency.Set(strconv.Itoa(slot), []any{num(int(entry[0])), fnum(entry[1])})
		}
		rows = append(rows, mapOf(
			"date", key.date,
			"user", key.user,
			"model", key.model,
			"requests", num(b.Requests),
			"errors", num(b.Errors),
			"prompt_tokens", num(b.PromptTokens),
			"completion_tokens", num(b.CompletionTokens),
			"total_tokens", num(b.TotalTokens),
			"latency", latency,
		))
	}

	buildMS := float64(time.Since(started).Microseconds()) / 1000
	return mapOf(
		"version", num(RollupVersion),
		"built_at", fnum(nowSeconds()),
		"build_ms", json.Number(fmt.Sprintf("%.1f", buildMS)),
		"days_scanned", num(days),
		"traces", num(len(records)),
		"buckets", rows,
	)
}

func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var (
	cacheMu    sync.Mutex
	cache      *omap.Map
	cacheMTime float64
)

// Load returns the rollup as last written, re-read only when the file has actually changed.
func Load() *omap.Map {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	stamp := jsonfile.MTime(RollupPath)
	if cache == nil || stamp != cacheMTime {
		data := jsonfile.Read(RollupPath)
		// A rollup written by an older layout is discarded rather than misread: it would be
		// summed into totals that quietly mean something else.
		if data.Int("version", 0) == RollupVersion {
			cache = data
		} else {
			cache = omap.New()
		}
		cacheMTime = stamp
	}
	return cache
}

func BuiltAt() float64 { return Load().Float("built_at", 0) }

// -- Build state, shared across workers ---------------------------------------

// Status reports whether a build is running, and when the last one finished.
func Status() *omap.Map {
	state := jsonfile.Read(BuildPath)
	building := state.Str("state") == "building" &&
		nowSeconds()-state.Float("started_at", 0) < buildLeaseTTL
	rollup := Load()
	out := mapOf("building", building)
	if builtAt := BuiltAt(); builtAt != 0 {
		out.Set("built_at", fnum(builtAt))
	} else {
		out.Set("built_at", nil)
	}
	out.Set("build_ms", rollup.Value("build_ms"))
	out.Set("traces", rollup.Value("traces"))
	if errText := state.Str("error"); errText != "" {
		out.Set("error", errText)
	} else {
		out.Set("error", nil)
	}
	return out
}

// claim is a best-effort single-flight lease: data/ is shared between workers, writes are
// atomic, and a duplicate scan is wasteful rather than corrupting.
func claim() bool {
	state := jsonfile.Read(BuildPath)
	if state.Str("state") == "building" && nowSeconds()-state.Float("started_at", 0) < buildLeaseTTL {
		return false
	}
	pid := os.Getpid()
	_ = jsonfile.Write(BuildPath, mapOf(
		"state", "building",
		"started_at", fnum(nowSeconds()),
		"owner_pid", num(pid),
		"error", nil,
	))
	return jsonfile.Read(BuildPath).Int("owner_pid", 0) == pid
}

func release(errText string) {
	var errValue any
	if errText != "" {
		errValue = errText
	}
	_ = jsonfile.Write(BuildPath, mapOf(
		"state", "idle",
		"started_at", num(0),
		"finished_at", fnum(nowSeconds()),
		"owner_pid", num(os.Getpid()),
		"error", errValue,
	))
}

// Refresh rebuilds the rollup unless one is already running, reporting whether this call did it.
func Refresh(store *traces.Store, days int) (bool, error) {
	if !claim() {
		return false, nil
	}
	data := Build(store, days)
	if err := jsonfile.Write(RollupPath, data); err != nil {
		log.Printf("WARNING mr: usage rollup failed: %v", err)
		release(err.Error())
		return false, err
	}
	log.Printf("INFO mr: usage rollup rebuilt traces=%s buckets=%d in %sms",
		omap.AsString(data.Value("traces")), len(data.Slice("buckets")), omap.AsString(data.Value("build_ms")))
	release("")
	return true, nil
}

// ErrBusy reports that another worker holds the build lease.
var ErrBusy = errors.New("a usage rollup is already running")

// -- Serving a query ----------------------------------------------------------

// percentile reads the value at the rank a full sort would have found, off the merged
// histogram.
//
// Reports the mean of the entry the rank falls in, not its floor: the entry is one sample wide
// almost everywhere in the tail, which is where a P95 lands.
func percentile(hist map[int][2]float64, count int) any {
	if count == 0 {
		return nil
	}
	target := int(float64(count) * 0.95)
	if target < 1 {
		target = 1
	}
	slots := make([]int, 0, len(hist))
	for slot := range hist {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	seen := 0
	for _, slot := range slots {
		entry := hist[slot]
		seen += int(entry[0])
		if seen >= target {
			return json.Number(fmt.Sprintf("%.1f", entry[1]/entry[0]))
		}
	}
	return nil
}

// Report aggregates the buckets into the shape the console reads.
//
// `scope` narrows every rollup except `by_user`, which stays the full roster: it is what the
// console's scope picker is built from, so narrowing it would remove every other name from the
// list as soon as one was picked.
func Report(days int, scope string, isAdmin bool) *omap.Map {
	rollup := Load()
	rows := rollup.Slice("buckets")

	// The most recent `days` dates that actually have records, matching what scanning the N
	// newest date directories used to select -- a gap-free calendar window would silently
	// shorten the range on a service that was idle over a weekend.
	dateSet := map[string]bool{}
	for _, item := range rows {
		if row, ok := item.(*omap.Map); ok {
			dateSet[row.Str("date")] = true
		}
	}
	dates := make([]string, 0, len(dateSet))
	for date := range dateSet {
		dates = append(dates, date)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if days < 1 {
		days = 1
	}
	if len(dates) > days {
		dates = dates[:days]
	}
	inRange := map[string]bool{}
	for _, date := range dates {
		inRange[date] = true
	}

	type dayTotals struct{ requests, totalTokens, errors int }
	type userTotals struct{ requests, totalTokens int }

	byModel := map[string]int{}
	byDay := map[string]*dayTotals{}
	byUser := map[string]*userTotals{}
	totals := map[string]int{}
	latency := map[int][2]float64{}
	latencyCount := 0
	latencySum := 0.0

	for _, item := range rows {
		row, ok := item.(*omap.Map)
		if !ok || !inRange[row.Str("date")] {
			continue
		}
		owner := row.Str("user")
		u := byUser[owner]
		if u == nil {
			u = &userTotals{}
			byUser[owner] = u
		}
		requests := row.Int("requests", 0)
		u.requests += requests
		u.totalTokens += row.Int("total_tokens", 0)
		if scope != "" && owner != scope {
			continue
		}
		totals["requests"] += requests
		totals["errors"] += row.Int("errors", 0)
		for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
			totals[field] += row.Int(field, 0)
		}
		byModel[row.Str("model")] += requests
		day := byDay[row.Str("date")]
		if day == nil {
			day = &dayTotals{}
			byDay[row.Str("date")] = day
		}
		day.requests += requests
		day.totalTokens += row.Int("total_tokens", 0)
		day.errors += row.Int("errors", 0)
		if hist := row.Map("latency"); hist != nil {
			for _, slotText := range hist.Keys() {
				pair, ok := hist.Value(slotText).([]any)
				if !ok || len(pair) < 2 {
					continue
				}
				slot, err := strconv.Atoi(slotText)
				if err != nil {
					continue
				}
				count, _ := omap.AsFloat(pair[0])
				sum, _ := omap.AsFloat(pair[1])
				entry := latency[slot]
				entry[0] += count
				entry[1] += sum
				latency[slot] = entry
				latencyCount += int(count)
				latencySum += sum
			}
		}
	}

	totalsOut := mapOf(
		"requests", num(totals["requests"]),
		"errors", num(totals["errors"]),
		"prompt_tokens", num(totals["prompt_tokens"]),
		"completion_tokens", num(totals["completion_tokens"]),
		"total_tokens", num(totals["total_tokens"]),
	)
	if latencyCount > 0 {
		totalsOut.Set("avg_ms", json.Number(fmt.Sprintf("%.1f", latencySum/float64(latencyCount))))
	} else {
		totalsOut.Set("avg_ms", nil)
	}
	totalsOut.Set("p95_ms", percentile(latency, latencyCount))

	// Ties broken by name, so the chart's row order does not drift between rebuilds.
	modelNames := make([]string, 0, len(byModel))
	for name := range byModel {
		modelNames = append(modelNames, name)
	}
	sort.Slice(modelNames, func(i, j int) bool {
		if byModel[modelNames[i]] != byModel[modelNames[j]] {
			return byModel[modelNames[i]] > byModel[modelNames[j]]
		}
		return modelNames[i] < modelNames[j]
	})
	modelRows := make([]any, 0, len(modelNames))
	for _, name := range modelNames {
		modelRows = append(modelRows, mapOf("model", name, "requests", num(byModel[name])))
	}

	dayNames := make([]string, 0, len(byDay))
	for name := range byDay {
		dayNames = append(dayNames, name)
	}
	sort.Strings(dayNames)
	dayRows := make([]any, 0, len(dayNames))
	for _, name := range dayNames {
		day := byDay[name]
		dayRows = append(dayRows, mapOf(
			"date", name,
			"requests", num(day.requests),
			"total_tokens", num(day.totalTokens),
			"errors", num(day.errors),
		))
	}

	userNames := make([]string, 0, len(byUser))
	for name := range byUser {
		userNames = append(userNames, name)
	}
	sort.Slice(userNames, func(i, j int) bool {
		if byUser[userNames[i]].requests != byUser[userNames[j]].requests {
			return byUser[userNames[i]].requests > byUser[userNames[j]].requests
		}
		return userNames[i] < userNames[j]
	})
	userRows := make([]any, 0, len(userNames))
	for _, name := range userNames {
		u := byUser[name]
		userRows = append(userRows, mapOf(
			"user_id", name,
			"requests", num(u.requests),
			"total_tokens", num(u.totalTokens),
		))
	}

	scopeOut := scope
	if scopeOut == "" {
		scopeOut = "all"
	}
	out := mapOf(
		"scope", scopeOut,
		"is_admin", isAdmin,
		"days", num(days),
		"totals", totalsOut,
		"by_model", modelRows,
		"by_day", dayRows,
		"by_user", userRows,
	)
	// The page states how old the numbers are, because "precomputed" and "live" answer
	// different questions and a stat card cannot be read without knowing which it is.
	status := Status()
	for _, key := range status.Keys() {
		out.Set(key, status.Value(key))
	}
	return out
}
