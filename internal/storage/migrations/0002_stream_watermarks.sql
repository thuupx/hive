-- 0002_stream_watermarks.sql — durable per-session prune watermark.
--
-- Retention deletes durable events, so "the stream is empty" and "the whole
-- stream was pruned" must stay distinguishable. Without a watermark, a client
-- whose cursor predates retention would be served an empty stream instead of
-- an explicit cursor_expired.

CREATE TABLE stream_watermarks (
    session_id     TEXT    PRIMARY KEY,
    pruned_through INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL
);
