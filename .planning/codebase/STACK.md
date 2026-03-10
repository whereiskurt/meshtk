# Technology Stack

**Analysis Date:** 2026-03-10

## Languages

**Primary:**
- Go 1.24.1 - All backend services, CLI application, and core logic

**Secondary:**
- Protocol Buffers (protobuf) - Message serialization for Meshtastic protocol

## Runtime

**Environment:**
- Go 1.24.1

**Package Manager:**
- Go Modules (go.mod)
- Lockfile: `go.sum` (present)

## Frameworks

**Core CLI:**
- Cobra v1.9.1 - Command-line interface framework, root command structure defined in `internal/app/app.go`
- Viper v1.20.0 - Configuration management with YAML support and environment variable override

**MQTT:**
- Eclipse Paho MQTT Go Client v1.5.0 - MQTT broker connectivity at `internal/mqtt/mqtt.go`

**Protocol Buffers:**
- google.golang.org/protobuf v1.36.5 - Message marshaling/unmarshaling
- google.golang.org/grpc v1.71.1 - gRPC services for inspector protocol

**Logging:**
- Sirupsen Logrus v1.9.3 - Structured logging with file rotation, configured in `pkg/config/logging.go`

**Build/Dev:**
- None detected - single binary compiled directly

## Key Dependencies

**Critical:**
- github.com/aws/aws-sdk-go v1.55.7 - AWS S3 integration for blocklist log upload, credential handling for ECS/EC2 environments
- golang.org/x/crypto v0.36.0 - Cryptographic functions (AES-256, ECDH) for message encryption
- google.golang.org/protobuf v1.36.5 - Protocol buffer code generation and runtime

**Infrastructure:**
- github.com/pires/go-proxyproto v0.8.0 - PROXY protocol handling for reverse proxy scenarios
- golang.org/x/sync v0.12.0 - Concurrency primitives
- golang.org/x/net v0.38.0 - Network utilities
- github.com/mitchellh/go-homedir v1.1.0 - Cross-platform home directory detection
- github.com/go-viper/mapstructure/v2 v2.2.1 - YAML to struct mapping

**Telemetry/Observability:**
- go.opentelemetry.io/* v1.34.0 - OpenTelemetry for distributed tracing (auto SDK, OTEL, metrics, tracing)

## Configuration

**Environment:**
- YAML configuration files at `pkg/config/meshtk.yaml` (embedded default config)
- User config locations: `$HOME/meshtk.yaml` or `./meshtk.yaml` (current directory)
- Custom config via `-c` flag: `app -c /path/to/config.yaml`
- Environment variable override with prefix `MESHTK_` (e.g., `MESHTK_MQTT_USERNAME`)

**Key configs required:**
- MQTT broker credentials: `Mqtt.BrokerUri`, `Mqtt.Username`, `Mqtt.Password`
- Node identity: `NodeInfo.ClientId`, `NodeInfo.LongName`, `NodeInfo.ShortName`
- S3 bucket: `Server.S3BucketName`, `Server.S3BucketRegion` (if `Server.UseS3Bucket=true`)
- Encryption keys: `Meshtastic.Channels[].EncryptKey` (base64-encoded)
- PKI keys: `NodeInfo.PKI.PrivateKey`, `NodeInfo.PKI.PublicKey` (hex-encoded)

**Build:**
- No build configuration files detected (builds via `go build ./cmd/meshtk.go`)

## Platform Requirements

**Development:**
- Go 1.24.1 or later
- macOS, Linux, or Windows (cross-platform support via `golang.org/x/sys` abstractions)
- AWS credentials (for local S3 testing): `~/.aws/credentials` or environment variables

**Production:**
- Deployment targets: ECS (Elastic Container Service), EC2, or standalone servers
- AWS credentials chain for S3 access (ECS task role recommended over EC2 IMDSv2)
- MQTT broker accessibility (default: tcp://mqtt.meshtastic.org:1883)
- gRPC port 50051 for inspector protocol (configurable at `Server.InspectorListenAddress`)
- MQTT proxy port 1883 for client connections (configurable at `Server.ProxyListenAddress`)

---

*Stack analysis: 2026-03-10*
