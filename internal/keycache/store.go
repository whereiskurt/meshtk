package keycache

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBAPI is the subset of DynamoDB client methods used by DynamoDBStore.
// The ONE deliberate divergence from credcache: keycache uses GetItem, never Scan.
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// DynamoDBStore implements KeyStore using a direct DynamoDB GetItem by the
// byte-identical ElectroDB MeshRadio primary key. This bounds DDB load to one
// point read per node per TTL (never a Scan, never per-packet).
type DynamoDBStore struct {
	client    DynamoDBAPI
	tableName string
}

// NewDynamoDBStore creates a DynamoDBStore with a real AWS DynamoDB client.
// If endpoint is non-empty, it overrides the default endpoint (for local dev).
func NewDynamoDBStore(tableName, region, endpoint string) (*DynamoDBStore, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	opts := []func(*dynamodb.Options){
		func(o *dynamodb.Options) {
			o.Region = region
		},
	}
	if endpoint != "" {
		opts = append(opts, func(o *dynamodb.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}

	client := dynamodb.NewFromConfig(cfg, opts...)
	return &DynamoDBStore{
		client:    client,
		tableName: tableName,
	}, nil
}

// NewDynamoDBStoreWithClient creates a DynamoDBStore with a provided client (for testing).
func NewDynamoDBStoreWithClient(client DynamoDBAPI, tableName string) *DynamoDBStore {
	return &DynamoDBStore{
		client:    client,
		tableName: tableName,
	}
}

// NodeIDFromNum composes the canonical nodeId string ("!" + lowercase hex padded
// to 8) from a uint32 nodeNum — byte-identical to run.human's MeshRadio write.
func NodeIDFromNum(nodeNum uint32) string {
	return fmt.Sprintf("!%08x", nodeNum)
}

// meshRadioKey composes the byte-identical ElectroDB MeshRadio primary key for a
// nodeNum. ElectroDB v3.5 format: pk="$run#nodeid_<nodeId>", sk="$meshradio_1".
// This composition is LOCKED by key_parity_test.go against run.human's parity test.
func meshRadioKey(nodeNum uint32) map[string]types.AttributeValue {
	nodeID := NodeIDFromNum(nodeNum)
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: "$run#nodeid_" + nodeID},
		"sk": &types.AttributeValueMemberS{Value: "$meshradio_1"},
	}
}

// Fetch performs a direct GetItem on the MeshRadio item for the given nodeNum.
// It projects publicKey and nodeNum. Returns ErrNotFound when the item is absent.
// The returned Key.PubKeyHex is already 0x hex (ready for ParseHexKey).
func (s *DynamoDBStore) Fetch(ctx context.Context, nodeNum uint32) (*Key, error) {
	input := &dynamodb.GetItemInput{
		TableName:            aws.String(s.tableName),
		Key:                  meshRadioKey(nodeNum),
		ProjectionExpression: aws.String("publicKey, nodeNum"),
	}

	result, err := s.client.GetItem(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("dynamodb getitem: %w", err)
	}

	if len(result.Item) == 0 {
		return nil, ErrNotFound
	}

	var key Key
	if err := attributevalue.UnmarshalMap(result.Item, &key); err != nil {
		return nil, fmt.Errorf("unmarshal key: %w", err)
	}
	// nodeNum may be absent from a legacy row; fall back to the requested value
	// and always stamp the canonical nodeId for cache keying/inspection.
	if key.NodeNum == 0 {
		key.NodeNum = nodeNum
	}
	key.NodeID = NodeIDFromNum(nodeNum)
	return &key, nil
}
