package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cups-web/internal/auth"
	"cups-web/internal/ipp"
	"cups-web/internal/store"
	goipp "github.com/OpenPrinting/goipp"
	"github.com/gorilla/mux"
	rscpdf "rsc.io/pdf"
)

// Verify the actual PDF and IPP request together with durable batch/history state.
func TestScheduledPhotoOrientationPipeline(t *testing.T) {
	for _, tc := range []struct {
		name, orientation, paper string
		driver                   bool
		widthPt, heightPt        float64
		corrupt                  bool
		manual                   bool
	}{
		{name: "standard", orientation: "auto", paper: "A4", widthPt: 595.28, heightPt: 841.89},
		{name: "photo-manual-trigger", orientation: "auto", paper: "6inch", widthPt: 102 * 72 / 25.4, heightPt: 152 * 72 / 25.4, manual: true},
		{name: "driver", orientation: "auto", paper: "288 432", driver: true, widthPt: 288, heightPt: 432},
		{name: "driver-wide-dimensions", orientation: "auto", paper: "432 288", driver: true, widthPt: 288, heightPt: 432},
		{name: "manual-portrait", orientation: "portrait", paper: "6inch", widthPt: 102 * 72 / 25.4, heightPt: 152 * 72 / 25.4},
		{name: "manual-landscape", orientation: "landscape", paper: "6inch", widthPt: 102 * 72 / 25.4, heightPt: 152 * 72 / 25.4},
		{name: "manual-driver-portrait", orientation: "portrait", paper: "432 288", driver: true, widthPt: 288, heightPt: 432},
		{name: "unreadable-photo", orientation: "auto", paper: "A4", widthPt: 595.28, heightPt: 841.89, corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CUPS_SOCKET_PATH", "")
			s, user := scheduledTestStore(t)
			task := insertScheduledTestTask(t, s, user)
			task.Orientation, task.PhotosPerRun = tc.orientation, 3
			uploads := t.TempDir()
			if err := os.MkdirAll(filepath.Join(uploads, "scheduled"), 0755); err != nil {
				t.Fatal(err)
			}
			task.Files = nil
			for _, photo := range []struct {
				name   string
				width  int
				height int
			}{{"wide.png", 30, 20}, {"tall.jpg", 20, 30}, {"square.gif", 20, 20}} {
				var data bytes.Buffer
				img := image.NewRGBA(image.Rect(0, 0, photo.width, photo.height))
				var err error
				switch filepath.Ext(photo.name) {
				case ".png":
					err = png.Encode(&data, img)
				case ".jpg":
					err = jpeg.Encode(&data, img, nil)
				case ".gif":
					err = gif.Encode(&data, img, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.corrupt && photo.name == "tall.jpg" {
					data.Reset()
					data.WriteString("damaged photo")
				}
				path := "scheduled/" + photo.name
				if err := os.WriteFile(filepath.Join(uploads, path), data.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				task.Files = append(task.Files, store.ScheduledFile{Filename: photo.name, StoredPath: path})
			}
			task.Filename, task.StoredPath = task.Files[0].Filename, task.Files[0].StoredPath
			wantDirections := []string{"landscape", "portrait", "portrait"}
			if tc.orientation != "auto" {
				wantDirections = []string{tc.orientation, tc.orientation, tc.orientation}
			}
			submissions := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					io.WriteString(w, "*PPD-Adobe: \"4.3\"\n*PageSize Photo/Photo: \"code\"\n*PaperDimension Photo: \""+tc.paper+"\"\n*ImageableArea Photo: \"0 0 "+tc.paper+"\"\n")
					return
				}
				var request goipp.Message
				if err := request.Decode(r.Body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Code != goipp.Code(goipp.OpPrintJob) {
					response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, request.RequestID)
					data, err := response.EncodeBytes()
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", goipp.ContentType)
					w.Write(data)
					return
				}
				pdfData, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if submissions >= len(wantDirections) {
					t.Error("unexpected extra submission")
					return
				}
				direction := wantDirections[submissions]
				submissions++
				var orientation string
				for _, attr := range request.Job {
					if attr.Name == "orientation-requested" {
						orientation = attr.Values[0].V.String()
					}
				}
				// Preserve the existing IPP convention: landscape=4, portrait omitted.
				wantIPP := ""
				if direction == "landscape" {
					wantIPP = "4"
				}
				if orientation != wantIPP {
					t.Errorf("IPP orientation=%q, want %q", orientation, wantIPP)
				}
				doc, err := rscpdf.NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
				if err != nil {
					t.Error(err)
					return
				}
				if doc.NumPage() != 1 {
					t.Errorf("pages=%d", doc.NumPage())
				}
				page := doc.Page(1).V
				box := page.Key("MediaBox")
				for box.IsNull() && !page.IsNull() {
					page = page.Key("Parent")
					box = page.Key("MediaBox")
				}
				width, height := tc.widthPt, tc.heightPt
				if direction == "landscape" {
					width, height = height, width
				}
				if math.Abs(box.Index(2).Float64()-width) > 0.02 || math.Abs(box.Index(3).Float64()-height) > 0.02 {
					t.Errorf("PDF MediaBox=%v, want %.2fx%.2f", box, width, height)
				}
				response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, request.RequestID)
				response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(submissions)))
				data, err := response.EncodeBytes()
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", goipp.ContentType)
				w.Write(data)
			}))
			defer server.Close()
			task.PrinterURI = server.URL + "/printers/Photo"
			if tc.driver {
				options, err := ipp.GetPrinterOptions(context.Background(), task.PrinterURI, false)
				if err != nil || len(options.PaperSizes) != 1 {
					t.Fatalf("driver options=%+v, err=%v", options, err)
				}
				task.DriverOptions.PageSize = &options.PaperSizes[0]
			} else {
				task.PaperSize = tc.paper
			}
			if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
				return store.UpdateScheduledPrint(context.Background(), tx, &task)
			}); err != nil {
				t.Fatal(err)
			}
			now, _ := time.Parse(time.RFC3339, task.NextRunAt)
			err := executeScheduledOccurrence(s, uploads, task, now, tc.manual, executeScheduled)
			wantCount := 3
			if tc.corrupt {
				wantCount = 1
				if err == nil || !strings.Contains(err.Error(), "读取图片尺寸失败") {
					t.Fatalf("corrupted photo error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			actual := readScheduledTestTask(t, s, task.ID)
			if submissions != wantCount || actual.SubmittedCount != wantCount || actual.NextFileIndex != wantCount || actual.Orientation != tc.orientation {
				t.Fatalf("wrong progress/retained direction: submissions=%d, task=%+v", submissions, actual)
			}
			if tc.corrupt && (!actual.Enabled || actual.LastStatus != "failed") {
				t.Fatalf("failure should retain photo for next occurrence: %+v", actual)
			}
			if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
				records, err := store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
				if err != nil {
					return err
				}
				if len(records) != wantCount {
					t.Fatalf("history records=%d, want %d", len(records), wantCount)
				}
				for _, record := range records {
					for i, photo := range task.Files {
						if record.Filename == photo.Filename && (record.Orientation != wantDirections[i] || record.Status != "printed") {
							t.Errorf("wrong history direction/status: %+v", record)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestScheduledOrientationCreateAndEdit(t *testing.T) {
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	for _, tc := range []struct {
		field, orientation string
		valid              bool
		scheduleType       string
	}{
		{"files", "auto", true, ""}, {"files", "portrait", true, ""}, {"files", "landscape", true, ""},
		{"files", "", true, ""}, {"files", "sideways", false, ""}, {"files", "auto", false, "once"},
		{"file", "auto", false, ""}, {"file", "portrait", true, ""}, {"file", "landscape", true, ""}, {"file", "", true, ""},
	} {
		t.Run(tc.field+"/"+tc.orientation+"/"+tc.scheduleType, func(t *testing.T) {
			req := scheduledUploadRequest(t, user, tc.field, false)
			if err := req.ParseMultipartForm(32 << 20); err != nil {
				t.Fatal(err)
			}
			req.MultipartForm.Value["orientation"] = []string{tc.orientation}
			if tc.scheduleType != "" {
				req.MultipartForm.Value["schedule_type"] = []string{tc.scheduleType}
			}
			response := httptest.NewRecorder()
			scheduledCreateHandler(response, req)
			wantCode := http.StatusOK
			if !tc.valid {
				wantCode = http.StatusBadRequest
			}
			if response.Code != wantCode {
				t.Fatalf("create: %d %s", response.Code, response.Body)
			}
			if tc.scheduleType == "once" && !strings.Contains(response.Body.String(), "照片队列请选择每天、每周或每月") {
				t.Fatal("one-off photo queue bypassed validation")
			}
			if !tc.valid {
				return
			}
			var created scheduledPrintResponse
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			want := tc.orientation
			if want == "" {
				want = "portrait"
			}
			if created.Orientation != want {
				t.Fatalf("create orientation=%q, want %q", created.Orientation, want)
			}
			id := strconv.FormatInt(created.ID, 10)
			for _, orientation := range []string{"auto", "landscape", "portrait", "invalid"} {
				created.Orientation = orientation
				body, err := json.Marshal(created)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("PUT", "/api/scheduled-prints/"+id, bytes.NewReader(body))
				req = mux.SetURLVars(req, map[string]string{"id": id})
				req = req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}))
				response := httptest.NewRecorder()
				scheduledUpdateHandler(response, req)
				wantCode := http.StatusOK
				if orientation == "invalid" || (orientation == "auto" && tc.field == "file") {
					wantCode = http.StatusBadRequest
				}
				if response.Code != wantCode {
					t.Fatalf("edit %q: %d %s", orientation, response.Code, response.Body)
				}
				if wantCode == http.StatusOK && readScheduledTestTask(t, s, created.ID).Orientation != orientation {
					t.Fatal("edit lost selected orientation")
				}
			}
		})
	}
}
