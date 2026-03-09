# migratex

A minimal, dependency-free SQL migration runner for PostgreSQL.

Inspired by Flyway's versioned migration conventions — no external dependencies, pure stdlib.

---

## Features

- Flyway-compatible filename convention (`V<version>__<description>.sql`)
- Checksum validation — detects edits to already-applied migrations
- Each migration runs in its own transaction; failures are rolled back and recorded
- History table (`schema_migrations`) is created automatically
- Accepts `*sql.DB` and `fs.FS` — no coupling to any framework

---

## Installation

### When the library is published to GitHub

```bash
go get github.com/anypost/migratex
```

### When using locally (monorepo / local path)

1. Add a `replace` directive to your `go.mod`:

```
replace github.com/anypost/migratex => ../migratex
```

2. Add the `require` entry:

```
require github.com/anypost/migratex v0.0.0
```

3. Run:

```bash
go mod tidy
```

---

## Migration file naming

Files must follow this pattern:

```
V<version>__<description>.sql
```

| Part | Rules |
|---|---|
| `V` | Literal uppercase V prefix |
| `<version>` | Integer or underscore-separated integer (e.g. `1`, `1_1`, `2`) |
| `__` | Double underscore separator |
| `<description>` | Alphanumeric + underscores; underscores become spaces in the history table |
| `.sql` | Must end with `.sql` |

**Valid examples:**

```
V1__create_users.sql
V2__add_email_index.sql
V1_1__backfill_slugs.sql
V10__drop_legacy_table.sql
```

---

## Usage

### 1. Embed your migration files

The `//go:embed` directive **must live in your application code**, not inside the library.

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
```

### 2. Create the migrator and run

```go
import (
    "database/sql"
    "embed"

    "github.com/anypost/migratex"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func runMigrations(db *sql.DB) error {
    m := migratex.New(db, migrationsFS,
        migratex.WithDir("migrations"),
    )
    return m.Migrate()
}
```

### 3. Wire it in your entry point

```go
func main() {
    db, _ := sql.Open("postgres", dsn)

    if err := runMigrations(db); err != nil {
        log.Fatal(err)
    }
}
```

---

## Options

| Option | Default | Description |
|---|---|---|
| `WithDir(dir string)` | `"."` | Subdirectory inside the `fs.FS` that contains the `.sql` files |
| `WithLogger(log *slog.Logger)` | `slog` text handler → stderr | Custom structured logger |
| `WithHistoryTable(table string)` | `"schema_migrations"` | Name of the history tracking table |

### Using a custom logger

`migratex` accepts a stdlib `*slog.Logger`. If your project wraps `slog`, expose the underlying logger:

```go
m := migratex.New(db, migrationsFS,
    migratex.WithDir("migrations"),
    migratex.WithLogger(myLogger.Slog()),
)
```

---

## History table schema

Created automatically on first run:

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    installed_rank  SERIAL        PRIMARY KEY,
    version         VARCHAR(50)   NOT NULL UNIQUE,
    description     VARCHAR(200)  NOT NULL,
    script          VARCHAR(300)  NOT NULL,
    checksum        BIGINT        NOT NULL,
    installed_on    TIMESTAMP     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    execution_time  INTEGER       NOT NULL,  -- milliseconds
    success         BOOLEAN       NOT NULL
);
```

---

## Error conditions

| Scenario | Behaviour |
|---|---|
| Migration file has been edited after being applied | Fatal error — checksum mismatch |
| A migration previously failed | Fatal error — must be resolved manually |
| SQL execution error | Transaction rolled back; failure recorded in history table |
| Invalid filename pattern | Error at startup before any migration runs |

---

## Full project integration example

```
myservice/
  cmd/
    migrate/
      main.go          ← calls migratex.New().Migrate()
  internal/
    postgres/
      migrator.go      ← thin wrapper (embed lives here)
      migrations/
        V1__create_orders.sql
        V2__add_status_index.sql
  go.mod
```

**`internal/postgres/migrator.go`:**

```go
package postgres

import (
    "embed"
    "log/slog"

    "github.com/anypost/migratex"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func Migrate(db *sql.DB, log *slog.Logger) error {
    m := migratex.New(db, migrationsFS,
        migratex.WithDir("migrations"),
        migratex.WithLogger(log),
    )
    return m.Migrate()
}
```

**`cmd/migrate/main.go`:**

```go
package main

import (
    "database/sql"
    "log/slog"
    "os"

    "myservice/internal/postgres"
    _ "github.com/lib/pq"
)

func main() {
    log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

    db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Error("failed to open db", "error", err)
        os.Exit(1)
    }
    defer db.Close()

    if err := postgres.Migrate(db, log); err != nil {
        log.Error("migration failed", "error", err)
        os.Exit(1)
    }

    log.Info("migration completed")
}
```
