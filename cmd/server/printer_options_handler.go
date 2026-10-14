package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"cups-web/internal/ipp"
)

func printerOptionsHandler(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		writeJSONError(w, http.StatusBadRequest, "missing uri parameter")
		return
	}
	opts, err := ipp.GetPrinterOptions(r.Context(), uri, r.URL.Query().Get("force") == "1")
	if err != nil {
		log.Printf("[printer-options] %v", err)
		writeJSONError(w, http.StatusBadGateway, "无法读取打印机选项")
		return
	}
	writeJSON(w, opts)
}

func driverOptionsFromForm(r *http.Request, uri string) (ipp.DriverOptions, error) {
	var opts ipp.DriverOptions
	raw := r.FormValue("driver_options")
	if raw == "" {
		return opts, nil
	}
	if len(raw) > 16384 || json.Unmarshal([]byte(raw), &opts) != nil {
		return opts, fmt.Errorf("invalid driver_options")
	}
	return ipp.ResolveDriverOptions(r.Context(), uri, opts)
}
