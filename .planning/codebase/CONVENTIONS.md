# Coding Conventions

**Analysis Date:** 2026-03-10

## Naming Patterns

**Files:**
- Lowercase with underscores for multi-word files
- Examples: `cmd.go`, `handlers.go`, `decider.go`, `logging.go`
- Package-scoped files grouped by functional domain (e.g., `nodeinfo/`, `server/`, `fleet/`)

**Functions:**
- PascalCase for exported functions (public API): `NewNodeInfo()`, `Help()`, `Announce()`
- camelCase for unexported functions: `initNodeDb()`, `flushNodeDb()`, `makeFleet()`
- Constructor functions follow pattern `New[Type]()`: `NewNodeInfo()`, `NewServer()`, `NewFleets()`
- Methods use receiver shorthand, single or double letters: `(n *NodeInfoCmd)`, `(f *FleetCmd)`, `(c *CmdBuilder)`

**Variables:**
- camelCase for local variables and parameters: `nodeIDs`, `randIndices`, `backendAddress`
- ALL_CAPS for constants: `PString`, `PInt`, `PBool` (type constants), `Allow`, `Block`, `Rewrote` (decision enums)
- Uppercase for exported struct fields: `Config`, `CommandBuilder`, `RootCmd`
- Lowercase for unexported struct fields: `connTrack`, `ciphers` (accessed via receiver pattern)

**Types:**
- PascalCase for struct names: `NodeInfoCmd`, `ServerCmd`, `FleetCmd`, `CmdBuilder`
- PascalCase for interface names: `Decider` (one method interfaces)
- Descriptive suffixes for command types: `*Cmd` suffix (e.g., `ServerCmd`, `FleetCmd`)
- Suffixes for result types: `Result` (e.g., `DecisionResult`)

## Code Style

**Formatting:**
- Go standard formatting (implicit via `gofmt`)
- Indentation: tabs (Go standard)
- Line length: no hard limit enforced, follows Go conventions
- Spacing: single blank line between functions, double for logical blocks

**Linting:**
- No explicit linter configuration found (`.golangci.yml` not present)
- Code follows standard Go conventions and idioms
- Defer statements paired with resource acquisition immediately

**Import Organization:**
1. Standard library imports: `os`, `fmt`, `time`, `net`, etc.
2. Third-party imports: `github.com/eclipse/paho.mqtt.golang`, `github.com/sirupsen/logrus`, etc.
3. Local imports: `github.com/whereiskurt/meshtk/internal/...`, `github.com/whereiskurt/meshtk/pkg/...`

**Path Aliases:**
- No path aliases detected in codebase
- Full import paths used throughout: `github.com/whereiskurt/meshtk/internal/app`

## Error Handling

**Patterns:**
- Explicit error checking: `if err != nil { ... }`
- Log and return pattern: `n.Config.Log.Errorf("message: %v", err); return`
- Log and continue pattern: `n.Config.Log.Warnf("warning: %v", err)`
- Panic for unrecoverable errors: `panic(fmt.Sprintf("message: %v", err))`
- Custom error types not used; standard error interface utilized

**Examples from codebase:**
```go
// From /Users/khundeck/working/meshtk/internal/app/server/cmd.go
if err := proto.Unmarshal(payload, &user); err != nil {
    n.Config.Log.Warnf(`{error: '%v', from: '%v', topic: '%v'}`, err, from, topic)
    return
}

// Panic for initialization failures
if err != nil {
    panic(err)
}
```

## Logging

**Framework:** `github.com/sirupsen/logrus`

**Patterns:**
- Centralized logger via `Config.Log` instance
- Logging configured in `pkg/config/logging.go`
- Five levels used: Trace, Debug, Info, Warn, Error (set via `VerboseLevel` config)
- Formatted logging with `f` suffix: `Infof()`, `Warnf()`, `Errorf()`, `Debugf()`, `Tracef()`
- JSON-style logging in some handlers: `` n.Config.Log.Tracef(`{from: '%v', topic: '%v'}`, from, topic) ``

**Usage examples:**
```go
n.Config.Log.Infof("Message with value: %v", value)
n.Config.Log.Tracef(`{'key': '%v', 'value': '%v'}`, key, value)
n.Config.Log.Errorf("Error: %v", err)
n.Config.Log.Fatalf("Fatal: %v", err)
```

## Comments

**When to Comment:**
- Function handlers/callbacks documented: `// NodeHandler is the callback function for handling incoming messages`
- Complex logic explained inline
- Commented-out code blocks retained (TODO-like patterns): Lines 23-62 in `/Users/khundeck/working/meshtk/internal/app/nodeinfo/handlers.go`
- No package-level comments found (not required by convention in this codebase)

**JSDoc/TSDoc:**
- Not used (Go project, uses godoc conventions if any)
- Function declarations don't have doc comments in all cases
- Receiver pattern comments sparse but present where needed

## Function Design

**Size:**
- Functions typically 30-100 lines
- Larger functions up to 300+ lines for complex operations (e.g., `StartProxyServer`, `NodeHandler`)
- Average: 40-60 lines

**Parameters:**
- Receiver pattern for methods (no `this` or `self`)
- 2-4 parameters typical
- Variadic not used extensively
- Struct receivers preferred: `(n *NodeInfoCmd) Method() { ... }`

**Return Values:**
- Single return for simple operations: `func Help() string`
- Multiple returns for functions that may fail: `func (f *FleetCmd) callOpenAIGPT() (string, error)`
- Named returns not used
- Error always last in multi-return: `(result T, err error)`

## Module Design

**Exports:**
- Public methods use PascalCase: `NewServer()`, `StartProtobufServer()`
- Private methods use camelCase: `loadCiphers()`, `initNodeDb()`
- Struct fields follow same convention

**Barrel Files:**
- No barrel files (index.go) used
- Each package imports explicitly what it needs
- Internal packages not re-exported at higher levels

**Package Structure:**
- Clear separation: `/cmd/` for main entry, `/internal/app/` for commands, `/internal/mqtt/` for messaging, `/pkg/` for shared utilities
- Each command gets its own package: `nodeinfo/`, `server/`, `fleet/`
- Shared config in `pkg/config/`

## Interface Design

**Example from codebase:**
```go
// From /Users/khundeck/working/meshtk/internal/app/server/decider.go
type Decider interface {
    Decide(packet *InspectorPacket) DecisionResult
}

type RuleBasedDecider struct {
    Rules []Rule
}

func (d *RuleBasedDecider) Decide(packet *InspectorPacket) DecisionResult {
    // Implementation
}
```

**Pattern:** Small interfaces with single responsibility, concrete types implement via receiver methods.

---

*Convention analysis: 2026-03-10*
