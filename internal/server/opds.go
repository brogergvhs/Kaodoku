package server

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// OPDS 1.2 catalog feeds for third-party readers (Panels, Mihon, KOReader).
// Link rel/type strings follow Komga, the de-facto interop reference.
const (
	opdsAtomNS   = "http://www.w3.org/2005/Atom"
	opdsPseNS    = "http://vaemendis.net/opds-pse/ns"
	opdsNavType  = "application/atom+xml;profile=opds-catalog;kind=navigation"
	opdsAcqType  = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	opdsAcqRel   = "http://opds-spec.org/acquisition"
	opdsImageRel = "http://opds-spec.org/image"
	opdsThumbRel = "http://opds-spec.org/image/thumbnail"
	opdsPseRel   = "http://vaemendis.net/opds-pse/stream"
	opdsCbzType  = "application/x-cbz"
	opdsSearchNS = "http://a9.com/-/spec/opensearch/1.1/"
	opdsBase     = "/opds/v1.2"
)

type opdsLink struct {
	XMLName     xml.Name `xml:"link"`
	Rel         string   `xml:"rel,attr"`
	Type        string   `xml:"type,attr"`
	Href        string   `xml:"href,attr"`
	PseCount    int      `xml:"pse:count,attr,omitempty"`
	PseLastRead int      `xml:"pse:lastRead,attr,omitempty"`
}

type opdsText struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

type opdsEntry struct {
	XMLName xml.Name  `xml:"entry"`
	Title   string    `xml:"title"`
	ID      string    `xml:"id"`
	Updated string    `xml:"updated"`
	Content *opdsText `xml:"content,omitempty"`
	Links   []opdsLink
}

type opdsAuthor struct {
	Name string `xml:"name"`
}

type opdsFeed struct {
	XMLName xml.Name   `xml:"feed"`
	NS      string     `xml:"xmlns,attr"`
	PseNS   string     `xml:"xmlns:pse,attr,omitempty"`
	ID      string     `xml:"id"`
	Title   string     `xml:"title"`
	Updated string     `xml:"updated"`
	Author  opdsAuthor `xml:"author"`
	Links   []opdsLink
	Entries []opdsEntry
}

type opds struct {
	svc *service.JobService
}

func registerOPDS(mux *http.ServeMux, svc *service.JobService) {
	o := &opds{svc: svc}
	mux.HandleFunc("GET /opds", o.series)
	mux.HandleFunc("GET /opds/{$}", o.series)
	mux.HandleFunc("GET "+opdsBase+"/catalog", o.series)
	mux.HandleFunc("GET "+opdsBase+"/series", o.series)
	mux.HandleFunc("GET "+opdsBase+"/opensearch.xml", o.openSearch)
	mux.HandleFunc("GET "+opdsBase+"/series/{id}", o.title)
	mux.HandleFunc("GET "+opdsBase+"/series/{id}/chapters", o.chapters)
	mux.HandleFunc("GET "+opdsBase+"/series/{id}/volumes", o.volumes)
	mux.HandleFunc("GET "+opdsBase+"/covers/{id}", o.cover)
	mux.HandleFunc("GET "+opdsBase+"/download/chapters/{file}", o.chapterArchive)
	mux.HandleFunc("GET "+opdsBase+"/download/volumes/{file}", o.volumeArchive)
	mux.HandleFunc("GET "+opdsBase+"/image/chapters/{id}/{page}", o.chapterPage)
	mux.HandleFunc("GET "+opdsBase+"/image/volumes/{id}/{page}", o.volumePage)
	mux.HandleFunc("GET "+opdsBase+"/thumb/chapters/{id}", o.chapterThumb)
	mux.HandleFunc("GET "+opdsBase+"/thumb/volumes/{id}", o.volumeThumb)
}

func writeOPDS(w http.ResponseWriter, kind string, feed opdsFeed) {
	feed.NS = opdsAtomNS
	feed.Author = opdsAuthor{Name: "Kaodoku"}
	w.Header().Set("Content-Type", kind+";charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(feed)
}

func opdsNow() string { return time.Now().UTC().Format(time.RFC3339) }

func opdsSelfStart(self string) []opdsLink {
	kind := opdsAcqType
	if strings.HasPrefix(self, opdsBase+"/series") && !strings.Contains(strings.TrimPrefix(self, opdsBase+"/series"), "/") {
		kind = opdsNavType
	}
	return []opdsLink{
		{Rel: "self", Type: kind, Href: self},
		{Rel: "start", Type: opdsNavType, Href: opdsBase + "/series"},
		{Rel: "search", Type: "application/opensearchdescription+xml", Href: opdsBase + "/opensearch.xml"},
	}
}

type openSearchURL struct {
	Type     string `xml:"type,attr"`
	Template string `xml:"template,attr"`
}

type openSearchDoc struct {
	XMLName     xml.Name        `xml:"OpenSearchDescription"`
	NS          string          `xml:"xmlns,attr"`
	ShortName   string          `xml:"ShortName"`
	Description string          `xml:"Description"`
	URLs        []openSearchURL `xml:"Url"`
}

// openSearch emits absolute template URLs like Komga: several readers
// (Mantano, Marvin) mis-resolve relative ones. Behind a non-loopback TLS
// proxy the scheme falls back to http — the same ceiling as the login flow.
func (o *opds) openSearch(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	tmpl := scheme + "://" + requestHost(r) + opdsBase + "/series?search={searchTerms}"
	w.Header().Set("Content-Type", "application/opensearchdescription+xml;charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(openSearchDoc{
		NS: opdsSearchNS, ShortName: "Kaodoku", Description: "Search the Kaodoku library",
		URLs: []openSearchURL{
			{Type: "application/atom+xml", Template: tmpl},
			{Type: opdsNavType, Template: tmpl},
		},
	})
}

const opdsPageSize = 100

func (o *opds) series(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tq, visible := visibleTitles(ctx)
	feedID, feedTitle := "urn:kaodoku:series", "All series"
	params := url.Values{}
	if q := strings.TrimSpace(r.URL.Query().Get("search")); q != "" {
		tq.Name = q
		params.Set("search", q)
		feedID, feedTitle = "urn:kaodoku:search:"+url.QueryEscape(q), "Search: "+q
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	tq.Sort, tq.Offset, tq.Limit = "title", (page-1)*opdsPageSize, opdsPageSize
	var titles []library.Title
	total := 0
	if visible {
		total, _ = o.svc.CountTitles(ctx, tq)
		if tq.Offset < total {
			titles, _ = o.svc.PageTitles(ctx, tq)
		}
	}
	pageURL := func(p int) string {
		v := url.Values{}
		for k, vs := range params {
			v[k] = vs
		}
		if p > 1 {
			v.Set("page", strconv.Itoa(p))
		}
		if len(v) == 0 {
			return opdsBase + "/series"
		}
		return opdsBase + "/series?" + v.Encode()
	}
	links := opdsSelfStart(pageURL(page))
	if page > 1 {
		links = append(links, opdsLink{Rel: "previous", Type: opdsNavType, Href: pageURL(page - 1)})
	}
	if page*opdsPageSize < total {
		links = append(links, opdsLink{Rel: "next", Type: opdsNavType, Href: pageURL(page + 1)})
	}
	descriptions := o.descriptions(r, titles)
	feed := opdsFeed{
		ID: feedID, Title: feedTitle, Updated: opdsNow(),
		Links: links,
	}
	for _, t := range titles {
		id := strconv.FormatInt(t.ID, 10)
		entry := opdsEntry{
			Title: t.DisplayTitle, ID: "urn:kaodoku:series:" + id,
			Updated: t.UpdatedAt.UTC().Format(time.RFC3339),
			Links: []opdsLink{
				{Rel: "subsection", Type: opdsAcqType, Href: opdsBase + "/series/" + id},
				{Rel: opdsImageRel, Type: "image/jpeg", Href: opdsBase + "/covers/" + id},
				{Rel: opdsThumbRel, Type: "image/jpeg", Href: opdsBase + "/covers/" + id},
			},
		}
		if d := descriptions[t.ID]; d != "" {
			entry.Content = &opdsText{Type: "text", Text: d}
		}
		feed.Entries = append(feed.Entries, entry)
	}
	writeOPDS(w, opdsNavType, feed)
}

// descriptions maps title ids to plain-text catalog description snippets.
func (o *opds) descriptions(r *http.Request, titles []library.Title) map[int64]string {
	ids := make([]int64, 0, len(titles))
	for _, t := range titles {
		if t.CatalogMangaID != nil {
			ids = append(ids, *t.CatalogMangaID)
		}
	}
	byCatalog, _ := o.svc.MangaByIDs(r.Context(), ids)
	out := make(map[int64]string, len(byCatalog))
	for _, t := range titles {
		if t.CatalogMangaID != nil {
			if m, ok := byCatalog[*t.CatalogMangaID]; ok && m.Description != "" {
				out[t.ID] = util.PlainSnippet(m.Description, 300)
			}
		}
	}
	return out
}

// title renders the per-series feed: chapters and volumes as sibling
// sub-feeds when both exist, otherwise the one available list directly.
func (o *opds) title(w http.ResponseWriter, r *http.Request) {
	t, ok := o.allowedTitle(w, r)
	if !ok {
		return
	}
	chapters := o.downloadedChapters(r, t.ID)
	vols, _ := o.svc.Volumes(r.Context(), t.ID)
	id := strconv.FormatInt(t.ID, 10)
	switch {
	case len(chapters) > 0 && len(vols) > 0:
		writeOPDS(w, opdsAcqType, opdsFeed{
			ID: "urn:kaodoku:series:" + id, Title: t.DisplayTitle, Updated: opdsNow(),
			Links: opdsSelfStart(opdsBase + "/series/" + id),
			Entries: []opdsEntry{
				{
					Title: "Chapters", ID: "urn:kaodoku:series:" + id + ":chapters", Updated: opdsNow(),
					Links: []opdsLink{{Rel: "subsection", Type: opdsAcqType, Href: opdsBase + "/series/" + id + "/chapters"}},
				},
				{
					Title: "Volumes", ID: "urn:kaodoku:series:" + id + ":volumes", Updated: opdsNow(),
					Links: []opdsLink{{Rel: "subsection", Type: opdsAcqType, Href: opdsBase + "/series/" + id + "/volumes"}},
				},
			},
		})
	case len(vols) > 0:
		o.writeVolumesFeed(w, t, vols, opdsBase+"/series/"+id)
	default:
		o.writeChaptersFeed(w, t, chapters, opdsBase+"/series/"+id)
	}
}

func (o *opds) chapters(w http.ResponseWriter, r *http.Request) {
	t, ok := o.allowedTitle(w, r)
	if !ok {
		return
	}
	id := strconv.FormatInt(t.ID, 10)
	o.writeChaptersFeed(w, t, o.downloadedChapters(r, t.ID), opdsBase+"/series/"+id+"/chapters")
}

func (o *opds) volumes(w http.ResponseWriter, r *http.Request) {
	t, ok := o.allowedTitle(w, r)
	if !ok {
		return
	}
	vols, _ := o.svc.Volumes(r.Context(), t.ID)
	id := strconv.FormatInt(t.ID, 10)
	o.writeVolumesFeed(w, t, vols, opdsBase+"/series/"+id+"/volumes")
}

func (o *opds) writeChaptersFeed(w http.ResponseWriter, t library.Title, chapters []library.ChapterReadStatus, self string) {
	feed := opdsFeed{
		PseNS: opdsPseNS,
		ID:    "urn:kaodoku:series:" + strconv.FormatInt(t.ID, 10) + ":chapters",
		Title: t.DisplayTitle, Updated: opdsNow(),
		Links: opdsSelfStart(self),
	}
	updated := t.UpdatedAt.UTC().Format(time.RFC3339)
	for _, ch := range chapters {
		id := strconv.FormatInt(ch.ID, 10)
		entry := opdsEntry{
			Title: chapterEntryTitle(t.DisplayTitle, ch), ID: "urn:kaodoku:chapter:" + id, Updated: updated,
			Links: []opdsLink{
				{Rel: opdsAcqRel, Type: opdsCbzType, Href: opdsBase + "/download/chapters/" + id + ".cbz"},
				{Rel: opdsThumbRel, Type: "image/jpeg", Href: opdsBase + "/thumb/chapters/" + id},
			},
		}
		if ch.Pages > 0 {
			entry.Links = append(entry.Links, opdsLink{
				Rel: opdsPseRel, Type: "image/jpeg",
				Href:     opdsBase + "/image/chapters/" + id + "/{pageNumber}",
				PseCount: ch.Pages, PseLastRead: ch.LastPage,
			})
		}
		feed.Entries = append(feed.Entries, entry)
	}
	writeOPDS(w, opdsAcqType, feed)
}

func (o *opds) writeVolumesFeed(w http.ResponseWriter, t library.Title, vols []library.Volume, self string) {
	feed := opdsFeed{
		PseNS: opdsPseNS,
		ID:    "urn:kaodoku:series:" + strconv.FormatInt(t.ID, 10) + ":volumes",
		Title: t.DisplayTitle, Updated: opdsNow(),
		Links: opdsSelfStart(self),
	}
	updated := t.UpdatedAt.UTC().Format(time.RFC3339)
	for _, v := range vols {
		id := strconv.FormatInt(v.ID, 10)
		entry := opdsEntry{
			Title: volumeEntryTitle(t.DisplayTitle, v), ID: "urn:kaodoku:volume:" + id, Updated: updated,
			Links: []opdsLink{
				{Rel: opdsAcqRel, Type: opdsCbzType, Href: opdsBase + "/download/volumes/" + id + ".cbz"},
				{Rel: opdsThumbRel, Type: "image/jpeg", Href: opdsBase + "/thumb/volumes/" + id},
			},
		}
		if v.Pages > 0 {
			entry.Links = append(entry.Links, opdsLink{
				Rel: opdsPseRel, Type: "image/jpeg",
				Href:     opdsBase + "/image/volumes/" + id + "/{pageNumber}",
				PseCount: v.Pages, PseLastRead: v.LastPage,
			})
		}
		feed.Entries = append(feed.Entries, entry)
	}
	writeOPDS(w, opdsAcqType, feed)
}

func chapterEntryTitle(series string, ch library.ChapterReadStatus) string {
	name := "Ch " + ch.Label
	if ch.Title != "" {
		name += " · " + ch.Title
	}
	return series + " — " + name
}

func volumeEntryTitle(series string, v library.Volume) string {
	name := "Vol " + strconv.FormatFloat(v.Number, 'f', -1, 64)
	if v.Name != "" {
		name += " · " + v.Name
	}
	return series + " — " + name
}

func (o *opds) allowedTitle(w http.ResponseWriter, r *http.Request) (library.Title, bool) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return library.Title{}, false
	}
	t, err := o.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) {
		writeError(w, http.StatusNotFound, "series not found")
		return library.Title{}, false
	}
	return t, true
}

func (o *opds) downloadedChapters(r *http.Request, titleID int64) []library.ChapterReadStatus {
	statuses, _ := o.svc.TitleReadStatuses(r.Context(), titleID)
	out := statuses[:0]
	for _, st := range statuses {
		if st.Downloaded && st.OutputFile != "" {
			out = append(out, st)
		}
	}
	return out
}

func (o *opds) chapterThumb(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	status, err := o.svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, status.TitleID) {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	r.SetPathValue("page", "1")
	serveChapterPage(w, r, o.svc)
}

func (o *opds) volumeThumb(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	vol, err := o.svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, vol.TitleID) {
		writeError(w, http.StatusNotFound, "volume not found")
		return
	}
	r.SetPathValue("page", "1")
	serveVolumePage(w, r, o.svc)
}

// opdsPage parses the {page} path value as a 0-based PSE page number.
func opdsPage(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("page"))
	if err != nil || n < 0 {
		return 0, errInvalidPage
	}
	return n, nil
}

var errInvalidPage = errors.New("invalid page")

func (o *opds) cover(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	serveTitleCover(w, r, o.svc, id)
}

// opdsDownloadID parses the {file} path value ("123" or "123.cbz").
func opdsDownloadID(r *http.Request) (int64, error) {
	return strconv.ParseInt(strings.TrimSuffix(r.PathValue("file"), ".cbz"), 10, 64)
}

func (o *opds) chapterArchive(w http.ResponseWriter, r *http.Request) {
	id, err := opdsDownloadID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	status, err := o.svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, status.TitleID) {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if !status.Downloaded || status.OutputFile == "" {
		writeError(w, http.StatusNotFound, "chapter is not downloaded")
		return
	}
	serveArchive(w, r, status.OutputFile)
}

func (o *opds) volumeArchive(w http.ResponseWriter, r *http.Request) {
	id, err := opdsDownloadID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	vol, err := o.svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, vol.TitleID) {
		writeError(w, http.StatusNotFound, "volume not found")
		return
	}
	serveArchive(w, r, vol.File)
}

// chapterPage streams one 0-based PSE page and records it as read, so OPDS
// readers sync progress back.
func (o *opds) chapterPage(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	page, err := opdsPage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	status, err := o.svc.ChapterReadStatus(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, status.TitleID) {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	page++
	r.SetPathValue("page", strconv.Itoa(page))
	if opdsMarksProgress(r) && status.Downloaded && status.OutputFile != "" && page <= status.Pages {
		_, _ = o.svc.MarkPageRead(r.Context(), id, page, status.Pages)
	}
	serveChapterPage(w, r, o.svc)
}

func (o *opds) volumePage(w http.ResponseWriter, r *http.Request) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	page, err := opdsPage(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	vol, err := o.svc.GetVolume(r.Context(), id)
	if err != nil || !titleAllowed(r.Context(), o.svc, vol.TitleID) {
		writeError(w, http.StatusNotFound, "volume not found")
		return
	}
	page++
	r.SetPathValue("page", strconv.Itoa(page))
	if opdsMarksProgress(r) && vol.File != "" && page <= vol.Pages {
		_, _ = o.svc.MarkVolumePageRead(r.Context(), id, page, vol.Pages)
	}
	serveVolumePage(w, r, o.svc)
}

// opdsMarksProgress gates progress write-back to Basic-authed OPDS readers,
// keeping cross-site <img> loads with a session cookie side-effect free.
func opdsMarksProgress(r *http.Request) bool {
	return !authEnabled() || strings.HasPrefix(r.Header.Get("Authorization"), "Basic ")
}
