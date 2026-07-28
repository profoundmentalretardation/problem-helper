// Command helper is the Problem Helper MVP service: HTTP server + worker
// goroutines + cron, all in one binary.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
)

func main() {
	agentsPath := flag.String("agents", "agents.yaml", "path to agents.yaml")
	flag.Parse()

	cfg, err := config.Load(*agentsPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	log.Printf("problem-helper starting: platform=%s repair_model=%s hint_model=%s guardrail_model=%s curator_model=%s",
		cfg.Env.Platform, cfg.Agents.Repair.Model, cfg.Agents.Hint.Model, cfg.Agents.Guardrail.Model, cfg.Agents.Curator.Model)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.Env.DatabaseURL)
	if err != nil {
		log.Fatalf("store: connecting: %v", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("store: migrating: %v", err)
	}
	log.Print("problem-helper: migrations applied")
}
