// Package authstore holds JSON-file persistence for sign-in sessions, API keys, and who has
// ever signed in.
//
//	data/auth_sessions.json  sessions (survive a restart; expired entries are pruned on read)
//	data/api_keys.json       API keys -- both the sha256 hash (what lookup compares) and the
//	                         plaintext (so an owner can read their own key back later). The
//	                         plaintext is handed out only to the key's owner; see PublicKey.
//	data/known_users.json    one durable entry per login that has ever signed in, with first
//	                         and last sign-in and a count. Sessions cannot answer "who has used
//	                         this deployment": they are purged the moment they expire, so a user
//	                         who signed in last week leaves no trace in them at all. The model
//	                         policy needs that list -- an admin has to be able to bind a group
//	                         to a user without first asking them to spell their login.
//
// No database: JSON on disk plus an in-memory cache, and multiple workers only need to share
// the same data/ directory.
package authstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satomic/model-router/server-go/internal/jsonfile"
	"github.com/satomic/model-router/server-go/internal/omap"
)

// KeyPrefix is only the *display* prefix and what new keys are minted with: lookup compares
// the sha256 digest, so keys issued under an older prefix keep working unchanged.
const KeyPrefix = "mr_"

// How much of the plaintext prefix the listing shows.
const prefixVisible = 8

func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func token(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		// A failing CSPRNG cannot be worked around: there is no safe token to hand back, and
		// net/http contains the panic to this one connection.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

type Store struct {
	SessionsPath string
	KeysPath     string
	UsersPath    string

	mu            sync.Mutex
	usersMu       sync.Mutex
	sessions      *omap.Map
	keys          *omap.Map
	sessionsMTime float64
	keysMTime     float64
}

func New(dataDir string) *Store {
	s := &Store{
		SessionsPath: filepath.Join(dataDir, "auth_sessions.json"),
		KeysPath:     filepath.Join(dataDir, "api_keys.json"),
		UsersPath:    filepath.Join(dataDir, "known_users.json"),
	}
	s.sessions = jsonfile.Read(s.SessionsPath)
	s.keys = jsonfile.Read(s.KeysPath)
	s.sessionsMTime = jsonfile.MTime(s.SessionsPath)
	s.keysMTime = jsonfile.MTime(s.KeysPath)
	s.purgeExpired()
	return s
}

// -- Cache synchronisation ------------------------------------------------
// Disk is re-read only on a cache miss: that way sessions/keys written by another worker
// (or an external script) are still recognised, while the common case of a cache hit costs
// no extra IO. Callers must hold s.mu.

func (s *Store) reloadSessionsIfChanged() bool {
	stamp := jsonfile.MTime(s.SessionsPath)
	if stamp == s.sessionsMTime {
		return false
	}
	s.sessions = jsonfile.Read(s.SessionsPath)
	s.sessionsMTime = stamp
	return true
}

func (s *Store) reloadKeysIfChanged() bool {
	stamp := jsonfile.MTime(s.KeysPath)
	if stamp == s.keysMTime {
		return false
	}
	s.keys = jsonfile.Read(s.KeysPath)
	s.keysMTime = stamp
	return true
}

func (s *Store) saveSessions() {
	if err := jsonfile.Write(s.SessionsPath, s.sessions); err != nil {
		log.Printf("WARNING mr: could not write %s: %v", s.SessionsPath, err)
	}
	s.sessionsMTime = jsonfile.MTime(s.SessionsPath)
}

func (s *Store) saveKeys() {
	if err := jsonfile.Write(s.KeysPath, s.keys); err != nil {
		log.Printf("WARNING mr: could not write %s: %v", s.KeysPath, err)
	}
	s.keysMTime = jsonfile.MTime(s.KeysPath)
}

// -- Sessions -------------------------------------------------------------

func (s *Store) purgeExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := now()
	changed := false
	for _, sid := range s.sessions.Keys() {
		session := s.sessions.Map(sid)
		if session == nil || session.Float("expires_at", 0) <= stamp {
			s.sessions.Delete(sid)
			changed = true
		}
	}
	if changed {
		s.saveSessions()
	}
}

// CreateSession opens a session for `user` and returns its id.
func (s *Store) CreateSession(user *omap.Map, ttlSeconds int) string {
	sid := token(32)
	stamp := now()
	session := user.Clone()
	session.Set("created_at", json.Number(formatFloat(stamp)))
	session.Set("expires_at", json.Number(formatFloat(stamp+float64(ttlSeconds))))

	s.mu.Lock()
	// Merge in other writers' entries first, so the whole table is not overwritten.
	s.reloadSessionsIfChanged()
	s.sessions.Set(sid, session)
	s.saveSessions()
	s.mu.Unlock()

	// Recorded here rather than at each call site: there are two ways to open a session
	// (GitHub OAuth and the local administrator) and a third would be easy to add without
	// remembering the registry. A failure to record must not fail the sign-in.
	s.RecordSignIn(user)
	return sid
}

func (s *Store) GetSession(sid string) *omap.Map {
	if sid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions.Map(sid)
	if session == nil && s.reloadSessionsIfChanged() {
		session = s.sessions.Map(sid)
	}
	if session == nil {
		return nil
	}
	if session.Float("expires_at", 0) <= now() {
		s.sessions.Delete(sid)
		s.saveSessions()
		return nil
	}
	return session.Clone()
}

func (s *Store) DeleteSession(sid string) {
	if sid == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadSessionsIfChanged()
	if s.sessions.Has(sid) {
		s.sessions.Delete(sid)
		s.saveSessions()
	}
}

// RefreshAdminFlags recomputes is_admin on existing sessions after the admin list changes,
// so nobody has to sign in again.
//
// Local-administrator sessions are skipped: their authority comes from auth.local_admin
// rather than admin_logins, so running them through this predicate would strip the flag.
func (s *Store) RefreshAdminFlags(isAdminLogin func(string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadSessionsIfChanged()
	changed := false
	for _, sid := range s.sessions.Keys() {
		session := s.sessions.Map(sid)
		if session == nil || session.Bool("local_admin", false) {
			continue
		}
		flag := isAdminLogin(session.Str("login"))
		if session.Bool("is_admin", false) != flag {
			session.Set("is_admin", flag)
			changed = true
		}
	}
	if changed {
		s.saveSessions()
	}
}

// RekeyLocalAdmin points this session at the new username and drops every *other* local-admin
// session, after the local administrator's credential changes.
//
// Changing the password has to invalidate sessions opened with the old one -- that is most of
// what changing it is for -- while the operator doing the change stays signed in.
func (s *Store) RekeyLocalAdmin(sid, newLogin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadSessionsIfChanged()
	changed := false
	for _, other := range s.sessions.Keys() {
		session := s.sessions.Map(other)
		if session != nil && session.Bool("local_admin", false) && other != sid {
			s.sessions.Delete(other)
			changed = true
		}
	}
	if sid != "" {
		if current := s.sessions.Map(sid); current != nil && current.Str("login") != newLogin {
			current.Set("login", newLogin)
			current.Set("name", newLogin)
			changed = true
		}
	}
	if changed {
		s.saveSessions()
	}
}

// -- Known users ----------------------------------------------------------
// A separate file and a separate lock from the session table. The two have opposite
// lifetimes -- a session is transient and pruned, this is append-only history -- and keeping
// history in the sessions file would have made the purge delete it.

// RecordSignIn notes that `user` signed in: first_seen, last_seen, and a sign-in count.
//
// Never fails the caller. Being unable to write the registry is not a reason to refuse a
// sign-in, and there is no caller in a position to do anything useful with the error.
func (s *Store) RecordSignIn(user *omap.Map) {
	login := strings.TrimSpace(user.Str("login"))
	if login == "" {
		return
	}
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	data := jsonfile.Read(s.UsersPath)
	key := strings.ToLower(login)
	entry := data.Map(key)
	if entry == nil {
		entry = omap.New()
	}
	stamp := now()
	name := user.Str("name")
	if name == "" {
		name = entry.Str("name")
	}
	if name == "" {
		name = login
	}
	avatar := user.Value("avatar_url")
	if avatar == nil {
		avatar = entry.Value("avatar_url")
	}
	kind := "github"
	if user.Bool("local_admin", false) {
		// Which door they came through, so the admin list can distinguish the local
		// administrator from a GitHub user of the same name.
		kind = "local"
	}
	firstSeen := entry.Float("first_seen", 0)
	if firstSeen == 0 {
		firstSeen = stamp
	}
	updated := omap.New()
	updated.Set("login", login)
	updated.Set("name", name)
	updated.Set("avatar_url", avatar)
	updated.Set("kind", kind)
	updated.Set("first_seen", json.Number(formatFloat(firstSeen)))
	updated.Set("last_seen", json.Number(formatFloat(stamp)))
	updated.Set("sign_ins", json.Number(formatInt(entry.Int("sign_ins", 0)+1)))
	data.Set(key, updated)
	if err := jsonfile.Write(s.UsersPath, data); err != nil {
		log.Printf("WARNING mr: could not record the sign-in of %s: %v", login, err)
	}
}

// ListKnownUsers returns every login that has ever signed in, most recent first.
//
// Read straight from disk each time: this feeds an admin page rather than a request path, and
// a cache would only add a way for one worker's list to lag another's.
func (s *Store) ListKnownUsers() []*omap.Map {
	data := jsonfile.Read(s.UsersPath)
	users := []*omap.Map{}
	for _, key := range data.Keys() {
		if entry := data.Map(key); entry != nil {
			users = append(users, entry)
		}
	}
	sort.SliceStable(users, func(i, j int) bool {
		return users[i].Float("last_seen", 0) > users[j].Float("last_seen", 0)
	})
	return users
}

// -- API keys -------------------------------------------------------------

// CreateAPIKey returns (public record, plaintext key).
//
// Both the plaintext and its digest are persisted: the digest is what lookup compares, and
// the plaintext is what lets the owner read their own key back later instead of having to
// delete it and reconfigure every client. It is only ever handed to that owner.
//
// `scope` limits what this key may reach within its owner's own permitted set; it is
// validated before it gets here. A nil scope stores the unrestricted default, which is also
// what keys created before scopes existed read back as.
func (s *Store) CreateAPIKey(userLogin, name string, scope *omap.Map) (*omap.Map, string) {
	plaintext := KeyPrefix + token(32)
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		// A failing CSPRNG cannot be worked around: there is no safe token to hand back, and
		// net/http contains the panic to this one connection.
		panic(err)
	}
	keyID := hex.EncodeToString(idBytes)
	if name == "" {
		name = "default"
	}
	visible := len(KeyPrefix) + prefixVisible
	if visible > len(plaintext) {
		visible = len(plaintext)
	}
	record := omap.New()
	record.Set("id", keyID)
	record.Set("name", name)
	record.Set("user_login", userLogin)
	record.Set("key_hash", HashKey(plaintext))
	record.Set("key", plaintext)
	record.Set("prefix", plaintext[:visible])
	record.Set("created_at", json.Number(formatFloat(now())))
	record.Set("last_used_at", nil)
	record.Set("request_count", json.Number("0"))
	record.Set("disabled", false)
	if scope != nil && scope.Len() > 0 {
		record.Set("scope", scope.Clone())
	} else {
		defaultScope := omap.New()
		defaultScope.Set("kind", "all")
		record.Set("scope", defaultScope)
	}

	s.mu.Lock()
	// Merge in other writers' entries first, so the whole table is not overwritten.
	s.reloadKeysIfChanged()
	s.keys.Set(keyID, record)
	s.saveKeys()
	s.mu.Unlock()

	return PublicKey(record, true), plaintext
}

// LookupAPIKey finds an enabled key record by its plaintext.
func (s *Store) LookupAPIKey(plaintext string) *omap.Map {
	if plaintext == "" {
		return nil
	}
	digest := HashKey(plaintext)
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		for _, id := range s.keys.Keys() {
			record := s.keys.Map(id)
			if record == nil {
				continue
			}
			if record.Str("key_hash") == digest && !record.Bool("disabled", false) {
				return record.Clone()
			}
		}
		// Missed on the first pass: another worker may have just created this key, so
		// re-read from disk once and retry.
		if attempt == 0 && !s.reloadKeysIfChanged() {
			break
		}
	}
	return nil
}

func (s *Store) TouchAPIKey(keyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.keys.Map(keyID)
	if record == nil {
		return
	}
	record.Set("last_used_at", json.Number(formatFloat(now())))
	record.Set("request_count", json.Number(formatInt(record.Int("request_count", 0)+1)))
	s.saveKeys()
}

// ListAPIKeys returns the public view of every key, newest first. An empty userLogin returns
// every key (the administrator view).
//
// includeSecret is only correct when userLogin names the caller themselves: the cross-user
// administrator view must never carry plaintext.
func (s *Store) ListAPIKeys(userLogin string, includeSecret bool) []*omap.Map {
	s.mu.Lock()
	s.reloadKeysIfChanged()
	records := []*omap.Map{}
	for _, id := range s.keys.Keys() {
		if record := s.keys.Map(id); record != nil {
			records = append(records, record)
		}
	}
	s.mu.Unlock()

	if userLogin != "" {
		filtered := records[:0:0]
		for _, record := range records {
			if record.Str("user_login") == userLogin {
				filtered = append(filtered, record)
			}
		}
		records = filtered
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Float("created_at", 0) > records[j].Float("created_at", 0)
	})
	out := make([]*omap.Map, 0, len(records))
	for _, record := range records {
		out = append(out, PublicKey(record, includeSecret))
	}
	return out
}

func (s *Store) GetAPIKey(keyID string) *omap.Map {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.keys.Map(keyID)
	if record == nil && s.reloadKeysIfChanged() {
		record = s.keys.Map(keyID)
	}
	if record == nil {
		return nil
	}
	return record.Clone()
}

func (s *Store) DeleteAPIKey(keyID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadKeysIfChanged()
	if !s.keys.Has(keyID) {
		return false
	}
	s.keys.Delete(keyID)
	s.saveKeys()
	return true
}

// SetAPIKeyFields applies a validated patch (any of disabled / name / scope) in one write.
//
// One setter rather than three: each of these is a read-modify-write of the same file, and a
// caller changing two fields through two setters would write the table twice and could lose
// the first change to another worker's reload in between.
func (s *Store) SetAPIKeyFields(keyID string, patch *omap.Map) *omap.Map {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadKeysIfChanged()
	record := s.keys.Map(keyID)
	if record == nil {
		return nil
	}
	for _, field := range []string{"disabled", "name", "scope"} {
		if patch.Has(field) {
			record.Set(field, patch.Value(field))
		}
	}
	s.saveKeys()
	return PublicKey(record, false)
}

// PublicKey is the outward-facing representation.
//
// The digest never leaves the process. The plaintext leaves only once the caller has been
// confirmed to be the key's owner, hence includeSecret defaulting to false at every call
// site: a new one has to opt in deliberately rather than leak by omission. Keys created
// before the plaintext was stored simply have no "key" field.
func PublicKey(record *omap.Map, includeSecret bool) *omap.Map {
	out := omap.New()
	for _, field := range record.Keys() {
		if field == "key_hash" || field == "key" {
			continue
		}
		out.Set(field, omap.CloneValue(record.Value(field)))
	}
	// Keys minted before scopes existed have no field; they are unrestricted, and filling it
	// in here means every consumer -- console, enforcement, verify scripts -- reads one shape.
	if !out.Has("scope") {
		scope := omap.New()
		scope.Set("kind", "all")
		out.Set("scope", scope)
	}
	if includeSecret && record.Str("key") != "" {
		out.Set("key", record.Str("key"))
	}
	return out
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func formatInt(v int) string {
	return strconv.Itoa(v)
}
