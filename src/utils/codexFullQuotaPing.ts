import type { CodexAccount } from '../types/codex';
import { isCodexApiKeyAccount } from '../types/codex';

export const FULL_QUOTA_PING_TARGET_MIN_PERCENT = 99;
export const FULL_QUOTA_PING_RESERVE_MIN_PERCENT_EXCLUSIVE = 10;
export const FULL_QUOTA_PING_REFRESH_DELAY_MS = 3_000;
export const FULL_QUOTA_PING_MODEL = 'gpt-5.4-mini';
export const FULL_QUOTA_PING_PROMPT = 'ping';

const FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS = 1;
export type CodexFullQuotaPingWindowId = 'primary' | 'secondary';

interface CodexFullQuotaPingWindow {
  id: CodexFullQuotaPingWindowId;
  percentage: number;
  resetTime?: number;
  windowMinutes?: number;
}

function recordValue(value: unknown, key: string): unknown {
  return value && typeof value === 'object' ? (value as Record<string, unknown>)[key] : undefined;
}

function finiteNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function isRecord(value: unknown): boolean {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value));
}

function actualQuotaWindows(account: CodexAccount): CodexFullQuotaPingWindow[] {
  const quota = account.quota;
  if (!quota) return [];

  const hasPresenceFlags =
    quota.hourly_window_present !== undefined || quota.weekly_window_present !== undefined;
  const rateLimit = recordValue(quota.raw_data, 'rate_limit');
  const rawPrimaryWindow = recordValue(rateLimit, 'primary_window');
  const rawSecondaryWindow = recordValue(rateLimit, 'secondary_window');
  const hasRawWindowShape = isRecord(rawPrimaryWindow) || isRecord(rawSecondaryWindow);
  const includePrimary =
    quota.hourly_window_present ??
    (hasRawWindowShape ? isRecord(rawPrimaryWindow) : !hasPresenceFlags);
  const includeSecondary =
    quota.weekly_window_present ??
    (hasRawWindowShape ? isRecord(rawSecondaryWindow) : !hasPresenceFlags);
  const windows: CodexFullQuotaPingWindow[] = [];
  if (includePrimary) {
    windows.push({
      id: 'primary',
      percentage: quota.hourly_percentage,
      resetTime: quota.hourly_reset_time,
      windowMinutes: quota.hourly_window_minutes,
    });
  }
  if (includeSecondary) {
    windows.push({
      id: 'secondary',
      percentage: quota.weekly_percentage,
      resetTime: quota.weekly_reset_time,
      windowMinutes: quota.weekly_window_minutes,
    });
  }
  return windows;
}

function hasUnstartedQuotaWindow(
  account: CodexAccount,
  window: CodexFullQuotaPingWindow,
  nowSeconds: number,
): boolean {
  const rateLimit = recordValue(account.quota?.raw_data, 'rate_limit');
  const rawWindow = recordValue(
    rateLimit,
    window.id === 'primary' ? 'primary_window' : 'secondary_window',
  );
  const rawWindowSeconds = finiteNumber(recordValue(rawWindow, 'limit_window_seconds'));
  const normalizedWindowMinutes = finiteNumber(window.windowMinutes);
  const normalizedWindowSeconds =
    normalizedWindowMinutes === null ? null : normalizedWindowMinutes * 60;
  const limitWindowSeconds =
    rawWindowSeconds !== null && rawWindowSeconds > 0
      ? rawWindowSeconds
      : normalizedWindowSeconds !== null && normalizedWindowSeconds > 0
        ? normalizedWindowSeconds
        : null;
  if (limitWindowSeconds === null) return true;

  const resetAfterSeconds = finiteNumber(recordValue(rawWindow, 'reset_after_seconds'));
  if (resetAfterSeconds !== null && resetAfterSeconds >= 0) {
    return (
      resetAfterSeconds <= FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS ||
      resetAfterSeconds >= limitWindowSeconds - FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS
    );
  }

  const rawResetAtValue = finiteNumber(recordValue(rawWindow, 'reset_at'));
  const normalizedResetAtValue = finiteNumber(window.resetTime);
  const rawResetAt = rawResetAtValue !== null && rawResetAtValue > 0 ? rawResetAtValue : null;
  const normalizedResetAt =
    normalizedResetAtValue !== null && normalizedResetAtValue > 0
      ? normalizedResetAtValue
      : null;
  const resetAt = rawResetAt ?? normalizedResetAt;
  if (resetAt === null) return true;
  const resetInSeconds = resetAt - nowSeconds;
  return (
    resetInSeconds <= FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS ||
    resetInSeconds >= limitWindowSeconds - FULL_QUOTA_PING_RESET_TOLERANCE_SECONDS
  );
}

export function resolveCodexFullQuotaPingTargetWindow(
  account: CodexAccount,
  nowSeconds = Math.floor(Date.now() / 1_000),
): CodexFullQuotaPingWindowId | null {
  if (
    isCodexApiKeyAccount(account) ||
    (!account.tokens.access_token?.trim() && !account.tokens.refresh_token?.trim()) ||
    account.requires_reauth === true ||
    account.quota_error ||
    !account.quota
  ) {
    return null;
  }

  const windows = actualQuotaWindows(account);
  if (
    windows.length === 0 ||
    windows.some(
      (window) =>
        !Number.isFinite(window.percentage) ||
        window.percentage <= FULL_QUOTA_PING_RESERVE_MIN_PERCENT_EXCLUSIVE,
    )
  ) {
    return null;
  }

  return (
    windows.find(
      (window) =>
        window.percentage >= FULL_QUOTA_PING_TARGET_MIN_PERCENT &&
        hasUnstartedQuotaWindow(account, window, nowSeconds),
    )?.id ?? null
  );
}

export function isCodexFullQuotaPingEligible(account: CodexAccount): boolean {
  return resolveCodexFullQuotaPingTargetWindow(account) !== null;
}
