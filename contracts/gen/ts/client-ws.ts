/* eslint-disable */
/**
 * This file was automatically generated from /contracts JSON Schemas by
 * json-schema-to-typescript (contracts/scripts/generate-ts.mjs).
 * DO NOT EDIT IT BY HAND — edit the source .schema.json file and re-run
 * `npm run generate` instead.
 */

/**
 * Browser <-> control-plane WS protocol (§6.2). These 4 shapes are independent named payloads, not a discriminated union, so (per the PR-05 spec) there is deliberately no top-level oneOf here — each is emitted as its own top-level $def. Connect -> subscribe{token, clientId} within 30s (timeout lives in a later PR's platform/timeouts.go) -> a single 'subscribed' reply (SubscribedPayload: full state + event replay + artifacts + participants) -> broadcast stream. fetch_history is cursor-paginated. Close codes 4001 (re-auth) / 4002 (token expired) are connection-level, not part of these payload shapes. Field nullability convention: 'nullable' means a required key whose value may be JSON null.
 */
export interface ClientWSProtocol {
  [k: string]: unknown;
}
/**
 * Sent by the browser client immediately after connecting; must arrive within 30s (§6.2).
 *
 * This interface was referenced by `ClientWSProtocol`'s JSON-Schema
 * via the `definition` "SubscribeRequest".
 */
export interface SubscribeRequest {
  /**
   * Per-participant WS token minted via REST (/api/sessions/:id/ws-token).
   */
  token: string;
  clientId: string;
}
/**
 * The single reply to subscribe. state/events/artifacts/participants are deliberately loosely typed here (additionalProperties: true) — the full session/turn/sandbox read-model shape is assembled by later PRs; this schema only fixes the top-level envelope.
 *
 * This interface was referenced by `ClientWSProtocol`'s JSON-Schema
 * via the `definition` "SubscribedPayload".
 */
export interface SubscribedPayload {
  sessionId: string;
  /**
   * Full session/turn/sandbox read model (shape assembled by later PRs).
   */
  state: {
    [k: string]: unknown;
  };
  /**
   * Event replay.
   */
  events: {
    [k: string]: unknown;
  }[];
  /**
   * True when events is a cut prefix of this session's full history rather than all of it -- the fixed item-count cap or the byte-size budget was hit. False means events is the session's complete history to date. A client that needs what was cut already has fetch_history.
   */
  eventsTruncated: boolean;
  artifacts: {
    [k: string]: unknown;
  }[];
  participants: {
    [k: string]: unknown;
  }[];
}
/**
 * This interface was referenced by `ClientWSProtocol`'s JSON-Schema
 * via the `definition` "FetchHistoryRequest".
 */
export interface FetchHistoryRequest {
  sessionId: string;
  /**
   * Opaque pagination cursor; null to start from the beginning/most recent (server-defined).
   */
  cursor: string | null;
  /**
   * Null means use the server default page size.
   */
  limit: number | null;
}
/**
 * This interface was referenced by `ClientWSProtocol`'s JSON-Schema
 * via the `definition` "FetchHistoryResponse".
 */
export interface FetchHistoryResponse {
  events: {
    [k: string]: unknown;
  }[];
  /**
   * Null when there are no more pages.
   */
  nextCursor: string | null;
}
