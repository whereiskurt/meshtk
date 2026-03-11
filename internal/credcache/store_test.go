package credcache

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// mockDynamoDBClient implements DynamoDBAPI for testing.
type mockDynamoDBClient struct {
	scanOutput *dynamodb.ScanOutput
	scanError  error
	// captured holds the last ScanInput for inspection
	captured *dynamodb.ScanInput
}

func (m *mockDynamoDBClient) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	m.captured = params
	if m.scanError != nil {
		return nil, m.scanError
	}
	return m.scanOutput, nil
}

func TestStoreFetchKnownUser(t *testing.T) {
	cred := Credential{
		Username: "alice",
		Password: "abc123def456",
		Usertype: "rabbit",
	}
	item, err := attributevalue.MarshalMap(cred)
	if err != nil {
		t.Fatalf("MarshalMap() error = %v", err)
	}

	mock := &mockDynamoDBClient{
		scanOutput: &dynamodb.ScanOutput{
			Items: []map[string]types.AttributeValue{item},
			Count: 1,
		},
	}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	got, err := store.Fetch(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want %q", got.Username, "alice")
	}
	if got.Password != "abc123def456" {
		t.Errorf("Password = %q, want %q", got.Password, "abc123def456")
	}
	if got.Usertype != "rabbit" {
		t.Errorf("Usertype = %q, want %q", got.Usertype, "rabbit")
	}
}

func TestStoreFetchUnknownUser(t *testing.T) {
	mock := &mockDynamoDBClient{
		scanOutput: &dynamodb.ScanOutput{
			Items: []map[string]types.AttributeValue{},
			Count: 0,
		},
	}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, err := store.Fetch(context.Background(), "unknownuser")
	if err == nil {
		t.Fatal("Fetch() should return error for unknown user")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want ErrNotFound", err)
	}
}

func TestStoreFetchErrorPropagation(t *testing.T) {
	expectedErr := errors.New("dynamodb connection failed")
	mock := &mockDynamoDBClient{
		scanError: expectedErr,
	}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, err := store.Fetch(context.Background(), "alice")
	if err == nil {
		t.Fatal("Fetch() should propagate error")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("Fetch() error = %v, want wrapped %v", err, expectedErr)
	}
}

func TestStoreFetchFilterExpression(t *testing.T) {
	mock := &mockDynamoDBClient{
		scanOutput: &dynamodb.ScanOutput{
			Items: []map[string]types.AttributeValue{},
			Count: 0,
		},
	}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	store.Fetch(context.Background(), "testuser")

	if mock.captured == nil {
		t.Fatal("Scan was not called")
	}
	if mock.captured.FilterExpression == nil {
		t.Fatal("FilterExpression should not be nil")
	}
	if mock.captured.ExpressionAttributeNames == nil {
		t.Fatal("ExpressionAttributeNames should not be nil")
	}
	if mock.captured.ExpressionAttributeValues == nil {
		t.Fatal("ExpressionAttributeValues should not be nil")
	}

	// Verify the filter references mqttUsername via expression attribute names
	foundMqttUsername := false
	for _, v := range mock.captured.ExpressionAttributeNames {
		if v == "mqttUsername" {
			foundMqttUsername = true
			break
		}
	}
	if !foundMqttUsername {
		t.Errorf("ExpressionAttributeNames should reference mqttUsername, got %v", mock.captured.ExpressionAttributeNames)
	}
}

func TestStoreFetchProjectionExpression(t *testing.T) {
	mock := &mockDynamoDBClient{
		scanOutput: &dynamodb.ScanOutput{
			Items: []map[string]types.AttributeValue{},
			Count: 0,
		},
	}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	store.Fetch(context.Background(), "testuser")

	if mock.captured == nil {
		t.Fatal("Scan was not called")
	}
	if mock.captured.ProjectionExpression == nil {
		t.Fatal("ProjectionExpression should not be nil")
	}

	// Verify all three attributes are in the expression attribute names
	expectedAttrs := map[string]bool{
		"mqttUsername": false,
		"mqttPassword": false,
		"mqttUsertype": false,
	}
	for _, v := range mock.captured.ExpressionAttributeNames {
		if _, ok := expectedAttrs[v]; ok {
			expectedAttrs[v] = true
		}
	}
	for attr, found := range expectedAttrs {
		if !found {
			t.Errorf("ExpressionAttributeNames missing %q", attr)
		}
	}
}
