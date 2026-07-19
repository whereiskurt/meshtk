package keycache

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// mockDynamoDBClient implements DynamoDBAPI for testing (GetItem, never Scan).
type mockDynamoDBClient struct {
	getOutput *dynamodb.GetItemOutput
	getError  error
	// captured holds the last GetItemInput for inspection.
	captured *dynamodb.GetItemInput
}

func (m *mockDynamoDBClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	m.captured = params
	if m.getError != nil {
		return nil, m.getError
	}
	return m.getOutput, nil
}

func TestStoreFetchKnownNode(t *testing.T) {
	item := map[string]types.AttributeValue{
		"publicKey": &types.AttributeValueMemberS{Value: sampleHex},
		"nodeNum":   &types.AttributeValueMemberN{Value: "1128078572"}, // 0x433d1cec
	}

	mock := &mockDynamoDBClient{getOutput: &dynamodb.GetItemOutput{Item: item}}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	got, err := store.Fetch(context.Background(), 0x433d1cec)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if got.PubKeyHex != sampleHex {
		t.Errorf("PubKeyHex = %q, want %q", got.PubKeyHex, sampleHex)
	}
	if got.NodeNum != 0x433d1cec {
		t.Errorf("NodeNum = %#x, want %#x", got.NodeNum, uint32(0x433d1cec))
	}
	if got.NodeID != "!433d1cec" {
		t.Errorf("NodeID = %q, want %q", got.NodeID, "!433d1cec")
	}
}

func TestStoreFetchUnknownNode(t *testing.T) {
	// Empty Item map → ErrNotFound.
	mock := &mockDynamoDBClient{getOutput: &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{}}}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, err := store.Fetch(context.Background(), 0xdeadbeef)
	if err == nil {
		t.Fatal("Fetch() should return error for unknown node")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want ErrNotFound", err)
	}
}

func TestStoreFetchNilItem(t *testing.T) {
	// GetItem with no match returns a nil Item map → ErrNotFound.
	mock := &mockDynamoDBClient{getOutput: &dynamodb.GetItemOutput{Item: nil}}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, err := store.Fetch(context.Background(), 0x433d1cec)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Fetch() error = %v, want ErrNotFound for nil item", err)
	}
}

func TestStoreFetchErrorPropagation(t *testing.T) {
	expectedErr := errors.New("dynamodb connection failed")
	mock := &mockDynamoDBClient{getError: expectedErr}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, err := store.Fetch(context.Background(), 0x433d1cec)
	if err == nil {
		t.Fatal("Fetch() should propagate error")
	}
	if !errors.Is(err, expectedErr) {
		t.Errorf("Fetch() error = %v, want wrapped %v", err, expectedErr)
	}
}

func TestStoreFetchComposesKeyAndProjection(t *testing.T) {
	mock := &mockDynamoDBClient{getOutput: &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{}}}

	store := NewDynamoDBStoreWithClient(mock, "run-human-electro")
	_, _ = store.Fetch(context.Background(), 0x433d1cec)

	if mock.captured == nil {
		t.Fatal("GetItem was not called")
	}
	if mock.captured.TableName == nil || *mock.captured.TableName != "run-human-electro" {
		t.Errorf("TableName = %v, want run-human-electro", mock.captured.TableName)
	}

	pk, ok := mock.captured.Key["pk"].(*types.AttributeValueMemberS)
	if !ok || pk.Value != "$run#nodeid_!433d1cec" {
		t.Errorf("Key[pk] = %#v, want S \"$run#nodeid_!433d1cec\"", mock.captured.Key["pk"])
	}
	sk, ok := mock.captured.Key["sk"].(*types.AttributeValueMemberS)
	if !ok || sk.Value != "$meshradio_1" {
		t.Errorf("Key[sk] = %#v, want S \"$meshradio_1\"", mock.captured.Key["sk"])
	}

	if mock.captured.ProjectionExpression == nil {
		t.Fatal("ProjectionExpression should not be nil")
	}
	if *mock.captured.ProjectionExpression != "publicKey, nodeNum" {
		t.Errorf("ProjectionExpression = %q, want %q", *mock.captured.ProjectionExpression, "publicKey, nodeNum")
	}
}

// TestStoreUnmarshalsPublicKey confirms the 0x-hex publicKey attribute round-trips
// through attributevalue into Key.PubKeyHex (the value ParseHexKey consumes).
func TestStoreUnmarshalsPublicKey(t *testing.T) {
	var k Key
	item := map[string]types.AttributeValue{
		"publicKey": &types.AttributeValueMemberS{Value: sampleHex},
	}
	if err := attributevalue.UnmarshalMap(item, &k); err != nil {
		t.Fatalf("UnmarshalMap() error = %v", err)
	}
	if k.PubKeyHex != sampleHex {
		t.Errorf("PubKeyHex = %q, want %q", k.PubKeyHex, sampleHex)
	}
}
