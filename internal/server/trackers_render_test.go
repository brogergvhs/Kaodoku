package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/service"
)

func TestTrackerTemplatesRender(t *testing.T) {
	u := &webUI{}
	u.tmpl = template.Must(template.New("").Funcs(u.funcs()).ParseFS(templateFS, "templates/*.html"))
	render := func(name string, data any) string {
		var buf bytes.Buffer
		if err := u.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return buf.String()
	}
	expect := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Fatalf("missing %q in:\n%s", w, out)
			}
		}
	}

	cards := render("trackerCards", trackersView{CanConnect: true, Cards: []trackerCardView{
		{Provider: "anilist", Label: "AniList", Login: "oauth", Account: &service.TrackerAccount{Provider: "anilist", Label: "AniList", Name: "me", LastError: "boom"}},
		{Provider: "mal", Label: "MyAnimeList", Login: "oauth"},
		{Provider: "kitsu", Label: "Kitsu", Login: "password"},
		{Provider: "mangaupdates", Label: "MangaUpdates"},
	}})
	expect(cards, `AniList <span class="font-normal text-base-content/50">(me)</span>`, "boom", "/ui/trackers/anilist/sync", "/ui/trackers/anilist/settings",
		"/ui/trackers/anilist/disconnect", `<option value="mal">MyAnimeList</option>`, "/trackers/mal/connect", "/ui/trackers/kitsu/login", `type="password"`)
	if strings.Contains(cards, `value="anilist">`) || strings.Contains(cards, `value="mangaupdates"`) || strings.Contains(cards, "/library\"") {
		t.Fatalf("connect picker offers connected or unconfigured trackers:\n%s", cards)
	}
	if empty := render("trackerCards", trackersView{Cards: []trackerCardView{{Provider: "anilist", Label: "AniList", Login: "oauth", Account: &service.TrackerAccount{Provider: "anilist"}}}}); strings.Contains(empty, "tracker_connect_modal.showModal") {
		t.Fatal("Connect button shown with nothing left to connect")
	}
	expect(render("trackerCards", trackersView{CanConnect: true, Cards: []trackerCardView{{Provider: "kitsu", Label: "Kitsu", Login: "password"}}}), "No trackers here yet.", "tracker_connect_modal.showModal()")
	down := render("trackerCards", trackersView{Cards: []trackerCardView{
		{Provider: "mal", Label: "MyAnimeList", Login: "oauth", Outage: "maintenance", Account: &service.TrackerAccount{Provider: "mal", Label: "MyAnimeList", Name: "me"}},
		{Provider: "kitsu", Label: "Kitsu", Login: "password", Outage: "down"},
	}})
	expect(down, "Temporarily unavailable: maintenance")
	if strings.Contains(down, "/ui/trackers/mal/sync") || strings.Contains(down, `value="kitsu"`) {
		t.Fatalf("unavailable tracker still offers sync or connect:\n%s", down)
	}
	prefs := render("trackerPrefs", trackerPrefsView{
		TrackerAccount: service.TrackerAccount{Provider: "mal", Label: "MyAnimeList", TrackerPrefs: service.TrackerPrefs{AutoLink: true, Recommendations: true}},
		Unlinked:       []service.UnlinkedTitle{{Title: library.Title{ID: 7, DisplayTitle: "Vagabond"}, Suggested: true}},
	})
	expect(prefs, `id="tp-auto_link" name="auto_link" value="true" checked`, `id="tp-auto_progressive" name="auto_progressive" value="true" >`, "/library/7", "Vagabond", "suggested match", "data-filter-row")

	panel := render("providerPanel", providerPanelView{TitleID: 3, CanManage: true, Links: []service.ProviderLink{
		{Provider: "anilist", Label: "AniList", ID: "1", Primary: true, Confirmed: true, Connected: true, Syncing: true, URL: "https://anilist.co/manga/1"},
		{Provider: "mangaupdates", Label: "MangaUpdates", ID: "55", Method: "title_match"},
		{Provider: "kitsu", Label: "Kitsu", Method: "user"},
		{Provider: "mal", Label: "MyAnimeList", ID: "2", Confirmed: true, Method: "cross_link", Cover: "https://c/2.jpg"},
	}})
	expect(panel, `data-tip="Metadata source"`, `data-tip="Progressive sync"`, "Stop progressive sync", `data-tip="Suggested match"`, "Confirm match", `data-tip="Not linked"`, `data-tip="Linked"`, `"mode":"primary"`, "Use for metadata", "Relink", "Unlink",
		`"provider":"mal","provider_id":"2","mode":"primary"`, `"provider":"mal","provider_id":""`, "Connect MyAnimeList", "https://c/2.jpg", "openPicker('3-mal')", `id="picker-3-kitsu"`, `hx-include="#picker-form-3-mal"`, "Relink MyAnimeList")

	picker := render("providerPicker", providerPickerView{TitleID: 3, Provider: "mal", Label: "MyAnimeList", Query: "Berserk", Current: "9",
		Results: []catalog.Manga{{Provider: "mal", ProviderID: "9", TitleRomaji: "Berserk 2"}, {Provider: "mal", ProviderID: "2", TitleRomaji: "Berserk", Format: "MANGA"}}})
	expect(picker, "/ui/library/3/providers/set", `"provider_id":"2"`, "https://myanimelist.net/manga/2")
	if strings.Contains(picker, `"provider_id":"9"`) || strings.Contains(picker, "<select") {
		t.Fatalf("picker should not offer the linked entry or a provider select:\n%s", picker)
	}

	blocks := render("providerBlocks", map[string]any{"Options": []providerOption{{"anilist", "AniList"}, {"kitsu", "Kitsu"}}, "Blocked": []string{"kitsu"}})
	if !strings.Contains(blocks, `value="kitsu" checked`) || strings.Contains(blocks, `value="anilist" checked`) {
		t.Fatalf("blocked boxes wrong:\n%s", blocks)
	}

	results := render("mangaResults", mangaResults{View: "cards", CanAdd: true, Items: []searchResultView{{Manga: catalog.Manga{Provider: "mangaupdates", ProviderID: "55", TitleRomaji: "Solo"}}}})
	expect(results, `href="/manga/mangaupdates/55"`, `"provider":"mangaupdates"`)
	if strings.Contains(results, "left-2 top-2") {
		t.Fatal("single-backend list shows provider badges")
	}
	mixed := render("mangaResults", mangaResults{View: "cards", ShowProvider: true, Items: []searchResultView{
		{Manga: catalog.Manga{Provider: "mangaupdates", ProviderID: "55"}}, {Manga: catalog.Manga{Provider: "anilist", ProviderID: "1"}}}})
	expect(mixed, ">mangaupdates</span>", ">anilist</span>")

	queued := render("matches", matchView{DomID: "s", PollURL: "/p", Active: true, Queued: true})
	if !strings.Contains(queued, "Search queued") || strings.Contains(queued, "loading-spinner") || !strings.Contains(queued, `hx-get="/p"`) {
		t.Fatalf("queued source search:\n%s", queued)
	}
	expect(render("matches", matchView{DomID: "s", PollURL: "/p", Active: true}), "Searching sources…", "loading-spinner")

	banner := render("titleBanner", map[string]string{"Image": "", "Cover": "cover.jpg"})
	expect(banner, `src="cover.jpg"`, "object-cover")
}
