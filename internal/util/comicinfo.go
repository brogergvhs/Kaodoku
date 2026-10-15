package util

import (
	"archive/zip"
	"encoding/xml"
	"path/filepath"
	"strings"
)

// ComicInfo is the subset of the ComicInfo.xml schema kaodoku populates
// (v2.0 fields plus the v2.1-draft Tags), in XSD sequence order so strict
// validators accept the output.
type ComicInfo struct {
	XMLName   xml.Name `xml:"ComicInfo"`
	Title     string   `xml:"Title,omitempty"`
	Series    string   `xml:"Series,omitempty"`
	Number    string   `xml:"Number,omitempty"`
	Count     int      `xml:"Count,omitempty"`
	Volume    int      `xml:"Volume,omitempty"`
	Summary   string   `xml:"Summary,omitempty"`
	Year      int      `xml:"Year,omitempty"`
	Writer    string   `xml:"Writer,omitempty"`
	Genre     string   `xml:"Genre,omitempty"`
	Tags      string   `xml:"Tags,omitempty"`
	Web       string   `xml:"Web,omitempty"`
	PageCount int      `xml:"PageCount,omitempty"`
	Manga     string   `xml:"Manga,omitempty"`
	AgeRating string   `xml:"AgeRating,omitempty"`
}

const ComicInfoName = "ComicInfo.xml"

func MarshalComicInfo(ci ComicInfo) ([]byte, error) {
	body, err := xml.MarshalIndent(ci, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// ReadComicInfo extracts an embedded ComicInfo.xml from a CBZ; ok is false
// when absent or unparsable — callers fall back to filename parsing.
func ReadComicInfo(cbzPath string) (ComicInfo, bool) {
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return ComicInfo{}, false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Base(f.Name), ComicInfoName) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ComicInfo{}, false
		}
		defer rc.Close()
		var ci ComicInfo
		if xml.NewDecoder(rc).Decode(&ci) != nil {
			return ComicInfo{}, false
		}
		return ci, true
	}
	return ComicInfo{}, false
}
