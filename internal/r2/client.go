// Package r2 wraps Cloudflare R2 (S3-compatible) for storage-api. R2
// just speaks S3, so we use aws-sdk-go-v2/service/s3 with a custom
// endpoint. Region is "auto" — R2 ignores it but the SDK demands one.
package r2

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"construct/storage/internal/config"
)

// endpointClient bundles the S3 client + presigner for a single
// endpoint. We hold one per distinct endpoint URL so jurisdiction
// buckets (EU, FedRAMP, …) can be served from the same process.
type endpointClient struct {
	s3        *s3.Client
	presigner *s3.PresignClient
}

// Client is the storage-api's S3-compat handle for R2. Holds a lazy
// cache of per-endpoint S3 clients so requests for buckets in different
// jurisdictions don't all funnel through one mismatched endpoint.
type Client struct {
	cfg     *config.Config
	mu      sync.RWMutex
	clients map[string]*endpointClient

	// S3 / Presigner are kept for backwards compatibility — they expose
	// the *default* endpoint. New code should prefer For(bucket).
	S3        *s3.Client
	Presigner *s3.PresignClient
}

// New builds an R2 client from cfg. Returns an error if credentials are
// missing — the service refuses to start blind. Pre-warms the default
// endpoint; per-bucket endpoints are built lazily on first use.
func New(cfg *config.Config) (*Client, error) {
	if cfg.R2AccessKey == "" || cfg.R2SecretKey == "" {
		return nil, fmt.Errorf("R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY required")
	}
	if cfg.R2Endpoint == "" {
		return nil, fmt.Errorf("R2 endpoint missing — set R2_ACCOUNT_ID or R2_ENDPOINT")
	}
	c := &Client{
		cfg:     cfg,
		clients: map[string]*endpointClient{},
	}
	def := c.clientForEndpoint(cfg.R2Endpoint)
	c.S3 = def.s3
	c.Presigner = def.presigner
	return c, nil
}

// clientForEndpoint returns (creating if necessary) the S3 client +
// presigner bound to the given endpoint URL. Thread-safe; reuses
// existing clients so we don't pay TLS handshake costs per request.
func (c *Client) clientForEndpoint(endpoint string) *endpointClient {
	c.mu.RLock()
	if ec, ok := c.clients[endpoint]; ok {
		c.mu.RUnlock()
		return ec
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if ec, ok := c.clients[endpoint]; ok {
		return ec
	}
	s3c := s3.New(s3.Options{
		Region:       c.cfg.R2Region,
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(c.cfg.R2AccessKey, c.cfg.R2SecretKey, ""),
		UsePathStyle: true,
	})
	ec := &endpointClient{
		s3:        s3c,
		presigner: s3.NewPresignClient(s3c),
	}
	c.clients[endpoint] = ec
	return ec
}

// For returns the S3 client + presigner appropriate for `bucket`,
// honoring any per-bucket endpoint override in config.
func (c *Client) For(bucket string) (*s3.Client, *s3.PresignClient) {
	ec := c.clientForEndpoint(c.cfg.EndpointForBucket(bucket))
	return ec.s3, ec.presigner
}

// PresignPut returns a URL the client can PUT to directly. TTL comes
// from cfg.PresignTTLSeconds. Routes via the bucket-specific endpoint
// so EU-jurisdiction buckets get signed against the EU endpoint.
func (c *Client) PresignPut(ctx context.Context, bucket, key, contentType string) (string, time.Time, error) {
	_, presigner := c.For(bucket)
	expires := time.Now().Add(time.Duration(c.cfg.PresignTTLSeconds) * time.Second)
	put := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		put.ContentType = aws.String(contentType)
	}
	req, err := presigner.PresignPutObject(ctx, put,
		s3.WithPresignExpires(time.Duration(c.cfg.PresignTTLSeconds)*time.Second))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presign put: %w", err)
	}
	return req.URL, expires, nil
}

// PresignGet returns a short-lived signed GET URL. Used when objects are
// stored in private buckets but need to be handed to a browser without
// proxying through this service.
func (c *Client) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	_, presigner := c.For(bucket)
	if ttl == 0 {
		ttl = time.Duration(c.cfg.PresignTTLSeconds) * time.Second
	}
	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

// PutObject uploads content directly from this server. Used by the
// proxy-upload route for small files (<MaxUploadBytes). Big files
// should use PresignPut and PUT directly to R2.
func (c *Client) PutObject(ctx context.Context, bucket, key, contentType string, body []byte) error {
	s3c, _ := c.For(bucket)
	put := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}
	if contentType != "" {
		put.ContentType = aws.String(contentType)
	}
	if _, err := s3c.PutObject(ctx, put); err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// HeadObject returns object metadata + size; nil if not found.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (*s3.HeadObjectOutput, error) {
	s3c, _ := c.For(bucket)
	return s3c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
}

// GetObjectStream returns an open reader for the object. Caller MUST
// close the returned ReadCloser. Used by the GET proxy route when a
// signed URL isn't appropriate (e.g. authenticated downloads).
func (c *Client) GetObjectStream(ctx context.Context, bucket, key string) (*s3.GetObjectOutput, error) {
	s3c, _ := c.For(bucket)
	return s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
}

// DeleteObject removes a single object. Idempotent — R2 returns success
// for missing keys.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	s3c, _ := c.For(bucket)
	_, err := s3c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err
}

// CopyObject copies source → dest within R2. Both must be in
// allowlisted buckets at the handler level. Routes via dest's endpoint —
// cross-jurisdiction copies aren't supported by R2 and will fail at the
// API level here; that's the correct behaviour for data-residency.
func (c *Client) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) error {
	s3c, _ := c.For(dstBucket)
	_, err := s3c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(dstBucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(srcBucket + "/" + srcKey),
	})
	return err
}

// ListObjects returns up to 1000 keys under prefix in bucket.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	s3c, _ := c.For(bucket)
	out, err := s3c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(out.Contents))
	for _, o := range out.Contents {
		if o.Key != nil {
			keys = append(keys, *o.Key)
		}
	}
	return keys, nil
}

// PublicURL returns the canonical public URL for an object. Honors
// per-bucket CDN/public-base overrides so EU-jurisdiction buckets map
// to the EU CDN domain instead of the default one. Falls back to the
// per-jurisdiction endpoint if no public base is configured at all.
func (c *Client) PublicURL(bucket, key string) string {
	if pub := c.cfg.PublicBaseForBucket(bucket); pub != "" {
		return strings.TrimRight(pub, "/") + "/" + strings.TrimLeft(key, "/")
	}
	return c.cfg.EndpointForBucket(bucket) + "/" + bucket + "/" + key
}
