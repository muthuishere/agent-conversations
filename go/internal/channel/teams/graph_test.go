package teams

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Real Graph occasionally returns an @odata.nextLink that it then rejects with
// HTTP 400. That must not fail the conversation: keep the pages already
// collected and stop. A 400 on the very first request is still an error.
func TestGetCollectionToleratesRejectedContinuation(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Query().Get("page") {
		case "":
			fmt.Fprintf(w, `{"value":[{"id":"a"},{"id":"b"}],"@odata.nextLink":"%s/x?page=2"}`, "http://"+r.Host)
		case "2":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"BadRequest","message":"Parameter 'DeltaToken' not supported for this request."}}`)
		}
	}))
	defer srv.Close()
	c := &Channel{cfg: Config{BaseURL: srv.URL, HTTPClient: srv.Client(), UserName: "alice"}}

	type item struct {
		ID string `json:"id"`
	}
	got, _, err := getCollection[item](context.Background(), c, srv.URL+"/x")
	if err != nil {
		t.Fatalf("rejected continuation must not fail the collection: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("expected the two items from page 1, got %+v", got)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 2 requests (page 1 + the rejected link), got %d", calls)
	}

	// first-hop 400 is still an error
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"BadRequest","message":"nope"}}`)
	}))
	defer bad.Close()
	c2 := &Channel{cfg: Config{BaseURL: bad.URL, HTTPClient: bad.Client(), UserName: "alice"}}
	if _, _, err := getCollection[item](context.Background(), c2, bad.URL+"/x"); err == nil {
		t.Fatal("a 400 on the first request must still be an error")
	}
}
