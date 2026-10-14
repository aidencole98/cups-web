package ipp

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goipp "github.com/OpenPrinting/goipp"
)

func serveUnixCUPS(t *testing.T, handler http.Handler) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix Socket tests require Unix")
	}
	socketPath := filepath.Join(t.TempDir(), "cups.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	t.Cleanup(func() { server.Close() })
	go server.Serve(listener)
	t.Setenv("CUPS_SOCKET_PATH", socketPath)
	return socketPath
}

func operationString(msg *goipp.Message, name string) string {
	for _, attr := range msg.Operation {
		if attr.Name == name && len(attr.Values) > 0 {
			return attr.Values[0].V.String()
		}
	}
	return ""
}

func TestUnixSocketOperations(t *testing.T) {
	type observedRequest struct {
		operation goipp.Op
		uri       string
		path      string
		user      string
		document  string
		jobID     string
	}
	requests := make(chan observedRequest, 8)
	serveUnixCUPS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != socketHost {
			t.Errorf("HTTP Host = %q, want %q", r.Host, socketHost)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reader := bytes.NewReader(data)
		var req goipp.Message
		if err := req.Decode(reader); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		document, _ := io.ReadAll(reader)
		op := goipp.Op(req.Code)
		requests <- observedRequest{op, operationString(&req, "printer-uri"), r.URL.Path,
			operationString(&req, "requesting-user-name"), string(document), operationString(&req, "job-id")}
		resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
		switch op {
		case goipp.OpCupsGetPrinters:
			payload, err := cupsGetPrintersResponse()
			if err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", goipp.ContentType)
			w.Write(payload)
			return
		case goipp.OpGetPrinterAttributes:
			resp.Printer.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("HP_1020")))
		case goipp.OpPrintJob:
			resp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(42)))
		case goipp.OpGetJobs:
			resp.Groups = goipp.Groups{{Tag: goipp.TagJobGroup, Attrs: goipp.Attributes{
				goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(42)),
				goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(3)),
				goipp.MakeAttribute("job-printer-uri", goipp.TagURI, goipp.String("ipps://unresolvable.invalid:8631/printers/HP_1020")),
			}}}
		case goipp.OpCancelJob:
		default:
			t.Errorf("unexpected IPP operation %v", op)
		}
		payload, err := resp.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		w.Write(payload)
	}))
	// The configured socket overrides even an unusable CUPS_HOST.
	printers, err := ListPrinters("invalid TCP host")
	if err != nil || len(printers) != 2 {
		t.Fatalf("ListPrinters: %+v, %v", printers, err)
	}
	if printers[0].URI != "http://localhost:631/printers/HP_1020" {
		t.Fatalf("printer URI: %q", printers[0].URI)
	}
	info, err := GetPrinterAttributes(printers[0].URI)
	if err != nil || info.Name != "HP_1020" {
		t.Fatalf("GetPrinterAttributes: %+v, %v", info, err)
	}
	// A saved TCP URI is usable for reprints and scheduled prints after switching.
	job, err := SendPrintJob("https://old-cups.invalid/printers/HP_1020", strings.NewReader("document bytes"),
		"application/pdf", "alice", "test.pdf", PrintJobOptions{})
	if err != nil || job != "42" {
		t.Fatalf("SendPrintJob: %q, %v", job, err)
	}
	jobs, err := ListActiveJobs("invalid TCP host")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListActiveJobs: %+v, %v", jobs, err)
	}
	if jobs[0].PrinterURI != "ipp://localhost:631/printers/HP_1020" || jobs[0].PrinterName != "HP_1020" {
		t.Fatalf("job printer URI: %+v", jobs[0])
	}
	if err := CancelJob(jobs[0].PrinterURI, jobs[0].ID, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []observedRequest{
		{operation: goipp.OpCupsGetPrinters, path: "/"},
		{operation: goipp.OpGetPrinterAttributes, uri: "ipp://localhost:631/printers/HP_1020", path: "/printers/HP_1020"},
		{operation: goipp.OpPrintJob, uri: "ipp://localhost:631/printers/HP_1020", path: "/printers/HP_1020", user: "alice", document: "document bytes"},
		{operation: goipp.OpGetJobs, uri: "ipp://localhost:631/", path: "/"},
		{operation: goipp.OpCancelJob, uri: "ipp://localhost:631/printers/HP_1020", path: "/printers/HP_1020", user: "alice", jobID: "42"},
	} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("request = %+v, want %+v", got, want)
			}
		default:
			t.Fatal("missing IPP request")
		}
	}
}

func TestUnixSocketHTMLFallback(t *testing.T) {
	var posts, gets atomic.Int32
	serveUnixCUPS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		gets.Add(1)
		if r.URL.Path != "/printers" {
			t.Errorf("fallback path = %q", r.URL.Path)
		}
		io.WriteString(w, `<a href="/printers/Canon_LBP">Canon_LBP</a>`)
	}))
	printers, err := ListPrinters("")
	if err != nil || len(printers) != 1 || printers[0].URI != "http://localhost:631/printers/Canon_LBP" {
		t.Fatalf("HTML fallback: %+v, %v", printers, err)
	}
	if posts.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("posts=%d, gets=%d", posts.Load(), gets.Load())
	}
}

func TestUnixSocketFailureDoesNotUseTCP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer server.Close()
	t.Setenv("CUPS_SOCKET_PATH", filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := GetPrinterAttributes(server.URL + "/printers/test"); err == nil {
		t.Fatal("missing socket must fail")
	}
	if _, err := ListPrinters(strings.TrimPrefix(server.URL, "http://")); err == nil {
		t.Fatal("both IPP and HTML must fail when the socket is missing")
	}
	if hits.Load() != 0 {
		t.Fatal("socket failure fell back to TCP")
	}
}

func TestSocketConfigAndRequestValidation(t *testing.T) {
	for _, socketPath := range []string{"relative.sock", "unix:///run/cups/cups.sock"} {
		t.Setenv("CUPS_SOCKET_PATH", socketPath)
		if err := ValidateCUPSConfig(); err == nil {
			t.Errorf("accepted invalid config %q", socketPath)
		}
		if _, err := newSafeClient(time.Second); err == nil {
			t.Errorf("client accepted invalid config %q", socketPath)
		}
		if _, err := cupsHostPort("localhost"); err == nil {
			t.Errorf("host lookup accepted invalid config %q", socketPath)
		}
	}
	t.Setenv("CUPS_SOCKET_PATH", "/run/cups/cups.sock")
	if err := ValidateCUPSConfig(); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"unix:///other.sock", "file:///etc/passwd", "http:///printers/test",
		"http://localhost/admin", "http://localhost/printers/../admin",
		"http://localhost/printers/%2e%2e", "http://localhost/printers/test/../../admin",
		"http://localhost/printers/test?x=1", "http://localhost/printers/test#x",
		"http://user:pass@localhost/printers/test", "http://localhost/printers/%2fadmin",
	} {
		if _, err := printerRequestURI(uri); err == nil {
			t.Errorf("accepted unsafe socket request %q", uri)
		}
	}
	for _, uri := range []string{"ipp://old-cups.invalid/printers/test", "https://old-cups.invalid/classes/test", "http://169.254.169.254/printers/test"} {
		got, err := printerRequestURI(uri)
		if err != nil || !strings.HasPrefix(got, "http://localhost:631/") {
			t.Errorf("fixed socket request: %q, %v", got, err)
		}
	}
}

func TestUnixSocketDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	serveUnixCUPS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://localhost:631/admin", http.StatusFound)
	}))
	client, err := newSafeClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://localhost:631/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits.Load() != 1 {
		t.Fatalf("redirect followed: status=%d hits=%d", resp.StatusCode, hits.Load())
	}
}

func TestTCPPrinterOperations(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if want := "ipp://" + r.Host + "/printers/test"; operationString(&req, "printer-uri") != want {
			t.Errorf("TCP printer-uri = %q, want %q", operationString(&req, "printer-uri"), want)
		}
		resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
		resp.Printer.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("test")))
		resp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(7)))
		payload, err := resp.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		w.Write(payload)
	}))
	defer server.Close()
	uri := "ipp://" + strings.TrimPrefix(server.URL, "http://") + "/printers/test"
	if _, err := GetPrinterAttributes(uri); err != nil {
		t.Fatal(err)
	}
	if _, err := SendPrintJob(uri, strings.NewReader("document"), "application/pdf", "alice", "test.pdf", PrintJobOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := CancelJob(uri, 7, "alice"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("TCP operations: got %d requests", hits.Load())
	}
	if _, err := GetPrinterAttributes("http://169.254.169.254/printers/test"); err == nil {
		t.Fatal("TCP metadata address must remain blocked")
	}
}

func TestTCPPrinterURIDefaultPorts(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	for _, tc := range []struct{ uri, want string }{
		{"ipp://printer.local/printers/test", "http://printer.local:631/printers/test"},
		{"ipps://printer.local/printers/test", "https://printer.local:631/printers/test"},
		{"ipp://[::1]/printers/test", "http://[::1]:631/printers/test"},
		{"ipps://printer.local:8631/printers/test", "https://printer.local:8631/printers/test"},
		{"http://printer.local/printers/test", "http://printer.local/printers/test"},
		{"https://printer.local/printers/test", "https://printer.local/printers/test"},
	} {
		got, err := printerRequestURI(tc.uri)
		if err != nil || got != tc.want {
			t.Errorf("printerRequestURI(%q) = %q, %v; want %q", tc.uri, got, err, tc.want)
		}
	}
}
