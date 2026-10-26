package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelatedSplitsRelationsFromRecommendations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"Media":{
			"relations":{"edges":[
				{"relationType":"SEQUEL","node":{"id":2,"type":"MANGA","format":"MANGA"}},
				{"relationType":"SOURCE","node":{"id":3,"type":"MANGA","format":"NOVEL"}},
				{"relationType":"ADAPTATION","node":{"id":4,"type":"ANIME","format":"TV"}}]},
			"recommendations":{"nodes":[
				{"mediaRecommendation":{"id":2,"type":"MANGA","format":"MANGA"}},
				{"mediaRecommendation":{"id":1,"type":"MANGA","format":"MANGA"}},
				{"mediaRecommendation":{"id":5,"type":"MANGA","format":"MANGA"}},
				{"mediaRecommendation":{"id":6,"type":"MANGA","format":"NOVEL"}}]}}}}`))
	}))
	defer srv.Close()
	c := NewAniListClient(srv.Client())
	c.endpoint = srv.URL
	relations, recs, err := c.Related(context.Background(), "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations) != 2 || relations[0].ProviderID != "2" || relations[0].RelationType != "SEQUEL" || relations[1].ProviderID != "3" {
		t.Fatalf("relations = %+v", relations)
	}
	if len(recs) != 1 || recs[0].ProviderID != "5" || recs[0].RelationType != "" {
		t.Fatalf("recommendations = %+v", recs)
	}
}
