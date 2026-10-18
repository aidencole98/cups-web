package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"cups-web/internal/auth"
	"cups-web/internal/store"
	goipp "github.com/OpenPrinting/goipp"
	"github.com/gorilla/mux"
	"github.com/phpdave11/gofpdf"
	rscpdf "rsc.io/pdf"
)

// Check the conversion response that the browser saves/previews and then uploads
// as a PDF, rather than relying on a client-supplied flag about its layout.
func TestConvertedPhotoFillPDF(t *testing.T) {
	s, _ := scheduledTestStore(t)
	previous := appStore
	appStore = s
	t.Cleanup(func() { appStore = previous })
	data, err := os.ReadFile(writeCompressionTestImage(t, 200, 100))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field, perPage, rotations, orientation, borderless string
		count                                                    int
		height                                                   float64
		regions                                                  []photoRect
	}{
		{name: "single", field: "file", count: 1, height: 150, borderless: "true", regions: []photoRect{{0, 0, 100, 150}}},
		{name: "landscape", field: "file", count: 1, orientation: "landscape", height: 100, borderless: "true", regions: []photoRect{{0, 0, 150, 100}}},
		{name: "margins", field: "file", count: 1, height: 150, regions: []photoRect{{10, 10, 80, 130}}},
		{name: "multiple-pages", field: "files", count: 2, height: 150, borderless: "true", regions: []photoRect{{0, 0, 100, 150}}},
		{name: "rotated-grid", field: "files", count: 2, perPage: "2", rotations: "0,90", height: 150, borderless: "true", regions: []photoRect{{0, 0, 100, 73}, {0, 77, 100, 73}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dates := []string{"2026-10-03"}
			if tc.count == 2 {
				dates = append(dates, "2024-02-29")
			}
			rawDates, _ := json.Marshal(dates)
			req := photoUploadRequest(t, "/api/convert", tc.field, data, tc.count, map[string]string{
				"paper_size": "custom:100x150", "orientation": tc.orientation, "print_scaling": "fill",
				"borderless": tc.borderless, "per_page": tc.perPage, "rotations": tc.rotations,
				"photo_date_watermark": "true", "photo_dates": string(rawDates),
			})
			res := httptest.NewRecorder()
			convertHandler(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("convert: %d %s", res.Code, res.Body.String())
			}
			path := filepath.Join(t.TempDir(), "converted.pdf")
			if err := os.WriteFile(path, res.Body.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if got := photoPrintScaling(path, "fill", "application/pdf", tc.orientation, "custom:100x150"); got != "none" {
				t.Fatalf("converted photo scaling=%q", got)
			}
			doc, err := rscpdf.NewReader(bytes.NewReader(res.Body.Bytes()), int64(res.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= doc.NumPage(); i++ {
				text := doc.Page(i).Content().Text
				if len(text) == 0 {
					t.Fatal("missing date text")
				}
				seen := make([]bool, len(tc.regions))
				for _, glyph := range text {
					x, y := glyph.X*25.4/72, tc.height-(glyph.Y+glyph.FontSize)*25.4/72
					w, h := glyph.W*25.4/72, glyph.FontSize*25.4/72
					inside := false
					for j, r := range tc.regions {
						// Include foreground and shadow, and require a visible gap
						// between every glyph's full bounds and its photo's edges.
						if x > r.x+0.5 && y > r.y+0.5 && x+w < r.x+r.w-0.5 && y+h < r.y+r.h-0.5 {
							inside, seen[j] = true, true
						}
					}
					if !inside {
						t.Errorf("date glyph outside photo content: %+v", glyph)
					}
				}
				for j, found := range seen {
					if !found {
						t.Errorf("photo slot %d has no date", j)
					}
				}
			}
		})
	}
}

func TestPhotoFillScalingFallback(t *testing.T) {
	imagePath := writeCompressionTestImage(t, 200, 100)
	filled, cleanup, err := convertImageToPDF(imagePath, "portrait", "6inch", false, false, imagePDFOptions{PrintScaling: "fill"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	fit, cleanup, err := convertImageToPDF(imagePath, "portrait", "6inch", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	malformed := filepath.Join(t.TempDir(), "broken.pdf")
	if err := os.WriteFile(malformed, []byte("%PDF-1.4\nbroken"), 0600); err != nil {
		t.Fatal(err)
	}
	cyclic := filepath.Join(t.TempDir(), "cyclic.pdf")
	data, err := os.ReadFile(filled)
	if err != nil {
		t.Fatal(err)
	}
	// Keep byte offsets intact but replace the first page reference with the
	// parent page-tree object itself. Reader.Page would loop on this tree.
	needle := []byte("/Kids [3 0 R")
	if !bytes.Contains(data, needle) {
		t.Fatal("missing expected gofpdf page tree")
	}
	data = bytes.Replace(data, needle, []byte("/Kids [1 0 R"), 1)
	if err := os.WriteFile(cyclic, data, 0600); err != nil {
		t.Fatal(err)
	}
	mixed := newPhotoPDF("portrait", "6inch")
	mixed.SetKeywords(photoFillLayoutKeyword, false)
	mixed.AddPage()
	mixed.AddPageFormat("P", gofpdf.SizeType{Wd: 210, Ht: 297})
	mixedPath := filepath.Join(t.TempDir(), "mixed.pdf")
	if err := mixed.OutputFileAndClose(mixedPath); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, scaling, mime, orientation, paper, want string
	}{
		{"matching-photo", filled, "fill", "application/pdf", "portrait", "6inch", "none"},
		{"equivalent-custom-paper", filled, "fill", "application/pdf", "portrait", "custom:102x152", "none"},
		{"changed-paper", filled, "fill", "application/pdf", "portrait", "A4", "fill"},
		{"changed-orientation", filled, "fill", "application/pdf", "landscape", "6inch", "fill"},
		{"ordinary-pdf", fit, "fill", "application/pdf", "portrait", "6inch", "fill"},
		{"mixed-pages", mixedPath, "fill", "application/pdf", "portrait", "6inch", "fill"},
		{"other-scaling", filled, "fit", "application/pdf", "portrait", "6inch", "fit"},
		{"custom-scaling", filled, "150", "application/pdf", "portrait", "6inch", "150"},
		{"other-mime", filled, "fill", "application/octet-stream", "portrait", "6inch", "fill"},
		{"malformed", malformed, "fill", "application/pdf", "portrait", "6inch", "fill"},
		{"cyclic-page-tree", cyclic, "fill", "application/pdf", "portrait", "6inch", "fill"},
		{"missing", filled + ".missing", "fill", "application/pdf", "portrait", "6inch", "fill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := photoPrintScaling(tc.path, tc.scaling, tc.mime, tc.orientation, tc.paper); got != tc.want {
				t.Errorf("scaling=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertedPhotoPrintReprintAndSchedule(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	imagePath := writeCompressionTestImage(t, 200, 100)
	photo, cleanup, err := convertImageToPDF(imagePath, "portrait", "6inch", false, false, imagePDFOptions{
		PrintScaling: "fill", PhotoDateWatermark: true, PhotoDates: []string{"2026-10-03"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pdfData, err := os.ReadFile(photo)
	if err != nil {
		t.Fatal(err)
	}
	submissions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message goipp.Message
		if err := message.Decode(r.Body); err != nil {
			t.Error(err)
			return
		}
		if message.Code == goipp.Code(goipp.OpPrintJob) {
			submissions++
			scaling := ""
			for _, attr := range message.Job {
				if attr.Name == "print-scaling" {
					scaling = attr.Values[0].V.String()
				}
			}
			if scaling != "none" {
				t.Errorf("submission %d repeats fill: %q", submissions, scaling)
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(data, pdfData) {
				t.Error("printing changed the completed photo PDF")
			}
		}
		response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, message.RequestID)
		response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(124)))
		data, err := response.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	printer := server.URL + "/printers/photo"
	session := auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}
	req := photoUploadRequest(t, "/api/print", "file", pdfData, 1, map[string]string{
		"printer": printer, "paper_size": "6inch", "orientation": "portrait", "print_scaling": "fill", "photo_date_watermark": "true",
	})
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	req.MultipartForm.File["file"][0].Filename = "converted.pdf"
	req = req.WithContext(auth.WithSession(req.Context(), session))
	res := httptest.NewRecorder()
	printHandler(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("print: %d %s", res.Code, res.Body.String())
	}
	var recordID int64
	if err := s.DB.QueryRow("SELECT MAX(id) FROM print_jobs").Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(reprintRequest{Printer: printer, PaperSize: "6inch", Orientation: "portrait", PrintScaling: "fill", PhotoDateWatermark: true})
	req = httptest.NewRequest(http.MethodPost, "/api/print-records/1/reprint", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(recordID, 10)})
	req = req.WithContext(auth.WithSession(req.Context(), session))
	res = httptest.NewRecorder()
	reprintHandler(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("reprint: %d %s", res.Code, res.Body.String())
	}
	task := insertScheduledTestTask(t, s, user)
	task.PrinterURI, task.PaperSize, task.Orientation, task.PrintScaling = printer, "6inch", "portrait", "fill"
	task.Files = nil
	task.Filename, task.StoredPath = "converted.pdf", "converted.pdf"
	if err := os.WriteFile(filepath.Join(uploadDir, task.StoredPath), pdfData, 0600); err != nil {
		t.Fatal(err)
	}
	if status, err := executeScheduled(s, uploadDir, task); err != nil || status != "printed" {
		t.Fatalf("schedule: %s %v", status, err)
	}
	if submissions != 3 {
		t.Fatalf("submissions=%d, want 3", submissions)
	}
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		records, err := store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.PrintScaling != "fill" {
				t.Errorf("history lost user scaling: %+v", record)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
