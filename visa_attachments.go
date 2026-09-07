package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

const maxVisaAttachments = 10

type visaAttachmentItem struct {
	ID        int64
	Filename  string
	SizeLabel string
}

type storedS3Object struct {
	Key       string
	VersionID string
}

func receiveVisaAttachments(c *gin.Context) ([]*documentUpload, int, string) {
	form, err := c.MultipartForm()
	if errors.Is(err, http.ErrNotMultipart) {
		return nil, 0, ""
	}
	if err != nil {
		return nil, http.StatusBadRequest, "Не удалось прочитать вложения к визе"
	}
	headers := form.File["attachments"]
	if len(headers) > maxVisaAttachments {
		return nil, http.StatusRequestEntityTooLarge, "К одной визе можно приложить не более 10 файлов"
	}
	uploads := make([]*documentUpload, 0, len(headers))
	for _, header := range headers {
		upload, status, message := openValidatedQuestionFile(header)
		if message != "" {
			closeQuestionUploads(uploads)
			return nil, status, message
		}
		uploads = append(uploads, upload)
	}
	return uploads, 0, ""
}

func closeQuestionUploads(uploads []*documentUpload) {
	for _, upload := range uploads {
		_ = upload.file.Close()
	}
}

func (app *application) storeVisaAttachments(ctx context.Context, tx pgx.Tx, questionID, visaID int64, usr user, uploads []*documentUpload) ([]storedS3Object, error) {
	stored := make([]storedS3Object, 0, len(uploads))
	for index, upload := range uploads {
		randomPart, err := randomToken(12)
		if err != nil {
			app.removeStoredS3Objects(stored)
			return nil, err
		}
		objectKey := fmt.Sprintf("questions/%d/internal-visas/%d/attachments/%d-%s%s", questionID, visaID, index+1, randomPart, upload.extension)
		result, err := app.storage.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &app.storage.bucket, Key: &objectKey, Body: upload.file,
			ContentLength: aws.Int64(upload.header.Size), ContentType: &upload.contentType,
			Metadata: map[string]string{"question-id": strconv.FormatInt(questionID, 10), "visa-id": strconv.FormatInt(visaID, 10)},
		})
		if err != nil {
			app.removeStoredS3Objects(stored)
			return nil, err
		}
		versionID := ""
		if result.VersionId != nil {
			versionID = *result.VersionId
		}
		stored = append(stored, storedS3Object{Key: objectKey, VersionID: versionID})
		_, err = tx.Exec(ctx, `
			INSERT INTO internal_visa_attachments (
				visa_id, object_key, s3_version_id, original_filename,
				content_type, size_bytes, uploaded_by
			) VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)
		`, visaID, objectKey, versionID, upload.originalFilename, upload.contentType, upload.header.Size, usr.ID)
		if err != nil {
			app.removeStoredS3Objects(stored)
			return nil, err
		}
	}
	return stored, nil
}

func (app *application) removeStoredS3Objects(objects []storedS3Object) {
	for _, object := range objects {
		app.removeFailedQuestionUpload(object.Key, object.VersionID)
	}
}

func loadVisaAttachmentViews(ctx context.Context, queryer revisionQueryer, roundID int64) (map[int64][]visaAttachmentItem, error) {
	rows, err := queryer.Query(ctx, `
		SELECT displayed.id, attachment.id, attachment.original_filename, attachment.size_bytes
		FROM internal_review_visas displayed
		JOIN internal_review_requirements requirement ON requirement.id = displayed.requirement_id
		JOIN internal_visa_attachments attachment
		  ON attachment.visa_id = COALESCE(displayed.source_visa_id, displayed.id)
		WHERE requirement.round_id = $1
		ORDER BY attachment.id
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][]visaAttachmentItem)
	for rows.Next() {
		var visaID, size int64
		var item visaAttachmentItem
		if err := rows.Scan(&visaID, &item.ID, &item.Filename, &size); err != nil {
			return nil, err
		}
		item.SizeLabel = formatBytes(size)
		result[visaID] = append(result[visaID], item)
	}
	return result, rows.Err()
}

func (app *application) downloadInternalVisaAttachment(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, ok := parsePositiveID(c, "id", "Некорректный идентификатор вопроса")
	if !ok {
		return
	}
	attachmentID, ok := parsePositiveID(c, "attachmentID", "Некорректный идентификатор вложения")
	if !ok {
		return
	}
	if app.storage == nil {
		c.String(http.StatusServiceUnavailable, "S3 не настроен")
		return
	}
	var objectKey, versionID, filename, contentType string
	var size int64
	err := app.db.QueryRow(c.Request.Context(), `
		SELECT attachment.object_key, COALESCE(attachment.s3_version_id, ''),
		       attachment.original_filename, attachment.content_type, attachment.size_bytes
		FROM internal_visa_attachments attachment
		JOIN internal_review_visas visa ON visa.id = attachment.visa_id
		JOIN internal_review_requirements requirement ON requirement.id = visa.requirement_id
		JOIN internal_review_rounds round ON round.id = requirement.round_id
		JOIN questions question ON question.id = round.question_id
		WHERE attachment.id = $1 AND question.id = $2
		  AND ($3 <> 'committee' OR question.status IN ('committee_voting', 'approved', 'rejected', 'no_quorum'))
	`, attachmentID, questionID, usr.Role).Scan(&objectKey, &versionID, &filename, &contentType, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вложение не найдено")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось найти вложение")
		return
	}
	input := &s3.GetObjectInput{Bucket: &app.storage.bucket, Key: &objectKey}
	if versionID != "" {
		input.VersionId = &versionID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	object, err := app.storage.client.GetObject(ctx, input)
	if err != nil {
		log.Printf("download internal visa attachment %d from S3: %v", attachmentID, err)
		c.String(http.StatusBadGateway, "Не удалось скачать вложение из S3")
		return
	}
	defer object.Body.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, no-store")
	c.DataFromReader(http.StatusOK, size, contentType, object.Body, nil)
}
