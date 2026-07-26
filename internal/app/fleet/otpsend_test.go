package fleet

import (
	"context"
	"errors"
	"io"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/internal/otpqueue"
)

func testLogger() *log.Logger {
	l := log.New()
	l.SetOutput(io.Discard)
	return l
}

type fakeOtpStore struct {
	items          []otpqueue.Item
	welcomes       []otpqueue.WelcomeItem
	deleted        []string
	welcomeDeleted []string
	bumped         map[string]int
	welcomeBumped  map[string]int
	marked         []uint32
	listErr        error
}

func newFakeOtpStore(items ...otpqueue.Item) *fakeOtpStore {
	return &fakeOtpStore{items: items, bumped: map[string]int{}, welcomeBumped: map[string]int{}}
}

func (f *fakeOtpStore) List(context.Context) ([]otpqueue.Item, []otpqueue.WelcomeItem, error) {
	return f.items, f.welcomes, f.listErr
}
func (f *fakeOtpStore) DeleteWelcome(_ context.Context, nodeID string) error {
	f.welcomeDeleted = append(f.welcomeDeleted, nodeID)
	return nil
}
func (f *fakeOtpStore) BumpWelcomeAttempts(_ context.Context, nodeID string, attempts int) error {
	f.welcomeBumped[nodeID] = attempts
	return nil
}
func (f *fakeOtpStore) Delete(_ context.Context, nodeID string) error {
	f.deleted = append(f.deleted, nodeID)
	return nil
}
func (f *fakeOtpStore) BumpAttempts(_ context.Context, nodeID string, attempts int) error {
	f.bumped[nodeID] = attempts
	return nil
}
func (f *fakeOtpStore) MarkRadioCodeSent(_ context.Context, nodeNum uint32, _ int64) error {
	f.marked = append(f.marked, nodeNum)
	return nil
}

type fakeOtpDeps struct {
	pubkeys      map[uint32]string
	sendErr      error
	sent         []uint32
	welcomeSent  []string // messages, in order
	welcomeErr   error
	nowMs        int64
}

func (f *fakeOtpDeps) ResolvePubKeyHex(item otpqueue.Item) (string, bool) {
	if item.PublicKey != "" {
		return item.PublicKey, true
	}
	k, ok := f.pubkeys[item.NodeNum]
	return k, ok
}
func (f *fakeOtpDeps) SendCode(toNodeNum uint32, pubKeyHex, code string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, toNodeNum)
	return nil
}
func (f *fakeOtpDeps) SendWelcome(item otpqueue.WelcomeItem, _ string) error {
	if f.welcomeErr != nil {
		return f.welcomeErr
	}
	f.welcomeSent = append(f.welcomeSent, item.Message)
	return nil
}
func (f *fakeOtpDeps) NowMs() int64 { return f.nowMs }

func welcomeItem(nodeID string, nodeNum uint32, createdAt int64, attempts int) otpqueue.WelcomeItem {
	return otpqueue.WelcomeItem{NodeID: nodeID, NodeNum: nodeNum, Message: "Welcome!", CreatedAt: createdAt, Attempts: attempts}
}

func TestWelcomeSuccessSendsAndDeletes(t *testing.T) {
	store := newFakeOtpStore()
	store.welcomes = []otpqueue.WelcomeItem{welcomeItem("!433d1cec", 1128078572, nowMs, 0)}
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.welcomeSent) != 1 || deps.welcomeSent[0] != "Welcome!" {
		t.Fatalf("expected one welcome send: %+v", deps.welcomeSent)
	}
	if len(store.welcomeDeleted) != 1 || store.welcomeDeleted[0] != "!433d1cec" {
		t.Fatalf("sent welcome must be deleted: %+v", store.welcomeDeleted)
	}
	if len(store.marked) != 0 {
		t.Fatal("welcome must NOT stamp codeSentAt")
	}
}

func TestWelcomeNoPubkeyStaysQueued(t *testing.T) {
	store := newFakeOtpStore()
	store.welcomes = []otpqueue.WelcomeItem{welcomeItem("!433d1cec", 1128078572, nowMs, 0)}
	deps := &fakeOtpDeps{nowMs: nowMs}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.welcomeSent) != 0 || len(store.welcomeDeleted) != 0 || len(store.welcomeBumped) != 0 {
		t.Fatal("keyless welcome must be left untouched")
	}
}

func TestWelcomeFailureBumpsAndSurvives(t *testing.T) {
	store := newFakeOtpStore()
	store.welcomes = []otpqueue.WelcomeItem{welcomeItem("!433d1cec", 1128078572, nowMs, 4)}
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}, welcomeErr: errors.New("broker down")}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(store.welcomeDeleted) != 0 {
		t.Fatal("failed welcome must survive")
	}
	if store.welcomeBumped["!433d1cec"] != 5 {
		t.Fatalf("welcome attempts must bump to 5: %+v", store.welcomeBumped)
	}
}

func TestWelcomeExpiredAndCappedReap(t *testing.T) {
	store := newFakeOtpStore()
	store.welcomes = []otpqueue.WelcomeItem{
		welcomeItem("!00000001", 1, nowMs-otpqueue.MaxAgeMs-1, 0),
		welcomeItem("!00000002", 2, nowMs, otpqueue.MaxAttempts),
	}
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1: "0xk", 2: "0xk"}}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.welcomeSent) != 0 {
		t.Fatal("reaped welcomes must not send")
	}
	if len(store.welcomeDeleted) != 2 {
		t.Fatalf("both welcomes must be reaped: %+v", store.welcomeDeleted)
	}
}

func item(nodeID string, nodeNum uint32, createdAt int64, attempts int) otpqueue.Item {
	return otpqueue.Item{NodeID: nodeID, NodeNum: nodeNum, Code: "123456", CreatedAt: createdAt, Attempts: attempts}
}

const nowMs = int64(1_784_969_443_000)

func TestOtpExpiredItemReapedUnsent(t *testing.T) {
	store := newFakeOtpStore(item("!433d1cec", 1128078572, nowMs-otpqueue.MaxAgeMs-1, 0))
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.sent) != 0 {
		t.Fatal("expired item must not send")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "!433d1cec" {
		t.Fatalf("expired item must be reaped: %+v", store.deleted)
	}
}

func TestOtpNoPubkeyStaysQueued(t *testing.T) {
	store := newFakeOtpStore(item("!433d1cec", 1128078572, nowMs, 0))
	deps := &fakeOtpDeps{nowMs: nowMs} // no key anywhere

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.sent) != 0 || len(store.deleted) != 0 || len(store.bumped) != 0 {
		t.Fatalf("keyless item must be left untouched: sent=%v deleted=%v bumped=%v",
			deps.sent, store.deleted, store.bumped)
	}
}

func TestOtpSuccessSendsDeletesMarks(t *testing.T) {
	store := newFakeOtpStore(item("!433d1cec", 1128078572, nowMs, 0))
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.sent) != 1 || deps.sent[0] != 1128078572 {
		t.Fatalf("expected one send: %+v", deps.sent)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "!433d1cec" {
		t.Fatalf("sent item must be deleted: %+v", store.deleted)
	}
	if len(store.marked) != 1 || store.marked[0] != 1128078572 {
		t.Fatalf("sent item must stamp codeSentAt: %+v", store.marked)
	}
}

func TestOtpSendFailureBumpsAndSurvives(t *testing.T) {
	store := newFakeOtpStore(item("!433d1cec", 1128078572, nowMs, 2))
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}, sendErr: errors.New("broker down")}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(store.deleted) != 0 {
		t.Fatal("failed item must survive")
	}
	if store.bumped["!433d1cec"] != 3 {
		t.Fatalf("attempts must bump to 3: %+v", store.bumped)
	}
}

func TestOtpAttemptsCapReaps(t *testing.T) {
	store := newFakeOtpStore(item("!433d1cec", 1128078572, nowMs, otpqueue.MaxAttempts))
	deps := &fakeOtpDeps{nowMs: nowMs, pubkeys: map[uint32]string{1128078572: "0xkey"}}

	processOtpQueue(context.Background(), deps, store, testLogger())

	if len(deps.sent) != 0 {
		t.Fatal("capped item must not send")
	}
	if len(store.deleted) != 1 {
		t.Fatalf("capped item must be reaped: %+v", store.deleted)
	}
}

func TestPkiTopicForStripsChannelSegment(t *testing.T) {
	got := pkiTopicFor("msh/US/2/e/dc.run", 0xdc340001)
	if got != "msh/US/2/e/PKI/!dc340001" {
		t.Fatalf("bad topic: %q", got)
	}
}

func TestParseNodeID(t *testing.T) {
	n, err := parseNodeID("!dc340001")
	if err != nil || n != 0xdc340001 {
		t.Fatalf("parse failed: %v %x", err, n)
	}
	if _, err := parseNodeID("!zzz"); err == nil {
		t.Fatal("garbage must error")
	}
}
