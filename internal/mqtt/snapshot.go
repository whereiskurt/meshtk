package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ErrNoSnapshot is returned by a SnapshotStore whose backing object does not
// exist yet. It is deliberately distinct from a transport failure: "no snapshot
// has ever been written" is a normal first boot, while "S3 is unreachable" must
// never be mistaken for one.
var ErrNoSnapshot = errors.New("no snapshot")

// SnapshotStore is the durable home of the node database between task
// lifetimes. Kept to two methods so the reset semantics below can be tested
// against an in-memory fake rather than against S3.
type SnapshotStore interface {
	Get() ([]byte, error)
	Put([]byte) error
}

// MarshalSnapshot renders the database in the same shape WriteFile produces, so
// a snapshot pulled out of S3 is byte-comparable with the nodes.json an
// operator would read off the container.
func (db NodeDB) MarshalSnapshot() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(db); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalSnapshot parses a snapshot into the database.
func (db *NodeDB) UnmarshalSnapshot(b []byte) error {
	if *db == nil {
		*db = NodeDB{}
	}
	return json.Unmarshal(b, db)
}

// RestoreSnapshot seeds a COLD database from the durable snapshot. It reports
// whether it actually restored anything.
//
// A populated database is never overwritten: restore exists for the cold-start
// case, and clobbering live state with an older snapshot would be a regression
// dressed as a feature.
//
// A missing snapshot is not an error. First boot after a fresh deployment has
// nothing to restore, and returning an error there would make every clean
// rollout log a failure.
func RestoreSnapshot(db *NodeDB, store SnapshotStore) (bool, error) {
	if *db == nil {
		*db = NodeDB{}
	}
	if len(*db) > 0 {
		return false, nil
	}

	blob, err := store.Get()
	if errors.Is(err, ErrNoSnapshot) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := db.UnmarshalSnapshot(blob); err != nil {
		return false, err
	}
	return len(*db) > 0, nil
}

// SnapshotResult reports what a tick did, so the caller can log a reset
// distinctly from a routine backup. An operator wiping the node database should
// be obvious in the logs, not inferred from a node count dropping.
type SnapshotResult struct {
	Reset bool
	Wrote bool
}

// Snapshotter runs the snapshot cycle. It is STATEFUL, and the one piece of
// state it holds is what makes the operator reset safe -- see Tick.
type Snapshotter struct {
	Store SnapshotStore

	// sawEmpty is true when the PREVIOUS tick already observed an empty remote
	// snapshot. It makes the reset edge-triggered instead of level-triggered.
	//
	// Without it the reset never converges, and the failure is severe rather
	// than cosmetic. A reset skips the write, so the remote stays {}; within
	// one interval live traffic repopulates the database; the next tick sees
	// "remote {} + local populated" and resets AGAIN. The node database would
	// be wiped every interval forever and snapshots would never resume.
	//
	// Bumping the interval or clearing harder does not fix that -- {} is a
	// STATE, and an empty NodeDB serialises to {} as well, so "reset requested"
	// and "legitimately empty" are indistinguishable from content alone. Only
	// the transition carries the operator's intent.
	sawEmpty bool
}

// NewSnapshotter returns a Snapshotter over store.
func NewSnapshotter(store SnapshotStore) *Snapshotter {
	return &Snapshotter{Store: store}
}

// Tick performs one snapshot cycle: read, decide, write.
//
// The read comes FIRST, and that ordering is half the design. nodes.json is
// flushed locally every 5s and again on shutdown, so if the snapshot were
// write-only an operator could never empty it -- the outgoing task would
// overwrite the operator's {} before a new task ever read it, and the reset
// would silently never happen.
//
// A TRANSITION to an empty remote object is an operator reset request:
//
//	remote {} (newly) + local populated  -> clear local, SKIP the write
//	anything else                        -> write the current database
//
// Skipping the write keeps the {} authoritative for any task booting
// concurrently, which would otherwise restore the database just cleared. The
// following tick sees the same {} but no longer treats it as an edge, so it
// writes normally and the system resumes.
//
// A Get failure is FAIL-SAFE in both directions -- no reset is inferred (a
// transient S3 error must never wipe the node database) and the write still
// proceeds (a transient S3 error must never cost a backup). It also leaves
// sawEmpty untouched, so an error cannot swallow a pending operator edge.
func (s *Snapshotter) Tick(db *NodeDB) (SnapshotResult, error) {
	if *db == nil {
		*db = NodeDB{}
	}

	blob, err := s.Store.Get()
	switch {
	case err == nil:
		empty := isEmptySnapshot(blob)
		firstSighting := empty && !s.sawEmpty
		s.sawEmpty = empty
		if firstSighting && len(*db) > 0 {
			*db = NodeDB{}
			return SnapshotResult{Reset: true}, nil
		}
	case errors.Is(err, ErrNoSnapshot):
		// Nothing there yet; the write below creates it. A missing object is
		// not an empty one -- do not arm the edge.
		s.sawEmpty = false
	default:
		// Transport failure: deliberately fall through to the write.
	}

	out, merr := db.MarshalSnapshot()
	if merr != nil {
		return SnapshotResult{}, merr
	}
	if perr := s.Store.Put(out); perr != nil {
		return SnapshotResult{}, perr
	}
	// The database just written is authoritative, so the remote is empty iff
	// the database is. Recording that here keeps sawEmpty honest without a
	// second round trip.
	s.sawEmpty = len(*db) == 0
	return SnapshotResult{Wrote: true}, nil
}

// isEmptySnapshot reports whether a snapshot holds zero nodes.
//
// Parsed rather than string-compared: an operator types `{}`, but a formatter,
// an editor, or `aws s3 cp` may hand back `{}\n`, `{ }`, or an indented empty
// object, and every one of those means the same thing. Unparseable bytes are
// NOT empty -- a truncated upload must not read as "wipe the database".
func isEmptySnapshot(b []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	return len(probe) == 0
}
