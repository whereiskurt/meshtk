package mqtt

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

// fakeS3 stands in for the AWS client so the error MAPPING can be asserted
// without a network call. The mapping is the whole reason this type exists:
// SnapshotTick treats "no snapshot" and "transport failure" completely
// differently, so collapsing them would either arm a spurious reset edge or
// make every first boot log a failure.
type fakeS3 struct {
	body    []byte
	getErr  error
	putErr  error
	putBody []byte
}

func (f *fakeS3) GetObject(in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

func (f *fakeS3) PutObject(in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if f.putErr != nil {
		return nil, f.putErr
	}
	b, _ := io.ReadAll(in.Body)
	f.putBody = b
	return &s3.PutObjectOutput{}, nil
}

func TestS3StoreMapsNoSuchKeyToErrNoSnapshot(t *testing.T) {
	// First boot after a fresh deployment. This MUST be ErrNoSnapshot, not a
	// generic error, or the tick arms its reset edge off a phantom.
	api := &fakeS3{getErr: awserr.New(s3.ErrCodeNoSuchKey, "not found", nil)}
	store := &S3SnapshotStore{API: api, Bucket: "b", Key: "k"}

	_, err := store.Get()
	if !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("Get on a missing object returned %v, want ErrNoSnapshot", err)
	}
}

func TestS3StoreMapsNotFoundStatusToErrNoSnapshot(t *testing.T) {
	// S3 answers a missing key on some paths with NotFound rather than
	// NoSuchKey. Both mean the same thing to us.
	api := &fakeS3{getErr: awserr.New("NotFound", "not found", nil)}
	store := &S3SnapshotStore{API: api, Bucket: "b", Key: "k"}

	if _, err := store.Get(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("Get returned %v, want ErrNoSnapshot", err)
	}
}

func TestS3StorePreservesRealFailures(t *testing.T) {
	// AccessDenied is NOT "no snapshot". Swallowing it would silently disable
	// backups while every tick reported success.
	boom := awserr.New("AccessDenied", "nope", nil)
	store := &S3SnapshotStore{API: &fakeS3{getErr: boom}, Bucket: "b", Key: "k"}

	_, err := store.Get()
	if err == nil {
		t.Fatal("AccessDenied was swallowed")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Fatal("AccessDenied was misreported as ErrNoSnapshot")
	}
}

func TestS3StoreRoundTrips(t *testing.T) {
	api := &fakeS3{}
	store := &S3SnapshotStore{API: api, Bucket: "b", Key: "k"}

	want := []byte(`{"1":{"longName":"KPH"}}`)
	if err := store.Put(want); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !bytes.Equal(api.putBody, want) {
		t.Fatalf("put body = %q, want %q", api.putBody, want)
	}

	api.body = api.putBody
	got, err := store.Get()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("get = %q, want %q", got, want)
	}
}
