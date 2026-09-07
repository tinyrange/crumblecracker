package cvmfs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMirrorSelectionExploresNewMirrorsAndDemotesFailures(t *testing.T) {
	client := NewClient()
	client.mirrorStats = map[string]*mirrorStat{
		"failed": {failures: 1},
		"fast":   {successes: 1, ewma: 100 * time.Millisecond},
	}
	repo := &repository{client: client, mirrors: []string{"failed", "fast", "untried"}}

	ordered := repo.orderedMirrors()
	if len(ordered) != 3 || ordered[0] != "untried" || ordered[1] != "fast" || ordered[2] != "failed" {
		t.Fatalf("mirror order = %v, want untried then healthy then failed", ordered)
	}
}

func TestMirrorDownloadReportsLogicalTransfer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	defer server.Close()

	client := NewClient()
	var events []TransferEvent
	client.OnTransfer = func(event TransferEvent) { events = append(events, event) }
	repo := &repository{client: client, repo: "repo.example", mirrors: []string{server.URL}}
	err := repo.getFromMirrors("test", "/containers/tool.sif", func(mirror string) string { return mirror }, func(_ string, response *http.Response, _ uint64, _ time.Time) error {
		_, err := io.Copy(io.Discard, response.Body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0].State != "started" || events[len(events)-1].State != "completed" {
		t.Fatalf("transfer events = %+v", events)
	}
	completed := events[len(events)-1]
	if completed.Repo != "repo.example" || completed.Path != "/containers/tool.sif" || completed.Bytes != int64(len("payload")) {
		t.Fatalf("completed transfer = %+v", completed)
	}
}

func TestCatalogPathIndexIsBuiltOnceWithoutDuplicateChildren(t *testing.T) {
	repo := &repository{repo: "repo.example"}
	cat := &catalog{entries: []catalogEntry{
		{Md5Path1: 1, Md5Path2: 1, Name: "containers"},
		{Md5Path1: 2, Md5Path2: 2, Parent1: 1, Parent2: 1, Name: "tool"},
	}}
	if err := repo.indexCatalog(cat, "/"); err != nil {
		t.Fatal(err)
	}
	if err := repo.indexCatalog(cat, "/"); err != nil {
		t.Fatal(err)
	}
	if got := repo.entriesByPath["/containers/tool"].Name; got != "tool" {
		t.Fatalf("indexed tool name = %q", got)
	}
	if got := len(repo.childrenByPath["/containers"]); got != 1 {
		t.Fatalf("indexed child count = %d, want 1", got)
	}
}
