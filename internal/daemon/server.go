package daemon

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrianbdn/oddk/internal/crypto"
	"github.com/andrianbdn/oddk/internal/docker"
	"github.com/andrianbdn/oddk/internal/operations"
	"github.com/andrianbdn/oddk/internal/services"
	"github.com/andrianbdn/oddk/internal/store"
	"github.com/andrianbdn/oddk/internal/version"
)

type Server struct {
	store           *store.Store
	docker          *docker.Client
	port            int
	allowRemote     bool // If false, bind to loopback only
	masterKey       []byte
	executor        *operations.Executor
	opDeps          *operations.Dependencies
	httpServer      *http.Server
	backupDir       string
	cronTracker     *CronTaskTracker
	healthScheduler *HealthScheduler
	cancel          context.CancelFunc // Just keep cancel func to stop background processes

	// lastDownloadsSweep is when the snapshot downloads area was last pruned.
	// Touched only by the scheduler goroutine (startup counts as a sweep), so
	// it needs no lock.
	lastDownloadsSweep time.Time

	// dirLock is the exclusive advisory lock on the data directory, held for
	// the daemon's lifetime so 'snapshot apply' cannot run concurrently.
	dirLock *store.DataDirLock
}

func NewServer(port int, dataDir, backupDir string, healthCheckIntervalSec int, allowRemote bool) (*Server, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}

	log.Printf("Using data directory: %s", dataDir)
	log.Printf("Using backup directory: %s", backupDir)

	// Take the data-dir lock before touching anything. 'snapshot apply' holds
	// this same lock for its whole destructive phase, so refusing to start here
	// is what stops a systemd Restart=always from bringing the daemon up in the
	// middle of a restore — where it would drive Docker, sweep the restore's
	// helper containers, and write the oddk.db being replaced.
	dirLock, err := store.AcquireDataDirLock(dataDir)
	if err != nil {
		return nil, fmt.Errorf("cannot start: %w", err)
	}
	// On success the Server owns the lock and releases it in Shutdown; if
	// construction fails anywhere below, drop it here rather than holding it
	// for the life of the process (the e2e harness builds servers repeatedly).
	constructed := false
	defer func() {
		if !constructed {
			dirLock.Release()
		}
	}()

	masterKey, err := crypto.GetOrCreateKeyFile(dataDir)
	if err != nil {
		return nil, fmt.Errorf("get or create master key: %w", err)
	}

	dbPath := filepath.Join(dataDir, "oddk.db")
	log.Printf("Database path: %s", dbPath)

	store, err := store.NewStore(dbPath, dataDir)
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}

	// The daemon never mints tokens itself - tokens are created explicitly with
	// `oddk auth mint` (as the oddk user). Nudge the operator if none exist yet,
	// so a fresh install isn't silently unreachable (every request would 401).
	if n, err := store.Auth.CountTokens(); err != nil {
		return nil, fmt.Errorf("count auth tokens: %w", err)
	} else if n == 0 {
		log.Println("No CLI auth tokens exist yet. Mint one with `oddk auth mint` (as the oddk user) before using the CLI.")
	}

	dockerClient, err := docker.NewClient()
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	// Sweep helper containers orphaned by a previous crashed daemon run.
	if removed, err := dockerClient.RemoveHelperContainers(); err != nil {
		log.Printf("Warning: helper container sweep failed: %v", err)
	} else if removed > 0 {
		log.Printf("Removed %d orphaned helper container(s) from previous run", removed)
	}

	// Reconcile stored instance state with Docker reality, sweep orphaned
	// backup artifacts, and upgrade legacy ciphertexts — before anything can
	// submit operations.
	reconcileInstances(store, dockerClient)
	sweepBackupDir(store, backupDir)
	reconcileInterruptedSnapshotRuns(store)
	reencryptLegacySecrets(store, masterKey)

	executor := operations.NewExecutor()
	opDeps := &operations.Dependencies{
		Store:     store,
		Docker:    dockerClient,
		MasterKey: masterKey,
		DataDir:   dataDir,
		BackupDir: backupDir,
		Logger:    log.New(os.Stdout, "[ODDK] ", log.LstdFlags),
	}

	cronTracker := NewCronTaskTracker(opDeps, executor, backupDir)

	healthChecker := services.NewHealthChecker(store, dockerClient, dataDir, backupDir, masterKey)
	healthScheduler := NewHealthScheduler(healthChecker, healthCheckIntervalSec)

	constructed = true
	return &Server{
		store:           store,
		docker:          dockerClient,
		port:            port,
		allowRemote:     allowRemote,
		masterKey:       masterKey,
		executor:        executor,
		opDeps:          opDeps,
		backupDir:       backupDir,
		cronTracker:     cronTracker,
		healthScheduler: healthScheduler,
		dirLock:         dirLock,
		// sweepBackupDir above just pruned the downloads area; the next sweep
		// is due a day from now.
		lastDownloadsSweep: time.Now(),
	}, nil
}

func (s *Server) Start() error {
	mux := s.setupRoutes()

	// Create context for background processes and store cancel function
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go s.startCronScheduler(ctx)

	go s.healthScheduler.Start(ctx)

	go s.startLogRetentionSweeper(ctx)

	// Bind to loopback by default. --allow-remote opts into 0.0.0.0; warn
	// loudly because there's no TLS — the auth token transits cleartext.
	bindHost := "127.0.0.1"
	if s.allowRemote {
		bindHost = "0.0.0.0"
		log.Printf("WARNING: --allow-remote is set; daemon binds on all interfaces over plaintext HTTP. The auth token transits cleartext. Prefer SSH tunneling (ssh -L %d:localhost:%d <host>) and unset --allow-remote.", s.port, s.port)
	}
	addr := fmt.Sprintf("%s:%d", bindHost, s.port)
	buildInfo := version.GetBuildInfo()
	log.Printf("Starting ODDK daemon on %s (%s)", addr, buildInfo.Short())
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return s.httpServer.ListenAndServe()
}

// MintToken creates a new CLI auth token and returns its plaintext. The daemon
// does not mint tokens on its own (see NewServer); this is the programmatic
// equivalent of `oddk auth mint`, used by tests to obtain a usable token.
func (s *Server) MintToken() (string, error) {
	return s.store.Auth.CreateToken()
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	// Cancel background processes
	if s.cancel != nil {
		s.cancel()
	}

	// Release the data-dir lock so a 'snapshot apply' can proceed once the
	// daemon is down. (The kernel would drop it at process exit anyway, but the
	// daemon is also constructed in-process by tests.)
	defer s.dirLock.Release()

	// Shutdown HTTP server with provided context
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// pauseHealthChecksAndCleanupConnections pauses health checks and closes connections for an instance
// This method: pauses health checks -> waits for completion -> closes connections
// Caller must call unpauseHealthChecks() when the operation is complete
func (s *Server) pauseHealthChecksAndCleanupConnections(instanceName string) {
	if s.healthScheduler == nil {
		return
	}

	// 1. Pause health checks (new ones won't start)
	s.healthScheduler.Pause()

	// 2. Wait for any running health check to complete (5 second timeout)
	if !s.healthScheduler.WaitForCompletion(5 * time.Second) {
		log.Printf("Warning: Health check didn't complete within timeout, proceeding anyway")
	}

	// 3. Close instance connections
	s.healthScheduler.healthChecker.CloseInstanceConnections(instanceName)
}

// unpauseHealthChecks resumes health checks after an operation is complete
func (s *Server) unpauseHealthChecks() {
	if s.healthScheduler == nil {
		return
	}
	s.healthScheduler.Unpause()
}
