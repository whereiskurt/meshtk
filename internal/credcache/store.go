package credcache

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// DynamoDBAPI is the subset of DynamoDB client methods used by DynamoDBStore.
type DynamoDBAPI interface {
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// DynamoDBStore implements CredentialStore using DynamoDB Scan with filter expression.
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

// Fetch scans the DynamoDB table for a credential matching the given username.
// It uses FilterExpression on mqttUsername and ProjectionExpression for the three
// credential attributes. Handles pagination via LastEvaluatedKey.
// Returns ErrNotFound if no matching item is found.
func (s *DynamoDBStore) Fetch(ctx context.Context, username string) (*Credential, error) {
	filt := expression.Name("mqttUsername").Equal(expression.Value(username))
	proj := expression.NamesList(
		expression.Name("mqttUsername"),
		expression.Name("mqttPassword"),
		expression.Name("mqttUsertype"),
	)
	expr, err := expression.NewBuilder().WithFilter(filt).WithProjection(proj).Build()
	if err != nil {
		return nil, fmt.Errorf("build expression: %w", err)
	}

	input := &dynamodb.ScanInput{
		TableName:                 aws.String(s.tableName),
		FilterExpression:          expr.Filter(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		ProjectionExpression:      expr.Projection(),
	}

	// Paginate through scan results
	for {
		result, err := s.client.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("dynamodb scan: %w", err)
		}

		if len(result.Items) > 0 {
			var cred Credential
			if err := attributevalue.UnmarshalMap(result.Items[0], &cred); err != nil {
				return nil, fmt.Errorf("unmarshal credential: %w", err)
			}
			return &cred, nil
		}

		// Check for more pages
		if result.LastEvaluatedKey == nil {
			break
		}
		input.ExclusiveStartKey = result.LastEvaluatedKey
	}

	return nil, ErrNotFound
}
