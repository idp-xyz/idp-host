// Command idp-host is the IDP HOST: Agent Hosting & Coordination (ADR-0001).
//
//	idp-host [serve]                          run the API and the worker pool (default; IDP_HOST_WORKERS=0 for API only)
//	idp-host worker                           run the worker pool only (no API listener; scales independently)
//	idp-host migrate                          apply schema migrations (owner connection)
//	idp-host ensure-login USER                create/update a login role in idp_host_app;
//	                                          password from IDP_APP_DB_PASSWORD[_FILE]
//	idp-host seed-tenant SLUG [ID]            create a tenant, print TENANT_ID=<id>
//	idp-host dev-token TENANT SUBJECT ROLE..  print a development token (dev mode only)
//	idp-host new-key                          print a random credential key (hex)
//
// Environment:
//
//	IDP_DATABASE_URL             service connection (a member of idp_host_app, never a superuser)
//	IDP_MIGRATION_DATABASE_URL   schema owner connection (migrate, ensure-login, seed-tenant)
//	IDP_HOST_LISTEN              default 127.0.0.1:8450
//	IDP_HOST_CONFIG              providers and gateways file (YAML; see internal/config)
//	IDP_HOST_WORKERS             worker goroutines (default 2; 0 = API only)
//	IDP_HOST_INSTANCE            this replica's id (default: hostname); it names the worker's leases
//	IDP_HOST_LEASE               claim lease (default 30s)
//	IDP_HOST_CREDENTIAL_KEY[_FILE]  32-byte key (64 hex chars) that encrypts delegated gateway tokens
//	IDP_DEV_MODE                 "true": HS256 development tokens, plaintext model endpoints on loopback
//	IDP_HOST_ISSUER / IDP_HOST_AUDIENCE   accepted tokens (default idp-host-dev / idp-host)
//	IDP_HOST_JWKS_URL            production keys; IDP_DEV_SECRET[_FILE] (>= 32 bytes) in dev mode instead
//	IDP_HOST_TENANT_CLAIM / IDP_HOST_ROLES_CLAIM   claims (default tenant_id / roles)
//	IDP_HOST_TLS_CERT_FILE / IDP_HOST_TLS_KEY_FILE   serve HTTPS
//	IDP_IDENTITY_CA_FILE         extra trusted CAs for the JWKS and token endpoints
//	IDP_METRICS_LISTEN           serve Prometheus metrics (/metrics) on this address; off when unset
//	IDP_LOG_LEVEL                debug | info | warn | error
//	OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT / OTEL_EXPORTER_OTLP_HEADERS /
//	OTEL_EXPORTER_OTLP_CERTIFICATE / OTEL_SERVICE_NAME   OTLP/HTTP trace export (off without an endpoint)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"idp-host/adapters/identity"
	"idp-host/adapters/providers/anthropic"
	"idp-host/adapters/providers/gemini"
	"idp-host/adapters/providers/harnessworker"
	"idp-host/adapters/providers/openai"
	"idp-host/adapters/store/postgres"
	"idp-host/internal/a2a"
	"idp-host/internal/api"
	"idp-host/internal/cmiclient"
	"idp-host/internal/config"
	"idp-host/internal/credential"
	"idp-host/internal/guard"
	"idp-host/internal/harness"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/observability"
	"idp-host/internal/transport"
	"idp-host/internal/worker"
)

var version = "0.1.0-dev"

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func secret(key string) string {
	if v := env(key, ""); v != "" {
		return v
	}
	if f := env(key+"_FILE", ""); f != "" {
		b, err := os.ReadFile(f)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func main() {
	log := observability.NewLogger(os.Stdout, env("IDP_LOG_LEVEL", "info"))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log, true)
	case "worker":
		err = serve(log, false)
	case "migrate":
		var applied []string
		applied, err = postgres.Migrate(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""))
		for _, a := range applied {
			fmt.Println("applied", a)
		}
	case "ensure-login":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-host ensure-login USER")
			break
		}
		err = postgres.EnsureLogin(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), os.Args[2], secret("IDP_APP_DB_PASSWORD"))
	case "seed-tenant":
		if len(os.Args) < 3 {
			err = errors.New("usage: idp-host seed-tenant SLUG [ID]")
			break
		}
		id := ""
		if len(os.Args) > 3 {
			id = os.Args[3]
		}
		var out string
		if out, err = postgres.SeedTenant(context.Background(), env("IDP_MIGRATION_DATABASE_URL", ""), id, os.Args[2]); err == nil {
			fmt.Println("TENANT_ID=" + out)
		}
	case "dev-token":
		if len(os.Args) < 5 || env("IDP_DEV_MODE", "false") != "true" {
			err = errors.New("usage: idp-host dev-token TENANT SUBJECT ROLE... (dev mode only)")
			break
		}
		var tok string
		tok, err = identity.DevToken([]byte(secret("IDP_DEV_SECRET")), profile(), os.Args[3], os.Args[2], os.Args[4:], 12*time.Hour)
		fmt.Println(tok)
	case "new-key":
		var b [32]byte
		_, _ = rand.Read(b[:])
		fmt.Println(hex.EncodeToString(b[:]))
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error(cmd+" failed", "error", err)
		os.Exit(1)
	}
}

func profile() identity.Profile {
	return identity.Profile{Issuer: env("IDP_HOST_ISSUER", "idp-host-dev"), Audience: env("IDP_HOST_AUDIENCE", "idp-host"), JWKSURL: env("IDP_HOST_JWKS_URL", ""),
		TenantClaim: env("IDP_HOST_TENANT_CLAIM", "tenant_id"), RolesClaim: env("IDP_HOST_ROLES_CLAIM", "roles")}
}

// serve runs the process: the API with a worker pool (apiEnabled), or the worker pool alone.
func serve(log *slog.Logger, apiEnabled bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tp, stopTracing, err := observability.Setup("idp-host", version, log)
	if err != nil {
		return err
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopTracing(fctx)
	}()
	dev := env("IDP_DEV_MODE", "false") == "true"
	identityTLS, err := transport.ClientTLS(env("IDP_IDENTITY_CA_FILE", ""), "", "")
	if err != nil {
		return fmt.Errorf("IDP_IDENTITY_CA_FILE: %w", err)
	}
	identityHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: identityTLS}}
	prof := profile()
	var verifier *identity.Verifier
	switch {
	case !apiEnabled: // the worker authenticates nobody
	case prof.JWKSURL != "":
		verifier = identity.NewVerifier(prof, &identity.JWKS{URL: prof.JWKSURL, HTTP: identityHTTP, TTL: 10 * time.Minute, MinRefresh: 30 * time.Second})
	case dev && secret("IDP_DEV_SECRET") != "":
		if verifier, err = identity.NewDevVerifier(prof, []byte(secret("IDP_DEV_SECRET"))); err != nil {
			return err
		}
	default:
		return errors.New("configure IDP_HOST_JWKS_URL (or, in dev mode, IDP_DEV_SECRET)")
	}

	var box *credential.Box
	if k := secret("IDP_HOST_CREDENTIAL_KEY"); k != "" {
		raw, err := hex.DecodeString(k)
		if err != nil {
			return errors.New("IDP_HOST_CREDENTIAL_KEY must be 64 hex characters")
		}
		if box, err = credential.NewBox(raw); err != nil {
			return err
		}
	}
	store, err := postgres.Open(ctx, env("IDP_DATABASE_URL", ""), box)
	if err != nil {
		return err
	}
	defer store.Close()

	cfg := &config.File{}
	if path := env("IDP_HOST_CONFIG", ""); path != "" {
		if cfg, err = config.Load(path); err != nil {
			return err
		}
	}
	var providers []model.Provider
	for _, p := range cfg.Providers {
		hc, err := model.NewHTTPClient(2*time.Minute, p.CAFile)
		if err != nil {
			return fmt.Errorf("provider %s: %w", p.Name, err)
		}
		if p.Insecure && !dev {
			return fmt.Errorf("provider %s: insecure is allowed in development mode only", p.Name)
		}
		switch p.Kind {
		case "openai":
			usage := true
			if p.UsageInStream != nil {
				usage = *p.UsageInStream
			}
			prov, err := openai.New(openai.Config{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, HTTP: hc, UsageInStream: usage, MaxTokensField: p.MaxTokensField, AllowPlainHTTP: p.Insecure})
			if err != nil {
				return err
			}
			providers = append(providers, prov)
		case "anthropic":
			prov, err := anthropic.New(anthropic.Config{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, HTTP: hc, AllowPlainHTTP: p.Insecure})
			if err != nil {
				return err
			}
			providers = append(providers, prov)
		case "gemini":
			prov, err := gemini.New(gemini.Config{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, HTTP: hc, AllowPlainHTTP: p.Insecure})
			if err != nil {
				return err
			}
			providers = append(providers, prov)
		case "harness-worker": // ADR-0001 D31: an external harness behind the HOST's step protocol
			prov, err := harnessworker.New(harnessworker.Config{Name: p.Name, Endpoint: p.BaseURL, APIKey: p.APIKey, HTTP: hc, AllowPlainHTTP: p.Insecure})
			if err != nil {
				return err
			}
			providers = append(providers, prov)
		}
	}
	registry := model.NewRegistry(providers...)
	classifiers := map[string]guard.Classifier{}
	for _, c := range cfg.Classifiers {
		if c.Insecure && !dev {
			return fmt.Errorf("classifier %s: insecure is allowed in development mode only", c.Name)
		}
		endpoint, err := model.CheckEndpoint(c.Endpoint, c.Insecure)
		if err != nil {
			return fmt.Errorf("classifier %s: %w", c.Name, err)
		}
		hc, err := model.NewHTTPClient(10*time.Second, c.CAFile)
		if err != nil {
			return fmt.Errorf("classifier %s: %w", c.Name, err)
		}
		classifiers[c.Name] = &guard.HTTPClassifier{Name: c.Name, Endpoint: endpoint, APIKey: c.APIKey, HTTP: hc, Timeout: time.Duration(c.TimeoutSeconds) * time.Second}
	}
	var a2aClient *a2a.Client
	if len(cfg.A2APeers) > 0 {
		peers := cfg.A2APeers
		caFile := ""
		for _, p := range peers {
			if p.CAFile != "" {
				caFile = p.CAFile
			}
		}
		hc, err := model.NewHTTPClient(30*time.Second, caFile)
		if err != nil {
			return fmt.Errorf("a2aPeers: %w", err)
		}
		a2aClient = &a2a.Client{HTTP: hc, Bearer: func(cardURL string) string {
			for _, p := range peers {
				if strings.HasPrefix(cardURL, p.URLPrefix) {
					return p.APIKey
				}
			}
			return ""
		}}
	}
	gateways := map[string]*harness.Gateway{}
	gatewayAuth := map[string]api.GatewayAuth{}
	for _, g := range cfg.Gateways {
		endpoint, err := mcpclient.CheckEndpoint(g.Endpoint)
		if err != nil {
			return fmt.Errorf("gateway %s: %w", g.Name, err)
		}
		tlsCfg, err := transport.ClientTLS(g.CAFile, "", "")
		if err != nil {
			return fmt.Errorf("gateway %s: %w", g.Name, err)
		}
		gw := &harness.Gateway{Name: g.Name, Endpoint: endpoint, Client: &mcpclient.Client{HTTP: mcpclient.NewHTTPClient(60*time.Second, tlsCfg), Info: mcpclient.ClientInfo{Name: "idp-host", Version: version}}}
		auth := api.GatewayAuth{}
		switch g.Auth.Mode {
		case "static":
			gw.StaticBearer = g.Auth.APIKey
		case "delegation":
			if box == nil {
				return fmt.Errorf("gateway %s: delegation needs IDP_HOST_CREDENTIAL_KEY", g.Name)
			}
			tokenTLS, err := transport.ClientTLS(g.Auth.CAFile, "", "")
			if err != nil {
				return fmt.Errorf("gateway %s: %w", g.Name, err)
			}
			auth.Exchanger = &credential.Exchanger{TokenEndpoint: g.Auth.TokenEndpoint, ClientID: g.Auth.ClientID, ClientSecret: g.Auth.ClientSecret, Audience: g.Auth.Audience,
				HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tokenTLS}}}
			auth.ActorToken = g.Auth.ActorToken
		}
		gateways[g.Name] = gw
		gatewayAuth[g.Name] = auth
	}

	var cmiClient *cmiclient.Client
	var cmiKey string
	var cmiAuth api.GatewayAuth
	if c := cfg.CMI; c != nil { // ADR-0001 D32
		endpoint, err := mcpclient.CheckEndpoint(c.Endpoint)
		if err != nil {
			return fmt.Errorf("cmi: %w", err)
		}
		hc, err := model.NewHTTPClient(10*time.Second, c.CAFile)
		if err != nil {
			return fmt.Errorf("cmi: %w", err)
		}
		cmiClient = &cmiclient.Client{Endpoint: endpoint, HTTP: hc}
		switch c.Auth.Mode {
		case "static":
			cmiKey = c.Auth.APIKey
		case "delegation":
			tokenTLS, err := transport.ClientTLS(c.Auth.CAFile, "", "")
			if err != nil {
				return fmt.Errorf("cmi: %w", err)
			}
			cmiAuth = api.GatewayAuth{ActorToken: c.Auth.ActorToken, Exchanger: &credential.Exchanger{TokenEndpoint: c.Auth.TokenEndpoint, ClientID: c.Auth.ClientID, ClientSecret: c.Auth.ClientSecret, Audience: c.Auth.Audience,
				HTTP: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tokenTLS}}}}
		}
	}
	metrics := observability.NewMetrics()
	if ms := observability.ServeMetrics(env("IDP_METRICS_LISTEN", ""), metrics, log); ms != nil {
		defer ms.Close()
	}
	workers, _ := strconv.Atoi(env("IDP_HOST_WORKERS", "2"))
	lease, err := time.ParseDuration(env("IDP_HOST_LEASE", "30s"))
	if err != nil {
		return fmt.Errorf("IDP_HOST_LEASE: %w", err)
	}
	if !apiEnabled && workers <= 0 {
		return errors.New("idp-host worker needs IDP_HOST_WORKERS >= 1")
	}
	hostname, _ := os.Hostname()
	var wg sync.WaitGroup
	wctx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	if workers > 0 {
		h := &harness.Harness{Store: store, Providers: registry, Gateways: gateways, Classifiers: classifiers, A2A: a2aClient, CMI: cmiClient, CMIKey: cmiKey, Metrics: worker.NewMetrics(metrics), Log: log}
		w := &worker.Worker{ID: env("IDP_HOST_INSTANCE", hostname), Store: store, Harness: h, Concurrency: workers, Lease: lease, Log: log}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(wctx)
		}()
	}
	if !apiEnabled {
		log.Info("idp-host worker running", "instance", env("IDP_HOST_INSTANCE", hostname), "workers", workers, "providers", registry.Names(), "gateways", len(gateways))
		<-ctx.Done()
		cancelWorkers()
		wg.Wait()
		return nil
	}
	srvAPI := &api.API{Store: store, Auth: verifier, Gateways: gatewayAuth, Providers: registry.Names(), Log: log, Metrics: metrics, Version: version, CMI: cmiClient, CMIKey: cmiKey, CMIAuth: cmiAuth}
	srv := &http.Server{Addr: env("IDP_HOST_LISTEN", "127.0.0.1:8450"), Handler: observability.HTTPMiddleware(tp, "idp-host/api", srvAPI.Handler()),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	if cert := env("IDP_HOST_TLS_CERT_FILE", ""); cert != "" {
		if srv.TLSConfig, err = transport.ServerTLS(cert, env("IDP_HOST_TLS_KEY_FILE", "")); err != nil {
			return fmt.Errorf("TLS: %w", err)
		}
	}
	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ListenAndServeTLS("", "")
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	log.Info("idp-host listening", "addr", srv.Addr, "devMode", dev, "tls", srv.TLSConfig != nil, "workers", workers, "providers", registry.Names(), "gateways", len(gateways), "classifiers", len(classifiers))
	select {
	case err := <-errc:
		cancelWorkers()
		wg.Wait()
		return err
	case <-ctx.Done():
	}
	cancelWorkers()
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	wg.Wait()
	return err
}
