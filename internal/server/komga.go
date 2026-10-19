package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// Komga-compatible read API under /komga, so apps built for Komga (Mihon,
// Komelia, Panels) can browse, read, and sync progress. Response shapes
// follow Komga master and the field lists Mihon's decoders require.
const komgaLibraryID = "1"

type komga struct {
	svc *service.JobService
}

func registerKomga(mux *http.ServeMux, svc *service.JobService) {
	k := &komga{svc: svc}
	mux.HandleFunc("GET /komga/api/v2/users/me", k.me)
	mux.HandleFunc("GET /komga/api/v1/users/me", k.me)
	mux.HandleFunc("GET /komga/api/v1/claim", k.claim)
	mux.HandleFunc("GET /komga/api/v1/client-settings/global/list", k.emptyObject)
	mux.HandleFunc("GET /komga/api/v1/client-settings/user/list", k.emptyObject)
	mux.HandleFunc("GET /komga/api/v1/oauth2/providers", k.emptyList)
	mux.HandleFunc("GET /komga/api/v1/libraries", k.libraries)
	mux.HandleFunc("GET /komga/api/v1/libraries/{id}", k.library)
	mux.HandleFunc("GET /komga/api/v1/series", k.seriesList)
	mux.HandleFunc("POST /komga/api/v1/series/list", k.seriesListPost)
	mux.HandleFunc("GET /komga/api/v1/series/latest", k.seriesByTime("updated"))
	mux.HandleFunc("GET /komga/api/v1/series/updated", k.seriesByTime("updated"))
	mux.HandleFunc("GET /komga/api/v1/series/new", k.seriesByTime("created"))
	mux.HandleFunc("GET /komga/api/v1/books", k.emptyPage)
	mux.HandleFunc("POST /komga/api/v1/books/list", k.booksListPost)
	mux.HandleFunc("GET /komga/api/v1/books/latest", k.booksLatest)
	mux.HandleFunc("GET /komga/api/v1/books/ondeck", k.booksOnDeck)
	mux.HandleFunc("POST /komga/api/v1/series/list/alphabetical-groups", k.alphabeticalGroups)
	mux.HandleFunc("GET /komga/api/v1/series/{id}", k.seriesGet)
	mux.HandleFunc("GET /komga/api/v1/series/{id}/books", k.seriesBooks)
	mux.HandleFunc("GET /komga/api/v1/series/{id}/thumbnail", k.seriesThumbnail)
	mux.HandleFunc("GET /komga/api/v1/series/{id}/thumbnails", k.seriesThumbnails)
	mux.HandleFunc("GET /komga/api/v1/series/{id}/collections", k.emptyList)
	mux.HandleFunc("POST /komga/api/v1/series/{id}/read-progress", k.seriesReadProgress(true))
	mux.HandleFunc("DELETE /komga/api/v1/series/{id}/read-progress", k.seriesReadProgress(false))
	mux.HandleFunc("GET /komga/api/v1/books/{id}", k.bookGet)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/pages", k.bookPages)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/pages/{page}", k.bookPage)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/pages/{page}/raw", k.bookPage)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/pages/{page}/thumbnail", k.bookPage)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/previous", k.bookSibling(-1))
	mux.HandleFunc("GET /komga/api/v1/books/{id}/next", k.bookSibling(1))
	mux.HandleFunc("GET /komga/api/v1/books/{id}/readlists", k.emptyList)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/thumbnails", k.bookThumbnails)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/thumbnail", k.bookThumbnail)
	mux.HandleFunc("GET /komga/api/v1/books/{id}/file", k.bookFile)
	mux.HandleFunc("PATCH /komga/api/v1/books/{id}/read-progress", k.bookReadProgress)
	mux.HandleFunc("DELETE /komga/api/v1/books/{id}/read-progress", k.bookReadProgressDelete)
	mux.HandleFunc("GET /komga/api/v2/series/{id}/read-progress/tachiyomi", k.tachiyomiProgress)
	mux.HandleFunc("PUT /komga/api/v2/series/{id}/read-progress/tachiyomi", k.tachiyomiProgressPut)
	mux.HandleFunc("GET /komga/api/v1/collections", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v1/readlists", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v1/genres", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/tags", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/publishers", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/authors", k.emptyList)
	mux.HandleFunc("GET /komga/api/v1/authors/roles", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/authors/names", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/languages", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/age-ratings", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/sharing-labels", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v1/fonts/families", k.emptyStrings)
	mux.HandleFunc("GET /komga/api/v2/authors", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/authors/roles", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/authors/names", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/genres", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/tags", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/publishers", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/languages", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/sharing-labels", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/age-ratings", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/series/release-years", k.emptyPage)
	mux.HandleFunc("GET /komga/api/v2/users/me/api-keys", k.emptyList)
	mux.HandleFunc("GET /komga/api/v1/login/set-cookie", k.setCookie)
	mux.HandleFunc("GET /komga/sse/v1/events", k.sse)
	mux.HandleFunc("GET /komga/series/{id}", k.seriesWeb)
	mux.HandleFunc("GET /komga/book/{id}", k.bookWeb)
	mux.HandleFunc("/komga/", k.unmatched)
}

// --- DTOs: field sets mirror Komga; REQUIRED fields per Mihon must never be
// omitted or null (its decoder has ignoreUnknownKeys but no null coercion).

type komgaSort struct {
	Empty    bool `json:"empty"`
	Sorted   bool `json:"sorted"`
	Unsorted bool `json:"unsorted"`
}

type komgaPageable struct {
	Sort       komgaSort `json:"sort"`
	Offset     int       `json:"offset"`
	PageNumber int       `json:"pageNumber"`
	PageSize   int       `json:"pageSize"`
	Paged      bool      `json:"paged"`
	Unpaged    bool      `json:"unpaged"`
}

type komgaPage struct {
	Content          any           `json:"content"`
	Pageable         komgaPageable `json:"pageable"`
	Last             bool          `json:"last"`
	TotalPages       int           `json:"totalPages"`
	TotalElements    int           `json:"totalElements"`
	Size             int           `json:"size"`
	Number           int           `json:"number"`
	Sort             komgaSort     `json:"sort"`
	First            bool          `json:"first"`
	NumberOfElements int           `json:"numberOfElements"`
	Empty            bool          `json:"empty"`
}

type komgaAuthor struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type komgaSeriesMetadata struct {
	Status               string          `json:"status"`
	StatusLock           bool            `json:"statusLock"`
	Title                string          `json:"title"`
	TitleLock            bool            `json:"titleLock"`
	TitleSort            string          `json:"titleSort"`
	TitleSortLock        bool            `json:"titleSortLock"`
	Summary              string          `json:"summary"`
	SummaryLock          bool            `json:"summaryLock"`
	ReadingDirection     string          `json:"readingDirection"`
	ReadingDirectionLock bool            `json:"readingDirectionLock"`
	Publisher            string          `json:"publisher"`
	PublisherLock        bool            `json:"publisherLock"`
	AgeRating            *int            `json:"ageRating"`
	AgeRatingLock        bool            `json:"ageRatingLock"`
	Language             string          `json:"language"`
	LanguageLock         bool            `json:"languageLock"`
	Genres               []string        `json:"genres"`
	GenresLock           bool            `json:"genresLock"`
	Tags                 []string        `json:"tags"`
	TagsLock             bool            `json:"tagsLock"`
	TotalBookCount       *int            `json:"totalBookCount"`
	TotalBookCountLock   bool            `json:"totalBookCountLock"`
	SharingLabels        []string        `json:"sharingLabels"`
	SharingLabelsLock    bool            `json:"sharingLabelsLock"`
	Links                []komgaLink     `json:"links"`
	LinksLock            bool            `json:"linksLock"`
	AlternateTitles      []komgaAltTitle `json:"alternateTitles"`
	AlternateTitlesLock  bool            `json:"alternateTitlesLock"`
	Created              string          `json:"created"`
	LastModified         string          `json:"lastModified"`
}

type komgaLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type komgaAltTitle struct {
	Label string `json:"label"`
	Title string `json:"title"`
}

type komgaBooksMetadata struct {
	Authors       []komgaAuthor `json:"authors"`
	Tags          []string      `json:"tags"`
	ReleaseDate   *string       `json:"releaseDate"`
	Summary       string        `json:"summary"`
	SummaryNumber string        `json:"summaryNumber"`
	Created       string        `json:"created"`
	LastModified  string        `json:"lastModified"`
}

type komgaSeries struct {
	ID                   string              `json:"id"`
	LibraryID            string              `json:"libraryId"`
	Name                 string              `json:"name"`
	URL                  string              `json:"url"`
	Created              string              `json:"created"`
	LastModified         string              `json:"lastModified"`
	FileLastModified     string              `json:"fileLastModified"`
	BooksCount           int                 `json:"booksCount"`
	BooksReadCount       int                 `json:"booksReadCount"`
	BooksUnreadCount     int                 `json:"booksUnreadCount"`
	BooksInProgressCount int                 `json:"booksInProgressCount"`
	Metadata             komgaSeriesMetadata `json:"metadata"`
	BooksMetadata        komgaBooksMetadata  `json:"booksMetadata"`
	Deleted              bool                `json:"deleted"`
	Oneshot              bool                `json:"oneshot"`
}

type komgaMedia struct {
	Status               string `json:"status"`
	MediaType            string `json:"mediaType"`
	PagesCount           int    `json:"pagesCount"`
	Comment              string `json:"comment"`
	EpubDivinaCompatible bool   `json:"epubDivinaCompatible"`
	MediaProfile         string `json:"mediaProfile"`
}

type komgaBookMetadata struct {
	Title           string        `json:"title"`
	TitleLock       bool          `json:"titleLock"`
	Summary         string        `json:"summary"`
	SummaryLock     bool          `json:"summaryLock"`
	Number          string        `json:"number"`
	NumberLock      bool          `json:"numberLock"`
	NumberSort      float64       `json:"numberSort"`
	NumberSortLock  bool          `json:"numberSortLock"`
	ReleaseDate     *string       `json:"releaseDate"`
	ReleaseDateLock bool          `json:"releaseDateLock"`
	Authors         []komgaAuthor `json:"authors"`
	AuthorsLock     bool          `json:"authorsLock"`
	Tags            []string      `json:"tags"`
	TagsLock        bool          `json:"tagsLock"`
	ISBN            string        `json:"isbn"`
	ISBNLock        bool          `json:"isbnLock"`
	Links           []komgaLink   `json:"links"`
	LinksLock       bool          `json:"linksLock"`
	Created         string        `json:"created"`
	LastModified    string        `json:"lastModified"`
}

type komgaReadProgress struct {
	Page         int    `json:"page"`
	Completed    bool   `json:"completed"`
	ReadDate     string `json:"readDate"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	DeviceID     string `json:"deviceId"`
	DeviceName   string `json:"deviceName"`
}

type komgaBook struct {
	ID               string             `json:"id"`
	SeriesID         string             `json:"seriesId"`
	SeriesTitle      string             `json:"seriesTitle"`
	LibraryID        string             `json:"libraryId"`
	Name             string             `json:"name"`
	URL              string             `json:"url"`
	Number           int                `json:"number"`
	Created          string             `json:"created"`
	LastModified     string             `json:"lastModified"`
	FileLastModified string             `json:"fileLastModified"`
	SizeBytes        int64              `json:"sizeBytes"`
	Size             string             `json:"size"`
	Media            komgaMedia         `json:"media"`
	Metadata         komgaBookMetadata  `json:"metadata"`
	ReadProgress     *komgaReadProgress `json:"readProgress"`
	Deleted          bool               `json:"deleted"`
	FileHash         string             `json:"fileHash"`
	Oneshot          bool               `json:"oneshot"`
}

type komgaPageDto struct {
	Number    int    `json:"number"`
	FileName  string `json:"fileName"`
	MediaType string `json:"mediaType"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SizeBytes int64  `json:"sizeBytes"`
	Size      string `json:"size"`
}

func komgaTime(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0)
	}
	return t.UTC().Format("2006-01-02T15:04:05") + "Z"
}

func komgaWrite(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func komgaPageOf(content any, n, page, size int, unpaged bool) komgaPage {
	if unpaged || size <= 0 {
		return komgaPage{
			Content:  content,
			Pageable: komgaPageable{Sort: komgaSort{Empty: true, Unsorted: true}, Unpaged: true},
			Last:     true, TotalPages: 1, TotalElements: n, Size: n, Number: 0,
			Sort: komgaSort{Empty: true, Unsorted: true}, First: true,
			NumberOfElements: n, Empty: n == 0,
		}
	}
	totalPages := (n + size - 1) / size
	if totalPages == 0 {
		totalPages = 1
	}
	count := n - page*size
	if count > size {
		count = size
	}
	if count < 0 {
		count = 0
	}
	return komgaPage{
		Content: content,
		Pageable: komgaPageable{
			Sort: komgaSort{Empty: true, Unsorted: true}, Offset: page * size,
			PageNumber: page, PageSize: size, Paged: true,
		},
		Last: page >= totalPages-1, TotalPages: totalPages, TotalElements: n,
		Size: size, Number: page, Sort: komgaSort{Empty: true, Unsorted: true},
		First: page == 0, NumberOfElements: count, Empty: count == 0,
	}
}

// --- handlers

func (k *komga) seriesWeb(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/library/"+r.PathValue("id"), http.StatusSeeOther)
}

func (k *komga) bookWeb(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/library/"+strconv.FormatInt(ref.titleID, 10), http.StatusSeeOther)
}

// unmatched logs Komga-client requests we don't serve yet, so interop gaps
// name themselves in the server log.
func (k *komga) unmatched(w http.ResponseWriter, r *http.Request) {
	log.Printf("komga: unmatched %s %s", r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func (k *komga) emptyObject(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, map[string]any{})
}

// issueSession mirrors Komga's session flow: a Basic-authed users/me call
// returns X-Auth-Token + KOMGA-SESSION, which clients reuse for everything else.
func (k *komga) issueSession(w http.ResponseWriter, r *http.Request) {
	if !authEnabled() || r.Header.Get("X-Auth-Token") != "" {
		return
	}
	if _, err := r.Cookie("KOMGA-SESSION"); err == nil {
		return
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return
	}
	token, err := k.svc.Auth().Login(r.Context(), username, password, r.UserAgent(), clientIP(r))
	if err != nil {
		return
	}
	w.Header().Set("X-Auth-Token", token)
	http.SetCookie(w, &http.Cookie{
		Name: "KOMGA-SESSION", Value: token, Path: "/komga",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
}

func (k *komga) claim(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, map[string]any{"isClaimed": true})
}

func (k *komga) me(w http.ResponseWriter, r *http.Request) {
	k.issueSession(w, r)
	user := userFrom(r.Context())
	roles := []string{"USER", "FILE_DOWNLOAD", "PAGE_STREAMING"}
	if user.Can(auth.PermUsersManage) {
		roles = append([]string{"ADMIN"}, roles...)
	}
	komgaWrite(w, map[string]any{
		"id":                 strconv.FormatInt(user.ID, 10),
		"email":              user.Username,
		"roles":              roles,
		"sharedAllLibraries": true,
		"sharedLibrariesIds": []string{},
		"labelsAllow":        []string{},
		"labelsExclude":      []string{},
	})
}

func komgaLibrary() map[string]any {
	return map[string]any{
		"id": komgaLibraryID, "name": "Kaodoku", "root": "",
		"importComicInfoBook": false, "importComicInfoSeries": false,
		"importComicInfoCollection": false, "importComicInfoReadList": false,
		"importComicInfoSeriesAppendVolume": false, "importEpubBook": false,
		"importEpubSeries": false, "importMylarSeries": false,
		"importLocalArtwork": false, "importBarcodeIsbn": false,
		"scanForceModifiedTime": false, "scanInterval": "DISABLED",
		"scanOnStartup": false, "scanCbx": true, "scanPdf": false, "scanEpub": false,
		"scanDirectoryExclusions": []string{}, "repairExtensions": false,
		"convertToCbz": false, "emptyTrashAfterScan": false, "seriesCover": "FIRST",
		"hashFiles": false, "hashPages": false, "hashKoreader": false,
		"analyzeDimensions": false, "oneshotsDirectory": nil, "unavailable": false,
	}
}

func (k *komga) libraries(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, []map[string]any{komgaLibrary()})
}

func (k *komga) library(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, komgaLibrary())
}

func (k *komga) emptyPage(w http.ResponseWriter, r *http.Request) {
	komgaWrite(w, komgaPageOf([]any{}, 0, 0, 20, r.URL.Query().Get("unpaged") == "true"))
}

func (k *komga) emptyStrings(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, []string{})
}

func (k *komga) emptyList(w http.ResponseWriter, _ *http.Request) {
	komgaWrite(w, []komgaAuthor{})
}

func komgaSeriesStatus(release string) string {
	switch strings.ToLower(strings.TrimSpace(release)) {
	case "finished", "completed", "complete", "ended":
		return "ENDED"
	case "hiatus":
		return "HIATUS"
	case "cancelled", "canceled", "abandoned":
		return "ABANDONED"
	default:
		return "ONGOING"
	}
}

func (k *komga) toSeries(t library.Title, read, inProgress, books int) komgaSeries {
	id := strconv.FormatInt(t.ID, 10)
	created := komgaTime(t.CreatedAt)
	modified := komgaTime(t.UpdatedAt)
	tags := t.ContentTags
	if tags == nil {
		tags = []string{}
	}
	unread := books - read - inProgress
	if unread < 0 {
		unread = 0
	}
	return komgaSeries{
		ID: id, LibraryID: komgaLibraryID, Name: t.DisplayTitle, URL: "",
		Created: created, LastModified: modified, FileLastModified: modified,
		BooksCount: books, BooksReadCount: read, BooksUnreadCount: unread,
		BooksInProgressCount: inProgress,
		Metadata: komgaSeriesMetadata{
			Status: komgaSeriesStatus(t.ReleaseStatus),
			Title:  t.DisplayTitle, TitleSort: t.DisplayTitle,
			Summary: "", ReadingDirection: "", Publisher: "", Language: "",
			Genres: []string{}, Tags: tags, SharingLabels: []string{},
			Links: []komgaLink{}, AlternateTitles: []komgaAltTitle{},
			Created: created, LastModified: modified,
		},
		BooksMetadata: komgaBooksMetadata{
			Authors: []komgaAuthor{}, Tags: []string{}, Summary: "", SummaryNumber: "",
			Created: created, LastModified: modified,
		},
	}
}

// seriesCounts derives book counts from the title aggregates: books are the
// downloaded chapters, or the volumes for volume-only titles.
func seriesCounts(t library.Title) (books, read int) {
	books = int(t.DiscoveredCount - t.MissingCount - t.FailedCount)
	if books < 0 {
		books = 0
	}
	if books == 0 && t.VolumeCount > 0 {
		return int(t.VolumeCount), int(t.VolumeReadCount)
	}
	read = int(t.ReadCount)
	if read > books {
		read = books
	}
	return books, read
}

func (k *komga) seriesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tq, _ := visibleTitles(r.Context())
	tq.Name = strings.TrimSpace(q.Get("search"))
	tq.Sort = "title"
	if s := q.Get("sort"); s != "" {
		parts := strings.SplitN(s, ",", 2)
		switch parts[0] {
		case "createdDate":
			tq.Sort = "added"
		case "lastModifiedDate":
			tq.Sort = "updated"
		}
		if len(parts) == 2 && parts[1] == "desc" {
			tq.Dir = "desc"
		}
	}
	var keep []func(library.Title) bool
	if statuses := q.Get("status"); statuses != "" {
		want := map[string]bool{}
		for _, s := range strings.Split(statuses, ",") {
			want[s] = true
		}
		keep = append(keep, func(t library.Title) bool { return want[komgaSeriesStatus(t.ReleaseStatus)] })
	}
	if reads := q["read_status"]; len(reads) > 0 {
		want := map[string]bool{}
		for _, s := range reads {
			want[s] = true
		}
		keep = append(keep, func(t library.Title) bool {
			books, read := seriesCounts(t)
			state := "IN_PROGRESS"
			switch {
			case read == 0:
				state = "UNREAD"
			case read >= books && books > 0:
				state = "READ"
			}
			return want[state]
		})
	}
	k.writeSeriesPage(w, r, tq, keep...)
}

// seriesByTime serves the dashboard rows (latest/new/updated) as a sorted page.
func (k *komga) seriesByTime(field string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tq, _ := visibleTitles(r.Context())
		tq.Sort, tq.Dir = "updated", "desc"
		if field == "created" {
			tq.Sort = "added"
		}
		k.writeSeriesPage(w, r, tq)
	}
}

// seriesListPost is Komga's newer browse endpoint; the condition tree is
// ignored beyond full-text search.
func (k *komga) seriesListPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FullTextSearch string `json:"fullTextSearch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	tq, _ := visibleTitles(r.Context())
	tq.Name, tq.Sort = strings.TrimSpace(body.FullTextSearch), "title"
	k.writeSeriesPage(w, r, tq)
}

func (k *komga) writeSeriesPage(w http.ResponseWriter, r *http.Request, tq library.TitleQuery, keep ...func(library.Title) bool) {
	ctx := r.Context()
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 20
	}
	unpaged := q.Get("unpaged") == "true"
	var window []library.Title
	total := 0
	if _, ok := visibleTitles(ctx); ok {
		if len(keep) == 0 && !unpaged {
			total, _ = k.svc.CountTitles(ctx, tq)
			tq.Offset, tq.Limit = page*size, size
			if tq.Offset < total {
				window, _ = k.svc.PageTitles(ctx, tq)
			}
		} else {
			all, _ := k.svc.PageTitles(ctx, tq)
			kept := all[:0]
			for _, t := range all {
				if slices.IndexFunc(keep, func(f func(library.Title) bool) bool { return !f(t) }) < 0 {
					kept = append(kept, t)
				}
			}
			total = len(kept)
			window = kept
			if !unpaged {
				lo := min(page*size, total)
				window = kept[lo:min(lo+size, total)]
			}
		}
	}
	out := make([]komgaSeries, 0, len(window))
	for _, t := range window {
		books, read := seriesCounts(t)
		out = append(out, k.toSeries(t, read, 0, books))
	}
	komgaWrite(w, komgaPageOf(out, total, page, size, unpaged))
}

func (k *komga) booksLatest(w http.ResponseWriter, r *http.Request) {
	books := k.recentBooks(r)
	if books == nil {
		books = []komgaBook{}
	}
	sortKomgaBooks(books, "lastModifiedDate,desc")
	k.writeBooksPage(w, r, books)
}

// booksOnDeck lists the next unread book of each started series.
func (k *komga) booksOnDeck(w http.ResponseWriter, r *http.Request) {
	books := []komgaBook{}
	var titles []library.Title
	if tq, ok := visibleTitles(r.Context()); ok {
		ids, err := k.svc.ReadTitleIDs(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		tq.IDs = ids
		titles, _ = k.svc.PageTitles(r.Context(), tq)
	}
	for _, t := range titles {
		total, read := seriesCounts(t)
		if read == 0 || read >= total {
			continue
		}
		for _, b := range k.seriesBookList(r, t) {
			if b.ReadProgress == nil {
				books = append(books, b)
				break
			}
		}
		if len(books) >= 20 {
			break
		}
	}
	k.writeBooksPage(w, r, books)
}

func (k *komga) bookSibling(step int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ref, err := k.resolveBook(r)
		if err != nil {
			writeError(w, http.StatusNotFound, "book not found")
			return
		}
		t, err := k.svc.GetTitle(r.Context(), ref.titleID)
		if err != nil {
			writeError(w, http.StatusNotFound, "book not found")
			return
		}
		books := k.seriesBookList(r, t)
		want := r.PathValue("id")
		for i, b := range books {
			if b.ID == want {
				j := i + step
				if j < 0 || j >= len(books) {
					writeError(w, http.StatusNotFound, "no sibling")
					return
				}
				komgaWrite(w, books[j])
				return
			}
		}
		writeError(w, http.StatusNotFound, "book not found")
	}
}

func (k *komga) seriesReadProgress(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := k.allowedTitle(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		for _, b := range k.seriesBookList(r, t) {
			id, err := strconv.ParseInt(b.ID[1:], 10, 64)
			if err != nil {
				continue
			}
			if strings.HasPrefix(b.ID, "v") {
				_ = k.svc.SetVolumeRead(ctx, id, read)
			} else if read {
				_, _ = k.svc.MarkChapterRead(ctx, id)
			} else {
				_, _ = k.svc.MarkChapterUnread(ctx, id)
			}
		}
		if read {
			k.svc.PushAniListEntry(ctx, auth.UserID(ctx), t.ID)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// alphabeticalGroups backs the A-Z browse index.
func (k *komga) alphabeticalGroups(w http.ResponseWriter, r *http.Request) {
	var names []string
	if tq, ok := visibleTitles(r.Context()); ok {
		var err error
		if names, err = k.svc.TitleNames(r.Context(), tq); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	counts := map[string]int{}
	for _, name := range names {
		group := "#"
		for _, c := range name {
			if unicode.IsLetter(c) {
				group = strings.ToUpper(string(c))
			}
			break
		}
		counts[group]++
	}
	type groupCount struct {
		Group string `json:"group"`
		Count int    `json:"count"`
	}
	out := make([]groupCount, 0, len(counts))
	for g, c := range counts {
		out = append(out, groupCount{Group: g, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	komgaWrite(w, out)
}

func (k *komga) seriesThumbnails(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	komgaWrite(w, []map[string]any{{
		"id": "0", "seriesId": strconv.FormatInt(t.ID, 10), "type": "GENERATED", "selected": true,
		"mediaType": "image/jpeg", "fileSize": 0, "width": 0, "height": 0,
	}})
}

func (k *komga) bookThumbnails(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	komgaWrite(w, []map[string]any{{
		"id": "0", "bookId": r.PathValue("id"), "type": "GENERATED", "selected": true,
		"mediaType": "image/jpeg", "fileSize": ref.pages, "width": 0, "height": 0,
	}})
}

func (k *komga) setCookie(w http.ResponseWriter, r *http.Request) {
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "KOMGA-SESSION", Value: t, Path: "/komga",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// sse holds an event-stream open so clients don't error-loop on reconnects;
// kaodoku emits no Komga domain events.
func (k *komga) sse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

func (k *komga) writeBooksPage(w http.ResponseWriter, r *http.Request, books []komgaBook) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = 20
	}
	unpaged := q.Get("unpaged") == "true"
	total := len(books)
	window := books
	if !unpaged {
		lo := page * size
		if lo > total {
			lo = total
		}
		hi := lo + size
		if hi > total {
			hi = total
		}
		window = books[lo:hi]
	}
	komgaWrite(w, komgaPageOf(window, total, page, size, unpaged))
}

// booksListPost is Komga's current book-search endpoint. The condition tree
// is reduced to the constraints clients actually send: seriesId (chapter
// lists) and readStatus, nested under allOf/anyOf.
func (k *komga) booksListPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Condition      json.RawMessage `json:"condition"`
		FullTextSearch string          `json:"fullTextSearch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cond := parseKomgaCondition(body.Condition)

	ctx := r.Context()
	var books []komgaBook
	if len(cond.seriesIDs) > 0 {
		if tq, ok := visibleTitles(ctx); ok {
			tq.IDs = []int64{}
			for id := range cond.seriesIDs {
				if n, err := strconv.ParseInt(id, 10, 64); err == nil {
					tq.IDs = append(tq.IDs, n)
				}
			}
			titles, _ := k.svc.PageTitles(ctx, tq)
			for _, t := range titles {
				books = append(books, k.seriesBookList(r, t)...)
			}
		}
	} else {
		books = k.recentBooks(r)
	}
	if books == nil {
		books = []komgaBook{}
	}
	if cond.readStatus != nil {
		kept := books[:0]
		for _, b := range books {
			state := "UNREAD"
			switch {
			case b.ReadProgress != nil && b.ReadProgress.Completed:
				state = "READ"
			case b.ReadProgress != nil:
				state = "IN_PROGRESS"
			}
			if cond.readStatus[state] {
				kept = append(kept, b)
			}
		}
		books = kept
	}
	if q := strings.TrimSpace(body.FullTextSearch); q != "" {
		kept := books[:0]
		for _, b := range books {
			if strings.Contains(strings.ToLower(b.Name), strings.ToLower(q)) ||
				strings.Contains(strings.ToLower(b.SeriesTitle), strings.ToLower(q)) {
				kept = append(kept, b)
			}
		}
		books = kept
	}
	sortKomgaBooks(books, r.URL.Query().Get("sort"))
	k.writeBooksPage(w, r, books)
}

type komgaCondition struct {
	seriesIDs  map[string]bool
	readStatus map[string]bool
}

// parseKomgaCondition walks the polymorphic condition tree collecting the
// supported constraints; unknown condition types are ignored.
func parseKomgaCondition(raw json.RawMessage) komgaCondition {
	out := komgaCondition{}
	var walk func(json.RawMessage)
	walk = func(node json.RawMessage) {
		if len(node) == 0 {
			return
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(node, &m) != nil {
			return
		}
		for key, val := range m {
			switch key {
			case "allOf", "anyOf":
				var list []json.RawMessage
				if json.Unmarshal(val, &list) == nil {
					for _, item := range list {
						walk(item)
					}
				}
			case "seriesId":
				var c struct {
					Operator string `json:"operator"`
					Value    string `json:"value"`
				}
				if json.Unmarshal(val, &c) == nil && c.Value != "" && c.Operator != "isNot" {
					if out.seriesIDs == nil {
						out.seriesIDs = map[string]bool{}
					}
					out.seriesIDs[c.Value] = true
				}
			case "readStatus":
				var c struct {
					Operator string `json:"operator"`
					Value    string `json:"value"`
				}
				if json.Unmarshal(val, &c) == nil && c.Value != "" && c.Operator != "isNot" {
					if out.readStatus == nil {
						out.readStatus = map[string]bool{}
					}
					out.readStatus[c.Value] = true
				}
			}
		}
	}
	walk(raw)
	return out
}

// recentBooks approximates a library-wide book query from the most recently
// updated titles. ponytail: caps at 30 titles to bound the per-request
// queries; a chapters-table index query would be exact.
func (k *komga) recentBooks(r *http.Request) []komgaBook {
	tq, ok := visibleTitles(r.Context())
	if !ok {
		return nil
	}
	tq.Sort, tq.Dir, tq.Limit = "updated", "desc", 30
	recent, _ := k.svc.PageTitles(r.Context(), tq)
	var books []komgaBook
	for _, t := range recent {
		books = append(books, k.seriesBookList(r, t)...)
	}
	return books
}

func sortKomgaBooks(books []komgaBook, sortParam string) {
	desc := strings.HasSuffix(sortParam, ",desc")
	byCreated := strings.Contains(sortParam, "createdDate") || strings.Contains(sortParam, "lastModified")
	sort.SliceStable(books, func(i, j int) bool {
		var less bool
		if byCreated {
			less = books[i].Created < books[j].Created
		} else if books[i].SeriesTitle != books[j].SeriesTitle {
			less = books[i].SeriesTitle < books[j].SeriesTitle
		} else {
			less = books[i].Metadata.NumberSort < books[j].Metadata.NumberSort
		}
		if desc {
			return !less
		}
		return less
	})
}

func (k *komga) allowedTitle(w http.ResponseWriter, r *http.Request) (library.Title, bool) {
	id, err := parseInt64Path(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return library.Title{}, false
	}
	t, err := k.svc.GetTitle(r.Context(), id)
	if err != nil || !contentAllowed(r.Context(), t.IsAdult, t.ContentTags) {
		writeError(w, http.StatusNotFound, "series not found")
		return library.Title{}, false
	}
	return t, true
}

// seriesBookList returns a title's books: downloaded chapters, or volumes
// when the title has no chapters, sorted by numberSort.
func (k *komga) seriesBookList(r *http.Request, t library.Title) []komgaBook {
	ctx := r.Context()
	out := []komgaBook{}
	statuses, _ := k.svc.TitleReadStatuses(ctx, t.ID)
	for _, st := range statuses {
		if st.Downloaded && st.OutputFile != "" {
			out = append(out, k.chapterBook(t, st))
		}
	}
	if len(out) == 0 {
		vols, _ := k.svc.Volumes(ctx, t.ID)
		for _, v := range vols {
			if v.File != "" {
				out = append(out, k.volumeBook(t, v))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Metadata.NumberSort < out[j].Metadata.NumberSort
	})
	return out
}

func chapterNumberSort(ch library.ChapterReadStatus) float64 {
	if f, err := strconv.ParseFloat(ch.Label, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f
	}
	return float64(ch.NumberMain)
}

func (k *komga) chapterBook(t library.Title, ch library.ChapterReadStatus) komgaBook {
	id := "c" + strconv.FormatInt(ch.ID, 10)
	created := komgaTime(ch.DiscoveredAt)
	modified := komgaTime(ch.UpdatedAt)
	name := "Ch " + ch.Label
	if ch.Title != "" {
		name += " · " + ch.Title
	}
	var progress *komgaReadProgress
	if ch.Completed || ch.LastPage > 0 {
		at := komgaTime(t.UpdatedAt)
		if ch.LastReadAt != nil {
			at = komgaTime(*ch.LastReadAt)
		}
		page := ch.LastPage
		if page < 1 {
			page = 1
		}
		progress = &komgaReadProgress{
			Page: page, Completed: ch.Completed,
			ReadDate: at, Created: at, LastModified: at,
		}
	}
	return komgaBook{
		ID: id, SeriesID: strconv.FormatInt(t.ID, 10), SeriesTitle: t.DisplayTitle,
		LibraryID: komgaLibraryID, Name: name, URL: "",
		Number: ch.NumberMain, Created: created, LastModified: modified,
		FileLastModified: modified, SizeBytes: ch.Bytes, Size: util.Human(ch.Bytes),
		Media: komgaMedia{
			Status: "READY", MediaType: "application/zip", PagesCount: ch.Pages,
			MediaProfile: "DIVINA",
		},
		Metadata: komgaBookMetadata{
			Title: name, Summary: "", Number: ch.Label,
			NumberSort: chapterNumberSort(ch),
			Authors:    []komgaAuthor{}, Tags: []string{}, Links: []komgaLink{},
			Created: created, LastModified: modified,
		},
		ReadProgress: progress,
	}
}

func (k *komga) volumeBook(t library.Title, v library.Volume) komgaBook {
	id := "v" + strconv.FormatInt(v.ID, 10)
	stamp := komgaTime(t.UpdatedAt)
	name := "Vol " + strconv.FormatFloat(v.Number, 'f', -1, 64)
	if v.Name != "" {
		name += " · " + v.Name
	}
	var progress *komgaReadProgress
	if v.Read || v.LastPage > 0 {
		page := v.LastPage
		if page < 1 {
			page = 1
		}
		progress = &komgaReadProgress{
			Page: page, Completed: v.Read,
			ReadDate: stamp, Created: stamp, LastModified: stamp,
		}
	}
	return komgaBook{
		ID: id, SeriesID: strconv.FormatInt(t.ID, 10), SeriesTitle: t.DisplayTitle,
		LibraryID: komgaLibraryID, Name: name, URL: "",
		Number: int(v.Number), Created: stamp, LastModified: stamp,
		FileLastModified: stamp, SizeBytes: v.Bytes, Size: util.Human(v.Bytes),
		Media: komgaMedia{
			Status: "READY", MediaType: "application/zip", PagesCount: v.Pages,
			MediaProfile: "DIVINA",
		},
		Metadata: komgaBookMetadata{
			Title: name, Summary: "",
			Number:     strconv.FormatFloat(v.Number, 'f', -1, 64),
			NumberSort: v.Number,
			Authors:    []komgaAuthor{}, Tags: []string{}, Links: []komgaLink{},
			Created: stamp, LastModified: stamp,
		},
		ReadProgress: progress,
	}
}

func (k *komga) seriesGet(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	books := k.seriesBookList(r, t)
	read, inProgress := 0, 0
	for _, b := range books {
		switch {
		case b.ReadProgress != nil && b.ReadProgress.Completed:
			read++
		case b.ReadProgress != nil:
			inProgress++
		}
	}
	komgaWrite(w, k.toSeries(t, read, inProgress, len(books)))
}

func (k *komga) seriesBooks(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	k.writeBooksPage(w, r, k.seriesBookList(r, t))
}

func (k *komga) seriesThumbnail(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	serveTitleCover(w, r, k.svc, t.ID)
}

// bookRef resolves a "c<id>"/"v<id>" book id to its CBZ, page count, and
// title, enforcing content guards.
type bookRef struct {
	volume  bool
	id      int64
	titleID int64
	file    string
	pages   int
}

func (k *komga) resolveBook(r *http.Request) (bookRef, error) {
	raw := r.PathValue("id")
	if len(raw) < 2 {
		return bookRef{}, fmt.Errorf("invalid id")
	}
	id, err := strconv.ParseInt(raw[1:], 10, 64)
	if err != nil {
		return bookRef{}, fmt.Errorf("invalid id")
	}
	ctx := r.Context()
	switch raw[0] {
	case 'c':
		st, err := k.svc.ChapterReadStatus(ctx, id)
		if err != nil || !titleAllowed(ctx, k.svc, st.TitleID) {
			return bookRef{}, fmt.Errorf("not found")
		}
		if !st.Downloaded || st.OutputFile == "" {
			return bookRef{}, fmt.Errorf("not found")
		}
		return bookRef{id: id, titleID: st.TitleID, file: st.OutputFile, pages: st.Pages}, nil
	case 'v':
		v, err := k.svc.GetVolume(ctx, id)
		if err != nil || !titleAllowed(ctx, k.svc, v.TitleID) || v.File == "" {
			return bookRef{}, fmt.Errorf("not found")
		}
		return bookRef{volume: true, id: id, titleID: v.TitleID, file: v.File, pages: v.Pages}, nil
	}
	return bookRef{}, fmt.Errorf("invalid id")
}

func (k *komga) bookGet(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	t, err := k.svc.GetTitle(r.Context(), ref.titleID)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	if ref.volume {
		v, err := k.svc.GetVolume(r.Context(), ref.id)
		if err != nil {
			writeError(w, http.StatusNotFound, "book not found")
			return
		}
		komgaWrite(w, k.volumeBook(t, v))
		return
	}
	st, err := k.svc.ChapterReadStatus(r.Context(), ref.id)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	komgaWrite(w, k.chapterBook(t, st))
}

func (k *komga) bookPages(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	zr, err := zip.OpenReader(ref.file)
	if err != nil {
		writeError(w, http.StatusNotFound, "archive not available")
		return
	}
	defer zr.Close()
	entries := util.CBZImageEntries(zr.File)
	out := make([]komgaPageDto, 0, len(entries))
	for i, e := range entries {
		mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(e.Name)))
		if mt == "" {
			mt = "image/jpeg"
		}
		dto := komgaPageDto{
			Number: i + 1, FileName: filepath.Base(e.Name), MediaType: mt,
			SizeBytes: int64(e.UncompressedSize64), Size: util.Human(int64(e.UncompressedSize64)),
		}
		if rc, err := e.Open(); err == nil {
			if cfg, _, err := image.DecodeConfig(rc); err == nil {
				dto.Width, dto.Height = cfg.Width, cfg.Height
			}
			rc.Close()
		}
		out = append(out, dto)
	}
	komgaWrite(w, out)
}

func (k *komga) bookPage(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	page, err := strconv.Atoi(r.PathValue("page"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	if r.URL.Query().Get("zero_based") == "true" {
		page++
	}
	if page < 1 {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	k.servePage(w, r, ref, page)
}

func (k *komga) servePage(w http.ResponseWriter, r *http.Request, ref bookRef, page int) {
	r.SetPathValue("id", strconv.FormatInt(ref.id, 10))
	r.SetPathValue("page", strconv.Itoa(page))
	if ref.volume {
		serveVolumePage(w, r, k.svc)
		return
	}
	serveChapterPage(w, r, k.svc)
}

func (k *komga) bookThumbnail(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	k.servePage(w, r, ref, 1)
}

func (k *komga) bookFile(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	serveArchive(w, r, ref.file)
}

func (k *komga) bookReadProgress(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	var body struct {
		Page      *int  `json:"page"`
		Completed *bool `json:"completed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	ctx := r.Context()
	completed := body.Completed != nil && *body.Completed
	if body.Page != nil && *body.Page < 1 {
		writeError(w, http.StatusBadRequest, "page must be positive")
		return
	}
	if body.Page != nil && !completed && *body.Page >= ref.pages && ref.pages > 0 {
		completed = true // Komga derives completion from position
	}
	err = nil
	switch {
	case ref.volume && completed:
		err = k.svc.SetVolumeRead(ctx, ref.id, true)
	case ref.volume && body.Page != nil:
		_, err = k.svc.MarkVolumePageRead(ctx, ref.id, *body.Page, ref.pages)
	case completed:
		_, err = k.svc.MarkChapterRead(ctx, ref.id)
	case body.Page != nil:
		_, err = k.svc.MarkPageRead(ctx, ref.id, *body.Page, ref.pages)
	default:
		writeError(w, http.StatusBadRequest, "page or completed required")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if completed {
		k.svc.PushAniListEntry(ctx, auth.UserID(ctx), ref.titleID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (k *komga) bookReadProgressDelete(w http.ResponseWriter, r *http.Request) {
	ref, err := k.resolveBook(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ctx := r.Context()
	if ref.volume {
		_ = k.svc.SetVolumeRead(ctx, ref.id, false)
	} else {
		_, _ = k.svc.MarkChapterUnread(ctx, ref.id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// tachiyomiProgress serves Mihon's enhanced-tracker view of a series:
// lastReadContinuousNumberSort is the highest numberSort with every book up
// to it read.
func (k *komga) tachiyomiProgress(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	books := k.seriesBookList(r, t)
	read, inProgress := 0, 0
	continuous, maxSort := 0.0, 0.0
	streak := true
	for _, b := range books {
		if b.Metadata.NumberSort > maxSort {
			maxSort = b.Metadata.NumberSort
		}
		done := b.ReadProgress != nil && b.ReadProgress.Completed
		if done {
			read++
		} else if b.ReadProgress != nil {
			inProgress++
		}
		if streak && done {
			continuous = b.Metadata.NumberSort
		} else {
			streak = false
		}
	}
	unread := len(books) - read - inProgress
	komgaWrite(w, map[string]any{
		"booksCount":                   len(books),
		"booksReadCount":               read,
		"booksUnreadCount":             unread,
		"booksInProgressCount":         inProgress,
		"lastReadContinuousNumberSort": continuous,
		"maxNumberSort":                maxSort,
	})
}

func (k *komga) tachiyomiProgressPut(w http.ResponseWriter, r *http.Request) {
	t, ok := k.allowedTitle(w, r)
	if !ok {
		return
	}
	var body struct {
		LastBookNumberSortRead float64 `json:"lastBookNumberSortRead"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	ctx := r.Context()
	for _, b := range k.seriesBookList(r, t) {
		if float32(b.Metadata.NumberSort) > float32(body.LastBookNumberSortRead) {
			continue
		}
		if b.ReadProgress != nil && b.ReadProgress.Completed {
			continue
		}
		if strings.HasPrefix(b.ID, "v") {
			id, _ := strconv.ParseInt(b.ID[1:], 10, 64)
			_ = k.svc.SetVolumeRead(ctx, id, true)
		} else {
			id, _ := strconv.ParseInt(b.ID[1:], 10, 64)
			_, _ = k.svc.MarkChapterRead(ctx, id)
		}
	}
	k.svc.PushAniListEntry(ctx, auth.UserID(ctx), t.ID)
	w.WriteHeader(http.StatusNoContent)
}
