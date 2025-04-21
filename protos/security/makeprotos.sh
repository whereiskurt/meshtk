#!/bin/bash

##Generate golang protobufs
rm -rf generated 
mkdir -p generated
protoc -I=.  --go-grpc_out=. --go_out=./ meshtastic*.proto

##Generate c protobufs with nanopb
rm -rf c
mkdir -p c
protoc -I=. --nanopb_out=./c/ meshtastic*.proto
cp /opt/homebrew/Cellar/nanopb/0.4.9.1_1/include/nanopb/* c/
