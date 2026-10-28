package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type fakeBucketAPI struct {
	headErr   error
	createErr error
	created   int
}

func (f *fakeBucketAPI) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, f.headErr
}

func (f *fakeBucketAPI) CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.created++
	return &s3.CreateBucketOutput{}, f.createErr
}

func TestEnsureBucket(t *testing.T) {
	ctx := context.Background()

	// The bucket exists: nothing is created
	api := &fakeBucketAPI{}
	require.NoError(t, ensureBucket(ctx, api, "b"))
	require.Equal(t, 0, api.created)

	// Missing bucket is created
	api = &fakeBucketAPI{headErr: &types.NotFound{}}
	require.NoError(t, ensureBucket(ctx, api, "b"))
	require.Equal(t, 1, api.created)

	// Created by someone else in between: still fine
	for _, createErr := range []error{&types.BucketAlreadyOwnedByYou{}, &types.BucketAlreadyExists{}} {
		api = &fakeBucketAPI{headErr: &types.NotFound{}, createErr: createErr}
		require.NoError(t, ensureBucket(ctx, api, "b"))
	}

	// Other errors are reported
	api = &fakeBucketAPI{headErr: errors.New("connection refused")}
	require.ErrorContains(t, ensureBucket(ctx, api, "b"), "checking bucket")
	require.Equal(t, 0, api.created)

	api = &fakeBucketAPI{headErr: &types.NotFound{}, createErr: &smithy.GenericAPIError{Code: "AccessDenied"}}
	require.ErrorContains(t, ensureBucket(ctx, api, "b"), "creating bucket")
}

func TestBucketErrorClassification(t *testing.T) {
	require.True(t, isBucketNotFound(fmt.Errorf("wrapped: %w", &types.NotFound{})))
	require.True(t, isBucketNotFound(&types.NoSuchBucket{}))
	require.True(t, isBucketNotFound(&smithy.GenericAPIError{Code: "NotFound"}))
	require.False(t, isBucketNotFound(&smithy.GenericAPIError{Code: "Forbidden"}))
	require.False(t, isBucketNotFound(errors.New("timeout")))

	require.True(t, isBucketAlreadyThere(fmt.Errorf("wrapped: %w", &types.BucketAlreadyOwnedByYou{})))
	require.True(t, isBucketAlreadyThere(&smithy.GenericAPIError{Code: "BucketAlreadyExists"}))
	require.False(t, isBucketAlreadyThere(&smithy.GenericAPIError{Code: "AccessDenied"}))
}

func TestIsObjectNotFound(t *testing.T) {
	require.True(t, isObjectNotFound(fmt.Errorf("wrapped: %w", &types.NotFound{})))
	require.True(t, isObjectNotFound(&types.NoSuchKey{}))
	require.True(t, isObjectNotFound(&smithy.GenericAPIError{Code: "NotFound"}))
	require.False(t, isObjectNotFound(&smithy.GenericAPIError{Code: "AccessDenied"}))
	require.False(t, isObjectNotFound(errors.New("timeout")))
	require.False(t, isObjectNotFound(nil))
}

func TestIsNamespaceExists(t *testing.T) {
	require.True(t, isNamespaceExists(mongo.CommandError{Code: 48, Name: "NamespaceExists"}))
	require.True(t, isNamespaceExists(fmt.Errorf("wrapped: %w", mongo.CommandError{Code: 48})))
	require.False(t, isNamespaceExists(mongo.CommandError{Code: 13, Name: "Unauthorized"}))
	require.False(t, isNamespaceExists(errors.New("connection refused")))
}
