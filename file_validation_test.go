package main

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"os"
	"testing"
)

func TestValidateDocumentContent(t *testing.T) {
	validDOC := append([]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1},
		[]byte{'W', 0, 'o', 0, 'r', 0, 'd', 0, 'D', 0, 'o', 0, 'c', 0, 'u', 0, 'm', 0, 'e', 0, 'n', 0, 't', 0}...)
	validDOCX := makeTestDOCX(t, false, true)

	tests := []struct {
		name        string
		extension   string
		contents    []byte
		wantType    string
		wantFailure bool
	}{
		{name: "pdf", extension: ".pdf", contents: []byte("%PDF-1.7\n1 0 obj\n%%EOF\n"), wantType: "application/pdf"},
		{name: "pdf without eof", extension: ".pdf", contents: []byte("%PDF-1.7\n1 0 obj\n"), wantFailure: true},
		{name: "fake pdf", extension: ".pdf", contents: []byte("plain text %%EOF"), wantFailure: true},
		{name: "invalid pdf version", extension: ".pdf", contents: []byte("%PDF-1.x\n%%EOF\n"), wantFailure: true},
		{name: "doc", extension: ".doc", contents: validDOC, wantType: "application/msword"},
		{name: "ole but not word", extension: ".doc", contents: []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, wantFailure: true},
		{name: "docx", extension: ".docx", contents: validDOCX, wantType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{name: "fake docx", extension: ".docx", contents: []byte("not a zip"), wantFailure: true},
		{name: "docx missing metadata", extension: ".docx", contents: makeTestDOCX(t, false, false), wantFailure: true},
		{name: "docx with macros", extension: ".docx", contents: makeTestDOCX(t, true, true), wantFailure: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withMultipartFile(t, test.contents, func(file multipart.File) {
				contentType, err := validateDocumentContent(file, int64(len(test.contents)), test.extension)
				if (err != nil) != test.wantFailure {
					t.Fatalf("error=%v, wantFailure=%v", err, test.wantFailure)
				}
				if contentType != test.wantType {
					t.Fatalf("contentType=%q, want %q", contentType, test.wantType)
				}
			})
		})
	}
}

func makeTestDOCX(t *testing.T, withMacros, validMetadata bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := map[string]string{
		"_rels/.rels":       `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
		"word/document.xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body/></w:document>`,
	}
	if validMetadata {
		entries["[Content_Types].xml"] = `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`
	} else {
		entries["[Content_Types].xml"] = `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`
	}
	if withMacros {
		entries["word/vbaProject.bin"] = "macro"
	}
	for name, contents := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func withMultipartFile(t *testing.T, contents []byte, check func(multipart.File)) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "document-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	check(file)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
