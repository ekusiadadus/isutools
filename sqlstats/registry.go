package sqlstats

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Purpose selects which credential of a logical target a connection uses.
//
// A TargetID names a logical database; a Purpose names one of the credentials
// that reach it. Keeping them separate is what lets a least-privilege EXPLAIN
// user be introduced without splitting the aggregation key: every consumer
// (row stats, pool stats, query plans, the multi-host agent) still joins on
// the TargetID alone.
type Purpose string

const (
	// PurposeApp is the application's own traffic connection. It is the only
	// source of Display, Schema and Features, and every target has one:
	// either the proxy driver observed it or RegisterDBTarget declared it.
	PurposeApp Purpose = "app"
	// PurposeStats is the connection used for SHOW STATUS/VARIABLES and
	// performance_schema digests.
	PurposeStats Purpose = "stats"
	// PurposeExplain is the least-privilege connection used for EXPLAIN.
	// It never falls back to the application credential: an implicit
	// downgrade to a credential holding DML rights would defeat the point
	// of running EXPLAIN under a restricted user.
	PurposeExplain Purpose = "explain"
)

// purposeOrder fixes the order Purposes are reported in, so snapshots are
// byte-stable across runs.
var purposeOrder = []Purpose{PurposeApp, PurposeStats, PurposeExplain}

// Registry errors. They are sentinels so callers can decide between "skip
// this target" and "fail startup" with errors.Is.
var (
	// ErrInvalidTargetID means the explicit ID is empty, longer than
	// maxTargetIDLen, or contains a byte outside [A-Za-z0-9._-].
	ErrInvalidTargetID = errors.New("isutools: invalid target id")
	// ErrUnknownTarget means the ID was never registered. Consumer APIs
	// accept registered IDs only; they never create targets as a side effect.
	ErrUnknownTarget = errors.New("isutools: unknown target id")
	// ErrDuplicateTarget means the ID is taken by a different database, or
	// the database is already registered under another ID. Colliding targets
	// are rejected rather than merged, because merging would silently sum
	// two databases into one row of every report.
	ErrDuplicateTarget = errors.New("isutools: target id already in use")
	// ErrUnknownDriver means driverName is not registered with database/sql.
	ErrUnknownDriver = errors.New("isutools: driver is not registered")
	// ErrInvalidPurpose means the purpose is not valid for the call.
	ErrInvalidPurpose = errors.New("isutools: invalid purpose")
	// ErrDuplicatePurpose means (target, purpose) already has a credential.
	ErrDuplicatePurpose = errors.New("isutools: purpose already registered")
	// ErrPurposeNotRegistered means the purpose has no credential and no
	// fallback is allowed for it.
	ErrPurposeNotRegistered = errors.New("isutools: purpose not registered")
	// ErrExecNotAllowed means Querier.ExecContext was handed a statement
	// outside the session-settings allowlist.
	ErrExecNotAllowed = errors.New("isutools: exec statement not allowed")
	// ErrUnparsedDSN means the DSN could not be parsed structurally, so the
	// connection hygiene rules cannot be applied to it.
	ErrUnparsedDSN = errors.New("isutools: dsn could not be parsed")
	// ErrTooManyTargets means the registry is at MaxTargets.
	ErrTooManyTargets = errors.New("isutools: too many db targets")
	// ErrDriverFailed reports a failure raised by the database driver itself.
	// The driver's own error is deliberately not wrapped: drivers routinely
	// echo the DSN they were opened with, and everything the registry returns
	// can end up in a health note, a published snapshot or an agent payload.
	// The identifying context is the TargetID, the Purpose and Display, all of
	// which are rebuilt from an allowlist and hold no credential.
	ErrDriverFailed = errors.New("isutools: database driver failed")
)

const (
	// MaxTargets bounds the registry: reports, agent payloads and the
	// inspector handle budget all scale with it.
	MaxTargets = 16
	// maxTargetIDLen bounds explicit IDs. They end up as JSON keys, agent
	// configuration values and HTML headings.
	maxTargetIDLen = 64
	// aliasMaxLen bounds the human-readable prefix of an auto-derived ID so
	// that alias + "-" + hash26 stays within maxTargetIDLen.
	aliasMaxLen = 32
	// inspectorIdleTimeout closes idle inspector connections, so nothing of
	// ours stays connected between benchmark runs.
	inspectorIdleTimeout = 30 * time.Second
	// sessionInitStatement pins inspector sessions to UTC. Consumers read
	// server-side timestamps and compare them with Go time values.
	sessionInitStatement = `SET time_zone = '+00:00'`
)

// idEncoding is RFC 4648 base32 with a lowercase alphabet and no padding, so
// the hash suffix only uses bytes allowed in a target ID.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// TargetInfo is the public, credential-free description of one target.
type TargetInfo struct {
	// ID is the stable TargetID every collector joins on.
	ID string
	// Driver is the real driver name of the application connection (never
	// the proxied "<name>:isutools" variant).
	Driver string
	// Display is rebuilt from an allowlist of DSN fields, so no credential
	// can reach it even if a DSN carries unusual parameters.
	Display string
	// Schema is the application's default database name. It is not a
	// secret, and collectors bind it as a query parameter because their own
	// connections deliberately have no default database.
	Schema string
	// Purposes lists the registered credentials, always starting with
	// PurposeApp.
	Purposes []Purpose
}

// DSNFeatures reports go-sql-driver/mysql DSN attributes of the application
// connection. Advisor checks read it instead of string-matching a DSN they
// are not allowed to see.
type DSNFeatures struct {
	InterpolateParams bool
	ParseTime         bool
	MultiStatements   bool
}

// Rows is the result-set view handed to Inspect callbacks. It is a wrapper,
// not *sql.Rows, so the registry can force-close leaked result sets and keep
// the pinned connection reusable.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Err() error
	Close() error
}

// Row is the single-row view handed to Inspect callbacks.
type Row interface {
	Scan(dest ...any) error
	Err() error
}

// Querier is the restricted database handle passed to Inspect callbacks. It
// deliberately exposes no *sql.DB and no transaction control: a callback must
// not be able to outlive the call or open connections of its own.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) Row
	// ExecContext runs statements that return no result set. It is
	// restricted to session settings (first token SET); anything else
	// returns ErrExecNotAllowed, because an inspection connection must
	// never be able to modify data.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// RegisterDBTarget creates a logical target with an explicit, human-chosen ID
// and registers its PurposeApp credential. Call it before SQLDriverName so
// the proxy driver finds the target already named; otherwise the DSN is
// auto-registered under a derived ID and the explicit call fails.
func RegisterDBTarget(id, driverName, dsn string) error {
	return defaultRegistry.registerTarget(id, driverName, dsn)
}

// RegisterDBInspector attaches a purpose-specific credential to an existing
// target. Only PurposeStats and PurposeExplain are accepted: PurposeApp is
// the target's identity, and replacing it mid-run would change Display,
// Schema and the canonical tuple under the collectors' feet.
func RegisterDBInspector(targetID string, purpose Purpose, driverName, dsn string) error {
	return defaultRegistry.registerInspector(targetID, purpose, driverName, dsn)
}

// Targets returns every registered target ordered by ID.
func Targets() []TargetInfo { return defaultRegistry.targets() }

// Target returns one target by exact (byte-for-byte) ID match.
func Target(id string) (TargetInfo, bool) { return defaultRegistry.target(id) }

// TargetIDForDSN reports the ID currently assigned to the DSN's canonical
// tuple. It never registers anything: it exists so callers can obtain an
// auto-derived ID instead of trying to spell out its hash suffix by hand.
// ok is false for an unparseable, unregistered or ambiguous DSN.
func TargetIDForDSN(driverName, dsn string) (string, bool) {
	return defaultRegistry.targetIDForDSN(driverName, dsn)
}

// Features reports the application DSN's attributes. known is false when the
// DSN could not be parsed as a go-sql-driver/mysql DSN, which callers must
// distinguish from "parsed, and the feature is off".
func Features(id string) (DSNFeatures, bool) { return defaultRegistry.features(id) }

// Inspect runs fn on a dedicated connection of the target's purpose-specific
// credential.
//
// Every call pins its own *sql.Conn: a pool limited to one connection is not
// a session guarantee, and session-local state (time zone, roles) would
// silently be lost across a reconnect. The connection, and any result set fn
// left open, are closed before Inspect returns.
//
// PurposeStats falls back to the application credential when no stats
// credential is registered — normalized by the same hygiene rules — because
// the single-DSN setup must keep working. PurposeExplain never falls back and
// returns ErrPurposeNotRegistered instead.
func Inspect(ctx context.Context, id string, purpose Purpose, fn func(context.Context, Querier) error) error {
	return defaultRegistry.inspect(ctx, id, purpose, fn)
}

// Notes returns the registry's degradation notes (unparsed DSNs, ID
// collisions, exceeded limits). Registration is fail-open: it records a note
// and drops the target instead of breaking the instrumented application.
// Notes never contain a DSN or any error text taken from a driver.
func Notes() []string { return defaultRegistry.notesSnapshot() }

// CloseDBInspectors closes every pooled stats/explain connection. Nothing of
// ours should stay connected to the database between benchmark runs, so a
// shutdown path calls it; the registry stays usable and reopens on demand.
func CloseDBInspectors() { defaultRegistry.closeInspectors() }

// defaultRegistry is the process-wide registry all exported helpers use.
var defaultRegistry = newRegistry()

type credential struct {
	driver string
	dsn    string
	parsed parsedDSN
}

type target struct {
	id    string
	tuple string
	// explicit records whether the app credential came from
	// RegisterDBTarget; auto-derived targets must not be renamed later.
	explicit   bool
	creds      map[Purpose]credential
	inspectors map[Purpose]*sql.DB
}

// tupleEntry maps a canonical tuple to its target. ambiguous is set when two
// explicitly named targets share one tuple, which is legal (the operator
// wanted separate rows) but makes reverse lookup meaningless.
type tupleEntry struct {
	target    *target
	ambiguous bool
}

type registry struct {
	mu       sync.Mutex
	byID     map[string]*target
	byTuple  map[string]*tupleEntry
	observed map[string]struct{}
	noteSeen map[string]struct{}
	notes    []string
}

func newRegistry() *registry {
	return &registry{
		byID:     map[string]*target{},
		byTuple:  map[string]*tupleEntry{},
		observed: map[string]struct{}{},
		noteSeen: map[string]struct{}{},
	}
}

// note records a degradation reason once. Notes never contain a DSN.
func (r *registry) note(message string) {
	if _, seen := r.noteSeen[message]; seen {
		return
	}
	r.noteSeen[message] = struct{}{}
	r.notes = append(r.notes, message)
}

// addNote records a degradation reason without holding r.mu already.
func (r *registry) addNote(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note(message)
}

func (r *registry) notesSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.notes))
	copy(out, r.notes)
	return out
}

func (r *registry) registerTarget(id, driverName, dsn string) error {
	if err := validateTargetID(id); err != nil {
		return err
	}
	driverName = baseDriverName(driverName)
	if !driverRegistered(driverName) {
		return fmt.Errorf("%w: %q", ErrUnknownDriver, driverName)
	}
	parsed, parsedOK := parseDSN(driverName, dsn)

	r.mu.Lock()
	defer r.mu.Unlock()

	tuple := ""
	if parsedOK {
		tuple = parsed.tuple(driverName)
	}
	if existing := r.byID[id]; existing != nil {
		if tuple != "" && existing.tuple == tuple && existing.creds[PurposeApp].driver == driverName {
			return nil // same database re-registered: idempotent
		}
		return fmt.Errorf("%w: %q is already registered for a different database or an unparsed DSN", ErrDuplicateTarget, id)
	}
	entry := r.byTuple[tuple]
	if tuple != "" && entry != nil && !entry.target.explicit {
		return fmt.Errorf("%w: this database was already auto-registered as %q; call RegisterDBTarget before SQLDriverName",
			ErrDuplicateTarget, entry.target.id)
	}
	if len(r.byID) >= MaxTargets {
		r.note(fmt.Sprintf("db target %q dropped: at most %d targets are supported", id, MaxTargets))
		return fmt.Errorf("%w: limit is %d", ErrTooManyTargets, MaxTargets)
	}
	r.insert(&target{
		id:         id,
		tuple:      tuple,
		explicit:   true,
		creds:      map[Purpose]credential{PurposeApp: {driver: driverName, dsn: dsn, parsed: parsed}},
		inspectors: map[Purpose]*sql.DB{},
	})
	if !parsedOK {
		r.note(fmt.Sprintf("db target %q: unparsed DSN — display, schema and inspection are unavailable", id))
	} else if parsed.database == "" {
		r.note(fmt.Sprintf("db target %q: no default schema — include a database in the DSN passed to RegisterDBTarget", id))
	}
	return nil
}

// insert adds t to both indexes. The caller holds r.mu.
func (r *registry) insert(t *target) {
	r.byID[t.id] = t
	if t.tuple == "" {
		return
	}
	if entry := r.byTuple[t.tuple]; entry != nil {
		entry.ambiguous = true
		return
	}
	r.byTuple[t.tuple] = &tupleEntry{target: t}
}

// observeDSN registers the DSN of an application connection opened through a
// proxied driver. It is fail-open by construction: measurement must never
// break the application's own Open call.
func observeDSN(driverName, dsn string) { defaultRegistry.observe(driverName, dsn) }

func (r *registry) observe(driverName, dsn string) {
	defer func() { _ = recover() }()

	driverName = baseDriverName(driverName)
	key := driverName + "\x00" + dsn

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.observed[key]; seen {
		return
	}
	r.observed[key] = struct{}{}

	parsed, ok := parseDSN(driverName, dsn)
	if !ok {
		r.note(fmt.Sprintf("driver %q: unparsed DSN observed — call RegisterDBTarget to measure this database", driverName))
		return
	}
	tuple := parsed.tuple(driverName)
	if _, exists := r.byTuple[tuple]; exists {
		return // already registered explicitly or by an earlier connection
	}
	id := autoTargetID(driverName, parsed)
	if _, taken := r.byID[id]; taken {
		r.note(fmt.Sprintf("db target id %q collides with a different database — name them with RegisterDBTarget", id))
		return
	}
	if len(r.byID) >= MaxTargets {
		r.note(fmt.Sprintf("db target %q dropped: at most %d targets are supported", id, MaxTargets))
		return
	}
	r.insert(&target{
		id:         id,
		tuple:      tuple,
		creds:      map[Purpose]credential{PurposeApp: {driver: driverName, dsn: dsn, parsed: parsed}},
		inspectors: map[Purpose]*sql.DB{},
	})
}

func (r *registry) registerInspector(targetID string, purpose Purpose, driverName, dsn string) error {
	if purpose != PurposeStats && purpose != PurposeExplain {
		return fmt.Errorf("%w: %q (only %q and %q can be attached to an existing target)",
			ErrInvalidPurpose, purpose, PurposeStats, PurposeExplain)
	}
	driverName = baseDriverName(driverName)
	if !driverRegistered(driverName) {
		return fmt.Errorf("%w: %q", ErrUnknownDriver, driverName)
	}
	parsed, ok := parseDSN(driverName, dsn)
	if !ok {
		// Without a structural parse the default database cannot be
		// stripped, so this connection would pollute the measured schema.
		return fmt.Errorf("%w: inspector DSNs must be parseable so the default database can be removed", ErrUnparsedDSN)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byID[targetID]
	if t == nil {
		return fmt.Errorf("%w: %q", ErrUnknownTarget, targetID)
	}
	if _, exists := t.creds[purpose]; exists {
		return fmt.Errorf("%w: %q already has a %q credential", ErrDuplicatePurpose, targetID, purpose)
	}
	t.creds[purpose] = credential{driver: driverName, dsn: dsn, parsed: parsed}
	return nil
}

func (r *registry) targets() []TargetInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TargetInfo, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *registry) target(id string) (TargetInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byID[id]
	if t == nil {
		return TargetInfo{}, false
	}
	return t.info(), true
}

func (r *registry) targetIDForDSN(driverName, dsn string) (string, bool) {
	driverName = baseDriverName(driverName)
	parsed, ok := parseDSN(driverName, dsn)
	if !ok {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.byTuple[parsed.tuple(driverName)]
	if entry == nil || entry.ambiguous {
		return "", false
	}
	return entry.target.id, true
}

func (r *registry) features(id string) (DSNFeatures, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byID[id]
	if t == nil {
		return DSNFeatures{}, false
	}
	app := t.creds[PurposeApp]
	if app.parsed.form != formMySQL {
		return DSNFeatures{}, false
	}
	return app.parsed.features, true
}

// info renders the credential-free view. The caller holds r.mu.
func (t *target) info() TargetInfo {
	app := t.creds[PurposeApp]
	purposes := make([]Purpose, 0, len(t.creds))
	for _, p := range purposeOrder {
		if _, ok := t.creds[p]; ok {
			purposes = append(purposes, p)
		}
	}
	return TargetInfo{
		ID:       t.id,
		Driver:   app.driver,
		Display:  app.parsed.display(),
		Schema:   app.parsed.database,
		Purposes: purposes,
	}
}

func (r *registry) inspect(ctx context.Context, id string, purpose Purpose, fn func(context.Context, Querier) error) error {
	if fn == nil {
		return errors.New("isutools: Inspect requires a callback")
	}
	h, err := r.inspector(id, purpose)
	if err != nil {
		return err
	}
	conn, err := h.db.Conn(ctx)
	if err != nil {
		r.addNote(fmt.Sprintf("db target %q: the %s connection could not be established", id, purpose))
		return h.errs.wrap("pin connection", err)
	}
	defer func() { _ = conn.Close() }()
	if h.mysql {
		if _, err := conn.ExecContext(ctx, sessionInitStatement); err != nil {
			r.addNote(fmt.Sprintf("db target %q: the %s session could not be initialised", id, purpose))
			return h.errs.wrap("initialise session", err)
		}
	}
	q := &connQuerier{conn: conn, open: map[*trackedRows]struct{}{}, errs: h.errs}
	defer q.finish()
	return fn(ctx, q)
}

// inspectorHandle is a resolved inspector: the pooled connection, whether it
// speaks the MySQL DSN dialect (which is what makes the session-init statement
// applicable), and the redactor that keeps this credential's DSN out of every
// error the connection produces.
type inspectorHandle struct {
	db    *sql.DB
	mysql bool
	errs  driverErrors
}

// inspector resolves the credential for (id, purpose) and returns its pooled
// handle, creating it on first use.
func (r *registry) inspector(id string, purpose Purpose) (inspectorHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byID[id]
	if t == nil {
		return inspectorHandle{}, fmt.Errorf("%w: %q", ErrUnknownTarget, id)
	}
	if purpose != PurposeStats && purpose != PurposeExplain {
		return inspectorHandle{}, fmt.Errorf("%w: Inspect accepts %q or %q, got %q",
			ErrInvalidPurpose, PurposeStats, PurposeExplain, purpose)
	}
	cred, ok := t.creds[purpose]
	if !ok {
		if purpose == PurposeExplain {
			return inspectorHandle{}, fmt.Errorf("%w: %q has no %q credential", ErrPurposeNotRegistered, id, purpose)
		}
		cred = t.creds[PurposeApp]
	}
	dsn, note, err := cred.parsed.inspectorDSN(cred.dsn)
	if err != nil {
		return inspectorHandle{}, fmt.Errorf("isutools: inspector dsn for %q/%s: %w", id, purpose, err)
	}
	// The redactor is built before anything can fail, so every error below is
	// already covered by it. It is rebuilt on the cached path too: it costs a
	// string build and it is the only thing standing between a chatty driver
	// and a published snapshot.
	h := inspectorHandle{
		mysql: cred.parsed.form == formMySQL,
		errs:  newDriverErrors(id, purpose, t.info().Display, cred, dsn),
	}
	if db := t.inspectors[purpose]; db != nil {
		h.db = db
		return h, nil
	}
	if note != "" {
		r.note(fmt.Sprintf("db target %q: %s", id, note))
	}
	db, err := sql.Open(cred.driver, dsn)
	if err != nil {
		r.note(fmt.Sprintf("db target %q: the %s connection could not be opened", id, purpose))
		return inspectorHandle{}, h.errs.wrap("open", err)
	}
	// One connection per (target, purpose), closed while idle: inspection
	// must not hold connections the application could be using.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(inspectorIdleTimeout)
	t.inspectors[purpose] = db
	h.db = db
	return h, nil
}

// closeInspectors releases every pooled inspector handle. The registry itself
// stays usable and reopens on demand.
func (r *registry) closeInspectors() {
	r.mu.Lock()
	handles := make([]*sql.DB, 0, len(r.byID))
	for _, t := range r.byID {
		for purpose, db := range t.inspectors {
			handles = append(handles, db)
			delete(t.inspectors, purpose)
		}
	}
	r.mu.Unlock()
	for _, db := range handles {
		_ = db.Close()
	}
}

// maxDriverErrDetail bounds the driver text kept in a registry error. Errors
// travel into notes and snapshots, and a driver that dumps a whole statement
// or result row into its message must not be able to inflate them.
const maxDriverErrDetail = 200

// driverErrors turns a driver's own error into one the registry may publish.
//
// The DSN is the problem: it carries the password, and drivers echo it in
// dial failures, handshake failures and DSN parse failures alike. Rather than
// trusting each driver's discretion, the message is only kept when it provably
// mentions none of this credential's secrets; otherwise the identifying
// context (target, purpose and the allowlist-rebuilt Display) is all that is
// reported. Redacting a useful message is a smaller loss than leaking a
// password into a snapshot someone pastes into a chat window.
type driverErrors struct {
	id      string
	purpose Purpose
	display string
	// secrets are lowercased credential-derived strings, none of which may
	// appear in a published error.
	secrets []string
}

func newDriverErrors(id string, purpose Purpose, display string, cred credential, inspectorDSN string) driverErrors {
	d := driverErrors{id: id, purpose: purpose, display: display}
	password := cred.parsed.password
	for _, secret := range []string{
		cred.dsn,
		inspectorDSN,
		password,
		// URL-form DSNs carry the password escaped, so the decoded form alone
		// would miss an echo of the DSN's own bytes.
		url.QueryEscape(password),
		url.PathEscape(password),
		cred.parsed.user + ":" + password,
	} {
		if secret != "" && secret != ":" {
			d.secrets = append(d.secrets, strings.ToLower(secret))
		}
	}
	return d
}

// wrap maps a driver error onto ErrDriverFailed. A nil error stays nil.
func (d driverErrors) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	// Control-flow sentinels are returned verbatim: callers switch on them
	// with errors.Is, and their messages are compile-time constants that no
	// driver can inject a DSN into. The driver's own wrapper around them is
	// dropped, because that wrapper is exactly where a DSN would sit.
	if sentinel := passthroughSentinel(err); sentinel != nil {
		return sentinel
	}
	detail := "driver message withheld: it repeated the connection string"
	if message := collapseSpace(err.Error()); !d.mentionsSecret(message) {
		detail = truncateRunes(message, maxDriverErrDetail)
	}
	return fmt.Errorf("%w: %s for %q/%s (%s): %s", ErrDriverFailed, op, d.id, d.purpose, d.display, detail)
}

// mentionsSecret reports whether a driver message echoes any credential
// material. The comparison is case-insensitive so a driver that upper-cases
// part of a DSN cannot slip past it.
func (d driverErrors) mentionsSecret(message string) bool {
	lower := strings.ToLower(message)
	for _, secret := range d.secrets {
		if strings.Contains(lower, secret) {
			return true
		}
	}
	return false
}

// passthroughSentinel returns the well-known sentinel err stands for, or nil.
func passthroughSentinel(err error) error {
	for _, sentinel := range []error{
		sql.ErrNoRows,
		sql.ErrTxDone,
		sql.ErrConnDone,
		driver.ErrBadConn,
		context.Canceled,
		context.DeadlineExceeded,
		io.EOF,
	} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return nil
}

// collapseSpace folds all whitespace runs into single spaces so a multi-line
// driver message cannot break the one-line note and log formats.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateRunes cuts s to at most limit bytes without splitting a rune, so the
// result stays valid UTF-8 and can be marshalled into JSON.
func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// connQuerier is the Querier implementation bound to one pinned connection.
type connQuerier struct {
	conn *sql.Conn
	errs driverErrors
	mu   sync.Mutex
	open map[*trackedRows]struct{}
	done bool
}

var errQuerierDone = errors.New("isutools: querier used after Inspect returned")

func (q *connQuerier) QueryContext(ctx context.Context, query string, args ...any) (Rows, error) {
	q.mu.Lock()
	done := q.done
	q.mu.Unlock()
	if done {
		return nil, errQuerierDone
	}
	rows, err := q.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, q.errs.wrap("inspect query", err)
	}
	tracked := &trackedRows{rows: rows, owner: q}
	q.mu.Lock()
	if q.done {
		q.mu.Unlock()
		_ = rows.Close()
		return nil, errQuerierDone
	}
	q.open[tracked] = struct{}{}
	q.mu.Unlock()
	return tracked, nil
}

func (q *connQuerier) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	q.mu.Lock()
	done := q.done
	q.mu.Unlock()
	if done {
		return errRow{err: errQuerierDone}
	}
	return connRow{row: q.conn.QueryRowContext(ctx, query, args...), errs: q.errs}
}

func (q *connQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q.mu.Lock()
	done := q.done
	q.mu.Unlock()
	if done {
		return nil, errQuerierDone
	}
	if err := allowExec(query); err != nil {
		return nil, err
	}
	res, err := q.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, q.errs.wrap("inspect exec", err)
	}
	return res, nil
}

// finish closes every result set the callback left open. Without it a leaked
// *sql.Rows would keep the pinned connection busy and the next Inspect would
// block until its context expired.
func (q *connQuerier) finish() {
	q.mu.Lock()
	q.done = true
	leftover := make([]*trackedRows, 0, len(q.open))
	for tracked := range q.open {
		leftover = append(leftover, tracked)
	}
	q.open = nil
	q.mu.Unlock()
	for _, tracked := range leftover {
		_ = tracked.Close()
	}
}

func (q *connQuerier) forget(tracked *trackedRows) {
	q.mu.Lock()
	if q.open != nil {
		delete(q.open, tracked)
	}
	q.mu.Unlock()
}

// trackedRows lets the registry close result sets the callback forgot.
type trackedRows struct {
	rows  *sql.Rows
	owner *connQuerier
	once  sync.Once
}

// The result-set methods redact as well: a scan or a late row error is still a
// driver error, and it reaches exactly the same health and snapshot output as
// a failed query does.
func (t *trackedRows) Next() bool { return t.rows.Next() }
func (t *trackedRows) Scan(dest ...any) error {
	return t.owner.errs.wrap("inspect scan", t.rows.Scan(dest...))
}
func (t *trackedRows) Err() error { return t.owner.errs.wrap("inspect rows", t.rows.Err()) }
func (t *trackedRows) Columns() ([]string, error) {
	columns, err := t.rows.Columns()
	return columns, t.owner.errs.wrap("inspect columns", err)
}

func (t *trackedRows) Close() error {
	var err error
	t.once.Do(func() {
		err = t.owner.errs.wrap("inspect rows close", t.rows.Close())
		t.owner.forget(t)
	})
	return err
}

// connRow is the single-row view. It exists so that Scan's driver error is
// redacted like every other one instead of travelling out raw.
type connRow struct {
	row  *sql.Row
	errs driverErrors
}

func (r connRow) Scan(dest ...any) error { return r.errs.wrap("inspect row scan", r.row.Scan(dest...)) }
func (r connRow) Err() error             { return r.errs.wrap("inspect row", r.row.Err()) }

// errRow reports a failure that happened before the query could be issued.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }
func (r errRow) Err() error        { return r.err }

// allowExec implements the ExecContext allowlist: session settings only.
// Statement batches are rejected as well, so a "SET" prefix cannot be used to
// smuggle a second statement past the check.
func allowExec(query string) error {
	body := strings.TrimSpace(query)
	body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	if strings.Contains(body, ";") {
		return fmt.Errorf("%w: statement batches are not allowed", ErrExecNotAllowed)
	}
	token := firstToken(body)
	if !strings.EqualFold(token, "SET") {
		return fmt.Errorf("%w: only SET statements are allowed, got %q", ErrExecNotAllowed, token)
	}
	return nil
}

func firstToken(statement string) string {
	for i := 0; i < len(statement); i++ {
		switch statement[i] {
		case ' ', '\t', '\n', '\r', '(', ';':
			return statement[:i]
		}
	}
	return statement
}

// validateTargetID enforces the explicit-ID rules. IDs are compared byte for
// byte everywhere: no case folding, no Unicode normalization, no trimming.
// Restricting them to ASCII is what makes that comparison unambiguous.
func validateTargetID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: id must not be empty", ErrInvalidTargetID)
	}
	if len(id) > maxTargetIDLen {
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidTargetID, len(id), maxTargetIDLen)
	}
	for i := 0; i < len(id); i++ {
		if !isTargetIDByte(id[i]) {
			return fmt.Errorf("%w: byte %d is not in [A-Za-z0-9._-]", ErrInvalidTargetID, i)
		}
	}
	return nil
}

func isTargetIDByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '-':
		return true
	default:
		return false
	}
}

// baseDriverName strips the measuring suffix, so callers may pass either the
// name they registered or the name they opened.
func baseDriverName(name string) string { return strings.TrimSuffix(name, DriverSuffix) }

func driverRegistered(name string) bool {
	for _, n := range sql.Drivers() {
		if n == name {
			return true
		}
	}
	return false
}

// autoTargetID derives a connection-order independent ID: a readable alias
// plus a 128 bit fingerprint of the canonical tuple. The fingerprint is what
// keeps two databases that slug identically apart.
func autoTargetID(driverName string, p parsedDSN) string {
	return aliasFor(driverName, p) + "-" + hash26(p.tuple(driverName))
}

func aliasFor(driverName string, p parsedDSN) string {
	return slug(driverName + "-" + p.addr + "-" + p.database)
}

func hash26(tuple string) string {
	sum := sha256.Sum256([]byte(tuple))
	return idEncoding.EncodeToString(sum[:16])
}

// slug lowercases s, replaces every byte outside [a-z0-9._-] with "_",
// collapses runs of "_" and truncates to aliasMaxLen.
func slug(s string) string {
	var b strings.Builder
	var last byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if !isTargetIDByte(c) {
			if last == '_' {
				continue
			}
			c = '_'
		}
		b.WriteByte(c)
		last = c
	}
	out := b.String()
	if len(out) > aliasMaxLen {
		out = out[:aliasMaxLen]
	}
	out = strings.TrimRight(out, "_-.")
	if out == "" {
		out = "db"
	}
	return out
}

// dsnForm distinguishes the DSN dialects the registry can parse.
type dsnForm int

const (
	formUnparsed dsnForm = iota
	// formMySQL is [user[:pass]@][net[(addr)]]/dbname[?params].
	formMySQL
	// formURL is scheme://[user[:pass]@]host[:port]/dbname[?params].
	formURL
)

// parsedDSN is the structured view of a DSN. Credentials are kept only to
// re-format inspector DSNs; nothing outside this file reads them, and no
// public value is ever derived from them.
type parsedDSN struct {
	form     dsnForm
	scheme   string // URL form only
	user     string
	password string
	network  string // "tcp" | "unix" | driver-specific
	rawNet   string // network exactly as written ("" when omitted)
	rawAddr  string // address exactly as written ("" when omitted)
	addr     string // canonical address, safe to display
	database string
	params   map[string]string
	features DSNFeatures
}

// parseDSN structures a DSN, preferring the URL form when the string actually
// carries a host. Unparseable DSNs are reported rather than guessed at: a
// wrong guess would put credentials into a display string.
func parseDSN(driverName, dsn string) (parsedDSN, bool) {
	if strings.Contains(dsn, "://") {
		if p, ok := parseURLDSN(driverName, dsn); ok {
			return p, true
		}
	}
	return parseMySQLDSN(dsn)
}

// parseMySQLDSN parses the go-sql-driver/mysql DSN grammar
// [user[:pass]@][net[(addr)]]/dbname[?param=value].
func parseMySQLDSN(dsn string) (parsedDSN, bool) {
	slash, depth := -1, 0
	for i := len(dsn) - 1; i >= 0 && slash < 0; i-- {
		switch dsn[i] {
		case ')':
			depth++
		case '(':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				slash = i
			}
		}
	}
	if slash < 0 {
		return parsedDSN{}, false
	}
	p := parsedDSN{form: formMySQL, network: "tcp", params: map[string]string{}}

	left := dsn[:slash]
	if at := strings.LastIndex(left, "@"); at >= 0 {
		user, password, hasPassword := strings.Cut(left[:at], ":")
		p.user = user
		if hasPassword {
			p.password = password
		}
		left = left[at+1:]
	}
	if open := strings.Index(left, "("); open >= 0 {
		if !strings.HasSuffix(left, ")") {
			return parsedDSN{}, false
		}
		p.rawNet, p.rawAddr = left[:open], left[open+1:len(left)-1]
	} else {
		p.rawNet = left
	}
	if p.rawNet != "" {
		// A network token that is not an identifier means the string is not
		// really a MySQL DSN (a bare "user:pass/db", say). Rejecting it here
		// is what keeps credentials out of Display.
		if !isNetworkToken(p.rawNet) {
			return parsedDSN{}, false
		}
		p.network = p.rawNet
	}

	right := dsn[slash+1:]
	if q := strings.Index(right, "?"); q >= 0 {
		p.database = right[:q]
		p.params = parseParams(right[q+1:])
	} else {
		p.database = right
	}
	if strings.ContainsAny(p.database, "@()/?") {
		return parsedDSN{}, false
	}
	p.addr = canonicalAddr(p.network, p.rawAddr, "3306")
	p.features = featuresFrom(p.params)
	return p, true
}

// parseURLDSN parses scheme://[user[:pass]@]host[:port]/dbname?params, the
// form pgx and lib/pq accept.
func parseURLDSN(driverName, dsn string) (parsedDSN, bool) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return parsedDSN{}, false
	}
	p := parsedDSN{
		form:    formURL,
		scheme:  strings.ToLower(u.Scheme),
		network: "tcp",
		params:  map[string]string{},
	}
	if u.User != nil {
		p.user = u.User.Username()
		p.password, _ = u.User.Password()
	}
	p.rawAddr = u.Host
	p.addr = canonicalAddr("tcp", u.Host, defaultPort(driverName, p.scheme))
	p.database = strings.TrimPrefix(u.Path, "/")
	for key, values := range u.Query() {
		if len(values) > 0 {
			p.params[key] = values[0]
		}
	}
	return p, true
}

func isNetworkToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return s != ""
}

func parseParams(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		// Values are kept verbatim so re-formatting round-trips exactly.
		key, value, _ := strings.Cut(pair, "=")
		if key != "" {
			out[key] = value
		}
	}
	return out
}

func featuresFrom(params map[string]string) DSNFeatures {
	return DSNFeatures{
		InterpolateParams: boolParam(params, "interpolateParams"),
		ParseTime:         boolParam(params, "parseTime"),
		MultiStatements:   boolParam(params, "multiStatements"),
	}
}

func boolParam(params map[string]string, key string) bool {
	value, ok := params[key]
	if !ok {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}

func defaultPort(driverName, scheme string) string {
	name := strings.ToLower(driverName + " " + scheme)
	switch {
	case strings.Contains(name, "mysql"), strings.Contains(name, "maria"):
		return "3306"
	case strings.Contains(name, "postgres"), strings.Contains(name, "pgx"), strings.Contains(name, "pq"):
		return "5432"
	default:
		return ""
	}
}

// canonicalAddr normalizes an address so the same endpoint written in
// different ways yields the same target: lowercase host, explicit port, the
// canonical form of an IP literal, and a cleaned socket path.
func canonicalAddr(network, addr, defPort string) string {
	if strings.HasPrefix(network, "unix") {
		if addr == "" {
			addr = "/tmp/mysql.sock"
		}
		return filepath.Clean(addr)
	}
	if addr == "" {
		if defPort == "" {
			return "127.0.0.1"
		}
		return "127.0.0.1:" + defPort
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = strings.Trim(addr, "[]"), defPort
	}
	host = strings.ToLower(host)
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// tuple is the canonical identity of a target: no credentials, no driver
// parameters — only what decides which database bytes end up in.
func (p parsedDSN) tuple(driverName string) string {
	return strings.Join([]string{driverName, p.network, p.addr, p.database}, "\x00")
}

// display rebuilds a human-readable endpoint from an allowlist of fields
// (network, address, database). Because it is a rebuild rather than a
// redaction, an unknown or oddly escaped DSN parameter cannot leak into it.
func (p parsedDSN) display() string {
	switch p.form {
	case formMySQL:
		return p.network + "(" + p.addr + ")/" + p.database
	case formURL:
		return p.scheme + "://" + p.addr + "/" + p.database
	default:
		return "(unparsed dsn)"
	}
}

// inspectorParams are the connection hygiene overrides applied to every
// stats/explain DSN. It is read-only; callers copy it.
var inspectorParams = map[string]string{
	// No default database: statements from a connection without one are
	// recorded against a NULL schema, so collector queries cannot show up
	// as digests of the application's schema. That is this map's reason to
	// exist; the entries below keep the rest of the session predictable.
	"multiStatements":   "false", // a batch must never ride on an inspection connection
	"interpolateParams": "false", // schema names are passed as bound parameters
	"parseTime":         "true",  // server timestamps arrive as time.Time
	"loc":               "UTC",
	"timeout":           "1s", // an inspector must not stall a benchmark run
	"readTimeout":       "2s",
	"writeTimeout":      "2s",
}

// inspectorDSN rebuilds the DSN used for stats and explain connections. The
// main effect is dropping the default database; see inspectorParams.
// The returned note is a non-fatal degradation to record in health.
func (p parsedDSN) inspectorDSN(original string) (dsn, note string, err error) {
	switch p.form {
	case formMySQL:
		params := make(map[string]string, len(p.params)+len(inspectorParams))
		for key, value := range p.params {
			params[key] = value
		}
		for key, value := range inspectorParams {
			params[key] = value
		}
		var b strings.Builder
		if p.user != "" || p.password != "" {
			b.WriteString(p.user)
			if p.password != "" {
				b.WriteString(":")
				b.WriteString(p.password)
			}
			b.WriteString("@")
		}
		if p.rawNet != "" {
			b.WriteString(p.rawNet)
		} else if p.rawAddr != "" {
			b.WriteString(p.network)
		}
		if p.rawAddr != "" {
			b.WriteString("(")
			b.WriteString(p.rawAddr)
			b.WriteString(")")
		}
		b.WriteString("/")
		if encoded := encodeParams(params); encoded != "" {
			b.WriteString("?")
			b.WriteString(encoded)
		}
		return b.String(), "", nil
	case formURL:
		return original, "driver cannot omit the default database; inspection queries are attributed to the application schema", nil
	default:
		return "", "", fmt.Errorf("%w: register this target with RegisterDBTarget to inspect it", ErrUnparsedDSN)
	}
}

func encodeParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	return strings.Join(parts, "&")
}
