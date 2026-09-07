package main

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"testing"
)

func TestValidateQuestionFileContent(t *testing.T) {
	validXLS := append([]byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1},
		[]byte{'W', 0, 'o', 0, 'r', 0, 'k', 0, 'b', 0, 'o', 0, 'o', 0, 'k', 0}...)
	validXLSX := makeTestXLSX(t, false, true)
	tests := []struct {
		name        string
		extension   string
		contents    []byte
		wantType    string
		wantFailure bool
	}{
		{name: "xls", extension: ".xls", contents: validXLS, wantType: "application/vnd.ms-excel"},
		{name: "ole but not excel", extension: ".xls", contents: []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}, wantFailure: true},
		{name: "xlsx", extension: ".xlsx", contents: validXLSX, wantType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{name: "xlsx without metadata", extension: ".xlsx", contents: makeTestXLSX(t, false, false), wantFailure: true},
		{name: "xlsx with macros", extension: ".xlsx", contents: makeTestXLSX(t, true, true), wantFailure: true},
		{name: "fake xlsx", extension: ".xlsx", contents: []byte("plain text"), wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withMultipartFile(t, test.contents, func(file multipart.File) {
				contentType, err := validateQuestionFileContent(file, int64(len(test.contents)), test.extension)
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

func TestQuestionFileCategories(t *testing.T) {
	for _, category := range []string{"contract", "terms_summary", "lna_draft", "appendix", "explanatory_note", "calculation", "schedule", "other"} {
		if !validQuestionFileCategory(category) || questionFileCategoryLabel(category) == "" {
			t.Fatalf("category %q must be valid and have a label", category)
		}
	}
	if validQuestionFileCategory("") || validQuestionFileCategory("unknown") {
		t.Fatal("unknown category must not be valid")
	}
}

func makeTestXLSX(t *testing.T, withMacros, validMetadata bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := map[string]string{
		"_rels/.rels":     `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"></Relationships>`,
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"></workbook>`,
	}
	if validMetadata {
		entries["[Content_Types].xml"] = `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`
	} else {
		entries["[Content_Types].xml"] = `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`
	}
	if withMacros {
		entries["xl/vbaProject.bin"] = "macro"
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
