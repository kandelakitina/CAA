package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestReceiveVisaAttachmentsLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for index := 0; index < maxVisaAttachments+1; index++ {
		part, err := writer.CreateFormFile("attachments", "comment.pdf")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("%PDF-1.7\n%%EOF\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodPost, "/", &body)
	context.Request.Header.Set("Content-Type", writer.FormDataContentType())
	uploads, status, message := receiveVisaAttachments(context)
	closeQuestionUploads(uploads)
	if status != http.StatusRequestEntityTooLarge || message == "" {
		t.Fatalf("status=%d message=%q", status, message)
	}
}
