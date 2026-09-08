package main

import (
	"bytes"
	"io"
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

func TestAttachmentRequestAllowsCombinedSizeAboveSingleFileLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := &application{sessionSecret: []byte("attachments-test")}
	for _, validCSRF := range []bool{true, false} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		token := "invalid"
		if validCSRF {
			token = app.csrfDigest("session", "attachments-session")
		}
		if err := writer.WriteField("_csrf", token); err != nil {
			t.Fatal(err)
		}
		// Two valid PDFs, each below 25 MB, together above the old 26 MB cap.
		payload := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte(" "), 14<<20)...)
		payload = append(payload, []byte("\n%%EOF\n")...)
		for i := 0; i < 2; i++ {
			part, err := writer.CreateFormFile("attachments", "comment.pdf")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(part, bytes.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		router := gin.New()
		router.POST("/vote", app.requireCSRFWithLimit(maxAttachmentsBodySize), func(c *gin.Context) {
			uploads, status, message := receiveVisaAttachments(c)
			defer closeQuestionUploads(uploads)
			if message != "" || status != 0 || len(uploads) != 2 {
				t.Errorf("uploads=%d status=%d message=%s", len(uploads), status, message)
			}
			c.Status(http.StatusNoContent)
		})
		request := httptest.NewRequest(http.MethodPost, "/vote", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.AddCookie(&http.Cookie{Name: "session_token", Value: "attachments-session"})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if request.MultipartForm != nil {
			if err := request.MultipartForm.RemoveAll(); err != nil {
				t.Fatal(err)
			}
		}
		want := http.StatusForbidden
		if validCSRF {
			want = http.StatusNoContent
		}
		if response.Code != want {
			t.Fatalf("csrf=%v status=%d", validCSRF, response.Code)
		}
	}
}
