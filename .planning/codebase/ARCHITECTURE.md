# Architecture

**Analysis Date:** 2026-03-10

## Pattern Overview

**Overall:** Multi-layered command-based application with pluggable packet inspection/proxy system

**Key Characteristics:**
- Cobra CLI framework for command routing and argument management
- MQTT as primary protocol layer for Meshtastic mesh network communication
- Rule-based packet inspection engine for filtering and transformation
- Three primary operational modes: nodeinfo announcement, fleet simulation, server proxy/inspection
- Separation of concerns between command layer, network layer, and business logic

## Layers

**Command Layer (CLI):**
- Purpose: Parse arguments, manage global flags, route execution to operational commands
- Location: `internal/app/app.go`, `internal/app/cmdargs.go`
- Contains: Cobra command tree builders, flag/environment variable mappers, app lifecycle management
- Depends on: Config, sub-command handlers (Server, Fleet, NodeInfo)
- Used by: Main entry point `cmd/meshtk.go`

**Application/Business Logic Layer:**
- Purpose: Implement core operations specific to each command mode
- Locations: `internal/app/server/`, `internal/app/fleet/`, `internal/app/nodeinfo/`
- Contains: ServerCmd, FleetCmd, NodeInfoCmd - each handles specific operational mode
- Depends on: MQTT client, Config, protocol definitions
- Used by: Command layer

**Network/Protocol Layer:**
- Purpose: Abstract MQTT communication, message parsing, encryption/decryption
- Location: `internal/mqtt/mqtt.go`, `internal/mqtt/publish.go`, `internal/mqtt/crypto.go`
- Contains: MqttClient wrapper, message handlers, cipher management, PKI encryption
- Depends on: Paho MQTT client, Protobuf definitions, config for keys
- Used by: Fleet, Server, NodeInfo commands

**Configuration/Infrastructure Layer:**
- Purpose: Centralized configuration loading, logging setup, environment management
- Location: `pkg/config/config.go`, `pkg/config/logging.go`
- Contains: Config struct with all application settings, logging initialization
- Depends on: Viper for configuration file parsing, Logrus for logging
- Used by: All layers

**Utility/Support Layer:**
- Purpose: Shared services like cryptography, rate limiting, S3 upload
- Location: `pkg/otp/`, `pkg/network/`
- Contains: TOTP generation, S3Mover for cloud uploads, rate limiting
- Depends on: AWS SDK for S3, standard crypto libraries
- Used by: Fleet, Server commands

**Protobuf Definition Layer:**
- Purpose: Define Meshtastic protocol messages and security inspector protocols
- Location: `protos/meshtastic/generated/`, `protos/security/generated/`
- Contains: Generated Go code from .proto files for message serialization
- Depends on: Google Protocol Buffers runtime
- Used by: MQTT layer, packet inspection logic

## Data Flow

**Server (Proxy Mode) - Incoming Packet Flow:**

1. Client connects to proxy port (default 0.0.0.0:1883)
2. ProxyListener (proxyproto) accepts connection → `handleProxy()`
3. Connection tracked in ConnTrack map with unique socket address
4. MQTT packet read from client via bufio.Reader
5. Packet parsed into InspectorPacket struct (contains metadata + raw MQTT packet)
6. `inspectRawPacket()` extracts MQTT details (username, password, clientID)
7. `PacketDecider.Decide()` evaluates rule-based inspector rules
8. Decision applied:
   - **Allow**: Forward packet to backend server at ProxyForwardAddress
   - **Block**: Log and drop packet
   - **Kill**: Disconnect client
   - **Slow**: Apply rate limiting penalty
9. Backend response forwarded back to client
10. InspectorLogger writes audit log entries
11. Log rotation triggers S3 upload via network.S3Mover

**Server (Protobuf Inspector Mode) - Incoming Connection Flow:**

1. gRPC client connects to InspectorListenAddress (default localhost:50051)
2. `handleProtobuf()` processes connection
3. Establishes bidirectional communication for remote packet inspection
4. Streams inspection results to client

**NodeInfo Announce Mode - Broadcast Flow:**

1. Connect to MQTT broker with credentials from config
2. Subscribe to configured topics (e.g., `msh/+/c/#`)
3. Load node database from disk
4. On-load broadcast triggered if BroadcastOnLoad enabled
5. Every BroadcastIntervalSec, trigger broadcast:
   - Send NodeInfo packet to subscribed topics
   - Await responses from nodes
6. NodeHandler callback processes incoming NodeInfo responses
7. Update local node database with neighbor info, position, metrics
8. On SIGINT/SIGTERM: graceful disconnect and exit

**Fleet Simulation Mode - Multi-Fleet Broadcast Flow:**

1. Initialize N fleets based on config (one MQTT client per fleet)
2. For each fleet:
   - Create virtual nodes based on NodesPerRampInterval config
   - Load or create node database
   - Ramp up phase: gradually add nodes with staggered start times
   - Steady state phase: nodes broadcast continuously
   - Ramp down phase: gracefully reduce active nodes
3. Each node broadcasts:
   - NodeInfo packets at configured intervals with jitter
   - Position updates (with simulated GPS drift)
   - Text messages (optional, if ChatBot configured)
   - Telemetry data (optional)
4. Behaviors applied per node: small, friendly, etc. (affects message frequency/content)
5. GPX track files loaded to define node movement patterns
6. OTP unlock mechanism protects certain features
7. On completion or SIGINT: graceful shutdown and database persistence

**State Management:**

- **NodeDB**: Flat map (uint32 nodeID → Node struct) maintained per command instance, synchronized via mutex
- **ConnTrack**: Maps connection socket address → ClientID for proxy mode connection tracking
- **MqttClient**: Single instance per operational context, maintains cipher state and handlers
- **Config**: Singleton loaded at startup, read-only after initialization
- **Limiters**: Per-connection rate limiting with token bucket algorithm

## Key Abstractions

**Decider Interface (Packet Inspection):**
- Purpose: Pluggable decision-making for packet handling (allow/block/modify/slow/kill)
- Examples: `internal/app/server/decider.go`
- Pattern: RuleBasedDecider implements Decider, evaluates ordered rules against InspectorPacket
- Rules are composable functions that match packet properties and return action

**InspectorPacket:**
- Purpose: Unified representation of both MQTT and Meshtastic protocol layers
- Location: `internal/app/server/inspect.go`
- Pattern: Holds both raw MQTT packet + parsed Meshtastic Data, metadata about connection
- Used for rule matching and logging

**MqttClient:**
- Purpose: Abstraction over Eclipse Paho MQTT client with Meshtastic-specific handling
- Location: `internal/mqtt/mqtt.go`
- Pattern: Encapsulates cipher setup, message dispatching, encryption/decryption, PKI handling
- Provides SetMessageHandler(), SetAckHandler(), SetNackHandler() for extensibility

**NodeDB:**
- Purpose: In-memory node directory (type alias: map[uint32]Node)
- Location: `internal/mqtt/node.go`
- Pattern: Simple key-value store with JSON marshaling for persistence to disk
- Tracks node metadata (position, battery, firmware, neighbors)

**CommandBuilder:**
- Purpose: Fluent API for defining Cobra commands and global flags
- Location: `internal/app/cmdargs.go`
- Pattern: Builder pattern with GlobalS/GlobalB/GlobalI methods for string/bool/int flags
- Provides environment variable mapping and flag validation

## Entry Points

**Main:**
- Location: `cmd/meshtk.go`
- Triggers: Executable invocation
- Responsibilities: Create Config, instantiate App, call Run()

**App.Run():**
- Location: `internal/app/app.go`
- Triggers: After command-line args parsed
- Responsibilities: Parse flags, map environment variables, setup logging, execute Cobra command tree

**ServerCmd.ProxyServer():**
- Location: `internal/app/server/cmd.go`
- Triggers: `meshtk server proxy` command
- Responsibilities: Start TCP listener on proxy port, accept connections, route to handleProxy

**ServerCmd.ProtobufServer():**
- Location: `internal/app/server/cmd.go`
- Triggers: `meshtk server protobuf` command
- Responsibilities: Start gRPC listener, enable remote packet inspection

**FleetCmd.Simulate():**
- Location: `internal/app/fleet/cmd.go`
- Triggers: `meshtk fleet simulate` command
- Responsibilities: Initialize virtual nodes, run simulation lifecycle (ramp up/steady/ramp down)

**NodeInfoCmd.Announce():**
- Location: `internal/app/nodeinfo/cmd.go`
- Triggers: `meshtk nodeinfo announce` command
- Responsibilities: Connect to MQTT, broadcast NodeInfo, process responses

## Error Handling

**Strategy:** Mix of fatal early termination and logged errors with fallback behavior

**Patterns:**

- **Fatal Errors**: Configuration loading failures (invalid encryption keys, missing MQTT broker) → `log.Fatalf()` → exit(1)
- **Connection Errors**: MQTT connection lost → log warning, auto-reconnect with 5-second backoff (configured in Paho client)
- **Decryption Errors**: Failed to decrypt packet → log warning, trigger NACK response, continue processing
- **Packet Parse Errors**: Invalid protobuf → log debug message, skip packet
- **S3 Errors**: Bucket upload failure → log error, retry on next rotation, log warning to console
- **Rule Processing Errors**: No matching rule → NoMatch decision, allow by default (safe-fail)

## Cross-Cutting Concerns

**Logging:**
- Framework: Logrus with custom formatters
- Levels: Trace (debug details), Debug, Info, Warn, Error, Fatal
- Inspector mode uses separate logger to file with timestamp-based rotation
- Verbose flag controls main logger level (VerboseLevel = "trace"/"debug"/"info")

**Validation:**
- Config file validation happens at startup via Viper
- MQTT username/password checked per-packet in rules (RequireMQTTUserName rule)
- Encryption key format validated at server startup
- Node IDs validated as uint32 (Meshtastic standard)

**Authentication:**
- MQTT broker authentication via username/password in config
- PKI decryption for sensitive messages using ECDH + AES-256
- OTP-based unlock mechanism for fleet operations (TOTP-based)
- Node identification via radio ID (uint32 from)

---

*Architecture analysis: 2026-03-10*
