package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// FNAR looks up Prosperous Universe companies through the FIO REST API.
type FNAR struct {
	base string
	http *http.Client
}

func NewFNAR(base string) *FNAR {
	return &FNAR{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

var companyCodeRe = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)

// NormalizeCompanyCode uppercases and trims a code, or returns "" if it can't be one.
func NormalizeCompanyCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !companyCodeRe.MatchString(code) {
		return ""
	}
	return code
}

var ErrNoCompany = userError("No company with that code.")

// Company fetches a company by its code. It returns ErrNoCompany if FNAR
// doesn't know it.
func (f *FNAR) Company(ctx context.Context, code string) (Company, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/company/code/"+url.PathEscape(code), nil)
	if err != nil {
		return Company{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return Company{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent, http.StatusNotFound:
		return Company{}, ErrNoCompany
	default:
		return Company{}, fmt.Errorf("fnar: status %d", resp.StatusCode)
	}
	var body struct {
		CompanyCode     string `json:"CompanyCode"`
		CompanyName     string `json:"CompanyName"`
		UserName        string `json:"UserName"`
		CorporationCode string `json:"CorporationCode"`
		CorporationName string `json:"CorporationName"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Company{}, fmt.Errorf("fnar: %w", err)
	}
	if body.CompanyCode == "" {
		return Company{}, ErrNoCompany
	}
	return Company{Code: body.CompanyCode, Name: body.CompanyName, UserName: body.UserName,
		CorpCode: body.CorporationCode, CorpName: body.CorporationName}, nil
}
