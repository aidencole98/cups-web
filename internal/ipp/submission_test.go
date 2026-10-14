package ipp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

func TestPrintSubmissionOutcome(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	for _, scenario := range []struct {
		name          string
		status        goipp.Status
		httpStatus    int
		broken        bool
		disconnect    bool
		wantSuccess   bool
		wantUncertain bool
	}{
		{name: "success", status: goipp.StatusOk, wantSuccess: true},
		{name: "substituted", status: goipp.Status(1), wantSuccess: true},
		{name: "conflicting", status: goipp.Status(2), wantSuccess: true},
		{name: "ipp-rejected", status: goipp.StatusErrorBadRequest},
		{name: "http-rejected", httpStatus: http.StatusForbidden},
		{name: "gateway-error", httpStatus: http.StatusBadGateway, wantUncertain: true},
		{name: "response-corrupt", broken: true, wantUncertain: true},
		{name: "response-lost", disconnect: true, wantUncertain: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var received atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.ReadAll(r.Body); err != nil {
					t.Error(err)
					return
				}
				received.Store(true)
				if scenario.disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if scenario.httpStatus != 0 {
					w.WriteHeader(scenario.httpStatus)
					return
				}
				if scenario.broken {
					io.WriteString(w, "incomplete IPP response")
					return
				}
				response := goipp.NewResponse(goipp.DefaultVersion, scenario.status, 1)
				response.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(122)))
				data, err := response.EncodeBytes()
				if err != nil {
					t.Error(err)
					return
				}
				w.Write(data)
			}))
			defer server.Close()
			job, err := SendPrintJob(server.URL+"/printers/photo", strings.NewReader("document bytes"), "application/pdf", "test", "photo.pdf", PrintJobOptions{})
			if !received.Load() || (err == nil) != scenario.wantSuccess || IsSubmissionUncertain(err) != scenario.wantUncertain {
				t.Fatalf("received=%v job=%q err=%v uncertain=%v", received.Load(), job, err, IsSubmissionUncertain(err))
			}
			if scenario.wantSuccess && job != "122" {
				t.Fatalf("lost accepted job: %q", job)
			}
		})
	}
}

func TestPrintDialFailureIsDefinite(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") }))
	uri := server.URL + "/printers/photo"
	server.Close()
	_, err := SendPrintJob(uri, strings.NewReader("PDF"), "application/pdf", "", "", PrintJobOptions{})
	if err == nil || IsSubmissionUncertain(err) {
		t.Fatalf("dial failure should not claim a possible submission: %v", err)
	}
}
