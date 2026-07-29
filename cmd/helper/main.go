// Command helper is the Problem Helper MVP service: HTTP server + worker
// goroutines + cron, all in one binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/hint"
	"github.com/profoundmentalretardation/problem-helper/internal/agent/repair"
	"github.com/profoundmentalretardation/problem-helper/internal/api"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/format"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/mock"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// requiredPromptNames are the templates the wired agents need; startup
// fails if any is missing from the loaded prompts directory.
var requiredPromptNames = []string{"repair", "hint", "guardrail"}

func main() {
	agentsPath := flag.String("agents", "agents.yaml", "path to agents.yaml")
	promptsDir := flag.String("prompts", "prompts", "path to the prompts directory")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "grace period for in-flight HTTP requests on shutdown")
	flag.Parse()

	if err := run(*agentsPath, *promptsDir, *addr, *shutdownTimeout); err != nil {
		log.Fatal(err)
	}
}

func run(agentsPath, promptsDir, addr string, shutdownTimeout time.Duration) error {
	cfg, err := config.Load(agentsPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	log.Printf("problem-helper starting: platform=%s repair_model=%s hint_model=%s guardrail_model=%s curator_model=%s",
		cfg.Env.Platform, cfg.Agents.Repair.Model, cfg.Agents.Hint.Model, cfg.Agents.Guardrail.Model, cfg.Agents.Curator.Model)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.Env.DatabaseURL)
	if err != nil {
		return fmt.Errorf("store: connecting: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("store: migrating: %w", err)
	}
	log.Print("problem-helper: migrations applied")

	templates, err := prompt.LoadDir(promptsDir)
	if err != nil {
		return fmt.Errorf("prompt: loading %s: %w", promptsDir, err)
	}
	for _, name := range requiredPromptNames {
		if _, ok := templates[name]; !ok {
			return fmt.Errorf("prompt: %s: missing required template %q", promptsDir, name)
		}
	}

	st := store.New(pool)
	chat := llm.New(cfg.Env.LLMBaseURL, cfg.Env.LLMAPIKey, st, cfg.Agents.Pricing)

	// platform/mock stands in for the real judging platform until Task 15
	// wires up ejudge; every step before it runs against platform/mock, per
	// the plan's Platform interface section.
	plt := mock.New()

	pipeline := &worker.Pipeline{
		Store:    st,
		Platform: plt,
		Repair: &repair.Runner{
			Chat:     chat,
			Platform: plt,
			Template: templates["repair"],
			Agent:    cfg.Agents.Repair,
			Events:   st,
			Mistakes: st,
			Formatter: format.Runner{
				Enabled: cfg.Agents.Formatter.Enabled,
				Command: cfg.Agents.Formatter.Command,
			},
		},
		Hint: &hint.Runner{
			Chat:              chat,
			Guardrail:         chat,
			Template:          templates["hint"],
			GuardrailTemplate: templates["guardrail"],
			Agent:             cfg.Agents.Hint,
			GuardrailAgent:    cfg.Agents.Guardrail,
		},
	}

	workerID := fmt.Sprintf("%s-%d", hostname(), os.Getpid())
	w := &worker.Worker{
		ID:       workerID,
		Store:    st,
		Pipeline: pipeline,
	}

	srv := api.NewServer(st, cfg)
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		log.Printf("problem-helper: HTTP listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("problem-helper: HTTP server: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := w.Run(ctx); err != nil {
			log.Printf("problem-helper: worker pool: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("problem-helper: shutdown signal received, draining in-flight work")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("problem-helper: HTTP shutdown: %v", err)
	}

	wg.Wait()
	log.Print("problem-helper: shutdown complete")
	return nil
}

// hostname returns the local hostname, or a random id if it can't be read
// — either way, just needs to be a stable-enough tag for claimed_by.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return uuid.NewString()
	}
	return h
}
