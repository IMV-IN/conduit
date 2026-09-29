// Command conduit: serve | validate | simulate | doctor | version | healthcheck.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/conduit/internal/config"
	"github.com/yourorg/conduit/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: conduit <serve|validate|simulate|doctor|version|healthcheck> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "validate":
		cfgPath := flagVal("--config", "conduit.example.yaml")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid:", err)
			os.Exit(1)
		}
		fmt.Printf("valid: %d models, %d policies\n", len(cfg.Models), len(cfg.Policies))
	case "healthcheck":
		// Used by Docker HEALTHCHECK against the local data plane.
		url := os.Getenv("CONDUIT_HEALTH_URL")
		if url == "" {
			url = "http://localhost:8080/healthz"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
	case "simulate":
		cfgPath := flagVal("--config", "conduit.example.yaml")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid:", err)
			os.Exit(1)
		}
		gw := server.New(cfg)
		_ = gw
		out, _ := json.Marshal(map[string]any{"policies": len(cfg.Policies), "note": "full traffic replay lands in Phase 4 (internal/replay)"})
		fmt.Println(string(out))
	case "doctor":
		cfgPath := flagVal("--config", "conduit.example.yaml")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(1)
		}
		ok := true
		for _, p := range cfg.Providers {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			req, _ := http.NewRequestWithContext(ctx, "GET", p.BaseURL, nil)
			_, err := http.DefaultClient.Do(req)
			cancel()
			status := "ok"
			if err != nil {
				status = "unreachable (" + err.Error() + ")"
				// mock base_url /v1 without trailing path still resolves; don't fail doctor on 404.
				if p.Name == "mock" {
					status += " — is the mock running?"
				}
			}
			fmt.Printf("provider %-10s %-40s %s\n", p.Name, p.BaseURL, status)
			_ = ok
		}
		fmt.Printf("models: %d  policies: %d\n", len(cfg.Models), len(cfg.Policies))
	case "serve":
		cfgPath := flagVal("--config", "conduit.example.yaml")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			os.Exit(1)
		}
		gw := server.New(cfg)
		dataAddr := cfg.Server.Listen
		adminAddr := cfg.Server.AdminListen
		if v := os.Getenv("PORT"); v != "" && cfgPath == "conduit.example.yaml" {
			dataAddr = ":" + v
		}
		dataSrv := &http.Server{Addr: dataAddr, Handler: gw.DataMux(), ReadHeaderTimeout: 5 * time.Second}
		adminSrv := &http.Server{Addr: adminAddr, Handler: gw.AdminMux(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			fmt.Printf("conduit %s data=%s admin=%s\n", version, dataAddr, adminAddr)
			if err := dataSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "data:", err)
			}
		}()
		go func() {
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "admin:", err)
			}
		}()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		grace := cfg.Server.ShutdownGrace
		if grace == 0 {
			grace = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		_ = dataSrv.Shutdown(ctx)
		_ = adminSrv.Shutdown(ctx)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func flagVal(name, def string) string {
	for i := 2; i < len(os.Args)-1; i++ {
		if os.Args[i] == name {
			return os.Args[i+1]
		}
		if len(os.Args[i]) > len(name)+1 && os.Args[i][:len(name)+1] == name+"=" {
			return os.Args[i][len(name)+1:]
		}
	}
	return def
}
