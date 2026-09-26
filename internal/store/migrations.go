package store

// migrations is applied in order; index i is version i+1. Never edit an
// entry after it has shipped; append a new one.
var migrations = []string{
	// v1: every table from docs/plan.md §Storage.
	`
CREATE TABLE accounts (
	id          INTEGER PRIMARY KEY,
	provider    TEXT    NOT NULL,
	auth_index  TEXT    NOT NULL,
	auth_id     TEXT    NOT NULL DEFAULT '',
	email       TEXT    NOT NULL DEFAULT '',
	plan_type   TEXT    NOT NULL DEFAULT '',
	first_seen  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	UNIQUE(provider, auth_index)
);

CREATE TABLE usage_events (
	id               INTEGER PRIMARY KEY,
	account_id       INTEGER NOT NULL REFERENCES accounts(id),
	dedup_key        TEXT    NOT NULL UNIQUE,
	requested_at_ns  INTEGER NOT NULL,
	observed_at_ns   INTEGER NOT NULL,
	latency_ns       INTEGER NOT NULL DEFAULT 0,
	model            TEXT    NOT NULL DEFAULT '',
	model_family     TEXT    NOT NULL DEFAULT '',
	alias            TEXT    NOT NULL DEFAULT '',
	service_tier     TEXT    NOT NULL DEFAULT '',
	reasoning_effort TEXT    NOT NULL DEFAULT '',
	failed           INTEGER NOT NULL DEFAULT 0,
	status_code      INTEGER NOT NULL DEFAULT 0,
	uncached_input   INTEGER NOT NULL DEFAULT 0,
	cache_read       INTEGER NOT NULL DEFAULT 0,
	cache_write      INTEGER NOT NULL DEFAULT 0,
	output           INTEGER NOT NULL DEFAULT 0,
	reasoning        INTEGER NOT NULL DEFAULT 0,
	is_fast          INTEGER NOT NULL DEFAULT 0,
	is_long          INTEGER NOT NULL DEFAULT 0,
	api_usd          REAL    NOT NULL DEFAULT 0,
	api_usd_cw1h     REAL    NOT NULL DEFAULT 0,
	price_hash       TEXT    NOT NULL DEFAULT '',
	headers_json     TEXT,
	flags            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX usage_events_account_observed ON usage_events(account_id, observed_at_ns);
CREATE INDEX usage_events_price_hash ON usage_events(price_hash);

CREATE TABLE meter_readings (
	id             INTEGER PRIMARY KEY,
	account_id     INTEGER NOT NULL REFERENCES accounts(id),
	meter_key      TEXT    NOT NULL,
	source         TEXT    NOT NULL,
	event_id       INTEGER REFERENCES usage_events(id),
	observed_at_ns INTEGER NOT NULL,
	used_fraction  REAL    NOT NULL,
	raw            TEXT    NOT NULL DEFAULT '',
	precision_dp   INTEGER NOT NULL DEFAULT 2,
	reset_at       INTEGER NOT NULL DEFAULT 0,
	status         TEXT    NOT NULL DEFAULT '',
	window_s       INTEGER NOT NULL DEFAULT 0,
	cycle_id       INTEGER
);
CREATE INDEX meter_readings_account_meter_observed ON meter_readings(account_id, meter_key, observed_at_ns);
CREATE INDEX meter_readings_event ON meter_readings(event_id);

CREATE TABLE cycles (
	id               INTEGER PRIMARY KEY,
	account_id       INTEGER NOT NULL REFERENCES accounts(id),
	meter_key        TEXT    NOT NULL,
	window_s         INTEGER NOT NULL DEFAULT 0,
	reset_anchor     INTEGER NOT NULL,
	starts_at        INTEGER NOT NULL DEFAULT 0,
	first_reading_at INTEGER NOT NULL DEFAULT 0,
	closed_at        INTEGER,
	end_reason       TEXT    NOT NULL DEFAULT '',
	regime_seq       INTEGER NOT NULL DEFAULT 0,
	tick             REAL    NOT NULL DEFAULT 0.01,
	peak_fraction    REAL    NOT NULL DEFAULT 0,
	flags_json       TEXT    NOT NULL DEFAULT '[]',
	UNIQUE(account_id, meter_key, reset_anchor, regime_seq)
);

CREATE TABLE segments (
	id              INTEGER PRIMARY KEY,
	cycle_id        INTEGER NOT NULL REFERENCES cycles(id) ON DELETE CASCADE,
	lag             TEXT    NOT NULL,
	seq             INTEGER NOT NULL,
	from_level      INTEGER NOT NULL,
	to_level        INTEGER NOT NULL,
	delta_ticks     INTEGER NOT NULL,
	from_reading_id INTEGER,
	to_reading_id   INTEGER,
	t_start_ns      INTEGER NOT NULL,
	t_end_ns        INTEGER NOT NULL,
	usd             REAL    NOT NULL DEFAULT 0,
	usd_cw1h        REAL    NOT NULL DEFAULT 0,
	features_json   TEXT,
	n_events        INTEGER NOT NULL DEFAULT 0,
	n_failed        INTEGER NOT NULL DEFAULT 0,
	flags_json      TEXT    NOT NULL DEFAULT '[]',
	UNIQUE(cycle_id, lag, seq)
);

CREATE TABLE estimates (
	id               INTEGER PRIMARY KEY,
	cycle_id         INTEGER NOT NULL REFERENCES cycles(id) ON DELETE CASCADE,
	computed_at      INTEGER NOT NULL,
	kind             TEXT    NOT NULL,
	method           TEXT    NOT NULL DEFAULT '',
	lag              TEXT    NOT NULL DEFAULT '',
	v_hat            REAL    NOT NULL DEFAULT 0,
	v_hat_cw1h       REAL    NOT NULL DEFAULT 0,
	ci_lo            REAL    NOT NULL DEFAULT 0,
	ci_hi            REAL    NOT NULL DEFAULT 0,
	se_quant         REAL    NOT NULL DEFAULT 0,
	se_boot          REAL    NOT NULL DEFAULT 0,
	ticks_used       INTEGER NOT NULL DEFAULT 0,
	ticks_excluded   INTEGER NOT NULL DEFAULT 0,
	runs             INTEGER NOT NULL DEFAULT 0,
	unexplained_frac REAL    NOT NULL DEFAULT 0,
	grade            TEXT    NOT NULL DEFAULT '',
	mix_json         TEXT,
	v_blend          REAL    NOT NULL DEFAULT 0,
	flags_json       TEXT    NOT NULL DEFAULT '[]',
	price_hash       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX estimates_cycle ON estimates(cycle_id, computed_at);
CREATE UNIQUE INDEX estimates_one_final ON estimates(cycle_id, price_hash) WHERE kind = 'final';

CREATE TABLE weight_fits (
	id            INTEGER PRIMARY KEY,
	account_id    INTEGER NOT NULL REFERENCES accounts(id),
	meter_key     TEXT    NOT NULL,
	computed_at   INTEGER NOT NULL,
	lag           TEXT    NOT NULL DEFAULT '',
	anchor_family TEXT    NOT NULL DEFAULT '',
	factors_json  TEXT,
	scales_json   TEXT,
	loss          REAL    NOT NULL DEFAULT 0,
	backtest_json TEXT,
	price_hash    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX weight_fits_account_meter ON weight_fits(account_id, meter_key, computed_at);

CREATE TABLE price_snapshots (
	snapshot_hash TEXT    PRIMARY KEY,
	created_at    INTEGER NOT NULL,
	source        TEXT    NOT NULL DEFAULT '',
	raw_json      TEXT
);

CREATE TABLE prices (
	snapshot_hash  TEXT NOT NULL REFERENCES price_snapshots(snapshot_hash) ON DELETE CASCADE,
	provider       TEXT NOT NULL,
	model_family   TEXT NOT NULL,
	input          REAL NOT NULL DEFAULT 0,
	output         REAL NOT NULL DEFAULT 0,
	cache_read     REAL NOT NULL DEFAULT 0,
	cache_write_5m REAL NOT NULL DEFAULT 0,
	cache_write_1h REAL NOT NULL DEFAULT 0,
	long_ctx_mult  REAL NOT NULL DEFAULT 1,
	fast_mult      REAL NOT NULL DEFAULT 1,
	source         TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(snapshot_hash, provider, model_family)
);

CREATE TABLE settings (
	key        TEXT PRIMARY KEY,
	value_json TEXT NOT NULL
);
`,
}
