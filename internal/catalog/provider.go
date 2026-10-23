package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	MALProvider          = "mal"
	KitsuProvider        = "kitsu"
	MangaUpdatesProvider = "mangaupdates"
)

var (
	ErrNotFound     = errors.New("catalog: not found")
	ErrUnauthorized = errors.New("catalog: unauthorized")
	ErrUnavailable  = errors.New("unavailable")
)

func IsNotFound(err error) bool     { return errors.Is(err, ErrNotFound) }
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }
func IsUnavailable(err error) bool  { return errors.Is(err, ErrUnavailable) }

// health remembers the last outage seen by a client until any request succeeds again, and counts its calls.
type health struct {
	outage atomic.Pointer[string]
	mu     sync.Mutex
	calls  map[Call]int64
}

// Call is what a request asked a tracker for, and why.
type Call struct{ Kind, Reason string }

type callKindKey struct{}
type callReasonKey struct{}

// WithReason names why the requests made with ctx are sent; the first reason set wins.
func WithReason(ctx context.Context, reason string) context.Context {
	if ctx.Value(callReasonKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, callReasonKey{}, reason)
}

func asCall(ctx context.Context, kind string) context.Context {
	if ctx.Value(callKindKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, callKindKey{}, kind)
}

func (h *health) Count(c Call, n int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.calls == nil {
		h.calls = map[Call]int64{}
	}
	h.calls[c] += n
}

func (h *health) count(ctx context.Context) {
	kind, _ := ctx.Value(callKindKey{}).(string)
	reason, _ := ctx.Value(callReasonKey{}).(string)
	h.Count(Call{cmp.Or(kind, "Other"), cmp.Or(reason, "Other")}, 1)
}

func (h *health) TakeCalls() map[Call]int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.calls
	h.calls = nil
	return out
}

var imageHosts = map[string]string{"anilist.co": AniListProvider, "anili.st": AniListProvider, "myanimelist.net": MALProvider, "kitsu.app": KitsuProvider, "kitsu.io": KitsuProvider, "mangaupdates.com": MangaUpdatesProvider}

// ImageProvider names the tracker whose servers host an image URL, or "" for any other host.
func ImageProvider(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	for domain, p := range imageHosts {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return p
		}
	}
	return ""
}

func (h *health) Outage() string {
	if r := h.outage.Load(); r != nil {
		return *r
	}
	return ""
}

func (h *health) observe(ctx context.Context, err error) error {
	switch {
	case err == nil:
		h.outage.Store(nil)
	case IsUnavailable(err) && ctx.Err() == nil:
		msg := err.Error()
		h.outage.Store(&msg)
	}
	return err
}

// transportError treats refused, reset or unresolvable connections as an outage, but not a single slow request.
func transportError(label string, err error) error {
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return fmt.Errorf("%s request: %w", label, err)
	}
	return fmt.Errorf("%s %w: %w", label, ErrUnavailable, err)
}

func outageReason(data []byte) string {
	var body struct{ Message string }
	if json.Unmarshal(data, &body) == nil && body.Message != "" {
		return body.Message
	}
	return fmt.Sprintf("%.200s", data)
}

// Provider is one metadata/search/recommendation/tracking backend; tracking acts on the context Account.
type Provider interface {
	Name() string
	Label() string
	Get(ctx context.Context, id string) (Manga, []Relation, error)
	Search(ctx context.Context, query string, limit int, f SearchFilter) ([]Manga, bool, error)
	Trending(ctx context.Context, limit int) ([]Manga, error)
	Related(ctx context.Context, id string, limit int) (relations, recommendations []Manga, err error)
	Viewer(ctx context.Context) (id, name string, err error)
	ListEntries(ctx context.Context) ([]Entry, error)
	GetEntry(ctx context.Context, id string) (Entry, bool, error)
	SaveEntry(ctx context.Context, id string, u EntryUpdate) error
	DeleteEntry(ctx context.Context, id string) error
}

type Favouriter interface {
	Favourites(ctx context.Context) ([]string, error)
	IsFavourite(ctx context.Context, id string) (bool, error)
	ToggleFavourite(ctx context.Context, id string) error
}

// Vocabulary lists a provider's own genre and tag names, the ones its search filters and manga use.
type Vocabulary interface {
	TagVocabulary(ctx context.Context) ([]ContentTag, error)
}

var adultNames = map[string]bool{"adult": true, "hentai": true, "smut": true, "erotica": true, "lolicon": true, "shotacon": true}

// AdultName reports genre names that mark explicit content on any provider.
func AdultName(name string) bool { return adultNames[strings.ToLower(strings.TrimSpace(name))] }

type Resolver interface {
	Resolve(ctx context.Context, provider, id string) (string, bool, error)
}

type OAuth interface {
	AuthorizeURL(app App, state, verifier string) string
	Exchange(ctx context.Context, app App, code, verifier string) (Token, error)
}

type Refresher interface {
	Refresh(ctx context.Context, app App, refresh string) (Token, error)
}

type PasswordLogin interface {
	Login(ctx context.Context, username, password string) (Token, error)
}

type App struct{ ClientID, ClientSecret, RedirectURI string }

type Token struct {
	Access, Refresh string
	ExpiresIn       int
}

// Entry status is neutral: PLANNING, CURRENT, COMPLETED, DROPPED, PAUSED or REPEATING.
type Entry struct {
	Manga             Manga
	Progress          int
	Status            string
	Started, Finished time.Time
	ref               string
}

// EntryUpdate keeps the remote value for Progress < 0, an empty Status and zero dates; Remote is the entry already read, if any.
type EntryUpdate struct {
	Progress          int
	Status            string
	Started, Finished time.Time
	Remote            *Entry
}

func (u EntryUpdate) remote(ctx context.Context, id string, get func(context.Context, string) (Entry, bool, error)) (Entry, bool, error) {
	if u.Remote != nil {
		return *u.Remote, true, nil
	}
	return get(ctx, id)
}

func (u EntryUpdate) Empty() bool {
	return u.Progress < 0 && u.Status == "" && u.Started.IsZero() && u.Finished.IsZero()
}

// DateTracker marks providers whose lists store start and finish dates.
type DateTracker interface{ TracksDates() bool }

type Account struct{ Token, UserID string }

type ctxAccountKey struct{}

func WithAccount(ctx context.Context, a Account) context.Context {
	return context.WithValue(ctx, ctxAccountKey{}, a)
}

func AccountFrom(ctx context.Context) Account {
	a, _ := ctx.Value(ctxAccountKey{}).(Account)
	return a
}

func MangaURL(provider, id string) string {
	if id == "" {
		return ""
	}
	switch provider {
	case AniListProvider:
		return "https://anilist.co/manga/" + id
	case MALProvider:
		return "https://myanimelist.net/manga/" + id
	case KitsuProvider:
		return "https://kitsu.app/manga/" + id
	case MangaUpdatesProvider:
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			return "https://www.mangaupdates.com/series/" + strconv.FormatInt(n, 36)
		}
	}
	return ""
}

func RelKey(provider, id string) string {
	if provider == AniListProvider || provider == "" {
		return id
	}
	return provider + ":" + id
}

func Keys(m Manga) []string {
	out := []string{m.Provider + ":" + m.ProviderID}
	for p, id := range m.ExternalIDs {
		if id != "" {
			out = append(out, p+":"+id)
		}
	}
	return out
}

func Titles(m Manga) []string {
	return append([]string{m.TitleRomaji, m.TitleEnglish, m.TitleNative}, m.Synonyms...)
}

func MatchScore(a, b Manga) float64 {
	best := 0.0
	for _, x := range Titles(a) {
		for _, y := range Titles(b) {
			best = max(best, titleSimilarity(x, y))
		}
	}
	score := best * 0.8
	if a.Year != 0 && a.Year == b.Year {
		score += 0.1
	}
	if a.Format != "" && a.Format == b.Format {
		score += 0.1
	}
	return score
}

func titleSimilarity(a, b string) float64 {
	a, b = normTitle(a), normTitle(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	grams := func(s string) map[string]int {
		r := []rune(s)
		out := map[string]int{}
		for i := 0; i+1 < len(r); i++ {
			out[string(r[i:i+2])]++
		}
		return out
	}
	ga, gb := grams(a), grams(b)
	shared, total := 0, 0
	for g, n := range ga {
		shared += min(n, gb[g])
		total += n
	}
	for _, n := range gb {
		total += n
	}
	if total == 0 {
		return 0
	}
	return 2 * float64(shared) / float64(total)
}

func normTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space && b.Len() > 0 {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func normFormat(kind string) string {
	switch strings.ToLower(strings.ReplaceAll(kind, "_", "")) {
	case "oneshot":
		return "ONE_SHOT"
	case "novel", "lightnovel":
		return "NOVEL"
	case "doujinshi", "doujin":
		return "DOUJINSHI"
	case "artbook":
		return "ARTBOOK"
	default:
		return "MANGA"
	}
}

func countryRaw(kind string) string {
	switch strings.ToLower(kind) {
	case "manhwa":
		return `{"countryOfOrigin":"KR"}`
	case "manhua":
		return `{"countryOfOrigin":"CN"}`
	}
	return ""
}

func normRelation(kind string) string {
	k := strings.ToUpper(strings.NewReplacer(" ", "_", "-", "_").Replace(strings.TrimSpace(kind)))
	switch k {
	case "PARENT_STORY", "FULL_STORY", "MAIN_STORY":
		return "PARENT"
	case "ALTERNATIVE_SETTING", "ALTERNATIVE_VERSION", "ALTERNATE_STORY":
		return "ALTERNATIVE"
	case "SPINOFF":
		return "SPIN_OFF"
	case "SEQUEL", "PREQUEL", "SIDE_STORY", "SPIN_OFF", "SUMMARY", "CHARACTER", "ADAPTATION":
		return k
	}
	return "OTHER"
}
