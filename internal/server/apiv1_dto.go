package server

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/chapters"
	"github.com/brogergvhs/kaodoku/internal/jobs"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
	"github.com/brogergvhs/kaodoku/internal/sources"
	"github.com/brogergvhs/kaodoku/internal/util"
)

type titleDTO struct {
	ID              int64      `json:"id"`
	SourceID        string     `json:"source_id"`
	CatalogMangaID  *int64     `json:"catalog_manga_id,omitempty"`
	SourceURL       string     `json:"source_url"`
	DisplayTitle    string     `json:"display_title"`
	CoverImage      string     `json:"cover_image"`
	BannerImage     string     `json:"banner_image,omitempty"`
	Monitored       bool       `json:"monitored"`
	RefreshInterval string     `json:"refresh_interval"`
	ReleaseStatus   string     `json:"release_status"`
	IsAdult         bool       `json:"is_adult"`
	AverageScore    int64      `json:"average_score"`
	ContentTags     []string   `json:"content_tags"`
	Favourite       bool       `json:"favourite"`
	LanguageMode    string     `json:"language_mode"`
	LanguageGap     int64      `json:"language_gap"`
	DiscoveredCount int64      `json:"discovered_count"`
	MissingCount    int64      `json:"missing_count"`
	FailedCount     int64      `json:"failed_count"`
	ReadCount       int64      `json:"read_count"`
	CompletedCount  int64      `json:"completed_count"`
	SizeBytes       int64      `json:"size_bytes"`
	Pages           int64      `json:"pages"`
	VolumeCount     int64      `json:"volume_count"`
	VolumeReadCount int64      `json:"volume_read_count"`
	VolumeBytes     int64      `json:"volume_bytes"`
	VolumePages     int64      `json:"volume_pages"`
	LastRefreshedAt *time.Time `json:"last_refreshed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// titleCoverPath keeps the version query on custom covers so cached clients
// refetch after an upload.
func titleCoverPath(t library.Title) string {
	if t.CustomCover {
		return t.CoverImage
	}
	return fmt.Sprintf("/api/v1/covers/%d", t.ID)
}

func toTitleDTO(t library.Title) titleDTO {
	tags := t.ContentTags
	if tags == nil {
		tags = []string{}
	}
	banner := ""
	if t.BannerImage != "" {
		banner = fmt.Sprintf("/api/v1/banners/%d", t.ID)
	}
	return titleDTO{
		ID: t.ID, SourceID: t.SourceID, CatalogMangaID: t.CatalogMangaID, SourceURL: t.SourceURL,
		DisplayTitle: t.DisplayTitle, CoverImage: titleCoverPath(t), BannerImage: banner,
		Monitored: t.Monitored, RefreshInterval: t.RefreshInterval, ReleaseStatus: t.ReleaseStatus,
		IsAdult: t.IsAdult, AverageScore: t.AverageScore, ContentTags: tags, Favourite: t.Favourite,
		LanguageMode: t.LanguageMode, LanguageGap: t.LanguageGap, DiscoveredCount: t.DiscoveredCount,
		MissingCount: t.MissingCount, FailedCount: t.FailedCount, ReadCount: t.ReadCount,
		CompletedCount: t.CompletedCount, SizeBytes: t.SizeBytes, Pages: t.Pages,
		VolumeCount: t.VolumeCount, VolumeReadCount: t.VolumeReadCount, VolumeBytes: t.VolumeBytes,
		VolumePages: t.VolumePages, LastRefreshedAt: t.LastRefreshedAt, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

type chapterProgressDTO struct {
	ID              int64      `json:"id"`
	TitleID         int64      `json:"title_id"`
	Label           string     `json:"label"`
	Title           string     `json:"title"`
	DisplayName     string     `json:"display_name"`
	DisplayVolume   string     `json:"display_volume,omitempty"`
	VolumeNum       float64    `json:"volume_num,omitempty"`
	NumberMain      int        `json:"number_main"`
	Downloaded      bool       `json:"downloaded"`
	Bytes           int64      `json:"bytes"`
	Pages           int        `json:"pages"`
	TotalPages      int        `json:"total_pages"`
	ReadPages       int        `json:"read_pages"`
	LastPage        int        `json:"last_page"`
	Completed       bool       `json:"completed"`
	Manual          bool       `json:"manual"`
	FirstUnreadPage int        `json:"first_unread_page"`
	LastReadAt      *time.Time `json:"last_read_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func toChapterProgressDTO(c library.ChapterReadStatus) chapterProgressDTO {
	d := chapters.DisplayName(c.Label, c.Title)
	return chapterProgressDTO{
		ID: c.ID, TitleID: c.TitleID, Label: c.Label, Title: c.Title, NumberMain: c.NumberMain,
		DisplayName: d.Name, DisplayVolume: d.Volume, VolumeNum: c.VolumeNum,
		Downloaded: c.Downloaded, Bytes: c.Bytes, Pages: c.Pages, TotalPages: c.TotalPages,
		ReadPages: c.ReadPages, LastPage: c.LastPage, Completed: c.Completed, Manual: c.Manual,
		FirstUnreadPage: c.FirstUnreadPage, LastReadAt: c.LastReadAt, CompletedAt: c.CompletedAt,
	}
}

// mangaDetailDTO is the catalog metadata the web title page renders
// (mangaDetail template): badges, description, authors, genres.
type mangaDetailDTO struct {
	Description  string   `json:"description,omitempty"`
	Status       string   `json:"status,omitempty"`
	Format       string   `json:"format,omitempty"`
	Year         int      `json:"year,omitempty"`
	Chapters     *int     `json:"chapters,omitempty"`
	Volumes      *int     `json:"volumes,omitempty"`
	Authors      []string `json:"authors,omitempty"`
	Genres       []string `json:"genres,omitempty"`
	AverageScore int      `json:"average_score,omitempty"`
}

func toMangaDetailDTO(m catalog.Manga) *mangaDetailDTO {
	return &mangaDetailDTO{
		Description: util.PlainDescription(m.Description), Status: m.Status, Format: m.Format, Year: m.Year,
		Chapters: m.Chapters, Volumes: m.Volumes, Authors: m.Authors, Genres: m.Genres,
		AverageScore: m.AverageScore,
	}
}

type titleReadProgressDTO struct {
	Title         titleDTO             `json:"title"`
	Rating        *service.TitleRating `json:"rating,omitempty"`
	Manga         *mangaDetailDTO      `json:"manga,omitempty"`
	Chapters      []chapterProgressDTO `json:"chapters"`
	ReadChapters  int                  `json:"read_chapters"`
	TotalChapters int                  `json:"total_chapters"`
	ReadPages     int64                `json:"read_pages"`
	TotalPages    int64                `json:"total_pages"`
	NextChapterID int64                `json:"next_chapter_id"`
	NextPage      int                  `json:"next_page"`
}

func toTitleReadProgressDTO(p library.TitleReadProgress) titleReadProgressDTO {
	chs := make([]chapterProgressDTO, len(p.Chapters))
	for i, c := range p.Chapters {
		chs[i] = toChapterProgressDTO(c)
	}
	return titleReadProgressDTO{
		Title: toTitleDTO(p.Title), Chapters: chs, ReadChapters: p.ReadChapters, TotalChapters: p.TotalChapters,
		ReadPages: p.ReadPages, TotalPages: p.TotalPages, NextChapterID: p.NextChapterID, NextPage: p.NextPage,
	}
}

type volumeDTO struct {
	ID          int64   `json:"id"`
	TitleID     int64   `json:"title_id"`
	Number      float64 `json:"number"`
	Name        string  `json:"name"`
	Pages       int     `json:"pages"`
	Bytes       int64   `json:"bytes"`
	CustomCover bool    `json:"custom_cover"`
	Read        bool    `json:"read"`
	ReadPages   int     `json:"read_pages"`
	LastPage    int     `json:"last_page"`
	CoverURL    string  `json:"cover_url"`
}

func toVolumeDTO(v library.Volume) volumeDTO {
	return volumeDTO{
		ID: v.ID, TitleID: v.TitleID, Number: v.Number, Name: v.Name, Pages: v.Pages, Bytes: v.Bytes,
		CustomCover: v.CustomCover, Read: v.Read, ReadPages: v.ReadPages, LastPage: v.LastPage,
		CoverURL: fmt.Sprintf("/api/v1/volumes/%d/cover", v.ID),
	}
}

type jobDTO struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error"`
	ParentID  int64     `json:"parent_id"`
	RunAfter  time.Time `json:"run_after"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	TitleID   int64     `json:"title_id,omitempty"`
	SourceID  string    `json:"source_id,omitempty"`
	CatalogID int64     `json:"catalog_id,omitempty"`
}

func toJobDTO(j jobs.Job) jobDTO {
	var p service.JobPayload
	_ = json.Unmarshal([]byte(j.Payload), &p)
	return jobDTO{
		ID: j.ID, Type: j.Type, Status: j.Status, Attempts: j.Attempts, LastError: j.LastError,
		ParentID: j.ParentID, RunAfter: j.RunAfter, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		TitleID: p.TitleID, SourceID: p.SourceID, CatalogID: p.CatalogID,
	}
}

type collectionDTO struct {
	ID        int64   `json:"id,omitempty"`
	Key       string  `json:"key,omitempty"`
	Name      string  `json:"name"`
	TitleIDs  []int64 `json:"title_ids"`
	PinnedIDs []int64 `json:"pinned_ids,omitempty"`
	Chapters  int64   `json:"chapters"`
	Volumes   int64   `json:"volumes"`
	Pages     int64   `json:"pages"`
	SizeBytes int64   `json:"size_bytes"`
	ReadPct   int64   `json:"read_pct"`
}

// toCollectionDTO aggregates member stats the same way the web card does.
func toCollectionDTO(c collection, pins map[string][]int64) collectionDTO {
	d := collectionDTO{ID: c.CustomID, Key: c.SmartKey, Name: c.Name,
		TitleIDs: make([]int64, 0, len(c.Members))}
	if pins != nil {
		d.PinnedIDs = pins[c.SmartKey]
	}
	var total, read int64
	for _, m := range c.Members {
		d.TitleIDs = append(d.TitleIDs, m.ID)
		d.Chapters += m.DiscoveredCount
		d.Volumes += m.VolumeCount
		d.Pages += m.Pages + m.VolumePages
		d.SizeBytes += m.SizeBytes + m.VolumeBytes
		total += m.DiscoveredCount + m.VolumeCount
		read += m.ReadCount + m.VolumeReadCount
	}
	d.ReadPct = percent(read, total)
	return d
}

type screenDTO struct {
	ID     int64                `json:"id"`
	Name   string               `json:"name"`
	Config library.ScreenConfig `json:"config"`
}

func toScreenDTO(s library.Screen) screenDTO {
	return screenDTO{ID: s.ID, Name: s.Name, Config: s.Config}
}

// sourcePickDTO is the minimal source view for users without sources.manage.
type sourcePickDTO struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

func toSourcePickDTO(s sources.Source) sourcePickDTO {
	return sourcePickDTO{ID: s.ID, Name: s.Name, Enabled: s.Enabled}
}

type userSettingsDTO struct {
	ReaderMode         string   `json:"reader_mode,omitempty"`
	ReaderDir          string   `json:"reader_dir,omitempty"`
	ReaderFit          string   `json:"reader_fit,omitempty"`
	ReaderZoom         *float64 `json:"reader_zoom,omitempty"`
	ReaderPageLayout   string   `json:"reader_page_layout,omitempty"`
	ReaderSplitWide    bool     `json:"reader_split_wide,omitempty"`
	ReaderImageQuality string   `json:"reader_image_quality,omitempty"`
	Theme              string   `json:"theme,omitempty"`
}
