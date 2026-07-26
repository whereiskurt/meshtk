package otpqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoDBAPI is the subset of DynamoDB client methods used by DynamoDBStore.
// Query (single constant partition — never a Scan), plus the two writes.
type DynamoDBAPI interface {
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// DynamoDBStore implements Store against the shared run-human-electro table.
// Construction mirrors keycache.NewDynamoDBStore (same table/region/endpoint
// config block).
type DynamoDBStore struct {
	client    DynamoDBAPI
	tableName string
}

// NewDynamoDBStore creates a DynamoDBStore with a real AWS DynamoDB client.
// If endpoint is non-empty, it overrides the default endpoint (local dev).
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

	return &DynamoDBStore{
		client:    dynamodb.NewFromConfig(cfg, opts...),
		tableName: tableName,
	}, nil
}

// NewDynamoDBStoreWithClient creates a DynamoDBStore with a provided client (tests).
func NewDynamoDBStoreWithClient(client DynamoDBAPI, tableName string) *DynamoDBStore {
	return &DynamoDBStore{client: client, tableName: tableName}
}

func (s *DynamoDBStore) queueKey(nodeID string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"pk": &types.AttributeValueMemberS{Value: queuePK},
		"sk": &types.AttributeValueMemberS{Value: queueSK(nodeID)},
	}
}

// List returns every pending delivery in the queue partition — OTP items and
// welcome items, classified by sk prefix. Unknown prefixes are skipped
// (forward compatibility). The partition is tiny; pagination is followed
// anyway.
func (s *DynamoDBStore) List(ctx context.Context) ([]Item, []WelcomeItem, error) {
	var items []Item
	var welcomes []WelcomeItem
	var startKey map[string]types.AttributeValue

	for {
		out, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.tableName),
			KeyConditionExpression: aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: queuePK},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("otpqueue query: %w", err)
		}
		for _, raw := range out.Items {
			skAttr, ok := raw["sk"].(*types.AttributeValueMemberS)
			if !ok {
				continue
			}
			switch {
			case strings.HasPrefix(skAttr.Value, queueSKPrefix):
				var it Item
				if err := attributevalue.UnmarshalMap(raw, &it); err != nil {
					return nil, nil, fmt.Errorf("otpqueue unmarshal: %w", err)
				}
				items = append(items, it)
			case strings.HasPrefix(skAttr.Value, welcomeSKPrefix):
				var w WelcomeItem
				if err := attributevalue.UnmarshalMap(raw, &w); err != nil {
					return nil, nil, fmt.Errorf("otpqueue welcome unmarshal: %w", err)
				}
				welcomes = append(welcomes, w)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return items, welcomes, nil
}

// Delete removes a queue item (idempotent at the DynamoDB level).
func (s *DynamoDBStore) Delete(ctx context.Context, nodeID string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key:       s.queueKey(nodeID),
	})
	if err != nil {
		return fmt.Errorf("otpqueue delete %s: %w", nodeID, err)
	}
	return nil
}

// BumpAttempts records a failed publish attempt on the queue item.
func (s *DynamoDBStore) BumpAttempts(ctx context.Context, nodeID string, attempts int) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(s.tableName),
		Key:              s.queueKey(nodeID),
		UpdateExpression: aws.String("SET attempts = :a"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":a": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", attempts)},
		},
	})
	if err != nil {
		return fmt.Errorf("otpqueue bump %s: %w", nodeID, err)
	}
	return nil
}

// DeleteWelcome removes a welcome queue item (idempotent).
func (s *DynamoDBStore) DeleteWelcome(ctx context.Context, nodeID string) error {
	_, err := s.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: queuePK},
			"sk": &types.AttributeValueMemberS{Value: welcomeSK(nodeID)},
		},
	})
	if err != nil {
		return fmt.Errorf("otpqueue delete welcome %s: %w", nodeID, err)
	}
	return nil
}

// BumpWelcomeAttempts records a failed publish attempt on a welcome item.
func (s *DynamoDBStore) BumpWelcomeAttempts(ctx context.Context, nodeID string, attempts int) error {
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: queuePK},
			"sk": &types.AttributeValueMemberS{Value: welcomeSK(nodeID)},
		},
		UpdateExpression: aws.String("SET attempts = :a"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":a": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", attempts)},
		},
	})
	if err != nil {
		return fmt.Errorf("otpqueue bump welcome %s: %w", nodeID, err)
	}
	return nil
}

// MarkRadioCodeSent stamps codeSentAt on the authoritative MeshRadio row so
// run.human's UI flips from "waiting" to "code sent". The condition guards
// against minting an orphan half-row when the radio was deleted mid-flight —
// that race is expected and silently ignored.
func (s *DynamoDBStore) MarkRadioCodeSent(ctx context.Context, nodeNum uint32, sentAtMs int64) error {
	k := meshRadioKey(nodeNum)
	_, err := s.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: k.PK},
			"sk": &types.AttributeValueMemberS{Value: k.SK},
		},
		ConditionExpression: aws.String("attribute_exists(pk)"),
		UpdateExpression:    aws.String("SET codeSentAt = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", sentAtMs)},
		},
	})
	if err != nil {
		var ccf *types.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return nil // radio row deleted mid-flight — not an error
		}
		return fmt.Errorf("otpqueue mark sent !%08x: %w", nodeNum, err)
	}
	return nil
}
