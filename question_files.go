package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type questionFileItem struct {
	ID               int64
	Title            string
	CategoryLabel    string
	CurrentVersionNo int
	HasPending       bool
	Versions         []questionFileVersionItem
}

type questionFileVersionItem struct {
	VersionNo       int
	Filename        string
	SizeLabel       string
	UploadedBy      string
	CreatedLabel    string
	Status          string
	StatusLabel     string
	RejectionReason string
	IsCurrent       bool
	CanReview       bool
}

func validQuestionFileCategory(value string) bool {
	switch value {
	case "contract", "terms_summary", "lna_draft", "appendix", "explanatory_note", "calculation", "schedule", "other":
		return true
	default:
		return false
	}
}

func questionFileCategoryLabel(value string) string {
	labels := map[string]string{
		"contract":         "Основной договор",
		"terms_summary":    "Справка об основных условиях",
		"lna_draft":        "Проект ЛНА",
		"appendix":         "Приложение",
		"explanatory_note": "Пояснительная записка",
		"calculation":      "Расчёт",
		"schedule":         "График",
		"other":            "Другой материал",
	}
	return labels[value]
}

func questionFileVersionStatusLabel(value string) string {
	switch value {
	case "pending":
		return "Ожидает подтверждения"
	case "rejected":
		return "Отклонена"
	default:
		return "Официальная"
	}
}

func receiveQuestionFileUpload(c *gin.Context) (*documentUpload, int, string) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return nil, http.StatusUnprocessableEntity, "Выберите файл"
	}
	if fileHeader.Size < 1 || fileHeader.Size > maxDocumentSize {
		return nil, http.StatusRequestEntityTooLarge, "Допустимый размер файла — от 1 байта до 25 МБ"
	}
	filename := path.Base(strings.ReplaceAll(fileHeader.Filename, "\\", "/"))
	filename = strings.TrimSpace(strings.ReplaceAll(filename, "\x00", ""))
	extension := strings.ToLower(filepath.Ext(filename))
	if extension != ".doc" && extension != ".docx" && extension != ".xls" && extension != ".xlsx" && extension != ".pdf" {
		return nil, http.StatusUnsupportedMediaType, "Разрешены только файлы .doc, .docx, .xls, .xlsx и .pdf"
	}
	if filename == "" || filename == "." {
		filename = "document" + extension
	}
	file, err := fileHeader.Open()
	if err != nil {
		return nil, http.StatusBadRequest, "Не удалось прочитать загруженный файл"
	}
	contentType, err := validateQuestionFileContent(file, fileHeader.Size, extension)
	if err != nil {
		file.Close()
		return nil, http.StatusUnsupportedMediaType, err.Error()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, http.StatusInternalServerError, "Не удалось подготовить файл к загрузке"
	}
	return &documentUpload{
		file: file, header: fileHeader, extension: extension,
		contentType: contentType, originalFilename: filename,
	}, 0, ""
}

func validateQuestionFileContent(file multipart.File, size int64, extension string) (string, error) {
	if extension != ".xls" && extension != ".xlsx" {
		return validateDocumentContent(file, size, extension)
	}
	if extension == ".xls" {
		if err := validateXLS(file); err != nil {
			return "", err
		}
		return "application/vnd.ms-excel", nil
	}
	if err := validateXLSX(file, size); err != nil {
		return "", err
	}
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", nil
}

func validateXLS(file multipart.File) error {
	header := make([]byte, 8)
	oleSignature := []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}
	if _, err := file.ReadAt(header, 0); err != nil || !bytes.Equal(header, oleSignature) {
		return errors.New("Содержимое файла не соответствует формату XLS")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("Не удалось проверить содержимое XLS")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxDocumentSize+1))
	if err != nil {
		return errors.New("Не удалось проверить содержимое XLS")
	}
	workbook := []byte{'W', 0, 'o', 0, 'r', 0, 'k', 0, 'b', 0, 'o', 0, 'o', 0, 'k', 0}
	book := []byte{'B', 0, 'o', 0, 'o', 0, 'k', 0}
	if !bytes.Contains(contents, workbook) && !bytes.Contains(contents, book) {
		return errors.New("OLE-файл не содержит книгу Microsoft Excel")
	}
	return nil
}

func validateXLSX(file multipart.File, size int64) error {
	reader, err := zip.NewReader(file, size)
	if err != nil || len(reader.File) == 0 || len(reader.File) > 10_000 {
		return errors.New("Содержимое файла не соответствует формату XLSX")
	}
	required := map[string]bool{"[Content_Types].xml": false, "_rels/.rels": false, "xl/workbook.xml": false}
	seen := make(map[string]bool, len(reader.File))
	var total uint64
	for _, entry := range reader.File {
		name := path.Clean(strings.ReplaceAll(entry.Name, "\\", "/"))
		if name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(entry.Name, "/") || seen[name] {
			return errors.New("XLSX содержит небезопасную структуру")
		}
		seen[name] = true
		total += entry.UncompressedSize64
		if total > uint64(maxDocumentSize*20) {
			return errors.New("Распакованный XLSX слишком велик")
		}
		if _, ok := required[name]; ok {
			required[name] = true
		}
		if strings.HasSuffix(strings.ToLower(name), "vbaproject.bin") {
			return errors.New("XLSX с макросами не поддерживается")
		}
	}
	for _, found := range required {
		if !found {
			return errors.New("XLSX не содержит обязательные части книги")
		}
	}
	contentTypes, err := openZIPEntry(reader, "[Content_Types].xml", 1<<20)
	if err != nil {
		return errors.New("Не удалось проверить описание XLSX")
	}
	defer contentTypes.Close()
	decoder := xml.NewDecoder(contentTypes)
	foundWorkbook := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("Описание XLSX содержит некорректный XML")
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Override" {
			continue
		}
		var partName, contentType string
		for _, attribute := range start.Attr {
			if attribute.Name.Local == "PartName" {
				partName = attribute.Value
			}
			if attribute.Name.Local == "ContentType" {
				contentType = attribute.Value
			}
		}
		if partName == "/xl/workbook.xml" && contentType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml" {
			foundWorkbook = true
		}
	}
	if !foundWorkbook {
		return errors.New("Архив не является книгой XLSX")
	}
	workbook, err := openZIPEntry(reader, "xl/workbook.xml", 4<<20)
	if err != nil {
		return errors.New("Не удалось проверить книгу XLSX")
	}
	defer workbook.Close()
	decoder = xml.NewDecoder(workbook)
	for {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("Книга XLSX содержит некорректный XML")
		}
		if start, ok := token.(xml.StartElement); ok {
			if start.Name.Local != "workbook" || (start.Name.Space != "http://schemas.openxmlformats.org/spreadsheetml/2006/main" && start.Name.Space != "http://purl.oclc.org/ooxml/spreadsheetml/main") {
				return errors.New("Архив не является книгой Microsoft Excel")
			}
			break
		}
	}
	return nil
}

func canUploadQuestionFiles(usr user) bool {
	return usr.Role == "secretary" || (usr.Role == "approver" && validInternalService(usr.InternalService))
}

func (app *application) requireQuestionFileUploader() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !canUploadQuestionFiles(c.MustGet("user").(user)) {
			c.String(http.StatusForbidden, "Загружать материалы могут секретарь и назначенные внутренние согласующие")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (app *application) uploadQuestionFile(c *gin.Context) {
	app.saveQuestionFileVersion(c, 0)
}

func (app *application) uploadQuestionFileVersion(c *gin.Context) {
	fileID, err := strconv.ParseInt(c.Param("fileID"), 10, 64)
	if err != nil || fileID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор файла")
		return
	}
	app.saveQuestionFileVersion(c, fileID)
}

func (app *application) saveQuestionFileVersion(c *gin.Context, fileID int64) {
	usr := c.MustGet("user").(user)
	if !canUploadQuestionFiles(usr) {
		c.String(http.StatusForbidden, "Загружать материалы могут секретарь и назначенные внутренние согласующие")
		return
	}
	if app.storage == nil {
		c.String(http.StatusServiceUnavailable, "S3 не настроен")
		return
	}
	questionID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || questionID < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор вопроса")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxDocumentSize+(2<<20))
	title := strings.TrimSpace(c.PostForm("title"))
	category := strings.TrimSpace(c.PostForm("category"))
	if fileID == 0 && (title == "" || len([]rune(title)) > 250) {
		c.String(http.StatusUnprocessableEntity, "Укажите название материала длиной до 250 знаков")
		return
	}
	if fileID == 0 && !validQuestionFileCategory(category) {
		c.String(http.StatusUnprocessableEntity, "Выберите категорию материала")
		return
	}
	upload, status, message := receiveQuestionFileUpload(c)
	if message != "" {
		c.String(status, message)
		return
	}
	defer upload.file.Close()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	tx, err := app.db.Begin(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать сохранение файла")
		return
	}
	defer tx.Rollback(ctx)

	var questionStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM questions WHERE id = $1 FOR UPDATE`, questionID).Scan(&questionStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Вопрос не найден")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить вопрос")
		return
	}
	if questionStatus != "draft" && questionStatus != "internal_review" && questionStatus != "revision_required" {
		c.String(http.StatusConflict, "На текущем этапе комплект вопроса изменять нельзя")
		return
	}
	if fileID == 0 {
		err = tx.QueryRow(ctx, `
			INSERT INTO question_files (question_id, title, category, created_by)
			VALUES ($1, $2, $3, $4) RETURNING id
		`, questionID, title, category, usr.ID).Scan(&fileID)
		if err != nil {
			c.String(http.StatusInternalServerError, "Не удалось создать карточку файла")
			return
		}
	} else {
		var fileQuestionID int64
		err = tx.QueryRow(ctx, `SELECT question_id, title FROM question_files WHERE id = $1 AND status = 'active' FOR UPDATE`, fileID).Scan(&fileQuestionID, &title)
		if errors.Is(err, pgx.ErrNoRows) || fileQuestionID != questionID {
			c.String(http.StatusNotFound, "Файл вопроса не найден")
			return
		}
		if err != nil {
			c.String(http.StatusInternalServerError, "Не удалось загрузить карточку файла")
			return
		}
	}
	var nextVersion int
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version_no), 0) + 1
		FROM question_file_versions WHERE question_file_id = $1
	`, fileID).Scan(&nextVersion)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось определить номер версии")
		return
	}
	var hasPending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM question_file_versions WHERE question_file_id = $1 AND approval_status = 'pending'
	)`, fileID).Scan(&hasPending); err != nil {
		c.String(http.StatusInternalServerError, "Не удалось проверить ожидающие версии")
		return
	}
	if hasPending {
		c.String(http.StatusConflict, "Сначала подтвердите или отклоните ожидающую версию")
		return
	}
	randomPart, err := randomToken(12)
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сформировать ключ файла")
		return
	}
	objectKey := fmt.Sprintf("questions/%d/files/%d/versions/%d/%s%s", questionID, fileID, nextVersion, randomPart, upload.extension)
	putResult, err := app.storage.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &app.storage.bucket, Key: &objectKey, Body: upload.file,
		ContentLength: aws.Int64(upload.header.Size), ContentType: &upload.contentType,
		Metadata: map[string]string{"question-id": strconv.FormatInt(questionID, 10), "file-id": strconv.FormatInt(fileID, 10), "version-no": strconv.Itoa(nextVersion)},
	})
	if err != nil {
		log.Printf("upload question %d file %d version %d to S3: %v", questionID, fileID, nextVersion, err)
		c.String(http.StatusBadGateway, "S3 отклонил загрузку файла")
		return
	}
	versionID := ""
	if putResult.VersionId != nil {
		versionID = *putResult.VersionId
	}
	committed := false
	defer func() {
		if !committed {
			app.removeFailedQuestionUpload(objectKey, versionID)
		}
	}()
	approvalStatus := "pending"
	var reviewedBy any
	var reviewedAt any
	if usr.Role == "secretary" {
		approvalStatus = "confirmed"
		reviewedBy = usr.ID
		reviewedAt = time.Now()
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO question_file_versions (
			question_file_id, version_no, object_key, s3_version_id, original_filename,
			content_type, size_bytes, uploaded_by, approval_status, reviewed_by, reviewed_at
		) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, $10, $11)
	`, fileID, nextVersion, objectKey, versionID, upload.originalFilename, upload.contentType,
		upload.header.Size, usr.ID, approvalStatus, reviewedBy, reviewedAt)
	if err == nil && approvalStatus == "confirmed" {
		_, err = tx.Exec(ctx, `UPDATE question_files SET current_version_no = $2, updated_at = NOW() WHERE id = $1`, fileID, nextVersion)
	}
	if err == nil && approvalStatus == "confirmed" && questionStatus == "internal_review" {
		err = replaceActiveInternalReviewFileVersion(ctx, tx, questionID, fileID, nextVersion)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE questions SET updated_at = NOW() WHERE id = $1`, questionID)
	}
	if err == nil {
		details := fmt.Sprintf("Файл «%s», версия %d", title, nextVersion)
		if approvalStatus == "pending" {
			details += " ожидает подтверждения секретаря"
		}
		err = app.writeAudit(ctx, tx, usr, auditRecord{
			EventType: "question.file_version_uploaded", TargetType: "question_file", TargetID: &fileID,
			TargetLabel: title, QuestionID: &questionID, VersionNo: &nextVersion, Details: details,
		})
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("save question %d file %d version %d metadata: %v", questionID, fileID, nextVersion, err)
		c.String(http.StatusInternalServerError, "Не удалось сохранить сведения о версии")
		return
	}
	committed = true
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func (app *application) removeFailedQuestionUpload(objectKey, versionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	input := &s3.DeleteObjectInput{Bucket: &app.storage.bucket, Key: &objectKey}
	if versionID != "" {
		input.VersionId = &versionID
	}
	if _, err := app.storage.client.DeleteObject(ctx, input); err != nil {
		log.Printf("remove uncommitted S3 upload: %v", err)
	}
}

func (app *application) confirmQuestionFileVersion(c *gin.Context) {
	app.reviewQuestionFileVersion(c, true)
}

func (app *application) rejectQuestionFileVersion(c *gin.Context) {
	app.reviewQuestionFileVersion(c, false)
}

func (app *application) reviewQuestionFileVersion(c *gin.Context, confirm bool) {
	usr := c.MustGet("user").(user)
	questionID, fileID, versionNo, ok := parseQuestionFileVersionParams(c)
	if !ok {
		return
	}
	reason := strings.TrimSpace(c.PostForm("reason"))
	if !confirm && (reason == "" || len([]rune(reason)) > 2000) {
		c.String(http.StatusUnprocessableEntity, "Укажите причину отклонения длиной до 2 000 знаков")
		return
	}
	tx, err := app.db.Begin(c.Request.Context())
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось начать проверку версии")
		return
	}
	defer tx.Rollback(c.Request.Context())
	var title, approvalStatus, questionStatus string
	err = tx.QueryRow(c.Request.Context(), `
		SELECT f.title, v.approval_status, q.status
		FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		JOIN questions q ON q.id = f.question_id
		WHERE q.id = $1 AND f.id = $2 AND v.version_no = $3
		FOR UPDATE OF v, f, q
	`, questionID, fileID, versionNo).Scan(&title, &approvalStatus, &questionStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Версия файла не найдена")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось загрузить версию файла")
		return
	}
	if approvalStatus != "pending" {
		c.String(http.StatusConflict, "Решение по этой версии уже принято")
		return
	}
	if questionStatus == "committee_voting" || questionStatus == "approved" || questionStatus == "rejected" || questionStatus == "no_quorum" || questionStatus == "cancelled" {
		c.String(http.StatusConflict, "На текущем этапе комплект вопроса изменять нельзя")
		return
	}
	newStatus := "confirmed"
	if !confirm {
		newStatus = "rejected"
	}
	_, err = tx.Exec(c.Request.Context(), `
		UPDATE question_file_versions
		SET approval_status = $4, reviewed_by = $5, reviewed_at = NOW(), rejection_reason = $6
		WHERE question_file_id = $1 AND version_no = $2 AND approval_status = $3
	`, fileID, versionNo, "pending", newStatus, usr.ID, reason)
	if err == nil && confirm {
		_, err = tx.Exec(c.Request.Context(), `UPDATE question_files SET current_version_no = $2, updated_at = NOW() WHERE id = $1`, fileID, versionNo)
	}
	if err == nil && confirm && questionStatus == "internal_review" {
		err = replaceActiveInternalReviewFileVersion(c.Request.Context(), tx, questionID, fileID, versionNo)
	}
	if err == nil && !confirm && questionStatus == "internal_review" {
		err = app.finishActiveInternalReviewIfComplete(c.Request.Context(), tx, usr, questionID)
	}
	if err == nil {
		_, err = tx.Exec(c.Request.Context(), `UPDATE questions SET updated_at = NOW() WHERE id = $1`, questionID)
	}
	if err == nil {
		eventType := "question.file_version_confirmed"
		details := fmt.Sprintf("Подтверждена версия %d", versionNo)
		if !confirm {
			eventType = "question.file_version_rejected"
			details = fmt.Sprintf("Отклонена версия %d. Причина: %s", versionNo, reason)
		}
		err = app.writeAudit(c.Request.Context(), tx, usr, auditRecord{
			EventType: eventType, TargetType: "question_file", TargetID: &fileID,
			TargetLabel: title, QuestionID: &questionID, VersionNo: &versionNo, Details: details,
		})
	}
	if err == nil {
		err = tx.Commit(c.Request.Context())
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось сохранить решение по версии")
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/questions/%d", questionID))
}

func parseQuestionFileVersionParams(c *gin.Context) (int64, int64, int, bool) {
	questionID, errQuestion := strconv.ParseInt(c.Param("id"), 10, 64)
	fileID, errFile := strconv.ParseInt(c.Param("fileID"), 10, 64)
	versionNo, errVersion := strconv.Atoi(c.Param("version"))
	if errQuestion != nil || questionID < 1 || errFile != nil || fileID < 1 || errVersion != nil || versionNo < 1 {
		c.String(http.StatusBadRequest, "Некорректный идентификатор версии файла")
		return 0, 0, 0, false
	}
	return questionID, fileID, versionNo, true
}

func (app *application) loadQuestionFiles(ctx context.Context, questionID int64, usr user) ([]questionFileItem, error) {
	rows, err := app.db.Query(ctx, `
		SELECT id, title, category, COALESCE(current_version_no, 0)
		FROM question_files WHERE question_id = $1 AND status = 'active'
		ORDER BY created_at, id
	`, questionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []questionFileItem
	indexes := make(map[int64]int)
	for rows.Next() {
		var item questionFileItem
		var category string
		if err := rows.Scan(&item.ID, &item.Title, &category, &item.CurrentVersionNo); err != nil {
			return nil, err
		}
		item.CategoryLabel = questionFileCategoryLabel(category)
		indexes[item.ID] = len(files)
		files = append(files, item)
	}
	if err := rows.Err(); err != nil || len(files) == 0 {
		return files, err
	}
	versionRows, err := app.db.Query(ctx, `
		SELECT v.question_file_id, v.version_no, v.original_filename, v.size_bytes,
		       u.full_name, v.created_at, v.approval_status, v.rejection_reason
		FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		JOIN users u ON u.id = v.uploaded_by
		WHERE f.question_id = $1
		ORDER BY v.question_file_id, v.version_no DESC
	`, questionID)
	if err != nil {
		return nil, err
	}
	defer versionRows.Close()
	for versionRows.Next() {
		var fileID, size int64
		var created time.Time
		var item questionFileVersionItem
		if err := versionRows.Scan(&fileID, &item.VersionNo, &item.Filename, &size, &item.UploadedBy,
			&created, &item.Status, &item.RejectionReason); err != nil {
			return nil, err
		}
		index, ok := indexes[fileID]
		if !ok {
			continue
		}
		item.SizeLabel = formatBytes(size)
		item.CreatedLabel = created.Format("02.01.2006 15:04")
		item.StatusLabel = questionFileVersionStatusLabel(item.Status)
		item.IsCurrent = item.Status == "confirmed" && item.VersionNo == files[index].CurrentVersionNo
		item.CanReview = usr.Role == "secretary" && item.Status == "pending"
		files[index].HasPending = files[index].HasPending || item.Status == "pending"
		files[index].Versions = append(files[index].Versions, item)
	}
	return files, versionRows.Err()
}

func (app *application) downloadQuestionFileVersion(c *gin.Context) {
	usr := c.MustGet("user").(user)
	questionID, fileID, versionNo, ok := parseQuestionFileVersionParams(c)
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
		SELECT v.object_key, COALESCE(v.s3_version_id, ''), v.original_filename, v.content_type, v.size_bytes
		FROM question_file_versions v
		JOIN question_files f ON f.id = v.question_file_id
		JOIN questions q ON q.id = f.question_id
		WHERE q.id = $1 AND f.id = $2 AND v.version_no = $3
		  AND ($4 <> 'committee' OR q.status IN ('committee_voting', 'approved', 'rejected', 'no_quorum'))
	`, questionID, fileID, versionNo, usr.Role).Scan(&objectKey, &versionID, &filename, &contentType, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "Версия файла не найдена")
		return
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "Не удалось найти файл")
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
		log.Printf("download question %d file %d version %d from S3: %v", questionID, fileID, versionNo, err)
		c.String(http.StatusBadGateway, "Не удалось скачать файл из S3")
		return
	}
	defer object.Body.Close()
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, no-store")
	c.DataFromReader(http.StatusOK, size, contentType, object.Body, nil)
}
