package server

// Anchor import for the pinned MQTT v5 wire codec.
//
// `go mod tidy` drops a required module that nothing imports, and `go mod vendor`
// only materializes packages the main module actually needs — so pinning
// github.com/eclipse/paho.golang v0.22.0 in an isolated dependency commit (one that
// deliberately carries no protocol code, so a bisect can separate the dependency
// bump from the behavior change) requires this blank import to exist.
//
// Removed once proxy_v5.go imports the codec for real.
import _ "github.com/eclipse/paho.golang/packets"
