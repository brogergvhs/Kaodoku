// Package registry maps scraper names to constructors so profile validation
// and scraper construction share one list; new scrapers register here once.
package registry

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/brogergvhs/kaodoku/internal/kotatsu"
	"github.com/brogergvhs/kaodoku/internal/providers"
	"github.com/brogergvhs/kaodoku/internal/providers/comickz"
	"github.com/brogergvhs/kaodoku/internal/providers/generic"
	"github.com/brogergvhs/kaodoku/internal/providers/iken"
	"github.com/brogergvhs/kaodoku/internal/providers/madara"
	"github.com/brogergvhs/kaodoku/internal/providers/mangadex"
	"github.com/brogergvhs/kaodoku/internal/ui"
)

// Constructor builds a scraper with the shared provider dependencies.
type Constructor func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper

var constructors = map[string]Constructor{
	"generic": func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper {
		return generic.NewScraper(client, log, allowExt, checkJS, browser)
	},
	"comickz": func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper {
		return comickz.NewScraper(client, log, allowExt, checkJS, browser)
	},
	"madara": func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper {
		return madara.NewScraper(client, log, allowExt, checkJS, browser)
	},
	"iken": func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper {
		return iken.NewScraper(client, log, allowExt, checkJS, browser)
	},
	"mangadex": func(client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher) providers.Scraper {
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		return mangadex.NewScraper(client, log, allowExt, checkJS, browser)
	},
}

// Names lists the registered scrapers, sorted for display.
func Names() []string {
	names := make([]string, 0, len(constructors))
	for name := range constructors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Supported reports whether name is a known scraper.
func Supported(name string) bool {
	if strings.HasPrefix(name, kotatsu.ScraperPrefix) {
		return true
	}
	_, ok := constructors[name]
	return ok
}

// New builds the named scraper; an empty name selects the generic one.
// "kotatsu:<PARSER>" names route to the kotatsu-worker sidecar.
func New(name string, client *http.Client, log ui.Log, allowExt []string, checkJS bool, browser generic.BrowserFetcher, worker *kotatsu.Client) (providers.Scraper, error) {
	if id, ok := strings.CutPrefix(name, kotatsu.ScraperPrefix); ok {
		if worker == nil {
			return nil, fmt.Errorf("source needs the kotatsu worker, which is not enabled")
		}
		return kotatsu.NewScraper(worker, id), nil
	}
	if name == "" {
		name = "generic"
	}
	build, ok := constructors[name]
	if !ok {
		return nil, fmt.Errorf("unsupported scraper %q", name)
	}
	return build(client, log, allowExt, checkJS, browser), nil
}
