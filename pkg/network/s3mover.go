package network

import (
	"bytes"
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

	// Check for ECS credentials environment variables for logging/debugging
	ecsCredUri := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	ecsCredFullUri := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	ecsMetadataUri := os.Getenv("ECS_CONTAINER_METADATA_URI")
	ecsMetadataUriV4 := os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	awsProfile := os.Getenv("AWS_PROFILE")
	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = os.Getenv("AWS_DEFAULT_REGION")
	}
	
	// Detect if we're in ECS
	isECS := ecsCredUri != "" || ecsCredFullUri != "" || ecsMetadataUri != "" || ecsMetadataUriV4 != ""
	
	if isECS {
		// Disable EC2 metadata service to avoid timeout in ECS
		os.Setenv("AWS_EC2_METADATA_DISABLED", "true")
		fmt.Printf("ECS environment detected, disabled EC2 metadata service\n")
		
		if ecsCredUri != "" {
			fmt.Printf("  Task role credentials: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=%s\n", ecsCredUri)
		} else if ecsCredFullUri != "" {
			fmt.Printf("  Task role credentials: AWS_CONTAINER_CREDENTIALS_FULL_URI=%s\n", ecsCredFullUri)
		} else {
			fmt.Printf("  WARNING: No task role credentials found. Ensure taskRoleArn is set in task definition.\n")
			fmt.Printf("  ECS_CONTAINER_METADATA_URI=%s\n", ecsMetadataUri)
			fmt.Printf("  ECS_CONTAINER_METADATA_URI_V4=%s\n", ecsMetadataUriV4)
		}
		
		if awsRegion == "" {
			fmt.Printf("  WARNING: AWS_REGION not set. This may cause issues.\n")
		} else {
			fmt.Printf("  Region: %s\n", awsRegion)
		}
	} else {
		fmt.Printf("Running in non-ECS environment\n")
		if awsProfile != "" {
			fmt.Printf("  AWS profile: %s\n", awsProfile)
		}
		if awsRegion != "" {
			fmt.Printf("  Region: %s\n", awsRegion)
		}
	}

	// Use simple default session - let SDK handle the credential chain
	// With AWS_EC2_METADATA_DISABLED=true in ECS, this will skip IMDS and use container creds
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region:                        aws.String(bucketRegion),
			CredentialsChainVerboseErrors: aws.Bool(true),
		},
		SharedConfigState: session.SharedConfigEnable,
	})
	
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %v", err)
	}

	// Validate credentials are available
	creds, err := sess.Config.Credentials.Get()
	if err != nil {
		if isECS {
			ecsHelpMsg := "\n\nECS credential troubleshooting:\n"
			if ecsCredUri == "" && ecsCredFullUri == "" {
				ecsHelpMsg += "  ❌ No task role detected (AWS_CONTAINER_CREDENTIALS_* not set)\n" +
					"  → Fix: Set taskRoleArn in your ECS task definition\n" +
					"  → The execution role (executionRoleArn) is NOT the same as task role\n"
			} else {
				ecsHelpMsg += fmt.Sprintf("  ✓ Task role endpoint found: %s%s\n", ecsCredUri, ecsCredFullUri) +
					"  → But credentials could not be retrieved\n" +
					"  → Check task role has S3 permissions\n"
			}
			if awsRegion == "" {
				ecsHelpMsg += "  ❌ AWS_REGION not set\n" +
					"  → Fix: Set AWS_REGION environment variable in task definition\n"
			}
			ecsHelpMsg += "\nDebug with: aws sts get-caller-identity"
			return nil, fmt.Errorf("failed to get AWS credentials: %v%s", err, ecsHelpMsg)
		} else {
			localHelpMsg := "\n\nLocal development credential options:\n" +
				"  1. AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables\n" +
				"  2. AWS credentials file at ~/.aws/credentials\n" +
				"  3. AWS_PROFILE environment variable pointing to a valid profile\n" +
				"  4. Run 'aws configure' to set up credentials"
			return nil, fmt.Errorf("failed to get AWS credentials: %v%s", err, localHelpMsg)
		}
	}
	fmt.Printf("✓ AWS credentials loaded. Provider: %s\n", creds.ProviderName)

	s3Client := s3.New(sess)

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

	file, err := os.Open(filePath)
	if err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("failed to open file: %v", err)
		return result, err
	}
	defer file.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	putInput := &s3.PutObjectInput{
		Bucket: aws.String(m.BucketName),
		Key:    aws.String(customKey),
		Body:   file,
	}

	fmt.Printf("Uploading to S3: bucket=%s, key=%s\n", m.BucketName, customKey)

	_, err = m.S3Client.PutObjectWithContext(ctx, putInput)

	if err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("failed to upload file to S3 bucket '%s' in region '%s': %v", m.BucketName, m.BucketRegion, err)
		return result, err
	}

	result.URL = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", m.BucketName, m.BucketRegion, customKey)
	result.Success = true

	return result, nil
}

func (m *S3Mover) WriteStartupTest(prefix string) error {
	// Create a startup test file with timestamp and PID
	pid := os.Getpid()
	timestamp := time.Now().Format("20060102.150405")
	filename := fmt.Sprintf("startup.%s.%d.pid", timestamp, pid)
	
	// Create S3 key with prefix if provided
	s3Key := filename
	if prefix != "" {
		s3Key = fmt.Sprintf("%s/%s", prefix, filename)
	}
	
	// Create content for the test file
	content := fmt.Sprintf("S3 connectivity test\nStarted at: %s\nPID: %d\nBucket: %s\nRegion: %s\n",
		time.Now().Format(time.RFC3339),
		pid,
		m.BucketName,
		m.BucketRegion,
	)
	
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	putInput := &s3.PutObjectInput{
		Bucket:      aws.String(m.BucketName),
		Key:         aws.String(s3Key),
		Body:        bytes.NewReader([]byte(content)),
		ContentType: aws.String("text/plain"),
	}
	
	fmt.Printf("Testing S3 connectivity: writing startup file to bucket=%s, key=%s\n", m.BucketName, s3Key)
	
	_, err := m.S3Client.PutObjectWithContext(ctx, putInput)
	if err != nil {
		return fmt.Errorf("S3 startup test failed - unable to write to bucket '%s' in region '%s': %v", m.BucketName, m.BucketRegion, err)
	}
	
	fmt.Printf("S3 startup test successful: https://%s.s3.%s.amazonaws.com/%s\n", m.BucketName, m.BucketRegion, s3Key)
	return nil
}
