package mqtt

import (
	"bytes"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

// s3API is the slice of the AWS client this store needs. Narrow on purpose:
// it makes the error mapping below unit-testable without a network call, and
// the mapping is load-bearing (see Get).
type s3API interface {
	GetObject(*s3.GetObjectInput) (*s3.GetObjectOutput, error)
	PutObject(*s3.PutObjectInput) (*s3.PutObjectOutput, error)
}

// S3SnapshotStore keeps the node database in a single, overwritten S3 object.
//
// One fixed key, not a timestamped series: the object IS the operator control
// surface. `aws s3 cp - s3://…/nodes.json <<< '{}'` has to be the whole reset
// procedure, and that only works if there is exactly one object to write.
// Version history, if ever wanted, belongs to bucket versioning rather than to
// key naming.
type S3SnapshotStore struct {
	API    s3API
	Bucket string
	Key    string
}

// NewS3SnapshotStore builds a store from an existing S3 client — in practice
// the one on network.S3Mover, so all of its ECS task-role credential handling
// is reused rather than reimplemented here.
func NewS3SnapshotStore(api s3API, bucket, key string) *S3SnapshotStore {
	return &S3SnapshotStore{API: api, Bucket: bucket, Key: key}
}

// Get fetches the snapshot, mapping "the object does not exist" to
// ErrNoSnapshot and leaving every other failure intact.
//
// That distinction is not cosmetic. SnapshotTick treats a missing object as
// "first boot, write one" and a transport failure as "something is wrong, do
// not infer anything" — and it must never read either as the empty object that
// means an operator asked for a reset. Collapsing these would, at best, log a
// failure on every clean deployment and, at worst, arm a reset edge off a
// phantom.
func (s *S3SnapshotStore) Get() ([]byte, error) {
	out, err := s.API.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.Key),
	})
	if err != nil {
		if ae, ok := err.(awserr.Error); ok {
			switch ae.Code() {
			case s3.ErrCodeNoSuchKey, "NotFound":
				return nil, ErrNoSnapshot
			}
		}
		return nil, fmt.Errorf("get snapshot %s/%s: %w", s.Bucket, s.Key, err)
	}
	defer out.Body.Close()

	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("read snapshot %s/%s: %w", s.Bucket, s.Key, err)
	}
	return b, nil
}

// Put overwrites the snapshot object.
func (s *S3SnapshotStore) Put(b []byte) error {
	_, err := s.API.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(s.Bucket),
		Key:         aws.String(s.Key),
		Body:        bytes.NewReader(b),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("put snapshot %s/%s: %w", s.Bucket, s.Key, err)
	}
	return nil
}
