package credcache

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a credential lookup finds no matching entry.
var ErrNotFound = errors.New("credential not found")

// Credential holds MQTT credential data from DynamoDB.
type Credential struct {
	Username string `dynamodbav:"mqttUsername"`
	Password string `dynamodbav:"mqttPassword"`
	Usertype string `dynamodbav:"mqttUsertype"`
	Negative bool   // Not stored in DynamoDB — marks negative cache entries.
}

// CredentialStore defines the interface for fetching credentials from a backend.
type CredentialStore interface {
	Fetch(ctx context.Context, username string) (*Credential, error)
}
