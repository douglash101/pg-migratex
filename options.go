package pgmigratex

import "log/slog"

// Option configures a Migrator.
type Option func(*Migrator)

// WithLogger sets a custom *slog.Logger. Defaults to a text handler writing to stderr.
func WithLogger(log *slog.Logger) Option {
	return func(m *Migrator) {
		m.log = log
	}
}

// WithDir sets the directory inside the fs.FS that contains the *.sql files.
// Defaults to "." (root of the FS).
func WithDir(dir string) Option {
	return func(m *Migrator) {
		m.dir = dir
	}
}

// WithHistoryTable sets the name of the migrations history table.
// Defaults to "schema_migrations".
func WithHistoryTable(table string) Option {
	return func(m *Migrator) {
		m.historyTable = table
	}
}
