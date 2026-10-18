package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cups-web/internal/auth"
	"cups-web/internal/store"
	goipp "github.com/OpenPrinting/goipp"
	"github.com/gorilla/mux"
	rscpdf "rsc.io/pdf"
)

func originalPhotoJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3100, 40)), &jpeg.Options{Quality: 96}); err != nil {
		t.Fatal(err)
	}
	return testPhotoJPEG(b.Bytes(), testPhotoTIFF(binary.LittleEndian, "2026:10:03 12:00:00"))
}

func TestPhotoOriginalJPEGAndRotation(t *testing.T) {
	original := originalPhotoJPEG(t)
	input := filepath.Join(t.TempDir(), "original.jpg")
	if err := os.WriteFile(input, original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, rotation := range []int{0, 90, 180, 270} {
		t.Run(strconv.Itoa(rotation), func(t *testing.T) {
			path, cleanup, err := convertImagePathsToPDF([]string{input}, "portrait", "6inch", []int{rotation}, false, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, original) {
				t.Fatal("JPEG bytes changed when compression was disabled")
			}
			doc, err := rscpdf.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			images := doc.Page(1).Resources().Key("XObject")
			if len(images.Keys()) != 1 {
				t.Fatal("missing image")
			}
			img := images.Key(images.Keys()[0])
			if img.Key("Width").Int64() != 3100 || img.Key("Height").Int64() != 40 {
				t.Fatal("original pixels were resized or rotated")
			}
			// The rotation matrix's signs distinguish clockwise from anticlockwise.
			if rotation != 0 {
				found := false
				rscpdf.Interpret(doc.Page(1).V.Key("Contents"), func(stack *rscpdf.Stack, op string) {
					args := make([]rscpdf.Value, stack.Len())
					for i := len(args) - 1; i >= 0; i-- {
						args[i] = stack.Pop()
					}
					if op == "cm" && len(args) == 6 {
						a, b, c, d := args[0].Float64(), args[1].Float64(), args[2].Float64(), args[3].Float64()
						if (rotation == 90 && b < 0 && c > 0) || (rotation == 180 && a < 0 && d < 0) || (rotation == 270 && b > 0 && c < 0) {
							found = true
						}
					}
				})
				if !found {
					t.Fatal("incorrect PDF rotation matrix")
				}
			}
		})
	}
	path, cleanup, err := convertImageToPDF(input, "portrait", "6inch", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, original) {
		t.Fatal("enabled compression did not apply the pixel limit")
	}
}

func TestPhotoCustomPaperOrientation(t *testing.T) {
	input := writeCompressionTestImage(t, 200, 100)
	for _, paper := range []string{"custom:102x152", "custom:152x102"} {
		for _, orientation := range []string{"portrait", "landscape"} {
			t.Run(paper+"/"+orientation, func(t *testing.T) {
				path, cleanup, err := convertImageToPDF(input, orientation, paper, false, false, imagePDFOptions{PrintScaling: "fill"})
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				doc, err := rscpdf.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					t.Fatal(err)
				}
				w, h := 102.0, 152.0
				if orientation == "landscape" {
					w, h = h, w
				}
				box := photoPageAttribute(doc.Page(1).V, "MediaBox")
				if math.Abs(box.Index(2).Float64()-w*72/25.4) > 0.02 || math.Abs(box.Index(3).Float64()-h*72/25.4) > 0.02 {
					t.Fatalf("page size=%v, want %.0fx%.0fmm", box, w, h)
				}
				if scaling := photoPrintScaling(path, "fill", "application/pdf", orientation, paper); scaling != "none" {
					t.Fatalf("photo layout would be filled twice: %q", scaling)
				}
			})
		}
	}
}

func TestPhotoSourcesPrintReprintDownloadAndCleanup(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	original := originalPhotoJPEG(t)
	var submitted [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			t.Error(err)
			return
		}
		if req.Code == goipp.Code(goipp.OpPrintJob) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			submitted = append(submitted, data)
		}
		response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, req.RequestID)
		response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(len(submitted))))
		data, err := response.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	session := auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range []string{"one.jpg", "two.jpg"} {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(original)
	}
	for k, v := range map[string]string{"printer": server.URL + "/printers/photo", "paper_size": "6inch", "orientation": "portrait", "rotations": "0,90", "per_page": "2", "photo_date_watermark": "true", "print_scaling": "fill"} {
		writer.WriteField(k, v)
	}
	writer.Close()
	requestBytes := append([]byte(nil), body.Bytes()...)
	req := httptest.NewRequest("POST", "/api/print", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req = req.WithContext(auth.WithSession(req.Context(), session))
	res := httptest.NewRecorder()
	printHandler(res, req)
	if res.Code != 200 {
		t.Fatalf("print: %d %s", res.Code, res.Body.String())
	}
	getRecord := func(id int64) store.PrintRecord {
		t.Helper()
		var record store.PrintRecord
		if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
			var err error
			record, err = store.GetPrintRecordByID(context.Background(), tx, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := getRecord(1)
	if len(first.PhotoLayout.Sources) != 2 || first.PhotoLayout.PerPage != 2 || first.PhotoLayout.Sources[1].Rotation != 90 || first.RenderedPath == "" {
		t.Fatalf("lost source snapshot: %+v", first)
	}
	rendered, err := os.ReadFile(filepath.Join(uploadDir, first.RenderedPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(submitted) != 1 || !bytes.Equal(rendered, submitted[0]) || !bytes.Contains(rendered, original) {
		t.Fatal("stored print PDF differs from submitted bytes/original")
	}
	for _, source := range first.PhotoLayout.Sources {
		data, err := os.ReadFile(filepath.Join(uploadDir, source.StoredPath))
		if err != nil || !bytes.Equal(data, original) {
			t.Fatal("archived original changed", err)
		}
	}
	download := func(query string, who auth.Session, want int) []byte {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/print-records/1/file"+query, nil)
		r = mux.SetURLVars(r, map[string]string{"id": "1"})
		r = r.WithContext(auth.WithSession(r.Context(), who))
		w := httptest.NewRecorder()
		printRecordFileHandler(w, r)
		if w.Code != want {
			t.Fatalf("download %s: %d %s", query, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	if !bytes.Equal(download("?source=1", session, 200), original) || !bytes.Equal(download("?variant=rendered", session, 200), rendered) {
		t.Fatal("download mismatch")
	}
	download("?source=-1", session, 400)
	download("?source=2", session, 400)
	download("?source=0", auth.Session{UserID: user.ID + 1, Role: store.RoleUser}, 403)
	// Disable dates and change paper, while preserving the original ordering,
	// quarter-turn and two-per-page layout through the public reprint handler.
	payload, _ := json.Marshal(reprintRequest{Printer: server.URL + "/printers/photo", Orientation: "landscape", PaperSize: "A4", PrintScaling: "fill", PhotoDateWatermark: false})
	r := httptest.NewRequest("POST", "/api/print-records/1/reprint", bytes.NewReader(payload))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	r = r.WithContext(auth.WithSession(r.Context(), session))
	w := httptest.NewRecorder()
	reprintHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("reprint: %d %s", w.Code, w.Body.String())
	}
	second := getRecord(2)
	if second.PhotoDateWatermark || second.PhotoLayout.PerPage != 2 || second.PhotoLayout.Sources[1].Rotation != 90 {
		t.Fatal("reprint snapshot lost")
	}
	if second.PhotoLayout.Sources[0].StoredPath == first.PhotoLayout.Sources[0].StoredPath {
		t.Fatal("records must own independent originals")
	}
	_, date := photoPDFText(t, filepath.Join(uploadDir, second.RenderedPath))
	if date != "" {
		t.Fatal("old PDF date survived regeneration")
	}
	// Expire only the first record; the reprint must remain downloadable and usable.
	old := time.Now().AddDate(0, 0, -10).UTC().Format(time.RFC3339)
	if _, err := s.DB.Exec("UPDATE print_jobs SET created_at=? WHERE id=1", old); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.SetSettingInt(context.Background(), tx, store.SettingRetentionDays, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := cleanupOldPrints(context.Background(), s, uploadDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range printRecordPaths(first) {
		if path != "" {
			if _, err := os.Stat(filepath.Join(uploadDir, path)); !os.IsNotExist(err) {
				t.Errorf("expired asset remains: %s", path)
			}
		}
	}
	for _, path := range append(photoSourcePaths(second.PhotoLayout), second.RenderedPath) {
		if _, err := os.Stat(filepath.Join(uploadDir, path)); err != nil {
			t.Fatal("reprint asset removed", err)
		}
	}
	if n, err := cleanupAllPrints(context.Background(), s, uploadDir); err != nil || n != 1 {
		t.Fatalf("cleanup count=%d err=%v", n, err)
	}
	for _, path := range printRecordPaths(second) {
		if path != "" {
			if _, err := os.Stat(filepath.Join(uploadDir, path)); !os.IsNotExist(err) {
				t.Errorf("asset remains: %s", path)
			}
		}
	}
	// Disabling history must clean every original and generated PDF, including
	// the additional sources of a multi-image request.
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.SetSettingInt(context.Background(), tx, store.SettingSaveHistory, 0)
	}); err != nil {
		t.Fatal(err)
	}
	noHistory := httptest.NewRequest("POST", "/api/print", bytes.NewReader(requestBytes))
	noHistory.Header.Set("Content-Type", writer.FormDataContentType())
	noHistory = noHistory.WithContext(auth.WithSession(noHistory.Context(), session))
	noHistoryResponse := httptest.NewRecorder()
	printHandler(noHistoryResponse, noHistory)
	if noHistoryResponse.Code != 200 {
		t.Fatalf("no history: %d %s", noHistoryResponse.Code, noHistoryResponse.Body.String())
	}
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM print_jobs").Scan(&count); err != nil || count != 0 {
		t.Fatal("history disabled but record persisted", err)
	}
	if err := filepath.WalkDir(uploadDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("history disabled but asset remains: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Scheduled history also owns an original: deleting the scheduled source
	// must not destroy the new historical record's ability to reprint it.
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.SetSettingInt(context.Background(), tx, store.SettingSaveHistory, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(uploadDir, "scheduled"), 0755); err != nil {
		t.Fatal(err)
	}
	shared := "scheduled/source.jpg"
	if err := os.WriteFile(filepath.Join(uploadDir, shared), original, 0600); err != nil {
		t.Fatal(err)
	}
	task := store.ScheduledPrint{UserID: user.ID, Username: user.Username, PrinterURI: server.URL + "/printers/photo", Filename: "source.jpg", StoredPath: shared, PaperSize: "custom:152x102", Orientation: "landscape", Copies: 1, PrintScaling: "fill"}
	if status, err := executeScheduled(s, uploadDir, task); err != nil || status != "printed" {
		t.Fatalf("scheduled archive: %s %v", status, err)
	}
	var scheduledID int64
	if err := s.DB.QueryRow("SELECT MAX(id) FROM print_jobs").Scan(&scheduledID); err != nil {
		t.Fatal(err)
	}
	scheduled := getRecord(scheduledID)
	if err := os.Remove(filepath.Join(uploadDir, shared)); err != nil {
		t.Fatal(err)
	}
	if len(scheduled.PhotoLayout.Sources) != 1 {
		t.Fatal("scheduled history lost source")
	}
	archived, err := os.ReadFile(filepath.Join(uploadDir, scheduled.PhotoLayout.Sources[0].StoredPath))
	if err != nil || !bytes.Equal(archived, original) {
		t.Fatal("scheduled archive depends on task source", err)
	}
	// Reprint after the task's source is gone, retaining a wide driver paper
	// and its selected direction. Both PDFs must have identical page geometry.
	payload, _ = json.Marshal(reprintRequest{Printer: task.PrinterURI, PaperSize: task.PaperSize, Orientation: task.Orientation, PrintScaling: "fill"})
	r = httptest.NewRequest("POST", "/api/print-records/"+strconv.FormatInt(scheduledID, 10)+"/reprint", bytes.NewReader(payload))
	r = mux.SetURLVars(r, map[string]string{"id": strconv.FormatInt(scheduledID, 10)})
	r = r.WithContext(auth.WithSession(r.Context(), session))
	w = httptest.NewRecorder()
	reprintHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("scheduled reprint: %d %s", w.Code, w.Body.String())
	}
	for _, data := range submitted[len(submitted)-2:] {
		doc, err := rscpdf.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		box := photoPageAttribute(doc.Page(1).V, "MediaBox")
		if math.Abs(box.Index(2).Float64()-152*72/25.4) > 0.02 || math.Abs(box.Index(3).Float64()-102*72/25.4) > 0.02 {
			t.Fatalf("scheduled/reprinted direction mismatch: %v", box)
		}
	}
	if n, err := cleanupAllPrints(context.Background(), s, uploadDir); err != nil || n != 2 {
		t.Fatalf("scheduled cleanup: %d %v", n, err)
	}
	// Old shared scheduled references remain protected during history cleanup.
	if err := os.WriteFile(filepath.Join(uploadDir, shared), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		_, err := store.InsertPrintRecord(context.Background(), tx, &store.PrintRecord{UserID: user.ID, Filename: "source.jpg", StoredPath: shared})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := cleanupAllPrints(context.Background(), s, uploadDir); err != nil || n != 1 {
		t.Fatalf("legacy scheduled cleanup: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(uploadDir, shared)); err != nil {
		t.Fatal("shared scheduled source deleted", err)
	}
}

func TestPhotoAssetMigration(t *testing.T) {
	s, user := scheduledTestStore(t)
	ctx := context.Background()
	if err := s.WithTx(ctx, false, func(tx *sql.Tx) error {
		_, err := store.InsertPrintRecord(ctx, tx, &store.PrintRecord{UserID: user.ID, Filename: "old.pdf", StoredPath: "old.pdf"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"photo_layout_json", "rendered_path"} {
		if _, err := s.DB.Exec("ALTER TABLE print_jobs DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for i := 0; i < 2; i++ {
		reopened, err := store.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		err = reopened.WithTx(ctx, true, func(tx *sql.Tx) error {
			rec, err := store.GetPrintRecordByID(ctx, tx, 1)
			if err == nil && (rec.Filename != "old.pdf" || len(rec.PhotoLayout.Sources) != 0 || rec.RenderedPath != "") {
				t.Fatal("legacy record changed")
			}
			return err
		})
		reopened.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPhotoRejectedUploadRollsBackAssets(t *testing.T) {
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	session := auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}
	req := photoUploadRequest(t, "/api/print", "files", originalPhotoJPEG(t), 2, map[string]string{"printer": "http://localhost:631/printers/photo", "rotations": "0,45"})
	req = req.WithContext(auth.WithSession(req.Context(), session))
	w := httptest.NewRecorder()
	printHandler(w, req)
	if w.Code != 400 {
		t.Fatalf("invalid rotation: %d", w.Code)
	}
	filepath.WalkDir(uploadDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("rejected upload left asset: %s", path)
		}
		return err
	})
	// Metadata must never disclose internal paths through the public list.
	resp := mapPrintRecords([]store.PrintRecord{{PhotoLayout: store.PhotoLayout{Sources: []store.PhotoSource{{Filename: "one.jpg", StoredPath: "private/path"}}}}})
	data, _ := json.Marshal(resp)
	if strings.Contains(string(data), "private/path") {
		t.Fatal("source path leaked")
	}
}
