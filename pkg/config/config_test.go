package config

import (
	"testing"
)

func TestCredCacheConfigDefaults(t *testing.T) {
	c := NewConfig()

	if c.Server.CredCache.TTLSecs != 900 {
		t.Errorf("CredCache.TTLSecs = %d, want 900", c.Server.CredCache.TTLSecs)
	}
	if c.Server.CredCache.MaxSizeMB != 64 {
		t.Errorf("CredCache.MaxSizeMB = %d, want 64", c.Server.CredCache.MaxSizeMB)
	}
	if c.Server.CredCache.TableName != "run-human-electro" {
		t.Errorf("CredCache.TableName = %q, want %q", c.Server.CredCache.TableName, "run-human-electro")
	}
	if c.Server.CredCache.TableRegion != "us-east-1" {
		t.Errorf("CredCache.TableRegion = %q, want %q", c.Server.CredCache.TableRegion, "us-east-1")
	}
	if c.Server.CredCache.DynamoDBEndpoint != "" {
		t.Errorf("CredCache.DynamoDBEndpoint = %q, want empty string", c.Server.CredCache.DynamoDBEndpoint)
	}
	if c.Server.CredCache.TimeoutSecs != 5 {
		t.Errorf("CredCache.TimeoutSecs = %d, want 5", c.Server.CredCache.TimeoutSecs)
	}

	// Passthrough slice
	expectedPassthrough := []string{"ghosts", "kph", "ax", "meshmap"}
	if len(c.Server.CredCache.Passthrough) != len(expectedPassthrough) {
		t.Fatalf("CredCache.Passthrough length = %d, want %d", len(c.Server.CredCache.Passthrough), len(expectedPassthrough))
	}
	for i, v := range expectedPassthrough {
		if c.Server.CredCache.Passthrough[i] != v {
			t.Errorf("CredCache.Passthrough[%d] = %q, want %q", i, c.Server.CredCache.Passthrough[i], v)
		}
	}
}

func TestProxyCredsDefaults(t *testing.T) {
	c := NewConfig()

	if c.Server.ProxyUsername != "public" {
		t.Errorf("Server.ProxyUsername = %q, want %q", c.Server.ProxyUsername, "public")
	}
	if c.Server.ProxyPassword != "31337" {
		t.Errorf("Server.ProxyPassword = %q, want %q", c.Server.ProxyPassword, "31337")
	}
}

func TestAdminListenAddressDefault(t *testing.T) {
	c := NewConfig()

	if c.Server.AdminListenAddress != "localhost:9090" {
		t.Errorf("Server.AdminListenAddress = %q, want %q", c.Server.AdminListenAddress, "localhost:9090")
	}
}

// A configured node beacons on the Announce ticker forever (default 300s).
// Anything in BroadcastMessage rides along as a channel-wide TEXT_MESSAGE_APP
// to 0xffffffff, so silence must be what you get for free -- speaking on a mesh
// is opt-in, never a default.
func TestBroadcastMessageDefaultsToSilence(t *testing.T) {
	c := NewConfig()

	if c.NodeInfo.BroadcastMessage != "" {
		t.Errorf("NodeInfo.BroadcastMessage = %q, want %q (unconfigured nodes must not chat on the mesh)",
			c.NodeInfo.BroadcastMessage, "")
	}
}
