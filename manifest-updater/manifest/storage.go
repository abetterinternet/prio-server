package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/rs/zerolog/log"
)

type Storage interface {
	Writer
	Fetcher
}

// Writer writes manifests to some storage
type Writer interface {
	// WriteDataShareProcessorSpecificManifest writes the provided manifest for
	// the provided share processor name in the writer's backing storage, or
	// returns an error on failure.
	WriteDataShareProcessorSpecificManifest(manifest DataShareProcessorSpecificManifest, dataShareProcessorName string) error
	// WriteIngestorGlobalManifest writes the provided manifest to the writer's
	// backing storage, or returns an error on failure.
	WriteIngestorGlobalManifest(manifest IngestorGlobalManifest) error
}

// Fetcher fetches manifests from some storage
type Fetcher interface {
	// FetchDataShareProcessorSpecificManifest fetches the specific manifest for
	// the specified data share processor and returns it, if it exists and is
	// well-formed. Returns (nil,  nil) if the  manifest does not exist.
	// Returns (nil, error) if something went wrong while trying to fetch or
	// parse the manifest.
	FetchDataShareProcessorSpecificManifest(dataShareProcessorName string) (*DataShareProcessorSpecificManifest, error)
	// IngestorGlobalManifestExists returns true if the global manifest exists
	// and is well-formed. Returns (false, nil) if it does not exist. Returns
	// (false, error) if something went wrong while trying to fetch or parse the
	// manifest.
	IngestorGlobalManifestExists() (bool, error)
}

// Bucket specifies the cloud storage bucket where manifests are stored
type Bucket struct {
	// Bucket is the name of the bucket, without any URL scheme
	Bucket string `json:"bucket"`
	// AWSRegion is the region the bucket is in, if it is an S3 bucket
	AWSRegion string `json:"aws_region,omitempty"`
	// AWSProfile is the AWS CLI config profile that should be used to
	// authenticate to AWS, if the bucket is an S3 bucket
	AWSProfile string `json:"aws_profile,omitempty"`
}

// NewStorage creates an instance of the appropriate implementation of Writer for
// the provided bucket
func NewStorage(bucket *Bucket) (Storage, error) {
	if bucket.AWSRegion != "" {
		return newS3(bucket.Bucket, bucket.AWSRegion, bucket.AWSProfile)
	}
	return newGCS(bucket.Bucket)
}

// GCSStorage is a Storage that stores manifests in a Google Cloud Storage bucket
type GCSStorage struct {
	client                 *storage.Client
	manifestBucketLocation string
}

// newGCS creates a GCSStorage
func newGCS(manifestBucketLocation string) (*GCSStorage, error) {
	client, err := storage.NewClient(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to create a new storage client from background credentials: %w", err)
	}
	return &GCSStorage{client, manifestBucketLocation}, nil
}

func (s *GCSStorage) WriteIngestorGlobalManifest(manifest IngestorGlobalManifest) error {
	return s.writeManifest(manifest, "global-manifest.json")
}

func (s *GCSStorage) WriteDataShareProcessorSpecificManifest(manifest DataShareProcessorSpecificManifest, dataShareProcessorName string) error {
	return s.writeManifest(manifest, fmt.Sprintf("%s-manifest.json"))
}

func (s *GCSStorage) writeManifest(manifest interface{}, path string) error {
	log.Info().
		Str("path", path).
		Msg("writing a manifest file")

	ioWriter := s.getWriter(path)

	if err := json.NewEncoder(ioWriter).Encode(manifest); err != nil {
		_ = ioWriter.Close()
		return fmt.Errorf("encoding manifest json failed: %w", err)
	}

	if err := ioWriter.Close(); err != nil {
		return fmt.Errorf("writing manifest failed: %w", err)
	}

	return nil
}

func (s *GCSStorage) getWriter(path string) *storage.Writer {
	ioWriter := s.client.Bucket(s.manifestBucketLocation).
		Object(path).
		NewWriter(context.Background())
	ioWriter.CacheControl = "no-cache"
	ioWriter.ContentType = "application/json; charset=UTF-8"

	return ioWriter
}

func (s *GCSStorage) FetchDataShareProcessorSpecificManifest(dataShareProcessorName string) (*DataShareProcessorSpecificManifest, error) {
	reader, err := s.getReader(fmt.Sprintf("%s-manifest.json", dataShareProcessorName))
	if err != nil {
		return nil, err
	}

	var manifest DataShareProcessorSpecificManifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("error parsing manifest: %w", err)
	}

	return &manifest, nil
}

func (s *GCSStorage) IngestorGlobalManifestExists() (bool, error) {
	reader, err := s.getReader("global-manifest.json")
	if err != nil {
		return false, err
	}

	var manifest IngestorGlobalManifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return false, fmt.Errorf("error parsing manifest: %w", err)
	}

	return true, nil
}

func (s *GCSStorage) getReader(path string) (*storage.Reader, error) {
	reader, err := s.client.Bucket(s.manifestBucketLocation).
		Object(path).
		NewReader(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get GCS object reader: %w", err)
	}

	return reader, nil
}

// S3Storage is a Storage that stores manifests in an S3 bucket
type S3Storage struct {
	client         *s3.S3
	manifestBucket string
}

// newS3 creates an S3Writer that stores manifests in the specified S3 bucket
func newS3(manifestBucket, region, profile string) (*S3Storage, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, fmt.Errorf("making AWS session: %w", err)
	}

	config := aws.
		NewConfig().
		WithRegion(region).
		WithCredentials(credentials.NewSharedCredentials("", profile))
	s3Client := s3.New(sess, config)

	return &S3Storage{
		client:         s3Client,
		manifestBucket: manifestBucket,
	}, nil
}

func (s *S3Storage) WriteIngestorGlobalManifest(manifest IngestorGlobalManifest) error {
	return s.writeManifest(manifest, "global-manifest.json")
}

func (s *S3Storage) WriteDataShareProcessorSpecificManifest(manifest DataShareProcessorSpecificManifest, dataShareProcessorName string) error {
	return s.writeManifest(manifest, fmt.Sprintf("%s-manifest.json"))
}

func (s *S3Storage) writeManifest(manifest interface{}, path string) error {
	log.Info().
		Str("path", path).
		Str("bucket", s.manifestBucket).
		Msg("writing a manifest file")

	jsonManifest, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal manifest to JSON: %w", err)
	}

	input := &s3.PutObjectInput{
		ACL:          aws.String(s3.BucketCannedACLPublicRead),
		Body:         aws.ReadSeekCloser(bytes.NewReader(jsonManifest)),
		Bucket:       aws.String(s.manifestBucket),
		Key:          aws.String(path),
		CacheControl: aws.String("no-cache"),
		ContentType:  aws.String("application/json; charset=UTF-8"),
	}

	if _, err := s.client.PutObject(input); err != nil {
		return fmt.Errorf("storage.PutObject: %w", err)
	}

	return nil
}

func (s *S3Storage) FetchDataShareProcessorSpecificManifest(dataShareProcessorName string) (*DataShareProcessorSpecificManifest, error) {
	reader, err := s.getReader(fmt.Sprintf("%s-manifest.json", dataShareProcessorName))
	if err != nil {
		return nil, err
	}

	var manifest DataShareProcessorSpecificManifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("error parsing manifest: %w", err)
	}

	return &manifest, nil
}

func (s *S3Storage) IngestorGlobalManifestExists() (bool, error) {
	reader, err := s.getReader("global-manifest.json")
	if err != nil {
		return false, err
	}

	var manifest IngestorGlobalManifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return false, fmt.Errorf("error parsing manifest: %w", err)
	}

	return true, nil
}

func (s *S3Storage) getReader(path string) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.manifestBucket),
		Key:    aws.String(path),
	}

	output, err := s.client.GetObject(input)
	if err != nil {
		return nil, fmt.Errorf("storage.GetObject: %w", err)
	}

	return output.Body, nil
}
