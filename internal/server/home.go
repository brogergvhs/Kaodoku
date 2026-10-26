package server

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/brogergvhs/kaodoku/internal/auth"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
)

const homeRowLimit = 12

type homeCard struct {
	Href    string
	Cover   string
	Banner  string
	Title   string
	Sub     string
	Percent int64 // -1 hides the bar
}

type homeRow struct {
	Heading string
	Link    string
	Cards   []homeCard
}

type homeView struct {
	User    *auth.User
	Hero    *homeCard
	Eyebrow string
	Action  string
	Rows    []homeRow
}

func (u *webUI) homePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := homeView{User: userFrom(ctx)}
	if base, ok := visibleTitles(ctx); ok && view.User.Can(auth.PermLibraryView) {
		view.Rows = u.homeRows(ctx, base)
	}
	if len(view.Rows) > 0 {
		row := view.Rows[0]
		view.Hero, view.Eyebrow, view.Action = &row.Cards[0], row.Heading, "Open"
		if row.Link == "" {
			view.Eyebrow, view.Action = "Pick up where you left off", "Continue reading"
		}
	}
	u.page(w, r, "home", "Home", view)
}

func (u *webUI) homeRows(ctx context.Context, base library.TitleQuery) []homeRow {
	var rows []homeRow
	add := func(row homeRow) {
		if len(row.Cards) > 0 {
			rows = append(rows, row)
		}
	}
	for k := homeRowLimit * 4; ; k *= 4 {
		lastRead, more, _ := u.svc.LastReadAt(ctx, k)
		row := continueReadingRow(visibleByIDs(ctx, u.svc, base, lastRead), lastRead)
		if len(row.Cards) >= homeRowLimit || !more {
			add(row)
			break
		}
	}
	newest := base
	newest.Sort, newest.Dir, newest.Limit = "added", "desc", homeRowLimit
	recent, _ := u.svc.PageTitles(ctx, newest)
	arrivalsRow := latestArrivalsRow("Latest arrivals", recent, nil)
	arrivalsRow.Link = "/library?sort=added&dir=desc"
	add(arrivalsRow)
	for k := homeRowLimit * 4; ; k *= 4 {
		arrivals, more, _ := u.svc.LatestArrivals(ctx, k)
		row := latestUpdatesRow("Latest updates", visibleByIDs(ctx, u.svc, base, arrivals), arrivals, nil)
		row.Link = "/library?sort=updated&dir=desc"
		if len(row.Cards) >= homeRowLimit || !more {
			add(row)
			break
		}
	}
	if genre, _ := u.svc.TopGenre(ctx, base); genre != "" {
		newest.IncludeAny = append(append([][]string{}, base.IncludeAny...), []string{genre})
		inGenre, _ := u.svc.PageTitles(ctx, newest)
		row := latestArrivalsRow("Latest in "+genre, inGenre, nil)
		row.Link = "/library?" + url.Values{"include_tags": {genre}, "sort": {"added"}, "dir": {"desc"}}.Encode()
		add(row)
	}
	return rows
}

func visibleByIDs[V any](ctx context.Context, svc *service.JobService, base library.TitleQuery, byID map[int64]V) []library.Title {
	base.IDs = make([]int64, 0, len(byID))
	for id := range byID {
		base.IDs = append(base.IDs, id)
	}
	titles, _ := svc.PageTitles(ctx, base)
	return titles
}

type timedCard struct {
	card homeCard
	at   time.Time
}

// topCards sorts newest-first and keeps up to homeRowLimit cards.
func topCards(entries []timedCard) []homeCard {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
	out := make([]homeCard, 0, homeRowLimit)
	for i, e := range entries {
		if i == homeRowLimit {
			break
		}
		out = append(out, e.card)
	}
	return out
}

// continueReadingRow lists titles with partial progress, most recent first.
func continueReadingRow(titles []library.Title, lastRead map[int64]time.Time) homeRow {
	var entries []timedCard
	for _, t := range titles {
		chapters := t.ReadCount > 0 && t.ReadCount < t.DiscoveredCount
		volumes := t.VolumeReadCount > 0 && t.VolumeReadCount < t.VolumeCount
		if !chapters && !volumes {
			continue
		}
		at, ok := lastRead[t.ID]
		if !ok {
			continue
		}
		card := homeCard{Cover: t.CoverImage, Banner: t.BannerImage, Title: t.DisplayTitle}
		id := strconv.FormatInt(t.ID, 10)
		if chapters {
			card.Href = "/reader/" + id
			card.Sub = strconv.FormatInt(t.ReadCount, 10) + "/" + strconv.FormatInt(t.DiscoveredCount, 10) + " chapters"
			card.Percent = pctInt(t.ReadCount, t.DiscoveredCount)
		} else {
			card.Href = "/reader/" + id + "?mode=volumes"
			card.Sub = strconv.FormatInt(t.VolumeReadCount, 10) + "/" + strconv.FormatInt(t.VolumeCount, 10) + " volumes"
			card.Percent = pctInt(t.VolumeReadCount, t.VolumeCount)
		}
		entries = append(entries, timedCard{card: card, at: at})
	}
	return homeRow{Heading: "Continue reading", Cards: topCards(entries)}
}

// latestArrivalsRow lists titles by when they were added; `only` restricts the set.
func latestArrivalsRow(heading string, titles []library.Title, only map[int64]bool) homeRow {
	var entries []timedCard
	for _, t := range titles {
		if only != nil && !only[t.ID] {
			continue
		}
		sub := "Added"
		if rel := since(t.CreatedAt); rel != "" {
			sub = "Added " + rel
		}
		entries = append(entries, timedCard{at: t.CreatedAt, card: homeCard{
			Href:    "/library/" + strconv.FormatInt(t.ID, 10),
			Cover:   t.CoverImage,
			Banner:  t.BannerImage,
			Title:   t.DisplayTitle,
			Sub:     sub,
			Percent: -1,
		}})
	}
	return homeRow{Heading: heading, Cards: topCards(entries)}
}

// latestUpdatesRow lists titles by their newest downloaded chapter or volume;
// `only` restricts the set.
func latestUpdatesRow(heading string, titles []library.Title, arrivals map[int64]library.Arrival, only map[int64]bool) homeRow {
	var entries []timedCard
	for _, t := range titles {
		if only != nil && !only[t.ID] {
			continue
		}
		arrival, ok := arrivals[t.ID]
		if !ok {
			continue
		}
		entries = append(entries, timedCard{at: arrival.At, card: homeCard{
			Href:    "/library/" + strconv.FormatInt(t.ID, 10),
			Cover:   t.CoverImage,
			Banner:  t.BannerImage,
			Title:   t.DisplayTitle,
			Sub:     arrival.Label,
			Percent: -1,
		}})
	}
	return homeRow{Heading: heading, Cards: topCards(entries)}
}

func pctInt(part, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return part * 100 / total
}
