package gitdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestComparePaginatesAndMergesFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "page=2" {
			serveFixture(t, "compare_page2.json")(w, r)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo/widgets/compare/main...feature?page=2>; rel="next"`, "http://"+r.Host))
		serveFixture(t, "compare_page1.json")(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if got.MergeBaseSHA != "ccccccccccccccccccccccccccccccccccccccc1" {
		t.Fatalf("MergeBaseSHA = %q", got.MergeBaseSHA)
	}
	if len(got.Files) != 4 {
		t.Fatalf("Files = %d, want 4: %+v", len(got.Files), got.Files)
	}

	want := []ChangedFile{
		{Filename: "file1.go", Status: StatusAdded, SHA: "1111111111111111111111111111111111111a"},
		{Filename: "file2.go", Status: StatusRenamed, SHA: "2222222222222222222222222222222222222b", PreviousFilename: "old2.go"},
		{Filename: "file3.go", Status: StatusRemoved},
		{Filename: "file4.go", Status: StatusModified, SHA: "4444444444444444444444444444444444444d"},
	}
	for i, w := range want {
		if got.Files[i] != w {
			t.Fatalf("Files[%d] = %+v, want %+v", i, got.Files[i], w)
		}
	}
}

func TestCompareARenamedFileCarriesItsPreviousName(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "compare_page1.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var renamed *ChangedFile
	for i := range got.Files {
		if got.Files[i].Status == StatusRenamed {
			renamed = &got.Files[i]
		}
	}
	if renamed == nil {
		t.Fatalf("no renamed file in %+v", got.Files)
	}
	if renamed.Filename != "file2.go" || renamed.PreviousFilename != "old2.go" {
		t.Fatalf("renamed = %+v", renamed)
	}
}

func TestCompareARemovedFile(t *testing.T) {
	srv := httptest.NewServer(serveFixture(t, "compare_page2.json"))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	got, err := c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var removed *ChangedFile
	for i := range got.Files {
		if got.Files[i].Status == StatusRemoved {
			removed = &got.Files[i]
		}
	}
	if removed == nil {
		t.Fatalf("no removed file in %+v", got.Files)
	}
	if removed.Filename != "file3.go" || removed.SHA != "" {
		t.Fatalf("removed = %+v", removed)
	}
}

func TestCompareDiffTooLargePastGitHubsCap(t *testing.T) {
	files := make([]map[string]any, 0, DiffCapFiles)
	for i := 0; i < DiffCapFiles; i++ {
		files = append(files, map[string]any{
			"filename": fmt.Sprintf("file%d.go", i),
			"status":   "modified",
			"sha":      fmt.Sprintf("%040d", i),
		})
	}
	body, err := json.Marshal(map[string]any{
		"merge_base_commit": map[string]any{"sha": "ccccccccccccccccccccccccccccccccccccccc1"},
		"files":             files,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err = c.Compare(context.Background(), "octo/widgets", "main", "feature")
	if !IsDiffTooLarge(err) {
		t.Fatalf("err = %v, IsDiffTooLarge = false", err)
	}
}
