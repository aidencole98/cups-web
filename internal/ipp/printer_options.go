package ipp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	goipp "github.com/OpenPrinting/goipp"
)

// PrinterChoice retains the driver's keyword, choice and physical page dimensions.
// Margins are IPP units (1/100 mm), ordered bottom, left, right, top.
type PrinterChoice struct {
	Attribute  string  `json:"attribute"`
	Value      string  `json:"value"`
	Label      string  `json:"label"`
	WidthMM    float64 `json:"widthMM,omitempty"`
	HeightMM   float64 `json:"heightMM,omitempty"`
	Borderless bool    `json:"borderless,omitempty"`
	Margins    *[4]int `json:"margins,omitempty"`
}

type DriverOptions struct {
	PageSize  *PrinterChoice `json:"pageSize,omitempty"`
	MediaType *PrinterChoice `json:"mediaType,omitempty"`
	Quality   *PrinterChoice `json:"quality,omitempty"`
}

func (o DriverOptions) Empty() bool {
	return o.PageSize == nil && o.MediaType == nil && o.Quality == nil
}

func (o DriverOptions) SameChoices(other DriverOptions) bool {
	for _, pair := range [][2]*PrinterChoice{{o.PageSize, other.PageSize}, {o.MediaType, other.MediaType}, {o.Quality, other.Quality}} {
		if pair[0] == nil || pair[1] == nil {
			if pair[0] != pair[1] {
				return false
			}
			continue
		}
		if pair[0].Attribute != pair[1].Attribute || pair[0].Value != pair[1].Value {
			return false
		}
	}
	return true
}

// PaperSize uses the same explicit dimensions for preview, conversion and composition.
func (o DriverOptions) PaperSize(fallback string) string {
	if p := o.PageSize; p != nil && validDimensions(p.WidthMM, p.HeightMM) {
		return fmt.Sprintf("custom:%.4fx%.4f", p.WidthMM, p.HeightMM)
	}
	return fallback
}

func validDimensions(w, h float64) bool {
	return !math.IsNaN(w) && !math.IsNaN(h) && w > 0 && h > 0 && w <= 2000 && h <= 2000
}

func ParsePaperDimensions(size string) (float64, float64, bool) {
	if !strings.HasPrefix(size, "custom:") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(size, "custom:"), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, e1 := strconv.ParseFloat(parts[0], 64)
	h, e2 := strconv.ParseFloat(parts[1], 64)
	return w, h, e1 == nil && e2 == nil && validDimensions(w, h)
}

type PrinterOptions struct {
	PaperSizes []PrinterChoice `json:"paperSizes"`
	MediaTypes []PrinterChoice `json:"mediaTypes"`
	Qualities  []PrinterChoice `json:"qualities"`
	Defaults   DriverOptions   `json:"defaults"`
	Source     string          `json:"source"`
}

type optionsCacheEntry struct {
	options PrinterOptions
	expires time.Time
}

var printerOptionsCache = struct {
	sync.Mutex
	entries map[string]optionsCacheEntry
}{entries: make(map[string]optionsCacheEntry)}

func GetPrinterOptions(ctx context.Context, rawURI string, force bool) (PrinterOptions, error) {
	uri, err := printerRequestURI(rawURI)
	if err != nil {
		return PrinterOptions{}, err
	}
	// Include deployment configuration: changing a socket must not reuse another server's data.
	key := uri + "\x00" + configuredSocketPath()
	if !force {
		printerOptionsCache.Lock()
		cached, ok := printerOptionsCache.entries[key]
		printerOptionsCache.Unlock()
		if ok && time.Now().Before(cached.expires) {
			return cached.options, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	opts := PrinterOptions{PaperSizes: []PrinterChoice{}, MediaTypes: []PrinterChoice{}, Qualities: []PrinterChoice{}, Source: "ipp"}
	// Fetch the queue's PPD over its existing safe transport, also for web-only/socket deployments.
	u, _ := url.Parse(uri)
	parts := strings.Split(u.Path, "/")
	if len(parts) == 3 && parts[1] == "printers" && parts[2] != "" {
		u.Path += ".ppd"
		u.RawPath = ""
		if data, err := fetchPrinterPPD(ctx, u.String()); err == nil {
			opts = parsePrinterPPD(data)
		}
	}
	if len(opts.PaperSizes) == 0 || len(opts.MediaTypes) == 0 || len(opts.Qualities) == 0 {
		fallback, err := getIPPPrinterOptions(ctx, uri)
		if err != nil && opts.Source != "ppd" {
			return PrinterOptions{}, err
		}
		if err == nil {
			if len(opts.PaperSizes) == 0 {
				opts.PaperSizes, opts.Defaults.PageSize = fallback.PaperSizes, fallback.Defaults.PageSize
			}
			if len(opts.MediaTypes) == 0 {
				opts.MediaTypes, opts.Defaults.MediaType = fallback.MediaTypes, fallback.Defaults.MediaType
			}
			if len(opts.Qualities) == 0 {
				opts.Qualities, opts.Defaults.Quality = fallback.Qualities, fallback.Defaults.Quality
			}
		}
	}
	printerOptionsCache.Lock()
	for k, entry := range printerOptionsCache.entries {
		if time.Now().After(entry.expires) {
			delete(printerOptionsCache.entries, k)
		}
	}
	if len(printerOptionsCache.entries) >= 64 {
		clear(printerOptionsCache.entries)
	}
	printerOptionsCache.entries[key] = optionsCacheEntry{opts, time.Now().Add(time.Minute)}
	printerOptionsCache.Unlock()
	return opts, nil
}

func configuredSocketPath() string { p, _ := cupsSocketPath(); return p }

func fetchPrinterPPD(ctx context.Context, uri string) ([]byte, error) {
	uri, err := printerRequestURI(uri)
	if err != nil {
		return nil, err
	}
	client, err := newSafeClient(dialTimeout)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PPD HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 || !bytes.Contains(data[:min(len(data), 256)], []byte("*PPD-Adobe:")) {
		return nil, fmt.Errorf("invalid PPD")
	}
	return data, nil
}

var ppdLine = regexp.MustCompile(`^\*([A-Za-z0-9_.-]+)(?:\s+([^/:\s]+)(?:/([^:]*))?)?:\s*(.*)$`)
var ppdUI = regexp.MustCompile(`^\*OpenUI\s+\*([A-Za-z0-9_-]+)(?:/([^:]*))?:`)

func parsePrinterPPD(data []byte) PrinterOptions {
	opts := PrinterOptions{PaperSizes: []PrinterChoice{}, MediaTypes: []PrinterChoice{}, Qualities: []PrinterChoice{}, Source: "ppd"}
	groups := map[string]string{"PageSize": "pageSize", "MediaType": "mediaType"}
	defaults := map[string]string{}
	dimensions := map[string][2]float64{}
	areas := map[string][4]float64{}
	translations := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if m := ppdUI.FindStringSubmatch(line); m != nil {
			name, label := strings.ToLower(m[1]), strings.ToLower(m[2])
			if name == "cupsprintquality" || name == "printquality" || name == "quality" || name == "resolution" || strings.Contains(label, "print quality") || strings.Contains(label, "打印质量") {
				groups[m[1]] = "quality"
			}
		}
		m := ppdLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, choice, label, value := m[1], m[2], m[3], strings.Trim(m[4], "\"\r ")
		if strings.HasPrefix(key, "Default") {
			defaults[strings.TrimPrefix(key, "Default")] = value
		}
		if key == "PaperDimension" {
			var w, h float64
			if n, _ := fmt.Sscanf(value, "%f %f", &w, &h); n == 2 {
				dimensions[choice] = [2]float64{math.Round(w*25.4/72*10000) / 10000, math.Round(h*25.4/72*10000) / 10000}
			}
		}
		if key == "ImageableArea" {
			var a [4]float64
			if n, _ := fmt.Sscanf(value, "%f %f %f %f", &a[0], &a[1], &a[2], &a[3]); n == 4 {
				areas[choice] = a
			}
		}
		if (strings.HasPrefix(key, "zh_CN.") || strings.HasPrefix(key, "zh.")) && label != "" {
			translations[key+"/"+choice] = label
		}
	}
	// Prefer a named quality control over a separate resolution control.
	qualityKey := ""
	for _, preferred := range []string{"cupsPrintQuality", "PrintQuality", "Quality"} {
		if groups[preferred] == "quality" {
			qualityKey = preferred
			break
		}
	}
	if qualityKey == "" {
		for _, line := range lines {
			if m := ppdUI.FindStringSubmatch(line); m != nil && groups[m[1]] == "quality" && m[1] != "Resolution" {
				qualityKey = m[1]
				break
			}
		}
	}
	if qualityKey == "" && groups["Resolution"] == "quality" {
		qualityKey = "Resolution"
	}
	seen := map[string]bool{}
	for _, line := range lines {
		m := ppdLine.FindStringSubmatch(line)
		if m == nil || m[2] == "" {
			continue
		}
		key, value, label := m[1], m[2], m[3]
		group := groups[key]
		if group == "" || (group == "quality" && key != qualityKey) || seen[key+"/"+value] {
			continue
		}
		seen[key+"/"+value] = true
		if translated := translations["zh_CN."+key+"/"+value]; translated != "" {
			label = translated
		} else if translated := translations["zh."+key+"/"+value]; translated != "" {
			label = translated
		}
		if label == "" {
			label = value
		}
		c := PrinterChoice{Attribute: key, Value: value, Label: label}
		if group == "pageSize" {
			d := dimensions[value]
			if !validDimensions(d[0], d[1]) {
				continue
			}
			c.WidthMM, c.HeightMM = d[0], d[1]
			if a, ok := areas[value]; ok {
				// Full-bleed drivers can advertise an imageable area extending
				// beyond the sheet, with negative left/bottom coordinates.
				c.Borderless = a[0]*25.4/72 <= 0.1 && a[1]*25.4/72 <= 0.1 && a[2]*25.4/72 >= d[0]-0.1 && a[3]*25.4/72 >= d[1]-0.1
			}
			if c.Borderless && !strings.Contains(strings.ToLower(label), "borderless") && !strings.Contains(label, "无边距") {
				c.Label += "（无边距）"
			}
			opts.PaperSizes = append(opts.PaperSizes, c)
			if defaults[key] == value {
				copy := c
				opts.Defaults.PageSize = &copy
			}
		} else if group == "mediaType" {
			opts.MediaTypes = append(opts.MediaTypes, c)
			if defaults[key] == value {
				copy := c
				opts.Defaults.MediaType = &copy
			}
		} else {
			opts.Qualities = append(opts.Qualities, c)
			if defaults[key] == value {
				copy := c
				opts.Defaults.Quality = &copy
			}
		}
	}
	return opts
}

func getIPPPrinterOptions(ctx context.Context, uri string) (PrinterOptions, error) {
	opts := PrinterOptions{PaperSizes: []PrinterChoice{}, MediaTypes: []PrinterChoice{}, Qualities: []PrinterChoice{}, Source: "ipp"}
	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en-US")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(httpToIppURI(uri))))
	var wanted goipp.Values
	for _, name := range []string{"media-supported", "media-default", "media-col-database", "media-col-default", "media-type-supported", "media-type-default", "print-quality-supported", "print-quality-default"} {
		wanted.Add(goipp.TagKeyword, goipp.String(name))
	}
	req.Operation.Add(goipp.Attribute{Name: "requested-attributes", Values: wanted})
	payload, err := req.EncodeBytes()
	if err != nil {
		return opts, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uri, bytes.NewReader(payload))
	if err != nil {
		return opts, err
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	client, err := newSafeClient(dialTimeout)
	if err != nil {
		return opts, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return opts, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return opts, fmt.Errorf("printer options HTTP status %d", resp.StatusCode)
	}
	var rsp goipp.Message
	if err := rsp.Decode(limitedBody(resp.Body)); err != nil {
		return opts, err
	}
	if rsp.Code >= 0x0100 {
		return opts, fmt.Errorf("printer options IPP status %d", rsp.Code)
	}
	opts = parseIPPPrinterOptions(rsp.Printer)
	return opts, nil
}

var pwgSize = regexp.MustCompile(`_([0-9.]+)x([0-9.]+)(mm|in)$`)

func parseIPPPrinterOptions(attrs goipp.Attributes) PrinterOptions {
	opts := PrinterOptions{PaperSizes: []PrinterChoice{}, MediaTypes: []PrinterChoice{}, Qualities: []PrinterChoice{}, Source: "ipp"}
	defaults := map[string]string{}
	seenMedia := map[string]bool{}
	for _, a := range attrs {
		if len(a.Values) > 0 && strings.HasSuffix(a.Name, "-default") {
			defaults[a.Name] = a.Values[0].V.String()
		}
	}
	for _, a := range attrs {
		for _, v := range a.Values {
			value := v.V.String()
			switch a.Name {
			case "media-col-database":
				col, ok := v.V.(goipp.Collection)
				if !ok {
					continue
				}
				c := choiceFromMediaCol(col)
				if c == nil || seenMedia[c.Value] {
					continue
				}
				seenMedia[c.Value] = true
				opts.PaperSizes = append(opts.PaperSizes, *c)
			case "media-type-supported":
				c := PrinterChoice{Attribute: "media-type", Value: value, Label: value}
				opts.MediaTypes = append(opts.MediaTypes, c)
				if defaults["media-type-default"] == value {
					copy := c
					opts.Defaults.MediaType = &copy
				}
			case "print-quality-supported":
				labels := map[string]string{"3": "草稿", "4": "标准", "5": "高质量"}
				if labels[value] == "" {
					continue
				}
				c := PrinterChoice{Attribute: "print-quality", Value: value, Label: labels[value]}
				opts.Qualities = append(opts.Qualities, c)
				if defaults["print-quality-default"] == value {
					copy := c
					opts.Defaults.Quality = &copy
				}
			}
		}
	}
	if len(opts.PaperSizes) == 0 {
		for _, a := range attrs {
			if a.Name != "media-supported" {
				continue
			}
			for _, v := range a.Values {
				value := v.V.String()
				m := pwgSize.FindStringSubmatch(value)
				if m == nil {
					continue
				}
				w, _ := strconv.ParseFloat(m[1], 64)
				h, _ := strconv.ParseFloat(m[2], 64)
				if m[3] == "in" {
					w *= 25.4
					h *= 25.4
				}
				if !validDimensions(w, h) {
					continue
				}
				opts.PaperSizes = append(opts.PaperSizes, PrinterChoice{Attribute: "media", Value: value, Label: value, WidthMM: w, HeightMM: h})
			}
		}
	}
	for _, c := range opts.PaperSizes {
		if c.Attribute == "media" && c.Value == defaults["media-default"] {
			copy := c
			opts.Defaults.PageSize = &copy
		}
	}
	for _, a := range attrs {
		if a.Name == "media-col-default" && len(a.Values) > 0 {
			if col, ok := a.Values[0].V.(goipp.Collection); ok {
				def := choiceFromMediaCol(col)
				if def != nil {
					for _, c := range opts.PaperSizes {
						if c.Value == def.Value {
							copy := c
							opts.Defaults.PageSize = &copy
							break
						}
					}
				}
			}
		}
	}
	return opts
}

func choiceFromMediaCol(col goipp.Collection) *PrinterChoice {
	var w, h int
	var margins [4]int
	count := 0
	for _, a := range col {
		if len(a.Values) == 0 {
			continue
		}
		if a.Name == "media-size" {
			if size, ok := a.Values[0].V.(goipp.Collection); ok {
				for _, d := range size {
					if len(d.Values) == 0 {
						continue
					}
					n, _ := strconv.Atoi(d.Values[0].V.String())
					if d.Name == "x-dimension" {
						w = n
					}
					if d.Name == "y-dimension" {
						h = n
					}
				}
			}
		}
		for i, name := range []string{"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin"} {
			if a.Name == name {
				n, err := strconv.Atoi(a.Values[0].V.String())
				if err != nil || n < 0 {
					return nil
				}
				margins[i] = n
				count++
			}
		}
	}
	if !validDimensions(float64(w)/100, float64(h)/100) || count != 4 {
		return nil
	}
	c := &PrinterChoice{Attribute: "media-col", WidthMM: float64(w) / 100, HeightMM: float64(h) / 100, Margins: &margins, Borderless: margins == [4]int{}}
	c.Value = fmt.Sprintf("%dx%d:%d,%d,%d,%d", w, h, margins[0], margins[1], margins[2], margins[3])
	c.Label = fmt.Sprintf("%g × %g mm", c.WidthMM, c.HeightMM)
	if c.Borderless {
		c.Label += "（无边距）"
	}
	return c
}

// ResolveDriverOptions ignores client-supplied labels/dimensions and accepts only advertised choices.
func ResolveDriverOptions(ctx context.Context, uri string, selection DriverOptions) (DriverOptions, error) {
	if selection.Empty() {
		return selection, nil
	}
	opts, err := GetPrinterOptions(ctx, uri, false)
	if err != nil {
		return DriverOptions{}, fmt.Errorf("无法读取打印机选项")
	}
	return opts.Resolve(selection)
}

func (opts PrinterOptions) Resolve(selection DriverOptions) (DriverOptions, error) {
	var result DriverOptions
	for _, group := range []struct {
		selected *PrinterChoice
		choices  []PrinterChoice
		target   **PrinterChoice
	}{{selection.PageSize, opts.PaperSizes, &result.PageSize}, {selection.MediaType, opts.MediaTypes, &result.MediaType}, {selection.Quality, opts.Qualities, &result.Quality}} {
		if group.selected == nil {
			continue
		}
		for _, choice := range group.choices {
			if choice.Attribute == group.selected.Attribute && choice.Value == group.selected.Value {
				copy := choice
				*group.target = &copy
				break
			}
		}
		if *group.target == nil {
			return DriverOptions{}, fmt.Errorf("打印机不支持选项 %s=%s，请刷新打印机选项", group.selected.Attribute, group.selected.Value)
		}
	}
	return result, nil
}

func addDriverOptions(req *goipp.Message, opts DriverOptions) {
	for _, c := range []*PrinterChoice{opts.PageSize, opts.MediaType, opts.Quality} {
		if c == nil {
			continue
		}
		if c.Attribute == "media-col" && c.Margins != nil {
			col := goipp.Collection{goipp.MakeAttribute("media-size", goipp.TagBeginCollection, goipp.Collection{goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(math.Round(c.WidthMM*100))), goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(math.Round(c.HeightMM*100)))})}
			for i, name := range []string{"media-bottom-margin", "media-left-margin", "media-right-margin", "media-top-margin"} {
				col = append(col, goipp.MakeAttribute(name, goipp.TagInteger, goipp.Integer(c.Margins[i])))
			}
			if t := opts.MediaType; t != nil && t.Attribute == "media-type" {
				col = append(col, goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(t.Value)))
			}
			req.Job.Add(goipp.MakeAttribute("media-col", goipp.TagBeginCollection, col))
		} else if c.Attribute == "print-quality" {
			n, _ := strconv.Atoi(c.Value)
			req.Job.Add(goipp.MakeAttribute(c.Attribute, goipp.TagEnum, goipp.Integer(n)))
		} else {
			tag := goipp.TagName
			if c.Attribute == "media" || c.Attribute == "media-type" {
				tag = goipp.TagKeyword
			}
			req.Job.Add(goipp.MakeAttribute(c.Attribute, tag, goipp.String(c.Value)))
		}
	}
}
