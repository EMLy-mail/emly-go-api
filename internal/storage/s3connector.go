package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"emly-api-go/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Connector struct {
	client     *s3.Client
	uploader   *manager.Uploader
	downloader *manager.Downloader
	bucket     string
}

type FileInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	ContentType  string
	Metadata     map[string]string
}

// IsNotFound reports whether err represents a missing object (404 / NoSuchKey).
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	// Fallback for S3-compatible stores (e.g. Cloudflare R2) that surface
	// the error code via the generic APIError interface.
	var ae interface{ ErrorCode() string }
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

type FolderInfo struct {
	Prefix string
}

func NewCloudflareR2Connector(cfg config.R2Config) (*S3Connector, error) {
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" || cfg.BucketName == "" {
		return nil, fmt.Errorf("missing required R2 config fields (CF_R2_ACCESS_KEY_ID, CF_R2_SECRET_ACCESS_KEY, CF_R2_BUCKET_NAME)")
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("either CF_R2_ENDPOINT or CF_ACCOUNT_ID must be set")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}

	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.TODO(),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
		awsconfig.WithRegion(region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load R2 config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &S3Connector{
		client:     client,
		uploader:   manager.NewUploader(client),
		downloader: manager.NewDownloader(client),
		bucket:     cfg.BucketName,
	}, nil
}

// Ping verifies connectivity by calling HeadBucket on the configured bucket.
func (c *S3Connector) Ping(ctx context.Context) error {
	_, err := c.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("R2 ping failed for bucket %q: %w", c.bucket, err)
	}
	return nil
}

// UploadFile uploads body to key in the bucket and returns the public URL.
// metadata is optional; pass nil if not needed.
func (c *S3Connector) UploadFile(ctx context.Context, key string, body io.Reader, contentType string, metadata map[string]string) (string, error) {
	result, err := c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	})
	if err != nil {
		return "", fmt.Errorf("upload %q: %w", key, err)
	}
	return result.Location, nil
}

// GetFile returns the object body at key. Caller must close it.
func (c *S3Connector) GetFile(ctx context.Context, key string) (io.ReadCloser, *FileInfo, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("get %q: %w", key, err)
	}

	info := &FileInfo{
		Key:         key,
		Size:        aws.ToInt64(out.ContentLength),
		ETag:        strings.Trim(aws.ToString(out.ETag), `"`),
		ContentType: aws.ToString(out.ContentType),
		Metadata:    out.Metadata,
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}

	return out.Body, info, nil
}

// DownloadFile downloads key into dst and returns bytes written.
func (c *S3Connector) DownloadFile(ctx context.Context, key string, dst io.WriterAt) (int64, error) {
	n, err := c.downloader.Download(ctx, dst, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("download %q: %w", key, err)
	}
	return n, nil
}

// DeleteFile deletes the object at key.
func (c *S3Connector) DeleteFile(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// DeleteFiles deletes up to 1000 objects in one request.
func (c *S3Connector) DeleteFiles(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	objects := make([]types.ObjectIdentifier, len(keys))
	for i, k := range keys {
		objects[i] = types.ObjectIdentifier{Key: aws.String(k)}
	}
	_, err := c.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(c.bucket),
		Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("batch delete: %w", err)
	}
	return nil
}

// RenameFile copies src to dst then deletes src (R2 has no native rename).
func (c *S3Connector) RenameFile(ctx context.Context, srcKey, dstKey string) error {
	_, err := c.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		CopySource: aws.String(c.bucket + "/" + srcKey),
		Key:        aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("copy %q → %q: %w", srcKey, dstKey, err)
	}
	return c.DeleteFile(ctx, srcKey)
}

// ListFiles returns all objects directly under prefix (non-recursive).
func (c *S3Connector) ListFiles(ctx context.Context, prefix string) ([]FileInfo, error) {
	prefix = normalizePrefix(prefix)

	var files []FileInfo
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(c.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list files under %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, "/") {
				continue // skip folder placeholders
			}
			fi := FileInfo{
				Key:  key,
				Size: aws.ToInt64(obj.Size),
				ETag: strings.Trim(aws.ToString(obj.ETag), `"`),
			}
			if obj.LastModified != nil {
				fi.LastModified = *obj.LastModified
			}
			files = append(files, fi)
		}
	}
	return files, nil
}

// ListFolders returns the immediate sub-folders under prefix.
func (c *S3Connector) ListFolders(ctx context.Context, prefix string) ([]FolderInfo, error) {
	prefix = normalizePrefix(prefix)

	var folders []FolderInfo
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(c.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list folders under %q: %w", prefix, err)
		}
		for _, cp := range page.CommonPrefixes {
			folders = append(folders, FolderInfo{Prefix: aws.ToString(cp.Prefix)})
		}
	}
	return folders, nil
}

// CreateFolder writes a zero-byte placeholder object to make the folder visible.
func (c *S3Connector) CreateFolder(ctx context.Context, folderPath string) error {
	key := normalizePrefix(folderPath)
	if key == "" {
		return fmt.Errorf("folder path cannot be empty")
	}
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(0),
	})
	if err != nil {
		return fmt.Errorf("create folder %q: %w", key, err)
	}
	return nil
}

// DeleteFolder removes all objects under folderPath in batches of 1000.
func (c *S3Connector) DeleteFolder(ctx context.Context, folderPath string) error {
	prefix := normalizePrefix(folderPath)

	var keys []string
	pager := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list for delete %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}

	for i := 0; i < len(keys); i += 1000 {
		end := i + 1000
		if end > len(keys) {
			end = len(keys)
		}
		if err := c.DeleteFiles(ctx, keys[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// normalizePrefix ensures prefix ends with "/" (returns "" for root).
func normalizePrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}
