-- D1 audit fix ("a redelivery re-fires the automation"): gives an
-- automation_invocations row created by live GitHub/Linear webhook
-- dispatch (app/automation's own githubdispatch.go/lineardispatch.go) a
-- durable identity tied to the delivery that created it, so creating one
-- is idempotent on (automation_id, source_provider, source_delivery_id) --
-- surviving a webhook_deliveries claim RELEASE entirely (that claim's
-- lifetime is owned by the @mention/AgentSessionEvent pipelines, never
-- this consumer's to rely on: see invocationenqueue.go's own doc comment).
--
-- source_provider/source_delivery_id are BOTH NULL for every invocation
-- created by a trigger with no notion of "one delivery" at all (cron,
-- manual, the generic webhook trigger) -- the partial unique index below
-- only ever applies to a row where both are populated, so those trigger
-- types are completely unaffected: Postgres treats every NULL as distinct
-- from every other NULL in a unique index, and the WHERE clause excludes
-- them from the index outright regardless.
ALTER TABLE automation_invocations
    ADD COLUMN source_provider    TEXT,
    ADD COLUMN source_delivery_id TEXT;

-- One invocation per (automation, provider, delivery) -- never two, no
-- matter how many times CreateAutomationInvocationForDelivery is called
-- for the identical redelivered (provider, delivery_id): the SAME
-- "INSERT ... ON CONFLICT ... DO UPDATE ... RETURNING (xmax = 0) AS
-- inserted" idiom ClaimWebhookDelivery already establishes
-- (queries/webhookdeliveries.sql) detects "already there" via this exact
-- index, with no second round trip.
CREATE UNIQUE INDEX automation_invocations_source_delivery_uniq
    ON automation_invocations (automation_id, source_provider, source_delivery_id)
    WHERE source_provider IS NOT NULL AND source_delivery_id IS NOT NULL;
