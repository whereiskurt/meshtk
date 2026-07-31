package fleet

import "testing"

// burstLockFor backs the serialization fix for handleLLMChat's paced send
// goroutine: a second question from the same radio must queue behind the
// first burst instead of interleaving with it, while a different radio (or a
// different fleet) must never contend on the same mutex. Neither property
// needs a live FleetCmd, Bedrock, or MQTT -- just the lookup itself.
func TestBurstLockForSameKeySameMutex(t *testing.T) {
	f := &FleetCmd{}
	a := f.burstLockFor(0, 1129943268)
	b := f.burstLockFor(0, 1129943268)
	if a != b {
		t.Error("same (fleetIdx, from) key returned different mutexes")
	}
}

func TestBurstLockForDifferentRadioDifferentMutex(t *testing.T) {
	f := &FleetCmd{}
	a := f.burstLockFor(0, 1129943268)
	b := f.burstLockFor(0, 42)
	if a == b {
		t.Error("different requester radios shared a mutex; bursts would serialize across radios")
	}
}

func TestBurstLockForDifferentFleetDifferentMutex(t *testing.T) {
	f := &FleetCmd{}
	a := f.burstLockFor(0, 1129943268)
	b := f.burstLockFor(1, 1129943268)
	if a == b {
		t.Error("same radio in different fleets shared a mutex")
	}
}
