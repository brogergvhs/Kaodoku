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
	down := render("trackerCards", trackersView{CanConnect: true, Cards: []trackerCardView{
		{Provider: "mal", Label: "MyAnimeList", Login: "oauth", Outage: "maintenance", Account: &service.TrackerAccount{Provider: "mal", Label: "MyAnimeList", Name: "me"}},
		{Provider: "kitsu", Label: "Kitsu", Login: "password", Outage: "down"},
	}})
	expect(down, "Temporarily unavailable: maintenance", `<option value="kitsu" disabled title="Temporarily unavailable: down">Kitsu (unavailable)</option>`)
	if strings.Contains(down, "/ui/trackers/mal/sync") {
		t.Fatalf("unavailable tracker still offers sync:\n%s", down)
	}
	downPanel := render("providerPanel", providerPanelView{TitleID: 4, CanManage: true, Links: []service.ProviderLink{{Provider: "mal", Label: "MyAnimeList", ID: "2", Confirmed: true, Connected: true, Outage: "maintenance"}}})
	expect(downPanel, "opacity-60", `data-tip="Temporarily unavailable: maintenance"`, `disabled aria-label="MyAnimeList is temporarily unavailable"`)
	if strings.Contains(downPanel, "Relink") || strings.Contains(downPanel, "Connect MyAnimeList") {
		t.Fatalf("unavailable tracker still offers actions:\n%s", downPanel)
	}
	prefs := render("trackerPrefs", service.TrackerAccount{Provider: "mal", Label: "MyAnimeList", TrackerPrefs: service.TrackerPrefs{AutoLink: true, Recommendations: true, Source: service.SourceKaodoku}})
	expect(prefs, `id="tp-auto_link" name="auto_link" value="true" checked`, `id="tp-auto_progressive" name="auto_progressive" value="true" >`,
		`name="kaodoku_leads" value="true" checked`, "Off: Kaodoku takes them from MyAnimeList")

	two := []trackerCardView{
		{Provider: "anilist", Label: "AniList", Login: "oauth", Account: &service.TrackerAccount{Provider: "anilist", Label: "AniList"}},
		{Provider: "kitsu", Label: "Kitsu", Login: "password", Account: &service.TrackerAccount{Provider: "kitsu", Label: "Kitsu"}},
	}
	two[0].Account.TrackerPrefs = service.TrackerPrefs{AutoLink: true, RatingSync: "on", Source: service.SourceTracker}
	two[1].Account.TrackerPrefs = service.TrackerPrefs{AutoProgressive: true, Recommendations: true, Source: service.SourceKaodoku}
	badges := render("trackerCards", trackersView{Cards: two})
	expect(badges, `data-tip="Links new titles automatically"`, `data-tip="AniList is the source of truth"`, `data-tip="Ratings sync, always"`,
		`data-tip="Progressive sync for linked titles"`, `data-tip="Kaodoku is the source of truth"`, `data-tip="Ratings sync, same as progressive sync"`, `data-tip="Uses its recommendations"`)
	if strings.Contains(badges, "Uses its related manga") {
		t.Fatal("badge shown for a setting that is off")
	}
	expect(render("trackerCards", trackersView{Cards: two, Reorder: true}), `data-reorder="/ui/trackers/reorder"`, `draggable="true" data-reorder-id="anilist"`, `data-reorder-id="kitsu"`)
	if fixed := render("trackerCards", trackersView{Cards: two}); strings.Contains(fixed, "data-reorder") {
		t.Fatal("trackers are draggable with fewer than two rating sources")
	}

	manage := render("trackerCards", trackersView{CanManage: true, Cards: []trackerCardView{
		{Provider: "mal", Label: "MyAnimeList", Login: "oauth", Unlinked: 4, Account: &service.TrackerAccount{Provider: "mal", Label: "MyAnimeList", Name: "me"}},
		{Provider: "kitsu", Label: "Kitsu", Login: "password", Unlinked: 2, Outage: "down", Account: &service.TrackerAccount{Provider: "kitsu", Label: "Kitsu", Name: "me"}},
		{Provider: "anilist", Label: "AniList", Login: "oauth", Account: &service.TrackerAccount{Provider: "anilist", Label: "AniList", Name: "me"}},
	}})
	expect(manage, `hx-get="/ui/trackers/mal/unlinked"`, `data-unlinked-count="mal">4</span>`, `id="tracker_unlinked_modal"`)
	if strings.Contains(manage, "/ui/trackers/kitsu/unlinked") || strings.Contains(manage, "/ui/trackers/anilist/unlinked") || strings.Contains(cards, "/unlinked") {
		t.Fatalf("unlinked titles offered for an unavailable tracker or without the manage permission:\n%s", manage)
	}
	unlinked := render("trackerUnlinked", unlinkedView{Provider: "mal", Label: "MyAnimeList", Titles: []service.UnlinkedTitle{{Title: library.Title{ID: 7, DisplayTitle: "Vagabond", CoverImage: "https://c/v.jpg", AverageScore: 91}, Suggested: true, Provider: "anilist", Format: "MANGA", Year: 1998}}})
	expect(unlinked, "Vagabond", "suggested match", `src="https://c/v.jpg"`, "anilist · MANGA · 1998 · ★ 91%", `hx-post="/ui/library/7/providers/search"`, `value="Vagabond"`, `name="unlinked" value="1"`, "toggle once from:closest details", "grid-cols-1")
	expect(render("trackerUnlinked", unlinkedView{Provider: "mal", Label: "MyAnimeList"}), "Every title is linked or marked as not on MyAnimeList.")
	expect(render("unlinkedLeft", unlinkedView{Provider: "mal"}), `hx-swap-oob="innerHTML:[data-unlinked-count=mal]">0</span>`, `hx-swap-oob="delete:[data-unlinked-item=mal]"`)
	if left := render("unlinkedLeft", unlinkedView{Provider: "mal", Titles: []service.UnlinkedTitle{{}}}); strings.Contains(left, "delete:") {
		t.Fatalf("menu item removed with titles still unlinked:\n%s", left)
	}
	rowPicker := render("providerPicker", providerPickerView{TitleID: 7, Provider: "mal", Unlinked: true, Results: []catalog.Manga{{Provider: "mal", ProviderID: "2", TitleRomaji: "Vagabond"}}})
	expect(rowPicker, `"provider":"mal","provider_id":"2","unlinked":"1"`, `"provider":"mal","provider_id":"","unlinked":"1"`, `hx-target="closest [data-unlinked]"`, "None of these, not on mal")

	panel := render("providerPanel", providerPanelView{TitleID: 3, CanManage: true, Links: []service.ProviderLink{
		{Provider: "anilist", Label: "AniList", ID: "1", Primary: true, Confirmed: true, Connected: true, Syncing: true, URL: "https://anilist.co/manga/1"},
		{Provider: "mangaupdates", Label: "MangaUpdates", ID: "55", Method: "title_match"},
		{Provider: "kitsu", Label: "Kitsu", Method: "user"},
		{Provider: "mal", Label: "MyAnimeList", ID: "2", Confirmed: true, Method: "cross_link", Cover: "https://c/2.jpg"},
		{Provider: "other", Label: "Other"},
	}})
	expect(panel, `data-tip="Metadata source"`, `data-tip="Progressive sync"`, "Stop progressive sync", `data-tip="Suggested match"`, "Confirm match", `data-tip="Not linked"`, `data-tip="Linked"`, `data-tip="Not on Kitsu"`, `"provider":"other","provider_id":""`, "Not on Other", `"provider":"mangaupdates","provider_id":""`, "Not on MangaUpdates", "None of these, not on Other", `"mode":"primary"`, "Use for metadata", "Relink", "Unlink",
		`"provider":"mal","provider_id":"2","mode":"primary"`, `"provider":"mal","provider_id":""`, "https://c/2.jpg", "openPicker('3-mal')", `id="picker-3-kitsu"`, `hx-include="#picker-form-3-mal"`, "Relink MyAnimeList")

	if strings.Contains(panel, "Not on Kitsu</button>") || strings.Contains(panel, `"provider":"kitsu","provider_id":""`) {
		t.Fatalf("title already marked as not on Kitsu still offers the mark:\n%s", panel)
	}

	picker := render("providerPicker", providerPickerView{TitleID: 3, Provider: "mal", Current: "9",
		Results: []catalog.Manga{{Provider: "mal", ProviderID: "9", TitleRomaji: "Berserk 2"}, {Provider: "mal", ProviderID: "2", TitleRomaji: "Berserk", Format: "MANGA"}}})
	expect(picker, "/ui/library/3/providers/set", `"provider_id":"2"`, "https://myanimelist.net/manga/2")
	if strings.Contains(picker, `"provider_id":"9"`) || strings.Contains(picker, "<select") || strings.Contains(picker, "unlinked") {
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
