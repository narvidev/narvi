ALTER TABLE sandboxes DROP COLUMN image_decision_fingerprint;
ALTER TABLE sandboxes DROP COLUMN image_decision_reason;
DROP TYPE image_decision_reason;
