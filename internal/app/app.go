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
	"github.com/vamsiatluri/releasebot-migration/internal/release"
	"github.com/vamsiatluri/releasebot-migration/internal/slackclient"
)

func Build(ctx context.Context, action handler.Action, alias string, ackOnly bool) (*handler.Handler, error) {
	cfg, err := config.Load(ctx, config.EnvResolver)
	if err != nil {
		return nil, err
	}
	cfg = cfg.WithAlias(alias)

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
