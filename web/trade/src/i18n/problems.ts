import type { Key } from './messages'

// Problem.detail is English prose without a machine-readable code
// (ADR-0010 keeps Problem.code for after v0.1.0). The details a browser
// user can actually run into are matched verbatim here and shown in the
// interface's language; a detail that is not listed, or that drifts on the
// server, degrades to the generic sentence for its status with the English
// detail next to it, so nothing is ever lost, only untranslated.
const knownProblems: Record<string, Key> = {
  'email already registered': 'error.detail.email_registered',
  'invalid email or password': 'error.detail.bad_credentials',
  'this user is frozen': 'error.detail.frozen',
  'trading engine unavailable; retry with the same client_order_id': 'error.detail.engine_unavailable',
  'trading engine unavailable; retry': 'error.detail.engine_unavailable',
  "the order's market no longer accepts cancels": 'error.detail.no_cancels',
  'no deposit address available, retry shortly': 'error.detail.no_deposit_address',
  'deposits are not enabled on this deployment': 'error.detail.deposits_disabled',
  'withdrawals are not enabled on this deployment': 'error.detail.withdrawals_disabled',
  'authentication required: send a Bearer access token or a signed API key request': 'error.detail.auth_required',
  'invalid, expired or already used refresh token': 'error.detail.session_expired',
}

export function problemKey(detail: string): Key | undefined {
  return knownProblems[detail]
}

export function statusKey(status: number): Key {
  switch (status) {
    case 400:
      return 'error.http.400'
    case 401:
      return 'error.http.401'
    case 403:
      return 'error.http.403'
    case 404:
      return 'error.http.404'
    case 409:
      return 'error.http.409'
    case 422:
      return 'error.http.422'
    case 429:
      return 'error.http.429'
    case 503:
      return 'error.http.503'
    default:
      return 'error.http.other'
  }
}
