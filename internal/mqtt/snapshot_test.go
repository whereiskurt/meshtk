package mqtt

import (
	"errors"
	"testing"
)

// nodes.json lives on an ephemeral container filesystem and there is no
// snapshot code, so every task restart drops the node database to whatever
// re-announces. Measured on 2026-08-04: 9 of 81 nodes transmitted anything in
// 15 minutes, so a restart empties the map for hours -- silently, because a
// sparse map raises no alarm.
//
// These tests cover the snapshot/restore cycle and the operator reset that
// rides on it.

// fakeStore is an in-memory SnapshotStore. Errors are injectable because the
// fail-safe behaviour on a GET failure is a hard requirement, not a detail:
// inferring "reset" from a transient S3 error would wipe the node database.
type fakeStore struct {
	data    []byte
	present bool
	getErr  error
	putErr  error
	puts    int
	gets    int
}

func (f *fakeStore) Get() ([]byte, error) {
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.present {
		return nil, ErrNoSnapshot
	}
	return f.data, nil
}

func (f *fakeStore) Put(b []byte) error {
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	f.data = append([]byte(nil), b...)
	f.present = true
	return nil
}

func populated() NodeDB {
	return NodeDB{
		0xd50b630d: {From: 0xd50b630d, LongName: "KPH", ShortName: "KPH"},
		0x84b2fcb5: {From: 0x84b2fcb5, LongName: "AgentX", ShortName: "Ax"},
	}
}

// --- Restore -------------------------------------------------------------

func TestRestoreSeedsAnEmptyDatabase(t *testing.T) {
	src := populated()
	blob, err := src.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	store := &fakeStore{data: blob, present: true}

	db := NodeDB{}
	restored, err := RestoreSnapshot(&db, store)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !restored {
		t.Fatal("restore reported no-op; a cold task would start with an empty map")
	}
	if len(db) != 2 {
		t.Fatalf("restored %d nodes, want 2", len(db))
	}
	if db[0xd50b630d].LongName != "KPH" {
		t.Errorf("restored node lost its name: %+v", db[0xd50b630d])
	}
}

func TestRestoreLeavesAPopulatedDatabaseAlone(t *testing.T) {
	// A live database must never be clobbered by an older snapshot -- restore
	// is for cold starts only.
	blob, _ := NodeDB{0x11111111: {From: 0x11111111, LongName: "stale"}}.MarshalSnapshot()
	store := &fakeStore{data: blob, present: true}

	db := populated()
	restored, err := RestoreSnapshot(&db, store)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored {
		t.Error("restore overwrote a populated database")
	}
	if len(db) != 2 || db[0xd50b630d] == nil {
		t.Errorf("live database was modified: %+v", db)
	}
}

func TestRestoreWithNoSnapshotIsNotAnError(t *testing.T) {
	// First ever boot. Starting empty is correct, and must not look like a
	// failure or the caller will log noise on every fresh deployment.
	db := NodeDB{}
	restored, err := RestoreSnapshot(&db, &fakeStore{present: false})
	if err != nil {
		t.Fatalf("a missing snapshot must not be an error, got %v", err)
	}
	if restored {
		t.Error("reported a restore when there was no snapshot")
	}
}

func TestRestoreFromEmptyObjectYieldsAnEmptyDatabase(t *testing.T) {
	// The boot half of the operator reset: a snapshot of {} means "start
	// clean", and must agree with what the tick path does.
	db := NodeDB{}
	store := &fakeStore{data: []byte("{}"), present: true}
	restored, err := RestoreSnapshot(&db, store)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(db) != 0 {
		t.Errorf("restoring {} produced %d nodes, want 0", len(db))
	}
	_ = restored
}

// --- Snapshot tick -------------------------------------------------------

func TestTickWritesTheDatabase(t *testing.T) {
	store := &fakeStore{}
	db := populated()

	res, err := NewSnapshotter(store).Tick(&db)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reset {
		t.Error("tick reported a reset on a normal cycle")
	}
	if store.puts != 1 {
		t.Fatalf("tick did a %d PUTs, want 1", store.puts)
	}

	var round NodeDB
	if err := round.UnmarshalSnapshot(store.data); err != nil {
		t.Fatalf("snapshot does not round-trip: %v", err)
	}
	if len(round) != 2 {
		t.Errorf("snapshot holds %d nodes, want 2", len(round))
	}
}

func TestTickTreatsRemoteEmptyObjectAsAnOperatorReset(t *testing.T) {
	// THE operator control: writing {} to the snapshot clears the node DB.
	store := &fakeStore{data: []byte("{}"), present: true}
	db := populated()

	res, err := NewSnapshotter(store).Tick(&db)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !res.Reset {
		t.Fatal("a remote {} did not trigger a reset; the operator control does not work")
	}
	if len(db) != 0 {
		t.Errorf("reset left %d nodes behind", len(db))
	}
	// Skipping the PUT is what stops a concurrently-booting task from
	// restoring the pre-reset database from a snapshot we just rewrote.
	if store.puts != 0 {
		t.Errorf("reset cycle wrote %d snapshots, want 0 -- the {} must stay authoritative", store.puts)
	}
}

func TestResetConverges(t *testing.T) {
	// After a reset both sides are empty, so the next tick must NOT re-trigger
	// -- otherwise the PUT is suppressed forever and snapshots stop entirely.
	store := &fakeStore{data: []byte("{}"), present: true}
	db := populated()

	// ONE snapshotter across both ticks -- the convergence guarantee lives in
	// its state, so a fresh one per tick would not be testing the real thing.
	snap := NewSnapshotter(store)
	if _, err := snap.Tick(&db); err != nil {
		t.Fatalf("first tick: %v", err)
	}

	// Live traffic repopulates the DB before the next cycle. This is the case
	// that made the level-triggered version wipe the database forever.
	db[0xaaaaaaaa] = &Node{From: 0xaaaaaaaa, LongName: "rejoined"}
	res, err := snap.Tick(&db)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if res.Reset {
		t.Error("reset re-triggered after converging; snapshots would never resume")
	}
	if store.puts != 1 {
		t.Errorf("second tick did %d PUTs, want 1 -- normal snapshotting did not resume", store.puts)
	}
}

func TestGetFailureNeverResetsAndStillWrites(t *testing.T) {
	// Fail-safe. Inferring a reset from a transient S3 error would destroy the
	// node database, and skipping the PUT would lose the backup too.
	boom := errors.New("s3 unavailable")
	store := &fakeStore{getErr: boom}
	db := populated()

	res, err := NewSnapshotter(store).Tick(&db)
	if err != nil {
		t.Fatalf("a GET failure must not fail the tick, got %v", err)
	}
	if res.Reset {
		t.Fatal("a GET failure was misread as an operator reset -- this wipes the node DB")
	}
	if len(db) != 2 {
		t.Errorf("database was modified on a GET failure: %d nodes", len(db))
	}
	if store.puts != 1 {
		t.Errorf("a GET failure suppressed the PUT (%d); a transient error must not cost a backup", store.puts)
	}
}

func TestMissingRemoteSnapshotIsNotAReset(t *testing.T) {
	// "No snapshot yet" and "snapshot deliberately emptied" are different
	// things. Only the latter clears the database.
	store := &fakeStore{present: false}
	db := populated()

	res, err := NewSnapshotter(store).Tick(&db)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reset {
		t.Fatal("a missing snapshot was treated as a reset")
	}
	if len(db) != 2 {
		t.Errorf("database cleared on first-ever snapshot: %d nodes", len(db))
	}
	if store.puts != 1 {
		t.Errorf("first-ever tick did %d PUTs, want 1", store.puts)
	}
}

func TestEmptyRemoteAndEmptyLocalIsNotAReset(t *testing.T) {
	// A legitimately empty mesh must not look like a reset request forever.
	store := &fakeStore{data: []byte("{}"), present: true}
	db := NodeDB{}

	res, err := NewSnapshotter(store).Tick(&db)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Reset {
		t.Error("empty-remote + empty-local reported a reset")
	}
	if store.puts != 1 {
		t.Errorf("did %d PUTs, want 1 -- snapshotting must continue on an empty mesh", store.puts)
	}
}
