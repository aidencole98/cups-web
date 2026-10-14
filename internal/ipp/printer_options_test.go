package ipp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

const photoDriverPPD = `*PPD-Adobe: "4.3"
*OpenUI *PageSize/Media Size: PickOne
*DefaultPageSize: A4
*PageSize A4/A4: "<</PageSize[595.28 841.89]>>setpagedevice"
*PageSize Photo/4 x 6 in: "code"
*PageSize Photo.Borderless/4 x 6 in Borderless: "code"
*PaperDimension A4: "595.28 841.89"
*PaperDimension Photo: "288 432"
*PaperDimension Photo.Borderless: "288 432"
*ImageableArea A4: "10 10 585.28 831.89"
*ImageableArea Photo: "10 10 278 422"
*ImageableArea Photo.Borderless: "0 0 288 432"
*zh_CN.PageSize Photo.Borderless/102 x 152 毫米（无边距）: ""
*OpenUI *MediaType/Media Type: PickOne
*DefaultMediaType: Plain
*MediaType Plain/Plain Paper: "code"
*MediaType EpsonGlossy/Epson Premium Glossy: "code"
*zh_CN.MediaType EpsonGlossy/爱普生超级光泽纸: ""
*zh.MediaType Plain/普通纸: ""
*zh.MediaType EpsonGlossy/光泽纸: ""
*OpenUI *Ink/Print Quality: PickOne
*DefaultInk: Normal
*Ink Normal/Normal: "code"
*Ink Photo/High: "code"
*OpenUI *Resolution/Resolution: PickOne
*Resolution 600dpi/600 dpi: "code"
*OpenUI *Brightness/Brightness: PickOne
*Brightness 100/100: "code"
`

func TestPhotoDriverChoicesAndValidation(t *testing.T) {
	opts := parsePrinterPPD([]byte(photoDriverPPD))
	if len(opts.PaperSizes) != 3 || len(opts.MediaTypes) != 2 || len(opts.Qualities) != 2 {
		t.Fatalf("missing driver choices: %+v", opts)
	}
	page := opts.PaperSizes[2]
	if !page.Borderless || page.WidthMM != 101.6 || page.HeightMM != 152.4 || !strings.Contains(page.Label, "无边距") {
		t.Fatalf("wrong page: %+v", page)
	}
	if opts.MediaTypes[1].Label != "爱普生超级光泽纸" || opts.Qualities[1].Attribute != "Ink" {
		t.Fatalf("lost vendor choices: %+v", opts)
	}
	if opts.Defaults.PageSize.Value != "A4" || opts.Defaults.MediaType.Value != "Plain" || opts.Defaults.Quality.Value != "Normal" {
		t.Fatalf("missing defaults: %+v", opts.Defaults)
	}
	forged := page
	forged.WidthMM = 999
	resolved, err := opts.Resolve(DriverOptions{PageSize: &forged})
	if err != nil || resolved.PageSize.WidthMM != 101.6 {
		t.Fatalf("trusted client dimensions: %+v %v", resolved, err)
	}
	if _, err := opts.Resolve(DriverOptions{Quality: &PrinterChoice{Attribute: "Brightness", Value: "100"}}); err == nil {
		t.Fatal("accepted unrelated driver option")
	}
	if _, err := opts.Resolve(DriverOptions{PageSize: &PrinterChoice{Attribute: "PageSize", Value: "Nonexistent"}}); err == nil {
		t.Fatal("accepted unknown choice")
	}
}

func TestPPDFullBleedImageableArea(t *testing.T) {
	for _, area := range []struct {
		bounds     string
		borderless bool
	}{
		{"0 0 288 432", true},
		{"-8.4 -8.4 296.4 440.4", true},
		{"8 0 288 432", false},
		{"0 0 280 432", false},
	} {
		ppd := "*PPD-Adobe: \"4.3\"\n*PageSize Photo/Photo: \"code\"\n*PaperDimension Photo: \"288 432\"\n*ImageableArea Photo: \"" + area.bounds + "\"\n"
		opts := parsePrinterPPD([]byte(ppd))
		if len(opts.PaperSizes) != 1 || opts.PaperSizes[0].Borderless != area.borderless {
			t.Fatalf("bounds=%q choices=%+v", area.bounds, opts.PaperSizes)
		}
	}
}

func TestPrinterOptionsTransportAndCache(t *testing.T) {
	for _, transport := range []string{"tcp", "socket"} {
		t.Run(transport, func(t *testing.T) {
			calls := 0
			var submitted goipp.Message
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if r.URL.Path != "/printers/Photo.ppd" {
						t.Errorf("PPD path=%s", r.URL.Path)
					}
					calls++
					io.WriteString(w, photoDriverPPD)
					return
				}
				if err := submitted.Decode(r.Body); err != nil {
					t.Error(err)
				}
				rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
				rsp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(122)))
				payload, _ := rsp.EncodeBytes()
				w.Header().Set("Content-Type", goipp.ContentType)
				w.Write(payload)
			})
			uri := ""
			if transport == "tcp" {
				t.Setenv("CUPS_SOCKET_PATH", "")
				server := httptest.NewServer(handler)
				defer server.Close()
				uri = server.URL + "/printers/Photo"
			} else {
				serveUnixCUPS(t, handler)
				uri = "ipp://old-cups.invalid/printers/Photo"
			}
			opts, err := GetPrinterOptions(context.Background(), uri, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := GetPrinterOptions(context.Background(), uri, false); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("not cached: calls=%d", calls)
			}
			if _, err := GetPrinterOptions(context.Background(), uri, true); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("force did not refresh: calls=%d", calls)
			}
			selected := DriverOptions{PageSize: &opts.PaperSizes[2], MediaType: &opts.MediaTypes[1], Quality: &opts.Qualities[1]}
			job, err := SendPrintJob(uri, strings.NewReader("PDF"), "application/pdf", "test", "photo.pdf", PrintJobOptions{PaperSize: "A4", PaperType: "plain", DriverOptions: selected})
			if err != nil || job != "122" {
				t.Fatalf("print: %s %v", job, err)
			}
			if findJobAttr(&submitted, "PageSize") != "Photo.Borderless" || findJobAttr(&submitted, "MediaType") != "EpsonGlossy" || findJobAttr(&submitted, "Ink") != "Photo" {
				t.Fatalf("lost driver attributes: %+v", submitted.Job)
			}
			if findJobAttr(&submitted, "media") != "" || findJobAttr(&submitted, "media-type") != "" {
				t.Fatal("legacy attributes conflict with driver choices")
			}
		})
	}
}

func testMediaCol(margin int) goipp.Collection {
	col := goipp.Collection{goipp.MakeAttribute("media-size", goipp.TagBeginCollection, goipp.Collection{
		goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(10160)),
		goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(15240)),
	})}
	for _, name := range []string{"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin"} {
		col = append(col, goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(margin)))
	}
	return col
}

func TestIPPOptionsFallbackAndCollectionEncoding(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	var submitted goipp.Message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req goipp.Message
		if err := req.Decode(r.Body); err != nil {
			t.Error(err)
			return
		}
		rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.StatusOk, 1)
		if goipp.Op(req.Code) == goipp.OpGetPrinterAttributes {
			requested := operationString(&req, "requested-attributes")
			found := false
			for _, a := range req.Operation {
				if a.Name == "requested-attributes" {
					for _, v := range a.Values {
						found = found || v.V.String() == "media-col-database"
					}
				}
			}
			if !found {
				t.Errorf("missing explicit media-col-database request: %s", requested)
			}
			var cols goipp.Values
			cols.Add(goipp.TagBeginCollection, testMediaCol(300))
			cols.Add(goipp.TagBeginCollection, testMediaCol(0))
			cols.Add(goipp.TagBeginCollection, testMediaCol(0))
			rsp.Printer.Add(goipp.Attribute{Name: "media-col-database", Values: cols})
			rsp.Printer.Add(goipp.MakeAttribute("media-col-default", goipp.TagBeginCollection, testMediaCol(300)))
			rsp.Printer.Add(goipp.MakeAttribute("media-type-supported", goipp.TagKeyword, goipp.String("photographic-glossy")))
			rsp.Printer.Add(goipp.MakeAttribute("print-quality-supported", goipp.TagEnum, goipp.Integer(5)))
		} else {
			submitted = req
			rsp.Job.Add(goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(1)))
		}
		payload, _ := rsp.EncodeBytes()
		w.Header().Set("Content-Type", goipp.ContentType)
		w.Write(payload)
	}))
	defer server.Close()
	uri := server.URL + "/printers/driverless"
	opts, err := GetPrinterOptions(context.Background(), uri, false)
	if err != nil || len(opts.PaperSizes) != 2 || len(opts.Qualities) != 1 {
		t.Fatalf("IPP fallback: %+v %v", opts, err)
	}
	if opts.PaperSizes[0].Borderless || !opts.PaperSizes[1].Borderless || opts.Defaults.PageSize == nil || opts.Defaults.PageSize.Borderless {
		t.Fatalf("margins: %+v", opts)
	}
	_, err = SendPrintJob(uri, bytes.NewReader(nil), "application/pdf", "", "", PrintJobOptions{DriverOptions: DriverOptions{PageSize: &opts.PaperSizes[1], MediaType: &opts.MediaTypes[0], Quality: &opts.Qualities[0]}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range submitted.Job {
		if a.Name == "print-quality" && a.Values[0].T != goipp.TagEnum {
			t.Fatal("quality is not enum")
		}
		if a.Name == "media-col" {
			c := choiceFromMediaCol(a.Values[0].V.(goipp.Collection))
			if c == nil || !c.Borderless || c.WidthMM != 101.6 {
				t.Fatalf("bad collection: %+v", c)
			}
		}
	}
	if findJobAttr(&submitted, "print-quality") != "5" || findJobAttr(&submitted, "media-col") == "" {
		t.Fatal("missing quality/media-col")
	}
}

func TestExplicitPaperDimensions(t *testing.T) {
	for _, size := range []string{"custom:0x152", "custom:NaNx152", "custom:100xInf", "custom:2001x152", "custom:100x152x200"} {
		if _, _, ok := ParsePaperDimensions(size); ok {
			t.Errorf("accepted %s", size)
		}
	}
	if w, h, ok := ParsePaperDimensions("custom:101.6x152.4"); !ok || w != 101.6 || h != 152.4 {
		t.Fatal("lost physical dimensions")
	}
}

func TestPrinterOptionsRejectsIPPRedirect(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rsp := goipp.NewResponse(goipp.DefaultVersion, goipp.Status(0x0300), 1)
		data, err := rsp.EncodeBytes()
		if err != nil {
			t.Error(err)
			return
		}
		w.Write(data)
	}))
	defer server.Close()
	if _, err := GetPrinterOptions(context.Background(), server.URL+"/printers/redirect", false); err == nil {
		t.Fatal("treated IPP redirect as an empty successful capability response")
	}
}
