/* eslint-disable */
/**
 * This file was automatically generated from /contracts JSON Schemas by
 * json-schema-to-typescript (contracts/scripts/generate-ts.mjs).
 * DO NOT EDIT IT BY HAND — edit the source .schema.json file and re-run
 * `npm run generate` instead.
 */

/**
 * Control-plane -> sandbox-agent WS commands (technical plan §6.1). Every command carries the common envelope (type, messageId, sessionId, gen) so per-message gen-fencing (§3.2 'stale-gen inputs are rejected') does not rely solely on the connection-level X-Sandbox-Gen header. Field nullability convention used throughout /contracts: a property documented as 'nullable' is a REQUIRED key whose value may be JSON null (always sent, may carry no value); the sole exception is Prompt.conversationId, which is additionally OPTIONAL (both absence and null mean 'start a fresh conversation') per §6.1/§3.3.
 */
export type SandboxCommand = Prompt | Stop | Push | Snapshot | Shutdown | Ack | GitSyncComplete | Checkout;

/**
 * Dispatch a turn. Carries author identity (scmName/scmEmail) for git commit attribution (§6.1) and the plan-mode toggle (§8.1).
 */
export interface Prompt {
  type: 'prompt';
  messageId: string;
  sessionId: string;
  gen: number;
  /**
   * OpenCode conversation id to resume. Absent or null both mean 'start a fresh conversation' (§3.3) — this is the one field in /contracts where omission and explicit null are deliberately synonymous.
   */
  conversationId?: string | null;
  /**
   * The prompt text.
   */
  text: string;
  /**
   * Model override for this turn; null means use the session/plan default.
   */
  model: string | null;
  /**
   * Reasoning-effort override for this turn; null means use the default.
   */
  effort: string | null;
  /**
   * Git author name for commit attribution.
   */
  scmName: string;
  /**
   * Git author email for commit attribution.
   */
  scmEmail: string;
  /**
   * §8.1: dispatch into plan mode instead of direct execution.
   */
  planMode?: boolean;
  /**
   * Technical plan §3.3, prompt receipts: true asks the agent to answer this prompt, and every later copy of it, with a prompt_received event whose messageId is 'prompt_received:{this messageId}', and never to run a copy twice. Sent only to a gen whose ready advertised capabilities.promptReceipt. Absent or false: no receipt is asked.
   */
  receiptRequested?: boolean;
}
/**
 * Cancel the in-flight turn. No fields beyond the common envelope.
 */
export interface Stop {
  type: 'stop';
  messageId: string;
  sessionId: string;
  gen: number;
}
/**
 * Push one or more repos. §6.1: CP awaits push_complete within 360s (that timeout lives in a later PR's platform/timeouts.go, not here).
 */
export interface Push {
  type: 'push';
  messageId: string;
  sessionId: string;
  gen: number;
  /**
   * Per-repo push spec (§6.1). Repos are always a list (§3.4) — no scalar single-repo mirror.
   *
   * @minItems 1
   */
  repos: [
    {
      name: string;
      branch: string;
      /**
       * Remote name override; null means use the repo's configured default remote.
       */
      remote: string | null;
    },
    ...{
      name: string;
      branch: string;
      /**
       * Remote name override; null means use the repo's configured default remote.
       */
      remote: string | null;
    }[]
  ];
}
/**
 * Request a filesystem snapshot. No fields beyond the common envelope.
 */
export interface Snapshot {
  type: 'snapshot';
  messageId: string;
  sessionId: string;
  gen: number;
}
/**
 * Request an orderly sandbox shutdown. No fields beyond the common envelope.
 */
export interface Shutdown {
  type: 'shutdown';
  messageId: string;
  sessionId: string;
  gen: number;
}
/**
 * Acknowledge one of the 6 critical event types by its deterministic ackId (§6.1 ack protocol).
 */
export interface Ack {
  type: 'ack';
  messageId: string;
  sessionId: string;
  gen: number;
  /**
   * The ackId being acknowledged, formatted '{type}:{messageId}' (§6.1).
   */
  ackId: string;
}
/**
 * Acknowledge completion of the CP-side half of a git_sync step (stash/checkout/pop, §3.4). No fields beyond the common envelope.
 */
export interface GitSyncComplete {
  type: 'git_sync_complete';
  messageId: string;
  sessionId: string;
  gen: number;
}
/**
 * Technical plan §21.1, §30.4: check out, in each repo named, the commit a review turn records, read from its pull request's ref in the base repository. The agent fetches ref, checks out sha detached and forced, removes untracked files (ignored files are kept), and answers with one checkout_result whose messageId is 'checkout_result:{this messageId}'. Sent only to a gen whose ready advertised capabilities.reviewCheckout; an agent that predates it skips it as an unknown command.
 */
export interface Checkout {
  type: 'checkout';
  messageId: string;
  sessionId: string;
  gen: number;
  /**
   * Per-repo checkout spec. Repos are always a list (§3.4); each name is a SessionConfig.repos[].name.
   *
   * @minItems 1
   */
  repos: [
    {
      name: string;
      /**
       * The pull request's head ref in the base repository.
       */
      ref: string;
      /**
       * The commit the turn records, in lowercase hex: 40 characters, or 64 in a SHA-256 repository.
       */
      sha: string;
    },
    ...{
      name: string;
      /**
       * The pull request's head ref in the base repository.
       */
      ref: string;
      /**
       * The commit the turn records, in lowercase hex: 40 characters, or 64 in a SHA-256 repository.
       */
      sha: string;
    }[]
  ];
}
