// Copyright (c) 2020 Bojan Zivanovic and contributors
// SPDX-License-Identifier: MIT

// +build ignore

package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const assetDir = "raw"

const dataTemplate = `// Code generated by go generate; DO NOT EDIT.
//go:generate go run gen.go

package currency

// CLDRVersion is the CLDR version from which the data is derived.
const CLDRVersion = "{{ .CLDRVersion }}"

type currencyInfo struct {
	numericCode string
	digits      byte
}

// Defined separately to ensure consistent ordering (G10, then others).
var currencyCodes = []string{
	// G10 currencies https://en.wikipedia.org/wiki/G10_currencies.
	{{ export .G10Currencies 10 "\t" }}

	// Other currencies.
	{{ export .OtherCurrencies 10 "\t" }}
}

var currencies = map[string]currencyInfo{
	{{ export .CurrencyInfo 3 "\t" }}
}

var parentLocales = map[string]string{
	{{ export .ParentLocales 3 "\t" }}
}
`

type currencyInfo struct {
	numericCode string
	digits      byte
}

func (c currencyInfo) GoString() string {
	return fmt.Sprintf("{%q, %d}", c.numericCode, int(c.digits))
}

func main() {
	err := os.Mkdir(assetDir, 0755)
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(assetDir)

	log.Println("Fetching CLDR data...")
	CLDRVersion, err := fetchCLDR(assetDir)
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}

	log.Println("Fetching ISO data...")
	currencies, err := fetchISO()
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}

	log.Println("Processing...")
	err = replaceDigits(currencies, assetDir)
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}
	parentLocales, err := generateParentLocales(assetDir)
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}

	var currencyCodes []string
	for currencyCode := range currencies {
		currencyCodes = append(currencyCodes, currencyCode)
	}
	sort.Strings(currencyCodes)

	g10Currencies := []string{
		"AUD", "CAD", "CHF", "EUR", "GBP", "JPY", "NOK", "NZD", "SEK", "USD",
	}
	var otherCurrencies []string
	for _, currencyCode := range currencyCodes {
		if !contains(g10Currencies, currencyCode) {
			otherCurrencies = append(otherCurrencies, currencyCode)
		}
	}

	os.Remove("data.go")
	f, err := os.Create("data.go")
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}
	defer f.Close()

	funcMap := template.FuncMap{
		"export": export,
	}
	t, err := template.New("data").Funcs(funcMap).Parse(dataTemplate)
	if err != nil {
		os.RemoveAll(assetDir)
		log.Fatal(err)
	}
	t.Execute(f, struct {
		CLDRVersion     string
		G10Currencies   []string
		OtherCurrencies []string
		CurrencyInfo    map[string]*currencyInfo
		ParentLocales   map[string]string
	}{
		CLDRVersion:     CLDRVersion,
		G10Currencies:   g10Currencies,
		OtherCurrencies: otherCurrencies,
		CurrencyInfo:    currencies,
		ParentLocales:   parentLocales,
	})

	log.Println("Done.")
}

// fetchCLDR fetches the CLDR data from GitHub and returns its version.
//
// The JSON version of the data is used because it is more convenient
// to parse. See https://github.com/unicode-cldr/cldr-json for details.
func fetchCLDR(dir string) (string, error) {
	repos := []string{
		"https://github.com/unicode-cldr/cldr-core.git",
		"https://github.com/unicode-cldr/cldr-numbers-full.git",
	}
	for _, repo := range repos {
		cmd := exec.Command("git", "clone", repo)
		cmd.Dir = dir
		cmd.Stderr = os.Stderr
		_, err := cmd.Output()
		if err != nil {
			return "", err
		}
	}

	data, err := ioutil.ReadFile(dir + "/cldr-core/package.json")
	if err != nil {
		return "", fmt.Errorf("fetchCLDR: %w", err)
	}
	aux := struct {
		Version string
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return "", fmt.Errorf("fetchCLDR: %w", err)
	}

	return aux.Version, nil
}

// fetchISO fetches currency info from ISO.
//
// ISO data is needed because CLDR can't be used as a reliable source
// of numeric codes (e.g. BYR has no numeric code as of CLDR v36).
// Furthermore, CLDR includes both active and inactive currencies, while ISO
// includes only active ones, matching the needs of this package.
func fetchISO() (map[string]*currencyInfo, error) {
	data, err := fetchURL("https://www.currency-iso.org/dam/downloads/lists/list_one.xml")
	if err != nil {
		return nil, fmt.Errorf("fetchISO: %w", err)
	}
	aux := struct {
		Table []struct {
			Entry []struct {
				Code    string `xml:"Ccy"`
				Number  string `xml:"CcyNbr"`
				Digits  string `xml:"CcyMnrUnts"`
				Country string `xml:"CtryNm"`
				Name    struct {
					Value  string `xml:",chardata"`
					IsFund bool   `xml:"IsFund,attr"`
				} `xml:"CcyNm"`
			} `xml:"CcyNtry"`
		} `xml:"CcyTbl"`
	}{}
	if err := xml.Unmarshal(data, &aux); err != nil {
		return nil, fmt.Errorf("fetchISO: %w", err)
	}

	currencies := make(map[string]*currencyInfo, 170)
	for _, entry := range aux.Table[0].Entry {
		if entry.Code == "" || entry.Name.IsFund {
			continue
		}
		if entry.Code == "XUA" || entry.Code == "XSU" || entry.Code == "XDR" {
			continue
		}
		if strings.HasPrefix(entry.Country, "ZZ") {
			// Special currency (Gold, Platinum, etc).
			continue
		}
		digits := parseDigits(entry.Digits)
		currencies[entry.Code] = &currencyInfo{entry.Number, digits}
	}

	return currencies, nil
}

func fetchURL(url string) ([]byte, error) {
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetchURL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetchURL: Get %q: %v", url, resp.Status)
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetchURL: Get %q: %w", url, err)
	}

	return data, nil
}

// replaceDigits replaces each currency's digits with data from CLDR.
//
// CLDR data reflects real life usage more closely, specifying 0 digits
// (instead of 2 in ISO data) for ~14 currencies, such as ALL and RSD.
func replaceDigits(currencies map[string]*currencyInfo, dir string) error {
	data, err := ioutil.ReadFile(dir + "/cldr-core/supplemental/currencyData.json")
	if err != nil {
		return fmt.Errorf("replaceDigits: %w", err)
	}
	aux := struct {
		Supplemental struct {
			CurrencyData struct {
				Fractions map[string]map[string]string
			}
		}
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return fmt.Errorf("replaceDigits: %w", err)
	}

	for currencyCode := range currencies {
		fractions, ok := aux.Supplemental.CurrencyData.Fractions[currencyCode]
		if ok {
			currencies[currencyCode].digits = parseDigits(fractions["_digits"])
		}
	}

	return nil
}

// generateParentLocales generates parent locales from CLDR data.
//
// Ensures ignored locales are skipped.
// Replaces "root" with "en", since this package treats them as equivalent.
func generateParentLocales(dir string) (map[string]string, error) {
	data, err := ioutil.ReadFile(dir + "/cldr-core/supplemental/parentLocales.json")
	if err != nil {
		return nil, fmt.Errorf("generateParentLocales: %w", err)
	}
	aux := struct {
		Supplemental struct {
			ParentLocales struct {
				ParentLocale map[string]string
			}
		}
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return nil, fmt.Errorf("generateParentLocales: %w", err)
	}

	parentLocales := make(map[string]string)
	for locale, parent := range aux.Supplemental.ParentLocales.ParentLocale {
		// Avoid exposing the concept of "root" to users.
		if parent == "root" {
			parent = "en"
		}
		if !shouldIgnoreLocale(locale) {
			parentLocales[locale] = parent
		}
	}
	// Dsrt and Shaw are made up scripts.
	delete(parentLocales, "en-Dsrt")
	delete(parentLocales, "en-Shaw")

	return parentLocales, nil
}

func shouldIgnoreLocale(locale string) bool {
	ignoredLocales := []string{
		// Esperanto, Interlingua, Volapuk are made up languages.
		"eo", "ia", "vo",
		// Church Slavic, Manx, Prussian are historical languages.
		"cu", "gv", "prg",
		// Valencian differs from its parent only by a single character (è/é).
		"ca-ES-VALENCIA",
		// Africa secondary languages.
		"agq", "ak", "am", "asa", "bas", "bem", "bez", "bm", "cgg", "dav",
		"dje", "dua", "dyo", "ebu", "ee", "ewo", "ff", "ff-Latn", "guz",
		"ha", "ig", "jgo", "jmc", "kab", "kam", "kea", "kde", "ki", "kkj",
		"kln", "khq", "ksb", "ksf", "lag", "luo", "luy", "lu", "lg", "ln",
		"mas", "mer", "mua", "mgo", "mgh", "mfe", "naq", "nd", "nmg", "nnh",
		"nus", "nyn", "om", "pcm", "rof", "rwk", "saq", "seh", "ses", "sbp",
		"sg", "shi", "sn", "teo", "ti", "tzm", "twq", "vai", "vai-Latn", "vun",
		"wo", "xog", "xh", "zgh", "yav", "yo", "zu",
		// Europe secondary languages.
		"br", "dsb", "fo", "fur", "fy", "hsb", "ksh", "kw", "nds", "or",
		"rm", "se", "smn", "wae",
		// India secondary languages.
		"as", "brx", "gu", "kok", "ks", "mai", "ml", "mni", "mr", "sat",
		"sd", "te",
		// Other infrequently used locales.
		"ceb", "ccp", "chr", "ckb", "haw", "ii", "jv", "kl", "kn", "lkt",
		"lrc", "mi", "mzn", "os", "qu", "row", "sah", "su", "tt", "ug", "yi",
		// Special "grouping" locales.
		"root", "en-US-POSIX",
	}
	localeParts := strings.Split(locale, "-")
	ignore := false
	for _, ignoredLocale := range ignoredLocales {
		if ignoredLocale == locale || ignoredLocale == localeParts[0] {
			ignore = true
			break
		}
	}

	return ignore
}

func contains(a []string, x string) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}

func parseDigits(n string) byte {
	digits, err := strconv.Atoi(n)
	if err != nil {
		digits = 2
	}

	return byte(digits)
}

func export(i interface{}, width int, indent string) string {
	v := reflect.ValueOf(i)
	switch v.Kind() {
	case reflect.Map:
		return exportMap(v, width, indent)
	case reflect.Slice:
		return exportSlice(v, width, indent)
	default:
		return fmt.Sprintf("%#v", i)
	}
}

func exportMap(v reflect.Value, width int, indent string) string {
	var keys []string
	for _, key := range v.MapKeys() {
		keys = append(keys, key.Interface().(string))
	}
	sort.Strings(keys)

	b := strings.Builder{}
	i := 0
	for _, key := range keys {
		value := v.MapIndex(reflect.ValueOf(key))
		fmt.Fprintf(&b, `%q: %#v,`, key, value)
		if i+1 != v.Len() {
			if (i+1)%width == 0 {
				b.WriteString("\n")
				b.WriteString(indent)
			} else {
				b.WriteString(" ")
			}
		}
		i++
	}

	return b.String()
}

func exportSlice(v reflect.Value, width int, indent string) string {
	b := strings.Builder{}
	for i := 0; i < v.Len(); i++ {
		fmt.Fprintf(&b, `%#v,`, v.Index(i).Interface())
		if i+1 != v.Len() {
			if (i+1)%width == 0 {
				b.WriteString("\n")
				b.WriteString(indent)
			} else {
				b.WriteString(" ")
			}
		}
	}

	return b.String()
}
