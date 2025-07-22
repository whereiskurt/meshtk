package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Mover struct {
	BucketRegion string
	BucketName   string
	S3Client     *s3.S3
}

type S3MoveResult struct {
	Success      bool
	SourceFile   string
	S3Bucket     string
	S3Key        string
	URL          string
	ErrorMessage string
	Timestamp    time.Time
}

func NewS3Mover(region, bucketRegion, bucket string) (*S3Mover, error) {
	// Create session options with shared config
	sessionOptions := session.Options{
		Config: aws.Config{
			Region: aws.String(bucketRegion),
		},
		// This is critical for ECS - forces the SDK to check for ECS container credentials
		// It enables the task role credentials in Fargate
		SharedConfigState: session.SharedConfigEnable,
	}

	// Check if we're running in ECS environment
	if ecsRelativeUri := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); ecsRelativeUri != "" {
		fmt.Printf("Detected ECS environment, credentials URI: %s\n", ecsRelativeUri)
	}

	// Create session with the options
	sess, err := session.NewSessionWithOptions(sessionOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %v", err)
	}
	s3Client := s3.New(sess)

	// Log credential provider being used
	if creds, err := s3Client.Config.Credentials.Get(); err == nil {
		fmt.Printf("AWS S3 initialized using credential provider: %s\n", creds.ProviderName)
	} else {
		fmt.Printf("Warning: Unable to retrieve credential provider info: %v\n", err)
	}

	return &S3Mover{
		BucketRegion: bucketRegion,
		BucketName:   bucket,
		S3Client:     s3Client,
	}, nil
}

func (m *S3Mover) Move(filePath, s3prefix string) (*S3MoveResult, error) {
	fileName := filepath.Base(filePath)
	s3Key := fmt.Sprintf("%s/%s", time.Now().Format("2006/01/02"), fileName)
	if s3prefix != "" {
		s3Key = fmt.Sprintf("%s/%s", s3prefix, s3Key)
	}
	return m.WithCustomKey(filePath, s3Key)
}

func (m *S3Mover) WithCustomKey(filePath, customKey string) (*S3MoveResult, error) {
	result := &S3MoveResult{
		SourceFile: filePath,
		S3Bucket:   m.BucketName,
		S3Key:      customKey,
		Timestamp:  time.Now(),
	}

	// Verify credentials are available before attempting upload
	_, err := m.S3Client.Config.Credentials.Get()
	if err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("AWS credentials not available: %v", err)

		// Add detailed environment info for debugging
		if os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" {
			result.ErrorMessage += fmt.Sprintf("\nECS Task environment detected: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=%s",
				os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"))
		}

		// Log available environment variables for debugging
		result.ErrorMessage += "\nRelevant environment variables:"
		for _, envVar := range []string{
			"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
			"AWS_CONTAINER_CREDENTIALS_FULL_URI",
			"AWS_CONTAINER_AUTHORIZATION_TOKEN",
			"AWS_REGION",
			"AWS_DEFAULT_REGION",
		} {
			if val := os.Getenv(envVar); val != "" {
				result.ErrorMessage += fmt.Sprintf("\n  %s=%s", envVar, val)
			} else {
				result.ErrorMessage += fmt.Sprintf("\n  %s not set", envVar)
			}
		}

		return result, err
	}

	file, err := os.Open(filePath)
	if err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("failed to open file: %v", err)
		return result, err
	}
	defer file.Close()

	// Create a context with timeout for the upload
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Create the put object input
	input := &s3.PutObjectInput{
		Bucket: aws.String(m.BucketName),
		Key:    aws.String(customKey),
		Body:   file,
	}

	// Set up retry logic for transient failures
	maxRetries := 3
	var uploadErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait with exponential backoff before retrying
			delay := time.Duration(1<<uint(attempt)) * time.Second
			time.Sleep(delay)

			// Need to reopen the file for each retry since it may have been read to the end
			file.Close()
			file, err = os.Open(filePath)
			if err != nil {
				result.Success = false
				result.ErrorMessage = fmt.Sprintf("failed to reopen file for retry: %v", err)
				return result, err
			}
			input.Body = file
		}

		// Log credential state before each attempt
		if creds, err := m.S3Client.Config.Credentials.Get(); err == nil {
			fmt.Printf("S3 upload attempt %d using provider: %s\n", attempt+1, creds.ProviderName)
		} else {
			fmt.Printf("S3 upload attempt %d credential error: %v\n", attempt+1, err)
		}

		_, uploadErr = m.S3Client.PutObjectWithContext(ctx, input)
		if uploadErr == nil {
			// Upload succeeded
			fmt.Printf("S3 upload successful on attempt %d\n", attempt+1)
			break
		}

		// Log the retry attempt with more details
		fmt.Printf("S3 upload attempt %d failed: %v, retrying...\n", attempt+1, uploadErr)
	}

	if uploadErr != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("failed to upload file to S3 after %d attempts: %v", maxRetries, uploadErr)

		// Add more detailed diagnostics for ECS environments
		if os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" {
			result.ErrorMessage += "\nRunning in ECS environment - check that:"
			result.ErrorMessage += "\n1. The task role has s3:PutObject permissions for bucket: " + m.BucketName
			result.ErrorMessage += "\n2. The task execution role has proper permissions"
			result.ErrorMessage += "\n3. Network connectivity from the task to S3 is available"

			// Add task metadata endpoint check
			taskMetadataEndpoint := "http://169.254.170.2" + os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
			result.ErrorMessage += fmt.Sprintf("\n\nTask credential endpoint: %s", taskMetadataEndpoint)
		}

		return result, uploadErr
	}

	result.URL = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", m.BucketName, m.BucketRegion, customKey)
	result.Success = true

	return result, nil
}
