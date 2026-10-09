// Package app wires a ReleaseBot handler from the environment.
//
// Both bootstrap binaries and the local harness call Build, so the local
// harness exercises the same construction path as the deployed function. A
// local runner that builds its own simplified wiring is a runner that proves
// nothing.
package app

import (
	"context"
	"fmt"

	"github.com/vamsiatluri/releasebot-migration/internal/config"
	"github.com/vamsiatluri/releasebot-migration/internal/githubclient"
	"github.com/vamsiatluri/releasebot-migration/internal/handler"
	"github.com/vamsiatluri/releasebot-migration/internal/obs"
	"github.com/vamsiatluri/releasebot-migration/internal/paramstore"
	"github.com/vamsiatluri/releasebot-migration/internal/release"
	"github.com/vamsiatluri/releasebot-migration/internal/slackclient"
)

func Build(ctx context.Context, action handler.Action, alias string, ackOnly bool) (*handler.Handler, error) {
	cfg, err := config.Load(ctx, config.EnvResolver)
	if err != nil {
		return nil, err
	}
	cfg = cfg.WithAlias(alias)

	// ── Per-environment configuration, read ONCE at INIT ────────────────────
	//
	// The version froze RARC_ENV -- a POINTER. The values behind it live at
	// /releasebot/<env>/ and are read here, so rotating one does not strand
	// every older version as a rollback target.
	//
	// A failure is deliberately NOT fatal. The function still holds working
	// environment variables, and Parameter Store being briefly unavailable
	// should degrade to the previous behaviour, not take the service down.
	// Which source won is recorded and reported on /health.
	cfg.ConfigSource, cfg.ConfigPath = "env", "/releasebot/"+cfg.Env+"/"
	if ps, err := paramstore.Load(ctx, cfg.Env); err != nil {
		obs.New("releasebot", cfg.Env).
			With("path", cfg.ConfigPath).With("reason", err.Error()).
			Warn("parameter store unavailable; falling back to environment variables")
	} else {
		cfg = cfg.WithParameters(ps.Values)
		cfg.ConfigSource, cfg.ConfigPath = ps.Source, ps.Path
	}

	log := obs.New(cfg.DatadogService, cfg.Env).With("action", string(action))
	log.With("config", fmt.Sprintf("%v", cfg.Redacted())).Info("releasebot starting")

	gh := githubclient.New(cfg.GitHubAPI, cfg.GitHubToken, cfg.DryRun)
	sl := slackclient.New(cfg.SlackAPI, cfg.SlackToken, cfg.DryRun)

	return &handler.Handler{
		Action:  action,
		Cfg:     cfg,
		Svc:     release.NewService(gh, log, cfg.DefaultBranch),
		Slack:   sl,
		Log:     log,
		AckOnly: ackOnly,
	}, nil
}
