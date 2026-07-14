const CODEX_SHOW_CODE_REVIEW_QUOTA_STORAGE_KEY = 'agtools.codex_show_code_review_quota';
const CODEX_SHOW_ADDITIONAL_QUOTA_STORAGE_KEY = 'agtools.codex_show_additional_quota';

export const CODEX_CODE_REVIEW_QUOTA_VISIBILITY_CHANGED_EVENT =
  'agtools:codex-code-review-quota-visibility-changed';
export const CODEX_ADDITIONAL_QUOTA_VISIBILITY_CHANGED_EVENT =
  'agtools:codex-additional-quota-visibility-changed';

export function isCodexCodeReviewQuotaVisibleByDefault(): boolean {
  try {
    return localStorage.getItem(CODEX_SHOW_CODE_REVIEW_QUOTA_STORAGE_KEY) === '1';
  } catch {
    return false;
  }
}

export function isCodexAdditionalQuotaVisibleByDefault(): boolean {
  try {
    return localStorage.getItem(CODEX_SHOW_ADDITIONAL_QUOTA_STORAGE_KEY) !== '0';
  } catch {
    return true;
  }
}

export function persistCodexCodeReviewQuotaVisible(visible: boolean): void {
  try {
    localStorage.setItem(CODEX_SHOW_CODE_REVIEW_QUOTA_STORAGE_KEY, visible ? '1' : '0');
    window.dispatchEvent(
      new CustomEvent(CODEX_CODE_REVIEW_QUOTA_VISIBILITY_CHANGED_EVENT, { detail: visible }),
    );
  } catch {
    // ignore localStorage write failures
  }
}

export function persistCodexAdditionalQuotaVisible(visible: boolean): void {
  try {
    localStorage.setItem(CODEX_SHOW_ADDITIONAL_QUOTA_STORAGE_KEY, visible ? '1' : '0');
    window.dispatchEvent(
      new CustomEvent(CODEX_ADDITIONAL_QUOTA_VISIBILITY_CHANGED_EVENT, { detail: visible }),
    );
  } catch {
    // ignore localStorage write failures
  }
}
