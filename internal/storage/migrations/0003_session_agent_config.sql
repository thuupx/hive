-- A session remembers the agent selectors its user chose, so a new AgentRun
-- continues with the same model and mode instead of silently reverting.
ALTER TABLE sessions ADD COLUMN agent_config TEXT NOT NULL DEFAULT '{}';
