package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"cups-web/internal/auth"
	"cups-web/internal/store"
	goipp "github.com/OpenPrinting/goipp"
	"github.com/gorilla/mux"
	rscpdf "rsc.io/pdf"
)

type photoRect struct{ x, y, w, h float64 }

// Inspect the produced PDF, including its actual clip paths and image matrices.
// All expected rectangles use mm and a top-left origin, like the UI.
func assertPhotoPDFLayout(t *testing.T, data []byte, pageHeight float64, images, clips []photoRect) {
	t.Helper()
	doc, err := rscpdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var imageRects, clipRects []photoRect
	imageRE := regexp.MustCompile(`([-\d.]+) 0 0 ([-\d.]+) ([-\d.]+) ([-\d.]+) cm`)
	clipRE := regexp.MustCompile(`q ([-\d.]+) ([-\d.]+) ([-\d.]+) ([-\d.]+) re W n`)
	mm := func(s string) float64 {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n * 25.4 / 72
	}
	for p := 1; p <= doc.NumPage(); p++ {
		stream := doc.Page(p).V.Key("Contents").Reader()
		contents, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range imageRE.FindAllStringSubmatch(string(contents), -1) {
			imageRects = append(imageRects, photoRect{mm(m[3]), pageHeight - mm(m[4]) - mm(m[2]), mm(m[1]), mm(m[2])})
		}
		for _, m := range clipRE.FindAllStringSubmatch(string(contents), -1) {
			clipRects = append(clipRects, photoRect{mm(m[1]), pageHeight - mm(m[2]), mm(m[3]), -mm(m[4])})
		}
	}
	check := func(name string, got, want []photoRect) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s count=%d, want %d: %+v", name, len(got), len(want), got)
		}
		for i := range want {
			g, w := got[i], want[i]
			if math.Abs(g.x-w.x) > 0.02 || math.Abs(g.y-w.y) > 0.02 || math.Abs(g.w-w.w) > 0.02 || math.Abs(g.h-w.h) > 0.02 {
				t.Errorf("%s[%d]=%+v, want %+v", name, i, g, w)
			}
		}
	}
	check("image", imageRects, images)
	check("clip", clipRects, clips)
}

func photoUploadRequest(t *testing.T, uri, field string, data []byte, count int, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < count; i++ {
		part, err := w.CreateFormFile(field, "photo.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range fields {
		if err := w.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, uri, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	t.Cleanup(func() {
		if req.MultipartForm != nil {
			req.MultipartForm.RemoveAll()
		}
	})
	return req
}

func TestConvertPhotoScalingLayout(t *testing.T) {
	s, _ := scheduledTestStore(t)
	previous := appStore
	appStore = s
	t.Cleanup(func() { appStore = previous })
	for _, tc := range []struct {
		name, scaling, orientation, borderless, field, perPage, rotations string
		width, height, count                                              int
		pageHeight                                                        float64
		images, clips                                                     []photoRect
	}{
		{name: "legacy-default", width: 200, height: 100, count: 1, pageHeight: 150, images: []photoRect{{0, 50, 100, 50}}},
		{name: "fit", scaling: "fit", width: 200, height: 100, count: 1, pageHeight: 150, images: []photoRect{{0, 50, 100, 50}}},
		{name: "fill-wide", scaling: "fill", width: 200, height: 100, count: 1, pageHeight: 150, images: []photoRect{{-100, 0, 300, 150}}, clips: []photoRect{{0, 0, 100, 150}}},
		{name: "fill-tall", scaling: "fill", width: 100, height: 200, count: 1, pageHeight: 150, images: []photoRect{{0, -25, 100, 200}}, clips: []photoRect{{0, 0, 100, 150}}},
		{name: "fill-square", scaling: "fill", width: 100, height: 100, count: 1, pageHeight: 150, images: []photoRect{{-25, 0, 150, 150}}, clips: []photoRect{{0, 0, 100, 150}}},
		{name: "fill-matching-ratio", scaling: "fill", width: 100, height: 150, count: 1, pageHeight: 150, images: []photoRect{{0, 0, 100, 150}}, clips: []photoRect{{0, 0, 100, 150}}},
		{name: "fill-landscape", scaling: "fill", orientation: "landscape", width: 100, height: 200, count: 1, pageHeight: 100, images: []photoRect{{0, -100, 150, 300}}, clips: []photoRect{{0, 0, 150, 100}}},
		{name: "fit-with-margins", scaling: "fit", borderless: "false", width: 200, height: 100, count: 1, pageHeight: 150, images: []photoRect{{10, 55, 80, 40}}},
		{name: "fill-with-margins", scaling: "fill", borderless: "false", width: 200, height: 100, count: 1, pageHeight: 150, images: []photoRect{{-80, 10, 260, 130}}, clips: []photoRect{{10, 10, 80, 130}}},
		{name: "fill-multiple-pages", scaling: "fill", field: "files", width: 200, height: 100, count: 2, pageHeight: 150, images: []photoRect{{-100, 0, 300, 150}, {-100, 0, 300, 150}}, clips: []photoRect{{0, 0, 100, 150}, {0, 0, 100, 150}}},
		{name: "fill-grid-rotated", scaling: "fill", field: "files", perPage: "2", rotations: "0,90", width: 200, height: 100, count: 2, pageHeight: 150, images: []photoRect{{-23, 0, 146, 73}, {0, 13.5, 100, 200}}, clips: []photoRect{{0, 0, 100, 73}, {0, 77, 100, 73}}},
		{name: "fit-grid-rotated", scaling: "fit", field: "files", perPage: "2", rotations: "0,90", width: 200, height: 100, count: 2, pageHeight: 150, images: []photoRect{{0, 11.5, 100, 50}, {31.75, 77, 36.5, 73}}},
		{name: "fill-grid-with-margins", scaling: "fill", borderless: "false", field: "files", perPage: "2", width: 200, height: 100, count: 2, pageHeight: 150, images: []photoRect{{-13, 10, 126, 63}, {-13, 77, 126, 63}}, clips: []photoRect{{10, 10, 80, 63}, {10, 77, 80, 63}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(writeCompressionTestImage(t, tc.width, tc.height))
			if err != nil {
				t.Fatal(err)
			}
			field, borderless := tc.field, tc.borderless
			if field == "" {
				field = "file"
			}
			if borderless == "" {
				borderless = "true"
			}
			req := photoUploadRequest(t, "/api/convert", field, data, tc.count, map[string]string{
				"paper_size": "custom:100x150", "orientation": tc.orientation, "print_scaling": tc.scaling,
				"borderless": borderless, "per_page": tc.perPage, "rotations": tc.rotations,
			})
			response := httptest.NewRecorder()
			convertHandler(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("convert failed: %d %s", response.Code, response.Body)
			}
			assertPhotoPDFLayout(t, response.Body.Bytes(), tc.pageHeight, tc.images, tc.clips)
		})
	}
}

// Exercise the uploaded original, its reprint, and a scheduled original through
// the real handlers and IPP submission, including the stored scaling snapshot.
func TestPhotoFillPrintReprintAndSchedule(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	data, err := os.ReadFile(writeCompressionTestImage(t, 200, 100))
	if err != nil {
		t.Fatal(err)
	}
	submissions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, "*PPD-Adobe: \"4.3\"\n*PageSize Photo/Photo: \"code\"\n*PaperDimension Photo: \"283.464567 425.196850\"\n*ImageableArea Photo: \"0 0 283.464567 425.196850\"\n")
			return
		}
		var request goipp.Message
		if err := request.Decode(r.Body); err != nil {
			t.Error(err)
			return
		}
		if request.Code == goipp.Code(goipp.OpPrintJob) {
			submissions++
			pdfData, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			assertPhotoPDFLayout(t, pdfData, 150, []photoRect{{-100, 0, 300, 150}}, []photoRect{{0, 0, 100, 150}})
			found := false
			for _, attr := range request.Job {
				if attr.Name == "print-scaling" {
					found = attr.Values[0].V.String() == "fill"
				}
			}
			if !found {
				t.Error("lost IPP scaling choice")
			}
		}
		response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, request.RequestID)
		response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(122)))
		encoded, err := response.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(encoded)
	}))
	defer server.Close()
	printer := server.URL + "/printers/Photo"
	driverJSON := `{"pageSize":{"attribute":"PageSize","value":"Photo"}}`
	req := photoUploadRequest(t, "/api/print", "file", data, 1, map[string]string{
		"printer": printer, "print_scaling": "fill", "orientation": "portrait", "driver_options": driverJSON,
	})
	session := auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}
	req = req.WithContext(auth.WithSession(req.Context(), session))
	response := httptest.NewRecorder()
	printHandler(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("print failed: %d %s", response.Code, response.Body)
	}
	var records []store.PrintRecord
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		var err error
		records, err = store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
		return err
	}); err != nil || len(records) != 1 {
		t.Fatalf("records=%+v, err=%v", records, err)
	}
	body, err := json.Marshal(map[string]any{
		"printer": printer, "printScaling": "fill", "orientation": "portrait", "driverOptions": json.RawMessage(driverJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/print-records/1/reprint", bytes.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(records[0].ID, 10)})
	req = req.WithContext(auth.WithSession(req.Context(), session))
	response = httptest.NewRecorder()
	reprintHandler(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("reprint failed: %d %s", response.Code, response.Body)
	}
	task := insertScheduledTestTask(t, s, user)
	task.PrinterURI, task.Orientation, task.PrintScaling = printer, "portrait", "fill"
	task.StoredPath, task.Filename = records[0].StoredPath, records[0].Filename
	task.DriverOptions = records[0].DriverOptions
	if status, err := executeScheduled(s, uploadDir, task); err != nil || status != "printed" {
		t.Fatalf("schedule failed: %s %v", status, err)
	}
	if submissions != 3 {
		t.Fatalf("submissions=%d, want 3", submissions)
	}
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		records, err := store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
		if err != nil {
			return err
		}
		if len(records) != 3 {
			t.Errorf("records=%d, want 3", len(records))
		}
		for _, record := range records {
			if record.PrintScaling != "fill" || record.Status != "printed" {
				t.Errorf("lost scaling/status snapshot: %+v", record)
			}
			if _, err := os.Stat(filepath.Join(uploadDir, record.StoredPath)); err != nil {
				t.Error(err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
