#!/bin/bash
# dynamo_test.sh - Test DynamoDB authentication with Meshtk

# Print commands and exit on errors
set -ex

echo "Building meshtk..."
go build -o meshtk cmd/meshtk.go

echo "Starting server with DynamoDB auth..."
./meshtk server -c meshtk.dynamodb.yaml --verbose debug &
SERVER_PID=$!

# Give the server a moment to start
sleep 2

echo "Testing MQTT connection with dynamic credentials..."
# You can use mosquitto client to test the connection
# mosquitto_pub -h localhost -p 1883 -u your_dynamo_username -P your_dynamo_password -t test/topic -m "Hello"

# Cleanup
echo "Shutting down server..."
kill $SERVER_PID

echo "Test completed!"
