# Codebase Structure

**Analysis Date:** 2026-03-10

## Directory Layout

```
meshtk/
├── cmd/                          # Entry points
│   └── meshtk.go                 # Main executable entry point
├── internal/                      # Private application code
│   ├── app/                       # Command implementations
│   │   ├── nodeinfo/              # Node announcement feature
│   │   ├── server/                # Proxy/inspector server feature
│   │   ├── fleet/                 # Fleet simulation feature
│   │   ├── help/                  # Help text templates
│   │   ├── app.go                 # App lifecycle and config wiring
│   │   └── cmdargs.go             # Cobra command builder
│   ├── mqtt/                      # Meshtastic/MQTT protocol layer
│   └── embedded/                  # Embedded assets (GPX tracks)
├── pkg/                           # Public/reusable packages
│   ├── config/                    # Configuration management
│   ├── otp/                       # TOTP generation
│   └── network/                   # Network utilities (S3, rate limit)
├── protos/                        # Protocol Buffer definitions
│   ├── meshtastic/generated/      # Generated Meshtastic protobuf code
│   └── security/generated/        # Generated gRPC inspector protobuf code
├── logs/                          # Runtime log directory
├── .planning/                     # GSD planning artifacts
│   └── codebase/                  # Codebase analysis documents
├── go.mod                         # Go module definition
├── go.sum                         # Go dependency checksums
├── meshtk.yaml                    # Default configuration (embedded)
├── meshtk.*.yaml                  # Environment-specific configs
└── nodes.*.json                   # Node database snapshots
```

## Directory Purposes

**`cmd/`:**
- Purpose: Main application entry points
- Contains: Single main package with minimal logic
- Key files: `meshtk.go` - creates config and app, calls Run()

**`internal/app/`:**
- Purpose: Core application business logic and command handlers
- Contains: Three command modules (nodeinfo, server, fleet) + CLI infrastructure
- Key files:
  - `app.go` - App struct, lifecycle (NewApp, Run, Destroy)
  - `cmdargs.go` - CmdBuilder for fluent Cobra command registration

**`internal/app/nodeinfo/`:**
- Purpose: NodeInfo announcement and gathering feature
- Contains: NodeInfoCmd handler, node message processing, broadcast logic
- Key files:
  - `cmd.go` - Help and Announce command handlers
  - `handlers.go` - NodeHandler callback, broadcast implementation

**`internal/app/server/`:**
- Purpose: MQTT proxy and packet inspection/filtering server
- Contains: Proxy listener, rule engine, packet inspection logic, log rotation
- Key files:
  - `cmd.go` - Help, ProtobufServer, ProxyServer handlers, S3 integration
  - `proxy.go` - TCP socket handling, MQTT packet forwarding
  - `inspect.go` - InspectorPacket struct, packet parsing, metadata extraction
  - `decider.go` - Decider interface, RuleBasedDecider implementation
  - `rules.go` - Inspection rules (allow/block logic) and rewrite rules
  - `protobuf.go` - gRPC handler for remote packet inspection

**`internal/app/fleet/`:**
- Purpose: Multi-node fleet simulation for testing
- Contains: Fleet lifecycle, node simulation behaviors, GPX parsing
- Key files:
  - `cmd.go` - Simulate command, fleet initialization
  - `simulate.go` - Ramp up/steady/ramp down phases
  - `nodes.go` - Virtual node creation and management
  - `behaviours.go` - Message frequency/content behaviors
  - `gpx.go` - GPX track parsing and node movement

**`internal/app/help/`:**
- Purpose: Help text and documentation templates
- Contains: Template files for command help output
- Key files: `help.go` - Render functions for global/command-specific help

**`internal/mqtt/`:**
- Purpose: Meshtastic/MQTT protocol layer
- Contains: MQTT client wrapper, message encryption/decryption, node database
- Key files:
  - `mqtt.go` - MqttClient struct, dispatcher, connection management, PKI decryption
  - `publish.go` - Publishing helper functions for NodeInfo/Position/Text
  - `crypto.go` - Encryption/decryption utilities, AES-256-CTR, PKI (ECDH)
  - `node.go` - Node struct, NodeDB type, node metadata persistence
  - `handlers.go` - (separate message handlers for different port nums)

**`pkg/config/`:**
- Purpose: Configuration loading and initialization
- Contains: Config struct definition, YAML parsing, logging setup
- Key files:
  - `config.go` - Config struct, NewConfig, YAML unmarshal, env var binding
  - `logging.go` - Logrus initialization, formatter setup
  - `meshtk.yaml` (embedded) - Default configuration template

**`pkg/otp/`:**
- Purpose: One-time password (TOTP) generation
- Contains: TOTP configuration and token generation
- Key files: `totp.go` - TOTPConfig, token generation logic

**`pkg/network/`:**
- Purpose: Network utilities and cloud integration
- Contains: S3 file upload, rate limiting
- Key files:
  - `s3mover.go` - S3Mover struct, file upload to AWS S3
  - `limiter.go` - Token bucket rate limiter

**`protos/`:**
- Purpose: Protocol buffer definitions and generated code
- Contains: .pb.go files generated from .proto schemas
- Key subdirectories:
  - `meshtastic/generated/` - Meshtastic protocol messages (mesh.pb.go, mqtt.pb.go, etc.)
  - `security/generated/` - Inspector gRPC service definitions

**`logs/`:**
- Purpose: Runtime log files storage
- Contains: Application logs and inspector audit logs
- Generated: Yes (created at runtime)
- Committed: No

## Key File Locations

**Entry Points:**
- `cmd/meshtk.go`: Binary entry point - 15 lines, initializes Config and App
- `internal/app/app.go`: App lifecycle - NewApp, Run, ParseFlags, MapEnvVars, Destroy
- `internal/app/cmdargs.go`: Cobra command tree registration - RegisterOsArgs defines all commands

**Configuration:**
- `pkg/config/config.go`: Config struct with embedded default YAML (meshtk.yaml)
- `pkg/config/logging.go`: Logrus setup with file and stdout writers
- `meshtk.yaml`: Embedded default config (loaded via go:embed directive)
- `meshtk.*.yaml`: Runtime-loaded environment-specific configs (liamcottle, defcon, sslexample)

**Core Logic:**
- `internal/mqtt/mqtt.go`: MqttClient - 323 lines, dispatcher, encryption, PKI handling
- `internal/app/server/cmd.go`: ServerCmd - 331 lines, proxy/protobuf servers, S3 upload
- `internal/app/server/proxy.go`: Proxy connection handling and inspection
- `internal/app/server/rules.go`: Inspection rule definitions (Allow, Block, Kill, Slow decisions)
- `internal/app/fleet/cmd.go`: FleetCmd - fleet initialization and simulation
- `internal/app/nodeinfo/cmd.go`: NodeInfoCmd - announce and listen commands

**Testing/Data:**
- `nodes.*.json`: Snapshot node databases (12 different configurations)
- `internal/embedded/gpx/`: Embedded GPX track data for fleet simulation
  - `dc33/`, `ghosts/`, `city/` - Different geographic routes

**Infrastructure:**
- `go.mod`: Go 1.24.1, 13 direct dependencies (paho.mqtt, logrus, cobra, viper, aws-sdk, etc.)
- `.planning/codebase/`: GSD planning documents (ARCHITECTURE.md, STRUCTURE.md, etc.)

## Naming Conventions

**Files:**
- `cmd.go` - Command handler implementations (Help, Announce, Simulate, etc.)
- `handlers.go` - Message/callback handlers
- `*.pb.go` - Generated protobuf code
- `meshtk.yaml` - Configuration files with environment suffix
- `nodes.*.json` - Node database files with context suffix

**Directories:**
- `internal/app/{nodeinfo,server,fleet}/` - Feature modules, named by command
- `internal/mqtt/` - Protocol/network layer
- `pkg/{config,otp,network}/` - Utilities/shared packages
- `protos/{meshtastic,security}/generated/` - Generated code (generated/ subdirectory)

**Packages:**
- Feature packages named by command: `nodeinfo`, `server`, `fleet`
- Utility packages named by concern: `config`, `mqtt`, `otp`, `network`
- All follow Go convention: lowercase, no underscores

**Go Types:**
- Command handlers: `{Feature}Cmd` e.g., ServerCmd, FleetCmd, NodeInfoCmd
- Interfaces: `{Concept}er` e.g., Decider
- Structs for data: PascalCase without suffix e.g., Node, InspectorPacket, ConnectionInfo
- Enums: UPPERCASE with meaningful names e.g., Decision (Allow, Block, Kill, Slow, NoMatch, Rewrote)

## Where to Add New Code

**New Feature (e.g., new command):**
- Primary code: `internal/app/{featurename}/cmd.go` - implement {Feature}Cmd struct and handlers
- Helper logic: `internal/app/{featurename}/` additional .go files as needed
- Register command: `internal/app/cmdargs.go` - add to RegisterOsArgs() method
- Configuration: Add struct fields to `pkg/config/config.go` and meshtk.yaml

**New Packet Inspection Rule:**
- Add rule function: `internal/app/server/rules.go` - add new Rule to rewriteRules() or inspectRules()
- Rule pattern: Match function checks packet properties, Action is Allow/Block/Kill/Slow
- Rules evaluated in order (first match wins)

**New MQTT Handler/Utility:**
- Add to: `internal/mqtt/` - new .go file for logical grouping
- Depends on: MqttClient, Node, NodeDB (from same package)
- Registration: Hook into MqttClient via SetMessageHandler/SetAckHandler/SetNackHandler

**New Utility/Infrastructure:**
- Location: `pkg/{category}/` where category is concern (config, network, otp, etc.)
- Pattern: Self-contained package with minimal external dependencies
- Exported: Public APIs for use across internal/

**Configuration Addition:**
- Struct field: Add to relevant struct in `pkg/config/config.go` with `default:` tag
- YAML key: Add to embedded `meshtk.yaml` template in `pkg/config/`
- Environment binding: Use CmdBuilder GlobalS/GlobalB/GlobalI in RegisterOsArgs() if flag needed

## Special Directories

**`internal/embedded/gpx/`:**
- Purpose: Embedded GPX track files for fleet simulation movement
- Contents: XML-formatted track definitions (city, ghosts, dc33 locations)
- Generated: No (committed static assets)
- Committed: Yes
- Access: Via go:embed directive in fleet code

**`protos/meshtastic/generated/`:**
- Purpose: Generated Go code for Meshtastic protocol messages
- Contents: 20+ .pb.go files from .proto definitions (mesh, mqtt, module_config, admin, etc.)
- Generated: Yes (from .proto files via protoc)
- Committed: Yes (for ease of build without protoc dependency)
- Usage: Import meshtastic package for ServiceEnvelope, MeshPacket, Data types

**`protos/security/generated/`:**
- Purpose: Generated gRPC service code for remote packet inspection
- Contents: meshtastic_inspector.pb.go, meshtastic_inspector_grpc.pb.go
- Generated: Yes (from .proto files)
- Committed: Yes
- Usage: gRPC server/client for protobuf inspection mode

**`.planning/codebase/`:**
- Purpose: GSD (Generate-Scaffold-Deploy) codebase analysis documents
- Contents: ARCHITECTURE.md, STRUCTURE.md, CONVENTIONS.md, TESTING.md, CONCERNS.md
- Generated: Yes (by GSD analysis tools)
- Committed: Yes (guides future development)

---

*Structure analysis: 2026-03-10*
