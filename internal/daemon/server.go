package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypersequent/oddk/internal/crypto"
	"github.com/hypersequent/oddk/internal/docker"
	"github.com/hypersequent/oddk/internal/operations"
	"github.com/hypersequent/oddk/internal/services"
	"github.com/hypersequent/oddk/internal/store"
	"github.com/hypersequent/oddk/internal/version"
)

type Server struct {
	store             *store.Store
	docker            *docker.Client
	newlyCreatedToken string // Non-empty if token was just created
	port              int
	allowRemote       bool // If false, bind to loopback only
	masterKey         []byte
	executor          *operations.Executor
	opDeps            *operations.Dependencies
	httpServer        *http.Server
	backupDir         string
	cronTracker       *CronTaskTracker
	healthScheduler   *HealthScheduler
	cancel            context.CancelFunc // Just keep cancel func to stop background processes
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

	token, err := store.Auth.GetOrCreateToken()
	var newlyCreatedToken string
	if err != nil {
		if strings.Contains(err.Error(), "token already exists") {
			log.Println("Auth token already exists. Use existing CLI config or regenerate token.")
		} else {
			return nil, fmt.Errorf("get or create auth token: %w", err)
		}
	} else {
		// Token was just created
		newlyCreatedToken = token
		log.Printf("Generated new auth token: %s", token)
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

	cronTracker := NewCronTaskTracker(opDeps, executor)

	healthChecker := services.NewHealthChecker(store, dockerClient, dataDir, backupDir, masterKey)
	healthScheduler := NewHealthScheduler(healthChecker, healthCheckIntervalSec)

	return &Server{
		store:             store,
		docker:            dockerClient,
		newlyCreatedToken: newlyCreatedToken,
		port:              port,
		allowRemote:       allowRemote,
		masterKey:         masterKey,
		executor:          executor,
		opDeps:            opDeps,
		backupDir:         backupDir,
		cronTracker:       cronTracker,
		healthScheduler:   healthScheduler,
	}, nil
}

func (s *Server) Start() error {
	mux := s.setupRoutes()

	// Create context for background processes and store cancel function
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	go s.startCronScheduler(ctx)

	go s.healthScheduler.Start(ctx)

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

// HasNewlyCreatedToken returns true if a token was just created
func (s *Server) HasNewlyCreatedToken() bool {
	return s.newlyCreatedToken != ""
}

// WriteClientConfig writes the CLI configuration file to the specified directory
func (s *Server) WriteClientConfig(configDir string) error {
	if s.newlyCreatedToken == "" {
		return fmt.Errorf("no newly created token to write")
	}

	config := map[string]string{
		"daemonUrl": fmt.Sprintf("http://localhost:%d", s.port),
		"authToken": s.newlyCreatedToken,
	}

	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	configPath := filepath.Join(configDir, ".oddk-cli.json")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}

	log.Printf("Created %s with auth token", configPath)
	log.Printf("Auth token for CLI: %s", s.newlyCreatedToken)
	log.Println("Config saved to .oddk-cli.json (or save to ~/.config/oddk/cli.json)")

	return nil
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	// Cancel background processes
	if s.cancel != nil {
		s.cancel()
	}

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
