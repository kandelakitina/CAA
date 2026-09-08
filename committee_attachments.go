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

type committeeAttachmentItem struct {
	ID        int64
	Filename  string
	SizeLabel string
}

func (app *application) storeCommitteeAttachments(ctx context.Context, tx pgx.Tx, questionID, voteID int64, usr user, uploads []*documentUpload) ([]storedS3Object, error) {
	stored := make([]storedS3Object, 0, len(uploads))
	for index, upload := range uploads {
		randomPart, err := randomToken(12)
		if err != nil {
			app.removeStoredS3Objects(stored)
			return nil, err
		}
		objectKey := fmt.Sprintf("questions/%d/committee-votes/%d/attachments/%d-%s%s", questionID, voteID, index+1, randomPart, upload.extension)
		result, err := app.storage.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &app.storage.bucket, Key: &objectKey, Body: upload.file,
			ContentLength: aws.Int64(upload.header.Size), ContentType: &upload.contentType,
			Metadata: map[string]string{"question-id": strconv.FormatInt(questionID, 10), "vote-id": strconv.FormatInt(voteID, 10)},
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
			INSERT INTO committee_vote_attachments (
				vote_id, object_key, s3_version_id, original_filename,
				content_type, size_bytes, uploaded_by
			) VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7)
		`, voteID, objectKey, versionID, upload.originalFilename, upload.contentType, upload.header.Size, usr.ID)
		if err != nil {
			app.removeStoredS3Objects(stored)
			return nil, err
		}
	}
	return stored, nil
}

func loadCommitteeAttachmentViews(ctx context.Context, queryer revisionQueryer, roundID int64) (map[int64][]committeeAttachmentItem, error) {
	rows, err := queryer.Query(ctx, `
		SELECT vote.id, attachment.id, attachment.original_filename, attachment.size_bytes
		FROM committee_votes vote
		JOIN committee_vote_participants participant ON participant.id = vote.participant_id
		JOIN committee_vote_attachments attachment ON attachment.vote_id = vote.id
		WHERE participant.round_id = $1
		ORDER BY attachment.id
	`, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64][]committeeAttachmentItem)
	for rows.Next() {
		var voteID, size int64
		var item committeeAttachmentItem
		if err := rows.Scan(&voteID, &item.ID, &item.Filename, &size); err != nil {
			return nil, err
		}
		item.SizeLabel = formatBytes(size)
		result[voteID] = append(result[voteID], item)
	}
	return result, rows.Err()
}

func (app *application) downloadCommitteeVoteAttachment(c *gin.Context) {
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
		FROM committee_vote_attachments attachment
		JOIN committee_votes vote ON vote.id = attachment.vote_id
		JOIN committee_vote_participants participant ON participant.id = vote.participant_id
		JOIN committee_vote_rounds round ON round.id = participant.round_id
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
		log.Printf("download committee vote attachment %d from S3: %v", attachmentID, err)
		c.String(http.StatusBadGateway, "Не удалось скачать вложение из S3")
		return
	}
	defer object.Body.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, no-store")
	c.DataFromReader(http.StatusOK, size, contentType, object.Body, nil)
}
