package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brogergvhs/kaodoku/internal/util"
)

// imageSlots caps total concurrent image downloads across every running job.
var imageSlots = make(chan struct{}, 8)

const maxImageBytes = 64 << 20

type Downloader struct {
	client     *http.Client
	skipBroken bool
	retryDelay time.Duration
	browser    BrowserFetcher
	warmed     map[string]int
	warmFailed map[string]error
	loaded     map[string]bool
	warmMu     sync.Mutex
}

// BrowserFetcher warms browser-solved sessions for protected image hosts.
type BrowserFetcher interface {
	LoadCached(ctx context.Context, target string)
	Fetch(ctx context.Context, target string) (string, error)
}

// ProgressHandle receives image download progress updates.
type ProgressHandle interface {
	Update(done, total int, bytes int64)
	MarkDone()
}

func New(c *http.Client, skipBroken bool) *Downloader {
	return &Downloader{
		client:     c,
		skipBroken: skipBroken,
		retryDelay: time.Second,
		warmed:     map[string]int{},
		warmFailed: map[string]error{},
		loaded:     map[string]bool{},
	}
}

// SetBrowserFetcher enables browser-solver cookie warming for image downloads.
func (d *Downloader) SetBrowserFetcher(browser BrowserFetcher) {
	d.browser = browser
}

type chapterState struct {
	mu          sync.Mutex
	doneImages  int
	totalImages int
	doneBytes   int64
}

func (d *Downloader) DownloadImagesConcurrently(
	ctx context.Context,
	urls []string,
	folder string,
	referer string,
	maxParallel int,
	ph ProgressHandle,
) ([]string, int64, error) {
	if err := os.MkdirAll(folder, 0755); err != nil {
		return nil, 0, err
	}

	total := len(urls)
	if maxParallel < 1 {
		maxParallel = 1
	}
	if maxParallel > total && total > 0 {
		maxParallel = total
	}

	cs := &chapterState{totalImages: total}
	ph.Update(0, total, 0)

	var filesMu sync.Mutex
	files := make([]string, 0, len(urls))
	errs := make([]error, 0, 4)

	jobs := make(chan int)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for i := range jobs {
			u := urls[i]

			ext := imageExt(u)
			if ext == "" {
				ext = ".jpg"
			}

			path := filepath.Join(folder, fmt.Sprintf("page_%03d%s", i+1, ext))
			var last int64

			progress := func(done int64) {
				delta := done - last
				if delta <= 0 {
					return
				}

				last = done
				cs.mu.Lock()
				cs.doneBytes += delta
				ph.Update(cs.doneImages, cs.totalImages, cs.doneBytes)
				cs.mu.Unlock()
			}

			if err := d.downloadWithRetry(ctx, u, path, referer, progress); err != nil {
				_ = os.Remove(path)
				cs.mu.Lock()
				errs = append(errs, fmt.Errorf("image %d: %v", i+1, err))
				cs.doneImages++
				ph.Update(cs.doneImages, cs.totalImages, cs.doneBytes)
				cs.mu.Unlock()

				continue
			}

			if ext == ".jpg" {
				if real := sniffImageExt(path); real != "" && real != ext {
					renamed := strings.TrimSuffix(path, ext) + real
					if os.Rename(path, renamed) == nil {
						path = renamed
					}
				}
			}

			filesMu.Lock()
			files = append(files, path)
			filesMu.Unlock()

			cs.mu.Lock()
			cs.doneImages++
			ph.Update(cs.doneImages, cs.totalImages, cs.doneBytes)
			cs.mu.Unlock()
		}
	}

	wg.Add(maxParallel)
	for w := 0; w < maxParallel; w++ {
		go worker()
	}

	for i := range urls {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			ph.MarkDone()
			return files, cs.doneBytes, ctx.Err()
		case jobs <- i:
		}
	}

	close(jobs)
	wg.Wait()
	ph.MarkDone()

	if len(errs) > 0 && !d.skipBroken {
		return files, cs.doneBytes, fmt.Errorf("failed %d/%d images: %s (use --skip-broken to continue)", len(errs), total, sampleErrors(errs, 3))
	}

	return files, cs.doneBytes, nil
}

func sampleErrors(errs []error, limit int) string {
	if len(errs) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var b strings.Builder
	written := 0
	for _, err := range errs {
		text := err.Error()
		key := sampleErrorKey(text)
		if seen[key] {
			continue
		}
		seen[key] = true
		if written > 0 {
			b.WriteString("; ")
		}
		b.WriteString(text)
		written++
		if written == limit {
			break
		}
	}
	if written < len(errs) {
		fmt.Fprintf(&b, "; ...")
	}
	return b.String()
}

func sampleErrorKey(text string) string {
	prefix, suffix, ok := strings.Cut(text, ": ")
	if ok && strings.HasPrefix(prefix, "image ") {
		return suffix
	}
	return text
}

func (d *Downloader) downloadWithRetry(
	ctx context.Context,
	url string,
	output string,
	referer string,
	progress func(done int64),
) error {
	var err error
	d.loadCached(ctx, url)
	host := targetHost(url)
	if warmErr := d.warmFailure(host); warmErr != nil {
		return fmt.Errorf("HTTP 403 (browser warm failed for %s: %w)", host, warmErr)
	}
	maxAttempts := 3
	if d.browser != nil {
		maxAttempts = 4
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = d.download(ctx, url, output, referer, progress)
		if err == nil {
			return nil
		}
		var statusErr statusError
		if d.browser != nil && asStatusError(err, &statusErr) && statusErr.Code == http.StatusForbidden {
			if warmErr := d.warmNextBrowser(ctx, host, referer, imageOrigin(url)); warmErr == nil {
				continue
			} else {
				return fmt.Errorf("%w (browser warm failed for %s: %w)", err, host, warmErr)
			}
		}

		if attempt == maxAttempts {
			break
		}
		delay := time.Duration(attempt) * d.retryDelay
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	var statusErr statusError
	if d.browser == nil && asStatusError(err, &statusErr) && statusErr.Code == http.StatusForbidden {
		return fmt.Errorf("%w (image host blocked; enable browser_solver.enabled to warm CDN cookies)", err)
	}
	return err
}

func (d *Downloader) download(
	ctx context.Context,
	u, output, referer string,
	progress func(done int64),
) (err error) {
	select {
	case imageSlots <- struct{}{}:
		defer func() { <-imageSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = util.ExemptFromRateLimit(ctx)

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Dest", "image")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Site", fetchSite(referer, u))

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, resp.Body.Close())
	}()

	if resp.StatusCode != http.StatusOK {
		e := statusError{Code: resp.StatusCode}
		if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/json" {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			e.Body = strings.TrimSpace(string(body))
		}
		return e
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mt, _, _ := mime.ParseMediaType(ct); !strings.HasPrefix(mt, "image/") {
			if !genericBinaryMIME(mt) {
				return fmt.Errorf("unexpected MIME: %s", ct)
			}
		}
	}

	f, err := os.Create(output)
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, f.Close())
	}()

	written, err := copyWithProgress(f, io.LimitReader(resp.Body, maxImageBytes+1), progress)
	if err != nil {
		return err
	}
	if written > maxImageBytes {
		return fmt.Errorf("image exceeds %d bytes", maxImageBytes)
	}

	if progress != nil && resp.ContentLength > 0 && written < resp.ContentLength {
		progress(resp.ContentLength)
	}

	return nil
}

type statusError struct {
	Code int
	Body string
}

func (e statusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body)
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

func asStatusError(err error, out *statusError) bool {
	return errors.As(err, out)
}

func (d *Downloader) loadCached(ctx context.Context, target string) {
	if d.browser == nil {
		return
	}
	key := targetHost(target)
	if key == "" {
		key = target
	}
	d.warmMu.Lock()
	if d.loaded[key] {
		d.warmMu.Unlock()
		return
	}
	d.loaded[key] = true
	d.warmMu.Unlock()
	d.browser.LoadCached(ctx, target)
}

func (d *Downloader) warmNextBrowser(ctx context.Context, key string, targets ...string) error {
	if key == "" {
		key = firstNonEmpty(targets...)
	}
	d.warmMu.Lock()
	defer d.warmMu.Unlock()
	if err := d.warmFailed[key]; err != nil {
		return err
	}
	var lastErr error
	seen := map[string]bool{}
	for d.warmed[key] < len(targets) {
		target := targets[d.warmed[key]]
		d.warmed[key]++
		target = strings.TrimSpace(target)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		_, err := d.browser.Fetch(ctx, target)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	err := lastErr
	if err == nil {
		err = fmt.Errorf("browser warm targets exhausted for %s", key)
	}
	d.warmFailed[key] = err
	return err
}

func (d *Downloader) warmFailure(key string) error {
	if key == "" {
		return nil
	}
	d.warmMu.Lock()
	defer d.warmMu.Unlock()
	return d.warmFailed[key]
}

func targetHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return ""
	}
	return u.Host
}

func imageOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/"
}

func fetchSite(referer, target string) string {
	ref, err := url.Parse(referer)
	if err != nil || ref == nil {
		return "cross-site"
	}
	dst, err := url.Parse(target)
	if err != nil || dst == nil {
		return "cross-site"
	}
	if ref.Host == dst.Host {
		return "same-origin"
	}
	return "cross-site"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func imageExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil && u != nil {
		return cleanImageExt(path.Ext(u.Path))
	}
	return cleanImageExt(filepath.Ext(rawURL))
}

func sniffImageExt(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mt, _, err := mime.ParseMediaType(http.DetectContentType(head[:n]))
	if err != nil {
		return ""
	}
	if ext := cleanImageExt("." + strings.TrimPrefix(mt, "image/")); ext != ".jpeg" {
		return ext
	}
	return ".jpg"
}

func cleanImageExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return ext
	default:
		return ""
	}
}

func genericBinaryMIME(mt string) bool {
	switch strings.ToLower(mt) {
	case "application/octet-stream", "binary/octet-stream", "application/x-download", "application/force-download":
		return true
	default:
		return false
	}
}
