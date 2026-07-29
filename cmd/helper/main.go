// Command helper is the Problem Helper MVP service: HTTP server + worker
// goroutines + cron, all in one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/profoundmentalretardation/problem-helper/internal/agent/curator"
	"github.com/profoundmentalretardation/problem-helper/internal/agent/hint"
	"github.com/profoundmentalretardation/problem-helper/internal/agent/repair"
	"github.com/profoundmentalretardation/problem-helper/internal/api"
	"github.com/profoundmentalretardation/problem-helper/internal/config"
	"github.com/profoundmentalretardation/problem-helper/internal/format"
	"github.com/profoundmentalretardation/problem-helper/internal/llm"
	"github.com/profoundmentalretardation/problem-helper/internal/platform"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/ejudge"
	"github.com/profoundmentalretardation/problem-helper/internal/platform/mock"
	"github.com/profoundmentalretardation/problem-helper/internal/prompt"
	"github.com/profoundmentalretardation/problem-helper/internal/store"
	"github.com/profoundmentalretardation/problem-helper/internal/worker"
)

// requiredPlaceholders is the placeholder set each wired agent renders into
// its template. Startup fails when a template is missing one of them.
//
// Template.Render errors on a placeholder that is in the template but absent
// from the supplied values — the direction that fails loudly. The reverse
// fails *open* and silently: a value supplied for a placeholder the template
// no longer contains is simply discarded. Dropping or renaming `{{hint}}` in
// prompts/guardrail.md therefore renders cleanly, asks the guardrail whether a
// hint gives the answer away while showing it no hint, and gets back an
// explicit `{"approved":true}` — which the hint loop honours, delivering and
// caching an unreviewed hint. That is the one gate the whole "gate on the
// irreversible action" rule exists for, degrading with no error anywhere.
// Same class for prompts/repair.md losing `{{user_code}}` (the model repairs
// blind) or prompts/hint.md losing `{{diff}}`.
//
// Checked at startup for the same reason validateCaps and the pricing values
// are: a misedited prompt must fail before serving traffic, not silently at
// first call.
var requiredPlaceholders = map[string][]string{
	"repair":    {"problem_statement", "user_code", "mistakes", "previous_code"},
	"hint":      {"diff", "working_code"},
	"guardrail": {"diff", "working_code", "hint"},
	"curator":   {"raw_mistakes", "existing_mistakes"},
}

// checkPromptTemplates verifies every required template is present and carries
// every placeholder its agent renders.
func checkPromptTemplates(dir string, templates map[string]prompt.Template) error {
	names := make([]string, 0, len(requiredPlaceholders))
	for name := range requiredPlaceholders {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		tmpl, ok := templates[name]
		if !ok {
			return fmt.Errorf("prompt: %s: missing required template %q", dir, name)
		}
		have := make(map[string]bool, len(tmpl.Placeholders()))
		for _, p := range tmpl.Placeholders() {
			have[p] = true
		}
		for _, want := range requiredPlaceholders[name] {
			if !have[want] {
				return fmt.Errorf("prompt: %s: template %q is missing the {{%s}} placeholder", dir, name, want)
			}
		}
	}
	return nil
}

func main() {
	agentsPath := flag.String("agents", "agents.yaml", "path to agents.yaml")
	promptsDir := flag.String("prompts", "prompts", "path to the prompts directory")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "grace period on shutdown for in-flight HTTP requests and worker pipelines")
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
	// pgxpool.Close blocks until every checked-out connection is returned and
	// rejects new acquires immediately, so closing it while an abandoned
	// pipeline is still running would both defeat the shutdown budget
	// enforced below and make that pipeline's final claim-scoped writes
	// (TransitionStatus, SetError) fail against a closed pool. The process is
	// exiting either way; only close when the workers actually drained, or
	// when they never started.
	workersRunning := false
	defer func() {
		if !workersRunning {
			pool.Close()
		}
	}()

	if err := store.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("store: migrating: %w", err)
	}
	log.Print("problem-helper: migrations applied")

	templates, err := prompt.LoadDir(promptsDir)
	if err != nil {
		return fmt.Errorf("prompt: loading %s: %w", promptsDir, err)
	}
	if err := checkPromptTemplates(promptsDir, templates); err != nil {
		return err
	}

	st := store.New(pool)
	chat := llm.New(cfg.Env.LLMBaseURL, cfg.Env.LLMAPIKey, st, cfg.Agents.Pricing)

	plt, err := newPlatform(cfg.Env)
	if err != nil {
		return err
	}

	workerID := fmt.Sprintf("%s-%d", hostname(), os.Getpid())

	pipeline := &worker.Pipeline{
		Store:    st,
		Platform: plt,
		Repair: &repair.Runner{
			Chat:     chat,
			Platform: plt,
			Template: templates["repair"],
			Agent:    cfg.Agents.Repair,
			Events:   st,
			Runs:     st,
			Claims:   st,
			Mistakes: st,
			WorkerID: workerID,
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
		TopNMistakes: cfg.Agents.Repair.TopNMistakes,
		WorkerID:     workerID,
	}

	metaloop := &worker.Metaloop{
		Store: st,
		Curator: &curator.Runner{
			Chat:        chat,
			RawMistakes: st,
			Mistakes:    st,
			Template:    templates["curator"],
			Agent:       cfg.Agents.Curator,
		},
	}

	w := &worker.Worker{
		ID:          workerID,
		Store:       st,
		Pipeline:    pipeline,
		Metaloop:    metaloop,
		Concurrency: cfg.Env.WorkerConcurrency,
	}

	srv := api.NewServer(st, cfg, metaloop)
	// Timeouts are not optional here: header reads happen before any handler
	// (and so before bearer-token auth) runs, so without ReadHeaderTimeout an
	// unauthenticated client can hold connections open indefinitely and
	// exhaust the process's file descriptors and goroutines. The rest bound a
	// slow or stalled body/response the same way.
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	workersRunning = true

	// A bind failure must take the process down. Logging and carrying on
	// leaves a worker-only process that answers no HTTP at all while every
	// supervisor watching the exit code still sees it as healthy.
	serveErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		log.Printf("problem-helper: HTTP listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	go func() {
		defer wg.Done()
		if err := w.Run(ctx); err != nil {
			log.Printf("problem-helper: worker pool: %v", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		log.Print("problem-helper: shutdown signal received, draining in-flight work")
	case err := <-serveErr:
		runErr = fmt.Errorf("HTTP server: %w", err)
		log.Printf("problem-helper: %v, draining in-flight work", runErr)
		stop()
	}

	// Two independent budgets, not one shared deadline. handleMetaloopRun
	// deliberately holds its connection for up to metaloopRunTimeout (30
	// minutes against this 30-second grace period), so Shutdown can burn the
	// whole budget waiting on a single operator-triggered sweep — and a
	// shared context would then leave the worker drain below with none at
	// all, abandoning every in-flight pipeline instantly on an ordinary
	// deploy and handing it straight back to the reclaim sweep, which
	// re-spends both model budgets and re-submits under the shared system
	// login. Each phase gets its own shutdownTimeout.
	// Stop the admin metaloop sweep first: it is detached from its request and
	// runs for up to 30 minutes against a 30-second grace period, so without
	// this Shutdown burns the whole HTTP budget on it and the process then
	// exits through the middle of the sweep anyway — with merges committed and
	// the batch unsealed, which the next instance's startup sweep re-sends and
	// re-merges. Cancelling makes it stop at the next user boundary.
	srv.StopSweeps()

	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelHTTP()
	if err := httpServer.Shutdown(httpCtx); err != nil {
		log.Printf("problem-helper: HTTP shutdown: %v", err)
	}

	// Bound the worker drain too, not just the HTTP server. RunOnce
	// deliberately detaches a claimed pipeline from the signal context, so
	// claimLoop cannot return until an in-flight repair loop and its judge
	// polling finish — minutes, well past any orchestrator's SIGTERM grace
	// period, which then SIGKILLs us mid-run anyway. Returning here instead
	// leaves the request's heartbeat to lapse and the reclaim sweep to hand
	// it to another worker, which is the path already designed for a
	// worker that dies mid-pipeline.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelDrain()
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		workersRunning = false
		log.Print("problem-helper: shutdown complete")
	case <-drainCtx.Done():
		log.Printf("problem-helper: workers still running after %s, exiting anyway; "+
			"their requests will be reclaimed once their heartbeats lapse", shutdownTimeout)
	}
	return runErr
}

// newPlatform selects the judging platform backend from cfg.Env.Platform:
// "ejudge" for production, "mock" for local/dev runs (see docker-compose
// smoke test in the plan's Task 18).
func newPlatform(env config.Env) (platform.Platform, error) {
	switch env.Platform {
	case "ejudge":
		return ejudge.New(env.EjudgeURL, env.EjudgeSystemLogin, env.EjudgeSystemPassword,
			ejudge.WithContestID(env.EjudgeContestID)), nil
	case "mock":
		// NewDefaulting, not New: nothing scripts this instance, and the
		// panicking mock made every request fail through five reclaims.
		return mock.NewDefaulting(), nil
	default:
		return nil, fmt.Errorf("config: unknown PLATFORM %q (want \"ejudge\" or \"mock\")", env.Platform)
	}
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
