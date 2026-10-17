package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
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

func testPhotoTIFF(order binary.ByteOrder, date string) []byte {
	b := make([]byte, 64)
	copy(b, "II")
	if order == binary.BigEndian {
		copy(b, "MM")
	}
	order.PutUint16(b[2:], 42)
	order.PutUint32(b[4:], 8)
	order.PutUint16(b[8:], 1)
	order.PutUint16(b[10:], 0x8769)
	order.PutUint16(b[12:], 4)
	order.PutUint32(b[14:], 1)
	order.PutUint32(b[18:], 26)
	order.PutUint16(b[26:], 1)
	order.PutUint16(b[28:], 0x9003)
	order.PutUint16(b[30:], 2)
	order.PutUint32(b[32:], 20)
	order.PutUint32(b[36:], 44)
	copy(b[44:], date)
	return b
}

func testPhotoJPEG(data, tiff []byte) []byte {
	segment := append([]byte("Exif\x00\x00"), tiff...)
	b := []byte{0xff, 0xd8, 0xff, 0xe1, byte((len(segment) + 2) >> 8), byte(len(segment) + 2)}
	b = append(b, segment...)
	if len(data) > 2 {
		b = append(b, data[2:]...)
	} else {
		b = append(b, 0xff, 0xd9)
	}
	return b
}

func testPhotoPNG(data, tiff []byte) []byte {
	chunk := make([]byte, 12+len(tiff))
	binary.BigEndian.PutUint32(chunk, uint32(len(tiff)))
	copy(chunk[4:], "eXIf")
	copy(chunk[8:], tiff)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	// After IHDR, before IDAT.
	b := append([]byte{}, data[:33]...)
	b = append(b, chunk...)
	return append(b, data[33:]...)
}

func TestPhotoDateEXIF(t *testing.T) {
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, date := range []string{"2026:10:03 23:59:59", "2024:02:29 00:00:00", "2026:02:29 12:00:00", "0000:00:00 00:00:00", "2026:10:03 24:00:00", "2026:10:03 00:00:60"} {
			tiff := testPhotoTIFF(order, date)
			want := ""
			if strings.HasPrefix(date, "2026:10:03 23") {
				want = "2026-10-03"
			}
			if strings.HasPrefix(date, "2024") {
				want = "2024-02-29"
			}
			for _, data := range [][]byte{testPhotoJPEG(nil, tiff), testPhotoPNG(pngBytes.Bytes(), tiff)} {
				if got := readPhotoDate(bytes.NewReader(data)); got != want {
					t.Fatalf("%s %s got=%q want=%q", order, date, got, want)
				}
				// Every truncated container must be safe to parse.
				for n := 0; n < len(data); n++ {
					readPhotoDate(bytes.NewReader(data[:n]))
				}
			}
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[4:], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[18:], 0xfffffff0) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[26:], 65535) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[36:], 0xffffffff) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[28:], 0x0132) }, // edit date only
		func(b []byte) { b[63] = 'x' },
	} {
		b := testPhotoTIFF(binary.LittleEndian, "2026:10:03 00:00:00")
		mutate(b)
		if got := photoDateFromTIFF(b); got != "" {
			t.Fatalf("malformed EXIF gave %s", got)
		}
	}
}

func photoPDFText(t *testing.T, path string) ([]rscpdf.Text, string) {
	t.Helper()
	doc, err := rscpdf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	text := doc.Page(1).Content().Text
	var s strings.Builder
	rscpdf.Interpret(doc.Page(1).V.Key("Contents"), func(stack *rscpdf.Stack, op string) {
		args := make([]rscpdf.Value, stack.Len())
		for i := len(args) - 1; i >= 0; i-- {
			args[i] = stack.Pop()
		}
		if op == "Tj" && len(args) == 1 {
			s.WriteString(args[0].TextFromUTF16())
		}
	})
	return text, s.String()
}

func TestPhotoDatePDFLayout(t *testing.T) {
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 120, 60)), nil); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(input, testPhotoJPEG(jpegBytes.Bytes(), testPhotoTIFF(binary.LittleEndian, "2026:10:03 12:00:00")), 0600); err != nil {
		t.Fatal(err)
	}
	for _, scaling := range []string{"fit", "fill"} {
		for _, borderless := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				path, cleanup, err := convertImageToPDF(input, "portrait", "6inch", true, true, imagePDFOptions{Borderless: borderless, PrintScaling: scaling, PhotoDateWatermark: enabled})
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				text, s := photoPDFText(t, path)
				if strings.Contains(s, "2026年10月3日") != enabled {
					t.Fatalf("enabled=%v PDF text=%q", enabled, s)
				}
				if !enabled {
					continue
				}
				// A 2:1 photo on portrait paper: fit anchors in the image, fill in the slot.
				margin := 10.0
				if borderless {
					margin = 0
				}
				w, h := 102.0-2*margin, 152.0-2*margin
				bottom := margin + h
				if scaling == "fit" {
					bottom = margin + (h+w/2)/2
					h = w / 2
				}
				short := math.Min(w, h)
				wantY := (152 - (bottom - short*0.04)) * 72 / 25.4
				// Last layer is the foreground date, with all glyphs on its baseline.
				if math.Abs(text[len(text)-1].Y-wantY) > 0.03 {
					t.Fatalf("%s borderless=%v baseline=%f want=%f", scaling, borderless, text[len(text)-1].Y, wantY)
				}
			}
		}
	}
}

func TestPhotoDateConversionMultipart(t *testing.T) {
	testStore, _ := scheduledTestStore(t)
	previous := appStore
	appStore = testStore
	t.Cleanup(func() { appStore = previous })
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 20, 30))); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i < 2; i++ {
		part, _ := writer.CreateFormFile("files", "photo.png")
		part.Write(pngBytes.Bytes())
	}
	writer.WriteField("photo_date_watermark", "true")
	writer.WriteField("photo_dates", `["2026-10-03","2024-02-29"]`)
	writer.WriteField("per_page", "2")
	writer.WriteField("rotations", "90,270")
	writer.WriteField("print_scaling", "fill")
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/convert", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res := httptest.NewRecorder()
	convertHandler(res, req)
	if res.Code != 200 {
		t.Fatalf("convert: %d %s", res.Code, res.Body.String())
	}
	path := filepath.Join(t.TempDir(), "out.pdf")
	os.WriteFile(path, res.Body.Bytes(), 0600)
	_, s := photoPDFText(t, path)
	for _, want := range []string{"2026年10月3日", "2024年2月29日"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %q", want, s)
		}
	}
	for _, raw := range []string{`null`, `["2026-02-29"]`, `["2026-1-01"]`, `["2026-01-01","2025-01-01"]`, `[null]`, `["0000-01-01"]`} {
		if _, err := parsePhotoDates(raw, 1); err == nil {
			t.Errorf("accepted invalid dates: %s", raw)
		}
	}
}

func TestPhotoDatePersistence(t *testing.T) {
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	task.PhotoDateWatermark = true
	ctx := context.Background()
	if err := s.WithTx(ctx, false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(ctx, tx, &task) }); err != nil {
		t.Fatal(err)
	}
	if !readScheduledTestTask(t, s, task.ID).PhotoDateWatermark {
		t.Fatal("lost task flag")
	}
	// Old clients omitting the new option retain the stored value.
	applyScheduledPayload(&task, scheduledUpdatePayload{})
	if !task.PhotoDateWatermark {
		t.Fatal("omitted update cleared date flag")
	}
	off := false
	applyScheduledPayload(&task, scheduledUpdatePayload{PhotoDateWatermark: &off})
	if task.PhotoDateWatermark {
		t.Fatal("explicit disable ignored")
	}
	if err := s.WithTx(ctx, false, func(tx *sql.Tx) error {
		rec := store.PrintRecord{UserID: user.ID, Filename: "photo.jpg", StoredPath: "photo.jpg", PhotoDateWatermark: true}
		id, err := store.InsertPrintRecord(ctx, tx, &rec)
		if err != nil {
			return err
		}
		got, err := store.GetPrintRecordByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if !got.PhotoDateWatermark || !mapPrintRecords([]store.PrintRecord{got})[0].PhotoDateWatermark {
			t.Fatal("lost history flag")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate an old database and run migrations twice on the same data.
	var seq int
	var dbName, dbPath string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"print_jobs", "scheduled_prints"} {
		if _, err := s.DB.Exec("ALTER TABLE " + table + " DROP COLUMN photo_date_watermark"); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for i := 0; i < 2; i++ {
		reopened, err := store.Open(ctx, dbPath)
		if err != nil {
			t.Fatal(err)
		}
		got := readScheduledTestTask(t, reopened, task.ID)
		if got.PhotoDateWatermark || len(got.Files) != 2 {
			t.Fatal("old task migration changed data/defaults")
		}
		if err := reopened.WithTx(ctx, true, func(tx *sql.Tx) error {
			records, err := store.ListPrintRecords(ctx, tx, store.PrintFilter{Username: user.Username})
			if err == nil && (len(records) != 1 || records[0].PhotoDateWatermark) {
				t.Fatal("old history migration changed data/defaults")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		reopened.Close()
	}
}

// Exercise submitted PDF bytes and persisted flags through all three print paths.
func TestPhotoDatePrintingPipelines(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 30, 20)), nil); err != nil {
		t.Fatal(err)
	}
	photo := testPhotoJPEG(jpegBytes.Bytes(), testPhotoTIFF(binary.LittleEndian, "2026:10:03 12:00:00"))
	expected := []string{"2026年10月3日", "", "2026年10月3日", "2026年10月3日", "2026年10月3日", "2026年10月3日", "2024年2月29日"}
	submissions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message goipp.Message
		if err := message.Decode(r.Body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if message.Code == goipp.Code(goipp.OpPrintJob) {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			path := filepath.Join(t.TempDir(), "submitted.pdf")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Error(err)
				return
			}
			_, text := photoPDFText(t, path)
			if submissions >= len(expected) {
				t.Error("unexpected extra print")
				return
			}
			want := expected[submissions]
			if want == "" {
				if text != "" {
					t.Errorf("disabled date: %q", text)
				}
			} else if strings.Count(text, want) != 2 {
				t.Errorf("submission %d date text=%q, want one foreground and shadow of %s", submissions, text, want)
			}
			submissions++
		}
		response := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, message.RequestID)
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
	printer := server.URL + "/printers/photo"
	session := auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}
	submit := func(filename string, data []byte) int64 {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data)
		for key, value := range map[string]string{"printer": printer, "photo_date_watermark": "true", "paper_size": "6inch", "print_scaling": "fill"} {
			writer.WriteField(key, value)
		}
		writer.Close()
		req := httptest.NewRequest("POST", "/api/print", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req = req.WithContext(auth.WithSession(req.Context(), session))
		res := httptest.NewRecorder()
		printHandler(res, req)
		if res.Code != 200 {
			t.Fatalf("print: %d %s", res.Code, res.Body.String())
		}
		var id int64
		if err := s.DB.QueryRow("SELECT MAX(id) FROM print_jobs").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	reprint := func(id int64, enabled bool) {
		t.Helper()
		body, err := json.Marshal(reprintRequest{Printer: printer, PhotoDateWatermark: enabled, PaperSize: "6inch", PrintScaling: "fill", Copies: 1})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/api/print-records/1/reprint", bytes.NewReader(body))
		req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(id, 10)})
		req = req.WithContext(auth.WithSession(req.Context(), session))
		res := httptest.NewRecorder()
		reprintHandler(res, req)
		if res.Code != 200 {
			t.Fatalf("reprint: %d %s", res.Code, res.Body.String())
		}
	}
	first := submit("original.jpg", photo)
	reprint(first, false)
	reprint(first, true)
	originalPath := filepath.Join(t.TempDir(), "original.jpg")
	os.WriteFile(originalPath, photo, 0600)
	pdfPath, cleanup, err := convertImageToPDF(originalPath, "portrait", "6inch", false, false, imagePDFOptions{PhotoDateWatermark: true})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pdfData, err := os.ReadFile(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	baked := submit("baked.pdf", pdfData)
	reprint(baked, true)
	task := insertScheduledTestTask(t, s, user)
	task.PrinterURI, task.PhotoDateWatermark, task.PhotosPerRun = printer, true, 2
	task.Files = []store.ScheduledFile{{Filename: "one.jpg", StoredPath: "one.jpg"}, {Filename: "two.jpg", StoredPath: "two.jpg"}}
	if err := os.WriteFile(filepath.Join(uploadDir, "one.jpg"), photo, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadDir, "two.jpg"), testPhotoJPEG(jpegBytes.Bytes(), testPhotoTIFF(binary.BigEndian, "2024:02:29 23:00:00")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) }); err != nil {
		t.Fatal(err)
	}
	if err := executeScheduledOccurrence(s, uploadDir, task, time.Now(), true, executeScheduled); err != nil {
		t.Fatal(err)
	}
	if submissions != len(expected) {
		t.Fatalf("submissions=%d", submissions)
	}
	if readScheduledTestTask(t, s, task.ID).SubmittedCount != 2 {
		t.Fatal("date processing lost queue progress")
	}
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		records, err := store.ListPrintRecords(context.Background(), tx, store.PrintFilter{Username: user.Username})
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.PhotoDateWatermark != (record.ID != first+1) {
				t.Errorf("history flag: %+v", record)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
