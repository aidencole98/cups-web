package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cups-web/internal/auth"
	"cups-web/internal/ipp"
	"cups-web/internal/store"
	"github.com/gorilla/mux"
)

func scheduledTestStore(t *testing.T) (*store.Store, store.User) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var user store.User
	err = s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		var err error
		user, err = store.CreateUser(context.Background(), tx, store.CreateUserInput{
			Username: "test", PasswordHash: "unused", Role: store.RoleUser,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, user
}

func insertScheduledTestTask(t *testing.T, s *store.Store, user store.User) store.ScheduledPrint {
	t.Helper()
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.Local)
	task := store.ScheduledPrint{
		UserID: user.ID, Username: user.Username, PrinterURI: "ipp://localhost/printers/test",
		Filename: "01.png", StoredPath: "scheduled/01.png", ScheduleType: store.ScheduleDaily,
		ScheduleTime: "09:00", Enabled: true, Copies: 1, PaperSize: "6inch", PaperType: "photo",
		Files: []store.ScheduledFile{
			{Filename: "01.png", StoredPath: "scheduled/01.png"},
			{Filename: "02.png", StoredPath: "scheduled/02.png"},
		},
		NextRunAt: now.UTC().Format(time.RFC3339), CreatedAt: now.UTC().Format(time.RFC3339), UpdatedAt: now.UTC().Format(time.RFC3339),
	}
	err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		var err error
		task.ID, err = store.InsertScheduledPrint(context.Background(), tx, &task)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func readScheduledTestTask(t *testing.T, s *store.Store, id int64) store.ScheduledPrint {
	t.Helper()
	var task store.ScheduledPrint
	err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		var err error
		task, err = store.GetScheduledPrintByID(context.Background(), tx, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestScheduledQueueSequenceAndRestart(t *testing.T) {
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	for i, filename := range []string{"01.png", "02.png"} {
		now, _ := time.Parse(time.RFC3339, task.NextRunAt)
		now = now.In(time.Local)
		err := executeScheduledOccurrence(s, "", task, now, false, func(_ *store.Store, _ string, selected store.ScheduledPrint) (string, error) {
			if selected.Filename != filename || selected.StoredPath != task.Files[i].StoredPath {
				t.Fatalf("wrong selection: %+v", selected)
			}
			claimed := readScheduledTestTask(t, s, task.ID)
			if claimed.Enabled || claimed.LastStatus != "running" {
				t.Fatalf("missing durable claim: %+v", claimed)
			}
			return "printed", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		task = readScheduledTestTask(t, s, task.ID)
		if task.NextFileIndex != i+1 || task.SubmittedCount != i+1 {
			t.Fatalf("wrong progress: %+v", task)
		}
		if task.Enabled != (i == 0) {
			t.Fatalf("wrong enabled: %+v", task)
		}
	}
	if !scheduledQueueComplete(task) || len(task.History) != 2 || task.History[0].Filename != "02.png" {
		t.Fatalf("wrong completion: %+v", task)
	}
	var databasePath string
	if err := s.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&databasePath); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored := readScheduledTestTask(t, reopened, task.ID)
	if restored.SubmittedCount != 2 || !scheduledQueueComplete(restored) || restored.Enabled || len(restored.Files) != 2 {
		t.Fatalf("lost progress: %+v", restored)
	}
}

func TestScheduledQueueBatchProgress(t *testing.T) {
	for _, mode := range []string{"remaining", "loop", "partial-failure", "uncertain", "interrupted", "save-failed", "manual-paused"} {
		t.Run(mode, func(t *testing.T) {
			s, user := scheduledTestStore(t)
			task := insertScheduledTestTask(t, s, user)
			task.PhotosPerRun = 3
			if mode == "loop" {
				task.Loop = true
				task.NextFileIndex = 1
			}
			if mode == "manual-paused" {
				task.Enabled = false
			}
			if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error { return store.UpdateScheduledPrint(context.Background(), tx, &task) }); err != nil {
				t.Fatal(err)
			}
			now, _ := time.Parse(time.RFC3339, task.NextRunAt)
			calls := 0
			var execErr error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && mode != "interrupted" {
						panic(recovered)
					}
				}()
				execErr = executeScheduledOccurrence(s, "", task, now, mode == "manual-paused", func(_ *store.Store, _ string, selected store.ScheduledPrint) (string, error) {
					calls++
					claimed := readScheduledTestTask(t, s, task.ID)
					if claimed.Enabled || claimed.LastStatus != "running" || claimed.SubmittedCount != calls-1 {
						t.Fatalf("progress not durably claimed: %+v", claimed)
					}
					want := []string{"01.png", "02.png"}[(calls-1)%2]
					if mode == "loop" {
						want = []string{"02.png", "01.png"}[(calls-1)%2]
					}
					if selected.Filename != want {
						t.Fatalf("selected=%s want=%s", selected.Filename, want)
					}
					if calls == 2 && mode == "partial-failure" {
						return "failed", errors.New("offline")
					}
					if calls == 2 && mode == "uncertain" {
						return "running", &ipp.SubmissionUncertainError{Err: errors.New("response lost after upload")}
					}
					if calls == 2 && mode == "interrupted" {
						panic("process stopped")
					}
					if calls == 1 && mode == "save-failed" {
						_, err := s.DB.Exec(`CREATE TRIGGER fail_progress BEFORE UPDATE OF submitted_count ON scheduled_prints WHEN NEW.submitted_count > OLD.submitted_count BEGIN SELECT RAISE(FAIL, 'disk error'); END`)
						if err != nil {
							t.Fatal(err)
						}
					}
					return "printed", nil
				})
			}()
			actual := readScheduledTestTask(t, s, task.ID)
			switch mode {
			case "remaining", "manual-paused":
				if execErr != nil || calls != 2 || actual.SubmittedCount != 2 || actual.Enabled || !scheduledQueueComplete(actual) {
					t.Fatalf("bad completion: %+v %v calls=%d", actual, execErr, calls)
				}
			case "loop":
				if execErr != nil || calls != 3 || actual.SubmittedCount != 3 || actual.NextFileIndex != 0 || !actual.Enabled {
					t.Fatalf("bad loop: %+v %v", actual, execErr)
				}
			case "partial-failure":
				if execErr == nil || calls != 2 || actual.SubmittedCount != 1 || actual.NextFileIndex != 1 || !actual.Enabled || actual.LastStatus != "failed" {
					t.Fatalf("lost accepted photo: %+v %v", actual, execErr)
				}
			case "interrupted":
				if calls != 2 || actual.SubmittedCount != 1 || actual.NextFileIndex != 1 || actual.Enabled || actual.LastStatus != "running" {
					t.Fatalf("unsafe interruption: %+v", actual)
				}
			case "uncertain":
				if execErr == nil || calls != 2 || actual.SubmittedCount != 1 || actual.NextFileIndex != 1 || actual.Enabled || actual.LastStatus != "running" || actual.History[0].Status != "running" {
					t.Fatalf("uncertain result could auto-replay: %+v %v", actual, execErr)
				}
			case "save-failed":
				if execErr == nil || calls != 1 || actual.SubmittedCount != 0 || actual.Enabled || actual.LastStatus != "running" {
					t.Fatalf("continued after persistence failure: %+v %v", actual, execErr)
				}
			}
			next, _ := time.Parse(time.RFC3339, actual.NextRunAt)
			if next.Sub(now) != 24*time.Hour {
				t.Fatalf("batch consumed multiple schedule points: %s", actual.NextRunAt)
			}
		})
	}
}

func TestScheduledQueueFailureSkipLoopAndManual(t *testing.T) {
	for _, mode := range []string{"failed", "skipped", "loop", "manual-paused"} {
		t.Run(mode, func(t *testing.T) {
			s, user := scheduledTestStore(t)
			task := insertScheduledTestTask(t, s, user)
			now, _ := time.Parse(time.RFC3339, task.NextRunAt)
			now = now.In(time.Local)
			manual := false
			if mode == "skipped" {
				now = now.Add(5 * time.Minute)
			}
			if mode == "loop" {
				task.Loop = true
				task.NextFileIndex = 1
				task.SubmittedCount = 1
			}
			if mode == "manual-paused" {
				manual = true
				task.Enabled = false
				now = now.Add(-time.Hour)
			}
			if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
				return store.UpdateScheduledPrint(context.Background(), tx, &task)
			}); err != nil {
				t.Fatal(err)
			}
			called := false
			err := executeScheduledOccurrence(s, "", task, now, manual, func(_ *store.Store, _ string, _ store.ScheduledPrint) (string, error) {
				called = true
				if mode == "failed" {
					return "failed", errors.New("printer offline")
				}
				return "printed", nil
			})
			if (err != nil) != (mode == "failed") {
				t.Fatalf("err=%v", err)
			}
			actual := readScheduledTestTask(t, s, task.ID)
			switch mode {
			case "failed", "skipped":
				if actual.NextFileIndex != 0 || actual.SubmittedCount != 0 || actual.LastStatus != mode || !actual.Enabled {
					t.Fatalf("consumed failed/skipped photo: %+v", actual)
				}
				if called != (mode != "skipped") {
					t.Fatalf("called=%v", called)
				}
			case "loop":
				if actual.NextFileIndex != 0 || actual.SubmittedCount != 2 || !actual.Enabled {
					t.Fatalf("wrong loop: %+v", actual)
				}
			case "manual-paused":
				next, _ := time.Parse(time.RFC3339, actual.NextRunAt)
				if actual.Enabled || actual.NextFileIndex != 1 || next.In(time.Local).Day() != 16 {
					t.Fatalf("wrong manual: %+v", actual)
				}
			}
		})
	}
}

func TestScheduledInterruptedSubmissionStaysPaused(t *testing.T) {
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	now, _ := time.Parse(time.RFC3339, task.NextRunAt)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated interruption")
			}
		}()
		_ = executeScheduledOccurrence(s, "", task, now, false, func(_ *store.Store, _ string, _ store.ScheduledPrint) (string, error) {
			panic("simulate process interruption after printer accepted")
		})
	}()
	current := readScheduledTestTask(t, s, task.ID)
	if current.Enabled || current.LastStatus != "running" || current.NextFileIndex != 0 {
		t.Fatalf("interrupted submission could auto-replay: %+v", current)
	}
	var due []store.ScheduledPrint
	if err := s.WithTx(context.Background(), true, func(tx *sql.Tx) error {
		var err error
		due, err = store.ListDueScheduledPrints(context.Background(), tx, now.AddDate(0, 0, 2).UTC().Format(time.RFC3339))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("interrupted task was automatically selected")
	}
}

func TestScheduledCompletedQueueResumePolicy(t *testing.T) {
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	task.NextFileIndex, task.SubmittedCount, task.Enabled = len(task.Files), len(task.Files), false
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.SaveScheduledExecution(context.Background(), tx, &task)
	}); err != nil {
		t.Fatal(err)
	}
	enabled, loop := true, true
	payload := scheduledUpdatePayload{
		Name: "queue", PrinterURI: task.PrinterURI, ScheduleType: store.ScheduleDaily,
		ScheduleTime: "09:00", Copies: 1, Enabled: &enabled,
	}
	current := task
	applyScheduledPayload(&current, payload)
	if err := ensureScheduleDefaults(&current); err == nil {
		t.Fatal("completed queue could resume without looping")
	}
	payload.Loop = &loop
	current = task
	applyScheduledPayload(&current, payload)
	if err := ensureScheduleDefaults(&current); err != nil {
		t.Fatal(err)
	}
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.UpdateScheduledPrint(context.Background(), tx, &current)
	}); err != nil {
		t.Fatal(err)
	}
	restored := readScheduledTestTask(t, s, task.ID)
	if !restored.Loop || !restored.Enabled || restored.NextFileIndex != 0 || restored.SubmittedCount != 2 {
		t.Fatalf("wrong resumed queue: %+v", restored)
	}
}

func TestScheduledUpdatePreservesAdvancedOccurrence(t *testing.T) {
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	previousStore := appStore
	appStore = s
	t.Cleanup(func() { appStore = previousStore })
	now := time.Now().In(time.Local)
	task.StartDate = now.Format("2006-01-02")
	task.NextRunAt = time.Date(now.Year(), now.Month(), now.Day()+8, 9, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	if err := s.WithTx(context.Background(), false, func(tx *sql.Tx) error {
		return store.UpdateScheduledPrint(context.Background(), tx, &task)
	}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true, true} {
		payload := scheduledUpdatePayload{
			Name: "updated title", PrinterURI: task.PrinterURI, Enabled: &enabled,
			Copies: 1, ScheduleType: store.ScheduleDaily, ScheduleTime: "09:00",
			ScheduleWeekday: 1, ScheduleDay: 1, // 每天任务不受隐藏的周/月参数影响。
			// 未提供 startDate 的旧客户端不会清空新字段。
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("PUT", "/api/scheduled-prints", bytes.NewReader(body))
		req = mux.SetURLVars(req, map[string]string{"id": strconv.FormatInt(task.ID, 10)})
		req = req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}))
		resp := httptest.NewRecorder()
		scheduledUpdateHandler(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("update: %d %s", resp.Code, resp.Body)
		}
		current := readScheduledTestTask(t, s, task.ID)
		if current.NextRunAt != task.NextRunAt || current.StartDate != task.StartDate || current.Enabled != enabled {
			t.Fatalf("advanced occurrence lost: %+v", current)
		}
	}
	paused := task
	paused.Enabled = false
	paused.NextRunAt = now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	resumed := paused
	resumed.Enabled = true
	if err := refreshScheduledNextRun(&resumed, paused, now); err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339, resumed.NextRunAt)
	if err != nil || !next.After(now) {
		t.Fatalf("expired resume: %s %v", resumed.NextRunAt, err)
	}
	changed := task
	changed.ScheduleTime = "10:00"
	if err := refreshScheduledNextRun(&changed, task, now); err != nil {
		t.Fatal(err)
	}
	next, err = time.Parse(time.RFC3339, changed.NextRunAt)
	if err != nil || next.In(time.Local).Hour() != 10 || changed.NextRunAt == task.NextRunAt {
		t.Fatalf("schedule edit was not applied: %s %v", changed.NextRunAt, err)
	}
}

func TestScheduledStartDateAndLegacyMigration(t *testing.T) {
	for _, typ := range []string{store.ScheduleDaily, store.ScheduleWeekly, store.ScheduleMonthly} {
		task := store.ScheduledPrint{ScheduleType: typ, ScheduleTime: "00:00", ScheduleWeekday: 4, ScheduleDay: 15, StartDate: "2026-10-15"}
		before := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
		next, ok := computeNextRun(task, before)
		want := time.Date(2026, 10, 15, 0, 0, 0, 0, time.Local)
		if !ok || !next.Equal(want) {
			t.Fatalf("%s next=%v want=%v", typ, next, want)
		}
	}
	s, user := scheduledTestStore(t)
	task := insertScheduledTestTask(t, s, user)
	for _, column := range []string{"files_json", "history_json", "next_file_index", "submitted_count", "loop", "start_date", "driver_options_json", "photos_per_run"} {
		if _, err := s.DB.Exec("ALTER TABLE scheduled_prints DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	var path string
	if err := s.DB.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	legacy := readScheduledTestTask(t, reopened, task.ID)
	if len(legacy.Files) != 0 || scheduledQueueComplete(legacy) || !legacy.Enabled || legacy.PhotosPerRun != 1 || !legacy.DriverOptions.Empty() {
		t.Fatalf("legacy changed: %+v", legacy)
	}
	now, _ := time.Parse(time.RFC3339, legacy.NextRunAt)
	if err := executeScheduledOccurrence(reopened, "", legacy, now, false, func(_ *store.Store, _ string, selected store.ScheduledPrint) (string, error) {
		if selected.Filename != task.Filename {
			t.Fatal("legacy file changed")
		}
		return "printed", nil
	}); err != nil {
		t.Fatal(err)
	}
	if !readScheduledTestTask(t, reopened, task.ID).Enabled {
		t.Fatal("legacy daily task stopped")
	}
}

func scheduledUploadRequest(t *testing.T, user store.User, field string, invalid bool) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("printer", "ipp://localhost/printers/test")
	writer.WriteField("schedule_type", "daily")
	writer.WriteField("schedule_time", "09:00")
	for _, name := range []string{"02.png", "01.png"} {
		part, err := writer.CreateFormFile(field, name)
		if err != nil {
			t.Fatal(err)
		}
		if invalid && name == "01.png" {
			part.Write([]byte("not an image"))
		} else {
			if err := png.Encode(part, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
				t.Fatal(err)
			}
		}
		if field == "file" {
			break
		}
	}
	writer.Close()
	req := httptest.NewRequest("POST", "/api/scheduled-prints", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: user.ID, Username: user.Username, Role: user.Role}))
}

func TestScheduledUploadAccessBusyAndCleanup(t *testing.T) {
	s, user := scheduledTestStore(t)
	previousStore, previousDir := appStore, uploadDir
	appStore, uploadDir = s, t.TempDir()
	t.Cleanup(func() { appStore, uploadDir = previousStore, previousDir })
	bad := httptest.NewRecorder()
	scheduledCreateHandler(bad, scheduledUploadRequest(t, user, "files", true))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad upload: %d %s", bad.Code, bad.Body)
	}
	var remaining int
	filepath.WalkDir(uploadDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			remaining++
		}
		return nil
	})
	if remaining != 0 {
		t.Fatalf("rollback left %d files", remaining)
	}
	resp := httptest.NewRecorder()
	scheduledCreateHandler(resp, scheduledUploadRequest(t, user, "files", false))
	if resp.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", resp.Code, resp.Body)
	}
	var created scheduledPrintResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	task := readScheduledTestTask(t, s, created.ID)
	if len(task.Files) != 2 || task.Files[0].Filename != "02.png" {
		t.Fatal("upload order lost")
	}
	id := strconv.FormatInt(task.ID, 10)
	request := func(method string, ownerID int64) *http.Request {
		req := httptest.NewRequest(method, "/api/scheduled-prints/"+id, nil)
		req = mux.SetURLVars(req, map[string]string{"id": id, "index": "0"})
		return req.WithContext(auth.WithSession(req.Context(), auth.Session{UserID: ownerID, Username: user.Username, Role: user.Role}))
	}
	denied := httptest.NewRecorder()
	scheduledThumbnailHandler(denied, request("GET", user.ID+1))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("other owner: %d", denied.Code)
	}
	thumb := httptest.NewRecorder()
	scheduledThumbnailHandler(thumb, request("GET", user.ID))
	if thumb.Code != http.StatusOK || thumb.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumbnail: %d %s", thumb.Code, thumb.Body)
	}
	if !acquireScheduledTask(task.ID, true) {
		t.Fatal("couldn't acquire task")
	}
	if acquireScheduledTask(task.ID, true) {
		t.Fatal("duplicate task acquired")
	}
	busy := httptest.NewRecorder()
	scheduledRunNowHandler(busy, request("POST", user.ID))
	if busy.Code != http.StatusConflict {
		t.Fatalf("duplicate run: %d", busy.Code)
	}
	busy = httptest.NewRecorder()
	scheduledDeleteHandler(busy, request("DELETE", user.ID))
	if busy.Code != http.StatusConflict {
		t.Fatalf("delete while running: %d", busy.Code)
	}
	releaseScheduledTask(task.ID)
	deleted := httptest.NewRecorder()
	scheduledDeleteHandler(deleted, request("DELETE", user.ID))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	for _, file := range task.Files {
		for _, path := range []string{file.StoredPath, file.ThumbnailPath} {
			if _, err := os.Stat(filepath.Join(uploadDir, path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("not deleted: %s (%v)", path, err)
			}
		}
	}
	legacyResp := httptest.NewRecorder()
	scheduledCreateHandler(legacyResp, scheduledUploadRequest(t, user, "file", false))
	if legacyResp.Code != http.StatusOK {
		t.Fatalf("legacy upload: %d %s", legacyResp.Code, legacyResp.Body)
	}
	var legacy scheduledPrintResponse
	json.Unmarshal(legacyResp.Body.Bytes(), &legacy)
	if len(legacy.Files) != 0 {
		t.Fatal("legacy upload became queue")
	}
}
