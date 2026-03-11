# External Integrations

**Analysis Date:** 2026-03-10

## APIs & External Services

**Meshtastic MQTT Broker:**
- mqtt.meshtastic.org:1883 - Meshtastic mesh network MQTT broker
  - SDK/Client: github.com/eclipse/paho.mqtt.golang v1.5.0
  - Auth: Username (`Mqtt.Username`) and password (`Mqtt.Password`) from config
  - Topics: Configurable mesh topics (default: `msh/US/2/e/LongFast`)
  - Implementation: `internal/mqtt/mqtt.go` (MqttClient struct)

**Meshtastic Protocol:**
- gRPC and Protocol Buffers for Meshtastic node communication
  - SDK/Client: google.golang.org/grpc v1.71.1, google.golang.org/protobuf v1.36.5
  - Message types: NodeInfo, TextMessage, Position, Telemetry (in `protos/meshtastic/generated/`)
  - Port: 50051 (configurable at `Server.InspectorListenAddress`)

## Data Storage

**Databases:**
- JSON-based Node Database (local filesystem)
  - Connection: File path from `NodeDbPath` config (default: `./nodes.default.json`)
  - Client: Custom JSON unmarshaling in `internal/mqtt/node.go`
  - Format: JSON arrays of node metadata

**File Storage:**
- Local filesystem for logs and configuration
  - Log directory: `LogFolder` config (default: `logs/`)
  - Log file naming: `YYYYMMDD.client.log`
  - Block list files: Template-based names (e.g., `blocklist.20260310.log`)

**S3 (Optional but Primary):**
- AWS S3 for blocklist log archival
  - Service: Amazon S3
  - Client: github.com/aws/aws-sdk-go v1.55.7
  - Bucket: `Server.S3BucketName` (default: `meshtk-blocklist-20250101`)
  - Region: `Server.S3BucketRegion` (default: `us-east-1`)
  - Key prefix: `Server.S3BucketPrefix` (default: `meshtk/blocklist`)
  - Upload handler: `pkg/network/s3mover.go` (S3Mover struct)
  - Enabled: `Server.UseS3Bucket` (default: true)

**Caching:**
- None detected

## Authentication & Identity

**Auth Provider:**
- Custom - Meshtastic mesh PKI
  - Implementation: Elliptic curve ECDH for key exchange
  - PKI keys: `NodeInfo.PKI.PrivateKey`, `NodeInfo.PKI.PublicKey` (hex-encoded, 32 bytes)
  - Used for: Message encryption and node authentication in `internal/mqtt/crypto.go`
  - AES-256 encryption for message payloads (base64-encoded channel keys)

**MQTT Authentication:**
- Username/password credentials to Meshtastic public MQTT broker
  - Config: `Mqtt.Username`, `Mqtt.Password`
  - Default: username `meshdev`, password configured in YAML

**AWS Credentials:**
- Credential chain for S3 access (implemented in `pkg/network/s3mover.go`)
  - Order: Environment variables → IAM roles → Shared credentials
  - ECS support: Task role credentials via `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` or `AWS_CONTAINER_CREDENTIALS_FULL_URI`
  - EC2 support: IMDSv2 via instance role (auto-disabled in ECS to prevent timeout)
  - Local dev: AWS profile from `~/.aws/credentials` or `AWS_PROFILE` env var
  - Verbose error messages for credential chain failures

## Monitoring & Observability

**Error Tracking:**
- None detected (no external error tracking service)

**Logs:**
- File-based logging with Logrus v1.9.3
  - Output: `logs/{YYYYMMDD}.client.log`
  - Levels: error, warn, info, debug, trace (configurable)
  - Format: TextFormatter (human-readable)
  - Console output: Enabled for debug/trace levels
  - Inspector logger: Separate logger for server proxy events (file-based)

**Telemetry:**
- OpenTelemetry SDK v1.34.0 (initialized but not fully integrated)
  - Packages: go.opentelemetry.io/otel, otel/trace, otel/metric, otel/sdk

## CI/CD & Deployment

**Hosting:**
- AWS ECS (Elastic Container Service) - Primary deployment target
- AWS EC2 - Fallback/alternative deployment
- Standalone servers - Via local binary execution
- Container detection and credential handling in `pkg/network/s3mover.go`

**CI Pipeline:**
- None detected in codebase (likely external, GitHub Actions or similar)

## Environment Configuration

**Required env vars:**
- `AWS_REGION` or `AWS_DEFAULT_REGION` - For S3 operations (critical in ECS)
- `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` or `AWS_CONTAINER_CREDENTIALS_FULL_URI` - ECS task role (if using ECS)
- `AWS_EC2_METADATA_DISABLED=true` - Set automatically in ECS to prevent IMDSv2 timeout

**Optional env vars (override config):**
- `MESHTK_MQTT_BROKERURI` - MQTT broker address
- `MESHTK_MQTT_USERNAME` - MQTT username
- `MESHTK_MQTT_PASSWORD` - MQTT password
- `MESHTK_MQTT_CLIENTID` - MQTT client ID
- `MESHTK_SERVER_S3BUCKETNAME` - S3 bucket name
- `MESHTK_SERVER_S3BUCKETREGION` - S3 bucket region
- `MESHTK_SERVER_USESBUCKET` - Enable/disable S3 (true/false)
- `MESHTK_SERVER_SHOULDLOGBLOCKS` - Log blocked packets
- `MESHTK_SERVER_SHOULDLOGALLOWS` - Log allowed packets

**Secrets location:**
- `.env` file (if present) - Not recommended, use AWS Secrets Manager for production
- `~/.aws/credentials` - AWS credentials file for local development
- Environment variables - Preferred for ECS deployments
- Task definition secrets - For sensitive config values in ECS

## Webhooks & Callbacks

**Incoming:**
- MQTT message handlers via Eclipse Paho client
  - Default topics: `msh/US/2/e/LongFast`, `msh/+/+/2/map`, etc.
  - Handler registration: `MqttClient.SetMessageHandler()` in `internal/mqtt/mqtt.go`
  - NACK handler: `SetNackHandler()` for retransmit notification
  - ACK handler: `SetAckHandler()` for message acknowledgments

**Outgoing:**
- MQTT publish: `internal/mqtt/publish.go` for sending messages to broker
- S3 uploads: Automatic when blocklist files reach size threshold
- gRPC server: Inspector protocol responses on port 50051

---

*Integration audit: 2026-03-10*
