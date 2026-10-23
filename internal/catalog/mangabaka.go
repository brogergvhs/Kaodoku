package catalog

import (
	"cmp"
	"maps"
	"strconv"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/mangabaka"
)

const MangaBakaProvider = "mangabaka"

var mangaBakaStatus = map[string]string{"releasing": "RELEASING", "completed": "FINISHED", "cancelled": "CANCELLED", "hiatus": "HIATUS", "upcoming": "NOT_YET_RELEASED"}

// FromMangaBaka builds a record of a MangaBaka series under one provider's id, carrying every id it is known by.
func FromMangaBaka(sr mangabaka.Series, provider, id string) Manga {
	m := Manga{
		Provider: provider, ProviderID: id,
		TitleRomaji: cmp.Or(sr.Romanized, sr.Title), TitleEnglish: sr.English, TitleNative: sr.Native,
		Description: sr.Description, CoverImage: sr.CoverURL(350), Status: mangaBakaStatus[sr.Status], Format: "MANGA",
		Synonyms: sr.Titles, Genres: sr.Genres, Tags: sr.Tags, Authors: sr.Authors, Year: sr.Year,
		AverageScore: int(sr.Rating), IsAdult: sr.ContentRating == "pornographic",
		ExternalIDs: map[string]string{MangaBakaProvider: strconv.FormatInt(sr.ID, 10)},
	}
	if sr.Type == "novel" {
		m.Format = "NOVEL"
	}
	if sr.Chapters > 0 {
		m.Chapters = &sr.Chapters
	}
	if sr.Volumes > 0 {
		m.Volumes = &sr.Volumes
	}
	switch m.RelationType = strings.ToUpper(sr.Relation); m.RelationType {
	case "MAIN":
		m.RelationType = "PARENT"
	case "CHARACTER_FOCUS":
		m.RelationType = "CHARACTER"
	}
	maps.Copy(m.ExternalIDs, sr.IDs)
	return m
}
