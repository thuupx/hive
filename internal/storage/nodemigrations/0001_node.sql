-- 0001_node.sql — node-local schema.
--
-- A node database is local execution state and an event buffer. It is not an
-- independent authoritative copy of sessions, permissions, or handoff state:
-- during recovery it can provide execution evidence and buffered events, but it
-- must not silently become a second source of truth.

CREATE TABLE node_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE node_executions (
    agent_run_id       TEXT    PRIMARY KEY,
    generation         INTEGER NOT NULL,
    node_execution_id  TEXT,
    runtime_session_id TEXT,
    state              TEXT    NOT NULL,
    started_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE node_event_buffer (
    event_id       TEXT    PRIMARY KEY,
    local_sequence INTEGER NOT NULL,
    session_id     TEXT    NOT NULL,
    run_id         TEXT,
    type           TEXT    NOT NULL,
    version        INTEGER NOT NULL,
    protocol       TEXT,
    method         TEXT,
    payload        TEXT    NOT NULL,
    buffered_at    INTEGER NOT NULL
);

-- Node-local ordering only. It is a synchronization aid, not a Hive stream
-- sequence.
CREATE UNIQUE INDEX idx_node_buffer_sequence ON node_event_buffer (local_sequence);
