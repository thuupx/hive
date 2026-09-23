-- 0001_init.sql — initial coordinator schema.
--
-- Coordinator-owned domain state. Node-local execution state lives in a
-- separate node database.
--
-- Cluster failover tables are intentionally absent: coordinator failover
-- is not part of v1 (see hive-v1-implementation-plan.md D4). accepted_term
-- is retained as a nullable column so the field can be populated later
-- without a schema change.

CREATE TABLE sessions (
    id                         TEXT    PRIMARY KEY,
    state                      TEXT    NOT NULL,
    workspace_id               TEXT,
    default_interactive_run_id TEXT,
    metadata                   TEXT    NOT NULL DEFAULT '{}',
    created_at                 INTEGER NOT NULL,
    updated_at                 INTEGER NOT NULL
);

CREATE TABLE workspaces (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE workspace_locations (
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    path         TEXT NOT NULL,
    PRIMARY KEY (workspace_id, node_id)
);

CREATE TABLE agent_runs (
    id                   TEXT    PRIMARY KEY,
    session_id           TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    agent_id             TEXT    NOT NULL,
    node_id              TEXT,
    protocol             TEXT    NOT NULL,
    runtime_session_id   TEXT,
    execution_generation INTEGER NOT NULL DEFAULT 1,
    node_execution_id    TEXT,
    state                TEXT    NOT NULL,
    started_at           INTEGER,
    ended_at             INTEGER
);

CREATE INDEX idx_agent_runs_session ON agent_runs (session_id);

CREATE TABLE events (
    id          TEXT    PRIMARY KEY,
    sequence    INTEGER NOT NULL,
    session_id  TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    run_id      TEXT,
    origin_node TEXT,
    timestamp   INTEGER NOT NULL,
    type        TEXT    NOT NULL,
    version     INTEGER NOT NULL,
    protocol    TEXT,
    method      TEXT,
    payload     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);

-- The authoritative session-stream ordering. Unique per session so a
-- duplicate or out-of-order sequence cannot become durable.
CREATE UNIQUE INDEX idx_events_stream ON events (session_id, sequence);

CREATE TABLE outbox (
    event_id     TEXT    PRIMARY KEY REFERENCES events (id) ON DELETE CASCADE,
    session_id   TEXT    NOT NULL,
    sequence     INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    published_at INTEGER
);

CREATE INDEX idx_outbox_pending ON outbox (published_at, sequence);

CREATE TABLE commands (
    id            TEXT    PRIMARY KEY,
    actor         TEXT    NOT NULL,
    source_id     TEXT,
    method        TEXT    NOT NULL,
    accepted_term INTEGER,
    target        TEXT,
    state         TEXT    NOT NULL,
    result        TEXT,
    error         TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX idx_commands_source ON commands (source_id);

CREATE TABLE session_snapshots (
    id             TEXT    PRIMARY KEY,
    session_id     TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    schema_version INTEGER NOT NULL,
    sequence       INTEGER NOT NULL,
    payload        TEXT    NOT NULL,
    created_at     INTEGER NOT NULL
);

CREATE INDEX idx_snapshots_session ON session_snapshots (session_id, sequence);

CREATE TABLE handoffs (
    id                  TEXT    PRIMARY KEY,
    session_id          TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    source_run_id       TEXT,
    target_run_id       TEXT,
    state               TEXT    NOT NULL,
    summary             TEXT,
    context_snapshot_id TEXT,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

CREATE INDEX idx_handoffs_session ON handoffs (session_id);

CREATE TABLE permission_requests (
    id               TEXT    PRIMARY KEY,
    session_id       TEXT    NOT NULL,
    run_id           TEXT,
    agent_request_id TEXT    NOT NULL,
    payload          TEXT    NOT NULL,
    state            TEXT    NOT NULL,
    expires_at       INTEGER,
    created_at       INTEGER NOT NULL,
    resolved_at      INTEGER
);

CREATE INDEX idx_permissions_open ON permission_requests (state, expires_at);

CREATE TABLE conversation_bindings (
    id              TEXT    PRIMARY KEY,
    session_id      TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    transport       TEXT    NOT NULL,
    conversation_id TEXT    NOT NULL,
    principal       TEXT    NOT NULL,
    event_cursor    INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    UNIQUE (transport, conversation_id)
);

CREATE TABLE plugins (
    id          TEXT    PRIMARY KEY,
    type        TEXT    NOT NULL,
    identity    TEXT    NOT NULL,
    version     TEXT,
    trust_state TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE plugin_instances (
    id                    TEXT    PRIMARY KEY,
    plugin_id             TEXT    NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
    process_identity      TEXT,
    connection_generation INTEGER NOT NULL DEFAULT 1,
    state                 TEXT    NOT NULL,
    started_at            INTEGER NOT NULL,
    ended_at              INTEGER
);

CREATE TABLE capability_registrations (
    plugin_id          TEXT    NOT NULL REFERENCES plugins (id) ON DELETE CASCADE,
    capability         TEXT    NOT NULL,
    version            INTEGER NOT NULL DEFAULT 1,
    registration_state TEXT    NOT NULL,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    PRIMARY KEY (plugin_id, capability)
);
