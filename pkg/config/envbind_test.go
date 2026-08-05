package config

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// THE bug this file exists to prevent, shipped to production 2026-08-05.
//
// A config field was added with a `default:` struct tag and wired into the ECS
// task definition as MESHTK_NODESNAPSHOTBUCKET. The env var was set on the
// container and never read, so the node snapshot silently disabled itself and
// the deploy was a no-op that looked entirely healthy.
//
// The cause is a viper property that is easy to assume away: AutomaticEnv only
// resolves keys viper ALREADY KNOWS during Unmarshal, and keys come from the
// merged config -- i.e. the embedded meshtk.yaml -- NOT from Go struct tags. A
// field absent from that yaml is unreachable by env, whatever its default tag
// says. MESHTK_NODEDBPATH works only because NodeDbPath IS in the yaml.
//
// This is the same family as the still-live MESHTK_S3_LOGS_BUCKET bug, which
// ships every inspector log to a hardcoded default bucket. That one is a
// SPELLING mismatch (underscores); this one is a REGISTRATION gap. Both fail
// the same way: silently, with the default winning.
//
// So the assertion is not "the struct has a field" -- that would pass while
// production stayed broken. It is "an env var actually reaches the field".
func TestEnvVarsReachTheirConfigFields(t *testing.T) {
	cases := []struct {
		env   string
		key   string
		value string
		get   func(*Config) string
	}{
		{
			env:   "MESHTK_NODESNAPSHOTBUCKET",
			key:   "nodesnapshotbucket",
			value: "mqtt-logs-use1-dc34-80a6b349",
			get:   func(c *Config) string { return c.NodeSnapshotBucket },
		},
		{
			env:   "MESHTK_NODESNAPSHOTKEY",
			key:   "nodesnapshotkey",
			value: "snapshots/nodes/nodes.json",
			get:   func(c *Config) string { return c.NodeSnapshotKey },
		},
		{
			// The control. NodeDbPath is in meshtk.yaml and is known to work in
			// production, so if THIS case ever fails the harness is wrong rather
			// than the config -- which keeps a green run from being vacuous.
			env:   "MESHTK_NODEDBPATH",
			key:   "nodedbpath",
			value: "/var/www/html/nodes.json",
			get:   func(c *Config) string { return c.NodeDbPath },
		},
	}

	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)

			// Reproduce the production binding setup exactly: prefix, replacer,
			// the embedded defaults, AutomaticEnv, then Unmarshal. Calling
			// Config.Read() instead would drag in stdin slurping and config-file
			// discovery, neither of which is what is under test.
			v := viper.New()
			v.SetConfigType("yaml")
			v.SetEnvPrefix("meshtk")
			v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
			if err := v.MergeConfig(strings.NewReader(DefaultConfig)); err != nil {
				t.Fatalf("merge embedded defaults: %v", err)
			}
			v.AutomaticEnv()

			var c Config
			if err := v.Unmarshal(&c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got := tc.get(&c); got != tc.value {
				t.Errorf("%s did not reach its field: got %q, want %q.\n"+
					"Is %q present in pkg/config/meshtk.yaml? AutomaticEnv cannot see a key "+
					"that the merged config never registered, and a `default:` struct tag does "+
					"not register one.", tc.env, got, tc.value, tc.key)
			}
		})
	}
}
