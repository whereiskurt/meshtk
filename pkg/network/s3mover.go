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

	awsCfg := &aws.Config{
		Region:                        aws.String(region),
		CredentialsChainVerboseErrors: aws.Bool(true),
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Config:            *awsCfg,
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %v", err)
	}

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

	_, err = m.S3Client.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(m.BucketName),
		Key:    aws.String(customKey),
		Body:   file,
	})

	if err != nil {
		result.Success = false
		result.ErrorMessage = fmt.Sprintf("failed to upload file to S3: %v", err)
		return result, err
	}

	result.URL = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", m.BucketName, m.BucketRegion, customKey)
	result.Success = true

	return result, nil
}
