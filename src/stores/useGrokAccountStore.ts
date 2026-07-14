import { createProviderAccountStore } from './createProviderAccountStore';
import * as grokService from '../services/grokService';
import {
  getGrokAccountDisplayEmail,
  getGrokPlanBadge,
  getGrokUsage,
  type GrokAccount,
} from '../types/grok';

export const useGrokAccountStore = createProviderAccountStore<GrokAccount>(
  'agtools.grok.accounts.cache',
  {
    listAccounts: grokService.listGrokAccounts,
    deleteAccount: grokService.deleteGrokAccount,
    deleteAccounts: grokService.deleteGrokAccounts,
    injectAccount: grokService.switchGrokAccount,
    refreshToken: grokService.refreshGrokAccount,
    refreshAllTokens: grokService.refreshAllGrokAccounts,
    importFromJson: grokService.importGrokFromJson,
    exportAccounts: grokService.exportGrokAccounts,
    updateAccountTags: grokService.updateGrokAccountTags,
  },
  {
    getDisplayEmail: getGrokAccountDisplayEmail,
    getPlanBadge: getGrokPlanBadge,
    getUsage: getGrokUsage,
  },
  {
    platformId: 'grok',
    // Grok 账号彼此独立（各用 GROK_HOME），不再维护全局「当前账号」。
    currentAccountIdKey: 'agtools.grok.current_account_id',
    resolveCurrentAccountId: async () => null,
    preserveSourceQuota: true,
  },
);
