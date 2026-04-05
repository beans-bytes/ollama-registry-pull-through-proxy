package main

import (
	"fmt"
	"github.com/alecthomas/kong"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"ollama-registry-pull-through-cache/internal/handler/proxy"
	"ollama-registry-pull-through-cache/internal/worker/cache_worker"
	"ollama-registry-pull-through-cache/internal/worker/invalidate_manifests_worker"
	"ollama-registry-pull-through-cache/pkg/dumptransport"
	"os"
	"time"
)

var cli struct {
	Port                    int           `kong:"default='9200',env='PORT',help='port to listen on'"`
	UpstreamAddress         string        `kong:"default='https://registry.ollama.ai/',env='UPSTREAM_ADDRESS',help='upstream address to connect to. Can be IP or name, later one will be resolved'"`
	DumpUpstreamRequests    bool          `kong:"env='DUMP_UPSTREAM_REQUESTS',help='If set to true then all upstream request and responses will be dumped to the console'"`
	CacheDir                string        `kong:"default='./cache_dir',env='CACHE_DIR',help='What directory to use as base for the cache'"`
	NumberOfDownloadWorkers int           `kong:"default='1',env='NUM_DOWNLOAD_WORKERS',help='Number of parallel workers to use for downloading files'"`
	ManifestLifetime        time.Duration `kong:"default='240h',env='MANIFEST_LIFETIME',help='How long to keep manifests in cache before invalidating them. Default 10 days.'"`
	LogLevel                string        `kong:"default='info',env='LOG_LEVEL',help='Log level to use. Can be debug, info, warn, error'"`
	LogFormatJSON           bool          `kong:"env='LOG_FORMAT_JSON',help='If set to true, then log as JSON output'"`
}

func main() {
	kong.Parse(&cli)

	// Setup Logger
	if !cli.LogFormatJSON {
		// Use Human readable (console out) log format
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
	}
	switch cli.LogLevel {
	case "debug":
		log.Logger.Level(zerolog.DebugLevel)
	case "info":
		log.Logger.Level(zerolog.InfoLevel)
	case "warn":
		log.Logger.Level(zerolog.WarnLevel)
	case "error":
		log.Logger.Level(zerolog.ErrorLevel)
	}

	// Validate upstream address
	upstream, err := url.Parse(cli.UpstreamAddress)
	if err != nil {
		log.Error().Str("component", "main").Err(err).Msg("failed to parse upstream address")
	}
	if upstream.Host == "" {
		log.Error().Str("component", "main").Msg("Failed to parse upstream address, no host found")
	}
	if upstream.Scheme == "" {
		log.Error().Str("component", "main").Msg("Failed to parse upstream address, no scheme found")
	}

	go invalidate_manifests_worker.Run(cli.CacheDir, cli.ManifestLifetime)
	for i := 0; i < cli.NumberOfDownloadWorkers; i++ {
		go cache_worker.Run(cli.CacheDir, upstream)
	}

	singleHostReverseProxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ConfigureWWWAuthenticateRewrite(singleHostReverseProxy)
	if cli.DumpUpstreamRequests {
		singleHostReverseProxy.Transport = &dumptransport.Transport{}
	}
	http.HandleFunc("/", proxy.Handler(
		singleHostReverseProxy,
		cli.CacheDir,
		*upstream))

	log.Info().Str("component", "main").Msgf("Starting server on port %d", cli.Port)
	log.Error().Err(http.ListenAndServe(fmt.Sprintf(":%d", cli.Port), nil))
}
