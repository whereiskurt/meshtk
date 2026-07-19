package fleet

import (
	"testing"

	"github.com/whereiskurt/meshtk/pkg/config"
)

// TestBuildKeyResolver_NilLoggerDoesNotPanic is a regression guard for the
// startup SIGSEGV that crash-looped run.mqtt at deploy time: buildKeyResolver
// runs inside NewFleets during early bootstrap (NewApp -> RegisterOsArgs ->
// NewFleets), BEFORE c.Log is wired, so c.Log is nil. An unconditional
// c.Log.Infof / c.Log.Errorf there nil-derefs logrus and panics the whole
// process. This exercises the full construction path (including the final
// "resolver ready" Infof at the original crash site) with c.Log == nil.
func TestBuildKeyResolver_NilLoggerDoesNotPanic(t *testing.T) {
	c := &config.Config{}
	// Valid keycache config so NewCache/NewDynamoDBStore succeed and execution
	// reaches the final Infof (the exact line that crashed in prod).
	c.Server.KeyCache = config.KeyCacheConfig{
		TTLSecs:         90,
		MaxSizeMB:       16,
		TableName:       "run-human-electro",
		TableRegion:     "us-east-1",
		NegativeTTLSecs: 60,
		Fallback:        "nodes.json",
	}
	// c.Log intentionally left nil — mirrors the NewFleets bootstrap ordering.

	resolver := buildKeyResolver(c) // must NOT panic on the nil logger
	if resolver == nil {
		t.Fatal("expected a resolver from valid config with a nil logger, got nil")
	}
}
