package cvmfs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeMirrorsSelectsFastestRootCatalog(t *testing.T) {
	const rootHash = "0123456789abcdef0123456789abcdef01234567"
	server := func(rootDelay time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/.cvmfspublished"):
				_, _ = fmt.Fprintf(w, "C%s\n", rootHash)
			case strings.Contains(r.URL.Path, "/data/01/23456789abcdef0123456789abcdef01234567C"):
				time.Sleep(rootDelay)
				_, _ = w.Write(make([]byte, 1024))
			default:
				http.NotFound(w, r)
			}
		}))
	}
	slow := server(80 * time.Millisecond)
	defer slow.Close()
	fast := server(5 * time.Millisecond)
	defer fast.Close()

	selected, results, err := ProbeMirrors(t.Context(), http.DefaultClient, "repo.example", []string{slow.URL, fast.URL})
	if err != nil {
		t.Fatal(err)
	}
	if selected != normalizeMirror(fast.URL) {
		t.Fatalf("selected mirror = %q, want %q; results=%+v", selected, normalizeMirror(fast.URL), results)
	}
	if len(results) != 2 || results[1].RootCatalogBytes != 1024 || results[1].RootCatalogBytesPerSec <= results[0].RootCatalogBytesPerSec {
		t.Fatalf("probe results = %+v", results)
	}
}

func TestPreferredMirrorLeadsUntilItFails(t *testing.T) {
	client := NewClient()
	client.SetPreferredMirror("http://preferred.example")
	preferred := normalizeMirror("http://preferred.example")
	repo := &repository{client: client, mirrors: []string{"http://fallback.example/cvmfs", preferred}}
	if ordered := repo.orderedMirrors(); len(ordered) != 2 || ordered[0] != preferred {
		t.Fatalf("preferred mirror order = %v", ordered)
	}
	client.recordMirrorResult(preferred, time.Second, fmt.Errorf("unreachable"), 0)
	if ordered := repo.orderedMirrors(); len(ordered) != 2 || ordered[0] == preferred {
		t.Fatalf("failed preferred mirror was not demoted: %v", ordered)
	}
}
