# meshtk - A Meshtatic virtual node Toolkit
A toolkit for virtual meshtastic nodes (ie. no radio/serial) using mqtt+protobufs. A work in progress (WIP) that's been useful for some upcoming projects (defcon.run!)

> "Release early, release often." 🐇  

To 'just run it': `go run cmd/meshtk.go nodeinfo announce --verbose trace` (but you'll like want to tweak the config 😉) 

You'll start to get a tonne of MQTT message tracing like this, from the public meshtastic mqtt server on a `#` subscription:
```
time="2025-04-05T13:38:28-04:00" level=trace msg="{mapReport: 'long_name:\"stats2\" short_name:\"st2s\" role:CLIENT_MUTE hw_model:PORTDUINO firmware_version:\"2.4.0.a04de8c6\" region:EU_868 latitude_i:510885888 longitude_i:168787968 altitude:113 position_precision:16 num_online_local_nodes:1'}"
time="2025-04-05T13:38:28-04:00" level=trace msg="{'from': '876654321', 'topic': 'msh/PL/2/map/', 'portNum': 'MAP_REPORT_APP', 'longName': 'stats2', 'shortName': 'st2s', 'hwModel': 'PORTDUINO', 'role': 'CLIENT_MUTE', 'fwVersion': '2.4.0.a04de8c6', 'region': 'EU_868', 'modemPreset': 'LONG_FAST', 'hasDefaultCh': false, 'onlineLocalNodes': 1, 'latitude': 510885888, 'longitude': 168787968, 'altitude': 113, 'precision': 16}"
time="2025-04-05T13:38:28-04:00" level=trace msg="{'from': 862341856, 'topic': 'msh/US/2/e/LongFast/!33664ae0', 'portNum': NODEINFO_APP, 'isEncrypted': false, 'payload': '0x0a09213333363634616530120d544f46204d65736820426173651a04544f4652220664e833664ae0282b3803'}"
time="2025-04-05T13:38:28-04:00" level=trace msg="{'from': '862341856', 'to': '!33664ae0', 'topic': 'msh/US/2/e/LongFast/!33664ae0', 'portNum': 'NODEINFO_APP', 'longName': 'TOF Mesh Base', 'shortName': 'TOFR', 'hwModel': 'HELTEC_V3', 'role': 'ROUTER_CLIENT', 'pubkey': '[]'}"
time="2025-04-05T13:38:28-04:00" level=error msg="failed to unmarshal decrypted data: proto: cannot parse invalid wire-format data"
```

<img width="886" alt="meshtk trace execution example" src="https://github.com/user-attachments/assets/83e6c3b5-440e-46db-8230-236f111cc10e" />

This adds the node (defined in `meshtk.yaml`) to the public meshtastic mqtt server and will show-up here:

<img width="579" alt="meshmap showing added virtual node" src="https://github.com/user-attachments/assets/a8c3bce6-776e-4d8d-8318-a38661c0e420" />

### Status/Progress
Some details of the on meshtastic features in progress:
1. ✅ Works with with TTL/SSL (e.g. ssl://example.com:8883)
1. ✅ Decrypt/encrypt messages text channels with PSK AES (ie. AQ== or [32bytes hex encoded])
1. ✅ Creates golang meshtastic protobufs from meshtastic source repo
1. ✅ Maintains a node database with pubkey
1. ✅ Trace logging with '--verbose trace' inside of `client.log` and `message_ledger.log`
1. ⚠️ TODO: Private chat messages supporting PKI 
1. ⚠️ TODO: Interactive user responses/tracking in public channel
1. ⚠️ TODO: One-time-password protections for bot commands

I personally like `golang` for command line interace tools - compiling a single static-linked executable is easy. Obviously I'll get ChatGPT to rewrite this in rust later. 🧌 🤡

# Motivation 🐇
tl;dr For "reasons" we want interactive meshtastic bots that nodes can interact with over MQTT. We wanted a purely virtual meshtastic node (ie. no radio required) that could broadcast a location to appear on maps, be able to read/response to encrypted channel messages and to use PKI to send/receive private messages. The idea is someone could post to a public channel, with a OTP an action request, and a bot on the internet listening on mqtt would take action.

# Technical
The code uses a bunch of `golang` conventions and the `viper/cobra` for the configuration management. We pull and build the [latest protobufs from meshtastic](https://github.com/meshtastic/protobufs) to have the golang structures to put over the wire using [mqtt](https://github.com/eclipse-paho/paho.mqtt.golang)). 

We are use three basic message types for location and sharing pubkey details: `NODEINFO_APP`, `POSITION_APP`, `MAP_REPORT_APP`.  Publishing a plaintext/encrypted message just requires the appropriate `MeshPacket` contstuction either with `Decoded` payload or an `Encrypted` payload.

## Configuration
Inside the `pkg/config/mesktk.yaml` you'll see all of the options possible and defaults. Overwrite it, copy it to your home folder (~) or put it the local execution folder (./) named `mesktk.yaml`. If you overwrite this before doing a `go build cmd/meshtk.go` you can bake your config in. 👍

This config connects to the default meshtastic MQTT servers:
```yaml
NodeDbPath: "./nodes.default.json"

Mqtt:
  BrokerUri: "tcp://mqtt.meshtastic.org:1883"
  Username: "meshdev"
  Password: "large4cats"
  ClientId: "meshtk-abcd1234-432453"

Meshtastic:
  Channels:
    - Slot: "primary"
      Name: "LongFast"
      EncryptKey: "AQ=="
      IsEncrypted: true
      IsPrimary: true

NodeInfo:
  ClientId: "!28a1b2c3"
  LongName: "Mesh Toolkit 2025"
  ShortName: "MTK"
  HWModelId: 43
  RoleId: 0
  Latitude: 361354763
  Longitude: -1151597904
  Altitude: 420
  Precision: 32
```

For TLS (not support by default meshtastic servers) set values like this:
```yaml
Mqtt:
  BrokerUri: "ssl://mqtt.example.com:8883"
```

## Generated code + protobufs
Protobufs are the binary definitions of the packets/service envelopes that go over-the-wire for meshtastic. They are necessary for interoperability, and basically the 'schema' for communcations.

Reviewing the `protos\meshtastic` package you see a `makeprotos.sh` which git clones the latest meshtastic protobufs, applies some 'patches' to make the go build smooth, and them builds them for go. The output is commited for your convenience in `protos\meshtastic\generated` and referred to in the code base like this:

```golang
import (
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)
```

# Go Structure Chatter
This is kinda 'inside my head' from doing Golang over the years. There is a separation been the 'commandline' (`cmd`) and the 'application' (`internal\app`). The `cmd` is a way to specify the configruation options and pass execution - that's it. That's why the `main` looks like this:

```golang
func main() {
	app := app.NewApp(config.NewConfig())
	app.Run().Destroy()
	os.Exit(0)
}
```

The configuration and environment variables are all managed by `viper/cobra` package and constructed in our `config` package. 

In this structure, running `go run cmd/meshtk.go nodeinfo announce` triggers code inside of `internal/app/nodeinfo` and passes in a prefilled `pkg/config` object merged from `~/meshtk.yaml`, `./meshtk.yaml` and/or `-c <filename>`. 

```
├── cmd                   <-- golang best practice - command shell
├── internal
│   ├── app
│   │   ├── help          <-- help files/details
│   │   └── nodeinfo      <-- nodeinfo related app logic
│   └── mqtt              <-- MQTT logic for meshtastic concepts
├── pkg
│   └── config            <-- global configuration used in internal
├── protos
│   └── meshtastic
│       ├── generated.     <-- golang generated meshtastic protobufs
│       └── protobufs      <-- The meshtastic protobufs project 
│           └── meshtastic
```

More features will be added the `internal\app\FEATURE` folders will build out.

## Shoutouts!
These are some many amazing projects that I read and took inspiration from:
1. https://github.com/eclipse-paho/paho.mqtt.golang
1. https://github.com/brianshea2/meshmap.net
1. https://github.com/liamcottle/meshtastic-map
1. https://github.com/TheCommsChannel/TC2-BBS-mesh

I appreciate folks sharing and that's inspired me to share back, too. 🙏🐇
