package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Home Screen or a desktop install fetches the icon and the manifest the
// page names, with no session. A path that misses gives the device nothing
// to draw, and it draws a letter instead.
func TestInstallAsApp(t *testing.T) {
	h := newHarness(t)
	sizes := map[string]string{"/apple-touch-icon.png": "180x180", "/icon-192.png": "192x192", "/icon-512.png": "512x512"}
	for port, handler := range map[string]http.Handler{"desk": h.session, "status": h.status} {
		get := func(path string) *httptest.ResponseRecorder {
			return h.do(handler, httptest.NewRequest(http.MethodGet, path, nil))
		}
		for path, want := range sizes {
			rec := get(path)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("%s %s: %d %s", port, path, rec.Code, rec.Header().Get("Content-Type"))
			}
			png := rec.Body.Bytes()
			if len(png) < 24 || !bytes.HasPrefix(png, []byte("\x89PNG")) {
				t.Fatalf("%s %s: not a PNG", port, path)
			}
			if got := itoa(int64(binary.BigEndian.Uint32(png[16:]))) + "x" + itoa(int64(binary.BigEndian.Uint32(png[20:]))); got != want {
				t.Errorf("%s %s: %s, want %s", port, path, got, want)
			}
		}
		rec := get("/manifest.webmanifest")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/manifest+json" {
			t.Fatalf("%s manifest: %d %s", port, rec.Code, rec.Header().Get("Content-Type"))
		}
		var manifest struct {
			Display  string `json:"display"`
			StartURL string `json:"start_url"`
			Icons    []struct {
				Src     string `json:"src"`
				Sizes   string `json:"sizes"`
				Purpose string `json:"purpose"`
			} `json:"icons"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Display != "standalone" || manifest.StartURL != "/" || len(manifest.Icons) != 3 {
			t.Errorf("%s manifest: %+v", port, manifest)
		}
		for _, icon := range manifest.Icons {
			if sizes[icon.Src] != icon.Sizes {
				t.Errorf("%s manifest names %s as %s", port, icon.Src, icon.Sizes)
			}
		}
	}

	desk := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/", nil), member, "User")).Body.String()
	status := h.do(h.status, httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()
	for name, page := range map[string]string{"desk": desk, "status": status} {
		head, _, _ := strings.Cut(page, "</head>")
		for _, want := range []string{`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`, `<link rel="manifest" href="/manifest.webmanifest">`, `<meta name="theme-color"`, `<meta name="apple-mobile-web-app-title" content="Dispatch">`} {
			if !strings.Contains(head, want) {
				t.Errorf("%s head lacks %s", name, want)
			}
		}
	}
}
