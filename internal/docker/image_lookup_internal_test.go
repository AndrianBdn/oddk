package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

func TestImageLookupAcceptsUntaggedIDs(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/images/"+imageID+"/json") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": imageID, "RepoTags": []string{}})
	}))
	defer server.Close()
	api, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.56"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = api.Close() }()
	c := &Client{cli: api, ctx: t.Context()}

	if tags, exists := c.CheckImageExists(imageID); !exists || len(tags) != 0 {
		t.Fatalf("untagged rollback image must exist: tags=%v exists=%v", tags, exists)
	}
	if got, exists := c.GetImageID(imageID); !exists || got != imageID {
		t.Fatalf("image ID must resolve to itself: id=%q exists=%v", got, exists)
	}
	if _, exists := c.CheckImageExists("missing:17"); exists {
		t.Fatal("missing image must not exist")
	}
	if _, exists := c.GetImageID("missing:17"); exists {
		t.Fatal("missing image must not resolve")
	}
}
