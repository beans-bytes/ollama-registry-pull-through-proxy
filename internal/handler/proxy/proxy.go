package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"ollama-registry-pull-through-cache/internal/worker/cache_worker"
	"os"
	"path"
	"strings"

	"github.com/rs/zerolog/log"
)

const registryOllamaAI = "https://registry.ollama.ai"

type clientBaseURLKey struct{}

// clientVisibleBaseURL builds scheme://host as the client would use to reach this
// service (before Handler rewrites Host/URL for upstream). Honors X-Forwarded-Proto
// and X-Forwarded-Host when present (e.g. behind a TLS-terminating proxy).
func clientVisibleBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	if host == "" {
		return ""
	}
	return strings.ToLower(scheme) + "://" + host
}

// ConfigureWWWAuthenticateRewrite sets ModifyResponse on p so each WWW-Authenticate
// value has registryOllamaAI replaced by the client-visible base URL stored on the
// request context by Handler (Host/URL are rewritten for upstream before the proxy runs).
func ConfigureWWWAuthenticateRewrite(p *httputil.ReverseProxy) {
	p.ModifyResponse = func(resp *http.Response) error {
		if resp.Request == nil {
			return nil
		}
		base, _ := resp.Request.Context().Value(clientBaseURLKey{}).(string)
		if base == "" {
			return nil
		}
		vals := resp.Header.Values("WWW-Authenticate")
		if len(vals) == 0 {
			return nil
		}
		resp.Header.Del("WWW-Authenticate")
		for _, v := range vals {
			modified := strings.ReplaceAll(v, registryOllamaAI, base)
			log.Info().Str("component", "proxy").Str("www_authenticate", modified).Msg("WWW-Authenticate rewritten")
			resp.Header.Add("WWW-Authenticate", modified)
		}
		return nil
	}
}

func Handler(p *httputil.ReverseProxy, cacheDir string, upstream url.URL) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientBaseURLKey{}, clientVisibleBaseURL(r))
		r = r.WithContext(ctx)

		r.Host = upstream.Host
		r.URL.Host = upstream.Host
		r.URL.Scheme = upstream.Scheme

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Serve from upstream. Log
			log.Info().Str("component", "handler").Msgf("Method %s not supported, serving from upstream", r.Method)
			p.ServeHTTP(w, r)
			return
		}

		// Check if file with r.Path exists in cachedir. If yes, serve it
		cachePath := path.Join(cacheDir, r.URL.Path)
		if _, err := os.Stat(cachePath); err == nil {
			// File does exist. Serve from cache
			log.Info().Str("component", "handler").Str("source", "CACHE").Msgf("%s %s", r.Method, r.URL.Path)

			if strings.Contains(r.URL.Path, "/blobs/") {
				log.Info().Str("component", "handler").Msgf("Adding location HTTP header")
				w.Header().Set("location", r.URL.Path)
			}

			http.ServeFile(w, r, cachePath)
			return
		}

		// File does not exist in cache. Queue the download & serve from upstream
		log.Info().Str("component", "handler").Str("source", "ORIGIN").Msgf("%s %s", r.Method, r.URL.Path)

		cache_worker.QueueFileForDownload(cachePath)
		p.ServeHTTP(w, r)
	}
}
