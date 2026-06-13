# ODDK - Opinionated Database Deployment Kit

Local PostgreSQL management tool that acts like "locally running RDS".

## Stage 1 MVP Features

- **Single binary** with daemon and CLI functionality
- Create and manage PostgreSQL containers via Docker  
- **Explicit image management** - Pull images separately from instance creation
- **Operations layer** with sequential execution to prevent conflicts
- **Advanced consistency checks** - PostgreSQL connection validation with pgx/v5
- Token-based authentication with Argon2 hashing
- **Password encryption** with AES-256-GCM in the self-describing 3ncr.org/1 format (stored encrypted in database)
- Custom bridge network with stable IP (10.88.0.1)
- Auto-generated secure passwords
- Fine-grained CPU and RAM configuration with system resource validation
- Port conflict detection and strict instance name validation before container creation
- **Backup functionality** - Full instance backups with pg_dump/pg_dumpall and Zstd compression
- **Backup consistency checks** - Validates database records against filesystem state with automatic cleanup
- **Startup reconciliation** - On daemon start, instance state is reconciled with actual Docker container state and orphaned temp artifacts from interrupted operations are swept
- **Monotonic backup naming** - Thread-safe counters prevent naming collisions within same second
- **Go 1.22 HTTP routing** - Clean verb-based routing with organized handlers
- Comprehensive error handling and linting compliance
- **Enhanced e2e testing** - TCP-based waiting for 64% faster test execution
- **CLI integration testing** - Direct CLI testing without spawning processes
- **Health monitoring** - Comprehensive health checks for disk space and Docker daemon with optimized PostgreSQL connection caching, lifecycle coordination, and automated degraded/restored notifications
- **Advanced PostgreSQL diagnostics** - Differentiates between port, authentication, and other connectivity issues
- **Password management** - Get and set PostgreSQL passwords with multiple output formats and connection validation
- **Database management** - Create databases and manage users with read-only/read-write permissions
- **Notification system** - Template-based notifications via Email/Slack/Telegram/Webhook with validation and delivery logging
- **Scheduled backups** - Cron-like scheduling for automated daily backups with probabilistic execution and comprehensive logging; when offsite S3 is configured, failed uploads are retried on later runs and local retention never deletes a backup's only copy
- **Parameter Groups** - AWS-style parameter group management with expression evaluation and validation for PostgreSQL configuration
- **Professional CLI** - Built with urfave/cli v3 providing organized subcommands, built-in help, and flag validation

## Installation

For a Linux server with Docker and systemd, install (or update) the latest release with:

```bash
curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh
```

To pin a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh -s -- --version v0.1.0
```

The installer downloads the release binary from GitHub, verifies its `SHA256SUMS`, and:

- installs the binary to `/usr/local/bin/oddk`
- creates a dedicated `oddk` service user (no login shell) with state under `/var/lib/oddk` (`data/`, `backups/`)
- installs and starts a systemd unit (`oddk.service`)
- configures the CLI for the user who ran the installer, writing `~/.config/oddk/cli.json`

The CLI talks to the daemon over a local HTTP API with a bearer token, so day-to-day commands run as your normal user — no `sudo` and no need to become the `oddk` user. To configure the CLI for an **additional** user, mint a token and install their config in one step:

```bash
eval "$(sudo -u oddk /usr/local/bin/oddk cli-auth)"
```

**Updating**: re-run the install command above. It detects the existing service, swaps the binary in place, and restarts (keeping the previous binary as `oddk.prev` for rollback).

**Requirements**: Linux (x86_64 or arm64), Docker, and systemd.

## Quick Start

### Build and Test

```bash
make build        # Build single binary
make test         # Run unit tests  
make test-e2e     # Run end-to-end tests (requires Docker)
make test-all     # Run both unit and e2e tests
make lint         # Run golangci-lint
```

### Run the daemon

```bash
./bin/oddk daemon [--port 5442] [--data-dir /path]
```

On first run, the daemon will:
1. Generate an auth token in format `<id>:<secret>`
2. Automatically create `.oddk-cli.json` in the current directory with the token
3. Log the data directory location for transparency

The CLI client loads configuration from (in order of precedence):
1. `.oddk-cli.json` in your current working directory
2. `~/.config/oddk/cli.json` in your home directory

Example config format:
```json
{
  "daemonUrl": "http://localhost:5442",
  "authToken": "1:AbCdEf1234567890123456789012345678901234"
}
```

Note: `.oddk-cli.json` is automatically added to `.gitignore` to prevent accidental commits.

The plaintext token is only shown when it is first created, so it can't be read back later. To issue a fresh token and write a CLI config for the current user at any time, run `oddk cli-auth` as the user that owns the database and eval its output:

```bash
eval "$(sudo -u oddk /usr/local/bin/oddk cli-auth)"
```

### CLI Commands

**Built with urfave/cli v3** - Professional CLI with organized subcommands, built-in help system, and automatic flag validation.

```bash
# Run daemon (single binary)
oddk daemon [--port 5442] [--data-dir /path/to/data] [--backup-dir /path/to/backups]

# Mint a CLI token and configure the current user (run as the database owner)
eval "$(sudo -u oddk /usr/local/bin/oddk cli-auth)"

# Pull PostgreSQL image (required before creating instances)
oddk pull --version 17

# Create a new PostgreSQL instance with specific CPU and RAM allocation
oddk create --name myapp --version 17 --port 5432 --cpu 4 --ram 8

# Create instance with custom parameter group
oddk create --name prod --version 17 --port 5432 --cpu 4 --ram 8 --parameter-group custom-params

# Examples with different resource configurations
oddk create --name dev-db --version 17 --port 5433 --cpu 1 --ram 1     # 1 CPU, 1GB RAM
oddk create --name prod-db --version 17 --port 5434 --cpu 4 --ram 8     # 4 CPU, 8GB RAM  
oddk create --name analytics --version 17 --port 5435 --cpu 8 --ram 16  # 8 CPU, 16GB RAM

# RAM can be specified in megabytes with M/MB/MiB suffixes
oddk create --name test-db --version 17 --port 5436 --cpu 2 --ram 2048M  # 2 CPU, 2048MB RAM

# List all instances
oddk list

# Instance operations (organized subcommands)
oddk instance status myapp
oddk instance start myapp
oddk instance stop myapp
oddk instance list-dbs myapp      # List all databases in the instance
oddk instance backup myapp        # Create a backup of the instance
oddk instance backup myapp --comment "Before upgrade"  # Backup with descriptive comment
oddk instance list-backups myapp  # List all backups for the instance (shows comments)

# Backup restore
oddk backup restore --instance myapp --id 42 --database mydb                    # Restore database from backup
oddk backup restore --instance myapp --file /path/to/backup.tar.zst --database mydb  # Restore from file
oddk backup restore --instance myapp --id 42 --database mydb --restore-as mydb_copy  # Restore with different name

# Password management
oddk instance get-postgres-password myapp                    # Structured output with all details
oddk instance get-postgres-password myapp --plain           # Just the password
oddk instance get-postgres-password myapp --conn            # Connection string format
oddk instance get-postgres-password myapp --envs            # Environment variables
NEW_PGPASSWORD=newpass oddk instance set-postgres-password myapp # Set new password

# Database management
oddk instance create-db myapp --database analytics           # Create database within instance
oddk instance add-db-user myapp --username=appuser --database=analytics # Create read-write user
oddk instance add-db-user myapp --username=reader --database=analytics --readonly # Create read-only user
oddk instance add-db-user myapp --username=appuser --database=analytics --owner # Create user as database owner (for migrations)
oddk instance delete-db-user myapp --username=appuser        # Delete database user (preserves data)
oddk instance reset-db-user-password myapp --username=appuser # Reset user password

# Direct PostgreSQL access
oddk instance psql myapp                                     # Launch interactive PostgreSQL shell

# Switch instance to a different Docker image (same PG major version)
oddk instance switch myapp --image pgvector/pgvector:pg17-trixie

# Upgrade instance to a NEWER PG major version (dump/restore; causes downtime)
oddk instance major-upgrade myapp --target-version 18                 # official postgres images
oddk instance major-upgrade myapp --target-version 18 --yes           # skip confirmation
oddk instance major-upgrade pgv --target-version 18 --image pgvector/pgvector:pg18-trixie  # custom images need --image

# Apply new parameter group to existing instance
oddk instance apply myapp --parameter-group new-params       # Reconfigure instance with new parameters

# Parameter group management (AWS-style PostgreSQL configuration)
oddk parameters get                                  # List all parameter groups with summary
oddk parameters get --name default:2025-08-27       # Get specific parameter group details
oddk parameters get --json                          # Get all groups in JSON format
oddk parameters put test-params --file test-params.json  # Create parameter group from JSON file
oddk parameters delete test-params                  # Delete parameter group (with confirmation)
oddk parameters delete test-params --force          # Delete without confirmation

# Notification management
oddk notify add --type email --name alerts         # Generate template file for editing
oddk notify add --file alerts.json                 # Add notification from edited template
oddk notify list                                    # List all notifications
oddk notify test                                    # Send test message to all notifications
oddk notify logs --limit 50                        # View recent delivery logs

oddk instance destroy myapp

# Get help for any command
oddk --help                    # Main help
oddk instance --help           # Instance subcommand help  
oddk parameters --help         # Parameter group subcommand help
oddk notify --help             # Notification subcommand help
oddk create --help             # Create command options
```

### Connection

Passwords are auto-generated and shown during instance creation. Get your connection details:

```bash
# Get connection string directly
oddk instance get-postgres-password myapp --conn

# Or get environment variables to export
eval "$(oddk instance get-postgres-password myapp --envs)"
```

Connection format from host:
```
postgresql://postgres:PASSWORD@10.88.0.1:5432/postgres
```

From Docker containers (add to docker-compose.yml):
```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
# Then connect to: postgresql://postgres:PASSWORD@host.docker.internal:5432/postgres
```

## Architecture Highlights

- **Operations Layer**: All business logic runs through a sequential executor
- **Decoupled Design**: Thin HTTP handlers, operations contain business logic  
- **Conflict Prevention**: Database checks before Docker operations
- **Advanced Health Checks**: PostgreSQL connection validation using pgx/v5 with `Ping()`
- **Optimized Containers**: Proper shared memory allocation prevents PostgreSQL errors
- **Clean Separation**: Each operation in its own file (rdbms_*.go pattern)
- **Modern HTTP Routing**: Go 1.22 verb-based routing with organized handler files
- **Backup System**: Professional tar/zstd compression using mholt/archives library with Docker-based pg_dump
- **Comprehensive Testing**: Direct CLI testing + custom e2e runner with Docker integration
- **Modern CLI Framework**: urfave/cli v3 with organized subcommands, automatic help generation, and professional UX

## Resource Configuration

ODDK provides fine-grained control over CPU and RAM allocation:

### CPU Configuration
- Specify exact number of CPU cores (1-1024)
- Client-side bounds checking for reasonable limits
- Server-side validation against actual system CPU count
- Docker CPU shares automatically calculated (1024 per core)

### RAM Configuration
- Flexible input formats:
  - `--ram 8` (8GB, default unit)
  - `--ram 2048M` (2048 megabytes)
  - `--ram 1024MB` or `--ram 1024MiB` (1024 mebibytes)
- Client-side bounds checking (128MB to 1TB)
- Server-side validation against actual system memory
- Automatic shared memory configuration (25% of allocated RAM)

### Validation Architecture
- **Client-side**: Basic bounds checking to catch obvious errors early
- **Server-side**: System resource validation against actual hardware limits
- **Cross-platform**: Works on Linux and macOS using gopsutil library
- **Error handling**: Clear messages about resource limit violations

## PostgreSQL Optimizations

- **Dynamic Shared Memory Configuration**: Automatically calculates `shm_size` as 25% of allocated RAM to prevent "could not resize shared memory segment" errors
- **Health Monitoring**: Real PostgreSQL connection testing with pgx/v5
- **Authentication Validation**: Verifies stored passwords work correctly
- **Startup Detection**: Distinguishes between container running and PostgreSQL ready states

## Security Features

- **Encrypted passwords**: AES-256-GCM encryption with master key
- **Token authentication**: Argon2-hashed tokens in format `<id>:<secret>`; the plaintext is shown only at creation and cannot be retrieved later (mint a new one with `oddk cli-auth`)
- **Loopback by default**: the daemon binds `127.0.0.1`; `--allow-remote` binds all interfaces but sends the token over cleartext HTTP, so prefer an SSH tunnel
- **Dedicated service user**: runs as the unprivileged `oddk` user (no login shell)
- **Network isolation**: Custom Docker bridge (10.88.0.0/16)
- **Master key**: Auto-generated, stored at `{dataDir}/master.key`

## End-to-End Testing

ODDK includes a comprehensive e2e testing system:

```bash
# Run all e2e tests
make test-e2e

# Clean up test containers
make test-e2e-cleanup

# Run tests with options
cd e2e && go run . --help
cd e2e && go run . --cleanup    # Clean up and exit
cd e2e && go run . -v           # Verbose output
```

**Test Coverage:**
- Full lifecycle (create → start → stop → destroy)
- PostgreSQL connectivity validation with authentication
- Multiple instance management
- Network accessibility validation
- Direct CLI integration testing without process spawning
- Environment variable and configuration file handling

**Safety Features:**
- Isolated temp directories for each test
- Container names prefixed with `oddk-danger-funct-`
- Automatic cleanup on test completion
- No interference with production instances

## Parameter Groups

ODDK includes AWS-style parameter group management for PostgreSQL configuration with expression evaluation and validation.

### Features
- **Default Parameter Group**: Pre-configured `default:2025-08-27` group with optimized PostgreSQL settings
- **Expression Evaluation**: Dynamic configuration using expressions with variables like `DBContainerMemoryMB` and `DBContainerCoreCount`
- **Value Types**: Support for `numeric`, `numeric_mem` (with MB/GB suffixes), `string`, and `bool` types
- **Protection**: Cannot create or delete groups with `default:` prefix
- **Validation**: Full parameter validation with expression parsing and value type checking

### Parameter File Format

Create parameter groups using JSON files:
```json
[
  {
    "name": "shared_buffers",
    "type": "postgres_cli_arg", 
    "valueType": "numeric_mem",
    "value": "{expr}DBContainerMemoryMB / 4{/expr} MB"
  },
  {
    "name": "max_connections",
    "type": "postgres_cli_arg",
    "valueType": "numeric", 
    "value": "100"
  },
  {
    "name": "log_statement",
    "type": "postgres_cli_arg",
    "valueType": "string",
    "value": "all"
  }
]
```

### Expression Variables
- `DBContainerMemoryMB`: Container memory allocation in megabytes
- `DBContainerCoreCount`: Container CPU core count
- `MaxConnections`: Resolved max_connections value (for dependent parameters)

### Built-in Functions
Expressions support standard mathematical functions including `min()`, `max()`, `round()`, and arithmetic operations.

## Requirements

- Go 1.26+
- Docker
- Linux or macOS (development)
- golangci-lint is managed as a Go tool dependency (`go.mod`); `make lint` runs it via `go tool` — no separate install needed

## License

MIT — see [LICENSE](LICENSE).
