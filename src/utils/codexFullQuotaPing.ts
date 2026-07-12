import type { CodexAccount } from '../types/codex';
import { isCodexApiKeyAccount } from '../types/codex';

export const FULL_QUOTA_PING_HOURLY_MIN_PERCENT = 99;
export const FULL_QUOTA_PING_WEEKLY_MIN_PERCENT_EXCLUSIVE = 10;
export const FULL_QUOTA_PING_REFRESH_DELAY_MS = 3_000;
export const FULL_QUOTA_PING_MODEL = 'gpt-5.4-mini';
export const FULL_QUOTA_PING_PROMPT = 'ping';

const FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS = 1;

function recordValue(value: unknown, key: string): unknown {
  return value && typeof value === 'object' ? (value as Record<string, unknown>)[key] : undefined;
}

function finiteNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function hasUnstartedHourlyWindow(account: CodexAccount): boolean {
  const rateLimit = recordValue(account.quota?.raw_data, 'rate_limit');
  const primaryWindow = recordValue(rateLimit, 'primary_window');
  const limitWindowSeconds = finiteNumber(recordValue(primaryWindow, 'limit_window_seconds'));
  const resetAfterSeconds = finiteNumber(recordValue(primaryWindow, 'reset_after_seconds'));
  if (
    limitWindowSeconds === null ||
    resetAfterSeconds === null ||
    limitWindowSeconds <= 0 ||
    resetAfterSeconds < 0
  ) {
    return true;
  }
  return resetAfterSeconds >= limitWindowSeconds - FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS;
}

export function isCodexFullQuotaPingEligible(account: CodexAccount): boolean {
  if (
    isCodexApiKeyAccount(account) ||
    (!account.tokens.access_token?.trim() && !account.tokens.refresh_token?.trim()) ||
    account.requires_reauth === true ||
    account.quota_error ||
    !account.quota
  ) {
    return false;
  }

  const quota = account.quota;
  if (quota.hourly_window_present === false || quota.weekly_window_present === false) {
    return false;
  }
  const hourly = quota.hourly_percentage;
  const weekly = quota.weekly_percentage;
  return (
    Number.isFinite(hourly) &&
    Number.isFinite(weekly) &&
    hourly >= FULL_QUOTA_PING_HOURLY_MIN_PERCENT &&
    weekly > FULL_QUOTA_PING_WEEKLY_MIN_PERCENT_EXCLUSIVE &&
    hasUnstartedHourlyWindow(account)
  );
}
