package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 用独立子进程检查实际环境、参数和产物处理，不依赖开发机安装转换工具。
func installFakeConversionTool(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake conversion tools require /bin/sh")
	}
	binDir := t.TempDir()
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nset -eu\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", tmpDir)
	// 即使宿主继承了不可用的 locale，转换子进程也应覆盖它。
	t.Setenv("LANG", "missing_LOCALE.UTF-8")
	t.Setenv("LC_ALL", "missing_LOCALE.UTF-8")
	t.Setenv("CUPS_TEST_PDF", string(minimalPDF))
	return tmpDir
}

const fakeLibreOfficeScript = `
[ "$LANG" = "C.UTF-8" ] && [ "$LC_ALL" = "C.UTF-8" ] || exit 90
[ "$1" = "--headless" ] && [ "$2" = "--convert-to" ] && [ "$3" = "pdf" ] || exit 91
shift 3
if [ -n "$CUPS_TEST_INFILTER" ]; then
    [ "$1" = "--infilter=$CUPS_TEST_INFILTER" ] || exit 92
    shift
fi
[ "$1" = "--outdir" ] || exit 93
outdir=$2
input=$3
[ "$input" = "$CUPS_TEST_INPUT" ] || exit 94
base=${input##*/}
output="$outdir/${base%.*}.pdf"
case "$CUPS_TEST_MODE" in
    success) printf '%s' "$CUPS_TEST_PDF" > "$output" ;;
    fallback) printf '%s' "$CUPS_TEST_PDF" > "$outdir/renamed.pdf" ;;
    missing) echo 'Error: source file could not be loaded' >&2 ;;
    empty) : > "$output" ;;
    directory) mkdir "$output" ;;
    unreadable) ln -s "$outdir/absent" "$output" ;;
    failure) echo 'Error: conversion failed' >&2; exit 7 ;;
esac
`

func TestLibreOfficeConversion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		infilter string
		wantErr  string
	}{
		{name: "Chinese filename", mode: "success"},
		{name: "renamed output", mode: "fallback"},
		{name: "PDF normalization fallback", mode: "success", infilter: "writer_pdf_import"},
		{name: "exit zero without PDF", mode: "missing", wantErr: "conversion produced no PDF: Error: source file could not be loaded"},
		{name: "empty output", mode: "empty", wantErr: "conversion produced invalid PDF"},
		{name: "directory output", mode: "directory", wantErr: "conversion produced invalid PDF"},
		{name: "unreadable output", mode: "unreadable", wantErr: "conversion produced invalid PDF"},
		{name: "nonzero exit", mode: "failure", wantErr: "exit status 7 - Error: conversion failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := installFakeConversionTool(t, "libreoffice", fakeLibreOfficeScript)
			input := writeTempFile(t, []byte("test input"), "中文 测试文档.docx")
			if tc.infilter != "" {
				input = writeTempFile(t, minimalPDF, "中文 测试文档.pdf")
			}
			t.Setenv("CUPS_TEST_INPUT", input)
			t.Setenv("CUPS_TEST_INFILTER", tc.infilter)
			t.Setenv("CUPS_TEST_MODE", tc.mode)

			var path string
			var cleanup func()
			var err error
			if tc.infilter != "" {
				path, cleanup, err = convertPDFViaLibreOffice(context.Background(), input)
			} else {
				path, cleanup, err = convertOfficeToPDF(context.Background(), input)
			}
			if tc.wantErr != "" {
				if cleanup != nil {
					defer cleanup()
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if path != "" || cleanup != nil {
					t.Fatal("failed conversion returned output or cleanup")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if cleanup == nil {
					t.Fatal("successful conversion must return cleanup")
				}
				defer cleanup()
				data, err := os.ReadFile(path)
				if err != nil || string(data) != string(minimalPDF) {
					t.Fatalf("unexpected PDF output: %v", err)
				}
				cleanup()
			}
			assertConversionTempDirsRemoved(t, tmpDir)
		})
	}
}

func TestOFDConversionUTF8Locale(t *testing.T) {
	tmpDir := installFakeConversionTool(t, "java", `
[ "$LANG" = "C.UTF-8" ] && [ "$LC_ALL" = "C.UTF-8" ] || exit 90
[ "$1" = "-Xmx512m" ] && [ "$2" = "-jar" ] || exit 91
[ "$3" = "$OFD_CONVERTER_JAR" ] && [ "$4" = "$CUPS_TEST_INPUT" ] || exit 92
printf '%s' "$CUPS_TEST_PDF" > "$5"
`)
	input := writeTempFile(t, []byte("test input"), "中文 测试文档.ofd")
	t.Setenv("CUPS_TEST_INPUT", input)
	t.Setenv("OFD_CONVERTER_JAR", filepath.Join(t.TempDir(), "转换器.jar"))
	path, cleanup, err := convertOFDToPDF(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("successful conversion must return cleanup")
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(minimalPDF) {
		t.Fatalf("unexpected PDF output: %v", err)
	}
	cleanup()
	assertConversionTempDirsRemoved(t, tmpDir)
}

func assertConversionTempDirsRemoved(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "convert-") {
			t.Errorf("conversion left temporary directory %q", entry.Name())
		}
	}
}
