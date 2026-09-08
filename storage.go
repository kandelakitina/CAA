package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
)

type storage struct {
	client *s3.Client
	bucket string
}

func newStorage(ctx context.Context) (*storage, error) {
	endpoint := strings.TrimRight(os.Getenv("S3_ENDPOINT"), "/")
	region := os.Getenv("S3_REGION")
	bucket := os.Getenv("S3_BUCKET")
	accessKey := os.Getenv("S3_ACCESS_KEY_ID")
	secretKey := os.Getenv("S3_SECRET_ACCESS_KEY")

	if endpoint == "" || region == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("S3_ENDPOINT, S3_REGION, S3_BUCKET, S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required")
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS-compatible configuration: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.BaseEndpoint = &endpoint
		options.UsePathStyle = true
	})

	return &storage{client: client, bucket: bucket}, nil
}

func (app *application) checkStorage(c *gin.Context) {
	if app.storage == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"message": "S3 не настроен. Проверьте переменные приложения."})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	_, err := app.storage.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: &app.storage.bucket,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"message": "Нет доступа к S3. Проверьте endpoint, бакет и ключи."})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "S3 подключён. Приватный бакет доступен приложению."})
}
