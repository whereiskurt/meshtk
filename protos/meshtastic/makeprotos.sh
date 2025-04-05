#!/bin/bash

rm -fr protobufs generated

git clone --progress --depth 1 https://github.com/meshtastic/protobufs.git
cd protobufs
patch nanopb.proto < ../go_package.patch
protoc -I=. --go_out=../ --go_opt=module=github.com/meshtastic/go meshtastic/*.proto
patch ../generated/deviceonly.pb.go < ../no_init.patch
cd ..
