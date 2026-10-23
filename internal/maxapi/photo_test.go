package maxapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPUploadPhotoWithoutPhotoIDInURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"photos":{"abc123":{"token":"tok-1"}}}`))
	}))
	defer srv.Close()

	url := srv.URL + "/uploadImage?r=opaque"
	if _, err := photoIDFromUploadURL(url); err == nil {
		t.Fatal("expected no photoIds in the URL")
	}
	tok, err := httpUploadPhoto(url, []byte("jpeg"), "")
	if err != nil || tok != "tok-1" {
		t.Fatalf("token = %q, err = %v; want tok-1", tok, err)
	}
}
