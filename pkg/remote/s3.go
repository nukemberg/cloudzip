package remote

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"io"
	"log/slog"
	"net/url"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

type s3ParsedUri struct {
	Bucket string
	Path   string
}

var _ Downloader = &S3Downloader{}

type S3Downloader struct {
	lock         *sync.Mutex
	serviceCache map[string]s3iface.S3API
}

func NewS3Downloader() *S3Downloader {
	return &S3Downloader{
		lock:         &sync.Mutex{},
		serviceCache: make(map[string]s3iface.S3API),
	}
}

func buildRange(offsetStart int64, offsetEnd int64) *string {
	if offsetStart != 0 && offsetEnd != 0 {
		return aws.String(fmt.Sprintf("bytes=%d-%d", offsetStart, offsetEnd))
	} else if offsetStart != 0 {
		return aws.String(fmt.Sprintf("bytes=%d-", offsetStart))
	} else if offsetEnd != 0 {
		return aws.String(fmt.Sprintf("bytes=-%d", offsetEnd))
	}
	return nil
}

func (d *S3Downloader) getServiceForBucket(ctx context.Context, bucket string) (s3iface.S3API, error) {
	d.lock.Lock()
	defer d.lock.Unlock()
	if svc, ok := d.serviceCache[bucket]; ok {
		return svc, nil
	}
	const defaultRegion = "us-east-1"
	sess := session.Must(session.NewSession())
	svc := s3.New(sess, aws.NewConfig().WithRegion(defaultRegion))
	region, err := s3manager.GetBucketRegionWithClient(ctx, svc, bucket)
	if err != nil {
		return nil, err
	}
	svc = s3.New(sess, aws.NewConfig().WithRegion(region))
	d.serviceCache[bucket] = svc
	return svc, nil
}

func s3IsNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if awsErr, ok := err.(awserr.Error); ok {
		switch awsErr.Code() {
		case s3.ErrCodeNoSuchBucket, s3.ErrCodeNoSuchKey:
			return true
		}
	}
	return false
}

func (d *S3Downloader) parseUri(uri string) (*s3ParsedUri, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	return &s3ParsedUri{
		Bucket: parsed.Host,
		Path:   parsed.Path,
	}, nil
}

func (d *S3Downloader) Download(ctx context.Context, uri string, offsetStart int64, offsetEnd int64) (io.ReadCloser, error) {
	parsed, err := d.parseUri(uri)
	if err != nil {
		return nil, err
	}
	svc, err := d.getServiceForBucket(ctx, parsed.Bucket)
	if err != nil {
		return nil, err
	}

	rng := buildRange(offsetStart, offsetEnd)

	slog.Debug("s3:GetObject", "uri", uri, "range", rng)
	out, err := svc.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(parsed.Bucket),
		Key:    aws.String(parsed.Path),
		Range:  rng,
	})
	if s3IsNotFoundErr(err) {
		return nil, ErrDoesNotExist
	} else if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (d *S3Downloader) SizeOf(ctx context.Context, uri string) (int64, error) {
	slog.Debug("s3:HeadObject", "uri", uri)
	parsed, err := d.parseUri(uri)
	if err != nil {
		return 0, err
	}
	svc, err := d.getServiceForBucket(ctx, parsed.Bucket)
	if err != nil {
		return 0, err
	}
	out, err := svc.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(parsed.Bucket),
		Key:    aws.String(parsed.Path),
	})
	if s3IsNotFoundErr(err) {
		return 0, ErrDoesNotExist
	} else if err != nil {
		return 0, err
	}
	sizeBytes := aws.Int64Value(out.ContentLength)
	return sizeBytes, nil
}