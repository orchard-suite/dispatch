package server

import (
	"embed"
	"net/http"
)

// A Home Screen or a desktop install fetches the icon and the manifest a
// page names. The device fetches them without a session, so both the desk
// and the status page serve them to anyone, ahead of any sign-in check.
//
//go:embed install
var installFS embed.FS

// installFiles maps each path a device fetches to its content type.
var installFiles = map[string]string{
	"/apple-touch-icon.png": "image/png",
	"/icon-192.png":         "image/png",
	"/icon-512.png":         "image/png",
	"/manifest.webmanifest": "application/manifest+json",
}

// serveInstall adds the install files to mux, outside any gate.
func serveInstall(mux *http.ServeMux) {
	for path, kind := range installFiles {
		body, err := installFS.ReadFile("install" + path)
		if err != nil {
			panic(err)
		}
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(body)
		})
	}
}
