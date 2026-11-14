package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/version"
)

// copyIdleTimeout is how long a copy serves no call before this replica
// releases it, and copyIdleCheck how often the replica looks for such copies.
const (
	copyIdleTimeout = 30 * time.Minute
	copyIdleCheck   = time.Minute
)

// Limits of a stored upstream session.
const (
	maxUpstreamSessionIDBytes = 1024
	maxInitializeResultBytes  = 16 << 10
)

// errUpstreamSessionInvalid is the connect error of a server whose session ID
// or initialize result can't be stored.
var errUpstreamSessionInvalid = errors.New("the server's session can't be stored")

// errRemoteNotOwner refuses a forwarding session to a remote server's
// instance for anyone but its owner: the session sends the owner's
// credential.
var errRemoteNotOwner = errors.New("a remote server's instance serves only its owner")

// errNoSecretSource is the connect error of a remote server on a gateway
// that can't read user secrets.
var errNoSecretSource = errors.New("the gateway can't read user secrets")

// upstreamCopy is this replica's go-sdk client session for an upstream
// session. Other replicas can hold copies of the same upstream session, so
// closing a copy sends the DELETE that ends the upstream session only
// through End.
type upstreamCopy struct {
	session       *mcp.ClientSession
	deleteOnClose *atomic.Bool
	// remote holds the owner's headers of a remote server's copy, and
	// records whether the server refused them. It is nil otherwise.
	remote *runtime.RemoteAuth
	// sessionPk is the toolgate session of the copy's row, or zero for an
	// instance's one copy (a sessionless or 2026-07-28 server).
	sessionPk int64
	// inFlight counts the calls that hold the copy, and lastUsed is when the
	// last of them finished, in Unix nanoseconds.
	inFlight atomic.Int64
	lastUsed atomic.Int64

	closeOnce sync.Once
}

// done records that a call holding the copy finished at now.
func (c *upstreamCopy) done(now time.Time) {
	c.lastUsed.Store(now.UnixNano())
	c.inFlight.Add(-1)
}

// Release closes the copy and leaves the upstream session alive.
func (c *upstreamCopy) Release() {
	c.close(false)
}

// End closes the copy and ends the upstream session: the go-sdk sends
// DELETE.
func (c *upstreamCopy) End() {
	c.close(true)
}

func (c *upstreamCopy) close(end bool) {
	c.closeOnce.Do(func() {
		c.deleteOnClose.Store(end)
		_ = c.session.Close()
	})
}

// copyKey keys a replica's copies: the instance and the upstream session ID,
// empty for an instance's one copy.
type copyKey struct {
	instance string
	upstream string
}

// pairKey is an (instance, toolgate session) pair.
type pairKey struct {
	instance  string
	sessionPk int64
}

// copyEntry holds a copy while it is made, so concurrent forwards make one.
// users, guarded by upstreams.mu, counts the forwards using the entry, so an
// empty entry is removed only when no forward waits on it.
type copyEntry struct {
	users int

	mu   sync.Mutex
	copy *upstreamCopy
}

// pairLock lets one forward of a pair start its upstream session at a time.
type pairLock struct {
	mu    sync.Mutex
	users int
}

// upstreams keeps this replica's copies of upstream sessions: one for each
// stored (instance, toolgate session) pair this replica forwarded, and one
// for each sessionless or 2026-07-28 instance.
type upstreams struct {
	endpoint   UpstreamEndpoint
	httpClient *http.Client
	// secrets returns a principal's secrets, which fill a remote server's
	// headers. Nil means remote servers can't be reached.
	secrets func(principal string) runtime.SecretSource
	logger  *slog.Logger
	// clock is the clock idle copies are measured with; nil means
	// time.Now.
	clock atomic.Pointer[func() time.Time]

	mu     sync.Mutex
	copies map[copyKey]*copyEntry
	// sessionless holds the instances whose server answered initialize
	// with no session ID.
	sessionless map[string]bool
	pairs       map[pairKey]*pairLock
}

func newUpstreams(endpoint UpstreamEndpoint, httpClient *http.Client, secrets func(principal string) runtime.SecretSource, logger *slog.Logger) *upstreams {
	return &upstreams{
		endpoint: endpoint, httpClient: httpClient, secrets: secrets, logger: logger,
		copies: make(map[copyKey]*copyEntry), sessionless: make(map[string]bool), pairs: make(map[pairKey]*pairLock),
	}
}

// forwardingCopy is the copy a call is forwarded on. When once is set, the
// copy belongs to no stored row: the call is forwarded on it and then it is
// ended. row is the ID of the copy's stored row, or zero when it has none.
type forwardingCopy struct {
	*upstreamCopy

	key  copyKey
	row  int64
	once bool
}

// acquire returns the copy a call of the pair (instance, sessionPk) is
// forwarded on, making it when this replica has none.
func (u *upstreams) acquire(ctx context.Context, database *pgxpool.Pool, instance db.Instance, template *v1alpha1.MCPServerTemplate, caller runtime.Identity, sessionPk int64) (forwardingCopy, error) {
	// An instance is keyed by its ID. A per-user or per-session remote
	// server's instance belongs to one user: only that user's calls may use
	// its headers. A shared one's headers hold nothing per-user, so it
	// serves everyone.
	if template.Spec.Remote != nil && instance.Scope != catalog.ScopeShared &&
		(instance.OwnerPrincipal == "" || caller.PrincipalID != instance.OwnerPrincipal) {
		return forwardingCopy{}, errRemoteNotOwner
	}

	instanceKey := copyKey{instance: instance.ID}

	if u.instanceScoped(instance) {
		held, err := u.copyFor(instanceKey, func() (*upstreamCopy, error) {
			return u.connect(ctx, instance, template, caller, nil)
		})

		return forwardingCopy{upstreamCopy: held, key: instanceKey}, err
	}

	queries := db.New(database)
	pair := db.GetLiveUpstreamSessionParams{InstanceID: instance.ID, SessionPk: sessionPk}

	row, err := queries.GetLiveUpstreamSession(ctx, pair)
	if err == nil {
		return u.adopt(ctx, instance, template, caller, sessionPk, row)
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return forwardingCopy{}, fmt.Errorf("read upstream session: %w", err)
	}

	unlock := u.lockPair(pairKey{instance: instance.ID, sessionPk: sessionPk})
	defer unlock()

	// Another forward of the pair on this replica may have stored a row,
	// or learned the server is sessionless, while this one waited.
	if u.instanceScoped(instance) {
		held, err := u.copyFor(instanceKey, func() (*upstreamCopy, error) {
			return u.connect(ctx, instance, template, caller, nil)
		})

		return forwardingCopy{upstreamCopy: held, key: instanceKey}, err
	}

	row, err = queries.GetLiveUpstreamSession(ctx, pair)
	if err == nil {
		return u.adopt(ctx, instance, template, caller, sessionPk, row)
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return forwardingCopy{}, fmt.Errorf("read upstream session: %w", err)
	}

	return u.start(ctx, database, instance, template, caller, sessionPk)
}

// instanceScoped reports whether instance has one copy on this replica and
// no rows: its server has no sessions.
func (u *upstreams) instanceScoped(instance db.Instance) bool {
	if instance.McpRevision == "" || instance.McpRevision >= runtime.RevisionStateless {
		return true
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	return u.sessionless[instance.ID]
}

// lockPair waits until no other forward of pair on this replica starts its
// upstream session, and returns the unlock.
func (u *upstreams) lockPair(pair pairKey) func() {
	u.mu.Lock()

	lock, found := u.pairs[pair]
	if !found {
		lock = &pairLock{}
		u.pairs[pair] = lock
	}

	lock.users++
	u.mu.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()

		u.mu.Lock()
		defer u.mu.Unlock()

		lock.users--
		if lock.users == 0 {
			delete(u.pairs, pair)
		}
	}
}

// now reads the clock idle copies are measured with.
func (u *upstreams) now() time.Time {
	if clock := u.clock.Load(); clock != nil {
		return (*clock)()
	}

	return time.Now()
}

// copyFor returns the copy under key, making it with build when there is
// none, and counts the caller's call on it: the caller calls done when the
// call finished. Concurrent callers make one copy.
func (u *upstreams) copyFor(key copyKey, build func() (*upstreamCopy, error)) (*upstreamCopy, error) {
	u.mu.Lock()

	entry, found := u.copies[key]
	if !found {
		entry = &copyEntry{}
		u.copies[key] = entry
	}

	entry.users++
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		entry.users--
		u.prune(key, entry)
		u.mu.Unlock()
	}()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.copy == nil {
		held, err := build()
		if err != nil {
			return nil, err
		}

		held.lastUsed.Store(u.now().UnixNano())
		entry.copy = held
	}

	entry.copy.inFlight.Add(1)

	return entry.copy, nil
}

// prune removes the entry under key when it holds no copy and no forward
// uses it. The caller holds u.mu.
func (u *upstreams) prune(key copyKey, entry *copyEntry) {
	if entry.users > 0 || u.copies[key] != entry || !entry.mu.TryLock() {
		return
	}

	defer entry.mu.Unlock()

	if entry.copy == nil {
		delete(u.copies, key)
	}
}

// adopt returns this replica's copy of a stored row, making it with no
// network round trip when there is none.
func (u *upstreams) adopt(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate, caller runtime.Identity, sessionPk int64, row db.GetLiveUpstreamSessionRow) (forwardingCopy, error) {
	key := copyKey{instance: instance.ID, upstream: row.UpstreamSessionID}

	held, err := u.copyFor(key, func() (*upstreamCopy, error) {
		held, err := u.connect(ctx, instance, template, caller, &runtime.AdoptedSession{
			ID: row.UpstreamSessionID, InitializeResult: row.InitializeResult,
		})
		if err != nil {
			return nil, err
		}

		held.sessionPk = sessionPk

		opsmetrics.UpstreamSessionEvent(opsmetrics.UpstreamAdopt)

		return held, nil
	})

	return forwardingCopy{upstreamCopy: held, key: key, row: row.ID}, err
}

// start sends initialize for a pair with no row and claims the pair.
func (u *upstreams) start(ctx context.Context, database *pgxpool.Pool, instance db.Instance, template *v1alpha1.MCPServerTemplate, caller runtime.Identity, sessionPk int64) (forwardingCopy, error) {
	fresh, err := u.connect(ctx, instance, template, caller, nil)
	if err != nil {
		return forwardingCopy{}, err
	}

	opsmetrics.UpstreamSessionEvent(opsmetrics.UpstreamInitialize)

	id := fresh.session.ID()
	if id == "" {
		// A sessionless server: the instance keeps one copy, and nothing is
		// stored.
		u.mu.Lock()
		u.sessionless[instance.ID] = true
		u.mu.Unlock()

		key := copyKey{instance: instance.ID}

		held, err := u.copyFor(key, func() (*upstreamCopy, error) { return fresh, nil })
		if held != fresh {
			fresh.Release()
		}

		return forwardingCopy{upstreamCopy: held, key: key}, err
	}

	result, valid := storableInitializeResult(id, fresh.session.InitializeResult())
	if !valid {
		fresh.End()
		u.logger.WarnContext(ctx, "the server's upstream session can't be stored: its session ID or initialize result is invalid or too large",
			"instance", instance.ID, "session", sessionPk)

		return forwardingCopy{}, errUpstreamSessionInvalid
	}

	fresh.sessionPk = sessionPk

	claim, err := claimPair(ctx, database, instance.ID, sessionPk, id, result)
	if err != nil {
		fresh.End()

		return forwardingCopy{}, err
	}

	switch {
	case claim.won:
		key := copyKey{instance: instance.ID, upstream: id}

		held, err := u.copyFor(key, func() (*upstreamCopy, error) { return fresh, nil })
		if held != fresh {
			fresh.Release()
		}

		return forwardingCopy{upstreamCopy: held, key: key, row: claim.row}, err
	case claim.winner != nil:
		opsmetrics.UpstreamSessionEvent(opsmetrics.UpstreamRaceLost)
		fresh.End()

		return u.adopt(ctx, instance, template, caller, sessionPk, *claim.winner)
	}

	// The pair ended, or the winner's row did, while this call was in
	// flight: forward this one call and end the session.
	fresh.inFlight.Add(1)

	return forwardingCopy{upstreamCopy: fresh, once: true}, nil
}

// storableInitializeResult returns the initialize result as stored, without
// its instructions, and whether id and the result fit the stored limits.
func storableInitializeResult(id string, result *mcp.InitializeResult) ([]byte, bool) {
	if len(id) > maxUpstreamSessionIDBytes {
		return nil, false
	}

	for index := range len(id) {
		if id[index] < 0x21 || id[index] > 0x7E {
			return nil, false
		}
	}

	if result == nil {
		return nil, false
	}

	stored := *result
	stored.Instructions = ""

	encoded, err := json.Marshal(&stored)
	if err != nil || len(encoded) > maxInitializeResultBytes {
		return nil, false
	}

	return encoded, true
}

// pairClaim is the outcome of a claim: won with the stored row, lost to
// winner, or neither when the pair or the winner's row ended.
type pairClaim struct {
	won    bool
	row    int64
	winner *db.GetLiveUpstreamSessionRow
}

// claimPair stores the upstream session of a pair in one transaction. The
// key-share lock on the session row orders it against disable and session
// end, which lock that row for update.
func claimPair(ctx context.Context, database *pgxpool.Pool, instanceID string, sessionPk int64, upstreamID string, result []byte) (pairClaim, error) {
	var claim pairClaim

	err := pgx.BeginFunc(ctx, database, func(tx pgx.Tx) error {
		claim = pairClaim{}
		queries := db.New(tx)

		if _, err := queries.LockLiveSessionForKeyShare(ctx, sessionPk); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}

			return fmt.Errorf("lock session: %w", err)
		}

		enabled, err := queries.SessionHasServerForInstanceTemplate(ctx, db.SessionHasServerForInstanceTemplateParams{
			SessionPk: sessionPk, InstanceID: instanceID,
		})
		if err != nil {
			return fmt.Errorf("check enabled server: %w", err)
		}

		if !enabled {
			return nil
		}

		row, err := queries.InsertUpstreamSession(ctx, db.InsertUpstreamSessionParams{
			InstanceID: instanceID, SessionPk: sessionPk, UpstreamSessionID: upstreamID, InitializeResult: result,
		})
		if err == nil {
			claim.won, claim.row = true, row

			return nil
		}

		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("insert upstream session: %w", err)
		}

		winner, err := queries.GetLiveUpstreamSession(ctx, db.GetLiveUpstreamSessionParams{InstanceID: instanceID, SessionPk: sessionPk})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read upstream session: %w", err)
		}

		claim.winner = &winner

		return nil
	})
	if err != nil {
		return pairClaim{}, fmt.Errorf("claim upstream session: %w", err)
	}

	return claim, nil
}

// connect opens a forwarding copy to instance: an adopted one when adopt is
// set, otherwise a new upstream session. A remote server's copy sends the
// headers filled in from the secrets of the instance's owner.
func (u *upstreams) connect(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate, caller runtime.Identity, adopt *runtime.AdoptedSession) (*upstreamCopy, error) {
	var remote *runtime.RemoteAuth

	if template.Spec.Remote != nil {
		// A shared remote server's headers use no secret, so it needs no
		// secret source.
		var source runtime.SecretSource

		switch {
		case u.secrets != nil:
			source = u.secrets(instance.OwnerPrincipal)
		case instance.Scope != catalog.ScopeShared:
			return nil, errNoSecretSource
		}

		// The headers are filled in at each connect, so a secret set
		// again takes effect at the next copy.
		header, _, err := runtime.RemoteHeaders(ctx, template, caller, source)
		if err != nil {
			return nil, err
		}

		remote = &runtime.RemoteAuth{Header: header}
	}

	// Replicas ignore list_changed on forwarding sessions: the reconciler
	// keeps the tool list, so a forwarding copy opens no GET stream. Closing
	// it sends DELETE only through End. The copy speaks the row's MCP
	// revision.
	deleteOnClose := &atomic.Bool{}

	connectCtx, cancel := upstreamConnectContext(ctx)
	defer cancel()

	session, _, err := runtime.ConnectUpstream(connectCtx, u.endpoint(instance, template), instance.McpRevision, runtime.UpstreamOptions{
		Implementation:       &mcp.Implementation{Name: "toolgate", Version: version.Get()},
		HTTPClient:           u.httpClient,
		Remote:               remote,
		DisableStandaloneSSE: true,
		Adopt:                adopt,
		DeleteOnClose:        deleteOnClose,
	})
	if err != nil {
		if remote != nil && remote.Refused() {
			return nil, fmt.Errorf("%w: %w", runtime.ErrCredentialRefused, err)
		}

		return nil, err
	}

	return &upstreamCopy{session: session, deleteOnClose: deleteOnClose, remote: remote}, nil
}

// release drops copy from this replica and releases it, so the next call
// makes a new copy. The upstream session lives on.
func (u *upstreams) release(key copyKey, held *upstreamCopy) {
	u.mu.Lock()
	entry, found := u.copies[key]
	u.mu.Unlock()

	if found {
		entry.mu.Lock()
		if entry.copy == held {
			entry.copy = nil
		}
		entry.mu.Unlock()

		u.mu.Lock()
		u.prune(key, entry)
		u.mu.Unlock()
	}

	held.Release()
}

// releaseWhere drops and releases every copy that match accepts, without
// waiting for the calls in flight on them: they finish with whatever the
// upstream answers. Releasing never ends the upstream session of a stored
// row. With endInstanceCopies, an instance's one copy is ended instead: it
// belongs to no row, so nothing else ends its upstream session. It returns
// once every matching copy is released; a copy that is still being made is
// checked once it is made, in the background.
func (u *upstreams) releaseWhere(match func(key copyKey, held *upstreamCopy) bool, endInstanceCopies bool) {
	u.mu.Lock()

	entries := make(map[copyKey]*copyEntry, len(u.copies))
	for key, entry := range u.copies {
		entries[key] = entry
	}

	u.mu.Unlock()

	releaseEntry := func(key copyKey, entry *copyEntry) {
		held := entry.copy
		if held != nil && match(key, held) {
			entry.copy = nil
		} else {
			held = nil
		}

		entry.mu.Unlock()

		u.mu.Lock()
		u.prune(key, entry)
		u.mu.Unlock()

		switch {
		case held == nil:
		case endInstanceCopies && key.upstream == "":
			held.End()
		default:
			held.Release()
		}
	}

	for key, entry := range entries {
		// A copy that is being made holds its entry's lock for the
		// connect; the check waits for it off this goroutine.
		if !entry.mu.TryLock() {
			go func() {
				entry.mu.Lock()
				releaseEntry(key, entry)
			}()

			continue
		}

		releaseEntry(key, entry)
	}
}

// releaseInstance releases every copy of an instance that left ready and
// forgets that it is sessionless. Its one copy, when it has no rows, is
// ended.
func (u *upstreams) releaseInstance(id string) {
	u.mu.Lock()
	delete(u.sessionless, id)
	u.mu.Unlock()

	u.releaseWhere(func(key copyKey, _ *upstreamCopy) bool { return key.instance == id }, true)
}

// releaseAll releases every copy, at shutdown.
func (u *upstreams) releaseAll() {
	u.releaseWhere(func(copyKey, *upstreamCopy) bool { return true }, false)
}

// releaseIdle releases every copy that served no call for copyIdleTimeout.
func (u *upstreams) releaseIdle() {
	cutoff := u.now().Add(-copyIdleTimeout).UnixNano()

	u.releaseWhere(func(_ copyKey, held *upstreamCopy) bool {
		return held.inFlight.Load() == 0 && held.lastUsed.Load() <= cutoff
	}, false)
}

// releaseSessionCopies releases this replica's copies of toolgate session
// sessionPk that match none of its live rows: their pair was disabled or the
// session ended. An instance's one copy belongs to no session and stays.
func (u *upstreams) releaseSessionCopies(sessionPk int64, live []db.ListLiveUpstreamSessionsOfSessionRow) {
	kept := make(map[copyKey]bool, len(live))
	for _, row := range live {
		kept[copyKey{instance: row.InstanceID, upstream: row.UpstreamSessionID}] = true
	}

	u.releaseWhere(func(key copyKey, held *upstreamCopy) bool {
		return key.upstream != "" && held.sessionPk == sessionPk && !kept[key]
	}, false)
}

// holdsSession reports whether this replica holds a copy of a row of
// toolgate session sessionPk.
func (u *upstreams) holdsSession(sessionPk int64) bool {
	u.mu.Lock()

	entries := make([]*copyEntry, 0, len(u.copies))
	for key, entry := range u.copies {
		if key.upstream != "" {
			entries = append(entries, entry)
		}
	}

	u.mu.Unlock()

	for _, entry := range entries {
		// A copy still being made is counted: its session can't be read
		// without waiting for the connect.
		if !entry.mu.TryLock() {
			return true
		}

		held := entry.copy != nil && entry.copy.sessionPk == sessionPk
		entry.mu.Unlock()

		if held {
			return true
		}
	}

	return false
}

// hasInstance reports whether this replica holds a copy of instance id.
func (u *upstreams) hasInstance(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	for key := range u.copies {
		if key.instance == id {
			return true
		}
	}

	return u.sessionless[id]
}

// dropUnlessReady releases this replica's copies of instance id when the
// instance is no longer ready.
func (m *metaTools) dropUnlessReady(id string) {
	if !m.upstreams.hasInstance(id) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	defer cancel()

	instance, err := db.New(m.database).GetInstance(ctx, id)
	if err == nil && instance.Status == statusReady {
		return
	}

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return
	}

	m.upstreams.releaseInstance(id)
}

// releaseEndedSessionCopies releases this replica's copies of toolgate
// session id whose rows are no longer live, after a disable or the
// session's end.
func (m *metaTools) releaseEndedSessionCopies(id int64) {
	if !m.upstreams.holdsSession(id) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
	defer cancel()

	live, err := db.New(m.database).ListLiveUpstreamSessionsOfSession(ctx, id)
	if err != nil {
		return
	}

	m.upstreams.releaseSessionCopies(id, live)
}
