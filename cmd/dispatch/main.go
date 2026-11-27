// Command dispatch is the incident desk server.
package main

import (
	// The image carries no zone files; schedules, quiet hours, and the
	// desk's night are all kept in a named zone.
	_ "time/tzdata"

	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/assist"
	"github.com/orchard-suite/dispatch/internal/azuremonitor"
	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/github"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/mail"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/server"
	"github.com/orchard-suite/dispatch/internal/signals"
	"github.com/orchard-suite/dispatch/internal/sms"
	"github.com/orchard-suite/dispatch/internal/store"
	"github.com/orchard-suite/dispatch/internal/teams"
)

// version is set at build time from VERSION.
var version = "dev"

func main() {
	if ran, err := cli(os.Args[1:], os.Stdout); ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, "dispatch:", err)
			os.Exit(1)
		}
		return
	}
	log := telemetry.NewLogger(os.Stdout, metrics.Service, version, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("dispatch stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := env("DATA_DIR", "/data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	cfg := server.Config{
		PublicURL:         os.Getenv("PUBLIC_URL"),
		AlertmanagerToken: os.Getenv("ALERTMANAGER_TOKEN"),
		DeskRole:          env("DESK_ROLE", "User"),
		AdminRole:         env("ADMIN_ROLE", "Admin"),
		FaroURL:           os.Getenv("FARO_URL"),
		FaroKey:           os.Getenv("FARO_API_KEY"),
		Version:           version,
		SMSSenderName:     os.Getenv("SMS_SENDER_NAME"),
		SMSPolicyURL:      os.Getenv("SMS_POLICY_URL"),
	}

	shutdownTelemetry, err := telemetry.Setup(ctx, metrics.Service, version)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "dispatch.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	var d *desk.Desk
	m, err := metrics.New(func() metrics.Stats { return d.Stats() })
	if err != nil {
		return err
	}
	hooks := notify.New(os.Getenv("NOTIFY_WEBHOOK_URL"), log, m.Notification)
	hooks.SignWith(os.Getenv("NOTIFY_WEBHOOK_SECRET"))
	catalog := orrery.New(orrery.Config{
		URL: os.Getenv("ORRERY_URL"), Token: os.Getenv("ORRERY_TOKEN"), PushToken: os.Getenv("ORRERY_PUSH_TOKEN"), DeskURL: cfg.PublicURL,
	}, log, m.OrreryPush)
	decisions := annals.New(os.Getenv("ANNALS_URL"), os.Getenv("ANNALS_TOKEN"), log, m.AnnalsPost)
	sealer, err := seal.New(os.Getenv("SECRET_KEY"))
	if err != nil {
		return err
	}
	box := outbox.New(st, sealer, catalog.Reach, log, m.Delivery)
	box.UseCounters(m.Message, m.Page)
	box.UseAnnals(decisions)
	box.UseDesk(cfg.PublicURL)
	mailer, err := mail.New(mail.FromEnv())
	if err != nil {
		return err
	}
	mailKind := "off"
	if mailer != nil {
		box.UseMail(mailer)
		mailKind = mailer.Kind()
	}
	var texts *sms.Client
	if endpoint := os.Getenv("SMS_ENDPOINT"); endpoint != "" {
		appID := env("SMS_APP_ID", os.Getenv("AZURE_CLIENT_ID"))
		if appID == "" {
			// The pod's Azure identity is not granted yet: run without SMS.
			log.Warn("sms off", "reason", "SMS_ENDPOINT is set but no app or managed identity signs in to it")
		} else {
			texts, err = sms.New(sms.Config{
				Endpoint: endpoint, From: os.Getenv("SMS_FROM"), AppID: appID,
				TenantID: env("SMS_TENANT_ID", os.Getenv("AZURE_TENANT_ID")), TokenFile: env("SMS_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE")),
			})
			if err != nil {
				return err
			}
			box.UseMedium(notice.SMS, sms.Person{C: texts})
		}
	}
	var dbot *discord.Bot
	if token := os.Getenv("DISCORD_BOT_TOKEN"); token != "" {
		if dbot, err = discord.New(token, os.Getenv("DISCORD_PUBLIC_KEY")); err != nil {
			return err
		}
		box.UseMedium(notice.Discord, dbot)
		box.UseDiscord(dbot)
	}
	d = desk.New(st, m, hooks, catalog, decisions, box, log, cfg.PublicURL)
	d.UseStatusURL(os.Getenv("STATUS_URL"))
	d.UseVersion(version)
	// The watch reads Azure Monitor as the pod's workload identity, when it
	// has one; without it every Azure Monitor store is blind.
	var watchTokens signals.Tokens
	if appID := env("WATCH_APP_ID", os.Getenv("AZURE_CLIENT_ID")); appID != "" {
		signer, err := entra.New(entra.Config{Name: "the watch", AppID: appID, TenantID: env("WATCH_TENANT_ID", os.Getenv("AZURE_TENANT_ID")),
			TokenFile: env("WATCH_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE")), Scope: signals.AzureMonitorScope})
		if err != nil {
			log.Warn("watch reads no Azure Monitor", "reason", err.Error())
		} else {
			watchTokens = signer
		}
	}
	d.UseSignals(signals.NewOpener(watchTokens))
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		d.UseIssues(github.New(token))
	}
	if backend := os.Getenv("ASSIST_BACKEND"); backend != "" {
		// The assistant's data tools are the read tools the hub lends its
		// own token. With no door it still reads the incident's record.
		tools := append([]string{}, desk.IncidentTools...)
		door := orrery.NewDoor(os.Getenv("ORRERY_URL"), os.Getenv("ASSIST_TOOLS_TOKEN"), log)
		if door != nil {
			tools = append(tools, desk.HubTool)
			d.UseToolDoor(door)
		} else {
			log.Info("assistant has no data tools: set ORRERY_URL and ASSIST_TOOLS_TOKEN to lend it the hub's read tools")
		}
		a, err := assist.New(assist.Config{
			Ion: os.Getenv("ION_BIN"), DataDir: os.Getenv("ION_DATA_DIR"), Socket: os.Getenv("ION_SOCKET_PATH"),
			Backend: backend, Model: os.Getenv("ASSIST_MODEL"), BaseURL: os.Getenv("ASSIST_BASE_URL"),
			Tools: tools,
		})
		if err != nil {
			return fmt.Errorf("assistant: %w", err)
		}
		defer a.Close()
		d.UseAssistant(a)
		log.Info("assistant on", "backend", backend, "hub_tools", door != nil)
	}
	srv, err := server.New(cfg, d, m, log)
	if err != nil {
		return err
	}
	if texts != nil {
		srv.UseSMS(texts)
	}
	if dbot != nil {
		srv.UseDiscord(dbot)
	}
	azureMonitorOn := false
	if appID := os.Getenv("AZURE_MONITOR_APP_ID"); appID != "" {
		door, err := azuremonitor.NewDoor(azuremonitor.DoorConfig{AppID: appID, TenantID: env("AZURE_MONITOR_TENANT_ID", os.Getenv("AZURE_TENANT_ID")),
			Client: telemetry.Client(20 * time.Second)})
		if err != nil {
			return err
		}
		srv.UseAzureMonitor(door)
		azureMonitorOn = true
	}
	botOn := false
	if appID := os.Getenv("BOT_APP_ID"); appID != "" {
		bot, err := teams.New(teams.Config{
			AppID: appID, TenantID: env("BOT_TENANT_ID", os.Getenv("AZURE_TENANT_ID")),
			TokenFile: env("BOT_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE")), Connection: env("BOT_CONNECTION", "orrery"),
		})
		if err != nil {
			return err
		}
		srv.UseBot(server.BotConfig{Bot: bot, ActsFor: env("BOT_ACTS_FOR", "orrery"), ProductURL: os.Getenv("ORRERY_URL")})
		box.UseBot(srv.BotSender())
		box.UseTeams(srv.BotSender())
		botOn = true
		if team := os.Getenv("INCIDENT_TEAM_ID"); team != "" {
			graph, err := entra.New(entra.Config{Name: "the bot", AppID: appID, TenantID: env("BOT_TENANT_ID", os.Getenv("AZURE_TENANT_ID")),
				TokenFile: env("BOT_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE")), Scope: "https://graph.microsoft.com/.default"})
			if err != nil {
				return err
			}
			rooms, err := srv.TeamsRooms(team, graph)
			if err != nil {
				return err
			}
			d.UseRooms(rooms, incident.ParseSeverity(env("INCIDENT_ROOMS_FROM", "critical")))
		}
	}
	go box.Run(ctx)
	go d.Watch(ctx)

	listeners := []struct {
		name, addr string
		handler    http.Handler
	}{
		{"session", env("SESSION_ADDR", ":8080"), srv.Session()},
		{"api", env("API_ADDR", ":8081"), srv.API()},
	}
	if addr := os.Getenv("STATUS_ADDR"); addr != "" {
		listeners = append(listeners, struct {
			name, addr string
			handler    http.Handler
		}{"status", addr, srv.Status()})
	}
	errs := make(chan error, len(listeners))
	servers := make([]*http.Server, 0, len(listeners))
	for _, l := range listeners {
		hs := &http.Server{
			Addr:              l.addr,
			Handler:           l.handler,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		servers = append(servers, hs)
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	log.Info("dispatch started", "public_url", cfg.PublicURL, "status", os.Getenv("STATUS_ADDR") != "",
		"shared_secret", cfg.AlertmanagerToken != "", "notify", hooks.Enabled(), "orrery_resolves", catalog.Resolves(), "orrery_pushes", catalog.Pushes(), "annals", decisions.Enabled(), "channels", sealer.Keeps(), "teams_bot", botOn, "mail", mailKind, "sms", texts != nil, "discord", dbot != nil, "azure_monitor", azureMonitorOn, "assistant", d.HasAssistant(), "watch_azure_monitor", watchTokens != nil)

	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	log.Info("dispatch stopping")
	done, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, hs := range servers {
		_ = hs.Shutdown(done)
	}
	return errors.Join(err, shutdownTelemetry(done))
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
