package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"testing"
	"time"

	"cups-web/internal/auth"
	"cups-web/internal/ipp"
	"cups-web/internal/store"
	goipp "github.com/OpenPrinting/goipp"
	"github.com/gorilla/mux"
	rscpdf "rsc.io/pdf"
)

func TestScheduledSubmissionOutcomeThroughPrintPipeline(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	uploads := t.TempDir()
	if err := os.MkdirAll(uploads+"/scheduled", 0755); err != nil {
		t.Fatal(err)
	}
	imagePath := writeCompressionTestImage(t, 20, 30)
	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range task.Files {
		if err := os.WriteFile(uploads+"/"+file.StoredPath, imageData, 0600); err != nil {
			t.Fatal(err)
		}
	}
	submissions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
			return
		}
		submissions++
		if submissions == 2 {
			io.WriteString(w, "response damaged after accepting job")
			return
		}
		response := goipp.NewResponse(goipp.DefaultVersion, goipp.Status(1), 1)
		response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(122)))
		data, err := response.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	task.PrinterURI = server.URL + "/printers/photo"
	task.PhotosPerRun = 3
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) }); err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, task.NextRunAt)
	err = executeScheduledOccurrence(s, uploads, task, now, false, executeScheduled)
	if !ipp.IsSubmissionUncertain(err) {
		t.Fatalf("lost uncertain result: %v", err)
	}
	actual := readScheduledTestTask(t, s, task.ID)
	if submissions != 2 || actual.SubmittedCount != 1 || actual.NextFileIndex != 1 || actual.Enabled || actual.LastStatus != "running" {
		t.Fatalf("batch may replay accepted jobs: %+v submissions=%d", actual, submissions)
	}
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		records, err := store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
		if err != nil {
			return err
		}
		if len(records) != 2 {
			t.Fatalf("records=%+v", records)
		}
		for _, rec := range records {
			expected := "printed"
			if rec.Filename == "02.png" {
				expected = "queued"
			}
			if rec.Status != expected {
				t.Fatalf("wrong status for %s: %s", rec.Filename, rec.Status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScheduledPauseOfflinePreservesCanonicalDriverOptions(t *testing.T) {
	s, user := scheduledTestStore(t)
	previous := appStore
	appStore = s
	t.Cleanup(func() { appStore = previous })
	task := insertScheduledTestTask(t, s, user)
	task.DriverOptions = ipp.DriverOptions{PageSize: &ipp.PrinterChoice{Attribute: "PageSize", Value: "Photo", Label: "Photo", WidthMM: 101.6, HeightMM: 152.4}}
	task.PhotosPerRun = 3
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) }); err != nil {
		t.Fatal(err)
	}
	payload := mapScheduledPrint(task)
	payload.Enabled = false
	payload.DriverOptions.PageSize = &ipp.PrinterChoice{Attribute: "PageSize", Value: "Photo", WidthMM: 999, HeightMM: 999}
	for _, count := range []int{3, 0, 101} {
		payload.PhotosPerRun = count
		data, _ := json.Marshal(payload)
		req := httptest.NewRequest("PUT", "/api/scheduled-prints/"+strconv.FormatInt(task.ID, 10), bytes.NewReader(data))
		req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(task.ID, 10)})
		req = req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}))
		response := httptest.NewRecorder()
		scheduledUpdateHandler(response, req)
		if count == 3 && response.Code != http.StatusOK {
			t.Fatalf("offline pause failed: %d %s", response.Code, response.Body)
		}
		if count != 3 && response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid batch count %d: %d", count, response.Code)
		}
	}
	actual := readScheduledTestTask(t, s, task.ID)
	if actual.Enabled || actual.PhotosPerRun != 3 || actual.DriverOptions.PageSize.WidthMM != 101.6 {
		t.Fatalf("lost or forged settings: %+v", actual)
	}
}

func TestScheduledNewPrinterValidatesRetainedChoices(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	previous := appStore
	appStore = s
	t.Cleanup(func() { appStore = previous })
	task := insertScheduledTestTask(t, s, user)
	task.PhotosPerRun = 1
	task.DriverOptions = ipp.DriverOptions{PageSize: &ipp.PrinterChoice{Attribute: "PageSize", Value: "Photo", WidthMM: 101.6, HeightMM: 152.4}}
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) }); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, "*PPD-Adobe: \"4.3\"\n*PageSize Other/Other: \"code\"\n*PaperDimension Other: \"595 842\"\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	data, _ := json.Marshal(mapScheduledPrint(task))
	var payload map[string]interface{}
	json.Unmarshal(data, &payload)
	delete(payload, "driverOptions") // Legacy update retains choices, but the URI is changing.
	payload["printerUri"] = server.URL + "/printers/Other"
	data, _ = json.Marshal(payload)
	request := func() *http.Request {
		req := httptest.NewRequest("PUT", "/api/scheduled-prints/"+strconv.FormatInt(task.ID, 10), bytes.NewReader(data))
		req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(task.ID, 10)})
		return req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}))
	}
	if !acquireScheduledTask(task.ID, true) {
		t.Fatal("task lock unavailable")
	}
	busy := httptest.NewRecorder()
	scheduledUpdateHandler(busy, request())
	releaseScheduledTask(task.ID)
	if busy.Code != http.StatusConflict {
		t.Fatalf("edit running task: %d %s", busy.Code, busy.Body)
	}
	response := httptest.NewRecorder()
	scheduledUpdateHandler(response, request())
	if response.Code != http.StatusBadRequest {
		t.Fatalf("retained an incompatible choice: %d %s", response.Code, response.Body)
	}
	actual := readScheduledTestTask(t, s, task.ID)
	if actual.PrinterURI != task.PrinterURI || !actual.DriverOptions.SameChoices(task.DriverOptions) {
		t.Fatal("invalid edit modified the task")
	}
}

func TestDriverOptionsStoredForPrintAndSchedule(t *testing.T) {
	s, user := scheduledTestStore(t)
	options := ipp.DriverOptions{
		PageSize:  &ipp.PrinterChoice{Attribute: "PageSize", Value: "Photo.Borderless", Label: "照片（无边距）", WidthMM: 101.6, HeightMM: 152.4, Borderless: true},
		MediaType: &ipp.PrinterChoice{Attribute: "MediaType", Value: "PremiumGlossy", Label: "超级光泽纸"},
		Quality:   &ipp.PrinterChoice{Attribute: "Ink", Value: "Photo", Label: "高质量"},
	}
	var id int64
	err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		rec := store.PrintRecord{UserID: user.ID, PrinterURI: "ipp://localhost/printers/photo", Filename: "a.jpg", StoredPath: "a.jpg", Pages: 1, Status: "printed", DriverOptions: options, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		var err error
		id, err = store.InsertPrintRecord(context.Background(), tx, &rec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		rec, err := store.GetPrintRecordByID(context.Background(), tx, id)
		if err != nil {
			return err
		}
		response := mapPrintRecords([]store.PrintRecord{rec})[0]
		if !reflect.DeepEqual(response.DriverOptions, options) {
			t.Fatalf("lost reprint snapshot: %+v", response)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task := insertScheduledTestTask(t, s, user)
	task.DriverOptions = options
	task.PhotosPerRun = 5
	err = s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) })
	if err != nil {
		t.Fatal(err)
	}
	restored := readScheduledTestTask(t, s, task.ID)
	if !reflect.DeepEqual(restored.DriverOptions, options) || restored.PhotosPerRun != 5 {
		t.Fatalf("lost schedule snapshot: %+v", restored)
	}
	// Missing fields on legacy edits retain these choices and the batch size.
	payload := scheduledUpdatePayload{PaperSize: "A4"}
	applyScheduledPayload(&restored, payload)
	if !reflect.DeepEqual(restored.DriverOptions, options) || restored.PhotosPerRun != 5 {
		t.Fatal("legacy edit discarded driver/batch settings")
	}
}

func TestBorderlessImagePDFDimensionsAndPlacement(t *testing.T) {
	input := writeCompressionTestImage(t, 200, 300)
	for _, borderless := range []bool{false, true} {
		path, cleanup, err := convertImageToPDF(input, "portrait", "custom:101.6x152.4", false, false, imagePDFOptions{Borderless: borderless})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		info, _ := f.Stat()
		doc, err := rscpdf.NewReader(f, info.Size())
		if err != nil {
			t.Fatal(err)
		}
		page := doc.Page(1).V
		box := page.Key("MediaBox")
		for box.IsNull() && !page.IsNull() {
			page = page.Key("Parent")
			box = page.Key("MediaBox")
		}
		if math.Abs(box.Index(2).Float64()-288) > 0.01 || math.Abs(box.Index(3).Float64()-432) > 0.01 {
			t.Fatalf("wrong PDF page: %v", box)
		}
		stream := doc.Page(1).V.Key("Contents").Reader()
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		match := regexp.MustCompile(`([0-9.]+) 0 0 ([0-9.]+) ([0-9.]+) ([0-9.]+) cm`).FindStringSubmatch(string(data))
		fullPage := false
		if len(match) == 5 {
			w, _ := strconv.ParseFloat(match[1], 64)
			h, _ := strconv.ParseFloat(match[2], 64)
			x, _ := strconv.ParseFloat(match[3], 64)
			y, _ := strconv.ParseFloat(match[4], 64)
			fullPage = math.Abs(w-288) < 0.01 && math.Abs(h-432) < 0.01 && x == 0 && y == 0
		}
		if fullPage != borderless {
			t.Fatalf("borderless=%v placement=%s", borderless, data)
		}
	}
}
