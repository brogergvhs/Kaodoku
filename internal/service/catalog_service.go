package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/catalog"
	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/library"
	"github.com/brogergvhs/kaodoku/internal/sources"
)

// CatalogService owns metadata providers, the local catalog and matching catalog works to download sources.
type CatalogService struct {
	db          database.Handle
	repo        *catalog.Repository
	sources     *sources.Repository
	library     *library.Repository
	providers   map[string]catalog.Provider
	searchMu    sync.Mutex
	searches    map[string]cachedSearch
	searchLimit func(context.Context) int
}

type cachedSearch struct {
	items   []catalog.Manga
	more    bool
	expires time.Time
}

const searchTTL = 10 * time.Minute

var providerOrder = []string{catalog.AniListProvider, catalog.MALProvider, catalog.KitsuProvider, catalog.MangaUpdatesProvider, catalog.MangaBakaProvider}

var ErrContentBlocked = errors.New("content blocked")

func OpenCatalog(ctx context.Context, dbPath string) (*CatalogService, func(), error) {
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return newCatalogService(db), func() { _ = db.Close() }, nil
}

func newCatalogService(db database.Handle) *CatalogService {
	return &CatalogService{
		db:      db,
		repo:    catalog.NewRepository(db),
		sources: sources.NewRepository(db),
		library: library.NewRepository(db),
		providers: map[string]catalog.Provider{
			catalog.AniListProvider:      catalog.NewAniListClient(nil),
			catalog.MALProvider:          catalog.NewMALClient(nil),
			catalog.KitsuProvider:        catalog.NewKitsuClient(nil),
			catalog.MangaUpdatesProvider: catalog.NewMangaUpdatesClient(nil),
		},
	}
}

func (s *CatalogService) Provider(name string) (catalog.Provider, error) {
	if p, ok := s.providers[name]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}

// Search stores unseen results; known ones keep their stored record, since search results can be partial (no authors or synonyms).
func (s *CatalogService) Search(ctx context.Context, p catalog.Provider, query string, limit int, filter catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	items, more, err := s.searchCached(ctx, p, strings.TrimSpace(query), limit, filter)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ProviderID
	}
	known, err := s.repo.MangaByProviderIDs(ctx, p.Name(), ids)
	if err != nil {
		return nil, false, err
	}
	out := make([]catalog.Manga, 0, len(items))
	for _, item := range items {
		if m, ok := known[item.ProviderID]; ok {
			item.ID, item.Wanted = m.ID, m.Wanted
			out = append(out, item)
			continue
		}
		stored, err := s.repo.UpsertManga(ctx, item)
		if err != nil {
			return nil, false, err
		}
		stored.ExternalIDs = item.ExternalIDs
		out = append(out, stored)
	}
	return out, more, nil
}

// FlushRequests adds each tracker's calls since the last flush to today's rows.
func (s *CatalogService) FlushRequests(ctx context.Context) {
	for name, p := range s.providers {
		c, ok := p.(callCounter)
		if !ok {
			continue
		}
		for call, n := range c.TakeCalls() {
			if s.saveCalls(ctx, name, call, n) != nil {
				c.Count(call, n)
			}
		}
	}
}

func (s *CatalogService) saveCalls(ctx context.Context, name string, call catalog.Call, n int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tracker_calls (provider, day, kind, reason, count) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider, day, kind, reason) DO UPDATE SET count = count + excluded.count`, name, time.Now().UTC().Format(time.DateOnly), call.Kind, call.Reason, n)
	return err
}

func (s *CatalogService) searchCached(ctx context.Context, p catalog.Provider, query string, limit int, filter catalog.SearchFilter) ([]catalog.Manga, bool, error) {
	key := fmt.Sprintf("%s|%q|%d|%#v", p.Name(), query, limit, filter)
	now := time.Now()
	s.searchMu.Lock()
	c, ok := s.searches[key]
	s.searchMu.Unlock()
	if ok && now.Before(c.expires) {
		return c.items, c.more, nil
	}
	items, more, err := p.Search(ctx, query, limit, filter)
	if err != nil {
		return nil, false, err
	}
	keep := 500
	if s.searchLimit != nil {
		keep = s.searchLimit(ctx)
	}
	s.searchMu.Lock()
	if s.searches == nil {
		s.searches = map[string]cachedSearch{}
	}
	maps.DeleteFunc(s.searches, func(_ string, c cachedSearch) bool { return now.After(c.expires) })
	s.searches[key] = cachedSearch{items, more, now.Add(searchTTL)}
	trimOldest(s.searches, keep, func(c cachedSearch) time.Time { return c.expires })
	s.searchMu.Unlock()
	return items, more, nil
}

func (s *CatalogService) fetch(ctx context.Context, p catalog.Provider, id string) (catalog.Manga, error) {
	m, rels, err := p.Get(ctx, id)
	if err != nil {
		return catalog.Manga{}, err
	}
	stored, err := s.repo.UpsertManga(ctx, m)
	if err != nil {
		return catalog.Manga{}, err
	}
	return stored, s.repo.ReplaceRelations(ctx, catalog.RelKey(p.Name(), id), relKeys(p.Name(), rels))
}

func relKeys(provider string, rels []catalog.Relation) []catalog.Relation {
	out := make([]catalog.Relation, len(rels))
	for i, r := range rels {
		out[i] = catalog.Relation{ProviderID: catalog.RelKey(provider, r.ProviderID), Type: r.Type}
	}
	return out
}

// AddWanted stores a work as wanted; a non-nil allowed guard runs before anything is persisted.
func (s *CatalogService) AddWanted(ctx context.Context, p catalog.Provider, id string, allowed func(catalog.Manga) bool) (catalog.Manga, error) {
	item, _, err := p.Get(ctx, id)
	if err != nil {
		return catalog.Manga{}, err
	}
	if allowed != nil && !allowed(item) {
		return catalog.Manga{}, ErrContentBlocked
	}
	item.Wanted = true
	return s.repo.UpsertManga(ctx, item)
}

func (s *CatalogService) ListWanted(ctx context.Context) ([]catalog.Manga, error) {
	return s.repo.ListWanted(ctx)
}
