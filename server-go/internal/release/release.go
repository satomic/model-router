// Package release performs the background check for a newer published release.
//
// The result is cached in memory and on disk under data/, so a restart does not re-poll GitHub
// and several workers do not each ask. The check is deliberately unauthenticated: the releases
// endpoint is public, and requiring a token would make the feature unavailable exactly where
// it is most useful, on a fresh deployment nobody has configured yet.
//
// Failure is not an error state worth surfacing loudly. A router that cannot reach github.com
// still routes, so a failed check keeps the last known answer and records why, and the console
// simply shows no banner.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
	"github.com/satomic/model-router/server-go/internal/version"
)

var path string

func Init(dataDir string) { path = filepath.Join(dataDir, "release.json") }

const (
	// One day. A router is not a package manager: checking more often would add nothing an
	// operator would notice and would spend somebody's rate limit doing it.
	CheckSeconds = 24 * time.Hour
	// The first check waits this long. Startup must never depend on github.com being reachable.
	WarmupSeconds = 30 * time.Second
	// After a failure, retry sooner than the full interval but not so soon that an outage
	// becomes a tight loop.
	RetrySeconds = time.Hour
)

var (
	mu    sync.Mutex
	state *omap.Map
)

func load() *omap.Map {
	mu.Lock()
	defer mu.Unlock()
	if state == nil || state.Len() == 0 {
		state = jsonfile.Read(path)
	}
	return state
}

func mapOf(pairs ...any) *omap.Map {
	out := omap.New()
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Status is what the console renders. It always answers, even before the first check has run.
func Status() *omap.Map {
	st := load()
	latest := st.Str("latest_version")
	return mapOf(
		"current_version", version.Version,
		"latest_version", nilIfEmpty(latest),
		// Recomputed here rather than read from disk: the stored answer was true for whatever
		// version was running when it was written, and an upgrade must not keep showing the
		// banner for the release it just installed.
		"update_available", latest != "" && version.IsNewer(latest, version.Version),
		"release_url", st.Value("release_url"),
		"published_at", st.Value("published_at"),
		"checked_at", st.Value("checked_at"),
		"error", st.Value("error"),
	)
}

var client = &http.Client{Timeout: 10 * time.Second}

// Check asks GitHub once for the latest release and stores the answer. Never fails the caller.
func Check(ctx context.Context) *omap.Map {
	st := load().Clone()
	st.Set("checked_at", json.Number(strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', -1, 64)))

	fail := func(err error) *omap.Map {
		message := fmt.Sprintf("%T: %v", err, err)
		st.Set("error", message)
		log.Printf("INFO mr.release: release check failed: %s", message)
		store(st)
		return Status()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, version.ReleasesAPI, nil)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(err)
	}

	if resp.StatusCode == http.StatusNotFound {
		// A repository with no published release yet. Not a failure: there is simply no newer
		// version, and reporting an error here would look like a broken check.
		st.Set("latest_version", "")
		st.Set("release_url", nil)
		st.Set("published_at", nil)
		st.Set("error", nil)
	} else if resp.StatusCode >= 400 {
		return fail(fmt.Errorf("GitHub %d", resp.StatusCode))
	} else {
		value, err := omap.FromJSON(body)
		if err != nil {
			return fail(err)
		}
		data, ok := value.(*omap.Map)
		if !ok {
			return fail(fmt.Errorf("GitHub returned an unexpected payload"))
		}
		tag := data.Str("tag_name")
		if tag == "" {
			tag = data.Str("name")
		}
		st.Set("latest_version", tag)
		st.Set("release_url", data.Value("html_url"))
		st.Set("published_at", data.Value("published_at"))
		st.Set("error", nil)
		if tag != "" && version.IsNewer(tag, version.Version) {
			log.Printf("INFO mr.release: a newer release is available: %s (running %s)", tag, version.Version)
		}
	}
	store(st)
	return Status()
}

func store(st *omap.Map) {
	mu.Lock()
	state = st
	mu.Unlock()
	_ = jsonfile.Write(path, st)
}

// Loop polls forever. Each iteration is independent so no single failure ends the task.
func Loop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(WarmupSeconds):
	}
	for {
		delay := CheckSeconds
		result := Check(ctx)
		if result.Value("error") != nil {
			delay = RetrySeconds
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}
