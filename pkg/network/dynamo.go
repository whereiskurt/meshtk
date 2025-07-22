package network

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	log "github.com/sirupsen/logrus"
)

// UserCredential represents a single user credential record
type UserCredential struct {
	Username     string    `json:"mqtt_username"`
	Password     string    `json:"mqtt_password"`
	AccessLevel  string    `json:"mqtt_usertype"`
	LastAccessed time.Time `json:"last_accessed"`
	UserID       string    `json:"id"`
	Email        string    `json:"email"`
	ProfileScope string    `json:"profile_scope"`
	GSI2PK       string    `json:"gsi2pk"`
	GSI2SK       string    `json:"gsi2sk"`
	UpdatedAt    int64     `json:"updatedAt"`
	CreatedAt    int64     `json:"createdAt"`
}

// DynamoUserLookup handles user credential lookups with local caching
type DynamoUserLookup struct {
	Region       string
	TableName    string
	AccessKey    string
	SecretKey    string
	Endpoint     string
	IndexName    string // GSI2 index name
	GSI2PKPrefix string // Prefix for GSI2PK, normally "$auth#mqtt_username_"
	Logger       *log.Logger

	client    *dynamodb.DynamoDB
	cache     map[string]UserCredential
	cacheMux  sync.RWMutex
	cacheTime time.Time
	cacheExp  time.Duration
}

func NewDynamoUserLookup(region, tableName, accessKey, secretKey, endpoint string) *DynamoUserLookup {
	return &DynamoUserLookup{
		Region:       region,
		TableName:    tableName,
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		Endpoint:     endpoint,
		IndexName:    "gsi2",
		GSI2PKPrefix: "$auth#mqtt_username_",
		cache:        make(map[string]UserCredential),
		cacheExp:     time.Hour * 24, // Cache expiration time (24 hours)
	}
}

// Initialize sets up the DynamoDB client and logger
func (d *DynamoUserLookup) Initialize(logger *log.Logger) error {
	d.Logger = logger

	if d.Logger != nil {
		d.Logger.Infof("Initializing DynamoDB client with Region: %s, Table: %s, Endpoint: %s, IndexName: %s",
			d.Region, d.TableName, d.Endpoint, d.IndexName)
	}

	// Configure AWS Session
	config := &aws.Config{
		Region: aws.String(d.Region),
	}

	// Use custom endpoint if provided (for local testing)
	if d.Endpoint != "" {
		config.Endpoint = aws.String(d.Endpoint)
		if d.Logger != nil {
			d.Logger.Debugf("Using custom DynamoDB endpoint: %s", d.Endpoint)
		}
	}

	// Use credentials if provided
	if d.AccessKey != "" && d.SecretKey != "" {
		config.Credentials = credentials.NewStaticCredentials(d.AccessKey, d.SecretKey, "")
		if d.Logger != nil {
			d.Logger.Debugf("Using provided AWS credentials for authentication")
		}
	} else if d.Logger != nil {
		d.Logger.Debugf("Using environment/instance AWS credentials")
	}

	// Create session and client
	sess, err := session.NewSession(config)
	if err != nil {
		return fmt.Errorf("failed to create AWS session: %v", err)
	}

	d.client = dynamodb.New(sess)
	return nil
}

// LookupUsername checks the cache first, then DynamoDB if needed
func (d *DynamoUserLookup) LookupUsername(username string) (UserCredential, error) {
	if username == "" {
		return UserCredential{}, errors.New("username must be provided")
	}

	// Check cache first
	d.cacheMux.RLock()
	cred, exists := d.cache[username]
	isCacheExpired := time.Since(d.cacheTime) > d.cacheExp
	d.cacheMux.RUnlock()

	// Return from cache if valid
	if exists && !isCacheExpired {
		if d.Logger != nil {
			d.Logger.Debugf("Cache hit for username: %s", username)
		}
		return cred, nil
	}

	// Default credential for "public" user
	if username == "public" {
		cred = UserCredential{
			Username:     "public",
			Password:     "31337",
			AccessLevel:  "public",
			LastAccessed: time.Now(),
		}

		// Update cache
		d.cacheMux.Lock()
		d.cache[username] = cred
		d.cacheTime = time.Now()
		d.cacheMux.Unlock()

		return cred, nil
	}

	// Query DynamoDB for user credentials
	if d.client == nil {
		return UserCredential{}, errors.New("DynamoDB client not initialized")
	}

	// Format the GSI2PK lookup key: "$auth#mqtt_username_$USERNAME"
	gsi2pk := d.GSI2PKPrefix + username

	// Query the GSI2 index
	queryInput := &dynamodb.QueryInput{
		TableName:              aws.String(d.TableName),
		IndexName:              aws.String(d.IndexName),
		KeyConditionExpression: aws.String("gsi2pk = :gsi2pk"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":gsi2pk": {
				S: aws.String(gsi2pk),
			},
		},
		Limit: aws.Int64(1), // We only need one record
	}

	if d.Logger != nil {
		d.Logger.Infof("Querying DynamoDB table '%s' using index '%s' with GSI2PK: '%s'",
			d.TableName, d.IndexName, gsi2pk)
	}

	result, err := d.client.Query(queryInput)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Errorf("DynamoDB query error: %v", err)
		}
		return UserCredential{}, fmt.Errorf("DynamoDB query error: %v", err)
	}

	if len(result.Items) == 0 {
		// Try lookup by username directly as fallback
		if d.Logger != nil {
			d.Logger.Infof("No results found using GSI2PK lookup, trying direct 'mqtt_username' lookup for: %s", username)
		}

		getInput := &dynamodb.GetItemInput{
			TableName: aws.String(d.TableName),
			Key: map[string]*dynamodb.AttributeValue{
				"mqtt_username": {
					S: aws.String(username),
				},
			},
		}

		getResult, getErr := d.client.GetItem(getInput)
		if getErr != nil {
			if d.Logger != nil {
				d.Logger.Errorf("DynamoDB GetItem error: %v", getErr)
			}
			return UserCredential{}, fmt.Errorf("DynamoDB GetItem error: %v", getErr)
		}

		if getResult.Item == nil {
			if d.Logger != nil {
				d.Logger.Warnf("User not found for username: %s (checked both GSI2PK and direct lookup)", username)
			}
			return UserCredential{}, fmt.Errorf("user not found: %s", username)
		}

		result.Items = []map[string]*dynamodb.AttributeValue{getResult.Item}
		if d.Logger != nil {
			d.Logger.Infof("Found user with direct 'mqtt_username' lookup: %s", username)
		}
	}

	// Unmarshal DynamoDB result
	var dbCred UserCredential
	err = dynamodbattribute.UnmarshalMap(result.Items[0], &dbCred)
	if err != nil {
		if d.Logger != nil {
			d.Logger.Errorf("Failed to unmarshal DynamoDB result: %v", err)
			d.Logger.Debugf("Raw item content: %+v", result.Items[0])
		}
		return UserCredential{}, fmt.Errorf("failed to unmarshal DynamoDB result: %v", err)
	}

	// If the MQTT username is empty in the record, try to extract it from GSI2PK
	if dbCred.Username == "" && dbCred.GSI2PK != "" {
		// Extract username from GSI2PK: "$auth#mqtt_username_$USERNAME"
		parts := strings.Split(dbCred.GSI2PK, "_")
		if len(parts) > 2 {
			dbCred.Username = parts[len(parts)-1]
			if d.Logger != nil {
				d.Logger.Infof("Extracted username '%s' from GSI2PK: '%s'", dbCred.Username, dbCred.GSI2PK)
			}
		} else {
			if d.Logger != nil {
				d.Logger.Warnf("Failed to extract username from GSI2PK: '%s'", dbCred.GSI2PK)
			}
		}
	}

	// Update last accessed time
	dbCred.LastAccessed = time.Now()

	// Update cache
	d.cacheMux.Lock()
	d.cache[username] = dbCred
	d.cacheTime = time.Now()
	d.cacheMux.Unlock()

	if d.Logger != nil {
		d.Logger.Infof("DynamoDB lookup successful for: %s (AccessLevel: %s)",
			username, dbCred.AccessLevel)
	}

	return dbCred, nil
}

// Lookup is maintained for backwards compatibility
func (d *DynamoUserLookup) Lookup(username, password string) (string, string, error) {
	if username == "" || password == "" {
		return "", "", errors.New("username and password must be provided")
	}

	cred, err := d.LookupUsername(username)
	if err != nil {
		return "", "", err
	}

	return cred.Username, cred.Password, nil
}

func (d *DynamoUserLookup) LatestMqttCredentials() (string, error) {
	return "", nil
}
