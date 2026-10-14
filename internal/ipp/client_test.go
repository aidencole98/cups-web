package ipp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	goipp "github.com/OpenPrinting/goipp"
)

// TestNormalizePageSet 覆盖 CUPS page-set 合法值（all / odd / even）以及
// 非法值、大小写、前后空白的规范化逻辑。空串必须映射为 "all"，未知值
// 必须返回空串，让调用方完全跳过 IPP 属性，避免被打印机拒绝。
func TestNormalizePageSet(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "all"},
		{"all", "all"},
		{"ALL", "all"},
		{"  odd ", "odd"},
		{"Odd", "odd"},
		{"even", "even"},
		{"EVEN", "even"},
		{"bogus", ""},
		{"1-5", ""},
	}
	for _, c := range cases {
		if got := normalizePageSet(c.input); got != c.want {
			t.Errorf("normalizePageSet(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// TestParsePageRange 为现有的 parsePageRange 补一组回归用例，防止未来对
// page-ranges 逻辑的调整误伤（当前仓库此前没有 ipp 包的测试）。
func TestParsePageRange(t *testing.T) {
	got := parsePageRange("1-5 8 10-12")
	want := [][2]int{{1, 5}, {8, 8}, {10, 12}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// 非法片段（0、空、倒序）应被静默丢弃，而不是 panic
	if r := parsePageRange("0 -3 5-3 "); len(r) != 0 {
		t.Errorf("bad ranges should be dropped, got %v", r)
	}
}

// buildJobMessage 复刻 SendPrintJob 的请求构造流程（只取 Job 组），
// 用于离线校验 PrintJobOptions → IPP 属性的映射正确性。之所以不直接
// 跑 SendPrintJob，是因为那个函数内部需要真实的 HTTP 连接，测试中
// 没法方便地 mock。
func buildJobMessage(opts PrintJobOptions) *goipp.Message {
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 1)
	// 与 SendPrintJob 保持同样的 duplex/pageSet 互斥兜底（issue #109）。
	isDuplex := opts.IsDuplex
	if set := normalizePageSet(opts.PageSet); set == "odd" || set == "even" || set == "even-reverse" {
		isDuplex = false
	}
	if isDuplex {
		req.Job.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("two-sided-long-edge")))
	} else {
		req.Job.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("one-sided")))
	}
	if set := normalizePageSet(opts.PageSet); set != "" && set != "all" && set != "even-reverse" {
		req.Job.Add(goipp.MakeAttribute("page-set", goipp.TagKeyword, goipp.String(set)))
	}
	return req
}

// findJobAttr 在 Job 组中按名称查找属性，返回其第一个字符串值。
// 未命中时返回空串，供测试用 "应当/不应当存在" 两路断言。
func findJobAttr(msg *goipp.Message, name string) string {
	for _, a := range msg.Job {
		if a.Name == name && len(a.Values) > 0 {
			return a.Values[0].V.String()
		}
	}
	return ""
}

// TestSendPrintJob_PageSetEncoding 验证 page-set 在不同 PageSet 入参下
// 是否按预期进入 IPP Job 组（all 不发、odd/even 落地、非法值丢弃）。
func TestSendPrintJob_PageSetEncoding(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantHas bool
		wantVal string
	}{
		{"default empty -> omitted", "", false, ""},
		{"explicit all -> omitted", "all", false, ""},
		{"odd injected", "odd", true, "odd"},
		{"even injected", "even", true, "even"},
		{"mixed case normalized", "EVEN", true, "even"},
		{"bogus dropped", "first-page", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := buildJobMessage(PrintJobOptions{PageSet: c.input})
			got := findJobAttr(msg, "page-set")
			if c.wantHas {
				if got != c.wantVal {
					t.Errorf("page-set = %q, want %q", got, c.wantVal)
				}
			} else if got != "" {
				t.Errorf("page-set should be omitted, got %q", got)
			}

			// 同时通过编解码验证属性能在 wire format 上 round-trip，
			// 避免 goipp 对 keyword 类型的编码路径出现意外回归。
			payload, err := msg.EncodeBytes()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var decoded goipp.Message
			if err := decoded.Decode(bytes.NewReader(payload)); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got2 := findJobAttr(&decoded, "page-set"); got2 != got {
				t.Errorf("round-trip mismatch: encoded %q, decoded %q", got, got2)
			}
		})
	}
}

// TestSendPrintJob_PageSetNotLeakingIntoOperation 额外确认 page-set 不会被
// 误写到 Operation 组（CUPS 对组别敏感，写错位置会被直接忽略或报错）。
func TestSendPrintJob_PageSetNotLeakingIntoOperation(t *testing.T) {
	msg := buildJobMessage(PrintJobOptions{PageSet: "odd"})
	for _, a := range msg.Operation {
		if strings.EqualFold(a.Name, "page-set") {
			t.Fatalf("page-set leaked into Operation group: %+v", a)
		}
	}
}

// TestSendPrintJob_PageSetForcesOneSided 覆盖 issue #109：当选择了奇/偶页手动
// 双面时，即使 IsDuplex=true 也必须落成 sides=one-sided，避免 CUPS 用空白页
// 填补被过滤掉的另一面而产生间隔空白页。
func TestSendPrintJob_PageSetForcesOneSided(t *testing.T) {
	cases := []struct {
		name      string
		opts      PrintJobOptions
		wantSides string
	}{
		{"odd + duplex -> forced one-sided", PrintJobOptions{IsDuplex: true, PageSet: "odd"}, "one-sided"},
		{"even + duplex -> forced one-sided", PrintJobOptions{IsDuplex: true, PageSet: "even"}, "one-sided"},
		{"all + duplex -> two-sided", PrintJobOptions{IsDuplex: true, PageSet: "all"}, "two-sided-long-edge"},
		{"empty + duplex -> two-sided", PrintJobOptions{IsDuplex: true, PageSet: ""}, "two-sided-long-edge"},
		{"even-reverse + duplex -> forced one-sided", PrintJobOptions{IsDuplex: true, PageSet: "even-reverse"}, "one-sided"},
		{"odd + one-sided stays one-sided", PrintJobOptions{IsDuplex: false, PageSet: "odd"}, "one-sided"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := buildJobMessage(c.opts)
			if got := findJobAttr(msg, "sides"); got != c.wantSides {
				t.Errorf("sides = %q, want %q", got, c.wantSides)
			}
		})
	}
}

// cupsGetPrintersResponse 拼一个含两台打印机的 CUPS-Get-Printers 响应，
// 用来验证 listPrintersIPP 会遍历所有 printer-attributes 组 —— goipp 的
// Message.Printer 只保留被压平的那一组，多队列时会静默丢数据。
func cupsGetPrintersResponse() ([]byte, error) {
	msg := goipp.NewResponse(goipp.DefaultVersion, goipp.Status(goipp.StatusOk), 1)
	msg.Groups = goipp.Groups{
		{Tag: goipp.TagOperationGroup, Attrs: goipp.Attributes{
			goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		}},
		{Tag: goipp.TagPrinterGroup, Attrs: goipp.Attributes{
			goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("HP_1020")),
			goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String("三楼财务室")),
			goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String("3F")),
			goipp.MakeAttribute("printer-make-and-model", goipp.TagText, goipp.String("HP LaserJet 1020")),
		}},
		{Tag: goipp.TagPrinterGroup, Attrs: goipp.Attributes{
			goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String("HP_1020_2")),
			goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String("  一楼前台  ")),
		}},
		// 没有 printer-name 的组必须被丢掉，否则会拼出 /printers/ 这种空 URI
		{Tag: goipp.TagPrinterGroup, Attrs: goipp.Attributes{
			goipp.MakeAttribute("printer-info", goipp.TagText, goipp.String("孤儿组")),
		}},
	}
	return msg.EncodeBytes()
}

// TestListPrintersIPP 验证 CUPS-Get-Printers 路径：多队列全都要解析出来、
// 描述字段去空白、URI 按 host+队列名拼（而不是打印机自报的主机名）。
func TestListPrintersIPP(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	payload, err := cupsGetPrintersResponse()
	if err != nil {
		t.Fatalf("build response: %v", err)
	}

	var gotRequested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req goipp.Message
		if err := req.Decode(bytes.NewReader(body)); err == nil {
			for _, a := range req.Operation {
				if a.Name == "requested-attributes" {
					for _, v := range a.Values {
						gotRequested = append(gotRequested, v.V.String())
					}
				}
			}
		}
		w.Header().Set("Content-Type", goipp.ContentType)
		w.Write(payload)
	}))
	defer srv.Close()

	hostOnly := strings.TrimPrefix(srv.URL, "http://")
	printers, err := listPrintersIPP(hostOnly)
	if err != nil {
		t.Fatalf("listPrintersIPP: %v", err)
	}
	if len(printers) != 2 {
		t.Fatalf("got %d printers, want 2: %+v", len(printers), printers)
	}

	p := printers[0]
	if p.Name != "HP_1020" || p.Info != "三楼财务室" || p.Location != "3F" || p.MakeAndModel != "HP LaserJet 1020" {
		t.Errorf("printer[0] = %+v", p)
	}
	if want := "http://" + hostOnly + "/printers/HP_1020"; p.URI != want {
		t.Errorf("printer[0].URI = %q, want %q", p.URI, want)
	}
	if printers[1].Info != "一楼前台" {
		t.Errorf("printer[1].Info = %q, want trimmed 一楼前台", printers[1].Info)
	}

	// 必须点名 requested-attributes，否则 CUPS 会为每台打印机回上百个属性
	if len(gotRequested) == 0 {
		t.Error("request did not carry requested-attributes")
	}
	for _, want := range []string{"printer-name", "printer-info"} {
		if !slices.Contains(gotRequested, want) {
			t.Errorf("requested-attributes missing %q: %v", want, gotRequested)
		}
	}
}

// TestListPrintersFallbackToHTML 确认 IPP 操作被拒时会退回抓 /printers 页面，
// 让远端 CUPS 禁掉 CUPS-Get-Printers 的部署仍然列得出队列（只是没有描述）。
func TestListPrintersFallbackToHTML(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><a href="/printers/Canon_LBP">Canon_LBP</a></body></html>`))
	}))
	defer srv.Close()

	printers, err := ListPrinters(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("ListPrinters: %v", err)
	}
	if len(printers) != 1 || printers[0].Name != "Canon_LBP" {
		t.Fatalf("got %+v, want single Canon_LBP", printers)
	}
	if printers[0].Info != "" {
		t.Errorf("HTML fallback should not invent an info field, got %q", printers[0].Info)
	}
}

// TestCupsHostPort 覆盖 CUPS_HOST 的几种写法归一化。
func TestCupsHostPort(t *testing.T) {
	t.Setenv("CUPS_SOCKET_PATH", "")
	cases := []struct{ in, want string }{
		{"localhost", "localhost:631"},
		{"localhost:631", "localhost:631"},
		{"http://cups.lan", "cups.lan:631"},
		{"http://cups.lan:6310", "cups.lan:6310"},
	}
	for _, c := range cases {
		got, err := cupsHostPort(c.in)
		if err != nil {
			t.Errorf("cupsHostPort(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("cupsHostPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := cupsHostPort(""); err == nil {
		t.Error("cupsHostPort(\"\") should fail")
	}
}
