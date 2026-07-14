import type { CodexAccount } from '../types/codex';
import {
  getCodexEffectiveQuotaPercentages,
  getCodexPlanFilterKey,
} from '../types/codex';
import { sortCodexPlanFilterKeys } from './codexAccountOverview';

export interface CodexQuotaPoolItem {
  key: string;
  count: number;
  hourly: number;
  weekly: number;
}

export interface CodexQuotaPoolSummary {
  all: CodexQuotaPoolItem;
  byPlan: Record<string, CodexQuotaPoolItem>;
  visiblePlans: CodexQuotaPoolItem[];
}

function createQuotaPoolItem(key: CodexQuotaPoolItem['key']): CodexQuotaPoolItem {
  return { key, count: 0, hourly: 0, weekly: 0 };
}

function addAccountToQuotaPool(target: CodexQuotaPoolItem, account: CodexAccount): void {
  const percentages = getCodexEffectiveQuotaPercentages(account.quota);
  target.count += 1;
  target.hourly += percentages.hourly ?? 0;
  target.weekly += percentages.weekly ?? 0;
}

export function summarizeCodexQuotaPool(accounts: CodexAccount[]): CodexQuotaPoolSummary {
  const byPlan: Record<string, CodexQuotaPoolItem> = {};
  const all = createQuotaPoolItem('ALL');

  accounts.forEach((account) => {
    addAccountToQuotaPool(all, account);
    const planKey = getCodexPlanFilterKey(account);
    byPlan[planKey] ??= createQuotaPoolItem(planKey);
    addAccountToQuotaPool(byPlan[planKey], account);
  });

  return {
    all,
    byPlan,
    visiblePlans: sortCodexPlanFilterKeys(Object.keys(byPlan)).map(
      (key) => byPlan[key],
    ),
  };
}

export function formatCodexQuotaPoolPercent(value: number): string {
  return `${Math.max(0, Math.round(value))}%`;
}
