package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/rs/zerolog/log"
)

type S3Storage struct {
	svc        *s3.Client
	downloader *manager.Downloader
	uploader   *manager.Uploader
	bucket     string
}

func NewS3Storage(host string, port int, username string, password string, region string, bucket string, useTLS bool) *S3Storage {
	creds := credentials.NewStaticCredentialsProvider(username, password, "")
	awscfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(creds),
		config.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxAttempts(retry.NewStandard(), 3)
		}),
	)

	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load SDK config")
	}

	scheme := "http://"
	if useTLS {
		scheme = "https://"
	}

	svc := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(scheme + host + ":" + strconv.Itoa(port))
		o.UsePathStyle = true
	})

	downloader := manager.NewDownloader(svc)
	uploader := manager.NewUploader(svc)

	// Check if connection is established
	_, err = svc.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to S3")
	}

	// Create the bucket unless it already exists, so restarts keep working
	if err := ensureBucket(context.Background(), svc, bucket); err != nil {
		log.Fatal().Err(err).Str("bucket", bucket).Msg("Failed to prepare the S3 bucket")
	}

	return &S3Storage{svc: svc, downloader: downloader, uploader: uploader, bucket: bucket}
}

// bucketAPI is the part of the S3 client used to prepare the bucket
type bucketAPI interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
}

// ensureBucket creates the bucket only when it does not exist yet
func ensureBucket(ctx context.Context, api bucketAPI, bucket string) error {
	_, err := api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
	if err == nil {
		return nil
	}
	if !isBucketNotFound(err) {
		return fmt.Errorf("checking bucket: %w", err)
	}

	_, err = api.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	if err != nil && !isBucketAlreadyThere(err) {
		return fmt.Errorf("creating bucket: %w", err)
	}
	return nil
}

// isBucketNotFound reports whether an S3 error means the bucket does not exist
func isBucketNotFound(err error) bool {
	var notFound *types.NotFound
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &notFound) || errors.As(err, &noSuchBucket) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

// isBucketAlreadyThere reports whether CreateBucket failed only because the bucket exists
func isBucketAlreadyThere(err error) bool {
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}

var (
	_ Storage = (*S3Storage)(nil)
	_ Deleter = (*S3Storage)(nil)
)

func (s *S3Storage) Set(key string, value string, skip_expiration bool) error {
	ctx := context.Background() // TODO: Add timeout control

	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(key),
		Body:   bytes.NewReader([]byte(value)),
	})

	return err
}

func (s *S3Storage) Get(key string, skip_expiration bool) (string, error) {
	var nsk *types.NoSuchKey

	ctx := context.Background() // TODO: Add timeout control

	buf := manager.NewWriteAtBuffer([]byte{})
	_, err := s.downloader.Download(ctx, buf, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(key),
	})
	if errors.As(err, &nsk) {
		return "", ErrNotFound
	}

	return string(buf.Bytes()), err
}

func (s *S3Storage) Size(key string) (int64, error) {
	ctx := context.Background() // TODO: Add timeout control

	out, err := s.svc.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(key),
	})
	if isObjectNotFound(err) {
		return 0, notFound(err)
	}
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(out.ContentLength), nil
}

// isObjectNotFound reports whether an S3 error means the object does not exist.
// HeadObject has no body, so a miss comes back as a bare NotFound.
func isObjectNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound *types.NotFound
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noSuchKey) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}

// Delete removes the object of key. S3 answers a delete of a missing object
// with success; a store that says NotFound instead is not an error either.
func (s *S3Storage) Delete(key string) error {
	ctx := context.Background() // TODO: Add timeout control

	_, err := s.svc.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    aws.String(key),
	})
	if isObjectNotFound(err) {
		return nil
	}
	return err
}

func (s *S3Storage) Close() error {
	return nil
}
