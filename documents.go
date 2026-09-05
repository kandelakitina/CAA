package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

const maxDocumentSize = 25 << 20 // 25 MiB

type documentListItem struct {
	ID               int64
	Title            string
	Description      string
	StatusLabel      string
	VersionNo        int
	OriginalFilename string
	SizeLabel        string
	UpdatedLabel     string
}

func (app *application) renderDashboard(c *gin.Context, status int, message string) {
	usr := c.MustGet("user").(user)
	documents, err := app.listDocuments(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить реестр документов")
		return
	}

	c.HTML(status, "dashboard.html", gin.H{
		"Title":     "Neva Concert Hall Corporate Approvals",
		"User":      usr,
		"Documents": documents,
		"Error":     message,
	})
}

func (app *application) listDocuments(ctx context.Context) ([]documentListItem, error) {
	rows, err := app.db.Query(ctx, `
		SELECT d.id, d.title, d.description, d.status, d.current_version,
		       v.original_filename, v.size_bytes, d.updated_at
		FROM documents d
		JOIN document_versions v
		  ON v.document_id = d.id AND v.version_no = d.current_version
		ORDER BY d.updated_at DESC, d.id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []documentListItem
	for rows.Next() {
		var item documentListItem
		var status string
		var size int64
		var updated time.Time
		if err := rows.Scan(
			&item.ID, &item.Title, &item.Description, &status, &item.VersionNo,
			&item.OriginalFilename, &size, &updated,
		); err != nil {
			return nil, err
		}
		item.StatusLabel = documentStatusLabel(status)
		item.SizeLabel = formatBytes(size)
		item.UpdatedLabel = updated.Format("02.01.2006 15:04")
		result = append(result, item)
	}
	return result, rows.Err()
}

func (app *application) createDocument(c *gin.Context) {
	usr := c.MustGet("user").(user)
	if usr.Role != "admin" && usr.Role != "secretary" {
		c.String(http.StatusForbidden, "Недостаточно прав для загрузки документов")
		return
	}
	if app.storage == nil {
		app.renderDashboard(c, http.StatusServiceUnavailable, "S3 не настроен: документ не загружен")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDocumentSize+(1<<20))
	title := strings.TrimSpace(c.PostForm("title"))
	description := strings.TrimSpace(c.PostForm("description"))
	if title == "" || len([]rune(title)) > 250 {
		app.renderDashboard(c, http.StatusUnprocessableEntity, "Укажите название документа длиной до 250 знаков")
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		app.renderDashboard(c, http.StatusUnprocessableEntity, "Выберите файл документа")
		return
	}
	if fileHeader.Size < 1 || fileHeader.Size > maxDocumentSize {
		app.renderDashboard(c, http.StatusRequestEntityTooLarge, "Допустимый размер файла — от 1 байта до 25 МБ")
		return
	}

	extension := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if extension != ".doc" && extension != ".docx" && extension != ".pdf" {
		app.renderDashboard(c, http.StatusUnsupportedMediaType, "Разрешены только файлы .doc, .docx и .pdf")
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		app.renderDashboard(c, http.StatusBadRequest, "Не удалось прочитать загруженный файл")
		return
	}
	defer file.Close()

	header := make([]byte, 512)
	read, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		app.renderDashboard(c, http.StatusBadRequest, "Не удалось проверить загруженный файл")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		app.renderDashboard(c, http.StatusInternalServerError, "Не удалось подготовить файл к загрузке")
		return
	}
	contentType := http.DetectContentType(header[:read])

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		app.renderDashboard(c, http.StatusInternalServerError, "Не удалось начать сохранение документа")
		return
	}
	defer tx.Rollback(ctx)

	var documentID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO documents (title, description, created_by)
		VALUES ($1, $2, $3)
		RETURNING id
	`, title, description, usr.ID).Scan(&documentID)
	if err != nil {
		app.renderDashboard(c, http.StatusInternalServerError, "Не удалось создать карточку документа")
		return
	}

	randomPart, err := randomToken(12)
	if err != nil {
		app.renderDashboard(c, http.StatusInternalServerError, "Не удалось сформировать ключ файла")
		return
	}
	objectKey := fmt.Sprintf("documents/%d/versions/1/%s%s", documentID, randomPart, extension)
	putResult, err := app.storage.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &app.storage.bucket,
		Key:           &objectKey,
		Body:          file,
		ContentLength: aws.Int64(fileHeader.Size),
		ContentType:   &contentType,
		Metadata: map[string]string{
			"document-id": strconv.FormatInt(documentID, 10),
			"version-no":  "1",
		},
	})
	if err != nil {
		log.Printf("upload document %d to S3: %v", documentID, err)
		app.renderDashboard(c, http.StatusBadGateway, "S3 отклонил загрузку файла. Посмотрите логи приложения")
		return
	}
	var s3VersionID any
	if putResult.VersionId != nil {
		s3VersionID = *putResult.VersionId
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO document_versions (
			document_id, version_no, object_key, s3_version_id,
			original_filename, content_type, size_bytes, uploaded_by
		) VALUES ($1, 1, $2, $3, $4, $5, $6, $7)
	`, documentID, objectKey, s3VersionID, filepath.Base(fileHeader.Filename), contentType, fileHeader.Size, usr.ID)
	if err == nil {
		_, err = tx.Exec(ctx, `
			UPDATE documents SET current_version = 1, updated_at = NOW() WHERE id = $1
		`, documentID)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("save document %d metadata: %v", documentID, err)
		app.renderDashboard(c, http.StatusInternalServerError, "Файл загружен, но сведения о нём не удалось сохранить")
		return
	}

	c.Redirect(http.StatusSeeOther, "/")
}

func (app *application) downloadCurrentDocument(c *gin.Context) {
	if app.storage == nil {
		c.String(http.StatusServiceUnavailable, "S3 не настроен")
		return
	}
	documentID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || documentID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор документа")
		return
	}

	var objectKey, versionID, filename, contentType string
	var size int64
	err = app.db.QueryRow(c.Request.Context(), `
		SELECT v.object_key, COALESCE(v.s3_version_id, ''), v.original_filename, v.content_type, v.size_bytes
		FROM documents d
		JOIN document_versions v
		  ON v.document_id = d.id AND v.version_no = d.current_version
		WHERE d.id = $1
	`, documentID).Scan(&objectKey, &versionID, &filename, &contentType, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Документ не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось найти файл документа")
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
		log.Printf("download document %d from S3: %v", documentID, err)
		c.String(http.StatusBadGateway, "Не удалось скачать файл из S3")
		return
	}
	defer object.Body.Close()

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	c.Header("Content-Disposition", disposition)
	c.DataFromReader(http.StatusOK, size, contentType, object.Body, nil)
}

func documentStatusLabel(status string) string {
	switch status {
	case "in_review":
		return "На согласовании"
	case "approved":
		return "Согласован"
	case "rejected":
		return "Отклонён"
	default:
		return "Черновик"
	}
}

func formatBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d Б", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f КБ", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f МБ", float64(size)/(1024*1024))
}
