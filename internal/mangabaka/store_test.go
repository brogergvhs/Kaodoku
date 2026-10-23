package mangabaka

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const fixture = `{"id":1,"state":"active","title":"Made in Abyss Official Anthology","titles":[{"title":"Made in Abyss Anthology","language":"en","is_primary":true}],"type":"manga","status":"completed","content_rating":"safe","total_chapters":"40","genres":["Adventure"],"tags":["Anthology"],"cover":{"raw":{"url":"https://images.mangabaka.dev/a"}},"popularity":{"global":{"current":20,"history":{"1w":30}}},"relationships_v2":[{"to_series_id":2,"relation_type":"main"}],"source":{"anilist":{"id":113956},"my_anime_list":{"id":130512},"kitsu":{"id":40211},"manga_updates":{"id":"wj13tgm"}}}
{"id":2,"state":"active","title":"Made in Abyss","type":"manga","status":"releasing","content_rating":"suggestive","popularity":{"global":{"current":5,"history":{"1w":5}}},"source":{"anilist":{"id":86551},"manga_updates":{"id":"aippxsa","response":{"recommendations":[{"series_id":70807749862,"weight":9}]}}}}
{"id":3,"state":"merged","merged_with":1,"title":"Made in Abyss Official Anthology"}
{"id":4,"state":"active","title":"Twin A","source":{"manga_updates":{"id":"aaaa"}}}
{"id":5,"state":"active","title":"Twin B","source":{"manga_updates":{"id":"aaaa"}}}
{"id":6,"state":"deleted","title":"Gone"}
`

func dump(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "series.jsonl", Mode: 0o644, Size: int64(len(fixture)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(fixture))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestRefreshImportsAndLooksUp(t *testing.T) {
	data := dump(t)
	sum := sha1.Sum(data)
	checksum := hex.EncodeToString(sum[:])
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			_, _ = w.Write([]byte(checksum + "  series.jsonl.tar.gz\n"))
			return
		}
		downloads.Add(1)
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	ctx := context.Background()
	s := Open(filepath.Join(t.TempDir(), "mangabaka.db"))
	defer s.Close()
	if changed, err := s.Refresh(ctx, srv.Client(), srv.URL+"/series.jsonl.tar.gz", "full", false, nil); err != nil || !changed {
		t.Fatalf("refresh = %v, %v", changed, err)
	}
	if m := s.Meta(ctx); m.Series != 4 || m.SHA1 != checksum || m.Variant != "full" {
		t.Fatalf("meta = %+v", m)
	}
	a, ok := s.ByProvider(ctx, "anilist", "113956")
	if !ok || a.ID != 1 || a.IDs["mangaupdates"] != "70807749862" || a.IDs["kitsu"] != "40211" || a.English != "Made in Abyss Anthology" || a.Chapters != 40 {
		t.Fatalf("by anilist = %+v, %v", a, ok)
	}
	if _, ok := s.ByProvider(ctx, "mangaupdates", "479890"); ok {
		t.Fatal("an id shared by two series must not match")
	}
	if got := s.ByIDs(ctx, []int64{3}); got[3].ID != 1 {
		t.Fatalf("merged id = %+v", got)
	}
	rels, recs := s.Related(ctx, 1, 10)
	if len(rels) != 1 || rels[0].ID != 2 || rels[0].Relation != "main" {
		t.Fatalf("relations = %+v", rels)
	}
	if _, recs = s.Related(ctx, 2, 10); len(recs) != 1 || recs[0].ID != 1 {
		t.Fatalf("recommendations = %+v", recs)
	}
	if found, _ := s.Search(ctx, "abyss anth", Filter{Limit: 5}); len(found) != 1 || found[0].ID != 1 {
		t.Fatalf("search = %+v", found)
	}
	if found, _ := s.Search(ctx, "", Filter{Limit: 5, GenreIn: []string{"Adventure"}}); len(found) != 1 || found[0].ID != 1 {
		t.Fatalf("genre browse = %+v", found)
	}
	if trend := s.Trending(ctx, 1); len(trend) != 1 || trend[0].ID != 1 {
		t.Fatalf("trending = %+v", trend)
	}
	if !strings.HasPrefix(a.CoverURL(350), "https://cdn.mangabaka.dev/imgproxy/plain/x350@1/aHR0") {
		t.Fatalf("cover = %s", a.CoverURL(350))
	}

	if changed, err := s.Refresh(ctx, srv.Client(), srv.URL+"/series.jsonl.tar.gz", "full", false, nil); err != nil || changed || downloads.Load() != 1 {
		t.Fatalf("unchanged refresh = %v, %v, %d downloads", changed, err, downloads.Load())
	}
	checksum = strings.Repeat("0", 40)
	if _, err := s.Refresh(ctx, srv.Client(), srv.URL+"/series.jsonl.tar.gz", "full", true, nil); err == nil {
		t.Fatal("a checksum mismatch must fail")
	}
	if _, ok := s.ByProvider(ctx, "anilist", "113956"); !ok {
		t.Fatal("a failed refresh must keep the old copy")
	}
}
